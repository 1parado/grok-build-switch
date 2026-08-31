package server

// nativeAgentService 用自研引擎（agentloop + tools + agentkit）实现
// AgentService 接口。WS 协议、REST 路由与前端零改动（设计文档 D6）。
//
// 状态机：idle（未启动）→ ready（会话就绪）→ busy（turn 进行中）→ ready。
// 一个服务实例同一时刻持有一个活动会话（与 acp 桥的 ErrBusy 语义一致）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"grok_switch/internal/agentbridge"
	"grok_switch/internal/agentfs"
	"grok_switch/internal/agentkit"
	"grok_switch/internal/agentloop"
	"grok_switch/internal/llm"
	"grok_switch/internal/permission"
	"grok_switch/internal/tools"
)

// EngineDeps 是 native 引擎在 server 层的依赖（main.go 注入）。
type EngineDeps struct {
	SessionsRoot string
	// ProviderFor 返回当前会话应使用的 Provider（按当前生效 Profile 构造）。
	// 返回 error 时 Prompt 报"引擎未配置"。
	ProviderFor func() (llm.Provider, error)
	// SystemPrompt 组装系统提示词（每 turn 以当前会话的环境块调用）。
	SystemPrompt func(env string) string
	// ImageGen 为 nil 时不注册 generate_image。
	ImageGen tools.ImageGenerator
	// DefaultCwd 兜底工作目录。
	DefaultCwd string
}

// permissionOptionFor 构造与 acp 桥一致的审批选项。
var nativePermissionOptions = []agentbridge.PermissionOption{
	{ID: "allow_once", Name: "允许本次", Kind: "allow_once"},
	{ID: "allow_always", Name: "本会话总是允许", Kind: "allow_always"},
	{ID: "reject_once", Name: "拒绝", Kind: "reject_once"},
}

type nativeAgentService struct {
	deps EngineDeps
	st   *agentkit.Store
	perm *permission.Engine

	mu sync.Mutex
	// 会话状态。
	sessionID string
	cwd       string
	memory    *agentkit.CtxMemory
	registry  *tools.Registry
	state     string // idle | ready | busy
	autoAppr  bool
	model     string
	effort    string
	errText   string

	// turn 控制。
	turnCancel context.CancelFunc
	turnSeq    int64

	// 审批挂起：requestID → 等待通道 + 工具名（allow_always 写规则用）。
	permMu      sync.Mutex
	pendingPerm map[string]*pendingPermReq
	planMu      sync.Mutex
	pendingPlan map[string]chan agentbridge.PlanDecision
	planSeq     int64
	// 挂起审批的原始事件（按发生顺序）：WS 断开/页面刷新会丢掉一次性广播，
	// 订阅与查询接口据此重放，用户重新打开页面可继续审批。
	pendingPermEvents map[string]agentbridge.Event
	pendingPermOrder  []string

	// 订阅者。
	subMu   sync.Mutex
	nextSub int
	subs    map[int]chan agentbridge.Event

	// 会话级 usage 观测（compaction 触发依据，跨 turn 保持）。
	usageHolder     *turnUsageHolder
	usageHolderOnce sync.Once
}

func newNativeAgentService(deps EngineDeps) (*nativeAgentService, error) {
	st, err := agentkit.NewStore(deps.SessionsRoot)
	if err != nil {
		return nil, err
	}
	// user 级权限规则持久化在会话存储根的上级（agent2/permissions.json）。
	perm, err := permission.NewEngine(filepath.Dir(strings.TrimSuffix(deps.SessionsRoot, "/")) + "/permissions.json")
	if err != nil {
		return nil, err
	}
	return &nativeAgentService{
		deps:              deps,
		st:                st,
		perm:              perm,
		state:             "idle",
		pendingPerm:       map[string]*pendingPermReq{},
		pendingPermEvents: map[string]agentbridge.Event{},
		pendingPlan:       map[string]chan agentbridge.PlanDecision{},
		subs:              map[int]chan agentbridge.Event{},
	}, nil
}

// --- 事件广播（对齐 agentbridge 的 Subscribe 模式） ---

