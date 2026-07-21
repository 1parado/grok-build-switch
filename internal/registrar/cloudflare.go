package registrar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cloudflareProvider struct {
	mu      sync.Mutex
	config  Config
	used    map[string]bool
	client  *http.Client
	domains []string
	next    int
}

type cloudflareMailbox struct {
	provider *cloudflareProvider
	address  string
	token    string
}

func newCloudflareProvider(config Config, used map[string]bool, client *http.Client) (*cloudflareProvider, error) {
	config = normalizeConfig(config)
	if err := validateConfig(config, true); err != nil {
		return nil, err
	}
	return &cloudflareProvider{
		config:  config,
		used:    used,
		client:  client,
		domains: splitDomains(config.DefaultDomains),
	}, nil
}

func splitDomains(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '；' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	domains := make([]string, 0, len(parts))
	for _, domain := range parts {
		if domain = strings.TrimSpace(domain); domain != "" {
			domains = append(domains, domain)
		}
	}
	return domains
}

func (p *cloudflareProvider) Allocate(ctx context.Context) (Mailbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var directErr error
	for attempt := 0; attempt < 5; attempt++ {
		address, token, err := p.createTemporaryAddress(ctx)
		if err != nil {
			directErr = err
			break
		}
		address = strings.ToLower(strings.TrimSpace(address))
		if address == "" || p.used[address] {
			continue
		}
		p.used[address] = true
		return &cloudflareMailbox{provider: p, address: address, token: token}, nil
	}

	address, token, fallbackErr := p.createAccountAddress(ctx)
	if fallbackErr == nil {
		address = strings.ToLower(strings.TrimSpace(address))
		if address != "" && !p.used[address] {
			p.used[address] = true
			return &cloudflareMailbox{provider: p, address: address, token: token}, nil
		}
		fallbackErr = fmt.Errorf("兼容接口返回了空地址或重复地址")
	}
	if directErr == nil {
		directErr = fmt.Errorf("临时邮箱接口连续返回重复地址")
	}
	return nil, fmt.Errorf("Cloudflare 创建邮箱失败: %v；兼容接口失败: %w", directErr, fallbackErr)
}

func (p *cloudflareProvider) createTemporaryAddress(ctx context.Context) (string, string, error) {
	payload := map[string]any{}
	if domain := p.nextConfiguredDomain(); domain != "" {
		payload["domain"] = domain
	}
	response, err := p.requestJSON(ctx, http.MethodPost, p.config.CloudflareAccountsPath, payload, "")
	if err != nil {
		return "", "", err
	}
	address := findString(response, "address", "email")
	token := findString(response, "jwt", "token")
	if address == "" || token == "" {
		return "", "", fmt.Errorf("%s 未返回 address 和 jwt", p.config.CloudflareAccountsPath)
	}
	return address, token, nil
}

func (p *cloudflareProvider) createAccountAddress(ctx context.Context) (string, string, error) {
	domain := p.nextConfiguredDomain()
	if domain == "" {
		domains, err := p.fetchDomains(ctx)
		if err != nil {
			return "", "", err
		}
		if len(domains) == 0 {
			return "", "", fmt.Errorf("%s 未返回可用域名", p.config.CloudflareDomainsPath)
		}
		domain = domains[0]
	}
	local, err := randomText(10)
	if err != nil {
		return "", "", err
	}
	password, err := randomText(20)
	if err != nil {
		return "", "", err
	}
	address := local + "@" + domain
	created, err := p.requestJSON(ctx, http.MethodPost, p.config.CloudflareAccountsPath, map[string]any{
		"address": address, "password": password, "expiresIn": 0,
	}, "")
	if err != nil {
		return "", "", err
	}
	if returnedAddress := findString(created, "address", "email"); returnedAddress != "" {
		address = returnedAddress
	}
	if token := findString(created, "jwt", "token"); token != "" {
		return address, token, nil
	}
	tokenResponse, err := p.requestJSON(ctx, http.MethodPost, p.config.CloudflareTokenPath, map[string]string{
		"address": address, "password": password,
	}, "")
	if err != nil {
		return "", "", err
	}
	token := findString(tokenResponse, "jwt", "token")
	if token == "" {
		return "", "", fmt.Errorf("%s 未返回 token", p.config.CloudflareTokenPath)
	}
	return address, token, nil
}

func (p *cloudflareProvider) nextConfiguredDomain() string {
	if len(p.domains) == 0 {
		return ""
	}
	domain := p.domains[p.next%len(p.domains)]
	p.next++
	return domain
}

func (p *cloudflareProvider) fetchDomains(ctx context.Context) ([]string, error) {
	response, err := p.requestJSON(ctx, http.MethodGet, p.config.CloudflareDomainsPath, nil, "")
	if err != nil {
		return nil, err
	}
	items := responseItems(response)
	verified := make([]string, 0, len(items))
	other := make([]string, 0, len(items))
	for _, item := range items {
		domain := firstMapString(item, "domain", "name")
		if domain == "" {
			continue
		}
		if value, ok := item["isVerified"].(bool); ok && value {
			verified = append(verified, domain)
		} else {
			other = append(other, domain)
		}
	}
	if len(verified) > 0 {
		return append(verified, other...), nil
	}
	return other, nil
}

func (m *cloudflareMailbox) Address() string { return m.address }

