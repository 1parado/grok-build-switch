package webpool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
)

// Manager 管理 Web 通道号池：账号（SSO cookie 组）的导入/轮换/隔离，
// 以及对 grok.com 网页 API 的请求执行。并发安全；http.Client 在设置
// 变更时整体替换（与 grokpool 相同的原子指针模式）。
type Manager struct {
	dir         string
	indexPath   string
	accountsDir string

	mu    sync.Mutex
	state persistedState

	client atomic.Pointer[http.Client]
	// roundRobin 轮换游标。
	roundRobin atomic.Uint64

	// upstreamBase 可在测试中替换。
	upstreamBase string

	// statsig 签名器（x-statsig-id）。
	statsig *statsigSigner

	// tlsClient 是 Chrome 152 指纹的 tls-client（含内建 h2 帧指纹）。
	tlsClient tls_client.HttpClient
	tlsMu     sync.Mutex

	// browserProxy 是浏览器代理模式（走真 Chrome 过 CF 四道防线）。
	browserProxy *BrowserProxy
	// browserPath 由注册机设置传入。
	browserPath string
}

// NewManager 构造并加载 <dir>/webpool.json。dir 通常为 <dataDir>/webpool。
func NewManager(dir string) (*Manager, error) {
	m := &Manager{
		dir:          dir,
		indexPath:    filepath.Join(dir, "webpool.json"),
		accountsDir:  filepath.Join(dir, "accounts"),
		upstreamBase: "https://grok.com",
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	m.rebuildClientLocked()
	m.statsig = newStatsigSigner("")
	m.browserProxy = NewBrowserProxy(m.browserPath, m.state.Settings.ProxyURL)
	return m, nil
}

// SetBrowserPath 设置 Chrome 路径（从注册机配置传入）。
func (m *Manager) SetBrowserPath(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.browserPath = strings.TrimSpace(path)
}

// Browser 暴露浏览器代理（server 层调 Chat 用）。
func (m *Manager) Browser() *BrowserProxy {
	return m.browserProxy
}

// SetUpstreamBase 仅供测试注入假上游。
func (m *Manager) SetUpstreamBase(base string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upstreamBase = base
}

func (m *Manager) rebuildClientLocked() {
	// Chrome 152 完整指纹（TLS + h2 帧）：bogdanfinn/tls-client。
	client, err := newChromeClient(m.state.Settings.ProxyURL)
	if err == nil {
		m.tlsMu.Lock()
		m.tlsClient = client
		m.tlsMu.Unlock()
	}
	// 同时更新 http.Client（测试 / http 目标用）。
	transport, err := buildWebTransport(m.state.Settings.ProxyURL)
	if err != nil {
		transport, _ = buildWebTransport("")
		if transport == nil {
			return
		}
	}
	m.client.Store(&http.Client{Transport: transport})
}

// UpstreamBase 返回 grok.com 基址（测试可替换）。
func (m *Manager) UpstreamBase() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.upstreamBase
}

func (m *Manager) httpClient() *http.Client {
	if c := m.client.Load(); c != nil {
		return c
	}
	return http.DefaultClient
}

// Status 返回全量状态（账号列表副本）。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	accounts := make([]Account, len(m.state.Accounts))
	copy(accounts, m.state.Accounts)
	return Status{
		Settings:    m.state.Settings,
		LocalAPIKey: m.state.LocalAPIKey,
		Accounts:    accounts,
		Summary:     summarizeAccounts(accounts, time.Now()),
	}
}

// SharedClearance 返回设置级 Cloudflare 通行证（空 = 无兜底）。
func (m *Manager) SharedClearance() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.TrimSpace(m.state.Settings.CFClearance)
}

// SharedUserAgent 返回 clearance 绑定的 UA（空 = 默认）。
func (m *Manager) SharedUserAgent() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.TrimSpace(m.state.Settings.ClearanceUserAgent)
}

// LocalAPIKey 暴露本地 key（服务端鉴权用）。
func (m *Manager) LocalAPIKey() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.LocalAPIKey
}

