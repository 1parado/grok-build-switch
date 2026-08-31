package llm

// ChatCompletionsProvider 适配 OpenAI chat/completions wire 协议（POST {base}/chat/completions，SSE）。
// 覆盖第三方中转（one-api/new-api 系）与任何 OpenAI 兼容后端（见设计文档 D7）。
//
// 会话状态模型与 Responses 相同：无状态重放（D1b），历史全量进 messages。
// usage：流式请求带 stream_options.include_usage，上游多数会回传；
// 拿不到时按字符估算并标记 UsageApproximate（D3）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type ChatCompletionsProvider struct {
	target    UpstreamTarget
	model     string
	effort    string
	maxTokens int64
	cap       ModelCapability
	// wirePath 是请求路径；默认 "/chat/completions"，工厂可覆盖。
	wirePath string

	client httpClient
}

// NewChatCompletionsProvider 构造适配器。capability 由 server 层从 Profile 汇出；
// 该后端 usage 可信度按上游回传动态判定，初始标记见 Generate。
func NewChatCompletionsProvider(target UpstreamTarget, model string, cap ModelCapability) (*ChatCompletionsProvider, error) {
	client, err := newHTTPClient(target)
	if err != nil {
		return nil, err
	}
	return &ChatCompletionsProvider{
		target: target,
		model:  model,
		cap:    cap,
		client: client,
	}, nil
}

func (p *ChatCompletionsProvider) Name() string      { return "chat_completions" }
func (p *ChatCompletionsProvider) ModelName() string { return p.model }
func (p *ChatCompletionsProvider) Capability() ModelCapability {
	return p.cap
}

func (p *ChatCompletionsProvider) WithThinking(effort string) Provider {
	clone := *p
	clone.effort = effort
	return &clone
}

func (p *ChatCompletionsProvider) WithMaxOutputTokens(n int64) Provider {
	clone := *p
	clone.maxTokens = n
	return &clone
}

// --- wire 请求/响应结构 ---

