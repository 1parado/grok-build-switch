package grokpool

import (
	"os"
	"testing"
)

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "grokpool-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removeTree(dir); err != nil {
			t.Errorf("remove temp dir: %v", err)
		}
	})
	return dir
}

func newManager(t *testing.T, dir string) *Manager {
	t.Helper()
	manager, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	return manager
}