func (m *cloudflareMailbox) WaitCode(ctx context.Context, timeout time.Duration, log func(string)) (string, error) {
	deadline := time.Now().Add(timeout)
	seenAttempts := map[string]int{}
	for time.Now().Before(deadline) {
		messagesPath := m.provider.config.CloudflareMessagesPath
		separator := "?"
		if strings.Contains(messagesPath, "?") {
			separator = "&"
		}
		messagesPath += separator + "limit=20&offset=0"
		response, err := m.provider.requestJSON(ctx, http.MethodGet, messagesPath, nil, m.token)
		if err == nil {
			messages := responseItems(response)
			if log != nil {
				log(fmt.Sprintf("Cloudflare 本轮邮件数量: %d", len(messages)))
			}
			for _, message := range messages {
				if recipient := messageRecipient(message); recipient != "" && !strings.Contains(strings.ToLower(recipient), strings.ToLower(m.address)) {
					continue
				}
				id := messageID(message)
				if id != "" && seenAttempts[id] >= 5 {
					continue
				}
				if id != "" {
					seenAttempts[id]++
				}
				chunks := messageChunks(message)
				if id != "" {
					if detail, detailErr := m.messageDetail(ctx, id); detailErr == nil {
						chunks = append(chunks, messageChunks(detail)...)
					} else if log != nil {
						log("Cloudflare 邮件详情读取失败，改用列表内容: " + detailErr.Error())
					}
				}
				if code := extractVerificationCode(chunks...); code != "" {
					if log != nil {
						log("已从 Cloudflare 邮件提取验证码")
					}
					return code, nil
				}
			}
		}
		if log != nil {
			if err != nil {
				log("Cloudflare 收件请求失败，继续重试: " + err.Error())
			} else {
				log("等待 Cloudflare 验证码")
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", fmt.Errorf("Cloudflare 在 %s 内未收到验证码", timeout)
}

func (m *cloudflareMailbox) messageDetail(ctx context.Context, id string) (map[string]any, error) {
	escapedID := url.PathEscape(id)
	paths := []string{"/api/mail/" + escapedID, strings.TrimRight(m.provider.config.CloudflareMessagesPath, "/") + "/" + escapedID}
	var lastErr error
	for index, path := range paths {
		if index > 0 && path == paths[0] {
			continue
		}
		response, err := m.provider.requestJSON(ctx, http.MethodGet, path, nil, m.token)
		if err != nil {
			lastErr = err
			continue
		}
		if detail := responseObject(response); detail != nil {
			return detail, nil
		}
		lastErr = fmt.Errorf("%s 未返回邮件详情", path)
	}
	return nil, lastErr
}

func (p *cloudflareProvider) requestJSON(ctx context.Context, method, path string, payload any, mailboxToken string) (any, error) {
	endpoint := p.config.CloudflareAPIBase + normalizeAPIPath(path, "/")
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if p.config.CloudflareAuthMode == "query-key" && p.config.CloudflareAPIKey != "" {
		query := parsed.Query()
		query.Set("key", p.config.CloudflareAPIKey)
		parsed.RawQuery = query.Encode()
	}
	var body io.Reader
	if payload != nil {
		raw, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return nil, marshalErr
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if mailboxToken != "" {
		request.Header.Set("Authorization", "Bearer "+mailboxToken)
	} else if p.config.CloudflareAuthMode == "bearer" && p.config.CloudflareAPIKey != "" {
		request.Header.Set("Authorization", "Bearer "+p.config.CloudflareAPIKey)
	}
	if p.config.CloudflareAuthMode == "x-api-key" && p.config.CloudflareAPIKey != "" {
		request.Header.Set("X-API-Key", p.config.CloudflareAPIKey)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("HTTP %s: %s", response.Status, strings.TrimSpace(string(raw)))
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("%s 返回无效 JSON: %w", path, err)
	}
	return decoded, nil
}

func findString(value any, keys ...string) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if found := firstMapString(object, keys...); found != "" {
		return found
	}
	for _, childKey := range []string{"data", "result"} {
		if child, ok := object[childKey].(map[string]any); ok {
			if found := firstMapString(child, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}

func firstMapString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func responseItems(value any) []map[string]any {
	switch typed := value.(type) {
	case []any:
		items := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if object, ok := item.(map[string]any); ok {
				items = append(items, object)
			}
		}
		return items
	case map[string]any:
		for _, key := range []string{"results", "hydra:member", "data", "messages", "mails"} {
			if child, exists := typed[key]; exists {
				if items := responseItems(child); len(items) > 0 {
					return items
				}
			}
		}
	}
	return nil
}

func responseObject(value any) map[string]any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"data", "result", "message", "mail"} {
		if child, ok := object[key].(map[string]any); ok {
			return child
		}
	}
	return object
}

func messageID(message map[string]any) string {
	for _, key := range []string{"id", "msgid", "messageId"} {
		switch value := message[key].(type) {
		case string:
			if value != "" {
				return value
			}
		case float64:
			return strconv.FormatInt(int64(value), 10)
		}
	}
	return ""
}

func messageRecipient(message map[string]any) string {
	for _, key := range []string{"address", "to", "toEmail", "recipient"} {
		switch value := message[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		case map[string]any:
			if address := firstMapString(value, "address", "email"); address != "" {
				return address
			}
		case []any:
			for _, item := range value {
				if object, ok := item.(map[string]any); ok {
					if address := firstMapString(object, "address", "email"); address != "" {
						return address
					}
				}
			}
		}
	}
	return ""
}

func messageChunks(message map[string]any) []string {
	chunks := make([]string, 0, 10)
	for _, key := range []string{"subject", "text", "raw", "content", "intro", "body", "snippet", "html", "textContent"} {
		switch value := message[key].(type) {
		case string:
			chunks = append(chunks, value)
		case []any:
			for _, item := range value {
				if text, ok := item.(string); ok {
					chunks = append(chunks, text)
				}
			}
		}
	}
	return chunks
}
