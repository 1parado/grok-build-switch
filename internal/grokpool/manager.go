package grokpool

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"grok_switch/internal/grokauth"
	"grok_switch/internal/netproxy"
)

func NewManager(dir string) (*Manager, error) {
	m := &Manager{
		dir:         dir,
		indexPath:   filepath.Join(dir, "pool.json"),
		accountsDir: filepath.Join(dir, "accounts"),
		client:      &http.Client{Timeout: 25 * time.Second},
		upstreamURL: grokauth.UpstreamURL(),
		wake:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		stores:      make(map[string]*grokauth.Store),
		watchHashes: make(map[string]string),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	normalizedProxy, transport, err := netproxy.BuildTransport(m.state.Settings.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("读取号池代理设置: %w", err)
	}
	m.state.Settings.ProxyURL = normalizedProxy
	m.transport = transport
	m.client = clientForTransport(transport)
	if err := m.saveLocked(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()
	go m.scheduler()
	go m.watchLoop()
	m.signalWake()
}

// SetOnAuthDirImport registers a callback invoked after a successful hot-load import.
func (m *Manager) SetOnAuthDirImport(fn func(ImportResult)) {
	m.mu.Lock()
	m.onAuthDirImport = fn
	m.mu.Unlock()
}

func (m *Manager) Close() {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	m.started = false
	if m.runCancel != nil {
		m.runCancel()
	}
	m.mu.Unlock()
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
	m.runWG.Wait()
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	accounts := append([]Account(nil), m.state.Accounts...)
	settings := m.state.Settings
	settings.AuthDir = m.resolvedAuthDirLocked()
	return Status{
		Configured:      len(accounts) > 0,
		LocalAPIKey:     m.state.LocalAPIKey,
		Settings:        settings,
		Accounts:        accounts,
		Summary:         summarize(accounts),
		Running:         m.running,
		Done:            m.doneCount,
		Total:           m.totalCount,
		LastRun:         m.state.LastRun,
		NextRun:         m.nextRun,
		LastError:       m.state.LastError,
		ResolvedAuthDir: settings.AuthDir,
		WatchLastScan:   m.watchLastScan,
		WatchLastImport: m.watchLastImport,
		WatchLastError:  m.watchLastError,
		WatchFileCount:  m.watchFileCount,
	}
}

// ResolvedAuthDir returns the absolute CPA auth directory path.
func (m *Manager) ResolvedAuthDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolvedAuthDirLocked()
}

func (m *Manager) resolvedAuthDirLocked() string {
	raw := strings.TrimSpace(m.state.Settings.AuthDir)
	if raw == "" {
		return filepath.Clean(filepath.Join(m.dir, "..", "cpa_auths"))
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Clean(filepath.Join(m.dir, raw))
}

func (m *Manager) Import(files []ImportFile) (ImportResult, error) {
	return m.importFiles(files, false)
}

func (m *Manager) Ensure(files []ImportFile) (ImportResult, error) {
	return m.importFiles(files, true)
}

func (m *Manager) importFiles(files []ImportFile, onlyMissing bool) (ImportResult, error) {
	if len(files) == 0 {
		return ImportResult{}, fmt.Errorf("请选择至少一个认证 JSON")
	}
	result := ImportResult{}
	changed := 0
	for _, file := range files {
		credentials, err := grokauth.ParseCredentials([]byte(file.Content))
		if err != nil {
			result.Failed = append(result.Failed, fmt.Sprintf("%s: %v", firstNonEmpty(file.Name, "未命名文件"), err))
			continue
		}
		for index, credential := range credentials {
			name := strings.TrimSpace(file.Name)
			if len(credentials) > 1 {
				name = fmt.Sprintf("%s#%d", firstNonEmpty(name, "auth.json"), index+1)
			}
			if onlyMissing && m.hasCredential(credentialID(credential)) {
				result.Updated++
				continue
			}
			updated, err := m.importCredential(name, credential)
			if err != nil {
				result.Failed = append(result.Failed, fmt.Sprintf("%s: %v", firstNonEmpty(name, "未命名账号"), err))
				continue
			}
			if updated {
				result.Updated++
			} else {
				result.Imported++
			}
			changed++
		}
	}
	if result.Imported+result.Updated == 0 {
		return result, fmt.Errorf("没有成功导入账号")
	}
	if changed > 0 {
		m.mu.Lock()
		if m.running {
			m.rerunRequested = true
		} else if m.state.Settings.Enabled {
			m.nextRun = time.Now().UTC()
		}
		m.mu.Unlock()
		m.signalWake()
	}
	return result, nil
}

func (m *Manager) hasCredential(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, account := range m.state.Accounts {
		if account.ID == id {
			return true
		}
	}
	return false
}

func (m *Manager) importCredential(fileName string, credential grokauth.Credential) (bool, error) {
	id := credentialID(credential)
	store := m.accountStore(id)
	status, err := store.ImportCredential(credential)
	if err != nil {
		return false, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for i := range m.state.Accounts {
		if m.state.Accounts[i].ID != id {
			continue
		}
		account := &m.state.Accounts[i]
		account.FileName = firstNonEmpty(fileName, account.FileName)
		account.Email = firstNonEmpty(status.Email, account.Email)
		account.Source = status.Source
		account.ExpiresAt = status.ExpiresAt
		account.ImportedAt = now
		account.Classification = "uninspected"
		account.Reason = "凭据已更新，等待巡检"
		account.Action = "keep"
		account.HTTPStatus = 0
		account.ErrorCode = ""
		account.ErrorMessage = ""
		account.LastInspected = time.Time{}
		m.sortAccountsLocked()
		return true, m.saveLocked()
	}
	m.state.Accounts = append(m.state.Accounts, Account{
		ID:             id,
		FileName:       fileName,
		Email:          status.Email,
		Source:         status.Source,
		Classification: "uninspected",
		Reason:         "等待首次巡检",
		Action:         "keep",
		ExpiresAt:      status.ExpiresAt,
		ImportedAt:     now,
	})
	m.sortAccountsLocked()
	return false, m.saveLocked()
}

func (m *Manager) UpdateSettings(settings Settings) (Status, error) {
	normalizedProxy, transport, err := netproxy.BuildTransport(settings.ProxyURL)
	if err != nil {
		return Status{}, err
	}
	settings.ProxyURL = normalizedProxy
	settings.AuthDir = strings.TrimSpace(settings.AuthDir)
	if err := validateSettings(settings); err != nil {
		return Status{}, err
	}
	m.mu.Lock()
	m.state.Settings = settings
	m.transport = transport
	m.client = clientForTransport(transport)
	for _, store := range m.stores {
		_ = store.SetProxyURL(settings.ProxyURL)
	}
	if settings.Enabled && len(m.state.Accounts) > 0 {
		if m.state.LastRun.IsZero() {
			m.nextRun = time.Now().UTC()
		} else {
			m.nextRun = m.state.LastRun.Add(time.Duration(settings.IntervalMinutes) * time.Minute)
			if m.nextRun.Before(time.Now()) {
				m.nextRun = time.Now().UTC()
			}
		}
	} else {
		m.nextRun = time.Time{}
	}
	// Ensure auth dir exists when configured for hot-load / mint output.
	authDir := m.resolvedAuthDirLocked()
	_ = os.MkdirAll(authDir, 0o700)
	err = m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return Status{}, err
	}
	m.signalWake()
	return m.Status(), nil
}

func (m *Manager) SetDisabled(id string, disabled bool) (Status, error) {
	m.mu.Lock()
	found := false
	for i := range m.state.Accounts {
		if m.state.Accounts[i].ID == id {
			m.state.Accounts[i].Disabled = disabled
			found = true
			break
		}
	}
	if !found {
		m.mu.Unlock()
		return Status{}, os.ErrNotExist
	}
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return Status{}, err
	}
	return m.Status(), nil
}

func (m *Manager) Delete(id string) (Status, error) {
	m.mu.Lock()
	next := m.state.Accounts[:0]
	found := false
	for _, account := range m.state.Accounts {
		if account.ID == id {
			found = true
			continue
		}
		next = append(next, account)
	}
	if !found {
		m.mu.Unlock()
		return Status{}, os.ErrNotExist
	}
	m.state.Accounts = next
	delete(m.stores, id)
	if len(next) == 0 {
		m.nextRun = time.Time{}
	}
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return Status{}, err
	}
	if err := os.Remove(m.accountPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	m.signalWake()
	return m.Status(), nil
}

func (m *Manager) BulkAction(action string) (BulkResult, Status, error) {
	action = strings.ToLower(strings.TrimSpace(action))
	if action != "disable" && action != "delete" {
		return BulkResult{}, Status{}, fmt.Errorf("批量操作只支持 disable 或 delete")
	}

	m.mu.Lock()
	ids := make([]string, 0)
	for _, account := range m.state.Accounts {
		if accountAbnormal(account) && (action != "disable" || !account.Disabled) {
			ids = append(ids, account.ID)
		}
	}
	if len(ids) == 0 {
		m.mu.Unlock()
		return BulkResult{}, Status{}, fmt.Errorf("当前没有可执行该操作的异常账号")
	}
	result := BulkResult{Action: action, Matched: len(ids)}
	if action == "disable" {
		selected := make(map[string]bool, len(ids))
		for _, id := range ids {
			selected[id] = true
		}
		for i := range m.state.Accounts {
			if selected[m.state.Accounts[i].ID] {
				m.state.Accounts[i].Disabled = true
				result.Updated++
			}
		}
	} else {
		selected := make(map[string]bool, len(ids))
		for _, id := range ids {
			selected[id] = true
			delete(m.stores, id)
		}
		next := m.state.Accounts[:0]
		for _, account := range m.state.Accounts {
			if !selected[account.ID] {
				next = append(next, account)
			}
		}
		m.state.Accounts = next
		result.Updated = len(ids)
		if len(next) == 0 {
			m.nextRun = time.Time{}
		}
	}
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		return BulkResult{}, Status{}, err
	}
	m.mu.Unlock()

	if action == "delete" {
		for _, id := range ids {
			if err := os.Remove(m.accountPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
				result.Failed = append(result.Failed, id+": "+err.Error())
			}
		}
	}
	m.signalWake()
	return result, m.Status(), nil
}

