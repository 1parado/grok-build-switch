//go:build !windows

package autostart

import "fmt"

func Enable(exePath string, silent bool) error {
	return fmt.Errorf("autostart is only implemented on Windows")
}

func Disable() error {
	return nil
}

func IsEnabled() (bool, string, error) {
	return false, "", nil
}

func Sync(enabled bool, exePath string, silent bool) error {
	if enabled {
		return Enable(exePath, silent)
	}
	return Disable()
}
