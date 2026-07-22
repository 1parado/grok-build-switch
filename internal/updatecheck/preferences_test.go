package updatecheck

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPreferenceStoreNotifiesOnceAndSkipsOneVersion(t *testing.T) {
	store := NewPreferenceStore(filepath.Join(t.TempDir(), "update_state.json"))
	claimed, err := store.ClaimNotification("v0.8.0")
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v; want true, nil", claimed, err)
	}
	claimed, err = store.ClaimNotification("v0.8.0")
	if err != nil || claimed {
		t.Fatalf("second claim = %v, %v; want false, nil", claimed, err)
	}
	if err := store.Skip("v0.9.0"); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimNotification("v0.9.0")
	if err != nil || claimed {
		t.Fatalf("skipped claim = %v, %v; want false, nil", claimed, err)
	}
	claimed, err = store.ClaimNotification("v0.10.0")
	if err != nil || !claimed {
		t.Fatalf("newer claim = %v, %v; want true, nil", claimed, err)
	}
	prefs, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	if prefs.NotifiedVersion != "v0.10.0" || prefs.SkippedVersion != "" {
		t.Fatalf("unexpected preferences: %#v", prefs)
	}
}

func TestPreferenceStoreRecoversCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update_state.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewPreferenceStore(path)
	prefs, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	if prefs != (Preferences{}) {
		t.Fatalf("recovered preferences = %#v, want empty", prefs)
	}
	matches, err := filepath.Glob(path + ".corrupt-*.bak")
	if err != nil || len(matches) != 1 {
		t.Fatalf("corrupt backup matches = %v, %v", matches, err)
	}
}