// UpdateSettings 更新设置并重建出口 client。
func (m *Manager) UpdateSettings(settings Settings) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Settings = normalizeSettings(settings)
	if err := m.saveLocked(); err != nil {
		return m.statusLocked(), err
	}
	m.rebuildClientLocked()
	return m.statusLocked(), nil
}

// statusLocked 在已持锁时组装 Status（不得再调用会抢锁的 Status）。
func (m *Manager) statusLocked() Status {
	accounts := make([]Account, len(m.state.Accounts))
	copy(accounts, m.state.Accounts)
	return Status{
		Settings:    m.state.Settings,
		LocalAPIKey: m.state.LocalAPIKey,
		Accounts:    accounts,
		Summary:     summarizeAccounts(accounts, time.Now()),
	}
}

// Authorized 校验 /web/v1 请求的 Bearer/x-api-key。
func (m *Manager) Authorized(r *http.Request) bool {
	key := m.LocalAPIKey()
	if key == "" {
		return false
	}
	provided := strings.TrimSpace(r.Header.Get("x-api-key"))
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		provided = strings.TrimSpace(auth[len("Bearer "):])
	}
	return provided != "" && subtleEqual(provided, key)
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// accountID 由 SSO cookie 值派生（稳定去重键）。
func accountID(sso string) string {
	sum := sha256.Sum256([]byte(sso))
	return hex.EncodeToString(sum[:12])
}

// loadAccountFile 读取账号 cookie 文件。
func (m *Manager) loadAccountFile(id string) (*accountFile, error) {
	raw, err := os.ReadFile(filepath.Join(m.accountsDir, id+".json"))
	if err != nil {
		return nil, err
	}
	var file accountFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	return &file, nil
}

func (m *Manager) saveAccountFile(file *accountFile) error {
	file.Version = poolVersion
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(m.accountsDir, fileAccountName(file)), append(data, '\n'))
}

func fileAccountName(file *accountFile) string {
	id := accountID(file.Cookies.SSO)
	return id + ".json"
}

// nextAccount 轮换选择一个可用账号；找不到返回空串。
func (m *Manager) nextAccount() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	accounts := m.state.Accounts
	if len(accounts) == 0 {
		return ""
	}
	now := time.Now()
	start := m.roundRobin.Add(1)
	for i := 0; i < len(accounts); i++ {
		account := accounts[int(start+uint64(i))%len(accounts)]
		if accountAvailable(account, now) {
			return account.ID
		}
	}
	return ""
}

// lookupAccount 返回账号副本。
func (m *Manager) lookupAccount(id string) (Account, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, account := range m.state.Accounts {
		if account.ID == id {
			return account, true
		}
	}
	return Account{}, false
}

// Next 返回一个可用账号的 cookie 组与账号 ID。
func (m *Manager) Next() (*CookieSet, string, error) {
	id := m.nextAccount()
	if id == "" {
		return nil, "", fmt.Errorf("Web 通道号池没有可用账号")
	}
	file, err := m.loadAccountFile(id)
	if err != nil {
		return nil, "", fmt.Errorf("读取 Web 通道账号凭据: %w", err)
	}
	cookies := file.Cookies
	return &cookies, id, nil
}

// ExcludeNext 在排除列表之外选择（故障转移用）。
func (m *Manager) ExcludeNext(exclude map[string]bool) (*CookieSet, string, error) {
	m.mu.Lock()
	accounts := make([]Account, len(m.state.Accounts))
	copy(accounts, m.state.Accounts)
	now := time.Now()
	m.mu.Unlock()
	for _, account := range accounts {
		if exclude[account.ID] || !accountAvailable(account, now) {
			continue
		}
		file, err := m.loadAccountFile(account.ID)
		if err != nil {
			continue
		}
		cookies := file.Cookies
		return &cookies, account.ID, nil
	}
	return nil, "", fmt.Errorf("Web 通道号池没有可用账号")
}

// Outcome 描述一次请求的结果，供 Observe 分类。
type Outcome int

const (
	OutcomeSuccess Outcome = iota
	OutcomeRateLimited
	OutcomeBlocked
	OutcomeFailure
)

