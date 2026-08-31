package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 能力表测试

func TestCapabilityFor(t *testing.T) {
	cap := capabilityFor("grok-4.6", true, []string{"low", "medium", "high"}, 500000, 65536)
	if !cap.ToolUse || !cap.Thinking || !cap.ImageIn {
		t.Fatalf("grok-4.6 应具备 tool_use/thinking/image_in: %+v", cap)
	}
	if cap.MaxContextTokens != 500000 || cap.MaxCompletionTokens != 65536 {
		t.Fatalf("窗口字段未透传: %+v", cap)
	}
	unknown := capabilityFor("some-unknown-model", false, nil, 0, 0)
	if unknown.ImageIn || unknown.Thinking {
		t.Fatalf("未知模型应保守降级: %+v", unknown)
	}
}

func TestValidateEffort(t *testing.T) {
	cap := capabilityFor("grok-4.6", true, []string{"low", "medium", "high"}, 100, 100)
	if got := validateEffort("HIGH", cap); got != "high" {
		t.Fatalf("大写强度应归一, got %q", got)
	}
	if got := validateEffort("ultra", cap); got != "" {
		t.Fatalf("不支持强度应返回空, got %q", got)
	}
	plain := capabilityFor("m2", false, nil, 100, 100)
	if got := validateEffort("high", plain); got != "" {
		t.Fatalf("不支持 thinking 的模型强度应为空, got %q", got)
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens("hello world"); got != 2 {
		t.Fatalf("ASCII 11 字符应约 2 token, got %d", got)
	}
	if got := EstimateTokens("你好世界你好世界"); got == 0 {
		t.Fatalf("CJK 应有非零估算")
	}
}

// 通用断言辅助

func assertStreamResult(t *testing.T, res *StreamResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	if res == nil {
		t.Fatal("结果为空")
	}
	if res.Accuracy != UsageExact {
		t.Fatalf("httptest 上游应回传 usage, got %v", res.Accuracy)
	}
	if res.FinishReason != "tool_use" {
		t.Fatalf("期望 tool_use, got %q", res.FinishReason)
	}
	if len(res.Message.ToolCalls) != 1 {
		t.Fatalf("期望 1 个工具调用, got %d", len(res.Message.ToolCalls))
	}
	tc := res.Message.ToolCalls[0]
	if tc.Name != "read" {
		t.Fatalf("工具名错误: %q", tc.Name)
	}
	if !strings.Contains(string(tc.Arguments), "internal/llm/types.go") {
		t.Fatalf("参数拼接错误: %s", string(tc.Arguments))
	}
}

// --- Responses 协议 ---

func responsesUpstream(t *testing.T, nonStream bool, checkReq func(*testing.T, *http.Request, map[string]any)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if checkReq != nil {
			checkReq(t, r, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if nonStream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "resp_1",
				"status": "completed",
				"output": []map[string]any{
					{
						"type":      "function_call",
						"call_id":   "call_abc",
						"name":      "read",
						"arguments": `{"path":"internal/llm/types.go"}`,
					},
				},
				"usage": map[string]any{
					"input_tokens":         100,
					"output_tokens":        20,
					"input_tokens_details": map[string]any{"cached_tokens": 60},
				},
			})
			return
		}
		f := w.(http.Flusher)
		writeSSE := func(event string, payload string) {
			_, _ = w.Write([]byte("event: " + event + "\ndata: " + payload + "\n\n"))
			f.Flush()
		}
		writeSSE("response.output_text.delta", `{"type":"response.output_text.delta","delta":"查看"}`)
		writeSSE("response.output_text.delta", `{"type":"response.output_text.delta","delta":"文件"}`)
		writeSSE("response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_abc","name":"read"}}`)
		writeSSE("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":\"internal/"}`)
		writeSSE("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":1,"delta":"llm/types.go\"}"}`)
		writeSSE("response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":60}}}}`)
	})
	return httptest.NewServer(mux)
}

