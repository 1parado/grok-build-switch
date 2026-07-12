package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"grok_switch/internal/autostart"
	"grok_switch/internal/crash"
	"grok_switch/internal/paths"
	"grok_switch/internal/profiles"
	"grok_switch/internal/server"
	"grok_switch/internal/settings"
	"grok_switch/internal/switcher"
	"grok_switch/internal/tray"
)

//go:embed ui/index.html ui/app.js ui/style.css icon.svg assets/icon.ico
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
	if err := resolved.Ensure(); err != nil {
		fatal(err)
	}
	// Open the crash log as early as possible so startup failures and any
	// later stderr writes / panics are captured instead of vanishing.
	crash.Setup(resolved.LogFile)

	exePath, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	exePath, _ = filepath.Abs(exePath)

	profileStore := profiles.NewStore(resolved.ProfilesFile)
	settingsStore := settings.NewStore(resolved.SettingsFile)
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

	appServer := &server.Server{
		Paths:    resolved,
		Profiles: profileStore,
		Settings: settingsStore,
		Switcher: sw,
		Assets:   assets,
		ExePath:  exePath,
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
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
