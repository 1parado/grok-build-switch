package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"grok_switch/internal/agentfs"
)

// --- bash ---

type BashTool struct{}

func (BashTool) Name() string { return "bash" }

func (BashTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "要执行的 shell 命令"},
			"timeout": map[string]any{"type": "integer", "description": "超时秒数（默认 120，上限 600）"},
			"dir":     map[string]any{"type": "string", "description": "执行目录（默认会话工作目录）"},
		},
		"required": []string{"command"},
	}
}

func (BashTool) Doc() string {
	return `在 shell 中执行命令（git、构建、测试、包管理等）。每次调用独立 shell：
变量与 cd 不跨调用保留，请在 command 内组合（&& / ; / 管道）或用 dir 参数。
cat/sed/grep 等文件操作优先用专用工具。输出保尾截断：超长时保留尾部并落盘，
提示 Full output 路径，模型可用 read 分段查看。`
}

type bashArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
	Dir     string `json:"dir"`
}

const (
	bashDefaultTimeout = 120 * time.Second
	bashMaxTimeout     = 600 * time.Second
)

func (BashTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	var a bashArgs
	if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Command) == "" {
		return argHelp("bash", err, `{"command": "npm test", "timeout"?: 120, "dir"?: "..."}`)
	}
	timeout := bashDefaultTimeout
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
		if timeout > bashMaxTimeout {
			timeout = bashMaxTimeout
		}
	}
	dir := env.Cwd
	if a.Dir != "" {
		abs, err := env.Guard(a.Dir)
		if err != nil {
			return ToolOutput{Text: err.Error(), IsError: true}
		}
		dir = abs
	}
	res := env.Shell(ctx, a.Command, dir, timeout)
	var b strings.Builder
	if out := res.Combined(); out != "" {
		b.WriteString(out)
		b.WriteString("\n")
	}
	if res.TimedOut {
		fmt.Fprintf(&b, "[命令超时（%s），已终止]\n", timeout)
		return ToolOutput{Text: truncateBashTailWithEnv(b.String(), env), IsError: true}
	}
	if res.ExitCode != 0 {
		fmt.Fprintf(&b, "[退出码: %d]\n", res.ExitCode)
		return ToolOutput{Text: truncateBashTailWithEnv(b.String(), env), IsError: true}
	}
	if b.Len() == 0 {
		return ToolOutput{Text: "(命令成功，无输出)"}
	}
	text := strings.TrimRight(b.String(), "\n")
	if truncated, out := spillBashOutputWithEnv(text, env); truncated {
		return ToolOutput{Text: out, Truncated: true}
	}
	return ToolOutput{Text: text}
}

// bashOutputBudget 保尾截断预算（对齐 pi truncateTail + 50KB）。
const bashOutputBudget = 30000

// truncateBashTailWithEnv 错误路径同样保尾截断，避免超长错误刷屏。
func truncateBashTailWithEnv(s string, env agentfs.Env) string {
	if len(s) <= bashOutputBudget {
		return s
	}
	if spilled, out := spillBashOutputWithEnv(s, env); spilled {
		_ = spilled
		return out
	}
	return s[len(s)-bashOutputBudget:] + "\n\n[输出超长已截断，保留尾部]"
}

// truncateBashTail 兼容旧调用（无 env 时落 /tmp，模型不可读，仅保尾）。
func truncateBashTail(s string) string {
	return truncateBashTailWithEnv(s, agentfs.Env{})
}

// spillBashOutputWithEnv 超预算时落盘全量到沙箱内 .agent_tmp 并返回尾部 + 相对路径（供 read 分段查看）。
func spillBashOutputWithEnv(s string, env agentfs.Env) (bool, string) {
	if len(s) <= bashOutputBudget {
		return false, s
	}
	tail := s[len(s)-25000:]
	// 按行边界对齐，避免半行。
	if idx := strings.Index(tail, "\n"); idx >= 0 && idx < 500 {
		tail = tail[idx+1:]
	}
	// 沙箱内落盘：cwd/.agent_tmp/pi-bash-*.log，模型可用 read 查看。
	if env.Cwd != "" {
		tmpDir := filepath.Join(env.Cwd, ".agent_tmp")
		if err := os.MkdirAll(tmpDir, 0o755); err == nil {
			if f, err := os.CreateTemp(tmpDir, "pi-bash-*.log"); err == nil {
				_, _ = f.WriteString(s)
				_ = f.Close()
				if rel, err := filepath.Rel(env.Cwd, f.Name()); err == nil {
					return true, tail + fmt.Sprintf("\n\n[输出超长已截断，保留尾部。完整输出: %s，可用 read 分段查看]", rel)
				}
				return true, tail + fmt.Sprintf("\n\n[输出超长已截断，保留尾部。完整输出: %s]", f.Name())
			}
		}
	}
	return true, tail + "\n\n[输出超长已截断，保留尾部；可用更精确的命令（如 grep/head/tail/read）缩小范围]"
}

// spillBashOutput 兼容旧调用。
func spillBashOutput(s string) (bool, string) {
	return spillBashOutputWithEnv(s, agentfs.Env{})
}

// --- todo_list ---

// TodoStore 是会话级 todo 状态（server 层持有并渲染到 UI）。
type TodoStore struct {
	mu    sync.Mutex
	items []TodoItem
}

// TodoItem 是单条任务。
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

type TodoListTool struct{ Store *TodoStore }

func (TodoListTool) Name() string { return "todo_list" }

func (TodoListTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string"},
						"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
					},
					"required": []string{"content", "status"},
				},
				"description": "完整替换当前任务列表",
			},
		},
		"required": []string{"items"},
	}
}

func (TodoListTool) Doc() string {
	return `维护结构化任务列表（多步骤任务时使用）。每次调用完整替换列表；
同一时刻最多一条 in_progress。完成一步后立即更新状态，让用户看到进度。`
}

type todoArgs struct {
	Items []TodoItem `json:"items"`
}

func (t TodoListTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	var a todoArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return argHelp("todo_list", err, `{"items": [{"content": "...", "status": "pending"}]}`)
	}
	if t.Store == nil {
		t.Store = &TodoStore{}
	}
	valid := make([]TodoItem, 0, len(a.Items))
	for _, it := range a.Items {
		status := it.Status
		if status != "pending" && status != "in_progress" && status != "completed" {
			status = "pending"
		}
		if strings.TrimSpace(it.Content) == "" {
			continue
		}
		valid = append(valid, TodoItem{Content: strings.TrimSpace(it.Content), Status: status})
	}
	t.Store.mu.Lock()
	t.Store.items = valid
	t.Store.mu.Unlock()
	return ToolOutput{Text: fmt.Sprintf("任务列表已更新（%d 项）", len(valid))}
}

// Snapshot 返回当前列表副本。
func (s *TodoStore) Snapshot() []TodoItem {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TodoItem, len(s.items))
	copy(out, s.items)
	return out
}
