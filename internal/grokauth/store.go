package grokauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"grok_switch/internal/netproxy"
	"grok_switch/internal/recovery"
)

const (
	defaultIssuer      = "https://auth.x.ai"
	defaultClientID    = "b1a00492-073a-47ea-816f-4c329264a828"
	defaultUpstreamURL = "https://cli-chat-proxy.grok.com/v1"
	refreshLead        = 5 * time.Minute
	maxCredentialSize  = 1 << 20
	credentialVersion  = 1
)

type Credential struct {
	Version       int       `json:"version"`
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	TokenEndpoint string    `json:"token_endpoint,omitempty"`
	Issuer        string    `json:"issuer,omitempty"`
	ClientID      string    `json:"client_id,omitempty"`
	Email         string    `json:"email,omitempty"`
	Subject       string    `json:"subject,omitempty"`
	LocalAPIKey   string    `json:"local_api_key"`
	Source        string    `json:"source"`
	ImportedAt    time.Time `json:"imported_at"`
	LastRefresh   time.Time `json:"last_refresh,omitempty"`
}

type Status struct {
	Configured   bool      `json:"configured"`
	Email        string    `json:"email,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	NeedsRefresh bool      `json:"needs_refresh"`
	LocalAPIKey  string    `json:"local_api_key,omitempty"`
	Source       string    `json:"source,omitempty"`
	ImportedAt   time.Time `json:"imported_at,omitempty"`
	LastRefresh  time.Time `json:"last_refresh,omitempty"`
}

type Store struct {
	path                 string
	client               *http.Client
	mu                   sync.Mutex
	allowUnsafeEndpoints bool

	// 热路径缓存：Token/Status/Authorized 位于每个本地代理请求的路径上，
	// 逐次读盘 + JSON 解析是纯开销。凭据文件只由本进程写入（单实例），
	// 用 mtime+size 判断是否需要重新读取（与 settings store 一致）。
	cacheValid bool
	cacheValue Credential
	cacheMod   time.Time
	cacheSize  int64
}

func NewStore(path string) *Store {
	return &Store{
		path:   path,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *Store) Path() string { return s.path }

func (s *Store) SetProxyURL(raw string) error {
	_, transport, err := netproxy.BuildTransport(raw)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if transport != nil {
		client.Transport = transport
	}
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
	return nil
}

func (s *Store) Import(raw []byte) (Status, error) {
	if len(raw) == 0 {
		return Status{}, fmt.Errorf("认证 JSON 为空")
	}
	if len(raw) > maxCredentialSize {
		return Status{}, fmt.Errorf("认证 JSON 超过 1 MiB 限制")
	}
	next, err := ParseCredential(raw)
	if err != nil {
		return Status{}, err
	}
	return s.ImportCredential(next)
}

func (s *Store) ImportCredential(next Credential) (Status, error) {
	next = normalizeCredential(next)
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, readErr := s.readLocked(); readErr == nil {
		next.LocalAPIKey = current.LocalAPIKey
	}
	if next.LocalAPIKey == "" {
		localAPIKey, err := newLocalAPIKey()
		if err != nil {
			return Status{}, fmt.Errorf("生成本地 API Key: %w", err)
		}
		next.LocalAPIKey = localAPIKey
	}
	next.Version = credentialVersion
	next.ImportedAt = time.Now().UTC()
	if err := s.writeLocked(next); err != nil {
		return Status{}, err
	}
	return statusFor(next), nil
}

func (s *Store) Status() (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, err := s.readLocked()
	if errors.Is(err, os.ErrNotExist) {
		return Status{Configured: false}, nil
	}
	if err != nil {
		return Status{}, err
	}
	return statusFor(credential), nil
}

func (s *Store) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, err := s.readLocked()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("尚未导入 Grok auth JSON")
		}
		return "", err
	}
	if credential.ExpiresAt.IsZero() || time.Until(credential.ExpiresAt) > refreshLead {
		return credential.AccessToken, nil
	}
	credential, err = s.refreshLocked(ctx, credential)
	if err != nil {
		return "", err
	}
	return credential.AccessToken, nil
}

func (s *Store) Refresh(ctx context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, err := s.readLocked()
	if err != nil {
		return Status{}, err
	}
	credential, err = s.refreshLocked(ctx, credential)
	if err != nil {
		return Status{}, err
	}
	return statusFor(credential), nil
}

func (s *Store) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		s.cacheValid = false
	}
	return err
}

func (s *Store) Authorized(r *http.Request) bool {
	status, err := s.Status()
	if err != nil || !status.Configured || status.LocalAPIKey == "" {
		return false
	}
	provided := strings.TrimSpace(r.Header.Get("x-api-key"))
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		provided = strings.TrimSpace(auth[len("Bearer "):])
	}
	return provided != "" && len(provided) == len(status.LocalAPIKey) &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(status.LocalAPIKey)) == 1
}

func (s *Store) refreshLocked(ctx context.Context, credential Credential) (Credential, error) {
	if credential.RefreshToken == "" {
		return credential, fmt.Errorf("access token 已过期，JSON 中没有 refresh_token")
	}
	endpoint, err := s.resolveTokenEndpoint(ctx, credential)
	if err != nil {
		return credential, err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {firstNonEmpty(credential.ClientID, defaultClientID)},
		"refresh_token": {credential.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return credential, fmt.Errorf("创建 token 刷新请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return credential, fmt.Errorf("刷新 xAI token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCredentialSize))
	if err != nil {
		return credential, fmt.Errorf("读取 token 刷新响应: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return credential, fmt.Errorf("刷新 xAI token 返回 %s: %s", resp.Status, compactError(body))
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		ExpiresAt    string `json:"expires_at"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return credential, fmt.Errorf("解析 token 刷新响应: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return credential, fmt.Errorf("token 刷新响应缺少 access_token")
	}
	credential.AccessToken = strings.TrimSpace(payload.AccessToken)
	if strings.TrimSpace(payload.RefreshToken) != "" {
		credential.RefreshToken = strings.TrimSpace(payload.RefreshToken)
	}
	now := time.Now().UTC()
	credential.LastRefresh = now
	credential.TokenEndpoint = endpoint
	credential.ExpiresAt = parseTime(payload.ExpiresAt)
	if credential.ExpiresAt.IsZero() && payload.ExpiresIn > 0 {
		credential.ExpiresAt = now.Add(time.Duration(payload.ExpiresIn) * time.Second)
	}
	if credential.ExpiresAt.IsZero() {
		credential.ExpiresAt = jwtExpiry(credential.AccessToken)
	}
	if credential.Email == "" {
		credential.Email = jwtStringClaim(firstNonEmpty(payload.IDToken, credential.AccessToken), "email")
	}
	if err := s.writeLocked(credential); err != nil {
		return credential, err
	}
	return credential, nil
}

