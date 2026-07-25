//go:build windows

package folderpick

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodePickerOutputBase64Chinese(t *testing.T) {
	path := `E:\本地项目\测试目录`
	b64 := base64.StdEncoding.EncodeToString([]byte(path))
	got, err := decodePickerOutput([]byte(pathB64Prefix + b64))
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("got %q want %q", got, path)
	}
}

func TestDecodePickerOutputPlainASCII(t *testing.T) {
	got, err := decodePickerOutput([]byte("  E:\\local_switch\\grok_switch  \n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != `E:\local_switch\grok_switch` {
		t.Fatalf("got %q", got)
	}
}

func TestDecodePickerOutputEmpty(t *testing.T) {
	got, err := decodePickerOutput(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestDecodePickerOutputInvalidBase64(t *testing.T) {
	if _, err := decodePickerOutput([]byte(pathB64Prefix + "!!!")); err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalizeStartChineseDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "中文目录")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got := normalizeStart(dir)
	if filepath.Base(got) != "中文目录" {
		t.Fatalf("normalizeStart(%q)=%q", dir, got)
	}
	info, err := os.Stat(got)
	if err != nil || !info.IsDir() {
		t.Fatalf("normalized path not a dir: %q err=%v", got, err)
	}
}
