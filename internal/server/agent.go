package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"grok_switch/internal/agentbridge"
	"grok_switch/internal/notify"
)

type AgentService interface {
	Status() agentbridge.Status
	Start(context.Context, agentbridge.StartOptions) error
	Stop() error
	NewSession(context.Context, string) error
	Prompt(string, []agentbridge.Attachment) error
	CancelPrompt() error
	Subscribe() (string, <-chan agentbridge.Event)
	Unsubscribe(string)
	RespondPermission(string, bool) error
	RespondPermissionEx(string, bool, bool) error
	RespondPermissionOption(string, string, bool) error
	RespondPlan(string, agentbridge.PlanDecision) error
	RewindDropLastUser(context.Context, bool) agentbridge.RewindResult
	ClearBootstrap()
	ArmBootstrapFromSession(string) error
	SetSessionAutoApprove(bool)
	SetSessionConfig(context.Context, string, string) error
	ListStoredSessions(string, int) ([]agentbridge.SessionSummary, error)
	StoredSessionHistory(string) (agentbridge.SessionHistory, error)
	RenameStoredSession(string, string) error
	DeleteStoredSession(string) error
}

func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, s.Agent.Status())
}

// handleAgentPending 返回仍然挂起的审批/计划请求。
// permission_request 走 WS 单次广播，页面刷新或 WS 断开期间发出的事件
// 无法再收到；此接口让前端重连后主动拉取，恢复审批入口（.turn 仍挂在
// WaitForDecision 上，不响应就会一直占着 busy）。
func (s *Server) handleAgentPending(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	pender, ok := s.Agent.(interface {
		PendingRequests() []agentbridge.Event
	})
	if !ok {
		writeJSON(w, []agentbridge.Event{})
		return
	}
	writeJSON(w, pender.PendingRequests())
}

func (s *Server) handleAgentStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	var opts agentbridge.StartOptions
	if err := decodeAgentJSON(r, &opts); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.Agent.Start(ctx, opts); err != nil {
		if agentbridge.IsSessionLoadOverflow(err) {
			writeSessionLoadError(w, err, s.Agent.Status())
			return
		}
		writeAgentError(w, err)
		return
	}
	s.rememberAgentCwd(s.Agent.Status().Cwd)
	writeJSON(w, s.Agent.Status())
}

func (s *Server) handleAgentStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	if err := s.Agent.Stop(); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.Agent.Status())
}

func (s *Server) handleAgentCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	if err := s.Agent.CancelPrompt(); err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, s.Agent.Status())
}

func (s *Server) handleAgentSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	var request struct {
		Cwd string `json:"cwd"`
	}
	if err := decodeAgentJSON(r, &request); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.Agent.NewSession(ctx, request.Cwd); err != nil {
		writeAgentError(w, err)
		return
	}
	s.rememberAgentCwd(s.Agent.Status().Cwd)
	writeJSON(w, s.Agent.Status())
}

func (s *Server) handleAgentSessionLoad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	var opts agentbridge.StartOptions
	if err := decodeAgentJSON(r, &opts); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(opts.SessionID) == "" {
		writeError(w, errors.New("会话 ID 不能为空"), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.Agent.Start(ctx, opts); err != nil {
		if agentbridge.IsSessionLoadOverflow(err) {
			writeSessionLoadError(w, err, s.Agent.Status())
			return
		}
		writeAgentError(w, err)
		return
	}
	s.rememberAgentCwd(s.Agent.Status().Cwd)
	writeJSON(w, s.Agent.Status())
}

func (s *Server) handleAgentSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	sessions, err := s.Agent.ListStoredSessions(r.URL.Query().Get("query"), limit)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessions)
}

func (s *Server) handleAgentSessionHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/agent/sessions/")
	history, err := s.Agent.StoredSessionHistory(id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, err, status)
		return
	}
	writeJSON(w, history)
}

