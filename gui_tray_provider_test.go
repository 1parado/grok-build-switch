//go:build wailsgui && windows

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestGUITrayProviderClientSnapshotAndActivate(t *testing.T) {
	var mu sync.Mutex
	requests := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/status":
			_, _ = w.Write([]byte(`{"active_profile":{"id":"vendor-1","name":"供应商一"},"official_active":false,"config_matches_active":true}`))
		case "GET /api/profiles":
			_, _ = w.Write([]byte(`[{"id":"vendor-1","name":"供应商一","base_url":"https://one.example","is_active":true},{"id":"vendor-2","name":"供应商二","base_url":"https://two.example","is_active":false}]`))
		case "POST /api/profiles/vendor-2/activate", "POST /api/official/activate":
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newGUITrayProviderClient(server.URL)
	snapshot, err := client.snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.currentName() != "供应商一" || snapshot.ActiveID != "vendor-1" || snapshot.OfficialActive {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	if len(snapshot.Providers) != 2 || !snapshot.Providers[0].IsActive {
		t.Fatalf("unexpected providers: %#v", snapshot.Providers)
	}
	if snapshot.drifted() {
		t.Fatal("matching active profile was reported as drifted")
	}
	if err := client.activate(context.Background(), "vendor-2"); err != nil {
		t.Fatalf("activate provider: %v", err)
	}
	if err := client.activateOfficial(context.Background()); err != nil {
		t.Fatalf("activate official: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{
		"GET /api/status",
		"GET /api/profiles",
		"POST /api/profiles/vendor-2/activate",
		"POST /api/official/activate",
	} {
		if requests[key] != 1 {
			t.Fatalf("request %q count = %d, want 1", key, requests[key])
		}
	}
}

func TestGUITrayProviderClientReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"切换服务不可用"}`))
	}))
	defer server.Close()

	client := newGUITrayProviderClient(server.URL)
	if err := client.activate(context.Background(), "vendor-1"); err == nil || err.Error() != "切换服务不可用" {
		t.Fatalf("activate error = %v, want API message", err)
	}
}

func TestGUITrayProviderSnapshotDisplayState(t *testing.T) {
	official := guiTrayProviderSnapshot{OfficialActive: true, ConfigMatchesActive: true}
	if official.currentName() != "官方账号" || official.drifted() {
		t.Fatalf("unexpected official state: %#v", official)
	}
	drifted := guiTrayProviderSnapshot{ActiveID: "vendor-1", ActiveName: "供应商一"}
	if drifted.currentName() != "供应商一" || !drifted.drifted() {
		t.Fatalf("unexpected drifted state: %#v", drifted)
	}
	if drifted.fingerprint() == (guiTrayProviderSnapshot{ActiveID: "vendor-2", ActiveName: "供应商二"}).fingerprint() {
		t.Fatal("different snapshots produced the same fingerprint")
	}
}
