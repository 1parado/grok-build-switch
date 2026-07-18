package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"grok_switch/internal/agentbridge"
	"grok_switch/internal/autostart"
	"grok_switch/internal/crash"
	"grok_switch/internal/grokauth"
	"grok_switch/internal/grokpool"
	"grok_switch/internal/paths"
	"grok_switch/internal/profiles"
	"grok_switch/internal/remoteaccess"
	"grok_switch/internal/server"
	"grok_switch/internal/settings"
	"grok_switch/internal/singleinstance"
	"grok_switch/internal/switcher"
	"grok_switch/internal/tray"
)

//go:embed ui/index.html ui/app.js ui/style.css ui/vendor icon.svg assets/icon.ico
var assets embed.FS

func main() {
	defer crash.RecoverMainThread()

	silent := flag.Bool("silent", false, "start without opening browser")
	noTray := flag.Bool("no-tray", false, "run http server without tray")
	flag.Parse()

	resolved, err := paths.Resolve()
	if err != nil {
		fatal(err)
	}
	// Initialize diagnostics before any directory setup so permission failures
	// still produce a native error dialog and use the log whenever possible.
	crash.Setup(resolved.LogFile)
	if err := resolved.Ensure(); err != nil {
		fatal(err)
	}

	settingsStore := settings.NewStore(resolved.SettingsFile)
	instanceLock, alreadyRunning, err := singleinstance.Acquire(resolved.DataDir)
	if err != nil {
		fatal(fmt.Errorf("创建单实例锁失败: %w", err))
	}
	if alreadyRunning {
		url, findErr := waitForExistingInstanceURL(settingsStore, resolved.DataDir, 3*time.Second)
		if findErr == nil {
			if openErr := tray.OpenBrowser(url); openErr == nil {
				return
			} else {
				crash.Logf("open existing instance failed: %v", openErr)
			}
		} else {
			crash.Logf("find existing instance failed: %v", findErr)
		}
		crash.ShowInfo("grok_switch", "grok_switch 已经在运行，但未能自动打开管理页面。请使用系统托盘图标打开。")
		return
	}
	defer instanceLock.Close()

	exePath, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	exePath, _ = filepath.Abs(exePath)

	profileStore := profiles.NewStore(resolved.ProfilesFile)
	grokAuthStore := grokauth.NewStore(resolved.GrokAuthFile)
	grokPool, err := grokpool.NewManager(resolved.GrokPoolDir)
	if err != nil {
		fatal(err)
	}
	if err := grokAuthStore.SetProxyURL(grokPool.Status().Settings.ProxyURL); err != nil {
		fatal(err)
	}
	if singleStatus, statusErr := grokAuthStore.Status(); statusErr == nil && singleStatus.Configured {
		if raw, readErr := os.ReadFile(resolved.GrokAuthFile); readErr == nil {
			if _, migrateErr := grokPool.Ensure([]grokpool.ImportFile{{Name: "legacy-grok-auth.json", Content: string(raw)}}); migrateErr != nil {
				crash.Logf("migrate legacy Grok auth into pool: %v", migrateErr)
			}
		}
	}
	grokPool.Start()
	defer grokPool.Close()
	sw := &switcher.Switcher{
		ConfigPath: resolved.GrokConfig,
		BackupsDir: resolved.BackupsDir,
		Profiles:   profileStore,
	}
	if err := sw.EnsureDefaultProfile(); err != nil {
		crash.Logf("default profile import skipped: %v", err)
	}

	currentSettings, err := settingsStore.Get()
	if err != nil {
		fatal(err)
	}
	if err := autostart.Sync(currentSettings.Autostart, exePath, currentSettings.SilentAutostart); err != nil {
		crash.Logf("autostart sync failed: %v", err)
	}
	agent := agentbridge.New(resolved.GrokHome, filepath.Join(resolved.DataDir, "agent.log"))
	agent.SetDefaultCwd(currentSettings.AgentDefaultCwd)
	defer agent.Stop()

	appServer := &server.Server{
		Paths:        resolved,
		Profiles:     profileStore,
		Settings:     settingsStore,
		RemoteAccess: remoteaccess.NewStore(resolved.RemoteAccessFile),
		GrokAuth:     grokAuthStore,
		GrokPool:     grokPool,
		Switcher:     sw,
		Agent:        agent,
		Assets:       assets,
		ExePath:      exePath,
	}
	httpServer, port, err := appServer.Listen(currentSettings.Port)
	if err != nil {
		fatal(err)
	}
	// Route net/http's internal panic/error reports into the crash log too.
	if crashFile := resolved.LogFile; crashFile != "" {
		if f, ferr := os.OpenFile(crashFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); ferr == nil {
			httpServer.ErrorLog = log.New(f, "http: ", log.LstdFlags)
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)

	trayApp := &tray.Tray{
		Profiles: profileStore,
		Settings: settingsStore,
		Switcher: sw,
		URL:      url,
		ExePath:  exePath,
		DataDir:  resolved.DataDir,
		LogFile:  resolved.LogFile,
		AuthFile: filepath.Join(resolved.GrokHome, "auth.json"),
		Assets:   assets,
	}
	if !*noTray {
		appServer.SetOnChanged(trayApp.Refresh)
	}

	if !*silent && currentSettings.AutoOpenBrowser {
		_ = tray.OpenBrowser(url)
	}

	if *noTray {
		waitForSignal()
		shutdown(httpServer)
		return
	}
	trayApp.Run()
	shutdown(httpServer)
}

func waitForExistingInstanceURL(store *settings.Store, dataDir string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 300 * time.Millisecond}
	var lastErr error
	for {
		current, err := store.Get()
		if err != nil {
			lastErr = err
		} else {
			ports := []int{current.ActualPort}
			if current.Port != current.ActualPort {
				ports = append(ports, current.Port)
			}
			for _, port := range ports {
				if settings.ValidatePort(port) != nil {
					continue
				}
				url := fmt.Sprintf("http://127.0.0.1:%d", port)
				resp, requestErr := client.Get(url + "/api/status")
				if requestErr != nil {
					lastErr = requestErr
					continue
				}
				var status struct {
					DataDir string `json:"data_dir"`
				}
				decodeErr := json.NewDecoder(resp.Body).Decode(&status)
				_ = resp.Body.Close()
				if decodeErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && status.DataDir == dataDir {
					return url, nil
				}
				if decodeErr != nil {
					lastErr = decodeErr
				} else {
					lastErr = fmt.Errorf("port %d is not the running grok_switch instance", port)
				}
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("running instance did not become ready")
	}
	return "", lastErr
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

func shutdown(srv interface{ Shutdown(context.Context) error }) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func fatal(err error) {
	crash.ReportFatal(err)
	os.Exit(1)
}