func (s *Server) handleAgentMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	mediaPath, err := s.resolveAgentMediaPath(r.URL.Query().Get("session_id"), r.URL.Query().Get("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(mediaPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(mediaPath)))
	if contentType == "" {
		header := make([]byte, 512)
		count, _ := file.Read(header)
		_, _ = file.Seek(0, io.SeekStart)
		contentType = http.DetectContentType(header[:count])
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if !strings.HasPrefix(mediaType, "image/") && !strings.HasPrefix(mediaType, "video/") && !strings.HasPrefix(mediaType, "audio/") {
		http.Error(w, "不支持的媒体类型", http.StatusUnsupportedMediaType)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(mediaPath), info.ModTime(), file)
}

func (s *Server) resolveAgentMediaPath(sessionID, reference string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	reference = strings.TrimSpace(reference)
	if sessionID == "" || reference == "" || strings.ContainsAny(sessionID, `/\\`) || s.Paths.GrokHome == "" {
		return "", os.ErrNotExist
	}
	sessionsRoot, err := filepath.Abs(filepath.Join(s.Paths.GrokHome, "sessions"))
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(sessionsRoot)
	if err != nil {
		return "", err
	}
	var sessionDir string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(sessionsRoot, entry.Name(), sessionID)
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			sessionDir = candidate
			break
		}
	}
	if sessionDir == "" {
		return "", os.ErrNotExist
	}
	sessionDir, err = filepath.EvalSymlinks(sessionDir)
	if err != nil {
		return "", err
	}

	referencePath, absolute, err := localMediaReferencePath(reference)
	if err != nil || referencePath == "" {
		return "", os.ErrNotExist
	}
	var candidate string
	if absolute {
		candidate = filepath.Clean(referencePath)
	} else {
		candidate = filepath.Join(sessionDir, filepath.FromSlash(strings.TrimLeft(referencePath, `/\\`)))
	}
	if resolved, ok := verifiedSessionMediaPath(sessionDir, candidate); ok {
		return resolved, nil
	}
	if hasPathTraversal(referencePath) {
		return "", os.ErrNotExist
	}

	// Some Grok builds return only /2.jpg even when the file is stored in a
	// generated subdirectory. Fall back to a bounded basename search in this
	// session, never outside it.
	name := filepath.Base(filepath.FromSlash(referencePath))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "", os.ErrNotExist
	}
	visited := 0
	found := ""
	err = filepath.WalkDir(sessionDir, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		visited++
		if visited > 10000 {
			return filepath.SkipAll
		}
		if !entry.IsDir() && strings.EqualFold(entry.Name(), name) {
			if resolved, ok := verifiedSessionMediaPath(sessionDir, current); ok {
				found = resolved
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil || found == "" {
		return "", os.ErrNotExist
	}
	return found, nil
}

func localMediaReferencePath(reference string) (string, bool, error) {
	if filepath.IsAbs(reference) {
		return reference, true, nil
	}
	parsed, err := url.Parse(reference)
	if err != nil {
		return "", false, err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "":
		return parsed.Path, false, nil
	case "file":
		if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
			return "", false, os.ErrPermission
		}
		decoded, decodeErr := url.PathUnescape(parsed.Path)
		if decodeErr != nil {
			return "", false, decodeErr
		}
		decoded = filepath.FromSlash(decoded)
		if len(decoded) >= 3 && (decoded[0] == '/' || decoded[0] == '\\') && decoded[2] == ':' {
			decoded = decoded[1:]
		}
		return decoded, true, nil
	case "http", "https":
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return "", false, os.ErrPermission
		}
		decoded, decodeErr := url.PathUnescape(parsed.Path)
		return decoded, false, decodeErr
	default:
		return "", false, os.ErrPermission
	}
}

func verifiedSessionMediaPath(sessionDir, candidate string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(sessionDir, resolved)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Stat(resolved)
	return resolved, err == nil && info.Mode().IsRegular()
}

