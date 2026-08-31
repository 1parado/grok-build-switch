package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFactoryLoopbackForcesResponses(t *testing.T) {
	// loopback 且未声明格式 → 固定 Responses 协议（Grok Auth 池路径）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/grok/v1/responses" {
			t.Errorf("loopback 应请求 Responses 端点, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "r1", "status": "completed", "output": []any{},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	defer srv.Close()

	p, err := NewProviderForProfile(ProfileBridge{
		BaseURL:    srv.URL + "/grok/v1",
		APIKey:     "k",
		Model:      "grok-4.6",
		SessionKey: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "responses" {
		t.Fatalf("loopback 应强制 responses, got %s", p.Name())
	}
	if _, err := p.Generate(context.Background(), "", nil, []Message{{Role: RoleUser, Parts: []ContentPart{TextPart{Text: "hi"}}}}, GenerateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestFactoryThirdPartyDefaultsChatCompletions(t *testing.T) {
	// 未标注 upstream_format：按 D7 兜底为 chat/completions。
	// 用非 loopback 域名——httptest 绑定 127.0.0.1，会触发 loopback→responses
	// 的强制规则；这里只验证第三方域名的兜底路径选择，不实际发请求。
	p, err := NewProviderForProfile(ProfileBridge{
		BaseURL: "http://relay.example.test:8080/v1",
		Model:   "gpt-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "chat_completions" {
		t.Fatalf("期望 chat_completions, got %s", p.Name())
	}
}

func TestFactoryThirdPartyRealRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("显式声明的 chat/completions Profile 不应被改写, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	// 显式声明 openai 格式：即使 loopback 也按声明走（httptest 恰好验证这点）。
	p, err := NewProviderForProfile(ProfileBridge{
		UpstreamFormat: "openai",
		BaseURL:        srv.URL + "/v1",
		Model:          "gpt-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "chat_completions" {
		t.Fatalf("期望 chat_completions, got %s", p.Name())
	}
	if _, err := p.Generate(context.Background(), "", nil, []Message{{Role: RoleUser, Parts: []ContentPart{TextPart{Text: "hi"}}}}, GenerateOptions{OnDelta: func(StreamedPart) {}}); err != nil {
		t.Fatal(err)
	}
}

func TestFactoryWirePathOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/cc" {
			t.Errorf("路径覆盖未生效: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p, err := NewProviderForProfile(ProfileBridge{
		UpstreamFormat:   "openai",
		BaseURL:          srv.URL,
		WirePathOverride: "/custom/cc",
		Model:            "m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Generate(context.Background(), "", nil, []Message{{Role: RoleUser, Parts: []ContentPart{TextPart{Text: "x"}}}}, GenerateOptions{OnDelta: func(StreamedPart) {}}); err != nil {
		t.Fatal(err)
	}
}

func TestFactoryEmptyModel(t *testing.T) {
	if _, err := NewProviderForProfile(ProfileBridge{BaseURL: "http://example.com"}); err == nil {
		t.Fatal("空模型应报错")
	}
}

func TestNormalizeModelFamily(t *testing.T) {
	cases := map[string]string{
		"grok-4.6":               "grok-4.6",
		"Grok 4.6":               "grok 4.6",
		"grok-composer-2.5-fast": "grok-composer-2.5-fast",
		"claude@20250101":        "claude",
		"vendor:grok-3":          "vendor",
		"ns/grok-3":              "ns",
	}
	for in, want := range cases {
		if got := NormalizeModelFamily(in); got != want {
			t.Fatalf("NormalizeModelFamily(%q)=%q, want %q", in, got, want)
		}
	}
}
