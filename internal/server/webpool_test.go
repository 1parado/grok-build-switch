package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok_switch/internal/webpool"
)

// newWebPoolTestServer 构造带假上游的最小 Server。
func newWebPoolTestServer(t *testing.T, upstream *httptest.Server) (*Server, *webpool.Manager) {
	t.Helper()
	dir := t.TempDir()
	pool, err := webpool.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	pool.SetUpstreamBase(upstream.URL)
	if _, err := pool.UpdateSettings(webpool.Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// 直接写凭据：绕过 manager 的未导出方法（经 manual 导入）。
	imported, _, err := pool.ImportManual("test-sso-1")
	if err != nil || imported != 1 {
		t.Fatalf("导入测试账号失败: imported=%d err=%v", imported, err)
	}
	server := &Server{WebPool: pool, ActualPort: 17878}
	return server, pool
}

func webV1ChatRequest(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/web/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+server.WebPool.LocalAPIKey())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleWebV1ChatCompletions(rec, req)
	return rec
}

func TestWebV1ModelsEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	server, _ := newWebPoolTestServer(t, upstream)

	req := httptest.NewRequest(http.MethodGet, "/web/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+server.WebPool.LocalAPIKey())
	rec := httptest.NewRecorder()
	server.handleWebV1Models(rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("models 状态码: %d", rec.Code)
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) == 0 {
		t.Fatal("模型列表为空")
	}
}

func TestWebV1ChatCompletionsStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		writeLines(w,
			`{"result":{"response":{"token":"先想","isThinking":true}}}`,
			`{"result":{"response":{"token":"一秒","isThinking":true}}}`,
			`{"result":{"response":{"token":"答案是2","isThinking":false}}}`,
		)
	}))
	defer upstream.Close()
	server, _ := newWebPoolTestServer(t, upstream)

	body := `{"model":"grok-4-reasoning","stream":true,"messages":[{"role":"user","content":"1+1?"}]}`
	rec := webV1ChatRequest(t, server, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码: %d body=%s", rec.Code, rec.Body.String())
	}

	var reasonings, contents []string
	// 帧是 JSON 行（非 data: 前缀），逐行解析。
	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		if frame.Error != nil {
			t.Fatalf("流内错误: %s", frame.Error.Message)
		}
		if len(frame.Choices) == 0 {
			continue
		}
		delta := frame.Choices[0].Delta
		if delta.ReasoningContent != "" {
			reasonings = append(reasonings, delta.ReasoningContent)
		}
		if delta.Content != "" {
			contents = append(contents, delta.Content)
		}
	}
	if strings.Join(reasonings, "") != "先想一秒" {
		t.Fatalf("reasoning_content 错误: %v", reasonings)
	}
	if strings.Join(contents, "") != "答案是2" {
		t.Fatalf("content 错误: %v", contents)
	}
}

func TestWebV1ChatCompletionsNonStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		writeLines(w,
			`{"result":{"response":{"token":"想想","isThinking":true}}}`,
			`{"result":{"response":{"token":"你好！","isThinking":false}}}`,
		)
	}))
	defer upstream.Close()
	server, _ := newWebPoolTestServer(t, upstream)

	body := `{"model":"grok-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	rec := webV1ChatRequest(t, server, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码: %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Choices) != 1 {
		t.Fatalf("choices 数量: %d", len(payload.Choices))
	}
	msg := payload.Choices[0].Message
	if msg.Content != "你好！" || msg.ReasoningContent != "想想" {
		t.Fatalf("非流式内容错误: %+v", msg)
	}
}

func TestWebV1RejectsBadKey(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	server, _ := newWebPoolTestServer(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/web/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec := httptest.NewRecorder()
	// 直接走总入口验证鉴权。
	server.handleWebV1(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 key 应 401: %d", rec.Code)
	}
}

func TestWebV1DisabledReturns503(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	server, _ := newWebPoolTestServer(t, upstream)
	if _, err := server.WebPool.UpdateSettings(webpool.Settings{Enabled: false}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/web/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+server.WebPool.LocalAPIKey())
	rec := httptest.NewRecorder()
	server.handleWebV1(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未启用应 503: %d", rec.Code)
	}
}

func writeLines(w http.ResponseWriter, lines ...string) {
	for _, l := range lines {
		_, _ = w.Write([]byte(l + "\n"))
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
