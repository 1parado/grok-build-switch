package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	maxUploadImageBytes = 16 << 20
	maxUploadTextBytes  = 1 << 20
)

var unsafeUploadName = regexp.MustCompile(`[^a-zA-Z0-9._\-\p{L}\p{N}]+`)

func (s *Server) handleAgentUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseMultipartForm(maxUploadImageBytes + (1 << 20)); err != nil {
		writeError(w, fmt.Errorf("解析上传内容失败: %w", err), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, fmt.Errorf("缺少 file 字段: %w", err), http.StatusBadRequest)
		return
	}
	defer file.Close()

	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = r.FormValue("mime_type")
	}
	name := strings.TrimSpace(header.Filename)
	if name == "" {
		name = strings.TrimSpace(r.FormValue("name"))
	}
	if name == "" {
		name = "upload.bin"
	}
	name = filepath.Base(name)
	name = unsafeUploadName.ReplaceAllString(name, "_")
	if name == "" || name == "." || name == ".." {
		name = "upload.bin"
	}

	kind := strings.TrimSpace(r.FormValue("kind"))
	if kind == "" {
		if strings.HasPrefix(strings.ToLower(mimeType), "image/") {
			kind = "image"
		} else {
			kind = "text_file"
		}
	}
	limit := int64(maxUploadTextBytes)
	if kind == "image" || strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		kind = "image"
		limit = maxUploadImageBytes
	}

	sessionID := strings.TrimSpace(r.FormValue("session_id"))
	if sessionID == "" && s.Agent != nil {
		sessionID = s.Agent.Status().SessionID
	}
	if sessionID == "" {
		sessionID = "tmp"
	}
	sessionID = unsafeUploadName.ReplaceAllString(sessionID, "_")
	dir := filepath.Join(s.Paths.DataDir, "attachments", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	stamp := time.Now().UTC().Format("20060102-150405.000")
	dest := filepath.Join(dir, stamp+"-"+name)
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	written, err := io.Copy(out, io.LimitReader(file, limit+1))
	_ = out.Close()
	if err != nil {
		_ = os.Remove(dest)
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if written > limit {
		_ = os.Remove(dest)
		writeError(w, fmt.Errorf("文件过大（上限 %d 字节）", limit), http.StatusRequestEntityTooLarge)
		return
	}
	if mimeType == "" && kind == "image" {
		mimeType = mimeFromFilename(name)
	}
	writeJSON(w, map[string]any{
		"kind":      kind,
		"name":      name,
		"path":      dest,
		"mime_type": mimeType,
		"size":      written,
	})
}

func mimeFromFilename(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
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
		return "application/octet-stream"
	}
}

func (s *Server) handleAgentFSList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cwd := strings.TrimSpace(r.URL.Query().Get("cwd"))
	if cwd == "" && s.Agent != nil {
		cwd = s.Agent.Status().Cwd
	}
	if cwd == "" {
		writeError(w, fmt.Errorf("工作目录为空"), http.StatusBadRequest)
		return
	}
	cwd = filepath.Clean(cwd)
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		writeError(w, fmt.Errorf("无效工作目录"), http.StatusBadRequest)
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	const maxEntries = 200
	type entry struct {
		Path  string `json:"path"`
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
	}
	var out []entry
	_ = filepath.WalkDir(cwd, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || len(out) >= maxEntries {
			if len(out) >= maxEntries {
				return io.EOF
			}
			return nil
		}
		if path == cwd {
			return nil
		}
		// Skip deep/hidden noise.
		base := d.Name()
		if strings.HasPrefix(base, ".") || base == "node_modules" || base == ".git" || base == "vendor" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			return nil
		}
		depth := strings.Count(rel, string(os.PathSeparator))
		if depth > 3 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if query != "" && !strings.Contains(strings.ToLower(base), query) && !strings.Contains(strings.ToLower(rel), query) {
			return nil
		}
		out = append(out, entry{Path: path, Name: rel, IsDir: d.IsDir()})
		return nil
	})
	writeJSON(w, out)
}
