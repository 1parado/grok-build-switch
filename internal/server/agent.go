package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	Prompt(string) error
	Subscribe() (string, <-chan agentbridge.Event)
	Unsubscribe(string)
	RespondPermission(string, bool) error
	ListStoredSessions(string, int) ([]agentbridge.SessionSummary, error)
	StoredSessionHistory(string) (agentbridge.SessionHistory, error)
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

type agentSocketMessage struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Allow     bool   `json:"allow,omitempty"`
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
	if err := wsjson.Write(ctx, conn, agentbridge.Event{
		Type: "agent_status", SessionID: status.SessionID, Status: status.State, Model: status.Model, Error: status.Error,
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
			err = s.Agent.Prompt(message.Text)
		case "permission_response":
			err = s.Agent.RespondPermission(message.RequestID, message.Allow)
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
	} else if strings.Contains(err.Error(), "工作目录") || strings.Contains(err.Error(), "消息不能为空") {
		status = http.StatusBadRequest
	}
	writeError(w, err, status)
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
