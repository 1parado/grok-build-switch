package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandSkillPromptWithSkillsSyntax(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "demo", "SKILL.md"), []byte("Use the demo workflow."), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := expandSkillPromptWithRoots("/skills demo explain the result", []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "/skills demo") || !strings.Contains(got, "Use the demo workflow.") || !strings.Contains(got, "explain the result") {
		t.Fatalf("unexpected expanded prompt: %q", got)
	}
}

func TestExpandSkillPromptSupportsShortSyntaxAndNestedSkill(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested-wrapper", "nested-wrapper")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "SKILL.md"), []byte("Nested instructions"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := expandSkillPromptWithRoots("/nested-wrapper\ncontinue", []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Nested instructions") || !strings.Contains(got, "continue") {
		t.Fatalf("unexpected nested expansion: %q", got)
	}
}

func TestExpandSkillPromptLeavesUnknownCommandsUntouched(t *testing.T) {
	const input = "/skills does-not-exist do not rewrite this"
	got, err := expandSkillPromptWithRoots(input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != input {
		t.Fatalf("unknown skill changed prompt: %q", got)
	}
}

func TestExpandSkillPromptFallsBackWhenHigherPrioritySkillIsIncomplete(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	if err := os.Mkdir(filepath.Join(first, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(second, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "demo", "SKILL.md"), []byte("Fallback instructions"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := expandSkillPromptWithRoots("/skills demo", []string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Fallback instructions") {
		t.Fatalf("expected fallback skill content, got %q", got)
	}
}
