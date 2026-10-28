package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"grok_switch/internal/grokauth"
	"grok_switch/internal/grokpool"
	"grok_switch/internal/profiles"
	"grok_switch/internal/switcher"
)

func TestGrokAuthImportCreatesLocalProfile(t *testing.T) {
	dir := t.TempDir()
	profileStore := profiles.NewStore(filepath.Join(dir, "profiles.json"))
	server := &Server{
		ActualPort: 19091,
		Profiles:   profileStore,
		GrokAuth:   grokauth.NewStore(filepath.Join(dir, "grok_auth.json")),
		Switcher: &switcher.Switcher{
			ConfigPath: filepath.Join(dir, "config.toml"),
			BackupsDir: filepath.Join(dir, "backups"),
			Profiles:   profileStore,
		},
	}
	mux := http.NewServeMux()
	server.routes(mux)

	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body := fmt.Sprintf(`{"type":"xai","access_token":"access","refresh_token":"refresh","expired":%q}`, expiry)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/grok-auth", strings.NewReader(body))
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /api/grok-auth status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response grokAuthResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Configured || response.BaseURL != "http://127.0.0.1:19091/grok/v1" || response.LocalAPIKey == "" {
		t.Fatalf("import response = %#v", response)
	}
	if response.Profile == nil || response.Profile.Name != grokAuthProfileName {
		t.Fatalf("generated profile = %#v", response.Profile)
	}
	if response.Profile.UpstreamFormat != "openai_responses" || len(response.Profile.Models) != 2 {
		t.Fatalf("generated profile config = %#v", response.Profile)
	}
	if response.Profile.EffectiveAPIKey() != response.LocalAPIKey || response.Profile.BaseURL != response.BaseURL {
		t.Fatalf("profile connection does not match local proxy: %#v", response.Profile)
	}

	profilesOnDisk, err := profileStore.List()
	if err != nil || len(profilesOnDisk) != 1 {
		t.Fatalf("profiles after import = %#v, err = %v", profilesOnDisk, err)
	}
}

func TestGrokProxyRejectsWrongLocalKeyWithoutCallingUpstream(t *testing.T) {
	store := grokauth.NewStore(filepath.Join(t.TempDir(), "grok_auth.json"))
	if _, err := store.Import([]byte(`{"type":"xai","access_token":"access"}`)); err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	server := &Server{GrokAuth: store}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(`{"model":"grok-4.5"}`))
	request.Header.Set("Authorization", "Bearer wrong")
	server.handleGrokProxy(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("proxy status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestGrokAuthImportAlsoAddsCredentialToUnifiedPool(t *testing.T) {
	dir := t.TempDir()
	profileStore := profiles.NewStore(filepath.Join(dir, "profiles.json"))
	pool, err := grokpool.NewManager(filepath.Join(dir, "pool"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	server := &Server{
		ActualPort: 19292,
		Profiles:   profileStore,
		GrokAuth:   grokauth.NewStore(filepath.Join(dir, "single.json")),
		GrokPool:   pool,
		Switcher: &switcher.Switcher{
			ConfigPath: filepath.Join(dir, "config.toml"),
			BackupsDir: filepath.Join(dir, "backups"),
			Profiles:   profileStore,
		},
	}
	mux := http.NewServeMux()
	server.routes(mux)
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body := fmt.Sprintf(`{"type":"xai","access_token":"unified-access","expired":%q,"email":"unified@example.com"}`, expiry)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/grok-auth", strings.NewReader(body))
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /api/grok-auth status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	poolStatus := pool.Status()
	if len(poolStatus.Accounts) != 1 || poolStatus.Accounts[0].Email != "unified@example.com" {
		t.Fatalf("unified pool status = %#v", poolStatus)
	}
	var response grokAuthResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.PoolAccounts != 1 || response.LocalAPIKey != poolStatus.LocalAPIKey || !response.SingleConfigured {
		t.Fatalf("Grok auth response = %#v", response)
	}
	profilesOnDisk, err := profileStore.List()
	if err != nil || len(profilesOnDisk) != 1 || profilesOnDisk[0].EffectiveAPIKey() != poolStatus.LocalAPIKey {
		t.Fatalf("unified profile = %#v, err = %v", profilesOnDisk, err)
	}
}
