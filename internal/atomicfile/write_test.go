package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := Write(path, []byte("old"), Options{DirMode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new"), Options{DirMode: 0o755, FileMode: 0o600, Sync: true}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q", got)
	}
	assertNoBackup(t, dir)
}

func TestReplaceRestoresDestinationWhenPublishFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := writeTemp(t, dir, "new")

	calls := 0
	prev := renameFile
	renameFile = func(oldPath, newPath string) error {
		calls++
		// 1: temp -> dest fails, which is what Windows reports when dest exists.
		// 2: dest -> backup succeeds.
		// 3: temp -> dest fails again, so the backup must be restored.
		// 4: backup -> dest.
		if calls == 1 || calls == 3 {
			return errors.New("injected rename failure")
		}
		return os.Rename(oldPath, newPath)
	}
	t.Cleanup(func() { renameFile = prev })

	if err := replace(tmp, path); err == nil {
		t.Fatal("expected publish failure")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("destination = %q, want old", got)
	}
	assertNoBackup(t, dir)
}

func TestReplacePublishesAfterDestinationIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := writeTemp(t, dir, "new")

	calls := 0
	prev := renameFile
	renameFile = func(oldPath, newPath string) error {
		calls++
		if calls == 1 {
			return errors.New("destination exists")
		}
		return os.Rename(oldPath, newPath)
	}
	t.Cleanup(func() { renameFile = prev })

	if err := replace(tmp, path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("destination = %q", got)
	}
	assertNoBackup(t, dir)
}

func writeTemp(t *testing.T, dir, content string) string {
	t.Helper()
	tmp, err := os.CreateTemp(dir, "new-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	return tmp.Name()
}

func assertNoBackup(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".bak") {
			t.Fatalf("backup left behind: %s", entry.Name())
		}
	}
}