func hasPathTraversal(value string) bool {
	for _, part := range strings.FieldsFunc(filepath.ToSlash(value), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

func (s *Server) handleAgentRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	var request struct {
		SessionID string `json:"session_id"`
		Title     string `json:"title"`
	}
	if err := decodeAgentJSON(r, &request); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.SessionID) == "" {
		writeError(w, errors.New("会话 ID 不能为空"), http.StatusBadRequest)
		return
	}
	if err := s.Agent.RenameStoredSession(request.SessionID, request.Title); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := decodeAgentJSON(r, &request); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.SessionID) == "" {
		writeError(w, errors.New("会话 ID 不能为空"), http.StatusBadRequest)
		return
	}
	if err := s.Agent.DeleteStoredSession(request.SessionID); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, err, status)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type agentSocketMessage struct {
	Type        string                   `json:"type"`
	Text        string                   `json:"text,omitempty"`
	Model       string                   `json:"model,omitempty"`
	Strength    string                   `json:"strength,omitempty"`
	RequestID   string                   `json:"request_id,omitempty"`
	Allow       bool                     `json:"allow,omitempty"`
	Remember    bool                     `json:"remember,omitempty"`
	OptionID    string                   `json:"option_id,omitempty"`
	Outcome     string                   `json:"outcome,omitempty"`
	Feedback    string                   `json:"feedback,omitempty"`
	Attachments []agentbridge.Attachment `json:"attachments,omitempty"`
	// Scope 仅 permission_response 使用："user" 时"总是允许"沉淀为
	// permissions.json 持久规则（native）；空/"session" 维持原语义。
	Scope string `json:"scope,omitempty"`
	// Hidden 仅 client_visibility 使用：页面 visibilitychange 上报。
	Hidden bool `json:"hidden,omitempty"`
}

func (s *Server) handleAgentWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	if !agentWebSocketOriginAllowed(r) {
		http.Error(w, "请求来源不受信任", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	// Path-only attachments keep payloads small; 256 KiB covers multi-file metadata.
	conn.SetReadLimit(256 << 10)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	subscriberID, events := s.Agent.Subscribe()
	defer s.Agent.Unsubscribe(subscriberID)
	replies := make(chan agentbridge.Event, 16)
	// 每连接可见性：默认可见，client_visibility 消息翻转；全局计数
	// 为 0 时桌面通知接管（见 startAgentNotifyLoop）。
	connHidden := &atomic.Bool{}
	s.agentVisible.Add(1)
	defer func() {
		if !connHidden.Load() {
			s.agentVisible.Add(-1)
		}
	}()
	go s.readAgentSocket(ctx, cancel, conn, replies, connHidden)

	status := s.Agent.Status()
	auto := status.SessionAutoApprove
	if err := wsjson.Write(ctx, conn, agentbridge.Event{
		Type: "agent_status", SessionID: status.SessionID, Status: status.State,
		Model: status.Model, Error: status.Error, SessionAutoApprove: &auto,
	}); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-events:
			if err := wsjson.Write(ctx, conn, event); err != nil {
				return
			}
		case event := <-replies:
			if err := wsjson.Write(ctx, conn, event); err != nil {
				return
			}
		}
	}
}

