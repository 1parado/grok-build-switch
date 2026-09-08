//go:build notray

package tray

import (
	"embed"
	"os/exec"
	"runtime"

	"grok_switch/internal/profiles"
	"grok_switch/internal/settings"
	"grok_switch/internal/switcher"
)

// Tray is a headless stub used when the fyne.io/systray cgo dependency is
// unavailable (e.g. macOS dev machines without a working Xcode toolchain).
// Build with `-tags notray` and run with `-no-tray`.
type Tray struct {
	Profiles *profiles.Store
	Settings *settings.Store
	Switcher *switcher.Switcher
	URL      string
	ExePath  string
	DataDir  string
	LogFile  string
	AuthFile string
	Assets   embed.FS
}

func (t *Tray) Run() {
	select {}
}

func (t *Tray) Refresh() {}

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

func StartGrokLogin() error {
	return exec.Command("grok", "login").Start()
}
