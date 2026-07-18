package agentbridge

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveExecutableUsesOverride(t *testing.T) {
	name := "grok"
	if runtime.GOOS == "windows" {
		name = "grok.exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveExecutable(path, "")
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := filepath.Abs(path)
	if resolved != expected {
		t.Fatalf("resolved = %q, want %q", resolved, expected)
	}
}