func (s *Store) resolveTokenEndpoint(ctx context.Context, credential Credential) (string, error) {
	if credential.TokenEndpoint != "" {
		if err := s.validateXAIEndpoint(credential.TokenEndpoint); err != nil {
			return "", err
		}
		return credential.TokenEndpoint, nil
	}
	issuer := strings.TrimRight(firstNonEmpty(credential.Issuer, defaultIssuer), "/")
	discoveryURL := issuer + "/.well-known/openid-configuration"
	if err := s.validateXAIEndpoint(discoveryURL); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("读取 xAI OIDC 配置: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("读取 xAI OIDC 配置返回 %s", resp.Status)
	}
	var discovery struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCredentialSize)).Decode(&discovery); err != nil {
		return "", fmt.Errorf("解析 xAI OIDC 配置: %w", err)
	}
	if err := s.validateXAIEndpoint(discovery.TokenEndpoint); err != nil {
		return "", err
	}
	return strings.TrimSpace(discovery.TokenEndpoint), nil
}

func (s *Store) validateXAIEndpoint(raw string) error {
	if s.allowUnsafeEndpoints {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" {
		return fmt.Errorf("不安全的 xAI OAuth 地址")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "x.ai" && !strings.HasSuffix(host, ".x.ai") {
		return fmt.Errorf("xAI OAuth 地址必须位于 x.ai 域名")
	}
	return nil
}

func (s *Store) readLocked() (Credential, error) {
	var modTime time.Time
	var size int64
	cacheHit := false
	if info, err := os.Stat(s.path); err == nil {
		modTime, size = info.ModTime(), info.Size()
		cacheHit = s.cacheValid && modTime.Equal(s.cacheMod) && size == s.cacheSize
	}
	if cacheHit {
		return s.cacheValue, nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		s.cacheValid = false
		return Credential{}, err
	}
	var credential Credential
	if err := json.Unmarshal(data, &credential); err != nil {
		return s.recoverCorruptLocked(fmt.Errorf("读取 Grok auth store: %w", err))
	}
	if credential.AccessToken == "" || credential.LocalAPIKey == "" {
		return s.recoverCorruptLocked(fmt.Errorf("Grok auth store 不完整"))
	}
	s.cacheValid = true
	s.cacheValue = credential
	s.cacheMod = modTime
	s.cacheSize = size
	return credential, nil
}

func (s *Store) recoverCorruptLocked(cause error) (Credential, error) {
	s.cacheValid = false
	backup, err := recovery.BackupCorrupt(s.path)
	if err != nil {
		return Credential{}, fmt.Errorf("%v; 备份损坏 Grok auth store: %w", cause, err)
	}
	log.Printf("recovered Grok auth store %s after %v; backup=%s", s.path, cause, backup)
	return Credential{}, os.ErrNotExist
}

func (s *Store) writeLocked(credential Credential) error {
	data, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(s.path, append(data, '\n')); err != nil {
		return err
	}
	// 写入成功后同步缓存；Stat 失败则失效缓存，下次读取时重建。
	s.cacheValue = credential
	if info, err := os.Stat(s.path); err == nil {
		s.cacheValid = true
		s.cacheMod = info.ModTime()
		s.cacheSize = info.Size()
	} else {
		s.cacheValid = false
	}
	return nil
}

func ParseCredential(raw []byte) (Credential, error) {
	credentials, err := ParseCredentials(raw)
	if err != nil {
		return Credential{}, err
	}
	return credentials[0], nil
}

func ParseCredentials(raw []byte) ([]Credential, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("认证文件不是有效 JSON: %w", err)
	}
	if provider := lowerString(root["type"]); provider != "" && provider != "xai" && provider != "grok" {
		return nil, fmt.Errorf("认证文件 type=%q，不是 xai/grok", provider)
	}
	if credential, ok := credentialFromMap(root, "cpa-xai"); ok {
		return []Credential{normalizeCredential(credential)}, nil
	}

	keys := make([]string, 0, len(root))
	for key := range root {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var candidates []Credential
	for _, key := range keys {
		entry, ok := root[key].(map[string]any)
		if !ok {
			continue
		}
		if credential, found := credentialFromMap(entry, "grok-auth-json"); found {
			candidates = append(candidates, normalizeCredential(credential))
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("未找到 xAI access token；支持 CPA xai-*.json 或 Grok CLI auth.json")
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ExpiresAt.After(candidates[j].ExpiresAt)
	})
	return candidates, nil
}

func credentialFromMap(entry map[string]any, source string) (Credential, bool) {
	accessToken := firstNonEmpty(stringValue(entry["access_token"]), stringValue(entry["key"]))
	if accessToken == "" {
		return Credential{}, false
	}
	expiresAt := parseTime(firstNonEmpty(
		stringValue(entry["expired"]),
		stringValue(entry["expires_at"]),
	))
	if expiresAt.IsZero() {
		expiresAt = jwtExpiry(accessToken)
	}
	issuer := firstNonEmpty(stringValue(entry["issuer"]), stringValue(entry["oidc_issuer"]), defaultIssuer)
	credential := Credential{
		AccessToken:   accessToken,
		RefreshToken:  stringValue(entry["refresh_token"]),
		ExpiresAt:     expiresAt,
		TokenEndpoint: stringValue(entry["token_endpoint"]),
		Issuer:        issuer,
		ClientID:      firstNonEmpty(stringValue(entry["client_id"]), stringValue(entry["oidc_client_id"]), defaultClientID),
		Email:         stringValue(entry["email"]),
		Subject:       firstNonEmpty(stringValue(entry["sub"]), jwtStringClaim(accessToken, "sub")),
		Source:        source,
	}
	return credential, true
}

func normalizeCredential(credential Credential) Credential {
	credential.Version = credentialVersion
	credential.AccessToken = strings.TrimSpace(credential.AccessToken)
	credential.RefreshToken = strings.TrimSpace(credential.RefreshToken)
	credential.TokenEndpoint = strings.TrimSpace(credential.TokenEndpoint)
	credential.Issuer = strings.TrimRight(strings.TrimSpace(firstNonEmpty(credential.Issuer, defaultIssuer)), "/")
	credential.ClientID = strings.TrimSpace(firstNonEmpty(credential.ClientID, defaultClientID))
	credential.Email = strings.TrimSpace(credential.Email)
	credential.Subject = strings.TrimSpace(credential.Subject)
	if credential.Email == "" {
		credential.Email = jwtStringClaim(credential.AccessToken, "email")
	}
	return credential
}

func statusFor(credential Credential) Status {
	return Status{
		Configured:   true,
		Email:        credential.Email,
		ExpiresAt:    credential.ExpiresAt,
		NeedsRefresh: !credential.ExpiresAt.IsZero() && time.Until(credential.ExpiresAt) <= refreshLead,
		LocalAPIKey:  credential.LocalAPIKey,
		Source:       credential.Source,
		ImportedAt:   credential.ImportedAt,
		LastRefresh:  credential.LastRefresh,
	}
}

func newLocalAPIKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "gsk-local-" + hex.EncodeToString(buf), nil
}

func parseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
		if unix > 1e12 {
			unix /= 1000
		}
		return time.Unix(unix, 0).UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func jwtExpiry(token string) time.Time {
	claim := jwtClaim(token, "exp")
	switch value := claim.(type) {
	case float64:
		return time.Unix(int64(value), 0).UTC()
	case json.Number:
		if unix, err := value.Int64(); err == nil {
			return time.Unix(unix, 0).UTC()
		}
	}
	return time.Time{}
}

func jwtStringClaim(token, name string) string {
	value, _ := jwtClaim(token, name).(string)
	return strings.TrimSpace(value)
}

func jwtClaim(token, name string) any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	var claims map[string]any
	if err := decoder.Decode(&claims); err != nil {
		return nil
	}
	return claims[name]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func lowerString(value any) string { return strings.ToLower(stringValue(value)) }

func compactError(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	if text == "" {
		return "空响应"
	}
	return text
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

func UpstreamURL() string { return defaultUpstreamURL }