func (n *nativeAgentService) Subscribe() (string, <-chan agentbridge.Event) {
	n.subMu.Lock()
	defer n.subMu.Unlock()
	id := n.nextSub
	n.nextSub++
	ch := make(chan agentbridge.Event, 256)
	n.subs[id] = ch
	// 订阅即推当前状态（与 acp 桥行为一致）。
	status := n.Status()
	ch <- agentbridge.Event{
		Type: "agent_status", SessionID: status.SessionID, Status: status.State,
		Model: status.Model, Error: status.Error, SessionAutoApprove: &status.SessionAutoApprove,
	}
	// 重放仍然挂起的审批：广播是 fire-and-forget，页面刷新/WS 断开期间
	// 发出的 permission_request 不会再来第二次；没有重放，用户重新打开
	// 页面只能眼睁睁看着 turn 卡在 busy。
	for _, requestID := range n.pendingPermOrder {
		if ev, ok := n.pendingPermEvents[requestID]; ok {
			ch <- ev
		}
	}
	return fmt.Sprintf("sub-%d", id), ch
}

func (n *nativeAgentService) Unsubscribe(id string) {
	var num int
	if _, err := fmt.Sscanf(id, "sub-%d", &num); err != nil {
		return
	}
	n.subMu.Lock()
	defer n.subMu.Unlock()
	if ch, ok := n.subs[num]; ok {
		close(ch)
		delete(n.subs, num)
	}
}

func (n *nativeAgentService) broadcast(ev agentbridge.Event) {
	n.subMu.Lock()
	defer n.subMu.Unlock()
	for _, ch := range n.subs {
		select {
		case ch <- ev:
		default:
			// 订阅者消费不及则丢弃（UI 只关心最新状态；录制在 transcript）。
		}
	}
}

func (n *nativeAgentService) broadcastStatus() {
	n.mu.Lock()
	status := agentbridge.Event{
		Type: "agent_status", SessionID: n.sessionID, Status: n.state,
		Model: n.model, Error: n.errText, SessionAutoApprove: &n.autoAppr,
	}
	n.mu.Unlock()
	n.broadcast(status)
}

// --- AgentService 实现 ---

func (n *nativeAgentService) Status() agentbridge.Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return agentbridge.Status{
		Available:          true,
		Running:            n.state == "ready" || n.state == "busy",
		State:              n.state,
		SessionID:          n.sessionID,
		Cwd:                n.cwd,
		DefaultCwd:         n.deps.DefaultCwd,
		Busy:               n.state == "busy",
		SessionAutoApprove: n.autoAppr,
		Model:              n.model,
		Error:              n.errText,
		UserTurnCount:      n.memoryUserTurnsLocked(),
	}
}

func (n *nativeAgentService) memoryUserTurnsLocked() int {
	if n.memory == nil {
		return 0
	}
	return n.memory.UserTurns()
}

// Start 初始化引擎会话（不 spawn 进程——原生引擎无需子进程）。
func (n *nativeAgentService) Start(ctx context.Context, opts agentbridge.StartOptions) error {
	n.mu.Lock()
	if opts.SessionID != "" && opts.SessionID == n.sessionID && n.state == "ready" {
		n.mu.Unlock()
		return nil // 已就绪（幂等）
	}
	if opts.Cwd == "" {
		opts.Cwd = n.deps.DefaultCwd
	}
	n.state = "starting"
	n.errText = ""
	cwd := opts.Cwd
	sessionID := opts.SessionID
	n.mu.Unlock()
	n.broadcastStatus()

	// cwd 必须真实存在：否则会话会在死路径上静默运行并被 rememberAgentCwd
	// 持久化，形成"路径不可用"的坏项目卡片。
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		n.mu.Lock()
		n.state = "dead"
		n.errText = "工作目录不存在: " + cwd
		n.mu.Unlock()
		n.broadcastStatus()
		return fmt.Errorf("工作目录不存在: %s", cwd)
	}

	// 恢复既有会话或新建。
	if sessionID != "" && n.st.Exists(sessionID) {
		records, err := n.st.LoadRecords(sessionID)
		if err == nil {
			mem := agentkit.NewCtxMemory()
			mem.Restore(records)
			meta, _ := n.st.GetMeta(sessionID)
			n.mu.Lock()
			n.memory = mem
			n.sessionID = sessionID
			n.cwd = cwd
			n.model = meta.Model
			n.state = "ready"
			n.mu.Unlock()
			n.broadcastStatus()
			return nil
		}
	}

	meta, err := n.st.Create(agentkit.SessionMeta{Cwd: cwd})
	if err != nil {
		n.mu.Lock()
		n.state = "dead"
		n.errText = err.Error()
		n.mu.Unlock()
		n.broadcastStatus()
		return err
	}
	n.mu.Lock()
	n.memory = agentkit.NewCtxMemory()
	n.sessionID = meta.ID
	n.cwd = cwd
	n.state = "ready"
	n.mu.Unlock()
	n.broadcastStatus()
	return nil
}

