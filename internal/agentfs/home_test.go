package agentfs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveSlashHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatal(err)
	}
	env := Env{Cwd: t.TempDir()}
	got, err := env.Resolve("~/.grok-switch-home")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".grok-switch-home")
	if got != want {
		t.Fatalf("Resolve = %s, want %s", got, want)
	}
	got, err = env.Resolve("~")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(home) {
		t.Fatalf("Resolve(~) = %s, want %s", got, home)
	}
}

func TestResolveWindowsBackslashHome(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("backslash home paths are Windows paths")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatal(err)
	}
	env := Env{Cwd: t.TempDir()}
	got, err := env.Resolve(`~\.grok-switch-home`)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".grok-switch-home")
	if got != want {
		t.Fatalf("Resolve = %s, want %s", got, want)
	}
}

func TestResolveBackslashTildeStaysLiteralOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a separator on Windows")
	}
	cwd := t.TempDir()
	env := Env{Cwd: cwd}
	got, err := env.Resolve(`~\foo`)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cwd, `~\foo`)
	if got != want {
		t.Fatalf("Resolve = %s, want %s", got, want)
	}
}
