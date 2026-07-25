package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"grok_switch/internal/folderpick"
	"grok_switch/internal/notify"
)

// handleAgentPickDirectory opens a native folder dialog and returns the path.
// POST /api/agent/pick-directory  body: { "start": "optional initial dir" }
// Response: { "path": "..." } or { "cancelled": true }
func (s *Server) handleAgentPickDirectory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Start string `json:"start"`
	}
	// Empty body is fine (no start path).
	if err := decodeAgentJSON(r, &req); err != nil && !strings.Contains(err.Error(), "EOF") {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	start := strings.TrimSpace(req.Start)
	if start == "" && s.Agent != nil {
		start = s.Agent.Status().Cwd
	}
	path, err := folderpick.Directory(r.Context(), start)
	if err != nil {
		if errors.Is(err, folderpick.ErrCancelled) {
			writeJSON(w, map[string]any{"cancelled": true, "path": ""})
			return
		}
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"cancelled": false, "path": path})
}

// handleAgentOpenPath reveals a directory (or file parent) in the OS file manager.
// POST /api/agent/open-path  body: { "path": "..." }
func (s *Server) handleAgentOpenPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := decodeAgentJSON(r, &req); err != nil && !strings.Contains(err.Error(), "EOF") {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	path := filepath.Clean(strings.TrimSpace(req.Path))
	if path == "" {
		writeError(w, errors.New("路径不能为空"), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	target := path
	if !info.IsDir() {
		target = filepath.Dir(path)
	}
	if err := notify.OpenPath(target); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": target})
}