func (n *nativeAgentService) Stop() error {
	n.mu.Lock()
	if n.turnCancel != nil {
		n.turnCancel()
		n.turnCancel = nil
	}
	n.state = "idle"
	n.sessionID = ""
	n.memory = nil
	n.mu.Unlock()
	n.broadcastStatus()
	return nil
}

func (n *nativeAgentService) NewSession(ctx context.Context, cwd string) error {
	// 取消进行中的 turn：新建会话意味着用户明确放弃当前任务，若 busy 时
	// 不复位状态，一次卡死的 turn（如权限审批无人应答）会把服务永久锁在
	// busy，后续所有 NewSession 都被吞掉，UI 表现为无限转圈。
	n.mu.Lock()
	if n.turnCancel != nil {
		n.turnCancel()
		n.turnCancel = nil
	}
	// 卡死 turn 的挂起审批/计划一并唤醒（CancelPrompt 同款语义），
	// 否则被取消的 turn goroutine 收口时可能把状态写回 ready 之外。
	n.mu.Unlock()
	n.resolveAllPending()
	if cwd == "" {
		cwd = n.deps.DefaultCwd
	}
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		return fmt.Errorf("工作目录不存在: %s", cwd)
	}
	n.mu.Lock()
	n.cwd = cwd
	n.memory = agentkit.NewCtxMemory()
	n.errText = ""
	n.sessionID = ""
	n.state = "starting"
	n.mu.Unlock()
	n.broadcastStatus()
	meta, err := n.st.Create(agentkit.SessionMeta{Cwd: cwd})
	if err != nil {
		n.mu.Lock()
		n.state = "dead"
		n.errText = err.Error()
		n.mu.Unlock()
		n.broadcastStatus()
		return err
	}
	n.mu.Lock()
	n.sessionID = meta.ID
	n.state = "ready"
	n.mu.Unlock()
	n.broadcastStatus()
	return nil
}

