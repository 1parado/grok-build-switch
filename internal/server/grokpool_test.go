package server

import (
	"encoding/json"
	"fmt"
	"io"
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

func TestGrokPoolImportCreatesPoolBackedProfile(t *testing.T) {
	dir := t.TempDir()
	profileStore := profiles.NewStore(filepath.Join(dir, "profiles.json"))
	pool, err := grokpool.NewManager(filepath.Join(dir, "pool"))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		ActualPort: 19191,
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
	credential := fmt.Sprintf(`{"type":"xai","access_token":"pool-access","expired":%q,"email":"pool@example.com"}`, expiry)
	body, _ := json.Marshal(map[string]any{
		"files": []map[string]string{{"name": "pool.json", "content": credential}},
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/grok-pool", strings.NewReader(string(body)))
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /api/grok-pool status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	status := pool.Status()
	if !status.Configured || len(status.Accounts) != 1 || status.LocalAPIKey == "" {
		t.Fatalf("pool status = %#v", status)
	}
	list, err := profileStore.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("profiles = %#v, err = %v", list, err)
	}
	if list[0].Name != grokAuthProfileName || list[0].EffectiveAPIKey() != status.LocalAPIKey {
		t.Fatalf("pool profile = %#v", list[0])
	}
	if list[0].BaseURL != "http://127.0.0.1:19191/grok/v1" {
		t.Fatalf("profile base URL = %q", list[0].BaseURL)
	}
}

func TestGrokProxyFallsBackToSingleAuthWhenPoolHasNoAvailableAccount(t *testing.T) {
	dir := t.TempDir()
	pool, err := grokpool.NewManager(filepath.Join(dir, "pool"))
	if err != nil {
		t.Fatal(err)
	}
	// Import 会立刻触发后台巡检：先把上游指向本地 httptest，巡检结束后
	// 才允许下面对 http.DefaultTransport 全局变量做替换，避免数据竞争。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer upstream.Close()
	pool.SetUpstreamURL(upstream.URL)

	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	_, err = pool.Import([]grokpool.ImportFile{{
		Name:    "disabled.json",
		Content: fmt.Sprintf(`{"type":"xai","access_token":"pool-access","expired":%q,"email":"disabled@example.com"}`, expiry),
	}})
	if err != nil {
		t.Fatal(err)
	}
	accountID := pool.Status().Accounts[0].ID
	if _, err := pool.SetDisabled(accountID, true); err != nil {
		t.Fatal(err)
	}
	single := grokauth.NewStore(filepath.Join(dir, "single.json"))
	if _, err := single.Import([]byte(fmt.Sprintf(`{"type":"xai","access_token":"single-access","expired":%q}`, expiry))); err != nil {
		t.Fatal(err)
	}
	waitForInspection(t, pool)

	previousTransport := http.DefaultTransport
	var upstreamAuthorization string
	http.DefaultTransport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		upstreamAuthorization = request.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    request,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	server := &Server{GrokAuth: single, GrokPool: pool}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(`{"model":"grok-4.5"}`))
	request.Header.Set("Authorization", "Bearer "+pool.Status().LocalAPIKey)
	server.handleGrokProxy(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamAuthorization != "Bearer single-access" {
		t.Fatalf("upstream Authorization = %q", upstreamAuthorization)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

// waitForInspection 等待 Import 触发的后台巡检完成（分类离开 uninspected）。
func waitForInspection(t *testing.T, pool *grokpool.Manager) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status := pool.Status()
		if len(status.Accounts) > 0 && status.Accounts[0].Classification != "uninspected" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background inspection did not finish in time")
}
