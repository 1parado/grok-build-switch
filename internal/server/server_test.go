package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"grok_switch/internal/paths"
	"grok_switch/internal/remoteaccess"
	"grok_switch/internal/settings"
	"grok_switch/internal/updatecheck"
)

func TestListenRejectsInvalidPreferredPort(t *testing.T) {
	server := &Server{}
	if _, _, err := server.Listen(70000); err == nil {
		t.Fatal("Listen() accepted an invalid preferred port")
	}
}

func TestUpdateEndpointCanSkipCurrentRelease(t *testing.T) {
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v0.8.0",
			"name":     "v0.8.0",
			"html_url": "https://github.com/1parado/grok-build-switch/releases/tag/v0.8.0",
		})
	}))
	defer releaseServer.Close()
	checker := updatecheck.New("v0.7.0", "grok_switch.exe")
	checker.Endpoint = releaseServer.URL
	state := updatecheck.NewPreferenceStore(filepath.Join(t.TempDir(), "update_state.json"))
	server := &Server{UpdateChecker: checker, UpdateState: state}

	get := httptest.NewRecorder()
	server.handleUpdate(get, httptest.NewRequest(http.MethodGet, "/api/update", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"update_available":true`) {
		t.Fatalf("initial update response = %d %s", get.Code, get.Body.String())
	}

	skip := httptest.NewRecorder()
	server.handleUpdate(skip, httptest.NewRequest(http.MethodPost, "/api/update", strings.NewReader(`{"action":"skip","version":"v0.8.0"}`)))
	if skip.Code != http.StatusOK || !strings.Contains(skip.Body.String(), `"skipped":true`) {
		t.Fatalf("skip response = %d %s", skip.Code, skip.Body.String())
	}

	getAgain := httptest.NewRecorder()
	server.handleUpdate(getAgain, httptest.NewRequest(http.MethodGet, "/api/update", nil))
	if !strings.Contains(getAgain.Body.String(), `"skipped":true`) {
		t.Fatalf("skipped state was not persisted: %s", getAgain.Body.String())
	}
}

func TestImageGenerationConnectionProbeUsesIndependentEndpointAndKey(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Fatalf("image probe path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-image" {
			t.Fatalf("image probe authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://cdn.example/test.png"}]}`))
	}))
	defer upstream.Close()

	server := &Server{}
	body := fmt.Sprintf(`{"base_url":%q,"api_key":"sk-image","model":"image-model","purpose":"image_generation"}`, upstream.URL+"/v1")
	response := httptest.NewRecorder()
	server.handleConnectionTest(response, httptest.NewRequest(http.MethodPost, "/api/connection/test", strings.NewReader(body)))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok":true`) {
		t.Fatalf("image probe response = %d %s", response.Code, response.Body.String())
	}
	if received["model"] != "image-model" || received["prompt"] == "" {
		t.Fatalf("image probe body = %#v", received)
	}
}

func TestAgentMediaServesFilesFromSessionOnly(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "sessions", "encoded-cwd", "session-1", "generated")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mediaPath := filepath.Join(sessionDir, "2.jpg")
	if err := os.WriteFile(mediaPath, []byte("generated-image"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := &Server{Paths: paths.Paths{GrokHome: root}}

	request := httptest.NewRequest(http.MethodGet, "/api/agent/media?session_id=session-1&path=http%3A%2F%2F127.0.0.1%3A17878%2F2.jpg", nil)
	response := httptest.NewRecorder()
	server.handleAgentMedia(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/jpeg" || response.Body.String() != "generated-image" {
		t.Fatalf("media response = %d %q headers=%v", response.Code, response.Body.String(), response.Header())
	}
	fileURI := "file:///" + strings.TrimLeft(filepath.ToSlash(mediaPath), "/")
	resolved, err := server.resolveAgentMediaPath("session-1", fileURI)
	resolvedInfo, resolvedStatErr := os.Stat(resolved)
	mediaInfo, mediaStatErr := os.Stat(mediaPath)
	if err != nil || resolvedStatErr != nil || mediaStatErr != nil || !os.SameFile(resolvedInfo, mediaInfo) {
		t.Fatalf("file URI resolved to %q, err=%v; want %q", resolved, err, mediaPath)
	}

	traversal := httptest.NewRecorder()
	server.handleAgentMedia(traversal, httptest.NewRequest(http.MethodGet, "/api/agent/media?session_id=session-1&path=../2.jpg", nil))
	if traversal.Code != http.StatusNotFound {
		t.Fatalf("traversal response = %d, want %d", traversal.Code, http.StatusNotFound)
	}

	if err := os.WriteFile(filepath.Join(sessionDir, "notes.txt"), []byte("private text"), 0o644); err != nil {
		t.Fatal(err)
	}
	nonMedia := httptest.NewRecorder()
	server.handleAgentMedia(nonMedia, httptest.NewRequest(http.MethodGet, "/api/agent/media?session_id=session-1&path=notes.txt", nil))
	if nonMedia.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-media response = %d, want %d", nonMedia.Code, http.StatusUnsupportedMediaType)
	}
}