// Prompt 启动一个 turn（异步）；事件经引擎回调翻译成 agentbridge.Event。
func (n *nativeAgentService) Prompt(text string, attachments []agentbridge.Attachment) error {
	n.mu.Lock()
	if n.state == "busy" {
		n.mu.Unlock()
		return agentbridge.ErrBusy
	}
	if n.state != "ready" || n.memory == nil {
		n.mu.Unlock()
		return agentbridge.ErrNotRunning
	}
	provider, err := n.deps.ProviderFor()
	if err != nil {
		n.mu.Unlock()
		return fmt.Errorf("引擎未就绪: %w", err)
	}
	if n.model == "" {
		n.model = provider.ModelName()
	}
	effort := n.effort
	// turn 独立 ctx（与 Start/Stop 的生命周期解耦）。
	turnCtx, cancel := context.WithCancel(context.Background())
	n.turnCancel = cancel
	n.turnSeq++
	turnID := fmt.Sprintf("turn-%d-%d", time.Now().UnixNano(), n.turnSeq)
	sessionID := n.sessionID
	mem := n.memory
	env := agentfs.Env{Cwd: n.cwd}
	n.state = "busy"
	n.mu.Unlock()
	n.broadcastStatus()

	// 组装用户消息（文本 + 附件）。
	userMsg := buildUserMessage(text, attachments)
	mem.AppendWithOrigin(userMsg, agentkit.OriginUser)
	n.persist(sessionID, agentkit.Record{
		Origin: agentkit.OriginUser, Role: llm.RoleUser,
		Text: text, TurnID: turnID, UserTurn: mem.UserTurns(),
		Media: attachmentsToMediaRefs(attachments),
	})
	n.broadcast(agentbridge.Event{
		Type: "user_message", SessionID: sessionID, Text: text,
	})

	// 工具注册表（每 turn 重建：生图开关即时生效）。
	registry := tools.DefaultRegistry(func() agentfs.Env { return env }, n.deps.ImageGen, &nativePlanApprover{n: n}, &tools.TodoStore{})

	// 引擎事件 → WS 事件翻译。usage 观测挂在服务上（跨 turn 保持）。
	n.usageHolderOnce.Do(func() { n.usageHolder = &turnUsageHolder{} })
	loopEvents := &nativeEventTranslator{svc: n, sessionID: sessionID, turnID: turnID, registry: registry, usage: n.usageHolder}

	// compaction hook（D3/§6.4）：无状态重放下「上一步 input tokens」≈ 当前
	// 全量历史体积；超阈值时在步边界压实（摘要走当前 Provider 一次调用）。
	compactionCfg := agentkit.DefaultCompactionConfig()
	compactionCap := provider.Capability()
	hooks := agentloop.Hooks{BeforeStep: func(ctx context.Context, step int) error {
		// 每个 step 边界（含 turn 起始）都检查：上一步 usage 已含上一 turn
		// 的真实 input；新会话 usage 为 0 自然不触发。
		if !compactionCfg.ShouldCompact(n.usageHolder.lastInputTokens(), int64(compactionCap.MaxContextTokens)) {
			return nil
		}
		msgs, origins := mem.Snapshot()
		out, outOrigins, stats, err := agentkit.Compact(msgs, origins, func(dropped []llm.Message) (string, error) {
			return compactViaLLM(ctx, provider, dropped)
		}, compactionCfg)
		if err != nil || stats.CompactedCount == 0 {
			return nil // 压实失败不终止 turn；下一步仍会重试
		}
		mem.CompactInPlace(out, outOrigins, stats)
		n.persist(sessionID, agentkit.Record{
			Origin: agentkit.OriginInjection, Role: llm.RoleUser,
			Text:   fmt.Sprintf("[上下文已压实: 折叠 %d 条，%d → %d tokens]", stats.CompactedCount, stats.TokensBefore, stats.TokensAfter),
			TurnID: turnID,
		})
		n.broadcast(agentbridge.Event{Type: "notice", SessionID: sessionID, Text: fmt.Sprintf("长对话已自动压缩（折叠 %d 条消息）", stats.CompactedCount)})
		return nil
	}}

	go func() {
		defer cancel()
		res, runErr := agentloop.RunTurn(turnCtx, agentloop.RunTurnInput{
			TurnID:       turnID,
			Provider:     provider,
			SystemPrompt: n.deps.SystemPrompt(buildEnvSection(n.cwd)),
			Memory:       mem,
			Tools:        &tools.ToolsAdapter{Registry: registry},
			PermGate:     &nativePermGate{svc: n, registry: registry},
			Events:       loopEvents,
			MaxSteps:     100,
			MaxRetries:   3,
			Hooks:        hooks,
			Effort:       effort,
		})
		// turn 收尾。
		n.mu.Lock()
		n.turnCancel = nil
		if n.state == "busy" {
			n.state = "ready"
		}
		n.mu.Unlock()
		switch {
		case runErr != nil && res.StopReason == agentloop.TurnUserAbort:
			n.broadcast(agentbridge.Event{Type: "turn_done", SessionID: sessionID, StopReason: "cancelled"})
		case runErr != nil:
			n.mu.Lock()
			n.errText = runErr.Error()
			n.mu.Unlock()
			n.broadcast(agentbridge.Event{Type: "error", SessionID: sessionID, Error: "Grok 对话失败: " + runErr.Error()})
			n.broadcast(agentbridge.Event{Type: "turn_done", SessionID: sessionID, StopReason: "error"})
		default:
			n.broadcast(agentbridge.Event{Type: "turn_done", SessionID: sessionID, StopReason: string(res.StopReason)})
		}
		n.broadcastStatus()
	}()
	return nil
}

