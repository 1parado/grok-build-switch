package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type chatProject struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	Trusted      bool      `json:"trusted"`
	Pinned       bool      `json:"pinned"`
	LastOpenedAt time.Time `json:"last_opened_at"`
	PathOK       bool      `json:"path_ok"`
}

// projectsMu 保护 projects.json 的整个「读-改-写」流程：所有 handler 必须
// 先持有 s.projectsMu 再调用 loadProjects / saveProjects，否则并发请求会
// 相互覆盖更新。

func (s *Server) projectsFile() string {
	return filepath.Join(s.Paths.DataDir, "projects.json")
}

func (s *Server) loadProjects() ([]chatProject, error) {
	path := s.projectsFile()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []chatProject{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []chatProject
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	dirty := false
	for i := range list {
		// Prefer absolute form so Chinese / mixed-slash paths match session cwd.
		if abs, absErr := filepath.Abs(filepath.Clean(strings.TrimSpace(list[i].Path))); absErr == nil && abs != "" {
			if abs != list[i].Path {
				list[i].Path = abs
				dirty = true
			}
		}
		info, statErr := os.Stat(list[i].Path)
		list[i].PathOK = statErr == nil && info.IsDir()
		if list[i].Name == "" && list[i].Path != "" {
			list[i].Name = filepath.Base(list[i].Path)
			dirty = true
		}
	}
	if dirty {
		_ = s.saveProjects(list)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Pinned != list[j].Pinned {
			return list[i].Pinned
		}
		return list[i].LastOpenedAt.After(list[j].LastOpenedAt)
	})
	return list, nil
}

func (s *Server) saveProjects(list []chatProject) error {
	path := s.projectsFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if runtime.GOOS == "windows" {
			// Windows 不允许 rename 覆盖已存在文件。
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				return err
			}
			return os.Rename(tmpName, path)
		}
		return err
	}
	return nil
}

func (s *Server) handleAgentProjects(w http.ResponseWriter, r *http.Request) {
	s.projectsMu.Lock()
	defer s.projectsMu.Unlock()
	switch r.Method {
	case http.MethodGet:
		list, err := s.loadProjects()
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	case http.MethodPost:
		var req struct {
			Path    string `json:"path"`
			Name    string `json:"name"`
			Trusted bool   `json:"trusted"`
		}
		if err := decodeAgentJSON(r, &req); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		path, err := normalizeProjectPath(req.Path)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		list, err := s.loadProjects()
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		for i, item := range list {
			if sameProjectPath(item.Path, path) {
				list[i].Trusted = list[i].Trusted || req.Trusted
				list[i].LastOpenedAt = time.Now().UTC()
				if strings.TrimSpace(req.Name) != "" {
					list[i].Name = strings.TrimSpace(req.Name)
				}
				list[i].PathOK = true
				if err := s.saveProjects(list); err != nil {
					writeError(w, err, http.StatusInternalServerError)
					return
				}
				writeJSON(w, list[i])
				return
			}
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = filepath.Base(path)
		}
		id, err := newProjectID()
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		item := chatProject{
			ID: id, Name: name, Path: path, Trusted: req.Trusted,
			LastOpenedAt: time.Now().UTC(), PathOK: true,
		}
		list = append(list, item)
		if err := s.saveProjects(list); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, item)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleAgentProjectAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	s.projectsMu.Lock()
	defer s.projectsMu.Unlock()
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agent/projects/"), "/")
	var req struct {
		ID     string `json:"id"`
		Path   string `json:"path"`
		Pinned bool   `json:"pinned"`
	}
	if err := decodeAgentJSON(r, &req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	list, err := s.loadProjects()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	idx := -1
	for i, item := range list {
		if item.ID == req.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, errors.New("项目不存在"), http.StatusNotFound)
		return
	}
	switch action {
	case "trust":
		list[idx].Trusted = true
		list[idx].LastOpenedAt = time.Now().UTC()
	case "pin":
		list[idx].Pinned = req.Pinned
	case "remove":
		list = append(list[:idx], list[idx+1:]...)
		if err := s.saveProjects(list); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
		return
	case "relocate":
		// Fix broken / garbled paths (e.g. Chinese path corrupted by old picker).
		newPath, nerr := normalizeProjectPath(req.Path)
		if nerr != nil {
			writeError(w, nerr, http.StatusBadRequest)
			return
		}
		// Avoid colliding with another project on the same path.
		for i, item := range list {
			if i != idx && sameProjectPath(item.Path, newPath) {
				writeError(w, fmt.Errorf("该路径已属于项目「%s」", item.Name), http.StatusConflict)
				return
			}
		}
		list[idx].Path = newPath
		list[idx].PathOK = true
		list[idx].LastOpenedAt = time.Now().UTC()
		if list[idx].Name == "" || looksLikeGarbledPath(list[idx].Name) {
			list[idx].Name = filepath.Base(newPath)
		}
	case "open":
		list[idx].LastOpenedAt = time.Now().UTC()
		if !list[idx].Trusted {
			writeError(w, errors.New("请先信任该项目"), http.StatusForbidden)
			return
		}
		info, statErr := os.Stat(list[idx].Path)
		if statErr != nil || !info.IsDir() {
			list[idx].PathOK = false
			_ = s.saveProjects(list)
			writeError(w, fmt.Errorf("项目路径不可用，请重新选择目录: %s", list[idx].Path), http.StatusBadRequest)
			return
		}
		list[idx].PathOK = true
		if err := s.saveProjects(list); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		s.rememberAgentCwd(list[idx].Path)
		writeJSON(w, list[idx])
		return
	default:
		writeError(w, fmt.Errorf("不支持的操作: %s", action), http.StatusBadRequest)
		return
	}
	if err := s.saveProjects(list); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, list[idx])
}

func newProjectID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// normalizeProjectPath cleans and absolutizes a workspace path so Chinese
// and other non-ASCII directory names match consistently across pick/add/open.
func normalizeProjectPath(raw string) (string, error) {
	path := filepath.Clean(strings.TrimSpace(raw))
	if path == "" || path == "." {
		return "", errors.New("项目路径不能为空")
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("路径不是有效目录: %s", path)
	}
	return path, nil
}

func sameProjectPath(a, b string) bool {
	ca := filepath.Clean(strings.TrimSpace(a))
	cb := filepath.Clean(strings.TrimSpace(b))
	if ca == cb {
		return true
	}
	if aa, err := filepath.Abs(ca); err == nil {
		ca = aa
	}
	if bb, err := filepath.Abs(cb); err == nil {
		cb = bb
	}
	return strings.EqualFold(ca, cb)
}

// looksLikeGarbledPath detects mojibake folder names from legacy code-page
// corruption (e.g. 测试 → æµ‹è¯• style replacement glyphs).
func looksLikeGarbledPath(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	// Common UTF-8-as-Latin1 artifacts for CJK.
	for _, r := range name {
		if r == 'Ã' || r == 'Â' || r == 'æ' || r == 'å' || r == 'è' || r == 'é' || r == 'ø' || r == '�' {
			return true
		}
	}
	return false
}