func TestRemoteRequestsAreRejectedByDefault(t *testing.T) {
	store := settings.NewStore(filepath.Join(t.TempDir(), "settings.json"))
	remote := remoteaccess.NewStore(filepath.Join(t.TempDir(), "remote_access.json"))
	s := &Server{Settings: store, RemoteAccess: remote}
	next := s.withAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "http://192.168.1.10:17878/api/profiles", nil)
	req.RemoteAddr = "192.168.1.10:40000"
	response := httptest.NewRecorder()
	next.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestRemoteRequestWithoutSessionPromptsPairing(t *testing.T) {
	settingsStore := settings.NewStore(filepath.Join(t.TempDir(), "settings.json"))
	current := settings.Default()
	current.LANAccessEnabled = true
	if _, err := settingsStore.Update(current); err != nil {
		t.Fatal(err)
	}
	remote := remoteaccess.NewStore(filepath.Join(t.TempDir(), "remote_access.json"))
	s := &Server{Settings: settingsStore, RemoteAccess: remote}
	next := s.withAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Run("browser page redirects to pairing", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://192.168.1.10:17878/", nil)
		req.RemoteAddr = "192.168.1.20:40000"
		response := httptest.NewRecorder()
		next.ServeHTTP(response, req)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusSeeOther, response.Body.String())
		}
		if location := response.Header().Get("Location"); location != "/pair" {
			t.Fatalf("Location = %q, want /pair", location)
		}
	})

	t.Run("API returns friendly unauthorized response", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://192.168.1.10:17878/api/status", nil)
		req.RemoteAddr = "192.168.1.20:40001"
		response := httptest.NewRecorder()
		next.ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnauthorized, response.Body.String())
		}
		body := response.Body.String()
		if !strings.Contains(body, "请先使用电脑端生成的二维码完成配对") {
			t.Fatalf("unexpected body: %s", body)
		}
		if strings.Contains(body, "named cookie not present") {
			t.Fatalf("raw missing-cookie error leaked: %s", body)
		}
	})
}

func TestRemoteSessionAndOriginProtection(t *testing.T) {
	settingsStore := settings.NewStore(filepath.Join(t.TempDir(), "settings.json"))
	current := settings.Default()
	current.LANAccessEnabled = true
	if _, err := settingsStore.Update(current); err != nil {
		t.Fatal(err)
	}
	remote := remoteaccess.NewStore(filepath.Join(t.TempDir(), "remote_access.json"))
	snapshot, err := remote.Get()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Settings: settingsStore, RemoteAccess: remote}
	next := s.withAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	valid := httptest.NewRequest(http.MethodPost, "http://192.168.1.10:17878/api/profiles", strings.NewReader("{}"))
	valid.RemoteAddr = "192.168.1.10:40000"
	valid.Host = "192.168.1.10:17878"
	valid.Header.Set("Origin", "http://192.168.1.10:17878")
	valid.AddCookie(&http.Cookie{Name: lanSessionCookie, Value: snapshot.SessionToken})
	response := httptest.NewRecorder()
	next.ServeHTTP(response, valid)
	if response.Code != http.StatusNoContent {
		t.Fatalf("valid session status = %d, want %d", response.Code, http.StatusNoContent)
	}

	forged := valid.Clone(valid.Context())
	forged.Header.Set("Origin", "http://attacker.example")
	response = httptest.NewRecorder()
	next.ServeHTTP(response, forged)
	if response.Code != http.StatusForbidden {
		t.Fatalf("forged origin status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestPairingSetsHTTPOnlySessionCookie(t *testing.T) {
	settingsStore := settings.NewStore(filepath.Join(t.TempDir(), "settings.json"))
	current := settings.Default()
	current.LANAccessEnabled = true
	if _, err := settingsStore.Update(current); err != nil {
		t.Fatal(err)
	}
	remote := remoteaccess.NewStore(filepath.Join(t.TempDir(), "remote_access.json"))
	pairing, err := remote.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Settings: settingsStore, RemoteAccess: remote}
	req := httptest.NewRequest(http.MethodGet, "/pair?code="+pairing.PairingCode, nil)
	req.RemoteAddr = "192.168.1.10:40000"
	response := httptest.NewRecorder()
	s.handlePair(response, req)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusSeeOther)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != lanSessionCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected session cookies: %#v", cookies)
	}
}

func TestReconfigureLANListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	s := &Server{
		ActualPort: port,
		listener:   listener,
		bindHost:   "127.0.0.1",
		httpServer: &http.Server{Handler: http.NotFoundHandler()},
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(ctx)
	}()
	if err := s.reconfigureLANAccess(true); err != nil {
		t.Fatal(err)
	}
	if s.bindHost != "0.0.0.0" {
		t.Fatalf("bind host = %q, want 0.0.0.0", s.bindHost)
	}
	if err := s.reconfigureLANAccess(false); err != nil {
		t.Fatal(err)
	}
	if s.bindHost != "127.0.0.1" {
		t.Fatalf("bind host = %q, want 127.0.0.1", s.bindHost)
	}
}
