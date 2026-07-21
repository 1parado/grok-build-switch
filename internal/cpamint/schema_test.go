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
