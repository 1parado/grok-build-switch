package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"grok_switch/internal/agentbridge"
	"grok_switch/internal/agentkit"
	"grok_switch/internal/llm"
	"grok_switch/internal/permission"
	"grok_switch/internal/profiles"
	"grok_switch/internal/settings"
)

// --- 测试基建：可控 Provider 注入 nativeAgentService ---

type stubProvider struct {
	mu    sync.Mutex
	steps []llm.StreamResult
	idx   int
	calls int
}

func (p *stubProvider) Name() string      { return "stub" }
func (p *stubProvider) ModelName() string { return "stub-model" }
func (p *stubProvider) Capability() llm.ModelCapability {
	return llm.ModelCapability{ToolUse: true, MaxContextTokens: 100000, UsageAccuracy: llm.UsageExact}
}

func (p *stubProvider) Generate(ctx context.Context, systemPrompt string, tools []llm.Tool, history []llm.Message, opts llm.GenerateOptions) (*llm.StreamResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
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

func (p *stubProvider) WithThinking(effort string) llm.Provider  { return p }
func (p *stubProvider) WithMaxOutputTokens(n int64) llm.Provider { return p }

func newTestNativeService(t *testing.T, prov llm.Provider) *nativeAgentService {
	t.Helper()
	root := t.TempDir()
	svc, err := newNativeAgentService(EngineDeps{
		SessionsRoot: root,
		ProviderFor:  func() (llm.Provider, error) { return prov, nil },
		SystemPrompt: func(toolDoc string) string { return "sys+" + toolDoc },
		DefaultCwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func textStep(text string) llm.StreamResult {
	return llm.StreamResult{
		Message:      llm.Message{Role: llm.RoleAssistant, Parts: []llm.ContentPart{llm.TextPart{Text: text}}},
		Usage:        llm.TokenUsage{InputOther: 50, Output: 10},
		Accuracy:     llm.UsageExact,
		FinishReason: "stop",
	}
}

func toolStep(id, name, args string) llm.StreamResult {
	return llm.StreamResult{
		Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}}},
		Usage:        llm.TokenUsage{InputOther: 60, Output: 12},
		Accuracy:     llm.UsageExact,
		FinishReason: "tool_use",
	}
}

func waitTurnDone(t *testing.T, sub *agentBridgeSub, timeout time.Duration) agentbridge.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-sub.events:
			if !ok {
				t.Fatal("订阅关闭")
			}
			if ev.Type == "turn_done" || ev.Type == "error" {
				return ev
			}
		case <-deadline:
			t.Fatal("等待 turn 结束超时")
		}
	}
}

// agentBridgeSub 收集事件的订阅封装。
type agentBridgeSub struct {
	id     string
	events <-chan agentbridge.Event
}

func subscribeNative(t *testing.T, svc *nativeAgentService) *agentBridgeSub {
	t.Helper()
	id, ch := svc.Subscribe()
	return &agentBridgeSub{id: id, events: ch}
}

// --- 用例 ---

func TestNativeServiceStatusAndStart(t *testing.T) {
	svc := newTestNativeService(t, &stubProvider{})
	st := svc.Status()
	if !st.Available || st.Running {
		t.Fatalf("初始状态错误: %+v", st)
	}
	if err := svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	st = svc.Status()
	if !st.Running || st.State != "ready" || st.SessionID == "" {
		t.Fatalf("启动后状态错误: %+v", st)
	}
	if err := svc.Stop(); err != nil {
		t.Fatal(err)
	}
	if svc.Status().Running {
		t.Fatal("停止后 Running 应为 false")
	}
}

func TestNativeServicePromptTextTurn(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{textStep("自研引擎回复")}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)

	if err := svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	<-sub.events // agent_status(ready)

	if err := svc.Prompt("你好", nil); err != nil {
		t.Fatal(err)
	}
	final := waitTurnDone(t, sub, 10*time.Second)
	if final.Type != "turn_done" || final.StopReason != "end_turn" {
		t.Fatalf("turn 结束错误: %+v", final)
	}

	// 忙碌保护：turn 已结束后可以再次 Prompt。
	if err := svc.Prompt("第二条", nil); err != nil {
		t.Fatalf("turn 结束后应可继续: %v", err)
	}
	waitTurnDone(t, sub, 10*time.Second)
}

func TestNativeServiceEventsSequence(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{textStep("hello")}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events

	_ = svc.Prompt("hi", nil)
	var types []string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-sub.events:
			types = append(types, ev.Type)
			if ev.Type == "turn_done" {
				goto done
			}
		case <-deadline:
			t.Fatal("超时")
		}
	}