func buildUserMessage(text string, attachments []agentbridge.Attachment) llm.Message {
	msg := llm.Message{Role: llm.RoleUser}
	if strings.TrimSpace(text) != "" {
		msg.Parts = append(msg.Parts, llm.TextPart{Text: text})
	}
	for _, a := range attachments {
		if a.Kind == "image" && a.Data != "" {
			msg.Parts = append(msg.Parts, llm.ImagePart{Data: a.Data, MimeType: a.MimeType})
		} else if a.Path != "" {
			// 路径附件作为文本上下文注入（P3 决策：不推 base64）。
			msg.Parts = append(msg.Parts, llm.TextPart{Text: fmt.Sprintf("[附件 %s: %s]", a.Name, a.Path)})
		}
	}
	return msg
}

func attachmentsToMediaRefs(attachments []agentbridge.Attachment) []agentkit.MediaRef {
	var out []agentkit.MediaRef
	for _, a := range attachments {
		if a.Kind == "image" && a.Path != "" {
			out = append(out, agentkit.MediaRef{Kind: "image", URI: a.Path, Name: a.Name, MimeType: a.MimeType})
		}
	}
	return out
}

// persist 落盘记录；失败仅记 errText（不阻塞对话）。
func (n *nativeAgentService) persist(sessionID string, rec agentkit.Record) {
	if sessionID == "" {
		return
	}
	if err := n.st.AppendRecord(sessionID, rec); err != nil {
		n.mu.Lock()
		n.errText = "会话记录写入失败: " + err.Error()
		n.mu.Unlock()
	}
}

func (n *nativeAgentService) CancelPrompt() error {
	n.mu.Lock()
	cancel := n.turnCancel
	n.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	// 权限/计划审批挂起也一并唤醒。
	n.resolveAllPending()
	return nil
}

// PendingRequests 返回仍然挂起的审批/计划事件（按发生顺序），供
// GET /api/agent/pending 在 WS 重连后拉取重放。事件是登记时的快照。
func (n *nativeAgentService) PendingRequests() []agentbridge.Event {
	n.permMu.Lock()
	events := make([]agentbridge.Event, 0, len(n.pendingPermOrder)+len(n.pendingPlan))
	for _, requestID := range n.pendingPermOrder {
		if ev, ok := n.pendingPermEvents[requestID]; ok {
			events = append(events, ev)
		}
	}
	n.permMu.Unlock()
	n.planMu.Lock()
	for requestID := range n.pendingPlan {
		events = append(events, agentbridge.Event{
			Type: "plan_request", SessionID: n.sessionID,
			Plan: &agentbridge.PlanEvent{RequestID: requestID, Waiting: true},
		})
	}
	n.planMu.Unlock()
	return events
}

// --- 审批（权限/计划） ---

// nativePermGate 实现 agentloop.PermissionGate：manual 模式下写类工具需要审批。
type nativePermGate struct {
	svc      *nativeAgentService
	registry *tools.Registry
}

// readOnlyTools 默认免审批工具（D5：manual 模式下其余工具 ask）。
var readOnlyTools = map[string]bool{"read": true, "glob": true, "grep": true, "todo_list": true, "enter_plan_mode": true}

