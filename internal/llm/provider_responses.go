package llm

// ResponsesProvider 适配 OpenAI Responses wire 协议（POST {base}/responses，SSE）。
// 上游预期是 loopback /grok/v1 代理（见设计文档 D2/D1b）；也可指向任何
// Responses 兼容端点。
//
// 会话状态模型：无状态重放。每步发送完整 input 数组；不使用
// previous_response_id（D1b）。prompt_cache_key 由调用方通过 SessionKey 注入，
// 交给代理层做账号粘性。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// httpClient 抽象 *http.Client 便于测试注入。
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// decodeStream 解析 SSE 流并装配结果；onDelta 逐段回调。
func (p *ResponsesProvider) decodeStream(ctx context.Context, resp *http.Response, onDelta func(StreamedPart)) (*StreamResult, error) {
	state := newResponsesStreamState()
	err := scanSSE(ctx, resp, func(ev sseEvent) error {
		if ev.Event != "" && !strings.HasPrefix(ev.Event, "response.") {
			return nil
		}
		return state.handle(ev.Data, onDelta)
	})
	if err != nil {
		return nil, err
	}
	return state.result()
}

// responsesStreamState 累积流式事件并装配最终消息。
type responsesStreamState struct {
	msg      Message
	usage    TokenUsage
	hasUsage bool
	finish   string
	// callIndex 把上游 output_index 映射到 msg.ToolCalls 下标。
	callIndex map[int64]int
	// callBegin 已通告过的 output_index，避免重复 ToolCallBegin。
	callBegin map[int64]bool
	// callArgs 按 output_index 原样累积 function_call_arguments.delta；
	// 存字符串而不是直接写 RawMessage，保证中间态非法 JSON 也不影响回放序列化。
	callArgs map[int64]string
}

func newResponsesStreamState() *responsesStreamState {
	return &responsesStreamState{
		callIndex: map[int64]int{},
		callBegin: map[int64]bool{},
		callArgs:  map[int64]string{},
	}
}

// streamEnvelope 是 Responses SSE 事件的最外层。
type streamEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// responseCompleted 事件携带完整响应对象。
type responseCompleted struct {
	Response responsesResponse `json:"response"`
}

// outputItemDone 携带单个 output 条目（function_call 的兜底路径）。
type outputItemDone struct {
	OutputIndex int64 `json:"output_index"`
	Item        struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
}

