package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"grok_switch/internal/llm"
)

// --- 测试基建：假 Provider / 工具 / Memory / 权限闸 ---

// scriptProvider 按 prelude 脚本逐个返回预设响应（脚本耗尽后重复最后一个）。
type scriptProvider struct {
	mu       sync.Mutex
	steps    []llm.StreamResult
	idx      int
	failures []error // 每次调用前先弹出的错误（nil 表示成功）
	calls    int
}

func (p *scriptProvider) Name() string      { return "script" }
func (p *scriptProvider) ModelName() string { return "test-model" }
func (p *scriptProvider) Capability() llm.ModelCapability {
	return llm.ModelCapability{ToolUse: true, Thinking: true, MaxContextTokens: 100000, UsageAccuracy: llm.UsageExact}
}

func (p *scriptProvider) Generate(ctx context.Context, systemPrompt string, tools []llm.Tool, history []llm.Message, opts llm.GenerateOptions) (*llm.StreamResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.failures) > 0 {
		err := p.failures[0]
		p.failures = p.failures[1:]
		if err != nil {
			return nil, err
		}
	}
	if p.idx >= len(p.steps) {
		p.idx = len(p.steps) - 1
	}
	res := p.steps[p.idx]
	p.idx++
	if opts.OnDelta != nil && res.Message.Text() != "" {
		opts.OnDelta(llm.TextDelta{Text: res.Message.Text()})
	}
	return &res, nil
}

func (p *scriptProvider) WithThinking(effort string) llm.Provider { return p }

func (p *scriptProvider) WithMaxOutputTokens(n int64) llm.Provider { return p }

func textResult(text string) llm.StreamResult {
	return llm.StreamResult{
		Message:      llm.Message{Role: llm.RoleAssistant, Parts: []llm.ContentPart{llm.TextPart{Text: text}}},
		Usage:        llm.TokenUsage{InputOther: 100, Output: 10},
		Accuracy:     llm.UsageExact,
		FinishReason: "stop",
	}
}

func toolCallResult(id, name, args string) llm.StreamResult {
	return llm.StreamResult{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}},
		},
		Usage:        llm.TokenUsage{InputOther: 120, Output: 15},
		Accuracy:     llm.UsageExact,
		FinishReason: "tool_use",
	}
}

// fakeTools 是确定性工具执行器：read 返回文件内容，fail 返回错误。
type fakeTools struct {
	mu          sync.Mutex
	calls       []llm.ToolCall
	results     map[string]ToolResult
	executeHook func() // 每次执行后回调（测试取消用）
}

func newFakeTools() *fakeTools {
	return &fakeTools{results: map[string]ToolResult{}}
}

func (t *fakeTools) set(name, output string, isError bool) {
	t.results[name] = ToolResult{Output: output, IsError: isError}
}

func (t *fakeTools) Schemas() []llm.Tool {
	return []llm.Tool{
		{Name: "read", Description: "读文件", Schema: map[string]any{"type": "object"}},
		{Name: "bash", Description: "跑命令", Schema: map[string]any{"type": "object"}},
	}
}

func (t *fakeTools) Execute(ctx context.Context, call llm.ToolCall) (ToolResult, error) {
	t.mu.Lock()
	t.calls = append(t.calls, call)
	res, ok := t.results[call.Name]
	hook := t.executeHook
	t.mu.Unlock()
	if hook != nil {
		hook()
	}
	if !ok {
		return ToolResult{Output: "ok (default)"}, nil
	}
	return res, nil
}

// sliceMemory 是内存历史。
type sliceMemory struct {
	msgs []llm.Message
}

func (m *sliceMemory) History() []llm.Message { return m.msgs }

func (m *sliceMemory) Append(msg llm.Message) { m.msgs = append(m.msgs, msg) }

// gatePerm 权限闸：askTools 中的工具需要审批。
type gatePerm struct {
	askTools map[string]bool
	decide   func(call llm.ToolCall) PermResult
}

