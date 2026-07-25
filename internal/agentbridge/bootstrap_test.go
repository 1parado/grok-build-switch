package agentbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildHistoryBootstrapLimits(t *testing.T) {
	msgs := make([]HistoryMessage, 0, 30)
	for i := 0; i < 30; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, HistoryMessage{Role: role, Content: "hello world content"})
	}
	text := BuildHistoryBootstrap(msgs)
	if text == "" {
		t.Fatal("expected bootstrap text")
	}
	for _, part := range []string{"会话连续性摘要", "### User", "摘要结束"} {
		if !strings.Contains(text, part) {
			t.Fatalf("missing %q in bootstrap", part)
		}
	}
}

func TestDropLastUserRewindIndex(t *testing.T) {
	if got := DropLastUserRewindIndex(0); got != 0 {
		t.Fatalf("0 -> %d", got)
	}
	if got := DropLastUserRewindIndex(1); got != 0 {
		t.Fatalf("1 -> %d", got)
	}
	if got := DropLastUserRewindIndex(3); got != 1 {
		t.Fatalf("3 -> %d", got)
	}
}

func TestCountUserTurns(t *testing.T) {
	n := CountUserTurns([]HistoryMessage{
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
		{Role: "tool", Content: "x"},
	})
	if n != 2 {
		t.Fatalf("count = %d", n)
	}
}

func TestBuildPromptBlocksPathText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks := buildPromptBlocks("hi", []Attachment{{Kind: "text_file", Path: path, Name: "note.txt"}})
	if len(blocks) == 0 {
		t.Fatal("expected blocks")
	}
	joined := ""
	for _, block := range blocks {
		if block.Text != nil {
			joined += block.Text.Text + "\n"
		}
	}
	if !strings.Contains(joined, "@") || !strings.Contains(joined, "hello attachment") {
		t.Fatalf("unexpected blocks text: %s", joined)
	}
	if !strings.Contains(joined, "hi") {
		t.Fatalf("missing user text: %s", joined)
	}
}
