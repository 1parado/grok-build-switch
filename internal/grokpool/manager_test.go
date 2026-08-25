package grokpool

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	manager.client.Store(upstream.Client())
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

func TestNewManagerRecoversCorruptPoolIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pool.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.LocalAPIKey == "" || len(status.Accounts) != 0 {
		t.Fatalf("recovered status = %#v", status)
	}
	matches, err := filepath.Glob(path + ".corrupt-*.bak")
	if err != nil || len(matches) != 1 {
		t.Fatalf("corrupt backups = %#v, err = %v", matches, err)
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

func TestBulkDeleteRemovesAbnormalSourceAuthFiles(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	authDir := filepath.Join(t.TempDir(), "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.state.Settings.AuthDir = authDir
	manager.state.Settings.WatchRecursive = true
	if err := manager.saveLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()

	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	// Each file maps to one credential per access token. shared-mixed and
	// shared-both each carry two credentials in the Grok CLI auth.json shape.
	sources := map[string]string{
		"abnormal1.json":            fmt.Sprintf(`{"type":"xai","access_token":%q,"expired":%q,"email":%q}`, "a1", expiry, "a1@x.com"),
		"healthy.json":              fmt.Sprintf(`{"type":"xai","access_token":%q,"expired":%q,"email":%q}`, "h", expiry, "h@x.com"),
		"shared-both-abnormal.json": fmt.Sprintf(`{"a":{"access_token":%q,"expired":%q,"email":%q},"b":{"access_token":%q,"expired":%q,"email":%q}}`, "sa", expiry, "sa@x.com", "sb", expiry, "sb@x.com"),
		"shared-mixed.json":         fmt.Sprintf(`{"a":{"access_token":%q,"expired":%q,"email":%q},"b":{"access_token":%q,"expired":%q,"email":%q}}`, "ma", expiry, "ma@x.com", "mh", expiry, "mh@x.com"),
	}
	for name, body := range sources {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.ImportAuthDir(); err != nil {
		t.Fatal(err)
	}

	abnormal := map[string]bool{"a1@x.com": true, "sa@x.com": true, "sb@x.com": true, "ma@x.com": true}
	manager.mu.Lock()
	for i := range manager.state.Accounts {
		if abnormal[manager.state.Accounts[i].Email] {
			manager.state.Accounts[i].Classification = "quota_exhausted"
		} else {
			manager.state.Accounts[i].Classification = "healthy"
		}
	}
	if err := manager.saveLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()

	result, status, err := manager.BulkAction("delete")
	if err != nil {
		t.Fatal(err)
	}
	if result.Matched != 4 || result.Updated != 4 {
		t.Fatalf("delete result = %#v", result)
	}
	if result.DeletedFiles != 2 {
		t.Fatalf("expected 2 source files deleted, got %d (result=%#v)", result.DeletedFiles, result)
	}
	if len(status.Accounts) != 2 {
		t.Fatalf("expected 2 remaining accounts, got %d", len(status.Accounts))
	}
	for _, account := range status.Accounts {
		if abnormal[account.Email] {
			t.Fatalf("abnormal account %q should have been deleted", account.Email)
		}
	}

	removed := []string{"abnormal1.json", "shared-both-abnormal.json"}
	kept := []string{"healthy.json", "shared-mixed.json"}
	for _, name := range removed {
		if _, err := os.Stat(filepath.Join(authDir, name)); !os.IsNotExist(err) {
			t.Fatalf("abnormal source file %s should be removed, stat err = %v", name, err)
		}
	}
	for _, name := range kept {
		if _, err := os.Stat(filepath.Join(authDir, name)); err != nil {
			t.Fatalf("source file %s should remain, stat err = %v", name, err)
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
	var proxyCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
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
	if proxyCalls.Load() < 2 || status.Accounts[0].Classification != "healthy" {
		t.Fatalf("proxy calls = %d, status = %#v", proxyCalls.Load(), status)
	}
	if status.Settings.ProxyURL != proxy.URL {
		t.Fatalf("stored proxy URL = %q", status.Settings.ProxyURL)
	}
}

func TestClientForTransportNilProxyUsesDefaultTransport(t *testing.T) {
	// Regression: empty proxy previously put a typed-nil *http.Transport into
	// http.Client.Transport and paniced on Do (inspect.go doProbe).
	client := clientForTransport(nil)
	if client.Transport != nil {
		t.Fatalf("expected nil Client.Transport for empty proxy, got %#v", client.Transport)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do with nil proxy transport: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestEmptyProxyInspectionDoesNotPanic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.5"}]}`))
		case r.URL.Path == "/responses":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"resp"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UpdateSettings(Settings{
		Enabled:         false,
		IntervalMinutes: 360,
		Workers:         1,
		ProxyURL:        "",
	}); err != nil {
		t.Fatal(err)
	}
	manager.upstreamURL = upstream.URL
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := manager.Import([]ImportFile{{
		Name:    "direct.json",
		Content: fmt.Sprintf(`{"type":"xai","access_token":"direct-token","expired":%q,"email":"direct@example.com"}`, expiry),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.StartInspection(); err != nil {
		t.Fatal(err)
	}
	manager.runWG.Wait()
	status := manager.Status()
	if len(status.Accounts) != 1 {
		t.Fatalf("accounts = %#v", status.Accounts)
	}
	if status.Accounts[0].Classification != "healthy" {
		t.Fatalf("classification = %q reason=%q err=%q",
			status.Accounts[0].Classification, status.Accounts[0].Reason, status.Accounts[0].ErrorMessage)
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
