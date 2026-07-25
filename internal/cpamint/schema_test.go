package cpamint

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOAuthEndpointsSplitAPIAndUIHosts(t *testing.T) {
	// API endpoints live on auth.x.ai (OIDC discovery).
	if DeviceCodeURL != "https://auth.x.ai/oauth2/device/code" {
		t.Fatalf("DeviceCodeURL = %q", DeviceCodeURL)
	}
	if TokenURL != "https://auth.x.ai/oauth2/token" {
		t.Fatalf("TokenURL = %q", TokenURL)
	}
	// Human verification UI lives on accounts.x.ai (returned as verification_uri).
	if !strings.HasPrefix(VerificationURIDefault, "https://accounts.x.ai/") {
		t.Fatalf("VerificationURIDefault = %q", VerificationURIDefault)
	}
	if DeviceConsentURL != "https://accounts.x.ai/oauth2/device/consent" {
		t.Fatalf("DeviceConsentURL = %q", DeviceConsentURL)
	}
	if Issuer != "https://auth.x.ai" {
		t.Fatalf("Issuer = %q", Issuer)
	}
	if ClientID == "" || Scope == "" {
		t.Fatal("client_id/scope must not be empty")
	}
}

func TestBuildAndWriteAuthFile(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"sub":"user-1","email":"a@example.com","exp":9999999999,"iat":9999990000}`,
	))
	access := "hdr." + payload + ".sig"
	auth, err := BuildAuthFile("", access, "refresh-token", "", 0, DefaultBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Type != "xai" || auth.Email != "a@example.com" || auth.Sub != "user-1" {
		t.Fatalf("auth = %#v", auth)
	}
	if !strings.HasSuffix(auth.BaseURL, "/v1") {
		t.Fatalf("base_url = %q", auth.BaseURL)
	}

	dir := t.TempDir()
	path, raw, err := WriteAuthFile(dir, auth)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "xai-a@example.com.json" {
		t.Fatalf("path = %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	var decoded AuthFile
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RefreshToken != "refresh-token" {
		t.Fatalf("decoded = %#v", decoded)
	}
	if decoded.Expired != "" {
		if _, err := time.Parse("2006-01-02T15:04:05Z", decoded.Expired); err != nil {
			t.Fatalf("expired = %q: %v", decoded.Expired, err)
		}
	}
}
