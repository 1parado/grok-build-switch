package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"grok_switch/internal/agentfs"
	"grok_switch/internal/llm"
)

// --- read ---

type ReadTool struct{}

func (ReadTool) Name() string { return "read" }

func (ReadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "要读取的文件路径（相对工作目录或绝对路径）"},
			"offset": map[string]any{"type": "integer", "description": "起始行号（1 起，从该行开始读）"},
			"limit":  map[string]any{"type": "integer", "description": "最多读取行数"},
		},
		"required": []string{"path"},
	}
}

func (ReadTool) Doc() string {
	return `读取文件内容，带行号返回。支持 offset/limit 分页读取大文件。
图片（png/jpg/gif/webp/bmp）直接返回画廊引用与 Media，不做 base64。
cat/head/tail 一律用本工具代替。`
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

const (
	readMaxBytes     = 4 << 20
	readDefaultLimit = 1000
	readMaxLineLen   = 2000
)

func (ReadTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	var a readArgs
	if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Path) == "" {
		return argHelp("read", err, `{"path": "...", "offset"?: 1, "limit"?: 500}`)
	}
	abs, err := env.Guard(a.Path)
	if err != nil {
		return ToolOutput{Text: err.Error(), IsError: true}
	}
	// 图片分支：按扩展名判定，先 Stat 校验存在性，直接回 URI 引用（对齐 pi processImage）。
	if isImagePath(abs) {
		st := env.Stat(abs)
		if !st.Exists {
			return ToolOutput{Text: fmt.Sprintf("读取失败: 文件不存在 %s", abs), IsError: true}
		}
		if st.IsDir {
			return ToolOutput{Text: fmt.Sprintf("读取失败: %s 是目录", abs), IsError: true}
		}
		return ToolOutput{
			Text:  fmt.Sprintf("[图片: %s（%d 字节，直接在画廊查看）]", abs, st.Size),
			Media: []llm.ContentPart{llm.ImagePart{URI: abs, MimeType: imageMime(abs)}},
		}
	}
	content, err := env.ReadText(abs, readMaxBytes)
	if err != nil {
		return ToolOutput{Text: fmt.Sprintf("读取失败: %v", err), IsError: true}
	}
	if content == "" {
		return ToolOutput{Text: "(空文件)"}
	}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	total := len(lines)
	start := 1
	if a.Offset > 1 {
		start = a.Offset
	}
	if start > total {
		return ToolOutput{Text: fmt.Sprintf("起始行 %d 超出文件总行数 %d", start, total), IsError: true}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = readDefaultLimit
	}
	end := start + limit - 1
	if end > total {
		end = total
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		line := lines[i-1]
		if len(line) > readMaxLineLen {
			line = line[:readMaxLineLen] + "…[行截断]"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i, line)
	}
	if end < total {
		fmt.Fprintf(&b, "... (共 %d 行，显示 %d-%d；用 offset=%d 继续读)\n", total, start, end, end+1)
	}
	return ToolOutput{Text: b.String()}
}

// --- write ---

type WriteTool struct{}

func (WriteTool) Name() string { return "write" }

func (WriteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "目标文件路径"},
			"content": map[string]any{"type": "string", "description": "完整文件内容（整文件覆盖写入）"},
		},
		"required": []string{"path", "content"},
	}
}

func (WriteTool) Doc() string {
	return `把完整内容写入文件（整文件覆盖，原子写入）。目标已存在时会被覆盖。
追加或局部修改请用 edit；echo > file / cat <<EOF 一律用本工具代替。`
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (WriteTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	var a writeArgs
	if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Path) == "" {
		return argHelp("write", err, `{"path": "...", "content": "..."}`)
	}
	abs, err := env.Guard(a.Path)
	if err != nil {
		return ToolOutput{Text: err.Error(), IsError: true}
	}
	if err := env.WriteText(abs, a.Content); err != nil {
		return ToolOutput{Text: fmt.Sprintf("写入失败: %v", err), IsError: true}
	}
	lines := strings.Count(a.Content, "\n") + 1
	return ToolOutput{Text: fmt.Sprintf("已写入 %s（%d 字节，%d 行）", abs, len(a.Content), lines)}
}

// --- edit ---

type EditTool struct{}

func (EditTool) Name() string { return "edit" }

func (EditTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       map[string]any{"type": "string", "description": "目标文件路径"},
			"old_string": map[string]any{"type": "string", "description": "要替换的原文（单块模式，必须精确匹配且唯一）"},
			"new_string": map[string]any{"type": "string", "description": "替换后的新文本（单块模式）"},
			"edits": map[string]any{
				"type": "array", "description": "多块批量替换（与 old_string/new_string 二选一，每块需唯一非重叠）",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"old_string": map[string]any{"type": "string"},
						"new_string": map[string]any{"type": "string"},
					},
					"required": []string{"old_string", "new_string"},
				},
			},
		},
		"required": []string{"path"},
	}
}

func (EditTool) Doc() string {
	return `在文件中做精确字符串替换。单块用 old_string/new_string（必须唯一命中）；
多块用 edits 数组一次提交多处不重叠替换，原子写回。old_string 命中多处时
拒绝并提示补充上下文。创建新文件用 write。`
}

type editBlock struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

type editArgs struct {
	Path      string          `json:"path"`
	OldString string          `json:"old_string"`
	NewString string          `json:"new_string"`
	Edits     json.RawMessage `json:"edits"`
}

