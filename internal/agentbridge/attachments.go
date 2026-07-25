package agentbridge

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

const textFileMaxChars = 20000

// buildPromptBlocks turns the user's text + attachments into ACP content blocks.
// Path-backed attachments are read on the host so the browser never pushes multi-MB
// base64 over the WebSocket.
func buildPromptBlocks(text string, attachments []Attachment) []acp.ContentBlock {
	blocks := make([]acp.ContentBlock, 0, 1+len(attachments)*2)
	pathLines := make([]string, 0, len(attachments))
	var body strings.Builder
	if strings.TrimSpace(text) != "" {
		body.WriteString(strings.TrimSpace(text))
	}

	for _, a := range attachments {
		kind := strings.ToLower(strings.TrimSpace(a.Kind))
		path := strings.TrimSpace(a.Path)
		name := strings.TrimSpace(a.Name)
		mime := strings.TrimSpace(a.MimeType)

		if path != "" {
			if name == "" {
				name = filepath.Base(path)
			}
			if kind == "" {
				kind = kindFromPath(path, mime)
			}
			switch kind {
			case "image":
				if block, err := imageBlockFromPath(path, mime); err == nil {
					blocks = append(blocks, block)
				} else {
					// Fall back to @path so the agent can still open the file.
					pathLines = append(pathLines, "@"+path)
				}
			case "text_file", "path":
				if kind == "text_file" || isTextyPath(path, mime) {
					if snippet, err := readTextSnippet(path); err == nil && snippet != "" {
						blocks = append(blocks, acp.TextBlock(fmt.Sprintf("【附件：%s】\n%s", name, snippet)))
					}
				}
				pathLines = append(pathLines, "@"+path)
			default:
				pathLines = append(pathLines, "@"+path)
			}
			continue
		}

		switch kind {
		case "image":
			if a.Data != "" && mime != "" {
				blocks = append(blocks, acp.ImageBlock(a.Data, mime))
			}
		case "text_file":
			snippet := strings.TrimSpace(a.Text)
			if snippet == "" {
				continue
			}
			if len([]rune(snippet)) > textFileMaxChars {
				runes := []rune(snippet)
				snippet = string(runes[:textFileMaxChars]) + "\n…（已截断，仅发送前 20000 字符）"
			}
			if name != "" {
				blocks = append(blocks, acp.TextBlock(fmt.Sprintf("【附件：%s】\n%s", name, snippet)))
			} else {
				blocks = append(blocks, acp.TextBlock(snippet))
			}
		}
	}

	if len(pathLines) > 0 {
		if body.Len() > 0 {
			body.WriteString("\n\n")
		}
		body.WriteString(strings.Join(pathLines, "\n"))
	}
	if body.Len() > 0 {
		// Text body first so the model sees the user message before binary blocks.
		blocks = append([]acp.ContentBlock{acp.TextBlock(body.String())}, blocks...)
	}
	return blocks
}

func imageBlockFromPath(path, mime string) (acp.ContentBlock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return acp.ContentBlock{}, err
	}
	if mime == "" {
		mime = mimeFromExt(filepath.Ext(path))
	}
	if mime == "" {
		mime = "image/png"
	}
	return acp.ImageBlock(base64.StdEncoding.EncodeToString(data), mime), nil
}

func readTextSnippet(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Cap raw bytes before rune conversion.
	const maxBytes = textFileMaxChars * 4
	truncated := false
	if len(data) > maxBytes {
		data = data[:maxBytes]
		truncated = true
	}
	text := string(data)
	runes := []rune(text)
	if len(runes) > textFileMaxChars {
		text = string(runes[:textFileMaxChars])
		truncated = true
	}
	if truncated {
		text += "\n…（已截断，仅发送前 20000 字符）"
	}
	return strings.TrimSpace(text), nil
}

func kindFromPath(path, mime string) string {
	mime = strings.ToLower(mime)
	if strings.HasPrefix(mime, "image/") {
		return "image"
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".avif":
		return "image"
	}
	if isTextyPath(path, mime) {
		return "text_file"
	}
	return "path"
}

func isTextyPath(path, mime string) bool {
	mime = strings.ToLower(mime)
	if strings.HasPrefix(mime, "text/") || strings.Contains(mime, "json") || strings.Contains(mime, "xml") || strings.Contains(mime, "javascript") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".txt", ".md", ".markdown", ".json", ".csv", ".tsv", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx",
		".go", ".py", ".rb", ".java", ".c", ".cc", ".cpp", ".h", ".hpp", ".rs", ".php", ".sh", ".bash",
		".zsh", ".ps1", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".xml", ".html", ".htm",
		".css", ".scss", ".less", ".sql", ".log", ".env":
		return true
	}
	return false
}

func mimeFromExt(ext string) string {
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
