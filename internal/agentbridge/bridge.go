package agentbridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

var (
	ErrBusy               = errors.New("Grok Agent 正在处理上一条消息")
	ErrNotRunning         = errors.New("Grok Agent 尚未启动")
	ErrPermissionNotFound = errors.New("权限请求已失效")
)

const SessionLoadOverflowCode = "session_load_overflow"

// SessionLoadError reports a session/load failure that left the ACP
// connection unusable. The bridge attempts to start a fresh session before
// returning this error so callers can keep the stored history as a read-only
// preview while still offering a working new conversation.
type SessionLoadError struct {
	Cause       error
	RecoveryErr error
}

func (e *SessionLoadError) Error() string {
	message := "会话过大或恢复时通知过多，引擎上下文未能加载。已展示本地历史（只读）"
	if e.RecoveryErr == nil {
		return message + "；Agent 已自动重启，可开启新对话"
	}
	return fmt.Sprintf("%s；Agent 自动重启失败: %v", message, e.RecoveryErr)
}

func (e *SessionLoadError) Unwrap() error { return e.Cause }

func (e *SessionLoadError) Recovered() bool { return e.RecoveryErr == nil }

func IsSessionLoadOverflow(err error) bool {
	if err == nil {
		return false
	}
	var loadErr *SessionLoadError
	if errors.As(err, &loadErr) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"notification queue overflow",
		"peer disconnected before response",
		"peer connection closed",
		"connection closed",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

type pendingPermission struct {
	options []acp.PermissionOption
	result  chan acp.PermissionOptionId
}

type Bridge struct {
	grokHome string
	logPath  string
	override string

	opMu sync.Mutex
	mu   sync.RWMutex

	state              string
	grokPath           string
	lastError          string
	defaultCwd         string
	cwd                string
	sessionID          string
	alwaysApprove      bool
	sessionAutoApprove bool
	busy               bool
	model              string
	suppressUpdates    atomic.Bool
	generation         uint64
	cmd                *exec.Cmd
	cancel             context.CancelFunc
	promptCancel       context.CancelFunc
	processDone        chan struct{}
	closeJob           func()
	conn               *acp.ClientSideConnection
	outputFilter       *sessionLoadNotificationFilter
	processCtx         context.Context

	subscribers map[string]chan Event
	subCounter  atomic.Uint64
	permissions map[string]*pendingPermission
	permCounter atomic.Uint64
}

func New(grokHome, logPath string) *Bridge {
	defaultCwd, err := os.Getwd()
	if err != nil {
		defaultCwd = grokHome
	}
	return &Bridge{
		grokHome:    grokHome,
		logPath:     logPath,
		state:       "idle",
		defaultCwd:  defaultCwd,
		subscribers: map[string]chan Event{},
		permissions: map[string]*pendingPermission{},
	}
}

func (b *Bridge) SetExecutable(path string) {
	b.mu.Lock()
	b.override = strings.TrimSpace(path)
	b.mu.Unlock()
}

func (b *Bridge) SetDefaultCwd(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	b.mu.Lock()
	b.defaultCwd = filepath.Clean(path)
	b.mu.Unlock()
}

func (b *Bridge) Status() Status {
	b.mu.RLock()
	status := Status{
		GrokPath:           b.grokPath,
		Running:            b.cmd != nil && (b.state == "starting" || b.state == "ready" || b.state == "busy"),
		State:              b.state,
		SessionID:          b.sessionID,
		Cwd:                b.cwd,
		DefaultCwd:         b.defaultCwd,
		Busy:               b.busy,
		AlwaysApprove:      b.alwaysApprove,
		SessionAutoApprove: b.sessionAutoApprove,
		Model:              b.model,
		Error:              b.lastError,
	}
	override := b.override
	b.mu.RUnlock()
	resolved, err := ResolveExecutable(override, b.grokHome)
	if err == nil {
		status.Available = true
		if status.GrokPath == "" {
			status.GrokPath = resolved
		}
	} else if status.Error == "" {
		status.Error = err.Error()
	}
	return status
}

func (b *Bridge) Start(ctx context.Context, opts StartOptions) error {
	b.opMu.Lock()
	defer b.opMu.Unlock()
	return b.startLocked(ctx, opts)
}

func (b *Bridge) startLocked(ctx context.Context, opts StartOptions) error {
	cwd, err := normalizeCwd(opts.Cwd, b.defaultCwd)
	if err != nil {
		return err
	}
	b.mu.RLock()
	alreadyRunning := b.cmd != nil
	sameProcessMode := b.alwaysApprove == opts.AlwaysApprove
	sameCwd := filepath.Clean(b.cwd) == cwd
	override := b.override
	b.mu.RUnlock()
	if alreadyRunning && sameProcessMode {
		if opts.SessionID != "" {
			err := b.loadSessionLocked(ctx, opts.SessionID, cwd)
			if IsSessionLoadOverflow(err) {
				return b.recoverSessionLoadLocked(opts, err)
			}
			return err
		}
		if sameCwd {
			return nil
		}
		return b.newSessionLocked(ctx, cwd)
	}
	if alreadyRunning {
		b.stopLocked()
	}

	grokPath, err := ResolveExecutable(override, b.grokHome)
	if err != nil {
		b.setDead(err)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.logPath), 0o755); err != nil {
		return fmt.Errorf("创建 Agent 日志目录失败: %w", err)
	}
	logFile, err := os.OpenFile(b.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("打开 Agent 日志失败: %w", err)
	}

	processCtx, cancel := context.WithCancel(context.Background())
	args := []string{"agent"}
	if opts.AlwaysApprove {
		args = append(args, "--always-approve")
	}
	args = append(args, "stdio")
	cmd := exec.CommandContext(processCtx, grokPath, args...)
	configureCommand(cmd)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GROK_HOME="+b.grokHome)
	cmd.Stderr = logFile
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		_ = logFile.Close()
		return fmt.Errorf("创建 Grok Agent stdin 失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = logFile.Close()
		return fmt.Errorf("创建 Grok Agent stdout 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = logFile.Close()
		b.setDead(err)
		return fmt.Errorf("启动 Grok Agent 失败: %w", err)
	}
	closeJob, jobErr := attachProcessTree(cmd.Process)
	if jobErr != nil {
		_, _ = fmt.Fprintf(logFile, "grok_switch: attach process job: %v\n", jobErr)
		closeJob = func() {}
	}
	outputFilter := newSessionLoadNotificationFilter(stdout, &b.suppressUpdates)
	conn := acp.NewClientSideConnection(b, stdin, outputFilter)
	done := make(chan struct{})

	b.mu.Lock()
	b.generation++
	generation := b.generation
	b.state = "starting"
	b.grokPath = grokPath
	b.lastError = ""
	b.cwd = cwd
	b.sessionID = ""
	b.alwaysApprove = opts.AlwaysApprove
	b.busy = false
	b.model = ""
	b.suppressUpdates.Store(false)
	b.cmd = cmd
	b.cancel = cancel
	b.processDone = done
	b.closeJob = closeJob
	b.conn = conn
	b.outputFilter = outputFilter
	b.processCtx = processCtx
	b.mu.Unlock()
	b.broadcastStatus()
	go b.waitProcess(generation, cmd, logFile, done)

	initializeResponse, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: 1,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{},
			Terminal: false,
		},
		ClientInfo: &acp.Implementation{Name: "grok_switch", Version: "1"},
	})
	if err != nil {
		b.stopLocked()
		wrapped := fmt.Errorf("Grok Agent initialize 失败: %w", err)
		b.setDead(wrapped)
		return wrapped
	}
	if model := modelFromMeta(initializeResponse.Meta); model != "" {
		b.setModel(model)
	}
	if opts.SessionID != "" {
		err = b.loadSessionLocked(ctx, opts.SessionID, cwd)
	} else {
		err = b.newSessionLocked(ctx, cwd)
	}
	if err != nil {
		if IsSessionLoadOverflow(err) {
			return b.recoverSessionLoadLocked(opts, err)
		}
		b.stopLocked()
		b.setDead(err)
		return err
	}
	return nil
}

