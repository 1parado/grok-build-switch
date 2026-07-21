package registrar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-sasl"
)

type hotmailAccount struct {
	mu           sync.Mutex
	Email        string
	Password     string
	ClientID     string
	RefreshToken string
}

type hotmailProvider struct {
	mu         sync.Mutex
	accounts   []*hotmailAccount
	maxAliases int
	used       map[string]bool
	allocated  map[string]bool
	counts     map[string]int
	client     *http.Client
}

func newHotmailProvider(raw string, maxAliases int, used map[string]bool, httpClient *http.Client) (*hotmailProvider, error) {
	provider := &hotmailProvider{
		maxAliases: maxAliases,
		used:       used,
		allocated:  make(map[string]bool),
		counts:     make(map[string]int),
		client:     httpClient,
	}
	for number, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "----", 4)
		if len(parts) != 4 {
			return nil, fmt.Errorf("Hotmail 凭证第 %d 行不是四段格式", number+1)
		}
		account := &hotmailAccount{
			Email: strings.TrimSpace(parts[0]), Password: strings.TrimSpace(parts[1]),
			ClientID: strings.TrimSpace(parts[2]), RefreshToken: strings.TrimSpace(parts[3]),
		}
		if _, err := mail.ParseAddress(account.Email); err != nil {
			return nil, fmt.Errorf("Hotmail 凭证第 %d 行邮箱无效", number+1)
		}
		provider.accounts = append(provider.accounts, account)
	}
	if len(provider.accounts) == 0 {
		return nil, fmt.Errorf("没有有效 Hotmail 凭证")
	}
	for _, account := range provider.accounts {
		at := strings.LastIndex(account.Email, "@")
		if at <= 0 {
			continue
		}
		main := strings.ToLower(account.Email)
		aliasPrefix := strings.ToLower(account.Email[:at] + "+")
		domainSuffix := strings.ToLower(account.Email[at:])
		for email := range used {
			if email == main || (strings.HasPrefix(email, aliasPrefix) && strings.HasSuffix(email, domainSuffix)) {
				provider.counts[main]++
			}
		}
	}
	return provider, nil
}

func (p *hotmailProvider) Allocate(ctx context.Context) (Mailbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, account := range p.accounts {
		main := strings.ToLower(account.Email)
		if p.counts[main] >= p.maxAliases {
			continue
		}
		if !p.used[main] && !p.allocated[main] {
			p.allocated[main] = true
			p.counts[main]++
			return &hotmailMailbox{provider: p, account: account, address: account.Email}, nil
		}
		at := strings.LastIndex(account.Email, "@")
		if at <= 0 {
			continue
		}
		for attempt := 1; attempt < p.maxAliases; attempt++ {
			suffix, err := randomText(8)
			if err != nil {
				return nil, err
			}
			alias := account.Email[:at] + "+" + suffix + account.Email[at:]
			key := strings.ToLower(alias)
			if p.used[key] || p.allocated[key] {
				continue
			}
			p.allocated[key] = true
			p.counts[main]++
			return &hotmailMailbox{provider: p, account: account, address: alias}, nil
		}
	}
	return nil, fmt.Errorf("Hotmail 可用邮箱或别名已耗尽")
}

func (p *hotmailProvider) CredentialsText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	lines := make([]string, 0, len(p.accounts))
	for _, account := range p.accounts {
		account.mu.Lock()
		lines = append(lines, strings.Join([]string{account.Email, account.Password, account.ClientID, account.RefreshToken}, "----"))
		account.mu.Unlock()
	}
	return strings.Join(lines, "\n")
}

type hotmailMailbox struct {
	provider *hotmailProvider
	account  *hotmailAccount
	address  string
}

func (m *hotmailMailbox) Address() string { return m.address }

