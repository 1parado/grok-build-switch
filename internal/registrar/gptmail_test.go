package registrar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGptmailAllocateAndWaitCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Fatalf("missing or wrong X-API-Key: %q", r.Header.Get("X-API-Key"))
		}
		switch r.URL.Path {
		case "/api/generate-email":
			writeTestJSON(w, map[string]any{
				"success": true,
				"data":    map[string]any{"email": "demo@example.org"},
			})
		case "/api/emails":
			writeTestJSON(w, map[string]any{
				"success": true,
				"data": map[string]any{
					"emails": []map[string]any{
						{
							"id":      "m79_abc123",
							"subject": "SpaceXAI confirmation code: SVN-BSH",
							"content": "below to validate your email address. SVN-BSH",
						},
					},
				},
			})
		case "/api/email/m79_abc123":
			writeTestJSON(w, map[string]any{
				"success": true,
				"data": map[string]any{
					"id":           "m79_abc123",
					"html_content": "<p>SVN-BSH</p>",
					"content":      "",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	config := Config{
		GptmailURL:    server.URL,
		GptmailAPIKey: "test-key",
	}
	provider, err := newGptmailProvider(config, map[string]bool{}, client)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := provider.Allocate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := mailbox.Address(); got != "demo@example.org" {
		t.Fatalf("address = %q, want demo@example.org", got)
	}
	code, err := mailbox.WaitCode(context.Background(), 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != "SVN-BSH" {
		t.Fatalf("code = %q, want SVN-BSH", code)
	}
}

func TestGptmailRequiresConfig(t *testing.T) {
	if _, err := newGptmailProvider(Config{}, map[string]bool{}, &http.Client{}); err == nil {
		t.Fatal("expected error for missing URL/key")
	}
}
