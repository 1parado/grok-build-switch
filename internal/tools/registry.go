// Package tools 是自研引擎的内置工具集（对标 Kimi builtin tools 的第一期清单）。
//
// 每个工具实现 agentloop.ToolExecutor 需要的接口形态：名字、JSON Schema、
// 文档（拼进 system prompt）、Execute。文件/进程触碰一律经 internal/agentfs
// 的沙箱 Env；结果统一做预算截断。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"grok_switch/internal/agentfs"
	"grok_switch/internal/llm"
)

// ResultBudget 是单个工具结果回传模型的字符预算（超出截断）。
const ResultBudget = 30000

// Registry 是工具注册表，实现 agentloop.ToolExecutor。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// ExtraHeaders 允许 server 层注入请求头（生图引擎等）。
	getEnv func() agentfs.Env
	// callLimits 单轮（本注册表实例生命周期内）各工具最大调用次数；
	// 0/缺省表示不限。注册表按 turn 重建，计数天然按 turn 清零。
	callLimits map[string]int
	callCounts map[string]int
}

// Tool 是单个工具的接口。
type Tool interface {
	Name() string
	Schema() map[string]any
	// Doc 是拼进 system prompt 的用法说明（Kimi 的 *.md 内嵌模式）。
	Doc() string
	Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput
}

// ToolOutput 是工具输出（agentloop.ToolResult 的工具层形态）。
type ToolOutput struct {
	Text      string
	IsError   bool
	Truncated bool
	Media     []llm.ContentPart
}

// NewRegistry 构造注册表。getEnv 提供当前会话的沙箱环境（工作目录可变）。
func NewRegistry(getEnv func() agentfs.Env) *Registry {
	return &Registry{tools: map[string]Tool{}, getEnv: getEnv, callLimits: map[string]int{}, callCounts: map[string]int{}}
}

// SetCallLimit 设置某工具在本轮内的最大调用次数（防模型循环调用烧额度，
// 如 generate_image 连调 28 次）。超限后不再执行，直接回 IsError 结果让模型收敛。
func (r *Registry) SetCallLimit(name string, max int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.callLimits == nil {
		r.callLimits = map[string]int{}
	}
	r.callLimits[name] = max
}

// Register 注册工具；同名覆盖（便于测试）。
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Unregister 移除工具（生图开关即时生效的通道）。
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// Names 返回已注册工具名（排序）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Schemas 实现 agentloop.ToolExecutor。
func (r *Registry) Schemas() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]llm.Tool, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		schema := t.Schema()
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, llm.Tool{Name: t.Name(), Description: t.Doc(), Schema: schema})
	}
	return out
}

// Execute 实现 agentloop.ToolExecutor：参数解析 + 沙箱 + 预算截断。
// 返回值通过 ToAgentloopResult 适配 agentloop.ToolExecutor 的签名（见 adapter.go）。
func (r *Registry) ExecuteTool(ctx context.Context, call llm.ToolCall) ToolOutput {
	r.mu.Lock()
	t, ok := r.tools[call.Name]
	limit := r.callLimits[call.Name]
	if ok && limit > 0 {
		if r.callCounts == nil {
			r.callCounts = map[string]int{}
		}
		r.callCounts[call.Name]++
		if r.callCounts[call.Name] > limit {
			r.mu.Unlock()
			return ToolOutput{Text: fmt.Sprintf("本轮 %s 已调用 %d 次达到上限（不再执行，不消耗额度）。请先向用户展示已有结果；如需更多，请向用户确认后再分轮生成。", call.Name, limit), IsError: true}
		}
	}
	r.mu.Unlock()
	if !ok {
		return ToolOutput{Text: fmt.Sprintf("未知工具 %q", call.Name), IsError: true}
	}
	env := agentfs.Env{}
	if r.getEnv != nil {
		env = r.getEnv()
	}
	out := t.Execute(ctx, call.Arguments, env)
	if out.Text == "" && !out.IsError && len(out.Media) == 0 {
		out.Text = "(无输出)"
	}
	if len(out.Text) > ResultBudget {
		out.Text = out.Text[:ResultBudget] + "\n\n[输出超过预算被截断]"
		out.Truncated = true
	}
	return out
}

// argHelp 构造参数解析错误信息（带期望字段，帮模型自纠）。
func argHelp(tool string, err error, expected string) ToolOutput {
	return ToolOutput{
		Text:    fmt.Sprintf("参数解析失败: %v。期望 JSON 对象字段: %s", err, expected),
		IsError: true,
	}
}
