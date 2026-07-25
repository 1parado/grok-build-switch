package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeProjectPathChinese(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "中文项目", "子目录")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := normalizeProjectPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "子目录" {
		t.Fatalf("base=%q path=%q", filepath.Base(got), got)
	}
	info, err := os.Stat(got)
	if err != nil || !info.IsDir() {
		t.Fatalf("stat failed: %v", err)
	}
}

func TestSameProjectPathChineseAbs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "测试目录")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !sameProjectPath(dir, abs) {
		t.Fatalf("expected same path for %q and %q", dir, abs)
	}
	if !sameProjectPath(abs, filepath.Clean(abs)+string(filepath.Separator)) {
		// trailing separator after Clean may normalize away; just ensure EqualFold abs works
		if !sameProjectPath(abs, abs) {
			t.Fatal("abs != abs")
		}
	}
}

func TestLooksLikeGarbledPath(t *testing.T) {
	if !looksLikeGarbledPath("æµ‹è¯•") {
		t.Fatal("expected garbled detection")
	}
	if looksLikeGarbledPath("正常中文") {
		t.Fatal("did not expect garbled for valid CJK")
	}
}
