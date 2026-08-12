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

// gptmailProvider talks to a GPTMail-compatible temporary mailbox service
// (e.g. https://mail.chatgpt.org.uk). All endpoints require the X-API-Key
// header; responses are uniformly { success, data, error }.
type gptmailProvider struct {
	mu     sync.Mutex
	config Config
	used   map[string]bool
	client *http.Client
	domain string
}

func newGptmailProvider(config Config, used map[string]bool, client *http.Client) (*gptmailProvider, error) {
	if strings.TrimSpace(config.GptmailURL) == "" {
		return nil, fmt.Errorf("GPTMail 需要 API Base URL")
	}
	if strings.TrimSpace(config.GptmailAPIKey) == "" {
		return nil, fmt.Errorf("GPTMail 需要 API Key")
	}
	return &gptmailProvider{
		config: config,
		used:   used,
		client: client,
	}, nil
}

type gptmailEnvelope struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    any    `json:"data"`
}

func (p *gptmailProvider) request(ctx context.Context, method, endpoint string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(strings.TrimRight(p.config.GptmailURL, "/") + endpoint)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", strings.TrimSpace(p.config.GptmailAPIKey))
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var envelope gptmailEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("GPTMail 返回无效 JSON: %w", err)
	}
	if !envelope.Success {
		return fmt.Errorf("GPTMail: %s", envelope.Error)
	}
	rawData, err := json.Marshal(envelope.Data)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(rawData, out); err != nil {
			return fmt.Errorf("GPTMail 返回结构不符: %w", err)
		}
	}
	return nil
}

func (p *gptmailProvider) Allocate(ctx context.Context) (Mailbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for attempt := 0; attempt < 10; attempt++ {
		var data struct {
			Email string `json:"email"`
		}
		var payload any
		if p.domain != "" {
			payload = map[string]string{"domain": p.domain}
		}
		if err := p.request(ctx, http.MethodPost, "/api/generate-email", payload, &data); err != nil {
			return nil, err
		}
		address := strings.ToLower(strings.TrimSpace(data.Email))
		if address == "" || p.used[address] {
			continue
		}
		p.used[address] = true
		if at := strings.LastIndex(address, "@"); at >= 0 {
			p.domain = address[at+1:]
		}
		return &gptmailMailbox{provider: p, address: address}, nil
	}
	return nil, fmt.Errorf("GPTMail 连续返回重复或空地址")
}

type gptmailMailbox struct {
	provider *gptmailProvider
	address  string
}

func (m *gptmailMailbox) Address() string { return m.address }

func (m *gptmailMailbox) WaitCode(ctx context.Context, timeout time.Duration, log func(string)) (string, error) {
	deadline := time.Now().Add(timeout)
	seen := map[string]bool{}
	for time.Now().Before(deadline) {
		var data any
		query := url.Values{"email": {m.address}}
		if err := m.provider.request(ctx, http.MethodGet, "/api/emails?"+query.Encode(), nil, &data); err != nil {
			if log != nil {
				log("GPTMail 收件请求失败，继续重试: " + err.Error())
			}
		} else {
			messages := responseItems(data)
			for index := len(messages) - 1; index >= 0; index-- {
				message := messages[index]
				id := messageID(message)
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				// List responses already carry a plain-text content snippet which
				// usually contains the code; try it before fetching the detail.
				if code := extractVerificationCode(messageChunks(message)...); code != "" {
					if log != nil {
						log("已从 GPTMail 邮件提取验证码")
					}
					return code, nil
				}
				var detail any
				if err := m.provider.request(ctx, http.MethodGet, "/api/email/"+url.PathEscape(id), nil, &detail); err != nil {
					if log != nil {
						log("GPTMail 邮件详情读取失败: " + err.Error())
					}
					continue
				}
				if code := extractVerificationCode(messageChunks(responseObject(detail))...); code != "" {
					if log != nil {
						log("已从 GPTMail 邮件提取验证码")
					}
					return code, nil
				}
			}
		}
		if log != nil {
			log("等待 GPTMail 验证码")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("GPTMail 在 %s 内未收到验证码", timeout)
}