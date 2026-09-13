package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"grok_switch/internal/agentbridge"
	"grok_switch/internal/llm"
)

// --- usage / todos 广播 ---

func TestNativeUsageBroadcast(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{textStep("回你一句")}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events

	if err := svc.Prompt("hi", nil); err != nil {
		t.Fatal(err)
	}
	var usage *agentbridge.UsageEvent
	done := false
	deadline := time.After(10 * time.Second)
	for !done {
		select {
		case ev := <-sub.events:
			if ev.Type == "usage" {
				usage = ev.Usage
			}
			if ev.Type == "turn_done" {
				done = true
			}
		case <-deadline:
			t.Fatal("等待 usage 超时")
		}
	}
	// textStep: InputOther=50, Output=10
	if usage == nil || usage.Input != 50 || usage.Output != 10 {
		t.Fatalf("usage 数值错误: %+v", usage)
	}
}

func TestNativeTodosBroadcast(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{
		toolStep("c1", "todo_list", `{"items":[{"content":"写代码","status":"in_progress"},{"content":"跑测试","status":"pending"}]}`),
		textStep("清单已记"),
	}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events

	_ = svc.Prompt("记一下任务", nil)
	var todos []agentbridge.TodoItem
	done := false
	deadline := time.After(10 * time.Second)
	for !done {
		select {
		case ev := <-sub.events:
			if ev.Type == "todos" {
				todos = ev.Todos
			}
			if ev.Type == "turn_done" {
				done = true
			}
		case <-deadline:
			t.Fatal("等待 todos 超时")
		}
	}
	if len(todos) != 2 || todos[0].Content != "写代码" || todos[0].Status != "in_progress" {
		t.Fatalf("todos 快照错误: %+v", todos)
	}
}

// --- 自动标题 + preview ---

