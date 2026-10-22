package server

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateTextServerKeepsUTF8(t *testing.T) {
	got := truncateTextServer("你好世界", 4)
	if got != "你…" {
		t.Fatalf("truncateTextServer = %q, want 你…", got)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated error text is not valid UTF-8")
	}
	if truncateTextServer("hello", 3) != "hel…" {
		t.Fatalf("ascii truncate = %q", truncateTextServer("hello", 3))
	}
	if truncateTextServer("  hello  ", 10) != "hello" {
		t.Fatalf("short text = %q", truncateTextServer("  hello  ", 10))
	}
}
