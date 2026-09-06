package server

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"grok_switch/internal/tools"
)

// loadProjectContext 加载项目上下文（对齐 pi resource-loader 语义的最小集）：
// 从 cwd 向上到根收集 AGENTS.override.md > AGENTS.md > CLAUDE.md（去重），
// 再叠 cwd/.pi/SYSTEM.md 自定义系统提示。单文件 8k、总量 16k 截断，避免撑爆上下文。
func loadProjectContext(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	// 祖先链：cwd → /，收集三类文件（override 优先）。
	seenDir := map[string]bool{}
	var files []string
	cur := abs
	for {
		if !seenDir[cur] {
			seenDir[cur] = true
			for _, name := range []string{"AGENTS.override.md", "AGENTS.md", "CLAUDE.md"} {
				p := filepath.Join(cur, name)
				if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 && st.Size() < 200<<10 {
					files = append(files, p)
					break // 同目录只取最高优先
				}
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
		// 防止超深链：最多向上 12 层。
		if len(seenDir) > 12 {
			break
		}
	}
	// 祖先在前、cwd 在后？pi 是全局 + 祖先链去重；此处按发现逆序（根→cwd）使 cwd 覆盖。
	for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
		files[i], files[j] = files[j], files[i]
	}
	var b strings.Builder
	total := 0
	const perFile = 8000
	const totalBudget = 16000
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil || len(data) == 0 {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		if len(text) > perFile {
			text = text[:perFile] + "\n\n[…项目上下文单文件截断]"
		}
		if total+len(text) > totalBudget {
			break
		}
		rel, _ := filepath.Rel(abs, f)
		if rel == "" {
			rel = f
		}
		b.WriteString("\n\n<project_context path=\"" + f + "\">\n")
		b.WriteString(text)
		b.WriteString("\n</project_context>")
		total += len(text)
	}
	// cwd/.pi/SYSTEM.md 自定义系统提示（需在 cwd 内，天然可信）。
	if sys := filepath.Join(abs, ".pi", "SYSTEM.md"); true {
		if st, err := os.Stat(sys); err == nil && !st.IsDir() && st.Size() > 0 && st.Size() < 32<<10 {
			if data, err := os.ReadFile(sys); err == nil {
				text := strings.TrimSpace(string(data))
				if text != "" {
					if len(text) > perFile {
						text = text[:perFile] + "\n[…SYSTEM.md 截断]"
					}
					b.WriteString("\n\n<custom_system_prompt>\n" + text + "\n</custom_system_prompt>")
				}
			}
		}
	}
	return b.String()
}

// buildToolPromptSection 把注册表工具 Doc 拼成系统提示词段（对齐 pi toolSnippets）。
// 只列当前 turn 激活工具；生图未注册时不提 generate_image，避免模型幻觉调用。
func buildToolPromptSection(reg *tools.Registry) string {
	if reg == nil {
		return ""
	}
	names := reg.Names()
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("\n# 可用工具\n\n")
	for _, n := range names {
		doc := ""
		for _, s := range reg.Schemas() {
			if s.Name == n {
				doc = s.Description
				break
			}
		}
		doc = strings.TrimSpace(doc)
		if doc == "" {
			b.WriteString("- " + n + "\n")
			continue
		}
		// 单工具多行 Doc 压成紧凑块，首行加粗。
		lines := strings.Split(doc, "\n")
		b.WriteString("- " + n + ": " + strings.TrimSpace(lines[0]) + "\n")
		for _, ln := range lines[1:] {
			if strings.TrimSpace(ln) != "" {
				b.WriteString("  " + strings.TrimSpace(ln) + "\n")
			}
		}
	}
	b.WriteString("\n- 文件探索优先 glob/grep 定位，再 read 精读；修改优先 edit 多块批量，整文件重写才用 write。\n")
	b.WriteString("- 回复简洁，涉及文件必须给相对工作目录的路径。\n")
	return b.String()
}
