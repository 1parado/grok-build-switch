package registrar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestYydsAllocateAndWaitCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "AC-test" {
			t.Fatalf("missing or wrong X-API-Key: %q", r.Header.Get("X-API-Key"))
		}
		if r.URL.Path == "/v1/accounts" && r.Method != http.MethodPost {
			t.Fatalf("accounts method = %s, want POST", r.Method)
		}
		switch r.URL.Path {
		case "/v1/accounts":
			writeTestJSON(w, map[string]any{
				"success": true,
				"data":    map[string]any{"address": "my-prefix@a3f9c2.mail.example.com", "mode": "wildcard"},
			})
		case "/v1/messages/next":
			writeTestJSON(w, map[string]any{
				"success": true,
				"data": map[string]any{
					"message": map[string]any{
						"verificationCode": "SVN-BSH",
					},
					"inboxAddress": "my-prefix@a3f9c2.mail.example.com",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := Config{
		YydsURL:    server.URL,
		YydsAPIKey: "AC-test",
	}
	provider, err := newYydsProvider(config, map[string]bool{}, &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := provider.Allocate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := mailbox.Address(); got != "my-prefix@a3f9c2.mail.example.com" {
		t.Fatalf("address = %q", got)
	}
	code, err := mailbox.WaitCode(context.Background(), 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != "SVN-BSH" {
		t.Fatalf("code = %q, want SVN-BSH", code)
	}
}

func TestYydsRequiresConfig(t *testing.T) {
	if _, err := newYydsProvider(Config{}, map[string]bool{}, &http.Client{}); err == nil {
		t.Fatal("expected error for missing URL/key")
	}
}

// A base URL that already ends with /v1 must not produce /v1/v1/... paths.
func TestYydsBaseURLNormalization(t *testing.T) {
	var hitPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		writeTestJSON(w, map[string]any{
			"success": true,
			"data":    map[string]any{"address": "norm@example.test"},
		})
	}))
	defer server.Close()

	config := Config{
		YydsURL:    server.URL + "/v1", // user-facing base including /v1
		YydsAPIKey: "AC-test",
	}
	provider, err := newYydsProvider(config, map[string]bool{}, &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Allocate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hitPath != "/v1/accounts" {
		t.Fatalf("path = %q, want /v1/accounts", hitPath)
	}
}
