package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
