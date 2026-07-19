//go:build wailsgui && windows

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"grok_switch/internal/crash"
	"grok_switch/internal/notify"
	browsertray "grok_switch/internal/tray"
)

type guiTrayController struct {
	url  string
	icon []byte

	ctxMu sync.RWMutex
	ctx   context.Context

	ready          atomic.Bool
	quitRequested  atomic.Bool
	providerClient *guiTrayProviderClient
	refreshCh      chan struct{}
	done           chan struct{}
	doneOnce       sync.Once

	menuMu   sync.Mutex
	menuStop *guiTrayMenuStopper
	lastMenu string

	showAction func(context.Context)
	hideAction func(context.Context)
	quitAction func(context.Context)
}

type guiTrayMenuStopper struct {
	ch   chan struct{}
	once sync.Once
}

func newGUITrayMenuStopper() *guiTrayMenuStopper {
	return &guiTrayMenuStopper{ch: make(chan struct{})}
}

func (s *guiTrayMenuStopper) close() {
	if s != nil {
		s.once.Do(func() { close(s.ch) })
	}
}

func newGUITrayController(url string, icon []byte) *guiTrayController {
	return &guiTrayController{
		url:            url,
		icon:           icon,
		providerClient: newGUITrayProviderClient(url),
		refreshCh:      make(chan struct{}, 1),
		done:           make(chan struct{}),
		showAction: func(ctx context.Context) {
			wailsruntime.WindowShow(ctx)
			wailsruntime.WindowUnminimise(ctx)
		},
		hideAction: wailsruntime.WindowHide,
		quitAction: wailsruntime.Quit,
	}
}

func (t *guiTrayController) register() {
	systray.Register(t.onReady, t.onExit)
}

func (t *guiTrayController) startup(ctx context.Context) {
	t.ctxMu.Lock()
	t.ctx = ctx
	t.ctxMu.Unlock()
	if t.quitRequested.Load() {
		t.quitAction(ctx)
	}
}

func (t *guiTrayController) context() context.Context {
	t.ctxMu.RLock()
	defer t.ctxMu.RUnlock()
	return t.ctx
}

func (t *guiTrayController) beforeClose(ctx context.Context) bool {
	if t.quitRequested.Load() || !t.ready.Load() {
		return false
	}
	t.hideAction(ctx)
	return true
}

func (t *guiTrayController) showWindow() {
	if ctx := t.context(); ctx != nil {
		t.showAction(ctx)
	}
}

func (t *guiTrayController) requestQuit() {
	if !t.quitRequested.CompareAndSwap(false, true) {
		return
	}
	if ctx := t.context(); ctx != nil {
		t.quitAction(ctx)
	}
}

func (t *guiTrayController) shutdown() {
	t.stop()
	systray.Quit()
}

func (t *guiTrayController) onReady() {
	t.ready.Store(true)
	if len(t.icon) > 0 {
		systray.SetIcon(t.icon)
	}
	systray.SetTitle("grok_switch GUI")
	t.refreshMenu(true)
	go t.refreshLoop()
}

func (t *guiTrayController) onExit() {
	t.ready.Store(false)
	t.stop()
}

func (t *guiTrayController) stop() {
	t.doneOnce.Do(func() { close(t.done) })
	t.menuMu.Lock()
	t.menuStop.close()
	t.menuStop = nil
	t.menuMu.Unlock()
}

func (t *guiTrayController) refreshLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.refreshMenu(false)
		case <-t.refreshCh:
			t.refreshMenu(true)
		}
	}
}

func (t *guiTrayController) requestRefresh() {
	select {
	case <-t.done:
		return
	case t.refreshCh <- struct{}{}:
	default:
	}
}

func (t *guiTrayController) refreshMenu(force bool) {
	snapshot, err := t.providerClient.snapshot(context.Background())
	key := snapshot.fingerprint()
	if err != nil {
		key = "error:" + err.Error()
	}
	if !force && key == t.lastMenu {
		return
	}
	t.lastMenu = key
	t.rebuildMenu(snapshot, err)
}