func (b *Bridge) recoverSessionLoadLocked(opts StartOptions, cause error) error {
	fmt.Fprintf(os.Stderr, "grok_switch: session load failed session=%s: %v\n", opts.SessionID, cause)
	b.stopLocked()
	recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	recoveryErr := b.startLocked(recoveryCtx, StartOptions{
		Cwd:           opts.Cwd,
		AlwaysApprove: opts.AlwaysApprove,
	})
	if recoveryErr != nil {
		fmt.Fprintf(os.Stderr, "grok_switch: session load recovery failed session=%s: %v\n", opts.SessionID, recoveryErr)
	}
	return &SessionLoadError{Cause: cause, RecoveryErr: recoveryErr}
}

func (b *Bridge) NewSession(ctx context.Context, cwd string) error {
	b.opMu.Lock()
	defer b.opMu.Unlock()
	normalized, err := normalizeCwd(cwd, b.defaultCwd)
	if err != nil {
		return err
	}
	return b.newSessionLocked(ctx, normalized)
}

func (b *Bridge) newSessionLocked(ctx context.Context, cwd string) error {
	b.mu.RLock()
	conn := b.conn
	running := b.cmd != nil
	busy := b.busy
	b.mu.RUnlock()
	if !running || conn == nil {
		return ErrNotRunning
	}
	if busy {
		return ErrBusy
	}
	response, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		return fmt.Errorf("创建 Grok 会话失败: %w", err)
	}
	for _, opt := range response.ConfigOptions {
		if sel := opt.Select; sel != nil {
			var vals []string
			if sel.Options.Ungrouped != nil {
				for _, o := range *sel.Options.Ungrouped {
					vals = append(vals, string(o.Value))
				}
			} else if sel.Options.Grouped != nil {
				for _, g := range *sel.Options.Grouped {
					for _, o := range g.Options {
						vals = append(vals, string(o.Value))
					}
				}
			}
			fmt.Fprintf(os.Stderr, "grok_switch: config option id=%q name=%q category=%v values=%v\n", sel.Id, sel.Name, sel.Category, vals)
		}
	}
	b.mu.Lock()
	b.cwd = cwd
	b.sessionID = string(response.SessionId)
	b.sessionAutoApprove = false
	if model := modelFromMeta(response.Meta); model != "" {
		b.model = model
	}
	b.state = "ready"
	b.lastError = ""
	b.mu.Unlock()
	b.broadcastStatus()
	return nil
}