done:
	// 事件序列：assistant_chunk → turn_done（agent_status 收尾）。
	joined := joinStrings(types)
	if !containsStr(joined, "assistant_chunk") || !containsStr(joined, "turn_done") {
		t.Fatalf("事件缺失: %v", types)
	}
}

func TestNativeServiceToolTurnAndPermission(t *testing.T) {
	// write 工具需要审批：默认 manual 模式 → permission_request。
	prov := &stubProvider{steps: []llm.StreamResult{
		toolStep("c1", "write", `{"path":"out.txt","content":"引擎写的"}`),
		textStep("写完了"),
	}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	cwd := t.TempDir()
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: cwd})
	<-sub.events

	_ = svc.Prompt("写个文件", nil)

	// 等待 permission_request。
	var permEvt agentbridge.Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-sub.events:
			if ev.Type == "permission_request" {
				permEvt = ev
				goto gotPerm
			}
		case <-deadline:
			t.Fatal("未收到审批请求")
		}
	}
gotPerm:
	if permEvt.Permission == nil || permEvt.Permission.RequestID == "" {
		t.Fatalf("审批事件不完整: %+v", permEvt)
	}
	if len(permEvt.Permission.Options) != 3 {
		t.Fatalf("审批选项错误: %+v", permEvt.Permission.Options)
	}
	// 批准。
	if err := svc.RespondPermissionEx(permEvt.Permission.RequestID, true, false); err != nil {
		t.Fatal(err)
	}
	final := waitTurnDone(t, sub, 10*time.Second)
	if final.Type != "turn_done" {
		t.Fatalf("批准后应完成: %+v", final)
	}
	// 文件确实被写。
	data, err := os.ReadFile(filepath.Join(cwd, "out.txt"))
	if err != nil || string(data) != "引擎写的" {
		t.Fatalf("工具未生效: %v %q", err, data)
	}
}

func TestNativeServiceSessionHistory(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{textStep("第一轮回复")}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events
	_ = svc.Prompt("问题一", nil)
	waitTurnDone(t, sub, 10*time.Second)
	_ = svc.Prompt("问题二", nil)
	waitTurnDone(t, sub, 10*time.Second)

	sessionID := svc.Status().SessionID
	sessions, err := svc.ListStoredSessions("", 100)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("会话列表错误: %v %d", err, len(sessions))
	}
	history, err := svc.StoredSessionHistory(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Messages) < 4 {
		t.Fatalf("历史消息不足: %d", len(history.Messages))
	}
	if history.Messages[0].Role != "user" || history.Messages[0].Content != "问题一" {
		t.Fatalf("首条消息错误: %+v", history.Messages[0])
	}
	found := false
	for _, m := range history.Messages {
		if m.Role == "assistant" && containsStr(m.Content, "第一轮回复") {
			found = true
		}
	}
	if !found {
		t.Fatal("assistant 回复未入历史")
	}
	// 重命名与删除。
	if err := svc.RenameStoredSession(sessionID, "测试标题"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteStoredSession(sessionID); err != nil {
		t.Fatal(err)
	}
	sessions, _ = svc.ListStoredSessions("", 100)
	if len(sessions) != 0 {
		t.Fatal("删除后列表应为空")
	}
}

func TestNativeServiceRewind(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{textStep("回复")}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events
	_ = svc.Prompt("q1", nil)
	waitTurnDone(t, sub, 10*time.Second)
	_ = svc.Prompt("q2", nil)
	waitTurnDone(t, sub, 10*time.Second)

	res := svc.RewindDropLastUser(t.Context(), false)
	if !res.OK || res.UserTurnCount != 1 {
		t.Fatalf("rewind 错误: %+v", res)
	}
	if svc.memory.UserTurns() != 1 {
		t.Fatalf("内存轮次错误: %d", svc.memory.UserTurns())
	}
}

func TestNativeServiceSystemPromptIncludesToolDoc(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{textStep("ok")}}
	svc := newTestNativeService(t, prov)
	var captured string
	svc.deps.SystemPrompt = func(toolDoc string) string {
		captured = toolDoc
		return "sys"
	}
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events
	_ = svc.Prompt("hi", nil)
	waitTurnDone(t, sub, 10*time.Second)
	if !containsStr(captured, "read") || !containsStr(captured, "bash") {
		t.Fatalf("工具文档未注入 system prompt: %q", captured[:min(200, len(captured))])
	}
}

