package llm

import "context"

// Provider 是统一的上游对话接口（对标 kosong 的 ChatProvider）。
// 实现必须：可并发安全使用；不保存历史（引擎是唯一的历史持有者，D1b）；
// usage 如实标注 Accuracy。
type Provider interface {
	// Name 是适配器标识（"responses" / "chat_completions"）。
	Name() string
	// ModelName 是发送给上游的模型名。
	ModelName() string
	// Capability 返回模型能力表；未知字段返回零值，调用方自行 gate。
	Capability() ModelCapability
	// Generate 发起一次对话生成。history 为全量历史（无状态重放）。
	Generate(ctx context.Context, systemPrompt string, tools []Tool, history []Message, opts GenerateOptions) (*StreamResult, error)
	// WithThinking 返回设置推理强度后的浅拷贝。
	WithThinking(effort string) Provider
	// WithMaxOutputTokens 返回限制输出 token 后的浅拷贝；不支持实现原样返回自身。
	WithMaxOutputTokens(n int64) Provider
}

// ModelCapability 声明模型能力（对标 kosong capability.ts）。
// 零值/未登记模型用 UnknownCapability，调用方按最保守处理。
type ModelCapability struct {
	ImageIn  bool `json:"image_in"`
	VideoIn  bool `json:"video_in"`
	Thinking bool `json:"thinking"`
	ToolUse  bool `json:"tool_use"`
	// MaxContextTokens 是总上下文窗口（输入+输出）；0 表示未知。
	MaxContextTokens int `json:"max_context_tokens"`
	// MaxCompletionTokens 是输出上限；0 表示未知。
	MaxCompletionTokens int `json:"max_completion_tokens"`
	// ReasoningEfforts 是模型支持的推理强度列表；空表示不支持强度调节。
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
	// UsageAccuracy 预告该后端 usage 的可信度，适配器在结果里必须一致。
	UsageAccuracy UsageAccuracy `json:"usage_accuracy,omitempty"`
}

// UnknownCapability 是能力未登记时的共享只读值。
var UnknownCapability = ModelCapability{}
