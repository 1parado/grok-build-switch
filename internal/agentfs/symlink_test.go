package agentfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardRejectsSymlinkThatLeavesTheSandbox(t *testing.T) {
	cwd := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cwd, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("symlink: " + err.Error())
	}

	env := Env{Cwd: cwd}
	if _, err := env.Guard("link.txt"); err == nil {
		t.Fatal("guard allowed a symlink out of the sandbox")
	}
	if got, err := env.ReadText(link, 0); err == nil && strings.Contains(got, "secret") {
		t.Fatalf("read followed the symlink and returned %q", got)
	}
}

func TestGuardRejectsPathThroughSymlinkedDirectory(t *testing.T) {
	cwd := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(cwd, "out")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlink: " + err.Error())
	}

	env := Env{Cwd: cwd}
	if _, err := env.Guard(filepath.Join("out", "new.txt")); err == nil {
		t.Fatal("guard allowed a path through a directory symlink")
	}
	target := filepath.Join(link, "new.txt")
	if err := env.WriteText(target, "pwned"); err == nil {
		if _, statErr := os.Stat(filepath.Join(outside, "new.txt")); statErr == nil {
			t.Fatal("write escaped through a directory symlink")
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("outside file was created")
	}
}

func TestGuardAllowsSymlinkInsideTheSandbox(t *testing.T) {
	cwd := t.TempDir()
	real := filepath.Join(cwd, "real.txt")
	if err := os.WriteFile(real, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cwd, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink: " + err.Error())
	}

	env := Env{Cwd: cwd}
	abs, err := env.Guard("link.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := env.ReadText(abs, 0)
	if err != nil || got != "ok" {
		t.Fatalf("inside symlink read = %q, %v", got, err)
	}
}
