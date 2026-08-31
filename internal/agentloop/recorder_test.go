package agentloop

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"grok_switch/internal/llm"
)

func TestJSONLRecorderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "turn.jsonl")
	rec, err := NewJSONLRecorder(path)
	if err != nil {
		t.Fatal(err)
	}

	rec.Dispatch(Event{Type: EventTurnBegin, TurnID: "t1"})
	rec.Dispatch(Event{Type: EventTextDelta, TurnID: "t1", Text: "你好"})
	u := llm.TokenUsage{InputOther: 5, Output: 3}
	rec.Dispatch(Event{Type: EventUsage, TurnID: "t1", Usage: &u, UsageAccuracy: string(llm.UsageExact)})
	rec.Dispatch(Event{Type: EventToolCall, TurnID: "t1", ToolCall: &ToolCallEvent{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)}})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	// 逐行读回并验证事件完整。
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []Event
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("行解析失败: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) != 4 {
		t.Fatalf("应有 4 行, got %d", len(events))
	}
	if events[1].Text != "你好" {
		t.Fatalf("文本丢失: %+v", events[1])
	}
	if events[2].Usage == nil || events[2].Usage.InputOther != 5 {
		t.Fatalf("usage 丢失: %+v", events[2])
	}
	if events[3].ToolCall == nil || events[3].ToolCall.Name != "read" {
		t.Fatalf("工具事件丢失: %+v", events[3])
	}
}

func TestJSONLRecorderAppendMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "turn.jsonl")
	rec1, _ := NewJSONLRecorder(path)
	rec1.Dispatch(Event{Type: EventTurnBegin, TurnID: "t1"})
	rec1.Close()

	rec2, _ := NewJSONLRecorder(path)
	rec2.Dispatch(Event{Type: EventTurnEnd, TurnID: "t1"})
	rec2.Close()

	data, _ := os.ReadFile(path)
	lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1
	if lines != 2 {
		t.Fatalf("重开应追加而非覆盖: %d 行", lines)
	}
}

func TestBroadcasterDropAndRecord(t *testing.T) {
	b := NewBroadcaster()
	rec := &MemoryRecorder{}
	b.AddRecorder(rec)

	// Subscribe 下限 16 缓冲：灌 20 条，超出 16 的部分应被丢弃且不影响录制。
	_, ch := b.Subscribe(4)
	for i := 0; i < 20; i++ {
		b.Dispatch(Event{Type: EventTextDelta, Text: "x"})
	}
	// 录制器 20 条全在。
	if got := len(rec.FilterTypes(EventTextDelta)); got != 20 {
		t.Fatalf("录制器不应丢事件: %d", got)
	}
	if b.Dropped() != 4 {
		t.Fatalf("直播应丢弃 4 条, got %d", b.Dropped())
	}
	// 缓冲内前 16 条可读。
	drained := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				drained = -1
			} else {
				drained++
			}
			continue
		default:
		}
		break
	}
	if drained != 16 {
		t.Fatalf("直播应缓冲 16 条, got %d", drained)
	}
	b.Unsubscribe(0)
	// 注销后再派发不应 panic。
	b.Dispatch(Event{Type: EventTurnEnd})
}

func TestBroadcasterUnsubscribeTwice(t *testing.T) {
	b := NewBroadcaster()
	id, _ := b.Subscribe(4)
	b.Unsubscribe(id)
	b.Unsubscribe(id) // 幂等
}

func TestTurnInterruptedEventCarriesError(t *testing.T) {
	prov := &scriptProvider{
		steps:    []llm.StreamResult{textResult("never")},
		failures: []error{llm.NewAPIError("auth", 401, "expired", 0)},
	}
	mem := &sliceMemory{msgs: []llm.Message{{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: "x"}}}}}
	rec := &MemoryRecorder{}
	_, _ = RunTurn(context.Background(), RunTurnInput{TurnID: "tE", Provider: prov, Memory: mem, Events: rec})

	interrupted := rec.FilterTypes(EventTurnInterrupt)
	if len(interrupted) != 1 {
		t.Fatalf("应有 1 条中断事件: %d", len(interrupted))
	}
	if interrupted[0].StopReason != TurnError || interrupted[0].Error == "" {
		t.Fatalf("中断事件应带原因与错误: %+v", interrupted[0])
	}
	if interrupted[0].TurnID != "tE" {
		t.Fatalf("turn_id 缺失: %+v", interrupted[0])
	}
}

func TestToolResultJSONShape(t *testing.T) {
	r := ToolResult{Output: "data", IsError: true, Truncated: true}
	if !contains(r.ResultJSON(), `"error":true`) || !contains(r.ResultJSON(), `"truncated":true`) || !contains(r.ResultJSON(), `"output":"data"`) {
		t.Fatalf("JSON 形态错误: %s", r.ResultJSON())
	}
}
