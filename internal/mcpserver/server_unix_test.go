//go:build unix

package mcpserver

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGenerateImageStopsWhenStdinCloses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/imagine/generate", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"job":{"id":"img-1","state":"running"}}`))
	})
	mux.HandleFunc("/api/imagine/jobs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[{"id":"img-1","state":"running"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	s := &server{
		baseURL: srv.URL,
		client:  srv.Client(),
		stdin:   r,
		in:      bufio.NewReader(r),
		jobWait: 2 * time.Second,
	}
	start := time.Now()
	contents, isError := s.generateImage([]byte(`{"prompt":"a cat"}`))
	if !isError {
		t.Fatalf("closed stdin still produced an image: %#v", contents)
	}
	text, _ := contents[0]["text"].(string)
	if !strings.Contains(text, "生图已取消") {
		t.Fatalf("text = %q", text)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("cancel took %s", time.Since(start))
	}
}
