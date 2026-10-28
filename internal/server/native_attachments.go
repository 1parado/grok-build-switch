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
func nativeAttachmentParts(a agentbridge.Attachment, roots []string) []llm.ContentPart {
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
		return nativePathParts(kind, name, path, mime, roots)
	}
	return nativeInlineParts(kind, name, mime, a.Data, a.Text)
}

func nativePathParts(kind, name, path, mime string, roots []string) []llm.ContentPart {
	line := llm.TextPart{Text: fmt.Sprintf("[附件 %s: %s]", name, path)}
	real, ok := nativeRealPath(path, roots)
	if !ok {
		return []llm.ContentPart{line}
	}
	switch kind {
	case "image":
		if part, ok := nativeImageFromPath(real, mime); ok {
			return []llm.ContentPart{part}
		}
		return []llm.ContentPart{line}
	case "text_file", "path":
		var parts []llm.ContentPart
		if kind == "text_file" || nativeTextyPath(real, mime) {
			if snippet, err := nativeTextSnippet(real); err == nil && snippet != "" {
				parts = append(parts, llm.TextPart{Text: fmt.Sprintf("【附件：%s】\n%s", name, snippet)})
			}
		}
		parts = append(parts, line)
		return parts
	default:
		return []llm.ContentPart{line}
	}
}

// nativeRealPath 只接受落在允许根目录里的真实路径。根目录为空、
// 目标不存在，或符号链接指向根外时，调用方退回路径文本，不读文件。
func nativeRealPath(path string, roots []string) (string, bool) {
	if len(roots) == 0 {
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	real = filepath.Clean(real)
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rootReal := filepath.Clean(rootAbs)
		if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
			rootReal = filepath.Clean(resolved)
		}
		if nativeWithin(rootReal, real) {
			return real, true
		}
	}
	return "", false
}

func nativeWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
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
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() <= 0 || st.Size() > maxUploadImageBytes {
		return llm.ImagePart{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || int64(len(data)) > maxUploadImageBytes {
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