func (b *Bridge) loadSessionLocked(ctx context.Context, sessionID, cwd string) error {
	b.mu.RLock()
	conn := b.conn
	outputFilter := b.outputFilter
	running := b.cmd != nil
	busy := b.busy
	b.mu.RUnlock()
	if !running || conn == nil {
		return ErrNotRunning
	}
	if busy {
		return ErrBusy
	}
	_, summary, err := b.findStoredSession(sessionID)
	if err != nil {
		return fmt.Errorf("读取历史会话失败: %w", err)
	}
	if filepath.Clean(summary.Info.Cwd) != filepath.Clean(cwd) {
		return fmt.Errorf("会话工作目录不匹配: %s", summary.Info.Cwd)
	}
	var droppedBefore uint64
	if outputFilter != nil {
		droppedBefore = outputFilter.Dropped()
	}
	b.mu.Lock()
	b.busy = true
	b.mu.Unlock()
	b.suppressUpdates.Store(true)
	response, err := conn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId: acp.SessionId(sessionID), Cwd: cwd, McpServers: []acp.McpServer{},
	})
	b.suppressUpdates.Store(false)
	if outputFilter != nil {
		if dropped := outputFilter.Dropped() - droppedBefore; dropped > 0 {
			fmt.Fprintf(os.Stderr, "grok_switch: suppressed session replay notifications session=%s count=%d\n", sessionID, dropped)
		}
	}
	b.mu.Lock()
	b.busy = false
	if err == nil {
		b.cwd = cwd
		b.sessionID = sessionID
		b.sessionAutoApprove = false
		b.state = "ready"
		b.lastError = ""
		b.model = summary.CurrentModelID
		if model := modelFromMeta(response.Meta); model != "" {
			b.model = model
		}
	}
	b.mu.Unlock()
	if err != nil {
		return fmt.Errorf("恢复 Grok 会话失败: %w", err)
	}
	b.broadcastStatus()
	return nil
}

func (b *Bridge) setModel(model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	b.mu.Lock()
	b.model = model
	b.mu.Unlock()
}

func modelFromMeta(meta map[string]any) string {
	for _, key := range []string{"x.ai/sessionDetail", "modelState"} {
		value, ok := meta[key].(map[string]any)
		if !ok {
			continue
		}
		for _, modelKey := range []string{"currentModelId", "modelId"} {
			if model, ok := value[modelKey].(string); ok && strings.TrimSpace(model) != "" {
				return model
			}
		}
	}
	return ""
}