func TestProfileToBridgePortRewrite(t *testing.T) {
	p := profiles.Profile{
		ID:             "p1",
		BaseURL:        "http://127.0.0.1:17878/grok/v1",
		APIKey:         "gsk-test",
		UpstreamFormat: "openai_responses",
		DefaultModel:   "grok-4.6",
		Models: []profiles.ModelDef{
			{Model: "grok-4.6", BaseURL: "http://127.0.0.1:17878/grok/v1", ContextWindow: 500000, MaxCompletionTokens: 65536, SupportsReasoningEffort: true},
		},
	}
	bridge := profileToBridge(p, 19999)
	if !containsStr(bridge.BaseURL, "127.0.0.1:19999") {
		t.Fatalf("端口未重写: %s", bridge.BaseURL)
	}
	if bridge.Model != "grok-4.6" || bridge.ContextWindow != 500000 || !bridge.SupportsReasoningEffort {
		t.Fatalf("模型字段投影错误: %+v", bridge)
	}
	if bridge.SessionKey != "p1" {
		t.Fatalf("sessionKey 应为 Profile ID: %q", bridge.SessionKey)
	}
}

func TestAgentEngineSettingNormalization(t *testing.T) {
	cases := map[string]string{
		"":        settings.AgentEngineACP,
		"acp":     settings.AgentEngineACP,
		"native":  settings.AgentEngineNative,
		"garbage": settings.AgentEngineACP,
	}
	for in, want := range cases {
		s := settings.Settings{AgentEngine: in}
		if got := s.NormalizedAgentEngine(); got != want {
			t.Fatalf("NormalizedAgentEngine(%q)=%q, want %q", in, got, want)
		}
	}
}

// --- 小工具 ---

func joinStrings(items []string) string {
	out := ""
	for _, s := range items {
		out += s + ","
	}
	return out
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestNativeServiceDenyRuleBlocksTool(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{
		toolStep("c1", "bash", `{"command":"rm -rf /tmp/x"}`),
		textStep("已跳过危险命令"),
	}}
	svc := newTestNativeService(t, prov)
	if err := svc.perm.AddUserRule("bash(rm *)", permission.Deny); err != nil {
		t.Fatal(err)
	}
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events
	_ = svc.Prompt("清理", nil)
	final := waitTurnDone(t, sub, 10*time.Second)
	if final.Type != "turn_done" {
		t.Fatalf("deny 不应终止 turn: %+v", final)
	}
	// 拒绝原因来自规则（DenyReason 可观测）。
	if reason := svc.perm.DenyReason("bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`)); reason == "" {
		t.Fatal("deny reason 不应为空")
	}
}

func TestNativeServiceAllowAlwaysSedimentsRule(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{
		toolStep("c1", "bash", `{"command":"npm test"}`),
		textStep("完成"),
		toolStep("c2", "bash", `{"command":"npm run build"}`),
		textStep("again"),
	}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events
	_ = svc.Prompt("跑测试", nil)

	// 等审批 → allow_always。
	deadline := time.After(10 * time.Second)
	var permEvt agentbridge.Event
	for {
		select {
		case ev := <-sub.events:
			if ev.Type == "permission_request" {
				permEvt = ev
				goto got
			}
		case <-deadline:
			t.Fatal("未收到审批")
		}
	}
got:
	if err := svc.RespondPermissionOption(permEvt.Permission.RequestID, "allow_always", true); err != nil {
		t.Fatal(err)
	}
	waitTurnDone(t, sub, 10*time.Second)

	// 规则沉淀：bash(*) 会话规则存在，第二个 bash 调用不再弹审批。
	sessionRules, _ := svc.perm.Rules()
	if len(sessionRules) != 1 || sessionRules[0] != "bash(*)" {
		t.Fatalf("allow_always 应沉淀工具级会话规则: %v", sessionRules)
	}
	_ = svc.Prompt("再跑构建", nil)
	deadline = time.After(10 * time.Second)
	for {
		select {
		case ev := <-sub.events:
			if ev.Type == "permission_request" {
				t.Fatal("规则命中后不应再弹审批")
			}
			if ev.Type == "turn_done" {
				return
			}
		case <-deadline:
			t.Fatal("等待第二轮结束超时")
		}
	}
}