func TestResponsesProviderStream(t *testing.T) {
	srv := responsesUpstream(t, false, nil)
	defer srv.Close()

	p, err := NewResponsesProvider(UpstreamTarget{BaseURL: srv.URL + "/v1", APIKey: "k-test"}, "grok-4.6", "sess-1", capabilityFor("grok-4.6", true, nil, 500000, 65536))
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	res, err := p.Generate(context.Background(), "你是编码代理", []Tool{{Name: "read", Description: "读文件", Schema: map[string]any{"type": "object"}}},
		[]Message{{Role: RoleUser, Parts: []ContentPart{TextPart{Text: "读一下 types.go"}}}},
		GenerateOptions{OnDelta: func(part StreamedPart) {
			switch v := part.(type) {
			case TextDelta:
				deltas = append(deltas, v.Text)
			}
		}})
	assertStreamResult(t, res, err)
	if strings.Join(deltas, "") != "查看文件" {
		t.Fatalf("流式文本增量错误: %v", deltas)
	}
	if res.Usage.InputCacheRead != 60 || res.Usage.InputOther != 40 || res.Usage.Output != 20 {
		t.Fatalf("usage 分解错误: %+v", res.Usage)
	}
}

func TestResponsesProviderNonStream(t *testing.T) {
	srv := responsesUpstream(t, true, nil)
	defer srv.Close()
	p, _ := NewResponsesProvider(UpstreamTarget{BaseURL: srv.URL + "/v1"}, "grok-4.6", "sess-1", capabilityFor("grok-4.6", true, nil, 500000, 65536))
	res, err := p.Generate(context.Background(), "", nil, []Message{{Role: RoleUser, Parts: []ContentPart{TextPart{Text: "hi"}}}}, GenerateOptions{})
	assertStreamResult(t, res, err)
}

func TestResponsesProviderWireShape(t *testing.T) {
	srv := responsesUpstream(t, true, func(t *testing.T, r *http.Request, body map[string]any) {
		if body["stream"] != false {
			t.Fatalf("非流式请求 stream 应为 false")
		}
		if body["store"] != false {
			t.Fatalf("无状态重放 store 应为 false (D1b)")
		}
		if _, has := body["previous_response_id"]; has {
			t.Fatalf("不应发送 previous_response_id (D1b)")
		}
		if body["prompt_cache_key"] != "sess-9" {
			t.Fatalf("sessionKey 应写入 prompt_cache_key")
		}
		input, _ := body["input"].([]any)
		// user 消息 + assistant(function_call) + function_call_output = 3 条。
		// assistant 的工具调用与文本分开成条目（Responses wire 形态）。
		if len(input) != 3 {
			t.Fatalf("期望 user + function_call + function_call_output 三条, got %d", len(input))
		}
		assistCall, _ := input[1].(map[string]any)
		if assistCall["type"] != "function_call" || assistCall["call_id"] != "call_9" {
			t.Fatalf("assistant function_call 翻译错误: %v", assistCall)
		}
		toolOut, _ := input[2].(map[string]any)
		if toolOut["type"] != "function_call_output" || toolOut["call_id"] != "call_9" {
			t.Fatalf("工具结果翻译错误: %v", toolOut)
		}
		if r.Header.Get("Authorization") != "Bearer k-test" {
			t.Fatalf("鉴权头缺失")
		}
	})
	defer srv.Close()

	p, _ := NewResponsesProvider(UpstreamTarget{BaseURL: srv.URL + "/v1", APIKey: "k-test"}, "grok-4.6", "sess-9", capabilityFor("grok-4.6", true, nil, 500000, 65536))
	history := []Message{
		{Role: RoleUser, Parts: []ContentPart{TextPart{Text: "读文件"}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_9", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)}}},
		{Role: RoleTool, ToolCallID: "call_9", Parts: []ContentPart{TextPart{Text: "package llm"}}},
	}
	res, err := p.Generate(context.Background(), "", nil, history, GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("结果为空")
	}
}

func TestResponsesProviderUpstreamError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "rate limited", "code": "429"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, _ := NewResponsesProvider(UpstreamTarget{BaseURL: srv.URL + "/v1"}, "m", "s", capabilityFor("m", true, nil, 100, 100))
	_, err := p.Generate(context.Background(), "", nil, []Message{{Role: RoleUser, Parts: []ContentPart{TextPart{Text: "x"}}}}, GenerateOptions{})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("期望 *APIError, got %T: %v", err, err)
	}
	if apiErr.Kind != "rate_limited" || apiErr.RetryAfter != 2*1e9 {
		t.Fatalf("错误分类错误: %+v", apiErr)
	}
	if !RetryableKind(apiErr.Kind) {
		t.Fatal("限流应可重试")
	}
}

