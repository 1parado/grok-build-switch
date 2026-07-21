package registrar

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type resendTestMailbox struct {
	address string
	ready   <-chan struct{}
}

func (m *resendTestMailbox) Address() string { return m.address }

func (m *resendTestMailbox) WaitCode(ctx context.Context, _ time.Duration, _ func(string)) (string, error) {
	select {
	case <-m.ready:
		return "ABC-123", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestExtractVerificationCode(t *testing.T) {
	for input, want := range map[string]string{
		"Your verification code is 123456":      "123456",
		"验证码：654321，请在十分钟内使用":                   "654321",
		"<strong>确认码 112233</strong>":           "112233",
		"Your xAI verification code is ABC-123": "ABC-123",
	} {
		if code := extractVerificationCode(input); code != want {
			t.Fatalf("extractVerificationCode(%q) = %q, want %q", input, code, want)
		}
	}
}

func TestWaitMailboxCodeTriggersResend(t *testing.T) {
	ready := make(chan struct{})
	mailbox := &resendTestMailbox{address: "issued@example.test", ready: ready}
	var once sync.Once
	var logs []string
	code, err := waitMailboxCodeWithResend(
		context.Background(),
		mailbox,
		time.Second,
		10*time.Millisecond,
		nil,
		func(context.Context) (bool, error) {
			once.Do(func() { close(ready) })
			return true, nil
		},
		func(message string) { logs = append(logs, message) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if code != "ABC-123" {
		t.Fatalf("verification code = %q", code)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "已触发重新发送验证码") {
		t.Fatalf("resend log missing: %#v", logs)
	}
}

func TestHotmailProviderEnforcesAliasLimit(t *testing.T) {
	provider, err := newHotmailProvider(
		"owner@example.com----mail-pass----client-id----refresh-token",
		2,
		map[string]bool{},
		&http.Client{},
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.Allocate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Address() != "owner@example.com" {
		t.Fatalf("first address = %q", first.Address())
	}
	second, err := provider.Allocate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(second.Address(), "owner+") {
		t.Fatalf("second address = %q, want plus alias", second.Address())
	}
	if _, err := provider.Allocate(context.Background()); err == nil {
		t.Fatal("third allocation succeeded past alias limit")
	}
}

func TestConfigPersists(t *testing.T) {
	dir := t.TempDir()
	service, err := NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.BrowserMode = "visible"
	config.ProxyURL = "http://127.0.0.1:7890"
	config.Count = 3
	if _, err := service.Update(config); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Get().Config
	if got.BrowserMode != "visible" || got.ProxyURL != config.ProxyURL || got.Count != 3 {
		t.Fatalf("reloaded config = %#v", got)
	}
}

func TestServiceRunsAccountsAndWritesLedger(t *testing.T) {
	service, browserPath, authDir := newTestService(t)
	config := testCloudmailConfig(browserPath)
	config.Count = 2
	config.Workers = 2
	if _, err := service.Update(config); err != nil {
		t.Fatal(err)
	}
	service.SetAuthDirResolver(func() string { return authDir })
	service.runAccount = func(_ context.Context, _ Config, mailbox Mailbox, _ string, _ func(string)) (registrationOutcome, error) {
		return registrationOutcome{Email: mailbox.Address(), Password: "generated", SSO: "sso-value", MintMethod: "protocol", AuthFile: "xai.json"}, nil
	}
	finished := make(chan Job, 1)
	service.SetOnFinished(func(job Job) { finished <- job })
	if _, err := service.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case job := <-finished:
		if job.Status != StatusSucceeded || job.Succeeded != 2 || job.Completed != 2 {
			t.Fatalf("finished job = %#v", job)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("registration job did not finish")
	}
	data, err := os.ReadFile(service.accountsPath)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != 2 {
		t.Fatalf("ledger lines = %d, content=%q", lines, data)
	}
}

func TestServiceStopCancelsJob(t *testing.T) {
	service, browserPath, authDir := newTestService(t)
	config := testCloudmailConfig(browserPath)
	if _, err := service.Update(config); err != nil {
		t.Fatal(err)
	}
	service.SetAuthDirResolver(func() string { return authDir })
	service.runAccount = func(ctx context.Context, _ Config, _ Mailbox, _ string, _ func(string)) (registrationOutcome, error) {
		<-ctx.Done()
		return registrationOutcome{}, ctx.Err()
	}
	finished := make(chan Job, 1)
	service.SetOnFinished(func(job Job) { finished <- job })
	if _, err := service.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case job := <-finished:
		if job.Status != StatusCancelled {
			t.Fatalf("status = %q", job.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled job did not finish")
	}
}

func newTestService(t *testing.T) (*Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	service, err := NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	browserPath := dir + string(os.PathSeparator) + "chrome.exe"
	if err := os.WriteFile(browserPath, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	authDir := dir + string(os.PathSeparator) + "auth"
	return service, browserPath, authDir
}

func testCloudmailConfig(browserPath string) Config {
	config := DefaultConfig()
	config.BrowserPath = browserPath
	config.EmailProvider = "cloudmail"
	config.DefaultDomains = "example.com"
	config.CloudmailURL = "https://mail.example.com"
	config.CloudmailAdminEmail = "admin@example.com"
	config.CloudmailPassword = "secret"
	return config
}
