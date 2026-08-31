package agentkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"grok_switch/internal/llm"
)

func TestStoreCreateListTouchDelete(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m1, err := store.Create(SessionMeta{Title: "会话一", Cwd: "/tmp/a"})
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := store.Create(SessionMeta{Title: "会话二", Cwd: "/tmp/b"})
	if m1.Engine != "native" || m1.ID == "" {
		t.Fatalf("Create 未回填: %+v", m1)
	}
	if m1.ID == m2.ID {
		t.Fatal("ID 应唯一")
	}

	// 追加记录 → MessageCount 增加、UpdatedAt 更新。
	before := m1.UpdatedAt
	time.Sleep(2 * time.Millisecond)
	if err := store.AppendRecord(m1.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "你好", UserTurn: 1}); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetMeta(m1.ID)
	if got.MessageCount != 1 || !got.UpdatedAt.After(before) {
		t.Fatalf("AppendRecord 未更新 meta: %+v", got)
	}

	// 列表倒序。
	list, _ := store.List(0)
	if len(list) != 2 || list[0].ID != m1.ID {
		t.Fatalf("列表顺序错误: %+v", list)
	}

	// 重命名。
	if err := store.Rename(m1.ID, "新标题"); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetMeta(m1.ID)
	if got.Title != "新标题" {
		t.Fatalf("重命名失败: %s", got.Title)
	}

	// 删除。
	if err := store.Delete(m2.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = store.List(0)
	if len(list) != 1 {
		t.Fatalf("删除后应剩 1: %d", len(list))
	}
}

func TestStoreTranscriptRoundTrip(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	meta, _ := store.Create(SessionMeta{Cwd: "/tmp"})
	records := []Record{
		{Origin: OriginUser, Role: llm.RoleUser, Text: "读一下 main.go", UserTurn: 1},
		{Origin: OriginAssistant, Role: llm.RoleAssistant, Text: "好的", Thinking: "思考中…"},
		{Origin: OriginAssistant, Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"main.go"}`)}}},
		{Origin: OriginTool, Role: llm.RoleTool, ToolCallID: "c1", Text: `{"output":"package main","error":false}`},
		{Origin: OriginUser, Role: llm.RoleUser, Text: "带图消息", Media: []MediaRef{{Kind: "image", URI: "/tmp/a.png", MimeType: "image/png"}}, UserTurn: 2},
	}
	for _, r := range records {
		if err := store.AppendRecord(meta.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.LoadRecords(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(records) {
		t.Fatalf("记录数不符: %d", len(got))
	}
	// Seq 递增。
	for i, r := range got {
		if r.Seq != int64(i+1) {
			t.Fatalf("Seq 错误: idx %d seq %d", i, r.Seq)
		}
	}
	// 回放消息投影。
	msg := got[2].ToMessage()
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name != "read" {
		t.Fatalf("tool_call 回放错误: %+v", msg)
	}
	toolMsg := got[3].ToMessage()
	if toolMsg.Role != llm.RoleTool || toolMsg.ToolCallID != "c1" || toolMsg.Text() == "" {
		t.Fatalf("tool 消息回放错误: %+v", toolMsg)
	}
	if len(got[4].Media) != 1 || got[4].Media[0].URI != "/tmp/a.png" {
		t.Fatalf("media 引用丢失: %+v", got[4])
	}
}

func TestStoreCorruptTolerant(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	meta, _ := store.Create(SessionMeta{Cwd: "/tmp"})
	if err := store.AppendRecord(meta.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "ok"}); err != nil {
		t.Fatal(err)
	}
	// 注入一条坏行。
	f, _ := os.OpenFile(store.transcriptPath(meta.ID), os.O_WRONLY|os.O_APPEND, 0o644)
	f.WriteString("{broken json\n")
	f.Close()
	if err := store.AppendRecord(meta.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "after"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadRecords(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Text != "after" {
		t.Fatalf("坏行应被跳过: %+v", got)
	}
}

func TestSanitizeID(t *testing.T) {
	cases := map[string]string{
		"abc-123":     "abc-123",
		"../evil":     "evil",
		"a/b\\c d":    "abcd",
		"":            "unnamed",
		"nat-123_456": "nat-123_456",
	}
	for in, want := range cases {
		if got := sanitizeID(in); got != want {
			t.Fatalf("sanitizeID(%q)=%q, want %q", in, got, want)
		}
	}
	// 路径穿越防护端到端。
	store, _ := NewStore(t.TempDir())
	dir := store.sessionDir("../../evil")
	root := store.root
	if !strings.HasPrefix(dir, root) {
		t.Fatalf("路径越界: %s not in %s", dir, root)
	}
	if filepath.Dir(dir) != root {
		t.Fatalf("sanitize 后应直接位于根下: %s", dir)
	}
}

func TestCtxMemoryTurnsAndRewind(t *testing.T) {
	mem := NewCtxMemory()
	mem.AppendWithOrigin(llm.Message{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "t1"}}}, OriginUser)
	mem.AppendWithOrigin(llm.Message{Role: llm.RoleAssistant, Parts: []llm.ContentPart{llm.TextPart{Text: "a1"}}}, OriginAssistant)
	mem.AppendWithOrigin(llm.Message{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "t2"}}}, OriginUser)
	mem.AppendWithOrigin(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c"}}}, OriginAssistant)
	mem.AppendWithOrigin(llm.Message{Role: llm.RoleTool, ToolCallID: "c"}, OriginTool)

	if mem.UserTurns() != 2 {
		t.Fatalf("用户轮次错误: %d", mem.UserTurns())
	}
	if !mem.RewindToUserTurn(2) {
		t.Fatal("rewind 应成功")
	}
	// 回退到第 2 轮用户消息：保留 [t1, a1]，轮次变 1。
	msgs, origins := mem.Snapshot()
	if len(msgs) != 2 || mem.UserTurns() != 1 || origins[0] != OriginUser || origins[1] != OriginAssistant {
		t.Fatalf("rewind 结果错误: %d msgs, %d turns, origins=%v", len(msgs), mem.UserTurns(), origins)
	}
	// 越界 rewind（第 5 轮不存在）：无操作返回 true（目标轮次本身有效定位失败时
	// 返回 false 的语义留给「定位不到任何轮」场景；越界视为已达末尾）。
	mem.RewindToUserTurn(5)
	if mem.UserTurns() != 1 || len(mem.History()) != 2 {
		t.Fatalf("越界 rewind 不应改变状态: turns=%d msgs=%d", mem.UserTurns(), len(mem.History()))
	}
}

func TestCtxMemoryRestoreFromRecords(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	meta, _ := store.Create(SessionMeta{Cwd: "/tmp"})
	_ = store.AppendRecord(meta.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "q1", UserTurn: 1})
	_ = store.AppendRecord(meta.ID, Record{Origin: OriginAssistant, Role: llm.RoleAssistant, Text: "a1"})

	records, _ := store.LoadRecords(meta.ID)
	mem := NewCtxMemory()
	mem.Restore(records)
	if mem.UserTurns() != 1 {
		t.Fatalf("恢复后轮次错误: %d", mem.UserTurns())
	}
	hist := mem.History()
	if len(hist) != 2 || hist[0].Text() != "q1" || hist[1].Text() != "a1" {
		t.Fatalf("恢复历史错误: %+v", hist)
	}
}