func (s *responsesStreamState) handle(data []byte, onDelta func(StreamedPart)) error {
	// 事件负载本身是完整 JSON 对象（含 type 字段）；兼容 data 为 envelope 的两种形态。
	var typed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &typed); err != nil {
		return nil // 忽略无法解析的事件（心跳等）
	}
	switch typed.Type {
	case "response.output_text.delta":
		var e struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(data, &e) == nil && e.Delta != "" {
			onDelta(TextDelta{Text: e.Delta})
			s.appendText(e.Delta)
		}
	case "response.reasoning_summary_text.delta":
		var e struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(data, &e) == nil && e.Delta != "" {
			onDelta(ThinkDelta{Text: e.Delta})
			s.msg.Parts = append(s.msg.Parts, ThinkPart{Text: e.Delta})
		}
	case "response.output_item.added":
		var e struct {
			OutputIndex int64 `json:"output_index"`
			Item        struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
		}
		if json.Unmarshal(data, &e) == nil && e.Item.Type == "function_call" {
			idx := len(s.msg.ToolCalls)
			s.callIndex[e.OutputIndex] = idx
			// added 自带部分 arguments 时以它为初值；不要预设 "{}"——
			// 否则后续 delta 追加会拼出 {}{...} 的非法 JSON。
			s.callArgs[e.OutputIndex] = e.Item.Arguments
			s.msg.ToolCalls = append(s.msg.ToolCalls, ToolCall{
				ID:        e.Item.CallID,
				Name:      e.Item.Name,
				Arguments: json.RawMessage(e.Item.Arguments),
			})
			if !s.callBegin[e.OutputIndex] {
				s.callBegin[e.OutputIndex] = true
				onDelta(ToolCallBegin{Call: s.msg.ToolCalls[idx]})
			}
		}
	case "response.function_call_arguments.delta":
		var e struct {
			OutputIndex int64  `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if json.Unmarshal(data, &e) == nil && e.Delta != "" {
			if idx, ok := s.callIndex[e.OutputIndex]; ok {
				s.callArgs[e.OutputIndex] += e.Delta
				s.msg.ToolCalls[idx].Arguments = json.RawMessage(s.callArgs[e.OutputIndex])
				onDelta(ToolCallDelta{CallIndex: idx, ArgumentsDelta: e.Delta})
			}
		}
	case "response.output_item.done":
		var e outputItemDone
		if json.Unmarshal(data, &e) == nil && e.Item.Type == "function_call" {
			// done 携带完整 arguments：无论 added/delta 走过没有，都以它为准，
			// 修正部分代理增量流不完整或缺失的中间态。
			if idx := s.findCall(e.Item.CallID); idx >= 0 {
				s.callArgs[e.OutputIndex] = e.Item.Arguments
				s.msg.ToolCalls[idx].Arguments = json.RawMessage(orEmptyJSON(e.Item.Arguments))
			} else {
				// 兜底：若 added 事件缺失（部分代理实现），在此补齐。
				idx := len(s.msg.ToolCalls)
				s.callIndex[e.OutputIndex] = idx
				s.callArgs[e.OutputIndex] = e.Item.Arguments
				s.msg.ToolCalls = append(s.msg.ToolCalls, ToolCall{
					ID:        e.Item.CallID,
					Name:      e.Item.Name,
					Arguments: json.RawMessage(orEmptyJSON(e.Item.Arguments)),
				})
				onDelta(ToolCallBegin{Call: s.msg.ToolCalls[idx]})
			}
		}
	case "response.completed", "response.incomplete", "response.failed":
		var e responseCompleted
		if json.Unmarshal(data, &e) == nil {
			parsed := e.Response
			// 流式下 output 已逐段累积；仅采纳 usage 与状态。
			s.usage = TokenUsage{
				InputOther:     parsed.Usage.InputTokens - parsed.Usage.InputTokensDetails.CachedTokens,
				Output:         parsed.Usage.OutputTokens,
				InputCacheRead: parsed.Usage.InputTokensDetails.CachedTokens,
			}
			if s.usage.InputOther < 0 {
				s.usage.InputOther = parsed.Usage.InputTokens
			}
			s.hasUsage = true
			s.finish = responsesFinishReason(parsed.Status, parsed.IncompleteDetails)
			// 流式装配置信度高于逐段累积：若累积为空而响应带输出，兜底装配。
			if len(s.msg.Parts) == 0 && len(s.msg.ToolCalls) == 0 {
				result := assembleResponsesOutput(&parsed)
				s.msg = result.Message
			}
		}
		if typed.Type == "response.failed" {
			var e struct {
				Response struct {
					Error *struct {
						Message string `json:"message"`
						Code    string `json:"code"`
					} `json:"error"`
				} `json:"response"`
			}
			if json.Unmarshal(data, &e) == nil && e.Response.Error != nil {
				return NewAPIError("unknown", 0, decodeJSONBody(mustJSON(e.Response.Error)), 0)
			}
		}
	case "error":
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) == nil {
			return NewAPIError("unknown", 0, e.Message+" ["+e.Code+"]", 0)
		}
	}
	return nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *responsesStreamState) findCall(callID string) int {
	for i, tc := range s.msg.ToolCalls {
		if tc.ID == callID {
			return i
		}
	}
	return -1
}

func (s *responsesStreamState) appendText(delta string) {
	if n := len(s.msg.Parts); n > 0 {
		if t, ok := s.msg.Parts[n-1].(TextPart); ok {
			s.msg.Parts[n-1] = TextPart{Text: t.Text + delta}
			return
		}
	}
	s.msg.Parts = append(s.msg.Parts, TextPart{Text: delta})
}

func (s *responsesStreamState) result() (*StreamResult, error) {
	accuracy := UsageExact
	if !s.hasUsage {
		// 上游未回传 usage（部分中转代理）：按估算标记近似。
		total := EstimateMessageTokens(s.msg)
		s.usage = TokenUsage{Output: total}
		accuracy = UsageApproximate
	}
	// 工具调用优先于状态字段：completed 状态下携带 function_call 时，
	// loop 必须继续执行工具，finish 判定不能是 stop。
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

type ResponsesProvider struct {
	target     UpstreamTarget
	model      string
	sessionKey string
	effort     string
	maxTokens  int64
	cap        ModelCapability
	// wirePath 是请求路径；默认 "/responses"，工厂可覆盖。
	wirePath string

	client httpClient
}

// NewResponsesProvider 构造适配器。capability 由 server 层从 Profile 汇出。
func NewResponsesProvider(target UpstreamTarget, model, sessionKey string, cap ModelCapability) (*ResponsesProvider, error) {
	client, err := newHTTPClient(target)
	if err != nil {
		return nil, err
	}
	return &ResponsesProvider{
		target:     target,
		model:      model,
		sessionKey: sessionKey,
		cap:        cap,
		client:     client,
	}, nil
}

func (p *ResponsesProvider) Name() string      { return "responses" }
func (p *ResponsesProvider) ModelName() string { return p.model }
func (p *ResponsesProvider) Capability() ModelCapability {
	return p.cap
}

func (p *ResponsesProvider) WithThinking(effort string) Provider {
	clone := *p
	clone.effort = effort
	return &clone
}

func (p *ResponsesProvider) WithMaxOutputTokens(n int64) Provider {
	clone := *p
	clone.maxTokens = n
	return &clone
}

// responsesRequest 是 Responses wire 请求体的最小集。
type responsesRequest struct {
	Model           string              `json:"model"`
	Input           []responsesInput    `json:"input"`
	Instructions    string              `json:"instructions,omitempty"`
	Tools           []responsesTool     `json:"tools,omitempty"`
	Stream          bool                `json:"stream"`
	Store           bool                `json:"store"`
	MaxOutputTokens int64               `json:"max_output_tokens,omitempty"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
	PromptCacheKey  string              `json:"prompt_cache_key,omitempty"`
}

type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict,omitempty"`
}