func (s *Server) readAgentSocket(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, replies chan<- agentbridge.Event, connHidden *atomic.Bool) {
	defer cancel()
	for {
		var message agentSocketMessage
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return
		}
		var err error
		switch message.Type {
		case "user_message":
			if message.Model != "" || message.Strength != "" {
				if setErr := s.Agent.SetSessionConfig(ctx, message.Model, message.Strength); setErr != nil {
					fmt.Fprintf(os.Stderr, "grok_switch: set session config failed: %v\n", setErr)
				}
			}
			promptText, expandErr := s.expandSkillPrompt(message.Text)
			if expandErr != nil {
				err = fmt.Errorf("加载 Skill 失败: %w", expandErr)
			} else {
				err = s.Agent.Prompt(promptText, message.Attachments)
			}
		case "cancel":
			err = s.Agent.CancelPrompt()
		case "permission_response":
			if message.Scope == "user" {
				// "总是允许"写持久规则：native 走 AddUserRule；其他引擎
				// 回落到其自身的 always-allow 语义（ACP 由 CLI 内部记住）。
				if ruler, ok := s.Agent.(interface {
					RespondPermissionUser(string, bool) error
				}); ok {
					err = ruler.RespondPermissionUser(message.RequestID, message.Allow)
				} else {
					err = s.Agent.RespondPermissionEx(message.RequestID, message.Allow, true)
				}
			} else if strings.TrimSpace(message.OptionID) != "" {
				err = s.Agent.RespondPermissionOption(message.RequestID, message.OptionID, message.Remember)
			} else {
				err = s.Agent.RespondPermissionEx(message.RequestID, message.Allow, message.Remember)
			}
		case "client_visibility":
			if message.Hidden {
				if !connHidden.Swap(true) {
					s.agentVisible.Add(-1)
				}
			} else if connHidden.Swap(false) {
				s.agentVisible.Add(1)
			}
			continue
		case "plan_response":
			err = s.Agent.RespondPlan(message.RequestID, agentbridge.PlanDecision{
				Outcome: message.Outcome, Feedback: message.Feedback,
			})
		case "set_session_auto_approve":
			s.Agent.SetSessionAutoApprove(message.Allow || message.Remember)
			// Status broadcast is emitted by SetSessionAutoApprove.
			continue
		case "ping":
			replies <- agentbridge.Event{Type: "pong"}
			continue
		default:
			err = fmt.Errorf("不支持的消息类型: %s", message.Type)
		}
		if err != nil {
			select {
			case replies <- agentbridge.Event{Type: "error", Error: err.Error()}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (s *Server) rememberAgentCwd(cwd string) {
	if s.Settings == nil || strings.TrimSpace(cwd) == "" {
		return
	}
	// 不持久化已失效的目录（浏览器端 localStorage 可能回传旧路径）。
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return
	}
	current, err := s.Settings.Get()
	if err != nil || current.AgentDefaultCwd == cwd {
		return
	}
	current.AgentDefaultCwd = cwd
	_, _ = s.Settings.Update(current)
}

func decodeAgentJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("请求内容无效: %w", err)
	}
	return nil
}

func writeAgentError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, agentbridge.ErrBusy) {
		status = http.StatusConflict
	} else if errors.Is(err, agentbridge.ErrNotRunning) {
		status = http.StatusServiceUnavailable
	} else if strings.Contains(err.Error(), "工作目录") || strings.Contains(err.Error(), "消息不能为空") ||
		strings.Contains(err.Error(), "没有正在生成") {
		status = http.StatusBadRequest
	}
	writeError(w, err, status)
}

func writeSessionLoadError(w http.ResponseWriter, err error, status agentbridge.Status) {
	restarted := false
	var loadErr *agentbridge.SessionLoadError
	if errors.As(err, &loadErr) {
		restarted = loadErr.Recovered()
	}
	// When recovery created a fresh session with bootstrap armed, the UI may continue chatting.
	bootstrapReady := restarted && status.NeedsBootstrap && status.State == "ready"
	writeJSONStatus(w, struct {
		Error           string             `json:"error"`
		Code            string             `json:"code"`
		ReadonlyHistory bool               `json:"readonly_history"`
		Recoverable     bool               `json:"recoverable"`
		EngineLoaded    bool               `json:"engine_loaded"`
		AgentRestarted  bool               `json:"agent_restarted"`
		NeedsBootstrap  bool               `json:"needs_bootstrap"`
		Status          agentbridge.Status `json:"status"`
	}{
		Error:           err.Error(),
		Code:            agentbridge.SessionLoadOverflowCode,
		ReadonlyHistory: !bootstrapReady,
		Recoverable:     true,
		EngineLoaded:    false,
		AgentRestarted:  restarted,
		NeedsBootstrap:  status.NeedsBootstrap,
		Status:          status,
	}, http.StatusConflict)
}

