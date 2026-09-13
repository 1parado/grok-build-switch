package agentkit

import (
	"encoding/json"
	"testing"

	"grok_switch/internal/llm"
)

func appendTestRecord(t *testing.T, st *Store, id string, rec Record) {
	t.Helper()
	if err := st.AppendRecord(id, rec); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
}

func TestStorePreviewFromRecords(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := st.Create(SessionMeta{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	appendTestRecord(t, st, meta.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "  帮我看看\n这个项目  "})
	got, err := st.GetMeta(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Preview != "帮我看看 这个项目" {
		t.Fatalf("user preview 错误: %q", got.Preview)
	}
	appendTestRecord(t, st, meta.ID, Record{Origin: OriginAssistant, Role: llm.RoleAssistant, Text: "好的，我先看一下目录结构"})
	appendTestRecord(t, st, meta.ID, Record{Origin: OriginTool, Role: llm.RoleTool, Text: `{"output":"x","is_error":false}`})
	got, _ = st.GetMeta(meta.ID)
	if got.Preview != "好的，我先看一下目录结构" {
		t.Fatalf("tool 记录不应覆盖 preview: %q", got.Preview)
	}
}

func TestStoreForkAll(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src, err := st.Create(SessionMeta{Cwd: t.TempDir(), Title: "原会话", Model: "grok-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	appendTestRecord(t, st, src.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "第一句"})
	appendTestRecord(t, st, src.ID, Record{Origin: OriginAssistant, Role: llm.RoleAssistant, Text: "回答一"})

	forked, err := st.Fork(src.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if forked.ID == src.ID || forked.ForkedFrom != src.ID {
		t.Fatalf("fork meta 错误: %+v", forked)
	}
	if forked.Title != "原会话（分叉）" || forked.MessageCount != 2 || forked.Preview != "回答一" {
		t.Fatalf("fork 元数据错误: %+v", forked)
	}
	recs, err := st.LoadRecords(forked.ID)
	if err != nil || len(recs) != 2 {
		t.Fatalf("fork 记录数错误: %v %d", err, len(recs))
	}
	if recs[0].Seq != 1 || recs[1].Seq != 2 {
		t.Fatalf("fork 后 seq 应重排: %+v", []int64{recs[0].Seq, recs[1].Seq})
	}
}

func TestStoreForkTrimsMidToolSequence(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src, _ := st.Create(SessionMeta{Cwd: t.TempDir()})
	appendTestRecord(t, st, src.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "读个文件", UserTurn: 1})
	appendTestRecord(t, st, src.ID, Record{Origin: OriginAssistant, Role: llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)}}})
	appendTestRecord(t, st, src.ID, Record{Origin: OriginTool, Role: llm.RoleTool, ToolCallID: "c1", Text: "文件内容"})

	// 截在 tool 结果上：应回退到 user 边界，丢弃悬空的 call/result 对。
	forked, err := st.Fork(src.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := st.LoadRecords(forked.ID)
	if len(recs) != 1 || recs[0].Origin != OriginUser {
		t.Fatalf("应回退到干净边界: %+v", recs)
	}
}

func TestStoreForkUptoSeq(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	src, _ := st.Create(SessionMeta{Cwd: t.TempDir()})
	for i, text := range []string{"u1", "a1", "u2", "a2"} {
		origin := OriginUser
		role := llm.RoleUser
		if i%2 == 1 {
			origin = OriginAssistant
			role = llm.RoleAssistant
		}
		appendTestRecord(t, st, src.ID, Record{Origin: origin, Role: role, Text: text})
	}
	forked, err := st.Fork(src.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := st.LoadRecords(forked.ID)
	if len(recs) != 2 || recs[1].Text != "a1" {
		t.Fatalf("upto_seq 截断错误: %d 条", len(recs))
	}
	if forked.Preview != "a1" {
		t.Fatalf("fork preview 错误: %q", forked.Preview)
	}
	// 源会话不受影响。
	srcRecs, _ := st.LoadRecords(src.ID)
	if len(srcRecs) != 4 {
		t.Fatalf("源会话被改动: %d", len(srcRecs))
	}
	// fork 后的会话可独立继续追加。
	appendTestRecord(t, st, forked.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "u3"})
	recs, _ = st.LoadRecords(forked.ID)
	if len(recs) != 3 || recs[2].Seq != 3 {
		t.Fatalf("fork 后续写 seq 错乱: %+v", recs[len(recs)-1])
	}
}

func TestStoreForkMissing(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	if _, err := st.Fork("nope", 0); err == nil {
		t.Fatal("不存在的会话 fork 应报错")
	}
}