type ccRequest struct {
	Model           string           `json:"model"`
	Messages        []ccMessage      `json:"messages"`
	Tools           []ccTool         `json:"tools,omitempty"`
	Stream          bool             `json:"stream"`
	StreamOptions   *ccStreamOptions `json:"stream_options,omitempty"`
	MaxTokens       int64            `json:"max_tokens,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
}

type ccStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ccTool struct {
	Type     string    `json:"type"`
	Function ccToolDef `json:"function"`
}

type ccToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ccMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ccToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type ccToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function ccFunc `json:"function"`
}

type ccFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// --- 统一消息 → chat/completions messages 翻译 ---

// ccContentPart 是 content 数组部件（多模态形态）。
type ccContentPart struct {
	Type     string      `json:"type"` // text | image_url
	Text     string      `json:"text,omitempty"`
	ImageURL *ccImageURL `json:"image_url,omitempty"`
}

type ccImageURL struct {
	URL string `json:"url"`
}

func buildCCMessages(systemPrompt string, history []Message) []ccMessage {
	msgs := make([]ccMessage, 0, len(history)+1)
	if strings.TrimSpace(systemPrompt) != "" {
		msgs = append(msgs, ccMessage{Role: "system", Content: mustJSONRaw(systemPrompt)})
	}
	for _, m := range history {
		switch m.Role {
		case RoleUser:
			msgs = append(msgs, ccMessage{Role: "user", Content: buildCCContent(m)})
		case RoleSystem:
			msgs = append(msgs, ccMessage{Role: "system", Content: buildCCContent(m)})
		case RoleAssistant:
			cm := ccMessage{Role: "assistant"}
			if txt := m.Text(); txt != "" {
				cm.Content = mustJSONRaw(txt)
			}
			for _, tc := range m.ToolCalls {
				cm.ToolCalls = append(cm.ToolCalls, ccToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: ccFunc{
						Name:      tc.Name,
						Arguments: string(tc.Arguments),
					},
				})
			}
			if cm.Content == nil && len(cm.ToolCalls) == 0 {
				cm.Content = mustJSONRaw("")
			}
			msgs = append(msgs, cm)
		case RoleTool:
			msgs = append(msgs, ccMessage{
				Role:       "tool",
				Content:    mustJSONRaw(m.Text()),
				ToolCallID: m.ToolCallID,
			})
		}
	}
	return msgs
}

// buildCCContent 把消息部件翻译成 content：纯文本 → 字符串；含图 → 数组。
func buildCCContent(m Message) json.RawMessage {
	hasImage := false
	texts := make([]string, 0, len(m.Parts))
	for _, p := range m.Parts {
		switch part := p.(type) {
		case TextPart:
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		case ImagePart:
			hasImage = true
		case ThinkPart:
			// 思考内容不重放（同 Responses 适配器的决策）。
		}
	}
	if hasImage {
		parts := make([]ccContentPart, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch part := p.(type) {
			case TextPart:
				if part.Text != "" {
					parts = append(parts, ccContentPart{Type: "text", Text: part.Text})
				}
			case ImagePart:
				url := part.URI
				if part.Data != "" {
					url = "data:" + imageMime(part) + ";base64," + part.Data
				}
				if url != "" {
					parts = append(parts, ccContentPart{Type: "image_url", ImageURL: &ccImageURL{URL: url}})
				}
			}
		}
		if len(parts) == 0 {
			return mustJSONRaw("")
		}
		b, _ := json.Marshal(parts)
		return b
	}
	return mustJSONRaw(strings.Join(texts, "\n"))
}

func mustJSONRaw(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func buildCCTools(tools []Tool) []ccTool {
	out := make([]ccTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Schema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, ccTool{
			Type: "function",
			Function: ccToolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}
	return out
}

// --- Generate ---

func (p *ChatCompletionsProvider) buildRequest(systemPrompt string, tools []Tool, history []Message, opts GenerateOptions) ([]byte, error) {
	if len(tools) > 0 && !p.cap.ToolUse {
		return nil, errToolUseUnsupported(p.model)
	}
	req := ccRequest{
		Model:     p.model,
		Messages:  buildCCMessages(systemPrompt, history),
		Tools:     buildCCTools(tools),
		Stream:    opts.OnDelta != nil,
		MaxTokens: p.maxTokens,
	}
	if opts.OnDelta != nil {
		req.StreamOptions = &ccStreamOptions{IncludeUsage: true}
	}
	if effort := validateEffort(effectiveEffort(p.effort, opts.Effort), p.cap); effort != "" {
		req.ReasoningEffort = effort
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (p *ChatCompletionsProvider) Generate(ctx context.Context, systemPrompt string, tools []Tool, history []Message, opts GenerateOptions) (*StreamResult, error) {
	body, err := p.buildRequest(systemPrompt, tools, history, opts)
	if err != nil {
		return nil, err
	}
	path := p.wirePath
	if path == "" {
		path = "/chat/completions"
	}
	resp, err := doJSON(ctx, p.client, p.target, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, NewAPIError(ClassifyStatus(resp.StatusCode), resp.StatusCode, decodeJSONBody(upstreamErrorBody(resp)), retryAfterFrom(resp))
	}
	if opts.OnDelta == nil {
		return decodeCCNonStream(resp)
	}
	return decodeCCStream(ctx, resp, opts.OnDelta)
}

// --- 非流式解析 ---

type ccResponse struct {
	Choices []struct {
		Message struct {
			Content          any          `json:"content"`
			ToolCalls        []ccToolCall `json:"tool_calls"`
			ReasoningContent string       `json:"reasoning_content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func decodeCCNonStream(resp *http.Response) (*StreamResult, error) {
	var parsed ccResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("llm: 解析 chat/completions 响应失败: %w", err)
	}
	return assembleCCChoice(len(parsed.Choices) > 0, &parsed.Choices[0], &parsed.Usage), nil
}

