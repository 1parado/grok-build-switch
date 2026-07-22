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
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"grok_switch/internal/agentbridge"
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
	SetSessionAutoApprove(bool)
	ListStoredSessions(string, int) ([]agentbridge.SessionSummary, error)
	StoredSessionHistory(string) (agentbridge.SessionHistory, error)
	RenameStoredSession(string, string) error
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

type agentSocketMessage struct {
	Type        string                   `json:"type"`
	Text        string                   `json:"text,omitempty"`
	RequestID   string                   `json:"request_id,omitempty"`
	Allow       bool                     `json:"allow,omitempty"`
	Remember    bool                     `json:"remember,omitempty"`
	Attachments []agentbridge.Attachment `json:"attachments,omitempty"`
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
	conn.SetReadLimit(64 << 10)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	subscriberID, events := s.Agent.Subscribe()
	defer s.Agent.Unsubscribe(subscriberID)
	replies := make(chan agentbridge.Event, 16)
	go s.readAgentSocket(ctx, cancel, conn, replies)

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

func (s *Server) readAgentSocket(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, replies chan<- agentbridge.Event) {
	defer cancel()
	for {
		var message agentSocketMessage
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return
		}
		var err error
		switch message.Type {
		case "user_message":
			err = s.Agent.Prompt(message.Text, message.Attachments)
		case "cancel":
			err = s.Agent.CancelPrompt()
		case "permission_response":
			err = s.Agent.RespondPermissionEx(message.RequestID, message.Allow, message.Remember)
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
	writeJSONStatus(w, struct {
		Error           string             `json:"error"`
		Code            string             `json:"code"`
		ReadonlyHistory bool               `json:"readonly_history"`
		Recoverable     bool               `json:"recoverable"`
		EngineLoaded    bool               `json:"engine_loaded"`
		AgentRestarted  bool               `json:"agent_restarted"`
		Status          agentbridge.Status `json:"status"`
	}{
		Error:           err.Error(),
		Code:            agentbridge.SessionLoadOverflowCode,
		ReadonlyHistory: true,
		Recoverable:     true,
		EngineLoaded:    false,
		AgentRestarted:  restarted,
		Status:          status,
	}, http.StatusConflict)
}

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
