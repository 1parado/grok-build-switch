//go:build unix

package agentfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesFinalSymlinkInsideSandbox(t *testing.T) {
	cwd := t.TempDir()
	real := filepath.Join(cwd, "real.txt")
	if err := os.WriteFile(real, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cwd, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink: " + err.Error())
	}
	env := Env{Cwd: cwd}
	if err := env.WriteText(link, "new"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("write followed the final symlink")
	}
	got, err := os.ReadFile(link)
	if err != nil || string(got) != "new" {
		t.Fatalf("link content = %q, %v", got, err)
	}
	orig, err := os.ReadFile(real)
	if err != nil || string(orig) != "old" {
		t.Fatalf("target content = %q, %v", orig, err)
	}
}

func TestWriteFollowsInsideDirectorySymlink(t *testing.T) {
	cwd := t.TempDir()
	realDir := filepath.Join(cwd, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cwd, "sub")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skip("symlink: " + err.Error())
	}
	env := Env{Cwd: cwd}
	if err := env.WriteText(filepath.Join(link, "a.txt"), "hi"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(realDir, "a.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("target content = %q, %v", got, err)
	}
}

func TestListDirRejectsSymlinkedDirectoryOutside(t *testing.T) {
	cwd := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cwd, "out")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlink: " + err.Error())
	}
	env := Env{Cwd: cwd}
	if names, err := env.ListDir(link); err == nil {
		t.Fatalf("list escaped: %v", names)
	}
}
