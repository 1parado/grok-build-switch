package tools

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepReportsCapWhenTheNextFileIsUnsearched(t *testing.T) {
	env := testEnv(t)
	if err := env.WriteText(filepath.Join(env.Cwd, "a.txt"), "hit-a\n"); err != nil {
		t.Fatal(err)
	}
	if err := env.WriteText(filepath.Join(env.Cwd, "b.txt"), "hit-b\n"); err != nil {
		t.Fatal(err)
	}
	out := runTool(t, GrepTool{}, `{"pattern":"hit","max_items":1}`, env)
	if out.IsError {
		t.Fatal(out.Text)
	}
	if strings.Contains(out.Text, "hit-b") {
		t.Fatalf("second file should not be shown:\n%s", out.Text)
	}
	if !strings.Contains(out.Text, "达到 1 条上限") {
		t.Fatalf("missing cap notice:\n%s", out.Text)
	}
}

func TestGrepOmitsCapNoticeWhenTheWalkIsFinished(t *testing.T) {
	env := testEnv(t)
	if err := env.WriteText(filepath.Join(env.Cwd, "a.txt"), "hit-a\n"); err != nil {
		t.Fatal(err)
	}
	out := runTool(t, GrepTool{}, `{"pattern":"hit","max_items":1}`, env)
	if out.IsError {
		t.Fatal(out.Text)
	}
	if strings.Contains(out.Text, "达到 1 条上限") {
		t.Fatalf("a finished walk should not claim truncation:\n%s", out.Text)
	}
}

func TestGrepReportsCapWhenAnotherLineRemains(t *testing.T) {
	env := testEnv(t)
	if err := env.WriteText(filepath.Join(env.Cwd, "a.txt"), "hit-a\nhit-b\n"); err != nil {
		t.Fatal(err)
	}
	out := runTool(t, GrepTool{}, `{"pattern":"hit","max_items":1}`, env)
	if out.IsError {
		t.Fatal(out.Text)
	}
	if !strings.Contains(out.Text, "达到 1 条上限") {
		t.Fatalf("missing cap notice:\n%s", out.Text)
	}
}

func TestGrepDoesNotMatchTheTrailingNewline(t *testing.T) {
	env := testEnv(t)
	if err := env.WriteText(filepath.Join(env.Cwd, "a.txt"), "abc\n"); err != nil {
		t.Fatal(err)
	}
	out := runTool(t, GrepTool{}, `{"pattern":"^$"}`, env)
	if out.IsError {
		t.Fatal(out.Text)
	}
	if !strings.Contains(out.Text, "无匹配") {
		t.Fatalf("trailing newline was reported as a blank line:\n%s", out.Text)
	}
}
