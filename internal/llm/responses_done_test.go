package llm

import "testing"

func TestResponsesDoneFillsCallIDOnExistingOutput(t *testing.T) {
	s := newResponsesStreamState()
	events := []string{
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","name":"read","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_a","name":"read","arguments":"{\"path\":\"a.go\"}"}}`,
	}
	for _, ev := range events {
		if err := s.handle([]byte(ev), func(StreamedPart) {}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1: %+v", len(s.msg.ToolCalls), s.msg.ToolCalls)
	}
	got := s.msg.ToolCalls[0]
	if got.ID != "call_a" || got.Name != "read" || string(got.Arguments) != `{"path":"a.go"}` {
		t.Fatalf("call = id %q name %q args %s", got.ID, got.Name, got.Arguments)
	}
}

func TestResponsesDoneUpdatesTheMatchingOutputIndex(t *testing.T) {
	s := newResponsesStreamState()
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"read"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","name":"glob"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_b","name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}`,
	}
	for _, ev := range events {
		if err := s.handle([]byte(ev), func(StreamedPart) {}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.msg.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2: %+v", len(s.msg.ToolCalls), s.msg.ToolCalls)
	}
	if s.msg.ToolCalls[0].Name != "read" || s.msg.ToolCalls[0].ID != "" {
		t.Fatalf("first call changed: %+v", s.msg.ToolCalls[0])
	}
	got := s.msg.ToolCalls[1]
	if got.ID != "call_b" || got.Name != "glob" || string(got.Arguments) != `{"pattern":"*.go"}` {
		t.Fatalf("second call = id %q name %q args %s", got.ID, got.Name, got.Arguments)
	}
}
