// Package llm 是自研 Agent 引擎的模型抽象层（对标 Kimi Code 的 kosong）。
//
// 引擎内部只使用本包的类型与 Provider 接口，不感知上游 wire 协议；
// 每个上游协议（OpenAI Responses / chat/completions）由独立适配器翻译。
// 关键决策见 docs/design/native-agent-engine.md（D1/D1b/D7）。
package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Role 是统一消息模型里的角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// TextPart 是普通文本内容。
type TextPart struct {
	Text string `json:"text"`
}

// ThinkPart 是模型的推理/思考内容（如上游 reasoning summary）。
// 适配器负责把上游格式映射进来；回放历史时是否重发由各协议适配器决定。
type ThinkPart struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

// ImagePart 是图片输入内容。Data 与 URI 二选一：Data 为 base64（不带 data: 前缀），
// URI 为可访问地址。
type ImagePart struct {
	MimeType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// ContentPart 是统一内容部件。Go 1.26 无 sealed interface，约定：包外只允许
// 构造 TextPart/ThinkPart/ImagePart；未知类型一律按 TextPart{Text: fmt.Sprint} 降级。
type ContentPart interface {
	contentPart()
}

func (TextPart) contentPart()  {}
func (ThinkPart) contentPart() {}
func (ImagePart) contentPart() {}

// ToolCall 是模型发起的一次工具调用。
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message 是引擎统一的消息表示（无状态重放的唯一事实来源，见 D1b）。
// ToolCallID 仅在 Role == RoleTool 时使用，对应被回复的 ToolCall.ID。
type Message struct {
	Role       Role          `json:"role"`
	Parts      []ContentPart `json:"parts,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

// Text 返回消息中全部 TextPart 拼接结果（仅用于日志/调试/标题派生）。
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if t, ok := p.(TextPart); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// TokenUsage 是单次 LLM 生成的用量分解，字段语义对齐 kosong 的 TokenUsage。
type TokenUsage struct {
	// InputOther 是未命中 prompt cache 的输入 token。
	InputOther int64 `json:"input_other"`
	// Output 是输出（completion）token。
	Output int64 `json:"output"`
	// InputCacheRead 是由上游 prompt cache 服务的输入 token。
	InputCacheRead int64 `json:"input_cache_read"`
	// InputCacheCreation 是写入上游 prompt cache 的输入 token。
	InputCacheCreation int64 `json:"input_cache_creation"`
}

func (u TokenUsage) InputTotal() int64 {
	return u.InputOther + u.InputCacheRead + u.InputCacheCreation
}

func (u TokenUsage) GrandTotal() int64 {
	return u.InputTotal() + u.Output
}

func (u TokenUsage) Add(other TokenUsage) TokenUsage {
	return TokenUsage{
		InputOther:         u.InputOther + other.InputOther,
		Output:             u.Output + other.Output,
		InputCacheRead:     u.InputCacheRead + other.InputCacheRead,
		InputCacheCreation: u.InputCacheCreation + other.InputCacheCreation,
	}
}

// UsageAccuracy 标记用量数字的可信度（见设计文档 D3/D7）。
type UsageAccuracy string

const (
	// UsageExact 表示来自上游真实回传的 usage 计数。
	UsageExact UsageAccuracy = "exact"
	// UsageApproximate 表示流式通道未回传 usage，按字符数/4 估算。
	// 使用近似值的调用方（compaction 触发器）应放宽阈值。
	UsageApproximate UsageAccuracy = "approximate"
)

// Tool 是暴露给模型的工具定义。Schema 为该工具参数的 JSON Schema 对象。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
}

// GenerateOptions 是一次生成的可选项。Stream 由 Provider 实现约定：
// 回调不为空即流式，否则 Provider 自行装配完整结果后一次性回调。
type GenerateOptions struct {
	// Effort 为推理强度（"low"/"medium"/"high"/"off"…），空串表示不指定。
	Effort string
	// MaxOutputTokens 限制输出 token；0 表示不限制。
	MaxOutputTokens int64
	// OnDelta 在流式过程中被调用，part 为增量片段；实现必须可容忍并发回调
	// 前的乱序（适配器保证同一字段顺序回调）。为空表示非流式。
	OnDelta func(part StreamedPart)
}

// StreamedPart 是流式增量。Adapter 会回调 TextDelta / ThinkDelta / ToolCallDelta /
// ToolCallBegin 之一。
type StreamedPart interface{ streamedPart() }

type TextDelta struct{ Text string }

type ThinkDelta struct{ Text string }

// ToolCallBegin 宣告一个新工具调用及其最终 ID/名称（参数随后增量到达）。
type ToolCallBegin struct {
	Call ToolCall
}

// ToolCallDelta 是工具调用参数的 JSON 文本增量。
type ToolCallDelta struct {
	CallIndex      int
	ArgumentsDelta string
}

func (TextDelta) streamedPart()     {}
func (ThinkDelta) streamedPart()    {}
func (ToolCallBegin) streamedPart() {}
func (ToolCallDelta) streamedPart() {}

// StreamResult 是一次生成的最终装配结果。
type StreamResult struct {
	// Message 是装配完成的 assistant 消息（文本/思考/工具调用合并）。
	Message Message
	// Usage 为本次生成用量；Accuracy 由适配器如实标注。
	Usage    TokenUsage
	Accuracy UsageAccuracy
	// FinishReason 是上游给出的结束原因的归一化表示，空表示未提供。
	// 约定值："stop" | "tool_use" | "max_tokens" | "filtered" | "unknown"。
	FinishReason string
}

// NormalizeFinishReason 把各协议的结束原因归一到约定值。
func NormalizeFinishReason(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "stop", "end_turn", "end", "complete":
		return "stop"
	case "tool_use", "tool_calls", "function_call", "tool":
		return "tool_use"
	case "max_tokens", "length", "max_output_tokens", "incomplete":
		return "max_tokens"
	case "content_filter", "filtered", "safety":
		return "filtered"
	case "":
		return ""
	default:
		return "unknown"
	}
}

// APIError 是 Provider 返回的归一化上游错误。Kind 供重试策略分类：
// rate_limited / auth / invalid_request / overloaded / network / unknown。
type APIError struct {
	Kind       string
	StatusCode int
	Upstream   string // 上游原始错误消息（已脱敏截断）
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("llm: 上游错误 %s (kind=%s): %s", http.StatusText(e.StatusCode), e.Kind, e.Upstream)
	}
	return fmt.Sprintf("llm: 上游错误 (kind=%s): %s", e.Kind, e.Upstream)
}
