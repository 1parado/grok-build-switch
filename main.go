//go:build !wailsgui

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"grok_switch/internal/agentbridge"
	"grok_switch/internal/autostart"
	"grok_switch/internal/cpamint"
	"grok_switch/internal/crash"
	"grok_switch/internal/grokauth"
	"grok_switch/internal/grokpool"
	"grok_switch/internal/paths"
	"grok_switch/internal/profiles"
	"grok_switch/internal/registrar"
	"grok_switch/internal/remoteaccess"
	"grok_switch/internal/server"
	"grok_switch/internal/settings"
	"grok_switch/internal/singleinstance"
	"grok_switch/internal/switcher"
	"grok_switch/internal/tray"
	"grok_switch/internal/updatecheck"
)

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
	registrarService, err := registrar.NewService(resolved.DataDir)
	if err != nil {
		fatal(err)
	}
	defer registrarService.Close()
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
	updateState := updatecheck.NewPreferenceStore(resolved.UpdateStateFile)

	appServer := &server.Server{
		Paths:         resolved,
		Profiles:      profileStore,
		Settings:      settingsStore,
		RemoteAccess:  remoteaccess.NewStore(resolved.RemoteAccessFile),
		GrokAuth:      grokAuthStore,
		GrokPool:      grokPool,
		CpaMint:       cpamint.NewService(),
		Registrar:     registrarService,
		Switcher:      sw,
		Agent:         agent,
		Assets:        assets,
		ExePath:       exePath,
		Version:       version,
		UpdateChecker: updatecheck.New(version, "grok_switch.exe"),
		UpdateState:   updateState,
	}
	httpServer, port, err := appServer.Listen(currentSettings.Port)
	if err != nil {
		fatal(err)
	}
	startUpdateNotification(appServer.UpdateChecker, updateState, "grok_switch")
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