func (t *guiTrayController) rebuildMenu(snapshot guiTrayProviderSnapshot, loadErr error) {
	t.menuMu.Lock()
	previous := t.menuStop
	stopper := newGUITrayMenuStopper()
	t.menuStop = stopper
	t.menuMu.Unlock()
	previous.close()

	systray.ResetMenu()
	currentName := snapshot.currentName()
	tooltip := "grok_switch GUI · 当前：" + currentName
	currentLabel := "当前：" + currentName
	if snapshot.drifted() {
		tooltip += " · 配置不一致"
		currentLabel += " ⚠"
	}
	if loadErr != nil {
		tooltip = "grok_switch GUI · 供应商状态不可用"
		currentLabel = "当前供应商：加载失败"
	}
	systray.SetTooltip(tooltip)
	current := systray.AddMenuItem(currentLabel, "")
	current.Disable()
	systray.AddSeparator()

	providers := systray.AddMenuItem("供应商", "快捷切换供应商 Profile")
	if loadErr != nil {
		failed := providers.AddSubMenuItem("供应商加载失败", loadErr.Error())
		failed.Disable()
	} else {
		officialLabel := "官方账号登录"
		if snapshot.OfficialActive {
			officialLabel = "✓ " + officialLabel
		}
		official := providers.AddSubMenuItem(officialLabel, "使用 grok login 的官方账号凭据")
		t.watch(stopper.ch, official, "activate:official", t.activateOfficial)
		providers.AddSeparator()
		if len(snapshot.Providers) == 0 {
			empty := providers.AddSubMenuItem("暂无供应商", "请在 GUI 或 Web 管理界面添加供应商")
			empty.Disable()
		} else {
			for _, provider := range snapshot.Providers {
				label := provider.Name
				if provider.ID == snapshot.ActiveID || provider.IsActive {
					label = "✓ " + label
				}
				item := providers.AddSubMenuItem(label, provider.BaseURL)
				selected := provider
				t.watch(stopper.ch, item, "activate:"+selected.ID, func() {
					t.activateProvider(selected)
				})
			}
		}
	}
	if loadErr == nil && snapshot.ActiveID != "" {
		reapply := systray.AddMenuItem("重新应用当前 Profile", "用当前 Profile 覆盖 config.toml")
		active := guiTrayProvider{ID: snapshot.ActiveID, Name: snapshot.currentName()}
		t.watch(stopper.ch, reapply, "reapply:"+active.ID, func() {
			t.activateProvider(active)
		})
	}

	systray.AddSeparator()
	openWindow := systray.AddMenuItem("打开 GUI 窗口", "显示 grok_switch GUI")
	openBrowser := systray.AddMenuItem("打开 Web 管理界面", t.url)
	systray.AddSeparator()
	exit := systray.AddMenuItem("退出 grok_switch GUI", "停止 GUI 与其本地服务")

	t.watch(stopper.ch, openWindow, "show", t.showWindow)
	t.watch(stopper.ch, openBrowser, "open-browser", func() {
		if err := browsertray.OpenBrowser(t.url); err != nil {
			crash.Logf("open GUI web interface: %v", err)
		}
	})
	t.watch(stopper.ch, exit, "quit", t.requestQuit)
}

func (t *guiTrayController) activateProvider(provider guiTrayProvider) {
	if err := t.providerClient.activate(context.Background(), provider.ID); err != nil {
		crash.Logf("GUI tray activate %s failed: %v", provider.ID, err)
		notify.Info("grok_switch GUI", "切换失败："+err.Error())
		return
	}
	notify.Info("grok_switch GUI", "已切换到 "+provider.Name+"\n新开 grok 会话生效")
	t.requestRefresh()
}

func (t *guiTrayController) activateOfficial() {
	if err := t.providerClient.activateOfficial(context.Background()); err != nil {
		crash.Logf("GUI tray activate official failed: %v", err)
		notify.Info("grok_switch GUI", "切换官方账号失败："+err.Error())
		return
	}
	notify.Info("grok_switch GUI", "已切换到官方账号\n新开 grok 会话生效")
	t.requestRefresh()
}

func (t *guiTrayController) watch(stop <-chan struct{}, item *systray.MenuItem, name string, action func()) {
	go func() {
		for {
			select {
			case <-stop:
				return
			case _, ok := <-item.ClickedCh:
				if !ok {
					return
				}
				crash.Guard("gui-tray:"+name, action)
			}
		}
	}()
}
