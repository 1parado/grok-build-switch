//go:build windows

package autostart

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	name   = "grok_switch"
)

func Enable(exePath string, silent bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	value := fmt.Sprintf("%q", exePath)
	if silent {
		value += " --silent"
	}
	return k.SetStringValue(name, value)
}

func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	err = k.DeleteValue(name)
	if err == registry.ErrNotExist {
		return nil
	}
	return err
}

func IsEnabled() (bool, string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false, "", err
	}
	defer k.Close()
	value, _, err := k.GetStringValue(name)
	if err == registry.ErrNotExist {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return value != "", value, nil
}

func Sync(enabled bool, exePath string, silent bool) error {
	if enabled {
		return Enable(exePath, silent)
	}
	return Disable()
}
