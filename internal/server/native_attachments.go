package server

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"grok_switch/internal/agentbridge"
	"grok_switch/internal/llm"
)

// nativeAttachTextRunes 与 ACP 桥一致：同一份上传文本，两边都只送前 20000 个字符。
const nativeAttachTextRunes = 20000

// nativeAttachmentParts 在本机读取路径附件。浏览器只传路径，不读的话模型只能看见文件名。
func nativeAttachmentParts(a agentbridge.Attachment) []llm.ContentPart {
	kind := strings.ToLower(strings.TrimSpace(a.Kind))
	path := strings.TrimSpace(a.Path)
	name := strings.TrimSpace(a.Name)
	mime := strings.TrimSpace(a.MimeType)
	if path != "" {
		if name == "" {
			name = filepath.Base(path)
		}
		if kind == "" {
			kind = nativeAttachKind(path, mime)
		}
		return nativePathParts(kind, name, path, mime)
	}
	return nativeInlineParts(kind, name, mime, a.Data, a.Text)
}

func nativePathParts(kind, name, path, mime string) []llm.ContentPart {
	line := llm.TextPart{Text: fmt.Sprintf("[附件 %s: %s]", name, path)}
	switch kind {
	case "image":
		if part, ok := nativeImageFromPath(path, mime); ok {
			return []llm.ContentPart{part}
		}
		return []llm.ContentPart{line}
	case "text_file", "path":
		var parts []llm.ContentPart
		if kind == "text_file" || nativeTextyPath(path, mime) {
			if snippet, err := nativeTextSnippet(path); err == nil && snippet != "" {
				parts = append(parts, llm.TextPart{Text: fmt.Sprintf("【附件：%s】\n%s", name, snippet)})
			}
		}
		parts = append(parts, line)
		return parts
	default:
		return []llm.ContentPart{line}
	}
}

func nativeInlineParts(kind, name, mime, data, text string) []llm.ContentPart {
	switch kind {
	case "image":
		if data == "" {
			return nil
		}
		return []llm.ContentPart{llm.ImagePart{Data: data, MimeType: mime}}
	case "text_file":
		snippet := strings.TrimSpace(text)
		if snippet == "" {
			return nil
		}
		if len([]rune(snippet)) > nativeAttachTextRunes {
			runes := []rune(snippet)
			snippet = string(runes[:nativeAttachTextRunes]) + "\n…（已截断，仅发送前 20000 字符）"
		}
		if name != "" {
			return []llm.ContentPart{llm.TextPart{Text: fmt.Sprintf("【附件：%s】\n%s", name, snippet)}}
		}
		return []llm.ContentPart{llm.TextPart{Text: snippet}}
	default:
		return nil
	}
}

func nativeImageFromPath(path, mime string) (llm.ImagePart, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return llm.ImagePart{}, false
	}
	if mime == "" {
		mime = nativeImageMime(filepath.Ext(path))
	}
	if mime == "" {
		mime = "image/png"
	}
	return llm.ImagePart{MimeType: mime, Data: base64.StdEncoding.EncodeToString(data)}, true
}

func nativeTextSnippet(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	const maxBytes = nativeAttachTextRunes * 4
	truncated := false
	if len(data) > maxBytes {
		data = trimPartialUTF8(data[:maxBytes])
		truncated = true
	}
	text := string(data)
	runes := []rune(text)
	if len(runes) > nativeAttachTextRunes {
		text = string(runes[:nativeAttachTextRunes])
		truncated = true
	}
	if truncated {
		text += "\n…（已截断，仅发送前 20000 字符）"
	}
	return strings.TrimSpace(text), nil
}

// trimPartialUTF8 去掉字节上限切在半个字符上留下的尾巴。
func trimPartialUTF8(data []byte) []byte {
	for len(data) > 0 {
		r, size := utf8.DecodeLastRune(data)
		if r != utf8.RuneError || size > 1 {
			return data
		}
		data = data[:len(data)-1]
	}
	return data
}

func nativeAttachKind(path, mime string) string {
	mime = strings.ToLower(mime)
	if strings.HasPrefix(mime, "image/") {
		return "image"
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".avif":
		return "image"
	}
	if nativeTextyPath(path, mime) {
		return "text_file"
	}
	return "path"
}

func nativeTextyPath(path, mime string) bool {
	mime = strings.ToLower(mime)
	if strings.HasPrefix(mime, "text/") || strings.Contains(mime, "json") || strings.Contains(mime, "xml") || strings.Contains(mime, "javascript") {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".txt", ".md", ".markdown", ".json", ".csv", ".tsv", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx",
		".go", ".py", ".rb", ".java", ".c", ".cc", ".cpp", ".h", ".hpp", ".rs", ".php", ".sh", ".bash",
		".zsh", ".ps1", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".xml", ".html", ".htm",
		".css", ".scss", ".less", ".sql", ".log", ".env":
		return true
	}
	return false
}

func nativeImageMime(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".avif":
		return "image/avif"
	default:
		return ""
	}
}
