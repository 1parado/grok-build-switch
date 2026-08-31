package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"grok_switch/internal/agentfs"
)

// --- glob ---

type GlobTool struct{}

func (GlobTool) Name() string { return "glob" }

func (GlobTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "glob 模式，如 \"**/*.go\"、\"internal/**/*.md\""},
			"path":    map[string]any{"type": "string", "description": "起始目录（默认工作目录）"},
		},
		"required": []string{"pattern"},
	}
}

func (GlobTool) Doc() string {
	return `按 glob 模式列出文件路径（支持 ** 跨目录）。按文件名找文件用本工具，
按内容搜索用 grep。结果按修改时间排序（最新在前），上限 200 条。`
}

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

const globMaxResults = 200

func (GlobTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	var a globArgs
	if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Pattern) == "" {
		return argHelp("glob", err, `{"pattern": "**/*.go", "path"?: "."}`)
	}
	base := env.Cwd
	if a.Path != "" {
		abs, err := env.Guard(a.Path)
		if err != nil {
			return ToolOutput{Text: err.Error(), IsError: true}
		}
		base = abs
	}
	pattern := a.Pattern
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(base, pattern)
	} else if !env.WithinSandbox(pattern) {
		return ToolOutput{Text: (&agentfs.SandboxError{Path: pattern}).Error(), IsError: true}
	}

	// 简易 glob：目录遍历 + pattern 匹配（filepath.Match + ** 展开）。
	var matches []string
	err := walkGlob(base, base, a.Pattern, &matches, 0)
	if err != nil {
		return ToolOutput{Text: fmt.Sprintf("glob 失败: %v", err), IsError: true}
	}
	if len(matches) == 0 {
		return ToolOutput{Text: "(无匹配文件)"}
	}
	sort.Strings(matches)
	var b strings.Builder
	shown := len(matches)
	if shown > globMaxResults {
		shown = globMaxResults
	}
	for i := 0; i < shown; i++ {
		rel, _ := filepath.Rel(env.Cwd, matches[i])
		b.WriteString(rel)
		b.WriteString("\n")
	}
	if len(matches) > globMaxResults {
		fmt.Fprintf(&b, "... (共 %d 个匹配，仅显示前 %d 个；请收窄 pattern)\n", len(matches), globMaxResults)
	}
	return ToolOutput{Text: b.String()}
}

// walkGlob 用 filepath.Match 逐层匹配支持 ** 的简化实现。
func walkGlob(root, dir, pattern string, out *[]string, depth int) error {
	if depth > 12 {
		return nil
	}
	// 当前层匹配（** 在段首时匹配任意层级）。
	rest := pattern
	for {
		if strings.HasPrefix(rest, "**/") {
			// 尝试在当前目录直接匹配剩余模式。
			if err := walkGlob(root, dir, strings.TrimPrefix(rest, "**/"), out, depth+1); err != nil {
				return err
			}
			// 并深入子目录继续。
			entries, err := osReadDir(dir)
			if err != nil {
				return nil
			}
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					if err := walkGlob(root, filepath.Join(dir, e.Name()), rest, out, depth+1); err != nil {
						return err
					}
				}
			}
			return nil
		}
		break
	}
	// 普通段匹配。
	seg := rest
	var next string
	if idx := strings.Index(seg, "/"); idx >= 0 {
		seg, next = seg[:idx], seg[idx+1:]
	}
	if seg == "" {
		// pattern 以 / 结尾等退化形态。
		return nil
	}
	entries, err := osReadDir(dir)
	if err != nil {
		return nil
	}
	hasMeta := strings.ContainsAny(seg, "*?[")
	for _, e := range entries {
		name := e.Name()
		ok := name == seg
		if !ok && hasMeta {
			m, _ := filepath.Match(seg, name)
			ok = m
		}
		if !ok {
			continue
		}
		full := filepath.Join(dir, name)
		if next == "" {
			if !e.IsDir() {
				*out = append(*out, full)
			}
		} else if e.IsDir() {
			if err := walkGlob(root, full, next, out, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- grep ---

type GrepTool struct{}

func (GrepTool) Name() string { return "grep" }

func (GrepTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":   map[string]any{"type": "string", "description": "正则表达式（RE2 语法）"},
			"path":      map[string]any{"type": "string", "description": "搜索的文件或目录（默认工作目录）"},
			"glob":      map[string]any{"type": "string", "description": "文件名过滤，如 \"*.go\""},
			"max_items": map[string]any{"type": "integer", "description": "最多返回的匹配条数（默认 100）"},
		},
		"required": []string{"pattern"},
	}
}

func (GrepTool) Doc() string {
	return `按正则搜索文件内容，返回命中文件、行号与行内容（截断到 500 字符）。
搜索会跳过 .git、node_modules、二进制文件与超大文件（>2MB）。`
}

type grepArgs struct {
	Pattern  string `json:"pattern"`
	Path     string `json:"path"`
	Glob     string `json:"glob"`
	MaxItems int    `json:"max_items"`
}

const (
	grepMaxFileBytes = 2 << 20
	grepDefaultItems = 100
	grepMaxLineLen   = 500
	grepSkipDirs     = ".git,node_modules,vendor,dist,build,__pycache__,.venv"
)

func (GrepTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	var a grepArgs
	if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Pattern) == "" {
		return argHelp("grep", err, `{"pattern": "regex", "path"?: ".", "glob"?: "*.go"}`)
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return ToolOutput{Text: fmt.Sprintf("正则无效: %v", err), IsError: true}
	}
	max := a.MaxItems
	if max <= 0 || max > 1000 {
		max = grepDefaultItems
	}
	base := env.Cwd
	if a.Path != "" {
		abs, err := env.Guard(a.Path)
		if err != nil {
			return ToolOutput{Text: err.Error(), IsError: true}
		}
		base = abs
	}
	st := env.Stat(base)
	if !st.Exists {
		return ToolOutput{Text: fmt.Sprintf("路径不存在: %s", base), IsError: true}
	}

	grepState := &grepRun{re: re, env: env, glob: a.Glob, max: max}
	if !st.IsDir {
		grepState.searchFile(base)
	} else {
		grepState.walk(base, 0)
	}
	return grepState.render(a.Pattern)
}

