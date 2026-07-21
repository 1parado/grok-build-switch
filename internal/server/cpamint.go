package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"grok_switch/internal/cpamint"
	"grok_switch/internal/grokpool"
)

func (s *Server) handleGrokPoolImportDir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.GrokPool == nil {
		writeError(w, fmt.Errorf("Grok 号池未初始化"), http.StatusServiceUnavailable)
		return
	}
	var request struct {
		Path      string `json:"path"`
		Recursive *bool  `json:"recursive"`
		UseAuthDir bool  `json:"use_auth_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, fmt.Errorf("读取导入目录请求: %w", err), http.StatusBadRequest)
		return
	}
	recursive := true
	if request.Recursive != nil {
		recursive = *request.Recursive
	}

	var result grokpool.ImportResult
	var err error
	if request.UseAuthDir || strings.TrimSpace(request.Path) == "" {
		result, err = s.GrokPool.ImportAuthDir()
	} else {
		result, err = s.GrokPool.ImportDirectory(request.Path, recursive)
	}
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	profile, err := s.upsertGrokAuthProfile()
	if err != nil {
		writeError(w, fmt.Errorf("账号已导入，但更新本地 profile 失败: %w", err), http.StatusInternalServerError)
		return
	}
	s.changed()
	writeJSONStatus(w, map[string]any{
		"result":  result,
		"status":  s.GrokPool.Status(),
		"profile": profile,
	}, http.StatusCreated)
}

func (s *Server) handleGrokPoolOpenAuthDir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.GrokPool == nil {
		writeError(w, fmt.Errorf("Grok 号池未初始化"), http.StatusServiceUnavailable)
		return
	}
	dir := s.GrokPool.ResolvedAuthDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if err := openLocalPath(dir); err != nil {
		writeError(w, fmt.Errorf("打开目录失败: %w", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": dir})
}

func (s *Server) handleCpaMint(w http.ResponseWriter, r *http.Request) {
	if s.CpaMint == nil || s.GrokPool == nil {
		writeError(w, fmt.Errorf("CPA 铸造服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
			session, err := s.CpaMint.Get(id)
			if err != nil {
				writeError(w, err, http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"session": session, "pool": s.GrokPool.Status()})
			return
		}
		if session, ok := s.CpaMint.Latest(); ok {
			writeJSON(w, map[string]any{"session": session, "pool": s.GrokPool.Status()})
			return
		}
		writeJSON(w, map[string]any{"session": nil, "pool": s.GrokPool.Status()})
	case http.MethodPost:
		var request struct {
			Email       string `json:"email"`
			OpenBrowser *bool  `json:"open_browser"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil && r.ContentLength != 0 {
			writeError(w, fmt.Errorf("读取铸造请求: %w", err), http.StatusBadRequest)
			return
		}
		openBrowser := true
		if request.OpenBrowser != nil {
			openBrowser = *request.OpenBrowser
		}
		poolStatus := s.GrokPool.Status()
		session, err := s.CpaMint.Start(cpamint.StartOptions{
			Email:       request.Email,
			AuthDir:     s.GrokPool.ResolvedAuthDir(),
			ProxyURL:    poolStatus.Settings.ProxyURL,
			BaseURL:     "https://cli-chat-proxy.grok.com/v1",
			OpenBrowser: openBrowser,
		})
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		// Background: when mint succeeds, auto-import into pool.
		go s.watchMintAndImport(session.ID)
		writeJSONStatus(w, map[string]any{"session": session, "pool": poolStatus}, http.StatusAccepted)
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			var body struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id = strings.TrimSpace(body.ID)
		}
		if id == "" {
			if session, ok := s.CpaMint.Latest(); ok {
				id = session.ID
			}
		}
		if id == "" {
			writeError(w, fmt.Errorf("没有可取消的铸造会话"), http.StatusBadRequest)
			return
		}
		session, err := s.CpaMint.Cancel(id)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"session": session})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) watchMintAndImport(sessionID string) {
	deadline := time.Now().Add(35 * time.Minute)
	for time.Now().Before(deadline) {
		session, err := s.CpaMint.Get(sessionID)
		if err != nil {
			return
		}
		switch session.Status {
		case cpamint.StatusSuccess:
			raw, path, rawErr := s.CpaMint.RawCredential(sessionID)
			if rawErr != nil {
				return
			}
			name := filepath.Base(path)
			if name == "" || name == "." {
				name = "cpa-mint.json"
			}
			result, importErr := s.GrokPool.Import([]grokpool.ImportFile{{
				Name:    name,
				Content: string(raw),
			}})
			if importErr != nil {
				return
			}
			if _, err := s.upsertGrokAuthProfile(); err == nil {
				s.changed()
			}
			_ = result
			return
		case cpamint.StatusFailed, cpamint.StatusCancelled:
			return
		}
		time.Sleep(1500 * time.Millisecond)
	}
}

func openLocalPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("empty path")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}