func (s *Server) handleAgentRewind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		DropLastUser bool `json:"drop_last_user"`
		RestoreFiles bool `json:"restore_files"`
	}
	if err := decodeAgentJSON(r, &req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if !req.DropLastUser {
		writeError(w, errors.New("目前仅支持 drop_last_user"), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result := s.Agent.RewindDropLastUser(ctx, req.RestoreFiles)
	writeJSON(w, result)
}

func (s *Server) handleAgentPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		RequestID string `json:"request_id"`
		Outcome   string `json:"outcome"`
		Feedback  string `json:"feedback"`
	}
	if err := decodeAgentJSON(r, &req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if err := s.Agent.RespondPlan(req.RequestID, agentbridge.PlanDecision{
		Outcome: req.Outcome, Feedback: req.Feedback,
	}); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, agentbridge.ErrPlanNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, err, status)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleAgentBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		Clear     bool   `json:"clear"`
	}
	if err := decodeAgentJSON(r, &req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if req.Clear {
		s.Agent.ClearBootstrap()
		writeJSON(w, s.Agent.Status())
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeError(w, errors.New("session_id 不能为空"), http.StatusBadRequest)
		return
	}
	if err := s.Agent.ArmBootstrapFromSession(req.SessionID); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, s.Agent.Status())
}

// handleAgentFork 复制 native 会话为新会话（POST {session_id, upto_seq}）。
// ACP/Grok CLI 会话存储不透明，硬分叉不可靠——非 native 引擎返回 400。
func (s *Server) handleAgentFork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	var request struct {
		SessionID string `json:"session_id"`
		UptoSeq   int64  `json:"upto_seq"`
	}
	if err := decodeAgentJSON(r, &request); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.SessionID) == "" {
		writeError(w, errors.New("会话 ID 不能为空"), http.StatusBadRequest)
		return
	}
	forker, ok := s.Agent.(interface {
		ForkStoredSession(context.Context, string, int64) (agentbridge.SessionSummary, error)
	})
	if !ok {
		writeError(w, errors.New("当前引擎不支持会话分叉"), http.StatusBadRequest)
		return
	}
	meta, err := forker.ForkStoredSession(r.Context(), request.SessionID, request.UptoSeq)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, err, status)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "session": meta})
}

// handleAgentExport 把会话历史渲染为 Markdown 下载（GET ?id=）。
// 两种引擎统一走 StoredSessionHistory（native 读 transcript 记录，
// ACP 读 chat_history.jsonl）。
func (s *Server) handleAgentExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Agent == nil {
		writeError(w, errors.New("Agent 服务未初始化"), http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeError(w, errors.New("会话 ID 不能为空"), http.StatusBadRequest)
		return
	}
	history, err := s.Agent.StoredSessionHistory(id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, err, status)
		return
	}
	title := strings.TrimSpace(history.Session.Title)
	if title == "" {
		title = "会话 " + id
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "session-" + id + ".md"}))
	_, _ = w.Write([]byte(sessionMarkdown(history, title)))
}

