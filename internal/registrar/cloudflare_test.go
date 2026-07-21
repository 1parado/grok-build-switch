package registrar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCloudflareDirectMailboxFlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/new_address":
			if r.Method != http.MethodPost || r.Header.Get("X-API-Key") != "service-key" {
				t.Fatalf("create request method=%s api-key=%q", r.Method, r.Header.Get("X-API-Key"))
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["domain"] != "example.test" {
				t.Fatalf("create domain = %#v", payload["domain"])
			}
			writeTestJSON(w, map[string]any{"address": "issued@example.test", "jwt": "mailbox-jwt"})
		case "/api/mails":
			if r.Header.Get("Authorization") != "Bearer mailbox-jwt" || r.Header.Get("X-API-Key") != "service-key" {
				t.Fatalf("mail headers authorization=%q api-key=%q", r.Header.Get("Authorization"), r.Header.Get("X-API-Key"))
			}
			writeTestJSON(w, map[string]any{"hydra:member": []any{map[string]any{
				"id": "42", "to": []any{map[string]any{"address": "issued@example.test"}}, "subject": "Grok verification",
			}}})
		case "/api/mail/42":
			writeTestJSON(w, map[string]any{"data": map[string]any{"text": "Your verification code is ABC-123"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := DefaultConfig()
	config.EmailProvider = "cloudflare"
	config.DefaultDomains = "example.test"
	config.CloudflareAPIBase = server.URL
	config.CloudflareAPIKey = "service-key"
	config.CloudflareAuthMode = "x-api-key"
	provider, err := newMailProvider(config, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := provider.Allocate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mailbox.Address() != "issued@example.test" {
		t.Fatalf("mailbox address = %q", mailbox.Address())
	}
	code, err := mailbox.WaitCode(context.Background(), time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != "ABC-123" {
		t.Fatalf("verification code = %q", code)
	}
}

func TestCloudflareFallsBackToAccountTokenFlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/new_address":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			address, _ := payload["address"].(string)
			if address == "" {
				http.Error(w, "account payload required", http.StatusUnprocessableEntity)
				return
			}
			if !strings.HasSuffix(address, "@fallback.test") {
				t.Fatalf("fallback address = %q", address)
			}
			writeTestJSON(w, map[string]any{"address": address})
		case "/api/domains":
			writeTestJSON(w, map[string]any{"data": []any{map[string]any{"domain": "fallback.test", "isVerified": true}}})
		case "/api/token":
			writeTestJSON(w, map[string]any{"data": map[string]any{"token": "fallback-jwt"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := DefaultConfig()
	config.EmailProvider = "cloudflare"
	config.CloudflareAPIBase = server.URL
	provider, err := newMailProvider(config, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := provider.Allocate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(mailbox.Address(), "@fallback.test") {
		t.Fatalf("mailbox address = %q", mailbox.Address())
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
