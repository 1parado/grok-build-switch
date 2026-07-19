package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"grok_switch/internal/remoteaccess"
	"grok_switch/internal/settings"
)

func TestListenRejectsInvalidPreferredPort(t *testing.T) {
	server := &Server{}
	if _, _, err := server.Listen(70000); err == nil {
		t.Fatal("Listen() accepted an invalid preferred port")
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
