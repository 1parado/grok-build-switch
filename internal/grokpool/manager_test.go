package grokpool

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImportPersistsMetadataWithoutTokensAndKeepsStableKey(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	result, err := manager.Import([]ImportFile{
		{Name: "a.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"secret-access-a","refresh_token":"secret-refresh-a","expired":%q,"email":"a@example.com"}`, expiry)},
		{Name: "b.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"secret-access-b","refresh_token":"secret-refresh-b","expired":%q,"email":"b@example.com"}`, expiry)},
	})
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if result.Imported != 2 || len(manager.Status().Accounts) != 2 {
		t.Fatalf("Import() result = %#v, status = %#v", result, manager.Status())
	}
	key := manager.Status().LocalAPIKey
	if key == "" {
		t.Fatal("pool local key is empty")
	}

	index, err := os.ReadFile(filepath.Join(manager.dir, "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(index), "secret-access") || strings.Contains(string(index), "secret-refresh") {
		t.Fatalf("pool index leaked token: %s", string(index))
	}

	result, err = manager.Import([]ImportFile{{
		Name:    "a-new.json",
		Content: fmt.Sprintf(`{"type":"xai","access_token":"secret-access-a2","refresh_token":"secret-refresh-a2","expired":%q,"email":"a@example.com"}`, expiry),
	}})
	if err != nil || result.Updated != 1 {
		t.Fatalf("re-import result = %#v, err = %v", result, err)
	}
	if manager.Status().LocalAPIKey != key {
		t.Fatal("pool local key changed after re-import")
	}
}

func TestAutomaticInspectionClassifiesAndRoutesOnlyAvailableAccount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"grok-4.5"}]}`)
		case "/responses":
			if token == "quota-token" {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"response-ok"}`)
		case "/chat/completions":
			// Conflicting fallback success must not override explicit quota exhaustion.
			_, _ = io.WriteString(w, `{"id":"fallback-ok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.client = upstream.Client()
	manager.upstreamURL = upstream.URL
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	_, err = manager.Import([]ImportFile{
		{Name: "healthy.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"healthy-token","expired":%q,"email":"healthy@example.com"}`, expiry)},
		{Name: "quota.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"quota-token","expired":%q,"email":"quota@example.com"}`, expiry)},
	})
	if err != nil {
		t.Fatal(err)
	}

	manager.Start()
	t.Cleanup(manager.Close)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := manager.Status()
		if !status.Running && !status.LastRun.IsZero() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	status := manager.Status()
	if status.LastRun.IsZero() || status.Running {
		t.Fatalf("automatic inspection did not finish: %#v", status)
	}
	classes := map[string]string{}
	for _, account := range status.Accounts {
		classes[account.Email] = account.Classification
	}
	if classes["healthy@example.com"] != "healthy" || classes["quota@example.com"] != "quota_exhausted" {
		t.Fatalf("classifications = %#v", classes)
	}
	if status.Summary.Available != 1 || status.Summary.Quota != 1 {
		t.Fatalf("summary = %#v", status.Summary)
	}
	token, _, err := manager.NextToken(t.Context(), "conversation-1")
	if err != nil || token != "healthy-token" {
		t.Fatalf("NextToken() = %q, %v", token, err)
	}
}

func TestGeneric429DoesNotIsolateAccount(t *testing.T) {
	got := classify(http.StatusTooManyRequests, "rate_limit", "too many requests", false)
	if got.Name != "probe_error" || got.Action != "keep" {
		t.Fatalf("generic 429 classification = %#v", got)
	}
	free := classify(http.StatusTooManyRequests, "subscription:free-usage-exhausted", "included free usage", false)
	if free.Name != "quota_exhausted" || free.Action != "disable" {
		t.Fatalf("free usage classification = %#v", free)
	}
}

func TestObserveResponseReactivelyIsolatesExhaustedAccount(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	_, err = manager.Import([]ImportFile{{
		Name:    "quota.json",
		Content: fmt.Sprintf(`{"type":"xai","access_token":"quota-token","expired":%q,"email":"quota@example.com"}`, expiry),
	}})
	if err != nil {
		t.Fatal(err)
	}
	id := manager.Status().Accounts[0].ID
	manager.ObserveResponse(id, http.StatusTooManyRequests, `{"code":"subscription:free-usage-exhausted","error":"included free usage has been exhausted"}`)
	status := manager.Status()
	if status.Accounts[0].Classification != "quota_exhausted" || status.Summary.Available != 0 {
		t.Fatalf("reactive status = %#v", status)
	}
}

func TestSettingsValidation(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UpdateSettings(Settings{Enabled: true, IntervalMinutes: 1, Workers: 4}); err == nil {
		t.Fatal("expected interval validation error")
	}
	if _, err := manager.UpdateSettings(Settings{Enabled: true, IntervalMinutes: 30, Workers: 17}); err == nil {
		t.Fatal("expected worker validation error")
	}
}

