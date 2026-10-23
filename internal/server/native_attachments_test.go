package server

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"grok_switch/internal/agentbridge"
	"grok_switch/internal/llm"
)

func TestBuildUserMessageReadsPathAttachments(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("文件里的句子"), 0o644); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "pic.png")
	png := []byte{0x89, 0x50, 0x4e, 0x47}
	if err := os.WriteFile(img, png, 0o644); err != nil {
		t.Fatal(err)
	}
	msg := buildUserMessage("看一下", []agentbridge.Attachment{
		{Kind: "text_file", Name: "note.txt", Path: note},
		{Kind: "image", Name: "pic.png", Path: img, MimeType: "image/png"},
	})

	var sawText, sawNotePath, sawImage bool
	for _, part := range msg.Parts {
		switch p := part.(type) {
		case llm.TextPart:
			if strings.Contains(p.Text, "文件里的句子") {
				sawText = true
			}
			if strings.Contains(p.Text, note) {
				sawNotePath = true
			}
			if strings.Contains(p.Text, img) {
				t.Fatalf("image path was sent as text after the pixels were read: %q", p.Text)
			}
		case llm.ImagePart:
			if p.MimeType != "image/png" || p.Data != base64.StdEncoding.EncodeToString(png) {
				t.Fatalf("image part = %#v", p)
			}
			sawImage = true
		}
	}
	if !sawText || !sawNotePath || !sawImage {
		t.Fatalf("uploaded files were not in the prompt: %#v", msg.Parts)
	}
}

func TestBuildUserMessageReadsTextyPathAndInlineText(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(note, []byte("# 标题\n正文"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := buildUserMessage("", []agentbridge.Attachment{
		{Kind: "path", Name: "notes.md", Path: note},
		{Kind: "text_file", Name: "paste.txt", Text: "粘贴的正文"},
	})
	var sawFile, sawPaste bool
	for _, part := range msg.Parts {
		p, ok := part.(llm.TextPart)
		if !ok {
			continue
		}
		if strings.Contains(p.Text, "正文") && strings.Contains(p.Text, "【附件：notes.md】") {
			sawFile = true
		}
		if strings.Contains(p.Text, "粘贴的正文") {
			sawPaste = true
		}
	}
	if !sawFile || !sawPaste {
		t.Fatalf("text attachments missing: %#v", msg.Parts)
	}
}

func TestBuildUserMessageCutsAttachmentOnRuneBoundary(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "long.txt")
	// 19999 four-byte runes, one complete 中, then a second 中 that the
	// 80000-byte cap splits. The kept text stays within 20000 runes, so
	// only the byte cut can leave a partial rune behind.
	body := append(bytes.Repeat([]byte("😀"), 19999), []byte("中中")...)
	if err := os.WriteFile(note, body, 0o644); err != nil {
		t.Fatal(err)
	}
	msg := buildUserMessage("", []agentbridge.Attachment{
		{Kind: "text_file", Name: "long.txt", Path: note},
	})
	var snippet string
	for _, part := range msg.Parts {
		p, ok := part.(llm.TextPart)
		if ok && strings.Contains(p.Text, "【附件：long.txt】") {
			snippet = p.Text
		}
	}
	if snippet == "" || !utf8.ValidString(snippet) || !strings.Contains(snippet, "已截断") {
		t.Fatalf("snippet = %q", snippet)
	}
	if strings.Count(snippet, "中") != 1 || strings.ContainsRune(snippet, utf8.RuneError) {
		t.Fatalf("byte cap split a rune: %q", snippet[len(snippet)-40:])
	}
}

func TestBuildUserMessageFallsBackWhenImageIsMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.png")
	msg := buildUserMessage("看图", []agentbridge.Attachment{
		{Kind: "image", Name: "gone.png", Path: missing, MimeType: "image/png"},
		{Kind: "image", Data: "aaaa", MimeType: "image/png"},
	})
	var sawPath, sawInline bool
	for _, part := range msg.Parts {
		switch p := part.(type) {
		case llm.TextPart:
			if strings.Contains(p.Text, missing) {
				sawPath = true
			}
		case llm.ImagePart:
			if p.Data == "aaaa" {
				sawInline = true
			}
			if p.Data != "" && p.Data != "aaaa" {
				t.Fatalf("missing file became an image part: %#v", p)
			}
		}
	}
	if !sawPath || !sawInline {
		t.Fatalf("parts = %#v", msg.Parts)
	}
}
