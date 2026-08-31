package llm

import (
	"fmt"
	"strings"
)

// ProfileBridge 是 server 层 Profile 数据的最小投影。定义在本包以避免
// 引擎依赖 profiles 存储（分层契约，设计文档 §5）；server 层负责把
// profiles.Profile 映射到这里。
type ProfileBridge struct {
	// UpstreamFormat 决定适配器："openai_responses"/"responses" → ResponsesProvider，
	// 其余（含空）→ ChatCompletionsProvider（D7：中转默认按 chat/completions 适配）。
	UpstreamFormat string
	// WirePathOverride 覆盖请求路径；空按适配器默认（/responses | /chat/completions）。
	// loopback 代理场景传空即可（代理按 /grok/v1 前缀分发）。
	WirePathOverride string
	BaseURL          string
	APIKey           string
	ExtraHeaders     map[string]string
	ProxyURL         string
	Model            string
	SessionKey       string
	// ModelDef 投影（能力表来源）。
	SupportsReasoningEffort bool
	ReasoningEfforts        []string
	ContextWindow           int64
	MaxCompletionTokens     int64
}

// NewProviderForProfile 是 Profile→Provider 的唯一工厂。
// loopback 代理（127.0.0.1 的 /grok/v1）永远按 Responses 协议处理——那是
// Grok 上游的真实 wire 格式，与 Profile 的 upstream_format 无关；该判定
// 在这里集中做，server 层不需要重复。
func NewProviderForProfile(p ProfileBridge) (Provider, error) {
	target := UpstreamTarget{
		BaseURL:      strings.TrimRight(p.BaseURL, "/"),
		APIKey:       p.APIKey,
		ExtraHeaders: p.ExtraHeaders,
		ProxyURL:     p.ProxyURL,
	}
	if strings.TrimSpace(p.Model) == "" {
		return nil, fmt.Errorf("llm: Profile 未配置模型")
	}
	cap := capabilityFor(p.Model, p.SupportsReasoningEffort, p.ReasoningEfforts, p.ContextWindow, p.MaxCompletionTokens)
	format := normalizeFormat(p.UpstreamFormat)
	declared := strings.TrimSpace(p.UpstreamFormat) != ""
	// loopback 代理（本机 /grok/v1，即 Grok Auth 池）固定 Responses 协议——那是
	// Grok 上游的真实 wire 格式（D2）；Profile 显式声明格式的自建配置除外。
	if !declared && isLoopbackBase(target.BaseURL) {
		format = "responses"
	}
	switch format {
	case "responses":
		return newResponsesProviderFull(target, p.Model, p.SessionKey, cap, "/responses", p.WirePathOverride)
	default:
		return newChatCompletionsProviderFull(target, p.Model, cap, "/chat/completions", p.WirePathOverride)
	}
}

// newResponsesProviderFull 构造带路径覆盖的 Responses 适配器（工厂内部使用）。
func newResponsesProviderFull(target UpstreamTarget, model, sessionKey string, cap ModelCapability, defaultPath, override string) (Provider, error) {
	p, err := NewResponsesProvider(target, model, sessionKey, cap)
	if err != nil {
		return nil, err
	}
	if override != "" {
		p.wirePath = override
	} else {
		p.wirePath = defaultPath
	}
	return p, nil
}

// newChatCompletionsProviderFull 构造带路径覆盖的 chat/completions 适配器。
func newChatCompletionsProviderFull(target UpstreamTarget, model string, cap ModelCapability, defaultPath, override string) (Provider, error) {
	p, err := NewChatCompletionsProvider(target, model, cap)
	if err != nil {
		return nil, err
	}
	if override != "" {
		p.wirePath = override
	} else {
		p.wirePath = defaultPath
	}
	return p, nil
}

func normalizeFormat(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "openai_responses", "responses":
		return "responses"
	case "openai", "openai_chat", "chat_completions", "chatcompletions":
		return "chat_completions"
	default:
		// D7：未标注格式按 chat/completions 兜底（中转的最普遍形态）。
		return "chat_completions"
	}
}

// isLoopbackBase 判断上游是否指向本机代理。
func isLoopbackBase(base string) bool {
	u := strings.TrimPrefix(base, "http://")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimSuffix(u, "/")
	host := u
	if idx := strings.Index(u, "/"); idx >= 0 {
		host = u[:idx]
	}
	if idx := strings.Index(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
