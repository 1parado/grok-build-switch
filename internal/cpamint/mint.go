package cpamint

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"grok_switch/internal/netproxy"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusPolling   Status = "polling"
	StatusSuccess   Status = "success"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

type Session struct {
	ID                       string    `json:"id"`
	Status                   Status    `json:"status"`
	UserCode                 string    `json:"user_code,omitempty"`
	VerificationURI          string    `json:"verification_uri,omitempty"`
	VerificationURIComplete  string    `json:"verification_uri_complete,omitempty"`
	Email                    string    `json:"email,omitempty"`
	Path                     string    `json:"path,omitempty"`
	Error                    string    `json:"error,omitempty"`
	BrowserOpened            bool      `json:"browser_opened"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
	ExpiresAt                time.Time `json:"expires_at,omitempty,omitzero"`
}

type StartOptions struct {
	Email     string
	AuthDir   string
	ProxyURL  string
	BaseURL   string
	OpenBrowser bool
}

type Service struct {
	mu       sync.Mutex
	sessions map[string]*liveSession
}

type liveSession struct {
	public Session
	cancel context.CancelFunc
	raw    []byte
}

func NewService() *Service {
	return &Service{sessions: make(map[string]*liveSession)}
}

func (s *Service) Start(opts StartOptions) (Session, error) {
	authDir := strings.TrimSpace(opts.AuthDir)
	if authDir == "" {
		return Session{}, fmt.Errorf("请先设置 CPA 认证目录")
	}
	client, err := httpClient(opts.ProxyURL)
	if err != nil {
		return Session{}, err
	}
	device, err := requestDeviceCode(context.Background(), client)
	if err != nil {
		return Session{}, err
	}
	id, err := newSessionID()
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	sess := &liveSession{
		public: Session{
			ID:                      id,
			Status:                  StatusPolling,
			UserCode:                device.UserCode,
			VerificationURI:         device.VerificationURI,
			VerificationURIComplete: device.VerificationURIComplete,
			Email:                   strings.TrimSpace(opts.Email),
			CreatedAt:               now,
			UpdatedAt:               now,
			ExpiresAt:               now.Add(time.Duration(device.ExpiresIn) * time.Second),
		},
		cancel: cancel,
	}
	if opts.OpenBrowser {
		if openErr := openURL(device.VerificationURIComplete); openErr == nil {
			sess.public.BrowserOpened = true
		} else {
			sess.public.Error = "未能自动打开浏览器，请手动打开验证链接"
		}
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	go s.poll(ctx, id, client, device, opts)
	return s.copy(id)
}

func (s *Service) Get(id string) (Session, error) {
	return s.copy(id)
}

func (s *Service) Latest() (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *liveSession
	for _, sess := range s.sessions {
		if best == nil || sess.public.CreatedAt.After(best.public.CreatedAt) {
			best = sess
		}
	}
	if best == nil {
		return Session{}, false
	}
	return best.public, true
}

func (s *Service) Cancel(id string) (Session, error) {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return Session{}, fmt.Errorf("铸造会话不存在")
	}
	if sess.public.Status == StatusPolling || sess.public.Status == StatusPending {
		sess.public.Status = StatusCancelled
		sess.public.UpdatedAt = time.Now().UTC()
		sess.public.Error = "已取消"
		if sess.cancel != nil {
			sess.cancel()
		}
	}
	out := sess.public
	s.mu.Unlock()
	return out, nil
}

func (s *Service) RawCredential(id string) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, "", fmt.Errorf("铸造会话不存在")
	}
	if sess.public.Status != StatusSuccess || len(sess.raw) == 0 {
		return nil, "", fmt.Errorf("铸造尚未成功")
	}
	return append([]byte(nil), sess.raw...), sess.public.Path, nil
}

func (s *Service) poll(ctx context.Context, id string, client *http.Client, device deviceCodeResponse, opts StartOptions) {
	token, err := pollDeviceToken(ctx, client, device)
	if err != nil {
		s.fail(id, err)
		return
	}
	auth, err := BuildAuthFile(opts.Email, token.AccessToken, token.RefreshToken, token.IDToken, token.ExpiresIn, opts.BaseURL)
	if err != nil {
		s.fail(id, err)
		return
	}
	path, raw, err := WriteAuthFile(opts.AuthDir, auth)
	if err != nil {
		s.fail(id, err)
		return
	}
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	if sess.public.Status == StatusCancelled {
		s.mu.Unlock()
		return
	}
	sess.public.Status = StatusSuccess
	sess.public.Path = path
	sess.public.Email = firstNonEmpty(auth.Email, sess.public.Email)
	sess.public.Error = ""
	sess.public.UpdatedAt = time.Now().UTC()
	sess.raw = raw
	s.mu.Unlock()
}

func (s *Service) fail(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return
	}
	if sess.public.Status == StatusCancelled {
		return
	}
	sess.public.Status = StatusFailed
	sess.public.Error = err.Error()
	sess.public.UpdatedAt = time.Now().UTC()
}

func (s *Service) copy(id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return Session{}, fmt.Errorf("铸造会话不存在")
	}
	return sess.public, nil
}

type deviceCodeResponse struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
	Interval                int
}

type tokenResult struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresIn    int
}

func requestDeviceCode(ctx context.Context, client *http.Client) (deviceCodeResponse, error) {
	form := url.Values{
		"client_id": {ClientID},
		"scope":     {Scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return deviceCodeResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grok-switch-cpamint/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return deviceCodeResponse{}, fmt.Errorf("请求 device code 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return deviceCodeResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return deviceCodeResponse{}, fmt.Errorf("device code 返回 %s: %s", resp.Status, compactBody(body))
	}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return deviceCodeResponse{}, fmt.Errorf("解析 device code 响应: %w", err)
	}
	if strings.TrimSpace(payload.DeviceCode) == "" || strings.TrimSpace(payload.UserCode) == "" {
		return deviceCodeResponse{}, fmt.Errorf("device code 响应缺少字段")
	}
	vuri := firstNonEmpty(payload.VerificationURI, "https://accounts.x.ai/oauth2/device")
	vcomplete := firstNonEmpty(payload.VerificationURIComplete, vuri+"?user_code="+payload.UserCode)
	expiresIn := payload.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 1800
	}
	interval := payload.Interval
	if interval < 1 {
		interval = 5
	}
	return deviceCodeResponse{
		DeviceCode:              strings.TrimSpace(payload.DeviceCode),
		UserCode:                strings.TrimSpace(payload.UserCode),
		VerificationURI:         vuri,
		VerificationURIComplete: vcomplete,
		ExpiresIn:               expiresIn,
		Interval:                interval,
	}, nil
}

func pollDeviceToken(ctx context.Context, client *http.Client, device deviceCodeResponse) (tokenResult, error) {
	deadline := time.Now().Add(time.Duration(device.ExpiresIn-5) * time.Second)
	if deadline.Before(time.Now().Add(30 * time.Second)) {
		deadline = time.Now().Add(30 * time.Second)
	}
	sleepFor := time.Duration(device.Interval) * time.Second
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return tokenResult{}, fmt.Errorf("已取消")
		}
		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {device.DeviceCode},
			"client_id":   {ClientID},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return tokenResult{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "grok-switch-cpamint/1.0")
		resp, err := client.Do(req)
		if err != nil {
			select {
			case <-ctx.Done():
				return tokenResult{}, fmt.Errorf("已取消")
			case <-time.After(sleepFor):
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var payload struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				IDToken      string `json:"id_token"`
				ExpiresIn    int    `json:"expires_in"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return tokenResult{}, fmt.Errorf("解析 token 响应: %w", err)
			}
			if strings.TrimSpace(payload.AccessToken) == "" {
				return tokenResult{}, fmt.Errorf("token 响应缺少 access_token")
			}
			if strings.TrimSpace(payload.RefreshToken) == "" {
				return tokenResult{}, fmt.Errorf("token 响应缺少 refresh_token")
			}
			return tokenResult{
				AccessToken:  strings.TrimSpace(payload.AccessToken),
				RefreshToken: strings.TrimSpace(payload.RefreshToken),
				IDToken:      strings.TrimSpace(payload.IDToken),
				ExpiresIn:    payload.ExpiresIn,
			}, nil
		}
		var errBody struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &errBody)
		switch errBody.Error {
		case "authorization_pending", "slow_down":
			if errBody.Error == "slow_down" {
				sleepFor += 2 * time.Second
			}
		case "access_denied", "expired_token":
			return tokenResult{}, fmt.Errorf("设备授权失败: %s", firstNonEmpty(errBody.ErrorDescription, errBody.Error))
		default:
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && errBody.Error != "" {
				return tokenResult{}, fmt.Errorf("设备授权失败: %s", firstNonEmpty(errBody.ErrorDescription, errBody.Error, compactBody(body)))
			}
		}
		select {
		case <-ctx.Done():
			return tokenResult{}, fmt.Errorf("已取消")
		case <-time.After(sleepFor):
		}
	}
	return tokenResult{}, fmt.Errorf("等待设备授权超时，请重新开始铸造")
}

func httpClient(proxyURL string) (*http.Client, error) {
	_, transport, err := netproxy.BuildTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 45 * time.Second}
	if transport != nil {
		client.Transport = transport
	}
	return client, nil
}

func openURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("empty url")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", raw)
	case "darwin":
		cmd = exec.Command("open", raw)
	default:
		cmd = exec.Command("xdg-open", raw)
	}
	return cmd.Start()
}

func newSessionID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func compactBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 240 {
		return text[:240] + "…"
	}
	return text
}