func (g *nativePermGate) Check(call llm.ToolCall) agentloop.Decision {
	// 规则引擎优先：deny > 模式 > allow > ask（D5 决策顺序）。
	// autoAppr 映射为 ModeAuto；手动模式走规则表。
	if g.svc.perm != nil {
		if g.svc.Status().SessionAutoApprove {
			g.svc.perm.SetMode(permission.ModeAuto)
		} else {
			g.svc.perm.SetMode(permission.ModeManual)
		}
		switch g.svc.perm.Check(call.Name, call.Arguments) {
		case "deny":
			return agentloop.DecDeny
		case "allow":
			return agentloop.DecAllow
		}
	} else if g.svc.Status().SessionAutoApprove {
		return agentloop.DecAllow
	}
	if readOnlyTools[call.Name] {
		return agentloop.DecAllow
	}
	if call.Name == "exit_plan_mode" {
		return agentloop.DecAllow // 计划审批走 plan_request 流
	}
	return agentloop.DecAsk
}

// pendingPermReq 是一条挂起的审批。
type pendingPermReq struct {
	ch   chan agentloop.PermResult
	tool string
}

func (g *nativePermGate) WaitForDecision(ctx context.Context, call llm.ToolCall) agentloop.PermResult {
	requestID := fmt.Sprintf("perm-%d", time.Now().UnixNano())
	req := &pendingPermReq{ch: make(chan agentloop.PermResult, 1), tool: call.Name}
	g.svc.permMu.Lock()
	g.svc.pendingPerm[requestID] = req
	permEvt := agentbridge.Event{
		Type: "permission_request", SessionID: g.svc.sessionID,
		Permission: &agentbridge.PermissionEvent{
			RequestID: requestID,
			Summary:   fmt.Sprintf("工具 %s 请求执行", call.Name),
			Tool: agentbridge.ToolEvent{
				ID: call.ID, Title: call.Name, Kind: call.Name,
				Status: "pending", RawInput: json.RawMessage(call.Arguments),
			},
			Options: nativePermissionOptions,
		},
	}
	// 记录挂起事件供重放：WS 是 fire-and-forget 广播，断线/刷新即丢；
	// 没有这条记录，用户重新打开页面就再也看不到这次审批。
	g.svc.pendingPermEvents[requestID] = permEvt
	g.svc.pendingPermOrder = append(g.svc.pendingPermOrder, requestID)
	g.svc.permMu.Unlock()
	defer func() {
		g.svc.permMu.Lock()
		delete(g.svc.pendingPerm, requestID)
		delete(g.svc.pendingPermEvents, requestID)
		for i, id := range g.svc.pendingPermOrder {
			if id == requestID {
				g.svc.pendingPermOrder = append(g.svc.pendingPermOrder[:i], g.svc.pendingPermOrder[i+1:]...)
				break
			}
		}
		g.svc.permMu.Unlock()
	}()

	g.svc.broadcast(permEvt)

	select {
	case res := <-req.ch:
		return res
	case <-ctx.Done():
		return agentloop.PermResult{Decision: agentloop.DecDeny, Reason: "用户取消了任务。"}
	}
}

func (n *nativeAgentService) resolveAllPending() {
	n.permMu.Lock()
	for _, req := range n.pendingPerm {
		req.ch <- agentloop.PermResult{Decision: agentloop.DecDeny, Reason: "用户取消了任务。"}
	}
	n.pendingPerm = map[string]*pendingPermReq{}
	n.pendingPermEvents = map[string]agentbridge.Event{}
	n.pendingPermOrder = nil
	n.permMu.Unlock()
	n.planMu.Lock()
	for _, ch := range n.pendingPlan {
		ch <- agentbridge.PlanDecision{Outcome: "cancelled"}
	}
	n.pendingPlan = map[string]chan agentbridge.PlanDecision{}
	n.planMu.Unlock()
}

func (n *nativeAgentService) RespondPermission(requestID string, allow bool) error {
	return n.RespondPermissionEx(requestID, allow, false)
}

