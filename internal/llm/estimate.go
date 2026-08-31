package llm

import (
	"strings"
	"unicode/utf8"
)

// EstimateTokens 按字符数近似估算 token（≈4 字符/token）。
// 仅用于 chat/completions 后端拿不到真实 usage 时的 compaction 触发预算，
// 以及测试；禁止用于计费口径。UTF-8 安全：按 rune 计数，非 ASCII 文本
// 按 1.5 rune/token 收紧（Grok/中英混合语料的经验区间）。
func EstimateTokens(s string) int64 {
	if s == "" {
		return 0
	}
	runes := float64(utf8.RuneCountInString(s))
	ascii := float64(len(s))/float64(utf8.RuneCountInString(s)) == 1
	if ascii {
		// 纯 ASCII：约 4 字符/token，另加每消息固定开销已在调用方计。
		return int64(runes / 4)
	}
	// 混合/非 ASCII：CJK 常见约 1~2 字符/token，取 1.5。
	return int64(runes / 1.5)
}

// EstimateMessageTokens 估算单条消息 token：文本部件 + 思考 + 工具调用参数。
func EstimateMessageTokens(m Message) int64 {
	var total int64
	for _, p := range m.Parts {
		switch part := p.(type) {
		case TextPart:
			total += EstimateTokens(part.Text)
		case ThinkPart:
			total += EstimateTokens(part.Text)
		case ImagePart:
			// 图片按中等分辨率经验值计。
			total += 800
		}
	}
	for _, tc := range m.ToolCalls {
		total += EstimateTokens(string(tc.Arguments)) + 8
	}
	if m.ToolCallID != "" {
		total += 8
	}
	return total
}

// EstimateHistoryTokens 估算整段历史的 token 预算（不含 system prompt）。
func EstimateHistoryTokens(history []Message) int64 {
	var total int64
	for _, m := range history {
		total += EstimateMessageTokens(m)
	}
	return total
}

// NormalizeModelFamily 从模型名提取家族前缀，用于能力表兜底匹配。
func NormalizeModelFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, sep := range []string{"@", ":", "/"} {
		if idx := strings.Index(m, sep); idx > 0 {
			m = m[:idx]
		}
	}
	return m
}