func (EditTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	blocks, path, errText, isErr := normalizeEditArgs(args)
	if isErr {
		return ToolOutput{Text: errText, IsError: true}
	}
	abs, err := env.Guard(path)
	if err != nil {
		return ToolOutput{Text: err.Error(), IsError: true}
	}
	content, err := env.ReadText(abs, readMaxBytes)
	if err != nil {
		return ToolOutput{Text: fmt.Sprintf("读取失败: %v", err), IsError: true}
	}
	// BOM 与换行归一：匹配在 \n 上做，写回时还原。
	bom := ""
	if strings.HasPrefix(content, "\xef\xbb\xbf") {
		bom = "\xef\xbb\xbf"
		content = strings.TrimPrefix(content, bom)
	}
	lineEnding := "\n"
	if strings.Contains(content, "\r\n") {
		lineEnding = "\r\n"
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	// 归一 blocks 的换行（模型可能用 \n 而文件是 \r\n，已在上步统一）。
	updated := content
	totalReplaced := 0
	firstLine := 0
	for i, b := range blocks {
		oldN := strings.ReplaceAll(b.OldString, "\r\n", "\n")
		newN := strings.ReplaceAll(b.NewString, "\r\n", "\n")
		count := strings.Count(updated, oldN)
		switch count {
		case 0:
			return ToolOutput{Text: fmt.Sprintf("第 %d 块 old_string 在文件中未命中。请用 read 确认精确内容（注意缩进与换行）后再试。", i+1), IsError: true}
		case 1:
			// 定位首改行号用于回显。
			if idx := strings.Index(updated, oldN); idx >= 0 && firstLine == 0 {
				firstLine = strings.Count(updated[:idx], "\n") + 1
			}
			updated = strings.Replace(updated, oldN, newN, 1)
			totalReplaced++
		default:
			return ToolOutput{Text: fmt.Sprintf("第 %d 块 old_string 命中 %d 处，无法唯一定位。请在 old_string 中包含更多上下文使其唯一。", i+1, count), IsError: true}
		}
	}
	// 写回时还原换行与 BOM。
	if lineEnding != "\n" {
		updated = strings.ReplaceAll(updated, "\n", lineEnding)
	}
	updated = bom + updated
	if err := env.WriteText(abs, updated); err != nil {
		return ToolOutput{Text: fmt.Sprintf("写回失败: %v", err), IsError: true}
	}
	if firstLine > 0 {
		return ToolOutput{Text: fmt.Sprintf("已编辑 %s（替换 %d 处，首改第 %d 行）", abs, totalReplaced, firstLine)}
	}
	return ToolOutput{Text: fmt.Sprintf("已编辑 %s（替换 %d 处）", abs, totalReplaced)}
}

// normalizeEditArgs 兼容三种形态：
//  1. {path, old_string, new_string} 单块（legacy）
//  2. {path, edits: [{old_string,new_string}]} 多块
//  3. edits 为 JSON 字符串（模型偶发 stringified）或单对象
func normalizeEditArgs(args json.RawMessage) ([]editBlock, string, string, bool) {
	var a editArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, "", fmt.Sprintf("参数解析失败: %v。期望 {\"path\": \"...\", \"old_string\": \"...\", \"new_string\": \"...\"} 或 {\"path\":..., \"edits\":[...]}", err), true
	}
	if strings.TrimSpace(a.Path) == "" {
		return nil, "", "参数解析失败: 缺少 path。期望 JSON 对象字段: {\"path\": \"...\", \"old_string\": \"...\", \"new_string\": \"...\"}", true
	}
	// edits 优先（多块模式）。
	if len(bytesTrimSpace(a.Edits)) > 0 && string(bytesTrimSpace(a.Edits)) != "null" {
		blocks, err := parseEditBlocks(a.Edits)
		if err != nil {
			return nil, "", fmt.Sprintf("参数解析失败: %v。期望 edits 为 [{old_string,new_string}]", err), true
		}
		if len(blocks) == 0 {
			return nil, "", "edits 不能为空（至少一块）。", true
		}
		for i, b := range blocks {
			if b.OldString == "" {
				return nil, "", fmt.Sprintf("第 %d 块 old_string 不能为空。", i+1), true
			}
		}
		return blocks, a.Path, "", false
	}
	// 单块 legacy。
	if a.OldString == "" {
		return nil, "", "old_string 不能为空（清空文件请用 write；多块请用 edits）。", true
	}
	if a.OldString == a.NewString {
		return nil, "", "old_string 与 new_string 相同，无需编辑", true
	}
	return []editBlock{{OldString: a.OldString, NewString: a.NewString}}, a.Path, "", false
}

// parseEditBlocks 兼容数组 / 单对象 / JSON 字符串三形态。
func parseEditBlocks(raw json.RawMessage) ([]editBlock, error) {
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("edits 为空")
	}
	// JSON 字符串内嵌数组/对象：先解一层 string。
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err == nil {
			trimmed = bytesTrimSpace(json.RawMessage(inner))
		}
	}
	// 单对象 → 包一层数组。
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var single editBlock
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return nil, err
		}
		return []editBlock{single}, nil
	}
	var blocks []editBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func bytesTrimSpace(b json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(b)))
}

func isImagePath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	default:
		return false
	}
}

func imageMime(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
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
	default:
		return "image/jpeg"
	}
}
