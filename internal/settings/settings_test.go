package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateRejectsInvalidPort(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "settings.json"))
	next := Default()
	next.Port = 70000
	if _, err := store.Update(next); err == nil {
		t.Fatal("Update() accepted an invalid port")
	} else if !IsValidationError(err) {
		t.Fatalf("Update() error = %T %v, want ValidationError", err, err)
	}
}

func TestGetRecoversCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := NewStore(path).Get()
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != Default().Port || got.ActualPort != Default().ActualPort {
		t.Fatalf("recovered settings = %#v", got)
	}
	assertOneCorruptBackup(t, path)
}

func TestGetRepairsInvalidPersistedPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	data := []byte(`{"port":70000,"actual_port":70000,"theme":"light"}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := NewStore(path).Get()
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != Default().Port || got.ActualPort != Default().Port {
		t.Fatalf("repaired settings = %#v", got)
	}
	assertOneCorruptBackup(t, path)
}

func assertOneCorruptBackup(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(path + ".corrupt-*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("corrupt backups = %#v, want one", matches)
	}
}

func TestNormalizeThemeAcceptsThreeStates(t *testing.T) {
	cases := map[string]string{
		"light": "light",
		"dark":  "dark",
		"auto":  "auto",
		"":      "light",
		"blue":  "light",
		"DARK":  "light",
	}
	for input, want := range cases {
		got := normalize(Settings{Theme: input}).Theme
		if got != want {
			t.Errorf("normalize(Settings{Theme:%q}).Theme = %q, want %q", input, got, want)
		}
	}
}