func TestNativeServiceAutoCompaction(t *testing.T) {
	// 场景：provider 回报 input tokens 超过 85% 窗口 → 第二个 turn 的
	// BeforeStep 触发压实，历史被折叠成摘要。
	big := strings.Repeat("历史内容", 3000) // ~12000 token
	heavy := textStep("第一轮回复")
	heavy.Usage = llm.TokenUsage{InputOther: 90000} // 触发 85% 阈值
	prov := &stubProvider{steps: []llm.StreamResult{
		heavy,             // turn 1（usage 超阈值）
		textStep("第二轮回复"), // turn 2 回复
		textStep(big),     // 摘要调用（compactViaLLM）
	}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events

	// turn 1：usage 巨大，触发阈值。
	_ = svc.Prompt("第一问", nil)
	waitTurnDone(t, sub, 10*time.Second)
	// turn 2：BeforeStep 命中压实 → 摘要调用（第 3 步脚本）→ 正常回复。
	_ = svc.Prompt("第二问", nil)
	final := waitTurnDone(t, sub, 10*time.Second)
	if final.Type != "turn_done" {
		t.Fatalf("压实不应破坏 turn: %+v", final)
	}
	// 压实后历史应显著小于压实体积：assistant/tool 全部折叠。
	msgs := svc.memory.History()
	if len(msgs) >= 8 {
		t.Fatalf("压实后历史应被折叠, got %d 条", len(msgs))
	}
	foundSummary := false
	for _, m := range msgs {
		if strings.HasPrefix(m.Text(), agentkit.COMPACTION_SUMMARY_PREFIX) {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatal("压实摘要缺失")
	}
	// transcript 里有压实记录。
	records, _ := svc.st.LoadRecords(svc.Status().SessionID)
	compacted := false
	for _, r := range records {
		if strings.Contains(r.Text, "[上下文已压实") {
			compacted = true
		}
	}
	if !compacted {
		t.Fatal("压实事件未落盘")
	}
}

func TestNativeServiceRejectsMissingCwd(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{textStep("never")}}
	svc := newTestNativeService(t, prov)
	dead := filepath.Join(t.TempDir(), "no-such-dir")
	err := svc.Start(t.Context(), agentbridge.StartOptions{Cwd: dead})
	if err == nil || !strings.Contains(err.Error(), "工作目录不存在") {
		t.Fatalf("死路径应报错: %v", err)
	}
	// NewSession 同样拒绝。
	if err := svc.NewSession(t.Context(), dead); err == nil {
		t.Fatal("NewSession 应拒绝死路径")
	}
	// 死路径不得被记住（状态里没有 cwd 残留）。
	if svc.Status().Cwd == dead {
		t.Fatal("失效路径不应写入状态")
	}
}

// 回归：turn 挂起审批时状态为 busy，旧实现 NewSession 保留 busy 不复位，
// 一次无人应答的审批会把服务永久锁死（UI 无限转圈），新建会话也被吞掉。
// 修复后 NewSession 应取消进行中的 turn 并复位到 ready。
func TestNativeServiceNewSessionUnblocksBusy(t *testing.T) {
	// write 工具需要审批：turn 停在 permission_request 上，不会自己结束。
	prov := &stubProvider{steps: []llm.StreamResult{
		toolStep("c1", "write", `{"path":"out.txt","content":"x"}`),
		textStep("写完了"),
	}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events

	if err := svc.Prompt("写个文件", nil); err != nil {
		t.Fatal(err)
	}
	// 等到 permission_request（此时 state=busy）。
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-sub.events:
			if ev.Type == "permission_request" {
				goto busyConfirmed
			}
		case <-deadline:
			t.Fatal("未收到审批请求")
		}
	}
busyConfirmed:
	for {
		if svc.Status().State == "busy" {
			break
		}
		select {
		case <-sub.events:
		case <-deadline:
			t.Fatal("turn 未进入 busy")
		}
	}

	// busy 期间新建会话：应成功并复位 ready，而不是静默保留 busy。
	cwd2 := t.TempDir()
	if err := svc.NewSession(t.Context(), cwd2); err != nil {
		t.Fatalf("busy 期间新建会话失败: %v", err)
	}
	if st := svc.Status(); st.State != "ready" {
		t.Fatalf("NewSession 后状态应为 ready, got %s (err=%s)", st.State, st.Error)
	}
	if st := svc.Status(); st.SessionID == "" || st.Cwd != cwd2 {
		t.Fatalf("NewSession 后应指向新会话: %+v", st)
	}
	// 旧 turn 已被取消：transcript 收到配对的取消结果，turn_done 收口。
	final := waitTurnDone(t, sub, 10*time.Second)
	if final.Type != "turn_done" {
		t.Fatalf("被放弃的 turn 应收口: %+v", final)
	}
	// 服务恢复可用：新会话能正常发起 turn。
	prov.steps = []llm.StreamResult{textStep("新的会话")}
	if err := svc.Prompt("你好", nil); err != nil {
		t.Fatalf("新会话 Prompt 失败: %v", err)
	}
	final = waitTurnDone(t, sub, 10*time.Second)
	if final.Type != "turn_done" {
		t.Fatalf("新会话 turn 应正常完成: %+v", final)
	}
}
