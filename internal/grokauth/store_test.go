package grokauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImportCPAFlatCredential(t *testing.T) {
	expiry := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	raw := fmt.Sprintf(`{
  "type": "xai",
  "access_token": %q,
  "refresh_token": "refresh-cpa",
  "expired": %q,
  "email": "cpa@example.com",
  "token_endpoint": "https://auth.x.ai/oauth/token"
}`, testJWT(map[string]any{"exp": expiry.Unix()}), expiry.Format(time.RFC3339))

	store := NewStore(filepath.Join(t.TempDir(), "grok_auth.json"))
	status, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if !status.Configured || status.Source != "cpa-xai" {
		t.Fatalf("Import() status = %#v", status)
	}
	if status.Email != "cpa@example.com" || status.LocalAPIKey == "" {
		t.Fatalf("Import() did not retain public identity/local key: %#v", status)
	}
	if !status.ExpiresAt.Equal(expiry) {
		t.Fatalf("ExpiresAt = %v, want %v", status.ExpiresAt, expiry)
	}
}

func TestImportNativeGrokAuthChoosesLatestAndPreservesLocalKey(t *testing.T) {
	earlier := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	later := earlier.Add(time.Hour)
	raw := fmt.Sprintf(`{
  "https://auth.x.ai::first": {
    "key": %q,
    "refresh_token": "refresh-first",
    "expires_at": %q,
    "oidc_issuer": "https://auth.x.ai",
    "oidc_client_id": "client-first"
  },
  "https://auth.x.ai::second": {
    "key": %q,
    "refresh_token": "refresh-second",
    "expires_at": %q,
    "email": "newest@example.com"
  }
}`, testJWT(map[string]any{"exp": earlier.Unix()}), earlier.Format(time.RFC3339Nano), testJWT(map[string]any{"exp": later.Unix()}), later.Format(time.RFC3339Nano))

	store := NewStore(filepath.Join(t.TempDir(), "grok_auth.json"))
	first, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if first.Source != "grok-auth-json" || first.Email != "newest@example.com" {
		t.Fatalf("Import() chose wrong native credential: %#v", first)
	}
	second, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("second Import() error = %v", err)
	}
	if second.LocalAPIKey != first.LocalAPIKey {
		t.Fatalf("local key changed across re-import: %q != %q", second.LocalAPIKey, first.LocalAPIKey)
	}
}

func TestImportRejectsAnotherProvider(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "grok_auth.json"))
	_, err := store.Import([]byte(`{"type":"codex","access_token":"not-xai"}`))
	if err == nil || !strings.Contains(err.Error(), "不是 xai/grok") {
		t.Fatalf("Import() error = %v, want provider validation error", err)
	}
}

func TestRefreshUpdatesCredentialAndUsesExpectedForm(t *testing.T) {
	var received url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	defer server.Close()

	store := NewStore(filepath.Join(t.TempDir(), "grok_auth.json"))
	store.client = server.Client()
	store.allowUnsafeEndpoints = true
	raw := fmt.Sprintf(`{
  "type": "xai",
  "access_token": "old-access",
  "refresh_token": "old-refresh",
  "expired": %q,
  "client_id": "test-client",
  "token_endpoint": %q
}`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), server.URL)
	if _, err := store.Import([]byte(raw)); err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	status, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if status.NeedsRefresh || status.LastRefresh.IsZero() {
		t.Fatalf("Refresh() status = %#v", status)
	}
	if received.Get("grant_type") != "refresh_token" || received.Get("client_id") != "test-client" || received.Get("refresh_token") != "old-refresh" {
		t.Fatalf("refresh form = %#v", received)
	}
	credential, err := store.readLocked()
	if err != nil {
		t.Fatalf("readLocked() error = %v", err)
	}
	if credential.AccessToken != "new-access" || credential.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed credential = %#v", credential)
	}
}

func TestAuthorizedAcceptsBearerAndXAPIKey(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "grok_auth.json"))
	status, err := store.Import([]byte(`{"type":"xai","access_token":"access"}`))
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	for _, header := range []struct {
		name  string
		value string
	}{
		{"Authorization", "Bearer " + status.LocalAPIKey},
		{"x-api-key", status.LocalAPIKey},
	} {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/grok/v1/responses", nil)
		req.Header.Set(header.name, header.value)
		if !store.Authorized(req) {
			t.Fatalf("Authorized() rejected %s", header.name)
		}
	}
	bad := httptest.NewRequest(http.MethodPost, "http://localhost/grok/v1/responses", nil)
	bad.Header.Set("Authorization", "Bearer wrong")
	if store.Authorized(bad) {
		t.Fatal("Authorized() accepted wrong key")
	}
}

func TestStatusRecoversCorruptPersistedStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grok_auth.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := NewStore(path).Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Configured {
		t.Fatalf("status = %#v, want unconfigured recovery", status)
	}
	matches, err := filepath.Glob(path + ".corrupt-*.bak")
	if err != nil || len(matches) != 1 {
		t.Fatalf("corrupt backups = %#v, err = %v", matches, err)
	}
}

func testJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]any{"alg": "none"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
