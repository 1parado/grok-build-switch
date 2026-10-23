package server

import (
	"encoding/json"
	"testing"

	"grok_switch/internal/agentkit"
	"grok_switch/internal/agentloop"
)

func TestStepTextIsStoredBeforeTheToolCall(t *testing.T) {
	st, err := agentkit.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := st.Create(agentkit.SessionMeta{})
	if err != nil {
		t.Fatal(err)
	}
	tr := &nativeEventTranslator{
		svc:       &nativeAgentService{st: st},
		sessionID: meta.ID,
		turnID:    "t1",
	}
	tr.Dispatch(agentloop.Event{Type: agentloop.EventTextDelta, Text: "先看一下"})
	tr.Dispatch(agentloop.Event{Type: agentloop.EventStepEnd})
	tr.Dispatch(agentloop.Event{Type: agentloop.EventToolCall, ToolCall: &agentloop.ToolCallEvent{
		ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`),
	}})
	tr.Dispatch(agentloop.Event{Type: agentloop.EventToolResult, ToolResult: &agentloop.ToolResultEvent{
		ID: "c1", Name: "read", Output: "package main",
	}})
	tr.Dispatch(agentloop.Event{Type: agentloop.EventTextDelta, Text: "结果是 2"})
	tr.Dispatch(agentloop.Event{Type: agentloop.EventTurnEnd})

	recs, err := st.LoadRecords(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 {
		t.Fatalf("records = %#v", recs)
	}
	if recs[0].Origin != agentkit.OriginAssistant || recs[0].Text != "先看一下" || len(recs[0].ToolCalls) != 0 {
		t.Fatalf("preamble = %#v", recs[0])
	}
	if recs[1].Origin != agentkit.OriginAssistant || len(recs[1].ToolCalls) != 1 || recs[1].ToolCalls[0].ID != "c1" {
		t.Fatalf("tool call = %#v", recs[1])
	}
	if recs[2].Origin != agentkit.OriginTool || recs[2].ToolCallID != "c1" {
		t.Fatalf("tool result = %#v", recs[2])
	}
	if recs[3].Origin != agentkit.OriginAssistant || recs[3].Text != "结果是 2" {
		t.Fatalf("final text = %#v", recs[3])
	}
}