// sessionMarkdown 把会话历史渲染为 Markdown：正文直排，思考与工具
// 调用收进 <details> 折叠块，保持导出文档可读而不丢信息。
func sessionMarkdown(history agentbridge.SessionHistory, title string) string {
	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	var meta []string
	if history.Session.Model != "" {
		meta = append(meta, "模型: "+history.Session.Model)
	}
	if history.Session.Cwd != "" {
		meta = append(meta, "目录: `"+history.Session.Cwd+"`")
	}
	if !history.Session.UpdatedAt.IsZero() {
		meta = append(meta, "更新于: "+history.Session.UpdatedAt.Format("2006-01-02 15:04"))
	}
	if len(meta) > 0 {
		b.WriteString("*" + strings.Join(meta, " · ") + "*\n\n---\n")
	}
	fence := func(text string) {
		// 输出含 ``` 时换用更长围栏，避免围栏被撞穿。
		f := "```"
		for strings.Contains(text, f) {
			f += "`"
		}
		b.WriteString("\n" + f + "\n" + text + "\n" + f + "\n")
	}
	for _, m := range history.Messages {
		switch m.Role {
		case "user":
			b.WriteString("\n## 用户\n\n" + strings.TrimSpace(m.Content) + "\n")
		case "assistant":
			head := "\n## Grok"
			if m.Model != "" {
				head += "（" + m.Model + "）"
			}
			b.WriteString(head + "\n\n" + strings.TrimSpace(m.Content) + "\n")
		case "thought":
			b.WriteString("\n<details><summary>思考过程</summary>\n")
			fence(strings.TrimSpace(m.Content))
			b.WriteString("</details>\n")
		case "tool":
			name := "tool"
			if m.Tool != nil && m.Tool.Title != "" {
				name = m.Tool.Title
			}
			b.WriteString("\n<details><summary>工具调用: " + name + "</summary>\n")
			if m.Tool != nil && m.Tool.RawInput != nil {
				if raw, err := json.Marshal(m.Tool.RawInput); err == nil {
					fence(string(raw))
				}
			}
			b.WriteString("</details>\n")
		case "tool_result":
			if strings.TrimSpace(m.Content) != "" {
				b.WriteString("\n<details><summary>工具结果</summary>\n")
				fence(strings.TrimSpace(m.Content))
				b.WriteString("</details>\n")
			}
		}
	}
	return b.String()
}

// startAgentNotifyLoop 常驻订阅 agent 事件：当没有任何可见的 agent 页面
// （全部 hidden 或没有 WS 连接）时，审批请求与回合结束补发桌面通知。
func (s *Server) startAgentNotifyLoop() {
	if s.Agent == nil {
		return
	}
	_, events := s.Agent.Subscribe()
	go func() {
		for ev := range events {
			s.notifyAgentEventIfHidden(ev)
		}
	}()
}

// notifyAgentEventIfHidden 在 permission_request / turn_done 且页面不可见时
// 发桌面通知。同 key 5 秒内去重（pending 审批重放不会连环轰炸）。
func (s *Server) notifyAgentEventIfHidden(ev agentbridge.Event) {
	if s.agentVisible.Load() > 0 {
		return
	}
	var title, body, key string
	switch ev.Type {
	case "permission_request":
		title = "Grok 需要审批"
		body = "返回工作台确认工具执行"
		key = "perm"
		if ev.Permission != nil {
			if ev.Permission.Summary != "" {
				body = ev.Permission.Summary
			}
			key = "perm:" + ev.Permission.RequestID
		}
	case "turn_done":
		title = "Grok 对话完成"
		body = "点击回到工作台查看回复"
		key = "turn:" + ev.SessionID
	default:
		return
	}
	s.agentNotifyMu.Lock()
	if s.agentNotifyAt == nil {
		s.agentNotifyAt = map[string]time.Time{}
	}
	if last, ok := s.agentNotifyAt[key]; ok && time.Since(last) < 5*time.Second {
		s.agentNotifyMu.Unlock()
		return
	}
	s.agentNotifyAt[key] = time.Now()
	s.agentNotifyMu.Unlock()
	agentNotifyInfo(title, body)
}

// agentNotifyInfo 是桌面通知出口（测试可替换，避免真弹系统通知）。
var agentNotifyInfo = notify.Info

func agentWebSocketOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return isLoopbackRequest(r)
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host != r.Host {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}
