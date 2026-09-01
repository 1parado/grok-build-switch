package grokpool

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"grok_switch/internal/recovery"
)

func (m *Manager) load() error {
	if err := os.MkdirAll(m.accountsDir, 0o700); err != nil {
		return err
	}
	data, err := os.ReadFile(m.indexPath)
	if errors.Is(err, os.ErrNotExist) {
		m.state = persistedState{Version: poolVersion, Settings: defaultSettings(), Accounts: []Account{}}
		key, keyErr := newPoolAPIKey()
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
		cause := fmt.Errorf("读取 Grok 号池: %w", err)
		backup, backupErr := recovery.BackupCorrupt(m.indexPath)
		if backupErr != nil {
			return fmt.Errorf("%v; 备份损坏号池文件: %w", cause, backupErr)
		}
		log.Printf("recovered Grok pool file %s after %v; backup=%s", m.indexPath, cause, backup)
		m.state = persistedState{Version: poolVersion, Settings: defaultSettings(), Accounts: []Account{}}
		m.state.LocalAPIKey, err = newPoolAPIKey()
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
		m.state.LocalAPIKey, err = newPoolAPIKey()
		if err != nil {
			return err
		}
	}
	m.sortAccountsLocked()
	return m.saveLocked()
}

func (m *Manager) saveLocked() error {
	m.state.Version = poolVersion
	m.state.Settings = normalizeSettings(m.state.Settings)
	// 账号状态可能已被改动（分类/停用/增删）；写盘必然伴随这些变化，
	// 统一在此置脏，下一次 summarizeLocked 重算。
	m.invalidateSummaryLocked()
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.indexPath, append(data, '\n'))
}

func (m *Manager) sortAccountsLocked() {
	sort.SliceStable(m.state.Accounts, func(i, j int) bool {
		left := strings.ToLower(firstNonEmpty(m.state.Accounts[i].Email, m.state.Accounts[i].FileName, m.state.Accounts[i].ID))
		right := strings.ToLower(firstNonEmpty(m.state.Accounts[j].Email, m.state.Accounts[j].FileName, m.state.Accounts[j].ID))
		return left < right
	})
}

func normalizeSettings(settings Settings) Settings {
	if settings.IntervalMinutes < minIntervalMinutes || settings.IntervalMinutes > maxIntervalMinutes {
		settings.IntervalMinutes = defaultIntervalMinutes
	}
	if settings.Workers < minWorkers || settings.Workers > maxWorkers {
		settings.Workers = defaultWorkers
	}
	settings.AuthDir = strings.TrimSpace(settings.AuthDir)
	return settings
}

func validateSettings(settings Settings) error {
	if settings.IntervalMinutes < minIntervalMinutes || settings.IntervalMinutes > maxIntervalMinutes {
		return fmt.Errorf("巡检间隔必须在 %d–%d 分钟之间", minIntervalMinutes, maxIntervalMinutes)
	}
	if settings.Workers < minWorkers || settings.Workers > maxWorkers {
		return fmt.Errorf("并发数必须在 %d–%d 之间", minWorkers, maxWorkers)
	}
	return nil
}

func newPoolAPIKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "gsk-pool-" + hex.EncodeToString(buf), nil
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// rename 前把数据块刷盘，避免掉电时丢失刚轮换的 refresh_token 等关键凭据。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if runtime.GOOS == "windows" {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return err
			}
			return os.Rename(tmpName, path)
		}
		return err
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func accountAvailable(account Account) bool {
	if account.Disabled {
		return false
	}
	switch account.Classification {
	case "permission_denied", "quota_exhausted", "reauth":
		return false
	default:
		return true
	}
}

func accountAbnormal(account Account) bool {
	classification := strings.TrimSpace(account.Classification)
	return classification != "" && classification != "healthy" && classification != "uninspected"
}

func summarize(accounts []Account) Summary {
	var summary Summary
	for _, account := range accounts {
		summary.Total++
		if account.Disabled {
			summary.Disabled++
		}
		if accountAvailable(account) {
			summary.Available++
		}
		if accountAbnormal(account) {
			summary.Abnormal++
		}
		switch account.Classification {
		case "healthy":
			summary.Healthy++
		case "permission_denied":
			summary.Permission++
		case "quota_exhausted":
			summary.Quota++
		case "reauth":
			summary.Reauth++
		case "probe_error", "model_unavailable", "unknown":
			summary.ProbeError++
		default:
			summary.Uninspected++
		}
	}
	return summary
}

// summarizeLocked 返回全池 summary（须持 m.mu）。账号集合脏标记置位时重算，
// 否则直接复用缓存——轮询端点每次调用都会全量重算，数千账号下纯属重复。
func (m *Manager) summarizeLocked() Summary {
	if m.accountsDirty {
		m.summaryCache = summarize(m.state.Accounts)
		m.accountsDirty = false
	}
	return m.summaryCache
}

// invalidateSummaryLocked 标记 summary 需要重算（须持 m.mu，在改动账号
// 分类/Disabled/增删之后调用）。
func (m *Manager) invalidateSummaryLocked() {
	m.accountsDirty = true
}
