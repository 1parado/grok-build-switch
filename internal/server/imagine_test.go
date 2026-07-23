package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"grok_switch/internal/profiles"
	"grok_switch/internal/switcher"
)

func TestImagineProxyInjectsIndependentAPIKeyAndModel(t *testing.T) {
	var gotAuth, gotPath, gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if m, ok := body["model"].(string); ok {
			gotModel = m
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://example.com/a.png"}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	store := profiles.NewStore(filepath.Join(dir, "profiles.json"))
	profile, err := store.Create(profiles.Profile{
		Name:     "dual",
		BaseURL:  "https://chat.example.com/v1",
		APIKey:   "sk-chat",
		IsActive: true,
		ImageGeneration: &profiles.ImageGenerationConfig{
			Enabled:    true,
			BaseURL:    upstream.URL + "/v1",
			APIKey:     "sk-image",
			APIBackend: "chat_completions",
			Model:      "doubao-seedream-test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(profile.ID); err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		Profiles: store,
		Switcher: &switcher.Switcher{Profiles: store, ConfigPath: filepath.Join(dir, "config.toml"), BackupsDir: filepath.Join(dir, "backups")},
	}
	mux := http.NewServeMux()
	srv.routes(mux)

	body := `{"model":"grok-imagine-image","prompt":"rainy city","n":1}`
	req := httptest.NewRequest(http.MethodPost, "/imagine/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer g2a_chat_session_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer sk-image" {
		t.Fatalf("expected image key rewrite, got %q", gotAuth)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotModel != "doubao-seedream-test" {
		t.Fatalf("model rewrite = %q", gotModel)
	}
}

func TestImagineProxyRequiresActiveImageConfig(t *testing.T) {
	dir := t.TempDir()
	store := profiles.NewStore(filepath.Join(dir, "profiles.json"))
	srv := &Server{Profiles: store}
	mux := http.NewServeMux()
	srv.routes(mux)

	req := httptest.NewRequest(http.MethodPost, "/imagine/v1/images/generations", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLocalImagineURLUsesActualPort(t *testing.T) {
	srv := &Server{ActualPort: 17878}
	if got := srv.localImagineURL(); got != "http://127.0.0.1:17878/imagine/v1" {
		t.Fatalf("url = %q", got)
	}
	// Ensure constant stays stable for config writers.
	_ = time.Second
}
