package tools

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"grok_switch/internal/agentfs"
)

func TestPrefixAndSuffixStayOnRuneBoundaries(t *testing.T) {
	const text = "你好世界"
	if got := prefixText(text, 4); got != "你" {
		t.Fatalf("prefixText = %q, want 你", got)
	}
	if got := suffixText(text, 4); got != "界" {
		t.Fatalf("suffixText = %q, want 界", got)
	}
	if prefixText("你", 2) != "" || suffixText("你", 2) != "" {
		t.Fatal("a rune longer than the budget must be omitted")
	}
	if prefixText("hello", 3) != "hel" || suffixText("hello", 3) != "llo" {
		t.Fatal("ASCII cuts must stay byte-accurate")
	}
	if prefixText("hello", 0) != "" || suffixText("", 5) != "" {
		t.Fatal("empty budget or empty input")
	}
	if prefixText(text, len(text)) != text || suffixText(text, len(text)+1) != text {
		t.Fatal("a string that already fits must be unchanged")
	}
}

func TestReadTruncatesLongLineOnRuneBoundary(t *testing.T) {
	env := testEnv(t)
	line := strings.Repeat("你", 700)
	path := filepath.Join(env.Cwd, "wide.txt")
	if err := env.WriteText(path, line+"\n"); err != nil {
		t.Fatal(err)
	}
	out := runTool(t, ReadTool{}, `{"path":"wide.txt"}`, env)
	if out.IsError {
		t.Fatal(out.Text)
	}
	if !utf8.ValidString(out.Text) {
		t.Fatal("read output is not valid UTF-8")
	}
	if !strings.Contains(out.Text, "…[行截断]") {
		t.Fatalf("missing truncation marker: %s", out.Text)
	}
	if strings.ContainsRune(out.Text, utf8.RuneError) {
		t.Fatal("read output contains a replacement rune")
	}
}

func TestGrepTruncatesLongLineOnRuneBoundary(t *testing.T) {
	env := testEnv(t)
	line := "hit" + strings.Repeat("你", 200)
	path := filepath.Join(env.Cwd, "wide.txt")
	if err := env.WriteText(path, "before\n"+line+"\nafter\n"); err != nil {
		t.Fatal(err)
	}
	out := runTool(t, GrepTool{}, `{"pattern":"hit","path":"wide.txt","context":1}`, env)
	if out.IsError {
		t.Fatal(out.Text)
	}
	if !utf8.ValidString(out.Text) || strings.ContainsRune(out.Text, utf8.RuneError) {
		t.Fatalf("grep output is not valid UTF-8: %q", out.Text)
	}
	if !strings.Contains(out.Text, "…") {
		t.Fatalf("missing truncation marker: %s", out.Text)
	}
}

func TestBashSpillTruncatesTailOnRuneBoundary(t *testing.T) {
	body := strings.Repeat("你", 10001)
	spilled, out := spillBashOutputWithEnv(body, agentfs.Env{})
	if !spilled {
		t.Fatal("oversized output was not truncated")
	}
	if !utf8.ValidString(out) || strings.ContainsRune(out, utf8.RuneError) {
		t.Fatal("bash tail is not valid UTF-8")
	}
	if !strings.HasPrefix(out, "你") {
		t.Fatalf("tail starts at %q", out[:12])
	}

	env := testEnv(t)
	spilled, out = spillBashOutputWithEnv(body, env)
	if !spilled || !utf8.ValidString(out) || strings.ContainsRune(out, utf8.RuneError) {
		t.Fatalf("spilled tail invalid: spilled=%v text=%q", spilled, out[:40])
	}
}