func TestNativeAutoTitleAndPreview(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{textStep("这是回答")}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events

	sessionID := svc.Status().SessionID
	_ = svc.Prompt("帮我整理一下 下载目录的   文件", nil)
	waitTurnDone(t, sub, 10*time.Second)

	meta, err := svc.st.GetMeta(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "帮我整理一下 下载目录的 文件" {
		t.Fatalf("自动标题错误: %q", meta.Title)
	}
	if meta.Preview != "这是回答" {
		t.Fatalf("preview 错误: %q", meta.Preview)
	}
	// 列表投影也要带上。
	list, err := svc.ListStoredSessions("", 10)
	if err != nil || len(list) == 0 {
		t.Fatal(err)
	}
	if list[0].Preview != "这是回答" {
		t.Fatalf("列表 preview 缺失: %+v", list[0])
	}
	// 第二条消息不再覆盖已存在的标题。
	_ = svc.Prompt("第二条消息", nil)
	waitTurnDone(t, sub, 10*time.Second)
	meta, _ = svc.st.GetMeta(sessionID)
	if meta.Title != "帮我整理一下 下载目录的 文件" {
		t.Fatalf("标题被覆写: %q", meta.Title)
	}
}

// --- fork 端点 ---

func TestAgentForkEndpoint(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{textStep("回答")}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events
	sessionID := svc.Status().SessionID
	_ = svc.Prompt("第一句", nil)
	waitTurnDone(t, sub, 10*time.Second)

	srv := &Server{Agent: svc}
	req := httptest.NewRequest(http.MethodPost, "/api/agent/session/fork",
		strings.NewReader(`{"session_id":"`+sessionID+`"}`))
	rec := httptest.NewRecorder()
	srv.handleAgentFork(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fork 失败: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool                       `json:"ok"`
		Session agentbridge.SessionSummary `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Session.ID == "" || resp.Session.ID == sessionID || resp.Session.ForkedFrom != sessionID {
		t.Fatalf("fork 响应错误: %+v", resp)
	}
	if resp.Session.MessageCount != 2 {
		t.Fatalf("fork 记录数错误: %d", resp.Session.MessageCount)
	}
}

func TestAgentForkRejectsACP(t *testing.T) {
	srv := &Server{Agent: &stubACPService{}}
	req := httptest.NewRequest(http.MethodPost, "/api/agent/session/fork",
		strings.NewReader(`{"session_id":"acp-1"}`))
	rec := httptest.NewRecorder()
	srv.handleAgentFork(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ACP 会话 fork 应 400: %d", rec.Code)
	}
}

// --- export ---

func TestSessionMarkdownRender(t *testing.T) {
	history := agentbridge.SessionHistory{
		Session: agentbridge.SessionSummary{ID: "s1", Title: "测试会话", Model: "grok-4.6", Cwd: "/tmp/x"},
		Messages: []agentbridge.HistoryMessage{
			{Role: "user", Content: "你好"},
			{Role: "thought", Content: "想一下"},
			{Role: "assistant", Content: "你好！```code```", Model: "grok-4.6"},
			{Role: "tool", Tool: &agentbridge.ToolEvent{Title: "read", RawInput: map[string]any{"path": "a.txt"}}},
			{Role: "tool_result", Content: "文件内容"},
		},
	}
	md := sessionMarkdown(history, "测试会话")
	for _, want := range []string{"# 测试会话", "## 用户", "## Grok", "思考过程", "工具调用: read", "文件内容", "模型: grok-4.6"} {
		if !strings.Contains(md, want) {
			t.Fatalf("导出缺 %q:\n%s", want, md)
		}
	}
}

func TestAgentExportEndpoint(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{textStep("导出我")}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events
	sessionID := svc.Status().SessionID
	_ = svc.Prompt("写点东西", nil)
	waitTurnDone(t, sub, 10*time.Second)

	srv := &Server{Agent: svc}
	rec := httptest.NewRecorder()
	srv.handleAgentExport(rec, httptest.NewRequest(http.MethodGet, "/api/agent/session/export?id="+sessionID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export 失败: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "## 用户") || !strings.Contains(body, "导出我") || !strings.Contains(body, "写点东西") {
		t.Fatalf("导出内容缺失:\n%s", body)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "session-"+sessionID+".md") {
		t.Fatalf("文件名错误: %q", cd)
	}
}

// --- user 级权限规则 ---

func TestRespondPermissionUserRule(t *testing.T) {
	prov := &stubProvider{steps: []llm.StreamResult{
		toolStep("c1", "write", `{"path":"x.txt","content":"a"}`),
		textStep("done"),
	}}
	svc := newTestNativeService(t, prov)
	sub := subscribeNative(t, svc)
	_ = svc.Start(t.Context(), agentbridge.StartOptions{Cwd: t.TempDir()})
	<-sub.events

	_ = svc.Prompt("写文件", nil)
	var permEvt agentbridge.Event
	deadline := time.After(10 * time.Second)
	for permEvt.Permission == nil {
		select {
		case ev := <-sub.events:
			if ev.Type == "permission_request" {
				permEvt = ev
			}
		case <-deadline:
			t.Fatal("未收到审批请求")
		}
	}
	if err := svc.RespondPermissionUser(permEvt.Permission.RequestID, true); err != nil {
		t.Fatal(err)
	}
	waitTurnDone(t, sub, 10*time.Second)

	session, user := svc.perm.Rules()
	if len(user) == 0 || !strings.Contains(user[0], "write") {
		t.Fatalf("user 规则未持久化: session=%v user=%v", session, user)
	}
	if svc.Status().SessionAutoApprove {
		t.Fatal("user 级允许不应附带打开会话自动批准")
	}
}

// --- 可见性通知 ---

func TestNotifyAgentEventIfHidden(t *testing.T) {
	var got []string
	old := agentNotifyInfo
	agentNotifyInfo = func(title, body string) { got = append(got, title+":"+body) }
	defer func() { agentNotifyInfo = old }()

	srv := &Server{}
	srv.notifyAgentEventIfHidden(agentbridge.Event{Type: "turn_done", SessionID: "s1"})
	if len(got) != 1 || got[0] != "Grok 对话完成:点击回到工作台查看回复" {
		t.Fatalf("通知未发: %v", got)
	}
	// 同 key 5s 内去重。
	srv.notifyAgentEventIfHidden(agentbridge.Event{Type: "turn_done", SessionID: "s1"})
	if len(got) != 1 {
		t.Fatalf("去重失败: %v", got)
	}
	// 有可见客户端时不发。
	srv.agentVisible.Add(1)
	srv.notifyAgentEventIfHidden(agentbridge.Event{Type: "permission_request",
		Permission: &agentbridge.PermissionEvent{RequestID: "p1", Summary: "工具 x"}})
	if len(got) != 1 {
		t.Fatalf("可见客户端时不应通知: %v", got)
	}
}

// stubACPService 是不实现 ForkStoredSession 的桩（模拟 ACP 引擎）。
type stubACPService struct{ AgentService }
