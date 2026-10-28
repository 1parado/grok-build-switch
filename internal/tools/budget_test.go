package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"grok_switch/internal/agentfs"
	"grok_switch/internal/llm"
)

type budgetTool struct{ text string }

func (budgetTool) Name() string { return "budget" }

func (budgetTool) Schema() map[string]any { return nil }

func (budgetTool) Doc() string { return "" }

func (t budgetTool) Execute(context.Context, json.RawMessage, agentfs.Env) ToolOutput {
	return ToolOutput{Text: t.text}
}

func TestExecuteToolCutsOnUTF8Boundary(t *testing.T) {
	// One leading byte shifts a 3-byte rune onto the 30000-byte budget.
	body := "x" + strings.Repeat("你", ResultBudget)
	reg := NewRegistry(nil)
	reg.Register(budgetTool{text: body})
	out := reg.ExecuteTool(context.Background(), llm.ToolCall{Name: "budget"})
	if !out.Truncated {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(out.Text) {
		t.Fatal("budget cut left invalid UTF-8")
	}
	if !strings.HasSuffix(out.Text, "[输出超过预算被截断]") {
		t.Fatalf("missing budget notice: %q", out.Text[len(out.Text)-40:])
	}
}
