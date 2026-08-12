package registrar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// yydsProvider talks to the YYDS Mail service (https://maliapi.215.im/v1).
// Auth is X-API-Key: AC-xxxxxx. The /v1/messages/next endpoint long-polls and
// atomically returns the oldest unread message with a server-extracted
// verificationCode, which makes the wait loop a single call per poll.
type yydsProvider struct {
	mu     sync.Mutex
	config Config
	used   map[string]bool
	client *http.Client
}

func newYydsProvider(config Config, used map[string]bool, client *http.Client) (*yydsProvider, error) {
	if strings.TrimSpace(config.YydsURL) == "" {
		return nil, fmt.Errorf("YYDS Mail 需要 API Base URL")
	}
	if strings.TrimSpace(config.YydsAPIKey) == "" {
		return nil, fmt.Errorf("YYDS Mail 需要 API Key")
	}
	// Endpoints below are written starting with /v1 (e.g. /v1/accounts).
	// Accept a base URL with or without the trailing /v1 so both
	// https://maliapi.215.im and https://maliapi.215.im/v1 work.
	baseURL := strings.TrimRight(strings.TrimSpace(config.YydsURL), "/")
	if strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	config.YydsURL = baseURL
	return &yydsProvider{
		config: config,
		used:   used,
		client: client,
	}, nil
}

func (p *yydsProvider) request(ctx context.Context, method, endpoint string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(raw))
	}
	parsed, err := url.Parse(strings.TrimRight(p.config.YydsURL, "/") + endpoint)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-API-Key", strings.TrimSpace(p.config.YydsAPIKey))
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var envelope struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Data    any    `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("YYDS Mail 返回无效 JSON: %w", err)
	}
	if !envelope.Success {
		return fmt.Errorf("YYDS Mail: %s", envelope.Error)
	}
	if out != nil {
		data, err := json.Marshal(envelope.Data)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("YYDS Mail 返回结构不符: %w", err)
		}
	}
	return nil
}

func (p *yydsProvider) Allocate(ctx context.Context) (Mailbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for attempt := 0; attempt < 10; attempt++ {
		local, err := randomText(10)
		if err != nil {
			return nil, err
		}
		payload := map[string]string{"localPart": local}
		if domain := strings.TrimSpace(p.config.DefaultDomains); domain != "" {
			payload["domain"] = domain // fixed domain; subdomain omitted → fixed-domain behavior
		}
		var data struct {
			Address string `json:"address"`
		}
		if err := p.request(ctx, http.MethodPost, "/v1/accounts", payload, &data); err != nil {
			return nil, err
		}
		address := strings.ToLower(strings.TrimSpace(data.Address))
		if address == "" || p.used[address] {
			continue
		}
		p.used[address] = true
		return &yydsMailbox{provider: p, address: address}, nil
	}
	return nil, fmt.Errorf("YYDS Mail 连续返回重复或空地址")
}

type yydsMailbox struct {
	provider *yydsProvider
	address  string
}

func (m *yydsMailbox) Address() string { return m.address }

func (m *yydsMailbox) WaitCode(ctx context.Context, timeout time.Duration, log func(string)) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		wait := 30
		// Long-poll window: never exceed the remaining deadline.
		if remaining < 30*time.Second {
			wait = int(remaining / time.Second)
			if wait < 1 {
				wait = 1
			}
		}
		query := url.Values{"address": {m.address}, "wait": {fmt.Sprintf("%d", wait)}}
		var data struct {
			Message struct {
				VerificationCode string `json:"verificationCode"`
			} `json:"message"`
		}
		err := m.provider.request(ctx, http.MethodGet, "/v1/messages/next?"+query.Encode(), nil, &data)
		if err != nil {
			if log != nil {
				log("YYDS Mail 收件请求失败，继续重试: " + err.Error())
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(1500 * time.Millisecond):
			}
			continue
		}
		if code := strings.TrimSpace(data.Message.VerificationCode); code != "" {
			if log != nil {
				log("已从 YYDS Mail 邮件提取验证码")
			}
			return code, nil
		}
		if log != nil {
			log("等待 YYDS Mail 验证码")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("YYDS Mail 在 %s 内未收到验证码", timeout)
}