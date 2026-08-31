package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"grok_switch/internal/agentfs"
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
	return `读取文本文件内容，带行号返回。支持 offset/limit 分页读取大文件。
读图片请用 read_media。cat/head/tail 一律用本工具代替。`
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
			"old_string": map[string]any{"type": "string", "description": "要替换的原文（必须与文件内容精确匹配且唯一）"},
			"new_string": map[string]any{"type": "string", "description": "替换后的新文本"},
		},
		"required": []string{"path", "old_string", "new_string"},
	}
}

func (EditTool) Doc() string {
	return `在文件中做精确字符串替换。old_string 必须与文件内容完全一致且唯一命中；
命中多处时工具会拒绝并提示补充上下文（把 old_string 写长一点以唯一定位）。
创建新文件用 write。`
}

type editArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func (EditTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	var a editArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Path == "" {
		return argHelp("edit", err, `{"path": "...", "old_string": "...", "new_string": "..."}`)
	}
	if a.OldString == "" {
		return ToolOutput{Text: "old_string 不能为空（清空文件请用 write）", IsError: true}
	}
	if a.OldString == a.NewString {
		return ToolOutput{Text: "old_string 与 new_string 相同，无需编辑", IsError: true}
	}
	abs, err := env.Guard(a.Path)
	if err != nil {
		return ToolOutput{Text: err.Error(), IsError: true}
	}
	content, err := env.ReadText(abs, readMaxBytes)
	if err != nil {
		return ToolOutput{Text: fmt.Sprintf("读取失败: %v", err), IsError: true}
	}
	count := strings.Count(content, a.OldString)
	switch count {
	case 0:
		return ToolOutput{Text: "old_string 在文件中未命中。请用 read 确认精确内容（注意缩进与换行）后再试。", IsError: true}
	case 1:
		updated := strings.Replace(content, a.OldString, a.NewString, 1)
		if err := env.WriteText(abs, updated); err != nil {
			return ToolOutput{Text: fmt.Sprintf("写回失败: %v", err), IsError: true}
		}
		return ToolOutput{Text: fmt.Sprintf("已编辑 %s（替换 1 处）", abs)}
	default:
		return ToolOutput{Text: fmt.Sprintf("old_string 命中 %d 处，无法唯一定位。请在 old_string 中包含更多上下文使其唯一。", count), IsError: true}
	}
}