func (n *nativeAgentService) RespondPermissionEx(requestID string, allow bool, remember bool) error {
	decision := agentloop.DecDeny
	reason := "用户拒绝了该操作。"
	if allow {
		decision = agentloop.DecAllow
		reason = ""
	}
	var toolName string
	if allow && remember {
		n.SetSessionAutoApprove(true)
	}
	n.permMu.Lock()
	req, ok := n.pendingPerm[requestID]
	if ok {
		toolName = req.tool
		delete(n.pendingPerm, requestID)
	}
	n.permMu.Unlock()
	if !ok {
		return agentbridge.ErrPermissionNotFound
	}
	// allow_always 同时沉淀工具级会话规则（D5）：用户关闭会话级自动批准后，
	// 该工具仍保持放行。
	if allow && remember && n.perm != nil && toolName != "" {
		_ = n.perm.AddSessionRule(toolName+"(*)", permission.Allow)
	}
	req.ch <- agentloop.PermResult{Decision: decision, Reason: reason}
	return nil
}

func (n *nativeAgentService) RespondPermissionOption(requestID, optionID string, remember bool) error {
	switch optionID {
	case "allow_once":
		return n.RespondPermissionEx(requestID, true, false)
	case "allow_always":
		return n.RespondPermissionEx(requestID, true, true)
	default:
		return n.RespondPermissionEx(requestID, false, false)
	}
}

// nativePlanApprover 实现 tools.PlanApprover，桥到 plan_request WS 事件。
type nativePlanApprover struct{ n *nativeAgentService }

func (p *nativePlanApprover) RequestPlanMode(ctx context.Context) bool {
	// 原生引擎一期：进入计划模式不阻塞审批（只读探索本身安全）。
	return true
}

func (p *nativePlanApprover) ExitPlanWithPlan(ctx context.Context, plan string) (bool, string) {
	requestID := fmt.Sprintf("plan-%d", time.Now().UnixNano())
	ch := make(chan agentbridge.PlanDecision, 1)
	p.n.planMu.Lock()
	p.n.pendingPlan[requestID] = ch
	p.n.planMu.Unlock()
	defer func() {
		p.n.planMu.Lock()
		delete(p.n.pendingPlan, requestID)
		p.n.planMu.Unlock()
	}()

	p.n.broadcast(agentbridge.Event{
		Type: "plan_request", SessionID: p.n.Status().SessionID,
		Plan: &agentbridge.PlanEvent{RequestID: requestID, Body: plan, Waiting: true},
	})

	select {
	case res := <-ch:
		return res.Outcome == "approved", res.Feedback
	case <-ctx.Done():
		return false, "用户取消了任务。"
	}
}

func (n *nativeAgentService) RespondPlan(requestID string, decision agentbridge.PlanDecision) error {
	n.planMu.Lock()
	ch, ok := n.pendingPlan[requestID]
	if ok {
		delete(n.pendingPlan, requestID)
	}
	n.planMu.Unlock()
	if !ok {
		return agentbridge.ErrPlanNotFound
	}
	ch <- decision
	return nil
}

// --- 会话配置与控制 ---

func (n *nativeAgentService) SetSessionAutoApprove(enabled bool) {
	n.mu.Lock()
	n.autoAppr = enabled
	n.mu.Unlock()
	n.broadcastStatus()
}

func (n *nativeAgentService) SetSessionConfig(ctx context.Context, model, strength string) error {
	n.mu.Lock()
	if model != "" {
		n.model = model
	}
	if strength != "" {
		n.effort = strength
	}
	sessionID := n.sessionID
	currentModel := n.model
	n.mu.Unlock()
	if sessionID != "" {
		_ = n.st.Touch(sessionID, func(m *agentkit.SessionMeta) { m.Model = currentModel })
	}
	n.broadcastStatus()
	return nil
}

func (n *nativeAgentService) RewindDropLastUser(ctx context.Context, restoreFiles bool) agentbridge.RewindResult {
	n.mu.Lock()
	mem := n.memory
	sessionID := n.sessionID
	busy := n.state == "busy"
	n.mu.Unlock()
	if busy || mem == nil {
		return agentbridge.RewindResult{OK: false, Error: "会话进行中，无法回退"}
	}
	target := mem.UserTurns()
	if !mem.RewindToUserTurn(target) {
		return agentbridge.RewindResult{OK: false, Error: "没有可回退的用户消息"}
	}
	n.mu.Lock()
	n.errText = ""
	n.mu.Unlock()
	_ = sessionID
	return agentbridge.RewindResult{OK: true, UserTurnCount: mem.UserTurns()}
}