// --- chat/completions 协议 ---

func ccUpstream(t *testing.T, withUsage bool, checkReq func(*testing.T, map[string]any)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if checkReq != nil {
			checkReq(t, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		writeSSE := func(payload string) {
			_, _ = w.Write([]byte("data: " + payload + "\n\n"))
			f.Flush()
		}
		base := func() map[string]any {
			return map[string]any{
				"choices": []map[string]any{
					{"index": 0, "delta": map[string]any{}},
				},
			}
		}
		first := base()
		first["choices"].([]map[string]any)[0]["delta"] = map[string]any{"role": "assistant", "content": "看看"}
		writeSSE(mustJSON(first))
		second := base()
		second["choices"].([]map[string]any)[0]["delta"] = map[string]any{"content": "文件"}
		writeSSE(mustJSON(second))
		tool := base()
		tool["choices"].([]map[string]any)[0]["delta"] = map[string]any{
			"tool_calls": []map[string]any{
				{"index": 0, "id": "call_cc1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{\"path\":\"int"}},
			},
		}
		writeSSE(mustJSON(tool))
		tool2 := base()
		tool2["choices"].([]map[string]any)[0]["delta"] = map[string]any{
			"tool_calls": []map[string]any{
				{"index": 0, "function": map[string]any{"arguments": "ernal/llm/types.go\"}"}},
			},
		}
		writeSSE(mustJSON(tool2))
		done := base()
		finish := "tool_calls"
		done["choices"].([]map[string]any)[0]["finish_reason"] = &finish
		if withUsage {
			done["usage"] = map[string]any{
				"prompt_tokens": 90, "completion_tokens": 25,
				"prompt_tokens_details": map[string]any{"cached_tokens": 30},
			}
		}
		writeSSE(mustJSON(done))
		writeSSE("[DONE]")
	})
	return httptest.NewServer(mux)
}

func TestChatCompletionsStreamWithUsage(t *testing.T) {
	srv := ccUpstream(t, true, func(t *testing.T, body map[string]any) {
		if body["stream"] != true {
			t.Fatal("应请求流式")
		}
		so, _ := body["stream_options"].(map[string]any)
		if so == nil || so["include_usage"] != true {
			t.Fatal("应请求 include_usage")
		}
	})
	defer srv.Close()

	p, err := NewChatCompletionsProvider(UpstreamTarget{BaseURL: srv.URL + "/v1"}, "grok-4.6", capabilityFor("grok-4.6", true, nil, 500000, 65536))
	if err != nil {
		t.Fatal(err)
	}
	var texts strings.Builder
	res, err := p.Generate(context.Background(), "sys", []Tool{{Name: "read", Schema: map[string]any{"type": "object"}}},
		[]Message{{Role: RoleUser, Parts: []ContentPart{TextPart{Text: "读"}}}},
		GenerateOptions{OnDelta: func(part StreamedPart) {
			if v, ok := part.(TextDelta); ok {
				texts.WriteString(v.Text)
			}
		}})
	assertStreamResult(t, res, err)
	if texts.String() != "看看文件" {
		t.Fatalf("流式文本错误: %q", texts.String())
	}
	if res.Usage.InputCacheRead != 30 || res.Usage.InputOther != 60 || res.Usage.Output != 25 {
		t.Fatalf("usage 错误: %+v", res.Usage)
	}
}