func (b *Bridge) Prompt(text string, attachments []Attachment) error {
	text = strings.TrimSpace(text)
	blocks := buildPromptBlocks(text, attachments)
	if len(blocks) == 0 {
		return errors.New("消息不能为空")
	}
	b.mu.Lock()
	if b.cmd == nil || b.conn == nil || b.sessionID == "" || b.processCtx == nil {
		b.mu.Unlock()
		return ErrNotRunning
	}
	if b.busy {
		b.mu.Unlock()
		return ErrBusy
	}
	turnCtx, turnCancel := context.WithCancel(b.processCtx)
	b.busy = true
	b.state = "busy"
	b.lastError = ""
	b.promptCancel = turnCancel
	conn := b.conn
	sessionID := b.sessionID
	generation := b.generation
	b.mu.Unlock()
	b.broadcastStatus()
	go b.runPrompt(turnCtx, turnCancel, generation, conn, sessionID, blocks)
	return nil
}

// buildPromptBlocks turns the user's text + attachments into ACP content
// blocks. Images become inline image blocks (base64); text files are folded
// into the prompt as labelled snippets.
func buildPromptBlocks(text string, attachments []Attachment) []acp.ContentBlock {
	blocks := make([]acp.ContentBlock, 0, 1+len(attachments))
	if text != "" {
		blocks = append(blocks, acp.TextBlock(text))
	}
	const textFileMax = 20000
	for _, a := range attachments {
		switch strings.ToLower(strings.TrimSpace(a.Kind)) {
		case "image":
			if a.Data != "" && a.MimeType != "" {
				blocks = append(blocks, acp.ImageBlock(a.Data, a.MimeType))
			}
		case "text_file":
			snippet := strings.TrimSpace(a.Text)
			if snippet == "" {
				continue
			}
			if len(snippet) > textFileMax {
				snippet = snippet[:textFileMax] + "\n…（已截断，仅发送前 20000 字符）"
			}
			name := strings.TrimSpace(a.Name)
			if name != "" {
				blocks = append(blocks, acp.TextBlock(fmt.Sprintf("【附件：%s】\n%s", name, snippet)))
			} else {
				blocks = append(blocks, acp.TextBlock(snippet))
			}
		}
	}
	return blocks
}

// CancelPrompt stops the in-flight turn via ACP session/cancel (context cancel).
func (b *Bridge) CancelPrompt() error {
	b.mu.Lock()
	if b.cmd == nil || b.conn == nil || b.sessionID == "" {
		b.mu.Unlock()
		return ErrNotRunning
	}
	if !b.busy || b.promptCancel == nil {
		b.mu.Unlock()
		return errors.New("当前没有正在生成的回复")
	}
	cancel := b.promptCancel
	sessionID := b.sessionID
	conn := b.conn
	b.mu.Unlock()
	cancel()
	// Best-effort explicit cancel in case the prompt request has not registered yet.
	_ = conn.Cancel(context.Background(), acp.CancelNotification{SessionId: acp.SessionId(sessionID)})
	return nil
}

func (b *Bridge) SetSessionAutoApprove(enabled bool) {
	b.mu.Lock()
	b.sessionAutoApprove = enabled
	b.mu.Unlock()
	b.broadcastStatus()
}

// SetSessionConfig sets model and/or reasoning effort on the current session
// via ACP SetSessionConfigOption. Empty strings are skipped.
func (b *Bridge) SetSessionConfig(ctx context.Context, model, strength string) error {
	b.mu.RLock()
	conn := b.conn
	sessionID := b.sessionID
	b.mu.RUnlock()
	if conn == nil || sessionID == "" {
		return ErrNotRunning
	}

	if model != "" {
		fmt.Fprintf(os.Stderr, "grok_switch: setting model config id=\"model\" value=%s\n", model)
		if _, err := conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
			ValueId: &acp.SetSessionConfigOptionValueId{
				SessionId: acp.SessionId(sessionID),
				ConfigId:  "model",
				Value:     acp.SessionConfigValueId(model),
			},
		}); err != nil {
			return fmt.Errorf("设置模型 %s 失败: %w", model, err)
		}
		b.mu.Lock()
		b.model = model
		b.mu.Unlock()
		b.broadcastStatus()
	}

	if strength != "" {
		if _, err := conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
			ValueId: &acp.SetSessionConfigOptionValueId{
				SessionId: acp.SessionId(sessionID),
				ConfigId:  "thought_level",
				Value:     acp.SessionConfigValueId(strength),
			},
		}); err != nil {
			return fmt.Errorf("设置推理强度 %s 失败: %w", strength, err)
		}
	}

	return nil
}

