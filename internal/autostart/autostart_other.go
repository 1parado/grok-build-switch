//go:build !windows

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const desktopFileName = "grok_switch.desktop"

func Enable(exePath string, silent bool) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("autostart is only implemented on Windows and Linux")
	}
	if exePath == "" {
		return fmt.Errorf("empty executable path")
	}
	path, err := desktopFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	execValue := quoteDesktopExec(exePath)
	if silent {
		execValue += " --silent"
	}
	content := strings.Join([]string{
		"[Desktop Entry]",
		"Type=Application",
		"Name=grok_switch",
		"Comment=Grok Build profile switcher",
		"Exec=" + execValue,
		"Terminal=false",
		"X-GNOME-Autostart-enabled=true",
		"",
	}, "\n")
	return os.WriteFile(path, []byte(content), 0o644)
}

func Disable() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	path, err := desktopFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func IsEnabled() (bool, string, error) {
	if runtime.GOOS != "linux" {
		return false, "", nil
	}
	path, err := desktopFilePath()
	if err != nil {
		return false, "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, string(data), nil
}

func Sync(enabled bool, exePath string, silent bool) error {
	if enabled {
		return Enable(exePath, silent)
	}
	return Disable()
}

func desktopFilePath() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "autostart", desktopFileName), nil
}

func quoteDesktopExec(value string) string {
	escaped := strings.NewReplacer("\\", "\\\\", `"`, `\"`, "`", "\\`", "$", "\\$").Replace(value)
	return `"` + escaped + `"`
}