func (m *Manager) Authorized(r *http.Request) bool {
	m.mu.Lock()
	configured := len(m.state.Accounts) > 0
	localAPIKey := m.state.LocalAPIKey
	m.mu.Unlock()
	if !configured || localAPIKey == "" {
		return false
	}
	provided := strings.TrimSpace(r.Header.Get("x-api-key"))
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		provided = strings.TrimSpace(auth[len("Bearer "):])
	}
	return provided != "" && len(provided) == len(localAPIKey) &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(localAPIKey)) == 1
}

func (m *Manager) NextToken(ctx context.Context, sessionID string) (string, string, error) {
	m.mu.Lock()
	accounts := make([]Account, 0, len(m.state.Accounts))
	for _, account := range m.state.Accounts {
		if accountAvailable(account) {
			accounts = append(accounts, account)
		}
	}
	m.mu.Unlock()
	if len(accounts) == 0 {
		return "", "", fmt.Errorf("Grok 号池没有可用账号")
	}
	sort.SliceStable(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	start := int(m.roundRobin.Add(1)-1) % len(accounts)
	if strings.TrimSpace(sessionID) != "" {
		hash := sha256.Sum256([]byte(sessionID))
		start = int(hash[0]) % len(accounts)
	}
	var failures []string
	for offset := 0; offset < len(accounts); offset++ {
		account := accounts[(start+offset)%len(accounts)]
		token, err := m.accountStore(account.ID).Token(ctx)
		if err == nil {
			return token, account.ID, nil
		}
		failures = append(failures, firstNonEmpty(account.Email, account.ID)+": "+err.Error())
		m.recordTokenFailure(account.ID, err)
	}
	return "", "", fmt.Errorf("号池账号 token 均不可用: %s", strings.Join(failures, "; "))
}

func (m *Manager) recordTokenFailure(id string, tokenErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.state.Accounts {
		if m.state.Accounts[i].ID != id {
			continue
		}
		account := &m.state.Accounts[i]
		name, action, reason := classifyTokenFailure(tokenErr)
		account.Classification = name
		account.Action = action
		account.Reason = reason
		account.ErrorMessage = tokenErr.Error()
		account.LastInspected = time.Now().UTC()
		_ = m.saveLocked()
		return
	}
}

func classifyTokenFailure(tokenErr error) (string, string, string) {
	blob := strings.ToLower(tokenErr.Error())
	if strings.Contains(blob, "invalid_grant") || strings.Contains(blob, "已过期") ||
		strings.Contains(blob, "没有 refresh_token") || strings.Contains(blob, "missing refresh") {
		return "reauth", "delete", "token 已失效，需要重新登录"
	}
	return "probe_error", "keep", "token 读取或刷新暂时失败"
}

func (m *Manager) accountPath(id string) string {
	return filepath.Join(m.accountsDir, id+".json")
}

func (m *Manager) accountStore(id string) *grokauth.Store {
	m.mu.Lock()
	defer m.mu.Unlock()
	if store := m.stores[id]; store != nil {
		return store
	}
	store := grokauth.NewStore(m.accountPath(id))
	_ = store.SetProxyURL(m.state.Settings.ProxyURL)
	m.stores[id] = store
	return store
}

func (m *Manager) Transport() http.RoundTripper {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.transport == nil {
		return nil
	}
	return m.transport
}

// clientForTransport builds an HTTP client for pool inspection.
// When transport is nil (no proxy configured), leave Client.Transport unset so
// net/http uses DefaultTransport. Assigning a typed-nil *http.Transport into
// the RoundTripper interface would panic on the first request.
func clientForTransport(transport *http.Transport) *http.Client {
	client := &http.Client{Timeout: 25 * time.Second}
	if transport != nil {
		client.Transport = transport
	}
	return client
}

func credentialID(credential grokauth.Credential) string {
	identity := strings.ToLower(strings.Join([]string{
		credential.Issuer,
		credential.ClientID,
		credential.Email,
		credential.Subject,
	}, "|"))
	if strings.Trim(identity, "|") == "" {
		identity = firstNonEmpty(credential.RefreshToken, credential.AccessToken)
	}
	hash := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(hash[:12])
}

func (m *Manager) signalWake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) scheduler() {
	defer close(m.done)
	for {
		wait := m.schedulerWait()
		timer := time.NewTimer(wait)
		select {
		case <-m.stop:
			timer.Stop()
			return
		case <-m.wake:
			timer.Stop()
			continue
		case <-timer.C:
			_ = m.StartInspection()
		}
	}
}

func (m *Manager) schedulerWait() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.Settings.Enabled || len(m.state.Accounts) == 0 || m.running {
		m.nextRun = time.Time{}
		return 24 * time.Hour
	}
	if m.nextRun.IsZero() {
		if m.state.LastRun.IsZero() {
			m.nextRun = time.Now().UTC()
		} else {
			m.nextRun = m.state.LastRun.Add(time.Duration(m.state.Settings.IntervalMinutes) * time.Minute)
		}
	}
	wait := time.Until(m.nextRun)
	if wait < 0 {
		return 0
	}
	return wait
}