// Observe 记录一次账号使用结果：成功/429 限流/403 屏蔽/其它失败。
func (m *Manager) Observe(id string, outcome Outcome, note string, elapsedMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for i := range m.state.Accounts {
		if m.state.Accounts[i].ID != id {
			continue
		}
		acc := &m.state.Accounts[i]
		acc.LastUsedAt = now
		acc.LastUsedMs = elapsedMs
		switch outcome {
		case OutcomeSuccess:
			acc.SuccessCount++
			acc.Class = "healthy"
			acc.ClassNote = ""
			acc.ClassAt = now
			acc.RateLimitedAt = time.Time{}
		case OutcomeRateLimited:
			acc.FailureCount++
			acc.Class = "rate_limited"
			acc.ClassNote = firstNonEmptyStr(note, "上游 429")
			acc.ClassAt = now
			acc.RateLimitedAt = now
		case OutcomeBlocked:
			acc.FailureCount++
			acc.Class = "blocked"
			acc.ClassNote = firstNonEmptyStr(note, "Cloudflare/账号被屏蔽")
			acc.ClassAt = now
			acc.BlockedAt = now
		case OutcomeFailure:
			acc.FailureCount++
		}
		break
	}
	_ = m.saveLocked()
}

// RefreshCookies 用请求途中拿到的 Set-Cookie 更新账号凭据（cf_bm /
// cf_clearance 滚动刷新，SSO 保持不变则不动文件）。
func (m *Manager) RefreshCookies(id string, updates map[string]string) {
	if len(updates) == 0 {
		return
	}
	file, err := m.loadAccountFile(id)
	if err != nil {
		return
	}
	changed := false
	apply := func(dst *string, key string) {
		if v := strings.TrimSpace(updates[key]); v != "" && v != *dst {
			*dst = v
			changed = true
		}
	}
	apply(&file.Cookies.CFBM, "__cf_bm")
	apply(&file.Cookies.CFClearance, "cf_clearance")
	apply(&file.Cookies.CUID, "__cuid")
	apply(&file.Cookies.XAIAnonID, "xai_anon_id")
	if !changed {
		return
	}
	if err := m.saveAccountFile(file); err == nil {
		fmt.Fprintf(os.Stderr, "grok_switch: webpool 刷新账号 %s 的 cookie（%d 项）\n", id, len(updates))
	}
}

// Delete 移除账号。
func (m *Manager) Delete(id string) (Status, error) {
	m.mu.Lock()
	next := make([]Account, 0, len(m.state.Accounts))
	for _, account := range m.state.Accounts {
		if account.ID != id {
			next = append(next, account)
		}
	}
	m.state.Accounts = next
	if err := m.saveLocked(); err != nil {
		return m.statusLocked(), err
	}
	m.mu.Unlock()
	_ = os.Remove(filepath.Join(m.accountsDir, id+".json"))
	return m.Status(), nil
}

// SetDisabled 启用/停用账号。
func (m *Manager) SetDisabled(id string, disabled bool) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.state.Accounts {
		if m.state.Accounts[i].ID == id {
			m.state.Accounts[i].Disabled = disabled
			break
		}
	}
	if err := m.saveLocked(); err != nil {
		return m.statusLocked(), err
	}
	return m.statusLocked(), nil
}

// ImportManual 导入手动粘贴的 SSO cookie（grok2api 风格：一行一枚，
// 允许 sso=xxx 或裸值，支持逗号分隔多枚）。
func (m *Manager) ImportManual(raw string) (imported int, status Status, err error) {
	count := 0
	for _, line := range strings.Split(raw, "\n") {
		for _, piece := range strings.Split(line, ",") {
			piece = strings.TrimSpace(piece)
			piece = strings.TrimPrefix(piece, "sso=")
			piece = strings.TrimPrefix(piece, "sso:")
			piece = strings.TrimSpace(piece)
			if piece == "" {
				continue
			}
			if _, _, err := m.upsertCookie(CookieSet{SSO: piece, SSORW: piece}, "", "manual", ""); err == nil {
				count++
			}
		}
	}
	return count, m.Status(), nil
}

