// Package agentloop 是自研 Agent 引擎的无状态循环（对标 Kimi Code 的 loop）。
//
// 边界契约（设计文档 §5，翻译自 loop/README.md 的纪律）：
//   - 本包不 import 任何 UI/存储/权限实现；依赖通过接口注入。
//   - 事件单出口：Dispatcher。录制与直播都在出口处分发，订阅者故障不影响 loop。
//   - usage 在 LLM 返回后立即记录（不等工具执行完成）。
//   - 每个 tool.call 必须有配对的 tool.result，除非 step 在派发点前被中断。
package agentloop

import (
	"encoding/json"

	"grok_switch/internal/llm"
)

// StepStopReason 是单个模型步的结束原因。"tool_use" 是 loop 控制信号：
// 引擎执行工具后继续下一步；其余为当前 turn 的终止原因。
type StepStopReason string

const (
	StopEndTurn  StepStopReason = "end_turn"
	StopMaxToken StepStopReason = "max_tokens"
	StopToolUse  StepStopReason = "tool_use"
	StopFiltered StepStopReason = "filtered"
	StopUnknown  StepStopReason = "unknown"
)

// TurnStopReason 是整个 turn 的结束原因（不含 tool_use——它不可能是终态）。
type TurnStopReason string

const (
	TurnEnd       TurnStopReason = "end_turn"
	TurnMaxToken  TurnStopReason = "max_tokens"
	TurnFiltered  TurnStopReason = "filtered"
	TurnAborted   TurnStopReason = "aborted"
	TurnError     TurnStopReason = "error"
	TurnMaxSteps  TurnStopReason = "max_steps"
	TurnUserAbort TurnStopReason = "user_cancelled"
)

// ToolResult 是工具执行的统一结果。
type ToolResult struct {
	// Output 是回给模型的文本（已截断预算内）。
	Output string
	// IsError 为 true 时模型看到的是失败结果。
	IsError bool
	// Truncated 标记输出因预算被截断（后续通用预算不再重复截断）。
	Truncated bool
	// Media 是可选的图片/资源结果（如 generate_image 的产物）。
	Media []llm.ContentPart
}

// ToolResultJSON 把结果序列化为回传模型的 JSON 文本。
func (r ToolResult) ResultJSON() string {
	payload := map[string]any{
		"output": r.Output,
		"error":  r.IsError,
	}
	if r.Truncated {
		payload["truncated"] = true
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return r.Output
	}
	return string(b)
}

// TurnResult 是一次 turn 的最终结果。
type TurnResult struct {
	StopReason TurnStopReason
	Steps      int
	Usage      llm.TokenUsage
	// Error 携带 TurnError 时的底层错误。
	Error error
}

// MaxStepsExceededError 表示超过单 turn 步数上限。
type MaxStepsExceededError struct{ Max int }

func (e *MaxStepsExceededError) Error() string {
	return "agentloop: 达到单轮步数上限"
}