func (b *Bridge) runPrompt(ctx context.Context, turnCancel context.CancelFunc, generation uint64, conn *acp.ClientSideConnection, sessionID string, blocks []acp.ContentBlock) {
	defer turnCancel()
	response, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: acp.SessionId(sessionID),
		Prompt:    blocks,
	})
	cancelled := errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) ||
		(err != nil && strings.Contains(strings.ToLower(err.Error()), "cancel"))
	if err == nil && string(response.StopReason) == string(acp.StopReasonCancelled) {
		cancelled = true
	}
	b.mu.Lock()
	if generation != b.generation {
		b.mu.Unlock()
		return
	}
	b.busy = false
	b.promptCancel = nil
	if cancelled {
		b.state = "ready"
		b.lastError = ""
	} else if err != nil {
		b.state = "ready"
		b.lastError = err.Error()
	} else {
		b.state = "ready"
		b.lastError = ""
	}
	b.mu.Unlock()
	if cancelled {
		b.broadcast(Event{Type: "turn_done", SessionID: sessionID, StopReason: string(acp.StopReasonCancelled)})
	} else if err != nil {
		b.broadcast(Event{Type: "error", SessionID: sessionID, Error: fmt.Sprintf("Grok 对话失败: %v", err)})
	} else {
		b.broadcast(Event{Type: "turn_done", SessionID: sessionID, StopReason: string(response.StopReason)})
	}
	b.broadcastStatus()
}

func (b *Bridge) Stop() error {
	b.opMu.Lock()
	defer b.opMu.Unlock()
	b.stopLocked()
	return nil
}

func (b *Bridge) stopLocked() {
	b.mu.Lock()
	if b.cmd == nil {
		b.state = "idle"
		b.busy = false
		b.sessionID = ""
		b.lastError = ""
		b.mu.Unlock()
		b.broadcastStatus()
		return
	}
	b.state = "stopping"
	b.busy = false
	b.sessionAutoApprove = false
	cancel := b.cancel
	promptCancel := b.promptCancel
	closeJob := b.closeJob
	process := b.cmd.Process
	done := b.processDone
	b.closeJob = nil
	b.cancel = nil
	b.promptCancel = nil
	b.mu.Unlock()
	b.broadcastStatus()
	if promptCancel != nil {
		promptCancel()
	}
	if cancel != nil {
		cancel()
	}
	if closeJob != nil {
		closeJob()
	}
	if process != nil {
		_ = process.Kill()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	b.mu.Lock()
	b.state = "idle"
	b.cmd = nil
	b.cancel = nil
	b.promptCancel = nil
	b.processDone = nil
	b.closeJob = nil
	b.conn = nil
	b.outputFilter = nil
	b.processCtx = nil
	b.sessionID = ""
	b.busy = false
	b.sessionAutoApprove = false
	b.lastError = ""
	b.permissions = map[string]*pendingPermission{}
	b.mu.Unlock()
	b.broadcastStatus()
}

func (b *Bridge) waitProcess(generation uint64, cmd *exec.Cmd, logFile *os.File, done chan struct{}) {
	err := cmd.Wait()
	_ = logFile.Close()
	defer close(done)
	b.mu.Lock()
	if generation != b.generation || b.cmd != cmd {
		b.mu.Unlock()
		return
	}
	stopping := b.state == "stopping"
	closeJob := b.closeJob
	promptCancel := b.promptCancel
	b.cmd = nil
	b.cancel = nil
	b.promptCancel = nil
	b.processDone = nil
	b.closeJob = nil
	b.conn = nil
	b.outputFilter = nil
	b.processCtx = nil
	b.sessionID = ""
	b.busy = false
	b.sessionAutoApprove = false
	if stopping {
		b.mu.Unlock()
		if promptCancel != nil {
			promptCancel()
		}
		if closeJob != nil {
			closeJob()
		}
		return
	}
	b.state = "dead"
	if err == nil {
		b.lastError = "Grok Agent 已退出"
	} else {
		b.lastError = fmt.Sprintf("Grok Agent 已退出: %v", err)
	}
	errorText := b.lastError
	b.mu.Unlock()
	if promptCancel != nil {
		promptCancel()
	}
	if closeJob != nil {
		closeJob()
	}
	b.broadcast(Event{Type: "error", Error: errorText})
	b.broadcastStatus()
}

func (b *Bridge) setDead(err error) {
	b.mu.Lock()
	b.state = "dead"
	if err != nil {
		b.lastError = err.Error()
	}
	b.mu.Unlock()
	b.broadcastStatus()
}

func normalizeCwd(value, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("工作目录无效: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("工作目录不可用: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("工作目录不是文件夹: %s", absolute)
	}
	return filepath.Clean(absolute), nil
}