// upsertCookie 新增或刷新一个 cookie 组账号。
func (m *Manager) upsertCookie(cookies CookieSet, email, source, registrarFile string) (string, bool, error) {
	if strings.TrimSpace(cookies.SSO) == "" {
		return "", false, fmt.Errorf("sso cookie 为空")
	}
	id := accountID(cookies.SSO)
	file := &accountFile{
		Version:             poolVersion,
		Email:               email,
		Source:              source,
		Cookies:             cookies,
		RegistrarCookieFile: registrarFile,
		SyncedAt:            time.Now(),
	}
	if err := m.saveAccountFile(file); err != nil {
		return "", false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.state.Accounts {
		if m.state.Accounts[i].ID == id {
			// 已存在：只更新凭据相关字段，保留运行状态。
			return id, false, nil
		}
	}
	m.state.Accounts = append(m.state.Accounts, Account{
		ID:        id,
		Email:     email,
		Source:    source,
		Class:     "unknown",
		CreatedAt: time.Now(),
	})
	if err := m.saveLocked(); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// SyncRegistrarCookies 扫描注册机 cookie 快照目录，导入/刷新全部账号。
// 返回（新增数, 刷新数, 总数）。
func (m *Manager) SyncRegistrarCookies(cookieDir string) (added, refreshed, total int, err error) {
	entries, readErr := os.ReadDir(cookieDir)
	if readErr != nil {
		return 0, 0, 0, readErr
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		raw, rErr := os.ReadFile(filepath.Join(cookieDir, ent.Name()))
		if rErr != nil {
			continue
		}
		var snapshot struct {
			Email   string `json:"email"`
			Cookies []struct {
				Name   string `json:"name"`
				Value  string `json:"value"`
				Domain string `json:"domain"`
			} `json:"cookies"`
		}
		if json.Unmarshal(raw, &snapshot) != nil {
			continue
		}
		var set CookieSet
		for _, c := range snapshot.Cookies {
			dom := strings.ToLower(c.Domain)
			if !strings.Contains(dom, "grok.com") && !strings.Contains(dom, "x.ai") {
				continue
			}
			switch c.Name {
			case "sso":
				set.SSO = c.Value
			case "sso-rw":
				set.SSORW = c.Value
			case "cf_clearance":
				set.CFClearance = c.Value
			case "__cf_bm":
				set.CFBM = c.Value
			case "__cuid":
				set.CUID = c.Value
			case "xai_anon_id":
				set.XAIAnonID = c.Value
			}
		}
		if set.SSO == "" {
			continue
		}
		if set.SSORW == "" {
			set.SSORW = set.SSO
		}
		_, created, upErr := m.upsertCookie(set, snapshot.Email, "registrar-cookie", ent.Name())
		if upErr != nil {
			continue
		}
		total++
		if created {
			added++
		} else {
			refreshed++
		}
	}
	return added, refreshed, total, nil
}

// CookieHeader 拼装账号的 Cookie 请求头值。
func CookieHeader(set *CookieSet) string {
	var b strings.Builder
	b.WriteString("sso-rw=")
	b.WriteString(set.SSORW)
	b.WriteString(";sso=")
	b.WriteString(set.SSO)
	if set.CFClearance != "" {
		b.WriteString(";cf_clearance=")
		b.WriteString(set.CFClearance)
	}
	if set.CFBM != "" {
		b.WriteString(";__cf_bm=")
		b.WriteString(set.CFBM)
	}
	if set.CUID != "" {
		b.WriteString(";__cuid=")
		b.WriteString(set.CUID)
	}
	if set.XAIAnonID != "" {
		b.WriteString(";xai_anon_id=")
		b.WriteString(set.XAIAnonID)
	}
	return b.String()
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Do 执行请求：https 走 Chrome 152 tls-client，其余走 http.Client。
func (m *Manager) Do(req *http.Request) (*http.Response, error) {
	if strings.EqualFold(req.URL.Scheme, "https") {
		m.tlsMu.Lock()
		client := m.tlsClient
		m.tlsMu.Unlock()
		if client != nil {
			return (&tlsRoundTripper{client: client}).RoundTrip(req)
		}
	}
	return m.httpClient().Do(req)
}
