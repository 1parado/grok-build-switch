package updatecheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsNewer(t *testing.T) {
	for _, test := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.8.0", "v0.7.0", true},
		{"0.7.0", "v0.7.0", false},
		{"v0.7.1", "v0.7", true},
		{"v0.7.0", "v0.8.0", false},
		{"v0.7.0", "dev", false},
		{"dev", "v0.7.0", false},
		{"v0.7.0-beta", "v0.7.0", false},
	} {
		if got := IsNewer(test.latest, test.current); got != test.want {
			t.Fatalf("IsNewer(%q, %q) = %v, want %v", test.latest, test.current, got, test.want)
		}
	}
}

func TestCheckSelectsMatchingAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v0.8.0",
			"name":     "v0.8.0",
			"html_url": "https://github.com/1parado/grok-build-switch/releases/tag/v0.8.0",
			"assets":   []map[string]string{{"name": "grok_switch_gui.exe", "browser_download_url": "https://example.test/gui.exe"}},
		})
	}))
	defer server.Close()
	checker := New("v0.7.0", "grok_switch_gui.exe")
	checker.Endpoint = server.URL
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.UpdateAvailable || info.DownloadURL != "https://example.test/gui.exe" {
		t.Fatalf("unexpected update info: %#v", info)
	}
}
