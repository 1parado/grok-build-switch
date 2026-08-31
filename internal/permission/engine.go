package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Engine 是权限决策器：模式 + 两级规则表。并发安全。
type Engine struct {
	mu   sync.RWMutex
	mode Mode

	session []parsedRule
	user    []parsedRule

	path string // user 规则持久化文件；空 = 不持久化（测试）
}

// NewEngine 构造引擎。path 非空时加载既有 user 规则。
func NewEngine(path string) (*Engine, error) {
	e := &Engine{mode: ModeManual, path: path}
	if path != "" {
		if err := e.loadUser(); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// SetMode 切换姿态。
func (e *Engine) SetMode(m Mode) {
	if m != ModeManual && m != ModeYolo && m != ModeAuto {
		m = ModeManual
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode = m
}

// Mode 返回当前姿态。
func (e *Engine) Mode() Mode {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.mode
}

// Check 是决策入口：deny 规则 > 模式 > allow 规则 > ask。
// 返回值语义对齐 agentloop.Decision（allow/deny/ask 的字符串一致）。
func (e *Engine) Check(tool string, args json.RawMessage) string {
	tool = lower(tool)
	e.mu.RLock()
	mode := e.mode
	session := e.session
	user := e.user
	e.mu.RUnlock()

	rules := append(append([]parsedRule{}, user...), session...)

	// 1. deny 规则永远优先（任何模式）。
	for _, r := range rules {
		if r.decision == Deny && r.tool == tool && matchArg(r, tool, argOf(tool, args)) {
			return "deny"
		}
	}
	// 2. 模式短路。
	switch mode {
	case ModeAuto:
		return "allow"
	case ModeYolo:
		return "allow"
	}
	// 3. allow 规则。
	for _, r := range rules {
		if r.decision == Allow && r.tool == tool && matchArg(r, tool, argOf(tool, args)) {
			return "allow"
		}
	}
	return "ask"
}

// DenyReason 返回命中的 deny 规则原因（Check 返回 deny 后调用）。
func (e *Engine) DenyReason(tool string, args json.RawMessage) string {
	tool = lower(tool)
	e.mu.RLock()
	defer e.mu.RUnlock()
	rules := append(append([]parsedRule{}, e.user...), e.session...)
	for _, r := range rules {
		if r.decision == Deny && r.tool == tool && matchArg(r, tool, argOf(tool, args)) {
			if r.reason != "" {
				return r.reason
			}
			return "被权限规则 " + r.String() + " 拒绝。"
		}
	}
	return ""
}

// AddSessionRule 添加会话级规则（内存）。
func (e *Engine) AddSessionRule(pattern string, decision Decision) error {
	r, err := Parse(pattern, decision)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.session = append(e.session, r)
	return nil
}

// AddUserRule 添加并持久化 user 级规则。
func (e *Engine) AddUserRule(pattern string, decision Decision) error {
	r, err := Parse(pattern, decision)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// 同 pattern 同决定去重。
	for _, exist := range e.user {
		if exist.String() == r.String() && exist.decision == r.decision {
			return nil
		}
	}
	e.user = append(e.user, r)
	return e.persistLocked()
}

// RemoveUserRule 按 DSL 文本移除 user 规则。
func (e *Engine) RemoveUserRule(pattern string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	kept := e.user[:0]
	removed := false
	for _, r := range e.user {
		if r.String() == lower(strings.TrimSpace(pattern)) {
			removed = true
			continue
		}
		kept = append(kept, r)
	}
	if !removed {
		return nil
	}
	e.user = kept
	return e.persistLocked()
}

// Rules 返回两级规则的 DSL 文本（调试/未来 UI 编辑器）。
func (e *Engine) Rules() (session, user []string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, r := range e.session {
		session = append(session, r.String())
	}
	for _, r := range e.user {
		user = append(user, r.String())
	}
	return session, user
}

// ClearSession 清空会话级规则（新会话/Stop 时调用）。
func (e *Engine) ClearSession() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.session = nil
}

// --- 持久化 ---

type persistFile struct {
	Version int      `json:"version"`
	Rules   []string `json:"rules"` // "allow:Tool(pattern)" / "deny:Tool(pattern)"
}

func (e *Engine) loadUser() error {
	data, err := os.ReadFile(e.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var pf persistFile
	if json.Unmarshal(data, &pf) != nil {
		// 损坏：重命名为备份并从默认开始（与项目 recovery 语义一致）。
		_ = os.Rename(e.path, e.path+".corrupt.bak")
		return nil
	}
	for _, line := range pf.Rules {
		decision := Allow
		text := line
		if idx := strings.Index(line, ":"); idx > 0 {
			switch lower(line[:idx]) {
			case "allow":
				decision = Allow
			case "deny":
				decision = Deny
			default:
				continue
			}
			text = line[idx+1:]
		}
		r, err := Parse(text, decision)
		if err != nil {
			continue
		}
		e.user = append(e.user, r)
	}
	return nil
}

func (e *Engine) persistLocked() error {
	if e.path == "" {
		return nil
	}
	pf := persistFile{Version: 1}
	for _, r := range e.user {
		pf.Rules = append(pf.Rules, string(r.decision)+":"+r.String())
	}
	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(e.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(e.path), ".perm-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, e.path)
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}
