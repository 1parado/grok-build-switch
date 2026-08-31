package tools

import (
	"context"
	"strings"

	"grok_switch/internal/agentfs"
	"grok_switch/internal/agentloop"
	"grok_switch/internal/llm"
)

// ToolsAdapter 把 *Registry 适配成 agentloop.ToolExecutor。
// tools → agentloop 单向依赖（agentloop 不 import tools），无环。
type ToolsAdapter struct {
	Registry *Registry
}

// NewToolsAdapter 构造适配器。
func NewToolsAdapter(reg *Registry) *ToolsAdapter {
	return &ToolsAdapter{Registry: reg}
}

// Schemas 实现 agentloop.ToolExecutor。
func (a *ToolsAdapter) Schemas() []llm.Tool { return a.Registry.Schemas() }

// Execute 实现 agentloop.ToolExecutor：工具失败转 IsError 结果而非 Go 错误
// （工具失败不终止 turn，模型看到错误后自行调整）。
func (a *ToolsAdapter) Execute(ctx context.Context, call llm.ToolCall) (agentloop.ToolResult, error) {
	out := a.Registry.ExecuteTool(ctx, call)
	return agentloop.ToolResult{
		Output:    out.Text,
		IsError:   out.IsError,
		Truncated: out.Truncated,
		Media:     out.Media,
	}, nil
}

// EnvFor 返回给定工作目录的沙箱 Env（server 层构造 Registry 时使用）。
func EnvFor(cwd string) agentfs.Env {
	return agentfs.Env{Cwd: cwd}
}

// TruncateForDisplay 截断文本用于 UI 预览。
func TruncateForDisplay(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}