func TestBulkDisableAndDeleteOnlyInspectedAbnormalAccounts(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	files := make([]ImportFile, 0, 4)
	for _, name := range []string{"healthy", "probe", "quota", "pending"} {
		files = append(files, ImportFile{
			Name:    name + ".json",
			Content: fmt.Sprintf(`{"type":"xai","access_token":%q,"expired":%q,"email":%q}`, name+"-token", expiry, name+"@example.com"),
		})
	}
	if _, err := manager.Import(files); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	for i := range manager.state.Accounts {
		switch manager.state.Accounts[i].Email {
		case "healthy@example.com":
			manager.state.Accounts[i].Classification = "healthy"
		case "probe@example.com":
			manager.state.Accounts[i].Classification = "probe_error"
		case "quota@example.com":
			manager.state.Accounts[i].Classification = "quota_exhausted"
		}
	}
	if err := manager.saveLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()

	disabled, status, err := manager.BulkAction("disable")
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Matched != 2 || disabled.Updated != 2 {
		t.Fatalf("disable result = %#v", disabled)
	}
	for _, account := range status.Accounts {
		wantDisabled := account.Email == "probe@example.com" || account.Email == "quota@example.com"
		if account.Disabled != wantDisabled {
			t.Fatalf("account disabled state = %#v", account)
		}
	}

	deleted, status, err := manager.BulkAction("delete")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Matched != 2 || deleted.Updated != 2 || len(status.Accounts) != 2 {
		t.Fatalf("delete result = %#v, status = %#v", deleted, status)
	}
	for _, account := range status.Accounts {
		if account.Email != "healthy@example.com" && account.Email != "pending@example.com" {
			t.Fatalf("unexpected remaining account: %#v", account)
		}
	}
}

func TestProbeErrorTextIsBounded(t *testing.T) {
	message := strings.Repeat("x", 1200)
	parsed := extractProbeError(message)
	if len(parsed.Message) != 1003 || !strings.HasSuffix(parsed.Message, "…") {
		t.Fatalf("bounded message length = %d", len(parsed.Message))
	}
}

func TestInspectionUsesConfiguredHTTPProxy(t *testing.T) {
	var proxyCalls int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"grok-4.5"}]}`)
		case "/responses":
			_, _ = io.WriteString(w, `{"id":"ok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()

	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UpdateSettings(Settings{
		Enabled:         false,
		IntervalMinutes: 360,
		Workers:         1,
		ProxyURL:        proxy.URL,
	}); err != nil {
		t.Fatal(err)
	}
	manager.upstreamURL = "http://direct-host-that-must-not-resolve.invalid"
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := manager.Import([]ImportFile{{
		Name:    "proxy.json",
		Content: fmt.Sprintf(`{"type":"xai","access_token":"proxy-token","expired":%q,"email":"proxy@example.com"}`, expiry),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.StartInspection(); err != nil {
		t.Fatal(err)
	}
	manager.runWG.Wait()
	status := manager.Status()
	if proxyCalls < 2 || status.Accounts[0].Classification != "healthy" {
		t.Fatalf("proxy calls = %d, status = %#v", proxyCalls, status)
	}
	if status.Settings.ProxyURL != proxy.URL {
		t.Fatalf("stored proxy URL = %q", status.Settings.ProxyURL)
	}
}

func TestClientForTransportNilDoesNotInstallTypedNil(t *testing.T) {
	client := clientForTransport(nil)
	if client.Transport != nil {
		t.Fatalf("nil *http.Transport must leave Client.Transport unset, got %#v", client.Transport)
	}

	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if manager.client == nil || manager.client.Transport != nil {
		t.Fatalf("empty proxy should use default transport path, client=%#v", manager.client)
	}
	if manager.Transport() != nil {
		t.Fatal("Manager.Transport() should stay nil when no proxy is configured")
	}

	// A typed-nil RoundTripper panics inside net/http; a true-nil Transport does not.
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := manager.client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected connection error, not success")
	}
}

func TestEnsureMigratesMissingCredentialWithoutResettingExistingResult(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	file := ImportFile{
		Name:    "legacy.json",
		Content: fmt.Sprintf(`{"type":"xai","access_token":"legacy-token","expired":%q,"email":"legacy@example.com"}`, expiry),
	}
	if _, err := manager.Ensure([]ImportFile{file}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.state.Accounts[0].Classification = "healthy"
	if err := manager.saveLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()
	if _, err := manager.Ensure([]ImportFile{file}); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if len(status.Accounts) != 1 || status.Accounts[0].Classification != "healthy" {
		t.Fatalf("Ensure() reset existing account: %#v", status)
	}
}
