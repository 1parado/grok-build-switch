package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// capabilityFor 依据 Profile 数据（ModelDef 字段的镜像）构造能力表。
// server 层解析 Profile 时调用；引擎不 import profiles 包。
// 参数语义与 profiles.ModelDef 一致；windows 为 0 表示未知。
func capabilityFor(model string, supportsReasoningEffort bool, efforts []string, contextWindow, maxCompletion int64) ModelCapability {
	cap := ModelCapability{
		ToolUse:             true,
		MaxContextTokens:    int(contextWindow),
		MaxCompletionTokens: int(maxCompletion),
		UsageAccuracy:       UsageExact,
	}
	if supportsReasoningEffort {
		cap.Thinking = true
		cap.ReasoningEfforts = efforts
	}
	if isVisionModelFamily(model) {
		cap.ImageIn = true
	}
	return cap
}

// isVisionModelFamily 判断模型家族是否已知接受图片输入。
// Grok 4.x 全系多模态；未知家族保守返回 false。
func isVisionModelFamily(model string) bool {
	family := NormalizeModelFamily(model)
	return strings.HasPrefix(family, "grok-") || strings.HasPrefix(family, "grok ")
}

// validateEffort 把请求里的推理强度归一到模型支持列表；不支持时返回空串。
func validateEffort(effort string, cap ModelCapability) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" || !cap.Thinking {
		return ""
	}
	if len(cap.ReasoningEfforts) == 0 {
		return e
	}
	for _, allowed := range cap.ReasoningEfforts {
		if strings.EqualFold(allowed, e) {
			return strings.ToLower(allowed)
		}
	}
	return ""
}

// compactArgsForLog 截断工具参数用于日志。
func compactArgsForLog(args json.RawMessage) string {
	s := string(args)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// errToolUseUnsupported 构造模型不支持工具调用的明确错误。
func errToolUseUnsupported(model string) error {
	return fmt.Errorf("llm: 模型 %q 未声明 tool_use 能力，无法执行带工具的对话", model)
}
