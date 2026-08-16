package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestImagineHTTPSmoke(t *testing.T) {
	dir, err := os.MkdirTemp("", "imagine-smoke")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := &Server{
		Assets:  embed.FS{},
		Imagine: NewImagineEngine(dir),
	}

	// 1) empty prompt -> 400
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/imagine/generate", bytes.NewBufferString(`{}`))
	s.handleImagineGenerate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt: want 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	// 2) valid prompt, no accounts -> 502 with {ok:false, err_code}
	body, _ := json.Marshal(map[string]string{
		"prompt":       "a red cat",
		"model":        "grok-imagine-image",
		"aspect_ratio": "1:1",
	})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/imagine/generate", bytes.NewReader(body))
	s.handleImagineGenerate(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("no accounts: want 502, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out["ok"] != false {
		t.Fatalf("expected ok=false, got %v", out["ok"])
	}
	if out["err_code"] == "" {
		t.Fatalf("expected non-empty err_code, got %v", out)
	}
	t.Logf("smoke OK: code=%d err_code=%v msg=%v", rec.Code, out["err_code"], out["err_msg"])

	// 3) account count helper
	if s.imagineAccountCount() != 0 {
		t.Fatalf("expected 0 accounts, got %d", s.imagineAccountCount())
	}

	// 4) output file serving route uses /imagine-output/ (regression guard for UI image URLs)
	dummy := []byte("fake-image-data")
	dummyPath := filepath.Join(s.Imagine.outputsDir, "test.jpg")
	if err := os.WriteFile(dummyPath, dummy, 0o644); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/imagine-output/test.jpg", nil)
	s.handleImagineOutput(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("output route: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %q", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), dummy) {
		t.Fatalf("output body mismatch")
	}
}
