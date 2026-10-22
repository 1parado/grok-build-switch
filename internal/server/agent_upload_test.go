package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"grok_switch/internal/paths"
)

func TestSameMillisecondUploadKeepsBothFiles(t *testing.T) {
	fixed := time.Date(2026, 10, 22, 3, 4, 5, 6_000_000, time.UTC)
	prev := uploadNow
	uploadNow = func() time.Time { return fixed }
	t.Cleanup(func() { uploadNow = prev })

	srv := &Server{Paths: paths.Paths{DataDir: t.TempDir()}}
	first := postUpload(t, srv, "image.png", "one")
	second := postUpload(t, srv, "image.png", "two")
	if first.Path == "" || second.Path == "" {
		t.Fatalf("paths: %q %q", first.Path, second.Path)
	}
	if first.Path == second.Path {
		t.Fatalf("同一毫秒的同名上传写成了同一个文件: %s", first.Path)
	}
	gotFirst, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	gotSecond, err := os.ReadFile(second.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotFirst) != "one" || string(gotSecond) != "two" {
		t.Fatalf("内容被覆盖: %q %q", gotFirst, gotSecond)
	}
}

type uploadResult struct {
	Path string `json:"path"`
}

func postUpload(t *testing.T, srv *Server, name, content string) uploadResult {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("session_id", "sess"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/agent/upload", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.handleAgentUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload %s: %d %s", name, rec.Code, rec.Body.String())
	}
	var out uploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