func (g *gatePerm) Check(call llm.ToolCall) Decision {
	if g.askTools[call.Name] {
		return DecAsk
	}
	return DecAllow
}

func (g *gatePerm) WaitForDecision(ctx context.Context, call llm.ToolCall) PermResult {
	return g.decide(call)
}

// --- 测试 ---

func TestRunTurnSimpleText(t *testing.T) {
	prov := &scriptProvider{steps: []llm.StreamResult{textResult("你好，我是引擎")}}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "hi"}}}}}
	rec := &MemoryRecorder{}

	res, err := RunTurn(context.Background(), RunTurnInput{
		TurnID: "t1", Provider: prov, SystemPrompt: "sys", Memory: mem, Events: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != TurnEnd || res.Steps != 1 {
		t.Fatalf("结果错误: %+v", res)
	}
	if got := mem.msgs[len(mem.msgs)-1].Text(); got != "你好，我是引擎" {
		t.Fatalf("assistant 回复未入历史: %q", got)
	}
	if res.Usage.Output != 10 {
		t.Fatalf("usage 聚合错误: %+v", res.Usage)
	}
	// 事件序列：turn_begin → step_begin → text_delta → step_end → usage → turn_end
	types := eventTypeList(rec)
	expect := []EventType{EventTurnBegin, EventStepBegin, EventTextDelta, EventStepEnd, EventUsage, EventTurnEnd}
	if fmt.Sprint(types) != fmt.Sprint(expect) {
		t.Fatalf("事件序列错误:\n got %v\nwant %v", types, expect)
	}
}

func TestRunTurnToolLoop(t *testing.T) {
	prov := &scriptProvider{steps: []llm.StreamResult{
		toolCallResult("c1", "read", `{"path":"a.go"}`),
		textResult("读完了"),
	}}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "读 a.go"}}}}}
	tools := newFakeTools()
	tools.set("read", "package main", false)
	rec := &MemoryRecorder{}

	res, err := RunTurn(context.Background(), RunTurnInput{
		TurnID: "t2", Provider: prov, SystemPrompt: "sys", Memory: mem, Tools: tools, Events: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != TurnEnd || res.Steps != 2 {
		t.Fatalf("结果错误: %+v", res)
	}
	if len(tools.calls) != 1 || tools.calls[0].Name != "read" {
		t.Fatalf("工具执行记录错误: %+v", tools.calls)
	}
	// 历史：user → assistant(tool_call) → tool → assistant(text)
	if len(mem.msgs) != 4 {
		t.Fatalf("历史长度错误: %d", len(mem.msgs))
	}
	toolMsg := mem.msgs[2]
	if toolMsg.Role != llm.RoleTool || toolMsg.ToolCallID != "c1" {
		t.Fatalf("工具结果消息错误: %+v", toolMsg)
	}
	if !contains(toolMsg.Text(), `"package main"`) {
		t.Fatalf("工具结果 JSON 缺内容: %s", toolMsg.Text())
	}
	// tool_call 与 tool_result 必须配对。
	calls := rec.FilterTypes(EventToolCall)
	results := rec.FilterTypes(EventToolResult)
	if len(calls) != 1 || len(results) != 1 {
		t.Fatalf("call/result 配对错误: %d calls, %d results", len(calls), len(results))
	}
	if calls[0].ToolCall.ID != results[0].ToolResult.ID {
		t.Fatalf("call/result ID 不配对")
	}
}

func TestRunTurnToolErrorContinues(t *testing.T) {
	prov := &scriptProvider{steps: []llm.StreamResult{
		toolCallResult("c1", "bash", `{"cmd":"boom"}`),
		textResult("命令失败了，我改用别的方法"),
	}}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "跑 boom"}}}}}
	tools := newFakeTools()
	tools.set("bash", "exit 1", true)

	res, err := RunTurn(context.Background(), RunTurnInput{
		TurnID: "t3", Provider: prov, Memory: mem, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != TurnEnd {
		t.Fatalf("工具失败不应终止 turn: %+v", res)
	}
	// 工具结果应带 error 标记回给模型。
	toolMsg := mem.msgs[2]
	if !contains(toolMsg.Text(), `"error":true`) {
		t.Fatalf("错误标记缺失: %s", toolMsg.Text())
	}
}

func TestRunTurnRateLimitRetry(t *testing.T) {
	prov := &scriptProvider{
		steps: []llm.StreamResult{textResult("重试后成功")},
		failures: []error{
			llm.NewAPIError("rate_limited", 429, "slow down", 10*time.Millisecond),
		},
	}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "hi"}}}}}

	res, err := RunTurn(context.Background(), RunTurnInput{
		TurnID: "t4", Provider: prov, Memory: mem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != TurnEnd {
		t.Fatalf("重试后应成功: %+v", res)
	}
	if prov.calls != 2 {
		t.Fatalf("应调用两次（1 重试）, got %d", prov.calls)
	}
}

func TestRunTurnRetryExhausted(t *testing.T) {
	prov := &scriptProvider{
		steps: []llm.StreamResult{textResult("never")},
		failures: []error{
			llm.NewAPIError("overloaded", 503, "down", 0),
			llm.NewAPIError("overloaded", 503, "down", 0),
			llm.NewAPIError("overloaded", 503, "down", 0),
			llm.NewAPIError("overloaded", 503, "down", 0),
		},
	}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "hi"}}}}}
	rec := &MemoryRecorder{}

	// maxRetry=2 → 最多 3 次调用（初试+2 重试）；提供 4 个错误时第 4 个不会被消费。
	_, err := RunTurn(context.Background(), RunTurnInput{
		TurnID: "t5", Provider: prov, Memory: mem, Events: rec, MaxRetries: 2,
	})
	if err == nil {
		t.Fatal("重试耗尽应返回错误")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应保留 APIError: %v", err)
	}
	if prov.calls != 3 {
		t.Fatalf("调用次数错误: %d", prov.calls)
	}
	interrupted := rec.FilterTypes(EventTurnInterrupt)
	if len(interrupted) != 1 || interrupted[0].StopReason != TurnError {
		t.Fatalf("中断事件错误: %+v", interrupted)
	}
}

