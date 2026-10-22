package switcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupKeepsBothSnapshotsFromTheSameSecond(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	backups := filepath.Join(dir, "backups")
	if err := os.WriteFile(configPath, []byte("version-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	previous := backupNow
	backupNow = func() time.Time { return fixed }
	t.Cleanup(func() { backupNow = previous })

	s := &Switcher{ConfigPath: configPath, BackupsDir: backups}
	first, err := s.Backup()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("version-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := s.Backup()
	if err != nil {
		t.Fatal(err)
	}
	if first.File == second.File {
		t.Fatalf("second backup reused %s and overwrote the first snapshot", first.File)
	}
	assertFile(t, first.Path, "version-a\n")
	assertFile(t, second.Path, "version-b\n")
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