func (m *hotmailMailbox) WaitCode(ctx context.Context, timeout time.Duration, log func(string)) (string, error) {
	deadline := time.Now().Add(timeout)
	accessToken := ""
	for time.Now().Before(deadline) {
		if accessToken == "" {
			m.account.mu.Lock()
			token, refresh, err := refreshHotmailToken(ctx, m.provider.client, m.account)
			if err == nil && refresh != "" {
				m.account.RefreshToken = refresh
			}
			m.account.mu.Unlock()
			if err != nil {
				return "", err
			}
			accessToken = token
		}
		var lastErr error
		for _, host := range []string{"outlook.office365.com:993", "imap-mail.outlook.com:993"} {
			code, err := fetchHotmailCode(host, m.account.Email, m.address, accessToken)
			if err == nil {
				if code != "" {
					return code, nil
				}
				lastErr = nil
				break
			}
			lastErr = err
		}
		if lastErr != nil {
			accessToken = ""
		}
		if log != nil {
			log("等待 Hotmail 验证码")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return "", fmt.Errorf("Hotmail 在 %s 内未收到验证码", timeout)
}

func refreshHotmailToken(ctx context.Context, client *http.Client, account *hotmailAccount) (string, string, error) {
	endpoints := []struct {
		URL   string
		Scope string
	}{
		{"https://login.microsoftonline.com/consumers/oauth2/v2.0/token", "https://outlook.office.com/IMAP.AccessAsUser.All offline_access"},
		{"https://login.live.com/oauth20_token.srf", ""},
	}
	var lastErr error
	for _, endpoint := range endpoints {
		form := url.Values{
			"client_id":     {account.ClientID},
			"refresh_token": {account.RefreshToken},
			"grant_type":    {"refresh_token"},
		}
		if endpoint.Scope != "" {
			form.Set("scope", endpoint.Scope)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, strings.NewReader(form.Encode()))
		if err != nil {
			return "", "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		var payload struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Error        string `json:"error"`
			Description  string `json:"error_description"`
		}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err == nil && payload.AccessToken != "" {
			return payload.AccessToken, payload.RefreshToken, nil
		}
		lastErr = fmt.Errorf("%s: %s", payload.Error, payload.Description)
	}
	return "", "", fmt.Errorf("Hotmail OAuth2 refresh 失败: %w", lastErr)
}

type xoauth2Client struct {
	username string
	token    string
}

var _ sasl.Client = (*xoauth2Client)(nil)

func (c *xoauth2Client) Start() (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + c.username + "\x01auth=Bearer " + c.token + "\x01\x01"), nil
}

func (c *xoauth2Client) Next([]byte) ([]byte, error) { return nil, nil }

func fetchHotmailCode(host, mailboxEmail, targetEmail, accessToken string) (string, error) {
	connection, err := client.DialTLS(host, nil)
	if err != nil {
		return "", err
	}
	defer connection.Logout()
	if err := connection.Authenticate(&xoauth2Client{username: mailboxEmail, token: accessToken}); err != nil {
		return "", err
	}
	status, err := connection.Select("INBOX", true)
	if err != nil || status.Messages == 0 {
		return "", err
	}
	start := uint32(1)
	if status.Messages > 30 {
		start = status.Messages - 29
	}
	set := new(imap.SeqSet)
	set.AddRange(start, status.Messages)
	section := &imap.BodySectionName{}
	messages := make(chan *imap.Message, 30)
	done := make(chan error, 1)
	go func() {
		done <- connection.Fetch(set, []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}, messages)
	}()
	var collected []*imap.Message
	for message := range messages {
		collected = append(collected, message)
	}
	if err := <-done; err != nil {
		return "", err
	}
	target := strings.ToLower(targetEmail)
	for index := len(collected) - 1; index >= 0; index-- {
		message := collected[index]
		body := message.GetBody(section)
		if body == nil {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(body, 4<<20))
		if err != nil {
			continue
		}
		parsed, _ := mail.ReadMessage(strings.NewReader(string(raw)))
		headerBlob := ""
		if parsed != nil {
			for _, key := range []string{"To", "Cc", "Delivered-To", "X-Original-To", "Envelope-To"} {
				headerBlob += " " + decodeMIMEHeader(parsed.Header.Get(key))
			}
		}
		if target != "" && !strings.Contains(strings.ToLower(headerBlob+string(raw)), target) {
			continue
		}
		if code := extractVerificationCode(string(raw)); code != "" {
			return code, nil
		}
	}
	return "", nil
}