func TestRunTurnNonRetryableError(t *testing.T) {
	prov := &scriptProvider{
		steps:    []llm.StreamResult{textResult("never")},
		failures: []error{llm.NewAPIError("auth", 401, "bad key", 0)},
	}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "hi"}}}}}

	_, err := RunTurn(context.Background(), RunTurnInput{TurnID: "t6", Provider: prov, Memory: mem})
	if err == nil {
		t.Fatal("auth 错误应直接失败")
	}
	if prov.calls != 1 {
		t.Fatalf("不可重试错误不应重试: %d", prov.calls)
	}
}

func TestRunTurnUserCancel(t *testing.T) {
	prov := &scriptProvider{steps: []llm.StreamResult{textResult("never")}}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "hi"}}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := RunTurn(ctx, RunTurnInput{TurnID: "t7", Provider: prov, Memory: mem})
	if err != nil {
		t.Fatalf("用户取消不应返回 error: %v", err)
	}
	if res.StopReason != TurnUserAbort {
		t.Fatalf("应标记 user_cancelled: %+v", res)
	}
}

func TestRunTurnMidTurnCancel(t *testing.T) {
	// 第一步工具执行后取消：第二步模型调用前 abort。
	prov := &scriptProvider{steps: []llm.StreamResult{
		toolCallResult("c1", "read", `{}`),
		textResult("never reached"),
	}}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "hi"}}}}}
	tools := newFakeTools()
	ctx, cancel := context.WithCancel(context.Background())
	tools.executeHook = func() { cancel() }

	res, err := RunTurn(ctx, RunTurnInput{TurnID: "t8", Provider: prov, Memory: mem, Tools: tools})
	if err != nil {
		t.Fatalf("取消应优雅返回: %v", err)
	}
	if res.StopReason != TurnUserAbort && res.StopReason != TurnAborted {
		t.Fatalf("停止原因错误: %+v", res)
	}
	// 已取消后剩余工具调用也要有配对 result（若在同批次）。
	calls := len(memHistoryToolCalls(mem))
	results := 0
	for _, m := range mem.msgs {
		if m.Role == llm.RoleTool {
			results++
		}
	}
	if calls != results {
		t.Fatalf("取消时 call/result 失配: %d calls, %d results", calls, results)
	}
}

