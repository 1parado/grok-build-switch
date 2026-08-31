package agentloop

import (
	"context"

	"grok_switch/internal/llm"
)

// ToolExecutor 是工具执行器接口：工具表持有者 + 执行入口。
// internal/tools 的注册表实现它；测试用假工具实现它。
type ToolExecutor interface {
	// Execute 执行一次工具调用。返回结果或错误；错误会被包装成 IsError
	// 的 ToolResult 回给模型（工具失败不终止 turn）。
	Execute(ctx context.Context, call llm.ToolCall) (ToolResult, error)
	// Schemas 返回当前步可用的工具定义（每步重查，支持运行时增减）。
	Schemas() []llm.Tool
}

// Decision 是权限决定。
type Decision string

const (
	DecAllow Decision = "allow"
	DecDeny  Decision = "deny"
	DecAsk   Decision = "ask"
)

// PermResult 是 WaitForDecision 的结果。
type PermResult struct {
	Decision Decision
	// Reason 拒绝时回给模型的原因（用户反馈）。
	Reason string
}

// PermissionGate 是权限闸。Check 在每次工具执行前调用：
//   - allow：直接执行；
//   - deny：返回拒绝结果给模型（不执行）；
//   - ask：调用 WaitForDecision 挂起，等待宿主（server 层）回填决定。
type PermissionGate interface {
	Check(call llm.ToolCall) Decision
	// WaitForDecision 挂起等待人工审批。ctx 被取消（用户取消 turn）时
	// 返回 deny 决定。宿主通过返回值回填决定。
	WaitForDecision(ctx context.Context, call llm.ToolCall) PermResult
}

// Hooks 是宿主层挂点（对齐 Kimi LoopHooks 的最小集）。
type Hooks struct {
	// BeforeStep 在每步消息构建前调用（compaction 的切入点）。
	// 返回错误会终止 turn。
	BeforeStep func(ctx context.Context, step int) error
	// ShouldContinueAfterStop 在模型给出非 tool_use 终止后询问是否续跑
	//（goal mode / 用户追加指令的场景）。默认不续。
	ShouldContinueAfterStop func(ctx context.Context, in ContinueCheck) bool
}

// ContinueCheck 是 ShouldContinueAfterStop 的入参。
type ContinueCheck struct {
	Step       int
	StopReason StepStopReason
	Usage      llm.TokenUsage
}

// RunTurnInput 是 RunTurn 的全部依赖。
type RunTurnInput struct {
	TurnID string
	// Provider 是模型后端（已由 server 层按 Profile 构造）。
	Provider llm.Provider
	// SystemPrompt 系统提示词。
	SystemPrompt string
	// Memory 是上下文记忆持有者（引擎自持历史，D1b）。
	Memory Memory
	// Tools 工具执行器；nil 表示纯对话。
	Tools ToolExecutor
	// PermGate 权限闸；nil 表示全放行（仅测试用）。
	PermGate PermissionGate
	// Events 事件出口；nil 丢弃全部事件。
	Events Dispatcher
	// MaxSteps 单 turn 步数上限；0 用默认 100。
	MaxSteps int
	// MaxRetries 可重试错误的最大尝试次数；0 用默认 3。
	MaxRetries int
	// Hooks 可选挂点。
	Hooks Hooks
	// Effort 覆盖推理强度；空用 Provider 当前值。
	Effort string
}

// Memory 是上下文记忆接口：引擎通过它读写历史。internal/ctxmem 实现它，
// 测试用内存切片实现。
type Memory interface {
	// History 返回当前 model-visible 全量历史（每步重建，D1b）。
	History() []llm.Message
	// Append 追加一条消息（user 输入 / assistant 回复 / 工具结果）。
	Append(msg llm.Message)
}
