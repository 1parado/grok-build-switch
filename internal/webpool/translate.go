package webpool

import (
	"fmt"
	"strings"
)

// OpenAI 兼容翻译层：把网页通道的请求/响应翻成 /v1/chat/completions 形态。
// 思考（isThinking 增量）→ delta.reasoning_content；正文 → delta.content。

// webModel 描述一个对外模型及其网页后端映射。
type webModel struct {
	Name        string // 对外模型名（/v1/models 暴露）
	ModelName   string // 网页 modelName（grok-3 / grok-4）
	IsReasoning bool   // 请求 isReasoning 标志
}

var webModelCatalog = []webModel{
	{Name: "grok-4", ModelName: "grok-4", IsReasoning: false},
	{Name: "grok-4-reasoning", ModelName: "grok-4", IsReasoning: true},
	{Name: "grok-3", ModelName: "grok-3", IsReasoning: false},
	{Name: "grok-3-reasoning", ModelName: "grok-3", IsReasoning: true},
}

// DefaultModel 是内置工作台接入时的默认模型。
const DefaultModel = "grok-4"

// ResolveModel 把对外模型名解析为网页请求参数；未知名称回落 grok-4。
func ResolveModel(name string) webModel {
	name = strings.TrimSpace(name)
	for _, m := range webModelCatalog {
		if m.Name == name {
			return m
		}
	}
	// 直接传网页模型名（grok-3/grok-4）也可用。
	for _, m := range webModelCatalog {
		if m.ModelName == name {
			return m
		}
	}
	return webModel{ModelName: "grok-4", IsReasoning: false}
}

// ModelList 返回 /v1/models 负载条目。
func ModelList() []map[string]any {
	out := make([]map[string]any, 0, len(webModelCatalog))
	for _, m := range webModelCatalog {
		out = append(out, map[string]any{
			"id":       m.Name,
			"object":   "model",
			"created":  1735689600,
			"owned_by": "grok-switch-web",
		})
	}
	return out
}

// OpenAIMessage 是 chat/completions 请求里的消息。
type OpenAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// maxTranscriptChars 超长转录截断（v1 不做文件上传，见设计文档「明确不做」）。
const maxTranscriptChars = 40000

// FlattenTranscript 把多轮消息扁平化为网页 API 的单一 message：
// grok2api 的成熟形态（[role]\n内容），system 消息并入最前。
func FlattenTranscript(messages []OpenAIMessage) string {
	var b strings.Builder
	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "user"
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		switch role {
		case "system", "developer":
			fmt.Fprintf(&b, "[system]\n%s\n\n", content)
		default:
			fmt.Fprintf(&b, "[%s]\n%s\n\n", role, content)
		}
	}
	transcript := strings.TrimRight(b.String(), "\n")
	if len(transcript) > maxTranscriptChars {
		transcript = transcript[:maxTranscriptChars]
	}
	return transcript
}

// ChatChunkFrame 是一个流式 chat.completion.chunk。
type ChatChunkFrame struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

func finishStop() *string {
	s := "stop"
	return &s
}

// NewChunk 构建一个增量帧。
func NewChunk(id, model, content, reasoning string) ChatChunkFrame {
	return ChatChunkFrame{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: 1735689600,
		Model:   model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{Content: content, ReasoningContent: reasoning}}},
	}
}

// NewFinishChunk 构建终止帧（finish_reason=stop，空 delta）。
func NewFinishChunk(id, model string) ChatChunkFrame {
	return ChatChunkFrame{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: 1735689600,
		Model:   model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{}, FinishReason: finishStop()}},
	}
}

// ChatCompletion 是非流式响应对象。
type ChatCompletion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   completionUsage    `json:"usage"`
}

type completionChoice struct {
	Index        int             `json:"index"`
	Message      completeMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type completeMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type completionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// NewCompletion 聚合非流式响应。
func NewCompletion(id, model, content, reasoning string, promptChars int) ChatCompletion {
	return ChatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: 1735689600,
		Model:   model,
		Choices: []completionChoice{{
			Index:        0,
			Message:      completeMessage{Role: "assistant", Content: content, ReasoningContent: reasoning},
			FinishReason: "stop",
		}},
		Usage: completionUsage{
			PromptTokens:     promptChars / 4,
			CompletionTokens: (len(content) + len(reasoning)) / 4,
			TotalTokens:      (promptChars + len(content) + len(reasoning)) / 4,
		},
	}
}