func TestRunTurnMaxSteps(t *testing.T) {
	// 无限要求工具调用 → 步数上限终止。
	prov := &scriptProvider{steps: []llm.StreamResult{toolCallResult("c", "read", `{}`)}}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "loop"}}}}}
	tools := newFakeTools()

	res, err := RunTurn(context.Background(), RunTurnInput{
		TurnID: "t9", Provider: prov, Memory: mem, Tools: tools, MaxSteps: 5,
	})
	if err == nil {
		t.Fatal("超步数应返回错误")
	}
	var maxErr *MaxStepsExceededError
	if !errors.As(err, &maxErr) {
		t.Fatalf("应为 MaxStepsExceededError: %v", err)
	}
	if res.StopReason != TurnMaxSteps || res.Steps != 5 {
		t.Fatalf("结果错误: %+v", res)
	}
}

func TestRunTurnPermissionAsk(t *testing.T) {
	prov := &scriptProvider{steps: []llm.StreamResult{
		toolCallResult("c1", "bash", `{"cmd":"rm -rf /"}`),
		textResult("好的，已跳过"),
	}}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "rm"}}}}}
	tools := newFakeTools()
	gate := &gatePerm{
		askTools: map[string]bool{"bash": true},
		decide: func(call llm.ToolCall) PermResult {
			return PermResult{Decision: DecDeny, Reason: "危险命令，不允许。"}
		},
	}

	_, err := RunTurn(context.Background(), RunTurnInput{
		TurnID: "t10", Provider: prov, Memory: mem, Tools: tools, PermGate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 拒绝应回给模型而不是执行。
	if len(tools.calls) != 0 {
		t.Fatalf("被拒工具不应执行: %+v", tools.calls)
	}
	toolMsg := mem.msgs[2]
	if !contains(toolMsg.Text(), "危险命令") || !contains(toolMsg.Text(), `"error":true`) {
		t.Fatalf("拒绝原因未回传模型: %s", toolMsg.Text())
	}
}

func TestRunTurnToolCallEventOrder(t *testing.T) {
	prov := &scriptProvider{steps: []llm.StreamResult{
		toolCallResult("c1", "read", `{"path":"x"}`),
		textResult("done"),
	}}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "x"}}}}}
	rec := &MemoryRecorder{}

	_, err := RunTurn(context.Background(), RunTurnInput{
		TurnID: "t11", Provider: prov, Memory: mem, Tools: newFakeTools(), Events: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	// tool_call 事件必须先于对应 tool_result。
	var sawCall bool
	for _, ev := range rec.Snapshot() {
		if ev.Type == EventToolCall {
			sawCall = true
		}
		if ev.Type == EventToolResult && !sawCall {
			t.Fatal("tool_result 先于 tool_call")
		}
	}
}

// --- 辅助 ---

func eventTypeList(rec *MemoryRecorder) []EventType {
	var out []EventType
	for _, ev := range rec.Snapshot() {
		out = append(out, ev.Type)
	}
	return out
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func memHistoryToolCalls(mem *sliceMemory) []llm.ToolCall {
	var out []llm.ToolCall
	for _, m := range mem.msgs {
		out = append(out, m.ToolCalls...)
	}
	return out
}
