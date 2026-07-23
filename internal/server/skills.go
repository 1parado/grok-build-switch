package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type SkillEntry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source"` // "agents" or "grok"
	IsDir  bool   `json:"is_dir"`
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	home, _ := os.UserHomeDir()
	agentsDir := filepath.Join(home, ".agents")
	grokDir := s.Paths.GrokHome
	if grokDir == "" {
		grokDir = filepath.Join(home, ".grok")
	}

	var skills []SkillEntry
	seen := make(map[string]bool)

	// Scan ~/.agents/skills/ — agent-specific skills
	scanSkillsDir(filepath.Join(agentsDir, "skills"), "agents/skills", &skills, seen)
	// Scan ~/.grok/skills/ — user-installed skills
	scanSkillsDir(filepath.Join(grokDir, "skills"), "grok/skills", &skills, seen)
	// Scan ~/.grok/bundled/skills/ — bundled/packaged skills
	scanSkillsDir(filepath.Join(grokDir, "bundled", "skills"), "grok/bundled", &skills, seen)

	sort.Slice(skills, func(i, j int) bool {
		if skills[i].Source != skills[j].Source {
			return skills[i].Source < skills[j].Source
		}
		return skills[i].Name < skills[j].Name
	})

	writeJSON(w, skills)
}

func scanSkillsDir(dir, source string, skills *[]SkillEntry, seen map[string]bool) {
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			item := SkillEntry{
				Name:   e.Name(),
				Path:   filepath.Join(dir, e.Name()),
				Source: source,
				IsDir:  e.IsDir(),
			}
			if !seen[item.Path] {
				*skills = append(*skills, item)
				seen[item.Path] = true
			}
		}
	}
}

const maxSkillInstructionBytes = 128 << 10

// expandSkillPrompt resolves the slash syntax used by Grok's TUI and expands
// the referenced SKILL.md before the text is sent through ACP Prompt.
func (s *Server) expandSkillPrompt(text string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return text, err
	}
	grokHome := s.Paths.GrokHome
	if grokHome == "" {
		grokHome = filepath.Join(home, ".grok")
	}
	roots := []string{
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(grokHome, "skills"),
		filepath.Join(grokHome, "bundled", "skills"),
	}
	return expandSkillPromptWithRoots(text, roots)
}

func expandSkillPromptWithRoots(text string, roots []string) (string, error) {
	parts := strings.SplitAfter(text, "\n")
	changed := false
	for i, raw := range parts {
		line := strings.TrimSuffix(raw, "\n")
		name, remainder, ok := parseSkillDirective(line)
		if !ok {
			continue
		}
		content, ok, err := loadSkillInstructions(roots, name)
		if err != nil {
			return text, err
		}
		if !ok {
			continue
		}
		parts[i] = formatSkillPrompt(name, content, remainder)
		if strings.HasSuffix(raw, "\n") {
			parts[i] += "\n"
		}
		changed = true
	}
	if !changed {
		return text, nil
	}
	return strings.Join(parts, ""), nil
}

func parseSkillDirective(line string) (name, remainder string, ok bool) {
	command := strings.TrimSpace(line)
	if !strings.HasPrefix(command, "/") {
		return "", "", false
	}
	command = strings.TrimSpace(strings.TrimPrefix(command, "/"))
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", "", false
	}
	if strings.EqualFold(fields[0], "skills") {
		if len(fields) < 2 {
			return "", "", false
		}
		name = fields[1]
		idx := strings.Index(command, name)
		remainder = strings.TrimSpace(command[idx+len(name):])
		return name, remainder, true
	}
	name = fields[0]
	idx := strings.Index(command, name)
	remainder = strings.TrimSpace(command[idx+len(name):])
	return name, remainder, true
}

func loadSkillInstructions(roots []string, name string) (string, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\\`) || name == "." || name == ".." {
		return "", false, nil
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, err
		}
		for _, entry := range entries {
			if !strings.EqualFold(entry.Name(), name) {
				continue
			}
			candidate := filepath.Join(root, entry.Name())
			instructionPath, found, err := findSkillInstructionPath(candidate)
			if err != nil {
				return "", false, err
			}
			if !found {
				continue
			}
			data, err := os.ReadFile(instructionPath)
			if err != nil {
				return "", false, err
			}
			if len(data) > maxSkillInstructionBytes {
				data = append(data[:maxSkillInstructionBytes], []byte("\n\n[Skill 内容已截断。]")...)
			}
			return strings.TrimSpace(string(data)), true, nil
		}
	}
	return "", false, nil
}

func findSkillInstructionPath(candidate string) (string, bool, error) {
	info, err := os.Stat(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if !info.IsDir() {
		if strings.EqualFold(filepath.Ext(candidate), ".md") {
			return candidate, true, nil
		}
		return "", false, nil
	}
	direct := filepath.Join(candidate, "SKILL.md")
	if stat, err := os.Stat(direct); err == nil && !stat.IsDir() {
		return direct, true, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	entries, err := os.ReadDir(candidate)
	if err != nil {
		return "", false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		nested := filepath.Join(candidate, entry.Name(), "SKILL.md")
		if stat, err := os.Stat(nested); err == nil && !stat.IsDir() {
			return nested, true, nil
		} else if err != nil && !os.IsNotExist(err) {
			return "", false, err
		}
	}
	return "", false, nil
}

func formatSkillPrompt(name, content, remainder string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "请按照 Skill `%s` 执行本轮请求。\n\n<skill name=\"%s\">\n%s\n</skill>", name, name, content)
	if remainder != "" {
		b.WriteString("\n\n")
		b.WriteString(remainder)
	}
	return b.String()
}

func (s *Server) handleSkillsDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		writeError(w, fmt.Errorf("path is required"), http.StatusBadRequest)
		return
	}

	// Security: prevent path traversal by resolving and verifying the path
	// is within allowed directories (~/.agents or ~/.grok).
	absPath, err := filepath.Abs(req.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	home, _ := os.UserHomeDir()
	agentsDir := filepath.Join(home, ".agents")
	grokDir := s.Paths.GrokHome
	if grokDir == "" {
		grokDir = filepath.Join(home, ".grok")
	}
	allowed := false
	for _, dir := range []string{agentsDir, grokDir} {
		if dir != "" {
			rel, err := filepath.Rel(dir, absPath)
			if err == nil && len(rel) > 0 && rel[0] != '.' {
				allowed = true
				break
			}
		}
	}
	if !allowed {
		writeError(w, fmt.Errorf("path is outside allowed directories"), http.StatusForbidden)
		return
	}

	if err := os.RemoveAll(absPath); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "已删除"})
}
