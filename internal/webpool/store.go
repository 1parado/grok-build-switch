package webpool

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"grok_switch/internal/atomicfile"
	"grok_switch/internal/recovery"
)

// webRateLimitTTL：429 隔离后的自动恢复窗口（网页版额度按小时滚动）。
const webRateLimitTTL = time.Hour

func defaultSettings() Settings {
	return Settings{Enabled: false}
}

func normalizeSettings(settings Settings) Settings {
	settings.ProxyURL = trimSpace(settings.ProxyURL)
	return settings
}

func (m *Manager) load() error {
	if err := os.MkdirAll(m.accountsDir, 0o700); err != nil {
		return err
	}
	data, err := os.ReadFile(m.indexPath)
	if errors.Is(err, os.ErrNotExist) {
		m.state = persistedState{Version: poolVersion, Settings: defaultSettings(), Accounts: []Account{}}
		key, keyErr := newWebAPIKey()
		if keyErr != nil {
			return keyErr
		}
		m.state.LocalAPIKey = key
		return m.saveLocked()
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &m.state); err != nil {
		cause := fmt.Errorf("读取 Web 通道号池: %w", err)
		backup, backupErr := recovery.BackupCorrupt(m.indexPath)
		if backupErr != nil {
			return fmt.Errorf("%v; 备份损坏文件: %w", cause, backupErr)
		}
		log.Printf("recovered webpool file %s after %v; backup=%s", m.indexPath, cause, backup)
		m.state = persistedState{Version: poolVersion, Settings: defaultSettings(), Accounts: []Account{}}
		m.state.LocalAPIKey, err = newWebAPIKey()
		if err != nil {
			return err
		}
		return m.saveLocked()
	}
	m.state.Version = poolVersion
	m.state.Settings = normalizeSettings(m.state.Settings)
	if m.state.Accounts == nil {
		m.state.Accounts = []Account{}
	}
	if m.state.LocalAPIKey == "" {
		m.state.LocalAPIKey, err = newWebAPIKey()
		if err != nil {
			return err
		}
	}
	return m.saveLocked()
}

func (m *Manager) saveLocked() error {
	m.state.Version = poolVersion
	m.state.Settings = normalizeSettings(m.state.Settings)
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.indexPath, append(data, '\n'))
}

func newWebAPIKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "gsk-web-" + hex.EncodeToString(buf), nil
}

func atomicWrite(path string, data []byte) error {
	return atomicfile.Write(path, data, atomicfile.Options{DirMode: 0o700, FileMode: 0o600, Sync: true})
}

func trimSpace(v string) string {
	for len(v) > 0 && (v[0] == ' ' || v[0] == '\t' || v[0] == '\n' || v[0] == '\r') {
		v = v[1:]
	}
	for len(v) > 0 && (v[len(v)-1] == ' ' || v[len(v)-1] == '\t' || v[len(v)-1] == '\n' || v[len(v)-1] == '\r') {
		v = v[:len(v)-1]
	}
	return v
}
