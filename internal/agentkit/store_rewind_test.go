package agentkit

import (
	"strings"
	"testing"

	"grok_switch/internal/llm"
)

func TestTruncateBeforeLastUserDropsTurnAndKeepsEarlierInjection(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := st.Create(SessionMeta{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	appendTestRecord(t, st, meta.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "q1"})
	appendTestRecord(t, st, meta.ID, Record{Origin: OriginAssistant, Role: llm.RoleAssistant, Text: "第一轮回复"})
	appendTestRecord(t, st, meta.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "q2"})
	// 压实提示是用户角色，但不是用户轮次。它夹在 q2 和回复之间时，
	// 按角色截断会停在提示上，把 q2 留在文件里。
	appendTestRecord(t, st, meta.ID, Record{Origin: OriginInjection, Role: llm.RoleUser, Text: "[上下文已压实: 折叠 1 条]"})
	appendTestRecord(t, st, meta.ID, Record{Origin: OriginAssistant, Role: llm.RoleAssistant, Text: "第二轮回复"})

	ok, err := st.TruncateBeforeLastUser(meta.ID)
	if err != nil || !ok {
		t.Fatalf("truncate: ok=%v err=%v", ok, err)
	}
	recs, err := st.LoadRecords(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Text != "q1" || recs[1].Text != "第一轮回复" {
		t.Fatalf("应留下第一轮: %+v", recs)
	}
	got, err := st.GetMeta(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageCount != 2 || got.Preview != "第一轮回复" {
		t.Fatalf("meta 未跟着截断: count=%d preview=%q", got.MessageCount, got.Preview)
	}

	appendTestRecord(t, st, meta.ID, Record{Origin: OriginUser, Role: llm.RoleUser, Text: "q3"})
	recs, err = st.LoadRecords(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[2].Text != "q3" || recs[2].Seq != 3 {
		t.Fatalf("截断后续写 seq 错乱: %+v", recs)
	}
}

func TestTruncateBeforeLastUserNoUserIsNoop(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := st.Create(SessionMeta{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	appendTestRecord(t, st, meta.ID, Record{Origin: OriginInjection, Role: llm.RoleUser, Text: "[上下文已压实]"})
	ok, err := st.TruncateBeforeLastUser(meta.ID)
	if err != nil || ok {
		t.Fatalf("没有用户记录应原样留下: ok=%v err=%v", ok, err)
	}
	recs, err := st.LoadRecords(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || !strings.Contains(recs[0].Text, "压实") {
		t.Fatalf("文件被改动: %+v", recs)
	}
}