// responsesInput 是 input 数组的一个条目。Responses wire 对消息、function_call、
// function_call_output 用不同的字段集，统一在一个 struct 里按需填充。
type responsesInput struct {
	Type string `json:"type,omitempty"` // message | function_call | function_call_output | reasoning

	Role    string          `json:"role,omitempty"`
	Content []responsesPart `json:"content,omitempty"`

	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`

	// reasoning 条目（回放 assistant 思考）：
	ID               string             `json:"id,omitempty"`
	Summary          []responsesSummary `json:"summary,omitempty"`
	EncryptedContent string             `json:"encrypted_content,omitempty"`
}

type responsesSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesPart struct {
	Type     string `json:"type"` // input_text | output_text | input_image
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// --- 统一消息 → Responses input 翻译 ---

func buildResponsesInput(history []Message) []responsesInput {
	items := make([]responsesInput, 0, len(history))
	for _, m := range history {
		switch m.Role {
		case RoleUser, RoleSystem:
			items = append(items, responsesInput{
				Type:    "message",
				Role:    string(m.Role),
				Content: buildResponsesContent(m, "input_text", "input_image"),
			})
		case RoleAssistant:
			if len(m.ToolCalls) > 0 {
				// 文本与 function_call 分开成条目；先文本后调用。
				if txt := m.Text(); txt != "" {
					items = append(items, responsesInput{
						Type:    "message",
						Role:    "assistant",
						Content: []responsesPart{{Type: "output_text", Text: txt}},
					})
				}
				for _, tc := range m.ToolCalls {
					items = append(items, responsesInput{
						Type:      "function_call",
						CallID:    tc.ID,
						Name:      tc.Name,
						Arguments: string(tc.Arguments),
					})
				}
			} else {
				items = append(items, responsesInput{
					Type:    "message",
					Role:    "assistant",
					Content: buildResponsesContent(m, "output_text", ""),
				})
			}
		case RoleTool:
			items = append(items, responsesInput{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: m.Text(),
			})
		}
	}
	return items
}

func buildResponsesContent(m Message, textType, imageType string) []responsesPart {
	parts := make([]responsesPart, 0, len(m.Parts))
	for _, p := range m.Parts {
		switch part := p.(type) {
		case TextPart:
			if part.Text != "" {
				parts = append(parts, responsesPart{Type: textType, Text: part.Text})
			}
		case ImagePart:
			if imageType == "" {
				continue
			}
			if part.Data != "" {
				parts = append(parts, responsesPart{Type: imageType, ImageURL: "data:" + imageMime(part) + ";base64," + part.Data})
			} else if part.URI != "" {
				parts = append(parts, responsesPart{Type: imageType, ImageURL: part.URI})
			}
		case ThinkPart:
			// 思考内容不作为消息内容重放：上游服务端 reasoning 状态在无状态
			// 重放下不可恢复，重发明文可能污染上下文。引擎在 compaction 层
			// 用摘要继承（ctxmem），这里直接丢弃。
		}
	}
	return parts
}

func imageMime(p ImagePart) string {
	if p.MimeType != "" {
		return p.MimeType
	}
	return "image/png"
}

// buildResponsesTools 翻译统一 Tool 定义。空 Schema 补 object。
func buildResponsesTools(tools []Tool) []responsesTool {
	out := make([]responsesTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Schema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, responsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
			Strict:      false,
		})
	}
	return out
}

// --- 请求体组装与响应解析 ---

func (p *ResponsesProvider) buildRequest(systemPrompt string, tools []Tool, history []Message, opts GenerateOptions) ([]byte, error) {
	if len(tools) > 0 && !p.cap.ToolUse {
		return nil, errToolUseUnsupported(p.model)
	}
	req := responsesRequest{
		Model:           p.model,
		Input:           buildResponsesInput(history),
		Instructions:    systemPrompt,
		Tools:           buildResponsesTools(tools),
		Stream:          opts.OnDelta != nil,
		Store:           false, // 无状态重放：上游不存会话（D1b）
		MaxOutputTokens: p.maxTokens,
		PromptCacheKey:  p.sessionKey,
	}
	if effort := validateEffort(effectiveEffort(p.effort, opts.Effort), p.cap); effort != "" {
		req.Reasoning = &responsesReasoning{Effort: effort, Summary: "auto"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func effectiveEffort(defaultEffort, perCall string) string {
	if perCall != "" {
		return perCall
	}
	return defaultEffort
}

func (p *ResponsesProvider) Generate(ctx context.Context, systemPrompt string, tools []Tool, history []Message, opts GenerateOptions) (*StreamResult, error) {
	body, err := p.buildRequest(systemPrompt, tools, history, opts)
	if err != nil {
		return nil, err
	}
	path := p.wirePath
	if path == "" {
		path = "/responses"
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
		return p.decodeNonStream(resp)
	}
	return p.decodeStream(ctx, resp, opts.OnDelta)
}

// --- 非流式解析 ---

type responsesResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"output"`
	Usage struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

func (p *ResponsesProvider) decodeNonStream(resp *http.Response) (*StreamResult, error) {
	var parsed responsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("llm: 解析 Responses 响应失败: %w", err)
	}
	result := assembleResponsesOutput(&parsed)
	result.Usage = TokenUsage{
		InputOther:     parsed.Usage.InputTokens - parsed.Usage.InputTokensDetails.CachedTokens,
		Output:         parsed.Usage.OutputTokens,
		InputCacheRead: parsed.Usage.InputTokensDetails.CachedTokens,
	}
	if result.Usage.InputOther < 0 {
		result.Usage.InputOther = parsed.Usage.InputTokens
	}
	result.Accuracy = UsageExact
	result.FinishReason = responsesFinishReason(parsed.Status, parsed.IncompleteDetails)
	// 工具调用优先于状态字段：completed 状态携带 function_call 时必须继续执行工具。
	if len(result.Message.ToolCalls) > 0 {
		result.FinishReason = "tool_use"
	}
	return result, nil
}

// assembleResponsesOutput 把 output 数组装配成统一消息（流式/非流式共用）。
func assembleResponsesOutput(parsed *responsesResponse) *StreamResult {
	msg := Message{Role: RoleAssistant}
	finish := ""
	for _, item := range parsed.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Text != "" {
					msg.Parts = append(msg.Parts, TextPart{Text: c.Text})
				}
			}
		case "reasoning":
			for _, s := range item.Summary {
				if s.Text != "" {
					msg.Parts = append(msg.Parts, ThinkPart{Text: s.Text})
				}
			}
		case "function_call":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:        item.CallID,
				Name:      item.Name,
				Arguments: json.RawMessage(orEmptyJSON(item.Arguments)),
			})
		}
	}
	if len(msg.ToolCalls) > 0 {
		finish = "tool_use"
	}
	return &StreamResult{Message: msg, FinishReason: finish}
}

func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

func responsesFinishReason(status string, incomplete *struct {
	Reason string `json:"reason"`
}) string {
	switch strings.ToLower(status) {
	case "completed":
		return "stop"
	case "incomplete":
		if incomplete != nil && strings.Contains(incomplete.Reason, "token") {
			return "max_tokens"
		}
		return "filtered"
	default:
		return "unknown"
	}
}