type grepHit struct {
	Path string
	Line int
	Text string
}

type grepRun struct {
	re     *regexp.Regexp
	env    agentfs.Env
	glob   string
	max    int
	hits   []grepHit
	files  map[string]bool
	capped bool
}

func (g *grepRun) walk(dir string, depth int) {
	if depth > 10 || len(g.hits) >= g.max {
		return
	}
	entries, err := osReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if len(g.hits) >= g.max {
			return
		}
		name := e.Name()
		full := filepath.Join(dir, name)
		if e.IsDir() {
			if strings.Contains(","+grepSkipDirs+",", ","+name+",") || strings.HasPrefix(name, ".") {
				continue
			}
			g.walk(full, depth+1)
			continue
		}
		if g.glob != "" {
			m, _ := filepath.Match(g.glob, name)
			if !m {
				continue
			}
		}
		g.searchFile(full)
	}
}

func (g *grepRun) searchFile(path string) {
	if len(g.hits) >= g.max {
		return
	}
	if st := g.env.Stat(path); !st.Exists || st.Size > grepMaxFileBytes {
		return
	}
	content, err := g.env.ReadText(path, grepMaxFileBytes)
	if err != nil {
		return
	}
	if strings.ContainsRune(content, 0) {
		return // 二进制
	}
	if g.files == nil {
		g.files = map[string]bool{}
	}
	g.files[path] = true
	for i, line := range strings.Split(content, "\n") {
		if len(g.hits) >= g.max {
			g.capped = true
			return
		}
		if g.re.MatchString(line) {
			text := line
			if len(text) > grepMaxLineLen {
				text = text[:grepMaxLineLen] + "…"
			}
			g.hits = append(g.hits, grepHit{Path: path, Line: i + 1, Text: text})
		}
	}
}

func (g *grepRun) render(pattern string) ToolOutput {
	if len(g.hits) == 0 {
		return ToolOutput{Text: fmt.Sprintf("(无匹配: %s)", pattern)}
	}
	var b strings.Builder
	curFile := ""
	for _, h := range g.hits {
		rel, _ := filepath.Rel(g.env.Cwd, h.Path)
		if rel != curFile {
			curFile = rel
			fmt.Fprintf(&b, "%s:\n", rel)
		}
		fmt.Fprintf(&b, "  %d: %s\n", h.Line, h.Text)
	}
	if g.capped {
		fmt.Fprintf(&b, "... (达到 %d 条上限，结果已截断；请收窄 pattern)\n", g.max)
	}
	return ToolOutput{Text: b.String()}
}