// assembleCCChoice 把单个 choice 装配成统一结果（流式/非流式共用）。
func assembleCCChoice(hasChoice bool, choice *struct {
	Message struct {
		Content          any          `json:"content"`
		ToolCalls        []ccToolCall `json:"tool_calls"`
		ReasoningContent string       `json:"reasoning_content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}, usage *struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}) *StreamResult {
	msg := Message{Role: RoleAssistant}
	if hasChoice {
		switch c := choice.Message.Content.(type) {
		case string:
			if c != "" {
				msg.Parts = append(msg.Parts, TextPart{Text: c})
			}
		case []any:
			for _, item := range c {
				if obj, ok := item.(map[string]any); ok {
					if t, _ := obj["text"].(string); t != "" {
						msg.Parts = append(msg.Parts, TextPart{Text: t})
					}
				}
			}
		}
		if rc := choice.Message.ReasoningContent; rc != "" {
			msg.Parts = append(msg.Parts, ThinkPart{Text: rc})
		}
		for _, tc := range choice.Message.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: json.RawMessage(orEmptyJSON(tc.Function.Arguments)),
			})
		}
	}
	result := &StreamResult{
		Message:      msg,
		FinishReason: NormalizeFinishReason(choiceFinish(hasChoice, choice)),
	}
	if usage != nil && usage.PromptTokens > 0 {
		cached := int64(0)
		if usage.PromptTokensDetails != nil {
			cached = usage.PromptTokensDetails.CachedTokens
		}
		result.Usage = TokenUsage{
			InputOther:     usage.PromptTokens - cached,
			Output:         usage.CompletionTokens,
			InputCacheRead: cached,
		}
		result.Accuracy = UsageExact
	} else {
		result.Usage = TokenUsage{Output: EstimateMessageTokens(msg)}
		result.Accuracy = UsageApproximate
	}
	return result
}

func choiceFinish(hasChoice bool, choice *struct {
	Message struct {
		Content          any          `json:"content"`
		ToolCalls        []ccToolCall `json:"tool_calls"`
		ReasoningContent string       `json:"reasoning_content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}) string {
	if !hasChoice {
		return "unknown"
	}
	return choice.FinishReason
}

// --- 流式解析 ---

// ccChunk 是 chat/completions 流式 chunk。
type ccChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			Role string `json:"role"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func decodeCCStream(ctx context.Context, resp *http.Response, onDelta func(StreamedPart)) (*StreamResult, error) {
	state := newCCStreamState()
	err := scanSSE(ctx, resp, func(ev sseEvent) error {
		if ev.Event != "" && ev.Event != "message" && ev.Event != "data" {
			// chat/completions SSE 通常无 event 字段；非标准 event 忽略。
			return nil
		}
		return state.handle(ev.Data, onDelta)
	})
	if err != nil {
		return nil, err
	}
	return state.result()
}

type ccStreamState struct {
	msg      Message
	usage    TokenUsage
	hasUsage bool
	finish   string
	// toolIndex 把上游工具调用 index 映射到 msg.ToolCalls 下标。
	toolIndex map[int]int
	// callArgs 按下标累积参数增量，保证拼接顺序稳定。
	callArgs map[int]string
}

func newCCStreamState() *ccStreamState {
	return &ccStreamState{
		toolIndex: map[int]int{},
		callArgs:  map[int]string{},
	}
}

func (s *ccStreamState) handle(data []byte, onDelta func(StreamedPart)) error {
	if string(data) == "[DONE]" {
		return nil
	}
	var chunk ccChunk
	if json.Unmarshal(data, &chunk) != nil {
		return nil
	}
	if chunk.Error != nil {
		return NewAPIError("unknown", 0, chunk.Error.Message, 0)
	}
	if chunk.Usage != nil {
		cached := int64(0)
		if chunk.Usage.PromptTokensDetails != nil {
			cached = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		s.usage = TokenUsage{
			InputOther:     chunk.Usage.PromptTokens - cached,
			Output:         chunk.Usage.CompletionTokens,
			InputCacheRead: cached,
		}
		if s.usage.InputOther < 0 {
			s.usage.InputOther = chunk.Usage.PromptTokens
		}
		s.hasUsage = true
	}
	for _, ch := range chunk.Choices {
		d := ch.Delta
		if d.ReasoningContent != "" {
			onDelta(ThinkDelta{Text: d.ReasoningContent})
			s.msg.Parts = append(s.msg.Parts, ThinkPart{Text: d.ReasoningContent})
		}
		if d.Content != "" {
			onDelta(TextDelta{Text: d.Content})
			s.appendText(d.Content)
		}
		for _, tcd := range d.ToolCalls {
			idx, ok := s.toolIndex[tcd.Index]
			if !ok {
				idx = len(s.msg.ToolCalls)
				s.toolIndex[tcd.Index] = idx
				s.msg.ToolCalls = append(s.msg.ToolCalls, ToolCall{
					ID:        tcd.ID,
					Name:      tcd.Function.Name,
					Arguments: json.RawMessage("{}"),
				})
				onDelta(ToolCallBegin{Call: s.msg.ToolCalls[idx]})
			}
			if tcd.Function.Arguments != "" {
				s.callArgs[idx] += tcd.Function.Arguments
				s.msg.ToolCalls[idx].Arguments = json.RawMessage(s.callArgs[idx])
				onDelta(ToolCallDelta{CallIndex: idx, ArgumentsDelta: tcd.Function.Arguments})
			}
			if tcd.Function.Name != "" {
				s.msg.ToolCalls[idx].Name = tcd.Function.Name
			}
			if tcd.ID != "" {
				s.msg.ToolCalls[idx].ID = tcd.ID
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finish = *ch.FinishReason
		}
	}
	return nil
}

func (s *ccStreamState) appendText(delta string) {
	if n := len(s.msg.Parts); n > 0 {
		if t, ok := s.msg.Parts[n-1].(TextPart); ok {
			s.msg.Parts[n-1] = TextPart{Text: t.Text + delta}
			return
		}
	}
	s.msg.Parts = append(s.msg.Parts, TextPart{Text: delta})
}

func (s *ccStreamState) result() (*StreamResult, error) {
	accuracy := UsageExact
	if !s.hasUsage {
		s.usage = TokenUsage{Output: EstimateMessageTokens(s.msg)}
		accuracy = UsageApproximate
	}
	// 工具调用优先于 finish_reason：部分中转不带 finish_reason 或提前给 stop。
	if len(s.msg.ToolCalls) > 0 {
		s.finish = "tool_use"
	} else if s.finish == "" {
		s.finish = "stop"
	}
	return &StreamResult{
		Message:      s.msg,
		Usage:        s.usage,
		Accuracy:     accuracy,
		FinishReason: NormalizeFinishReason(s.finish),
	}, nil
}
