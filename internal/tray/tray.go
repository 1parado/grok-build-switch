package tray

import (
	"embed"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"fyne.io/systray"

	"grok_switch/internal/autostart"
	"grok_switch/internal/crash"
	"grok_switch/internal/notify"
	"grok_switch/internal/profiles"
	"grok_switch/internal/settings"
	"grok_switch/internal/switcher"
)

type Tray struct {
	Profiles *profiles.Store
	Settings *settings.Store
	Switcher *switcher.Switcher
	URL      string
	ExePath  string
	DataDir  string
	LogFile  string
	Assets   embed.FS

	refreshCh chan struct{}
	done      chan struct{}
	curStop   *stopper
	mu        sync.Mutex
}

type stopper struct {
	ch   chan struct{}
	once sync.Once
}

func newStopper() *stopper { return &stopper{ch: make(chan struct{})} }
func (s *stopper) close() {
	if s == nil {
		return
	}
	s.once.Do(func() { close(s.ch) })
}

func (t *Tray) Run() {
	t.refreshCh = make(chan struct{}, 1)
	t.done = make(chan struct{})
	systray.Run(t.onReady, t.onExit)
}

func (t *Tray) Refresh() {
	if t.refreshCh == nil {
		return
	}
	select {
	case t.refreshCh <- struct{}{}:
	default:
	}
}

func (t *Tray) onReady() {
	if icon, err := t.Assets.ReadFile("assets/icon.ico"); err == nil {
		systray.SetIcon(icon)
	} else {
		systray.SetIcon(iconData)
	}
	systray.SetTitle("grok_switch")
	t.rebuild()
	go t.loop()
}

func (t *Tray) onExit() {
	if t.done != nil {
		close(t.done)
	}
	t.mu.Lock()
	t.curStop.close()
	t.curStop = nil
	t.mu.Unlock()
}

func (t *Tray) loop() {
	for {
		select {
		case <-t.done:
			return
		case <-t.refreshCh:
			t.rebuild()
		}
	}
}

func (t *Tray) rebuild() {
	select {
	case <-t.done:
		return
	default:
	}

	t.mu.Lock()
	prev := t.curStop
	next := newStopper()
	t.curStop = next
	t.mu.Unlock()
	prev.close()

	stop := next.ch
	systray.ResetMenu()
	t.buildMenu(stop)
}

func (t *Tray) buildMenu(stop <-chan struct{}) {
	activeName := "未设置"
	activeID := ""
	drifted := false
	list, err := t.Profiles.List()
	if err == nil {
		for _, profile := range list {
			if profile.IsActive {
				activeName = profile.Name
				activeID = profile.ID
				if _, matches, mErr := t.Switcher.ActiveStatus(); mErr == nil {
					drifted = !matches && activeID != ""
				}
				break
			}
		}
	}
	tip := "grok_switch · 当前：" + activeName
	if drifted {
		tip += " · 配置不一致"
	}
	systray.SetTooltip(tip)
	currentLabel := "当前：" + activeName
	if drifted {
		currentLabel += " ⚠"
	}
	current := systray.AddMenuItem(currentLabel, "")
	current.Disable()
	systray.AddSeparator()
	if err == nil {
		for _, profile := range list {
			label := profile.Name
			if profile.IsActive {
				label = "✓ " + label
			}
			item := systray.AddMenuItem(label, profile.BaseURL)
			p := profile
			t.watch(stop, item, "activate:"+p.ID, func() {
				if _, err := t.Switcher.Activate(p.ID); err != nil {
					crash.Logf("activate %s failed: %v", p.ID, err)
					notify.Info("grok_switch", "切换失败："+err.Error())
					return
				}
				notify.Info("grok_switch", "已切换到 "+p.Name+"\n新开 grok 会话生效")
				t.Refresh()
			})
		}
	}
	if activeID != "" {
		reapply := systray.AddMenuItem("重新应用当前 Profile", "用当前 Profile 覆盖 config.toml")
		id := activeID
		name := activeName
		t.watch(stop, reapply, "reapply", func() {
			if _, err := t.Switcher.Activate(id); err != nil {
				crash.Logf("reapply failed: %v", err)
				notify.Info("grok_switch", "重新应用失败："+err.Error())
				return
			}
			notify.Info("grok_switch", "已重新应用 "+name)
			t.Refresh()
		})
	}
	systray.AddSeparator()
	open := systray.AddMenuItem("打开 Web 管理界面", t.URL)
	t.watch(stop, open, "open", func() {
		_ = OpenBrowser(t.URL)
	})
	copyURL := systray.AddMenuItem("复制 Web 地址", t.URL)
	t.watch(stop, copyURL, "copy-url", func() {
		if err := notify.CopyText(t.URL); err != nil {
			crash.Logf("copy url failed: %v", err)
			notify.Info("grok_switch", "复制失败")
			return
		}
		notify.Info("grok_switch", "已复制 "+t.URL)
	})
	importItem := systray.AddMenuItem("从当前配置导入", "")
	t.watch(stop, importItem, "import", func() {
		if _, err := t.Switcher.ImportCurrent("Imported", false); err != nil {
			crash.Logf("import failed: %v", err)
			notify.Info("grok_switch", "导入失败："+err.Error())
			return
		}
		notify.Info("grok_switch", "已从 config.toml 导入")
		t.Refresh()
	})
	openData := systray.AddMenuItem("打开数据目录", t.DataDir)
	t.watch(stop, openData, "open-data", func() {
		if err := notify.OpenPath(t.DataDir); err != nil {
			crash.Logf("open data dir failed: %v", err)
		}
	})
	openLog := systray.AddMenuItem("打开日志目录", filepath.Dir(t.LogFile))
	t.watch(stop, openLog, "open-log", func() {
		dir := filepath.Dir(t.LogFile)
		if err := notify.OpenPath(dir); err != nil {
			crash.Logf("open log dir failed: %v", err)
		}
	})
	currentSettings, _ := t.Settings.Get()
	autoLabel := "开机自启"
	if currentSettings.Autostart {
		autoLabel = "✓ " + autoLabel
	}
	auto := systray.AddMenuItem(autoLabel, "")
	t.watch(stop, auto, "autostart", func() {
		next, err := t.Settings.Get()
		if err != nil {
			return
		}
		next.Autostart = !next.Autostart
		if next.Autostart {
			next.SilentAutostart = true
		}
		if _, err := t.Settings.Update(next); err == nil {
			_ = autostart.Sync(next.Autostart, t.ExePath, next.SilentAutostart)
			if next.Autostart {
				notify.Info("grok_switch", "已开启开机自启")
			} else {
				notify.Info("grok_switch", "已关闭开机自启")
			}
		}
		t.Refresh()
	})
	systray.AddSeparator()
	quit := systray.AddMenuItem("退出", "")
	t.watch(stop, quit, "quit", func() {
		systray.Quit()
	})
}

func (t *Tray) watch(stop <-chan struct{}, item *systray.MenuItem, name string, fn func()) {
	go func() {
		for {
			select {
			case <-stop:
				return
			case _, ok := <-item.ClickedCh:
				if !ok {
					return
				}
				crash.Guard("tray:"+name, fn)
			}
		}
	}()
}

func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

var iconData = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x10,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0xf3, 0xff,
	0x61, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
	0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d,
	0xb0, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
	0x44, 0xae, 0x42, 0x60, 0x82,
}
