package registrar

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"grok_switch/internal/netproxy"
)

type MailProvider interface {
	Allocate(context.Context) (Mailbox, error)
}

type Mailbox interface {
	Address() string
	WaitCode(context.Context, time.Duration, func(string)) (string, error)
}

type credentialSnapshotter interface {
	CredentialsText() string
}

var codePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b([A-Z0-9]{3}-[A-Z0-9]{3})\b`),
	regexp.MustCompile(`(?i)(?:verification|confirm|security|验证码|确认码|校验码)[^0-9]{0,40}([0-9]{6})`),
	regexp.MustCompile(`\b([0-9]{6})\b`),
}

func extractVerificationCode(values ...string) string {
	text := strings.Join(values, "\n")
	for _, pattern := range codePatterns {
		if match := pattern.FindStringSubmatch(text); len(match) > 1 {
			return match[1]
		}
	}
	return ""
}

func newMailProvider(config Config, used map[string]bool) (MailProvider, error) {
	client, err := registrarHTTPClient(config.ProxyURL)
	if err != nil {
		return nil, err
	}
	switch config.EmailProvider {
	case "hotmail":
		return newHotmailProvider(config.HotmailAccountsText, config.HotmailMaxAliases, used, client)
	case "cloudmail":
		return newCloudmailProvider(config, used, client)
	case "cloudflare":
		return newCloudflareProvider(config, used, client)
	default:
		return nil, fmt.Errorf("不支持的邮箱服务商: %s", config.EmailProvider)
	}
}

func registrarHTTPClient(proxyURL string) (*http.Client, error) {
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

func randomText(length int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buffer := make([]byte, length)
	random := make([]byte, length)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	for i, value := range random {
		buffer[i] = alphabet[int(value)%len(alphabet)]
	}
	return string(buffer), nil
}

type cloudmailProvider struct {
	mu      sync.Mutex
	config  Config
	used    map[string]bool
	client  *http.Client
	token   string
	domains []string
	next    int
}

func newCloudmailProvider(config Config, used map[string]bool, client *http.Client) (*cloudmailProvider, error) {
	var domains []string
	for _, domain := range regexp.MustCompile(`[,，\s]+`).Split(config.DefaultDomains, -1) {
		if domain = strings.TrimSpace(domain); domain != "" {
			domains = append(domains, domain)
		}
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("CloudMail 域名为空")
	}
	return &cloudmailProvider{config: config, used: used, client: client, domains: domains}, nil
}

func (p *cloudmailProvider) Allocate(ctx context.Context) (Mailbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for attempt := 0; attempt < 200; attempt++ {
		local, err := randomText(10)
		if err != nil {
			return nil, err
		}
		domain := p.domains[p.next%len(p.domains)]
		p.next++
		address := strings.ToLower(local + "@" + domain)
		if p.used[address] {
			continue
		}
		p.used[address] = true
		return &cloudmailMailbox{provider: p, address: address}, nil
	}
	return nil, fmt.Errorf("无法分配未使用的 CloudMail 地址")
}

func (p *cloudmailProvider) publicToken(ctx context.Context, force bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && !force {
		return p.token, nil
	}
	payload := map[string]string{"email": p.config.CloudmailAdminEmail, "password": p.config.CloudmailPassword}
	var response struct {
		Code int `json:"code"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := postJSON(ctx, p.client, p.config.CloudmailURL+"/api/public/genToken", payload, nil, &response); err != nil {
		return "", err
	}
	if response.Code != http.StatusOK || response.Data.Token == "" {
		return "", fmt.Errorf("CloudMail 获取公开 token 失败: %s", response.Message)
	}
	p.token = response.Data.Token
	return p.token, nil
}

type cloudmailMailbox struct {
	provider *cloudmailProvider
	address  string
}

func (m *cloudmailMailbox) Address() string { return m.address }

func (m *cloudmailMailbox) WaitCode(ctx context.Context, timeout time.Duration, log func(string)) (string, error) {
	deadline := time.Now().Add(timeout)
	token, err := m.provider.publicToken(ctx, false)
	if err != nil {
		return "", err
	}
	for time.Now().Before(deadline) {
		var response struct {
			Code    int              `json:"code"`
			Data    []map[string]any `json:"data"`
			Message string           `json:"message"`
		}
		headers := map[string]string{"Authorization": token}
		err := postJSON(ctx, m.provider.client, m.provider.config.CloudmailURL+"/api/public/emailList", map[string]any{
			"size": 20, "toEmail": m.address,
		}, headers, &response)
		if err == nil && response.Code == http.StatusOK {
			for _, message := range response.Data {
				var chunks []string
				for _, key := range []string{"subject", "content", "text", "textContent", "body", "snippet", "html", "intro"} {
					if value, ok := message[key].(string); ok {
						chunks = append(chunks, value)
					}
				}
				if code := extractVerificationCode(chunks...); code != "" {
					return code, nil
				}
			}
		} else if response.Code == http.StatusUnauthorized || strings.Contains(strings.ToLower(response.Message), "token") {
			token, _ = m.provider.publicToken(ctx, true)
		}
		if log != nil {
			log("等待 CloudMail 验证码")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", fmt.Errorf("CloudMail 在 %s 内未收到验证码", timeout)
}

func postJSON(ctx context.Context, client *http.Client, url string, payload any, headers map[string]string, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

func decodeMIMEHeader(value string) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}
