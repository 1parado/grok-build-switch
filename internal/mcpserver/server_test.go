package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoopReturnsParseErrorForMalformedRequest(t *testing.T) {
	var out bytes.Buffer
	s := &server{
		in:  bufio.NewReader(bytes.NewBufferString("{\n")),
		out: bufio.NewWriter(&out),
	}
	if err := s.loop(); err != nil {
		t.Fatal(err)
	}
	var response rpcResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != -32700 {
		t.Fatalf("response error = %#v, want JSON-RPC parse error", response.Error)
	}
}

func TestGenerateImageWaitsForAsyncJob(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/imagine/generate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"job":{"id":"img-1","state":"running","count":1}}`))
	})
	mux.HandleFunc("/api/imagine/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":"img-1","state":"done","ok_count":1,"images":[{"url":"/imagine-output/a.jpg","model":"grok-imagine-image","width":512,"height":512}]}]}`))
	})
	mux.HandleFunc("/imagine-output/a.jpg", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("jpeg-bytes"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &server{baseURL: srv.URL, client: srv.Client()}
	contents, isError := s.generateImage([]byte(`{"prompt":"a cat"}`))
	if isError {
		t.Fatalf("async job was treated as failure: %#v", contents)
	}
	if len(contents) != 2 {
		t.Fatalf("contents = %#v", contents)
	}
	if contents[0]["type"] != "image" || contents[0]["mimeType"] != "image/jpeg" {
		t.Fatalf("image block = %#v", contents[0])
	}
	text, _ := contents[1]["text"].(string)
	if text == "" {
		t.Fatalf("missing text block: %#v", contents[1])
	}
}

func TestWaitImagineJobStopsWhenContextCanceled(t *testing.T) {
	s := &server{baseURL: "http://127.0.0.1:1", client: http.DefaultClient}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.waitImagineJob(ctx, "img-1"); err == nil || !strings.Contains(err.Error(), "生图已取消") {
		t.Fatalf("err = %v", err)
	}
}