func TestChatCompletionsStreamApproxUsage(t *testing.T) {
	srv := ccUpstream(t, false, nil)
	defer srv.Close()
	p, _ := NewChatCompletionsProvider(UpstreamTarget{BaseURL: srv.URL + "/v1"}, "grok-4.6", capabilityFor("grok-4.6", true, nil, 500000, 65536))
	res, err := p.Generate(context.Background(), "", nil, []Message{{Role: RoleUser, Parts: []ContentPart{TextPart{Text: "x"}}}}, GenerateOptions{OnDelta: func(StreamedPart) {}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Accuracy != UsageApproximate {
		t.Fatalf("未回传 usage 应标记近似: %v", res.Accuracy)
	}
	if res.Usage.Output <= 0 {
		t.Fatalf("近似 usage 应有非零估算: %+v", res.Usage)
	}
}

func TestChatCompletionsWireShape(t *testing.T) {
	srv := ccUpstream(t, true, func(t *testing.T, body map[string]any) {
		msgs, _ := body["messages"].([]any)
		// system + user + assistant(tool_calls) + tool = 4 条。
		if len(msgs) != 4 {
			t.Fatalf("期望 system+user+assistant+tool 四条消息, got %d", len(msgs))
		}
		sys, _ := msgs[0].(map[string]any)
		if sys["role"] != "system" {
			t.Fatal("system prompt 应作为首条 system 消息")
		}
		assist, _ := msgs[2].(map[string]any)
		if assist["role"] != "assistant" {
			t.Fatalf("assistant 消息顺序错误: %v", assist)
		}
		toolMsg, _ := msgs[3].(map[string]any)
		if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_t9" {
			t.Fatalf("tool 消息翻译错误: %v", toolMsg)
		}
	})
	defer srv.Close()
	p, _ := NewChatCompletionsProvider(UpstreamTarget{BaseURL: srv.URL + "/v1", APIKey: "kk"}, "m1", capabilityFor("m1", true, nil, 100, 100))
	history := []Message{
		{Role: RoleUser, Parts: []ContentPart{TextPart{Text: "hi"}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_t9", Name: "bash", Arguments: json.RawMessage(`{"cmd":"ls"}`)}}},
		{Role: RoleTool, ToolCallID: "call_t9", Parts: []ContentPart{TextPart{Text: "file.go"}}},
	}
	if _, err := p.Generate(context.Background(), "sys", nil, history, GenerateOptions{OnDelta: func(StreamedPart) {}}); err != nil {
		t.Fatal(err)
	}
}

func TestChatCompletionsImageContent(t *testing.T) {
	var gotParts []any
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		msgs, _ := body["messages"].([]any)
		first, _ := msgs[0].(map[string]any)
		content := first["content"]
		if arr, ok := content.([]any); ok {
			gotParts = arr
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, _ := NewChatCompletionsProvider(UpstreamTarget{BaseURL: srv.URL + "/v1"}, "vision-m", capabilityFor("vision-m", false, nil, 100, 100))
	msg := Message{Role: RoleUser, Parts: []ContentPart{
		TextPart{Text: "这是什么图"},
		ImagePart{Data: "aGVsbG8=", MimeType: "image/png"},
	}}
	if _, err := p.Generate(context.Background(), "", nil, []Message{msg}, GenerateOptions{OnDelta: func(StreamedPart) {}}); err != nil {
		t.Fatal(err)
	}
	if len(gotParts) != 2 {
		t.Fatalf("图片消息应翻译为多部件数组, got %d", len(gotParts))
	}
	img, _ := gotParts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("部件类型错误: %v", img)
	}
}

func TestNormalizeFinishReason(t *testing.T) {
	cases := map[string]string{
		"stop": "stop", "end_turn": "stop", "tool_calls": "tool_use",
		"length": "max_tokens", "content_filter": "filtered", "": "", "weird": "unknown",
	}
	for in, want := range cases {
		if got := NormalizeFinishReason(in); got != want {
			t.Fatalf("NormalizeFinishReason(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestScanSSEMultiLineData(t *testing.T) {
	// 多行 data: 应拼接为单个事件负载（换行保留）。
	var got sseEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("event: e1\ndata: line1\ndata: line2\n\n"))
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	err = scanSSE(context.Background(), resp, func(ev sseEvent) error {
		got = ev
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Event != "e1" || string(got.Data) != "line1\nline2" {
		t.Fatalf("SSE 解析错误: %+v", got)
	}
}
