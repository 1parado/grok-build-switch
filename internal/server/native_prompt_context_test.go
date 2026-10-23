package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProjectContextKeepsNearestFile(t *testing.T) {
	grand := t.TempDir()
	parent := filepath.Join(grand, "parent")
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two ancestor files fill the 16 KiB budget exactly, so a farthest-first
	// cut drops the project the user actually opened.
	if err := os.WriteFile(filepath.Join(grand, "AGENTS.md"), []byte(strings.Repeat("G", 8000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "AGENTS.md"), []byte(strings.Repeat("P", 8000)), 0o644); err != nil {
		t.Fatal(err)
	}
	const nearest = "CHILD-RULE do not use the network"
	if err := os.WriteFile(filepath.Join(child, "AGENTS.md"), []byte(nearest), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadProjectContext(child)
	if !strings.Contains(got, nearest) {
		t.Fatalf("nearest project instructions were dropped:\n%s", got)
	}
	parentIdx := strings.Index(got, strings.Repeat("P", 80))
	childIdx := strings.Index(got, nearest)
	if parentIdx >= 0 && childIdx >= 0 && childIdx < parentIdx {
		t.Fatalf("nearest instructions should follow the ancestor file:\n%s", got)
	}
}