func (n *nativeAgentService) ClearBootstrap()                      {}
func (n *nativeAgentService) ArmBootstrapFromSession(string) error { return nil }

// --- 历史存储（ListStoredSessions 等映射到 agentkit.Store） ---

func (n *nativeAgentService) ListStoredSessions(cwd string, limit int) ([]agentbridge.SessionSummary, error) {
	metas, err := n.st.List(limit)
	if err != nil {
		return nil, err
	}
	out := make([]agentbridge.SessionSummary, 0, len(metas))
	for _, m := range metas {
		if cwd != "" && m.Cwd != cwd {
			continue
		}
		out = append(out, agentbridge.SessionSummary{
			ID: m.ID, Title: m.Title, Cwd: m.Cwd,
			CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
			Model: m.Model, MessageCount: m.MessageCount,
			CwdMissing: !dirExists(m.Cwd),
		})
	}
	return out, nil
}

func dirExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return true
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func (n *nativeAgentService) StoredSessionHistory(id string) (agentbridge.SessionHistory, error) {
	meta, err := n.st.GetMeta(id)
	if err != nil {
		return agentbridge.SessionHistory{}, err
	}
	records, err := n.st.LoadRecords(id)
	if err != nil {
		return agentbridge.SessionHistory{}, err
	}
	summary := agentbridge.SessionSummary{
		ID: meta.ID, Title: meta.Title, Cwd: meta.Cwd,
		CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt,
		Model: meta.Model, MessageCount: meta.MessageCount,
		CwdMissing: !dirExists(meta.Cwd),
	}
	msgs := make([]agentbridge.HistoryMessage, 0, len(records))
	for _, r := range records {
		switch r.Origin {
		case agentkit.OriginUser:
			msgs = append(msgs, agentbridge.HistoryMessage{Role: "user", Content: r.Text, Media: mediaRefsToBridge(r.Media)})
		case agentkit.OriginAssistant:
			if r.Text != "" {
				msgs = append(msgs, agentbridge.HistoryMessage{Role: "assistant", Content: r.Text, Model: meta.Model})
			}
			if len(r.ToolCalls) > 0 {
				for _, tc := range r.ToolCalls {
					msgs = append(msgs, agentbridge.HistoryMessage{
						Role: "tool",
						Tool: &agentbridge.ToolEvent{ID: tc.ID, Title: tc.Name, Kind: tc.Name, Status: "completed", RawInput: json.RawMessage(tc.Arguments)},
					})
				}
			}
		case agentkit.OriginTool:
			var payload struct {
				Output string `json:"output"`
				Error  bool   `json:"error"`
			}
			_ = json.Unmarshal([]byte(r.Text), &payload)
			msgs = append(msgs, agentbridge.HistoryMessage{
				Role:    "tool_result",
				Content: payload.Output,
				Tool:    &agentbridge.ToolEvent{ID: r.ToolCallID, Status: toolStatusFromPayload(payload.Error)},
			})
		}
	}
	return agentbridge.SessionHistory{Session: summary, Messages: msgs}, nil
}

func toolStatusFromPayload(isErr bool) string {
	if isErr {
		return "failed"
	}
	return "completed"
}

func mediaRefsToBridge(refs []agentkit.MediaRef) []agentbridge.MediaContent {
	if len(refs) == 0 {
		return nil
	}
	out := make([]agentbridge.MediaContent, 0, len(refs))
	for _, r := range refs {
		out = append(out, agentbridge.MediaContent{Kind: r.Kind, MimeType: r.MimeType, URI: r.URI, Name: r.Name})
	}
	return out
}

func (n *nativeAgentService) RenameStoredSession(id, title string) error {
	return n.st.Rename(id, title)
}

func (n *nativeAgentService) DeleteStoredSession(id string) error {
	n.mu.Lock()
	active := n.sessionID == id
	n.mu.Unlock()
	if active {
		_ = n.Stop()
	}
	return n.st.Delete(id)
}

// 编译期确保实现完整接口。
var _ AgentService = (*nativeAgentService)(nil)
