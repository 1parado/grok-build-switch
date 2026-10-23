package remoteaccess

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPairingIsSingleUse(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "remote_access.json"))
	first, err := store.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionToken == "" || len(first.PairingCode) != 8 {
		t.Fatalf("unexpected snapshot: %#v", first)
	}

	token, ok, err := store.ConsumePairing(first.PairingCode)
	if err != nil || !ok || token != first.SessionToken {
		t.Fatalf("first consume = token %q, ok %v, err %v", token, ok, err)
	}
	if _, ok, err := store.ConsumePairing(first.PairingCode); err != nil || ok {
		t.Fatalf("second consume = ok %v, err %v, want rejected", ok, err)
	}
}

func TestResetSessionsRevokesExistingToken(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "remote_access.json"))
	before, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ResetSessions(); err != nil {
		t.Fatal(err)
	}
	authorized, err := store.Authorized(before.SessionToken)
	if err != nil {
		t.Fatal(err)
	}
	if authorized {
		t.Fatal("old session token remained authorized after reset")
	}
}

func TestResetSessionsDropsCachedTokenWhenMtimeStays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote_access.json")
	store := NewStore(path)
	before, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := store.Authorized(before.SessionToken)
	if err != nil || !authorized {
		t.Fatalf("prime cache: authorized=%v err=%v", authorized, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ResetSessions(); err != nil {
		t.Fatal(err)
	}
	// A new token is the same length, so the file size does not change.
	// Filesystems that do not advance mtime would keep the cache hit.
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	authorized, err = store.Authorized(before.SessionToken)
	if err != nil {
		t.Fatal(err)
	}
	if authorized {
		t.Fatal("revoked session token stayed authorized from the mtime cache")
	}
	after, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	authorized, err = store.Authorized(after.SessionToken)
	if err != nil || !authorized {
		t.Fatalf("new token after reset: authorized=%v err=%v", authorized, err)
	}
}
