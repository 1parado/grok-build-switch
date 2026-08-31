// Package agentkit 是原生引擎的会话宿主层：上下文记忆（PromptOrigin 标记）、
// 会话持久化（meta.json + transcript.jsonl）与生命周期。
// server 层的 nativeAgentService 组合本包与 agentloop 实现完整服务。
package agentkit

import (
	"sync"
	"time"

	"grok_switch/internal/llm"
)

// Origin 标记消息来源（设计文档 D1b/§6.4：compaction 与回放的根基）。
// 压实时只保留 OriginUser 原话；injection/system 类可重建，直接丢弃。
type Origin string

const (
	OriginUser      Origin = "user"      // 真实用户输入
	OriginAssistant Origin = "assistant" // 模型回复
	OriginTool      Origin = "tool"      // 工具结果
	OriginInjection Origin = "injection" // 注入的系统内容（bootstrap、reminder）
	OriginSummary   Origin = "summary"   // compaction 摘要
)

// Record 是持久化 transcript 的一条记录。
type Record struct {
	Seq        int64          `json:"seq"`
	Time       time.Time      `json:"time"`
	Origin     Origin         `json:"origin"`
	Role       llm.Role       `json:"role"`
	Text       string         `json:"text,omitempty"`
	Thinking   string         `json:"thinking,omitempty"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	// Media 引用（路径或 URI，不落 base64 大对象）。
	Media []MediaRef `json:"media,omitempty"`
	// TurnID 归属的引擎 turn。
	TurnID string `json:"turn_id,omitempty"`
	// UserTurnCount 是截至该记录的用户轮次（rewind 定位用）。
	UserTurn int `json:"user_turn,omitempty"`
}

// MediaRef 是媒体附件的持久化引用。
type MediaRef struct {
	Kind     string `json:"kind"` // image | video | audio | resource
	MimeType string `json:"mime_type,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}

// ToMessage 把记录转回统一消息（回放进 ctxmem）。
func (r Record) ToMessage() llm.Message {
	msg := llm.Message{Role: r.Role, ToolCallID: r.ToolCallID}
	if r.Text != "" {
		msg.Parts = append(msg.Parts, llm.TextPart{Text: r.Text})
	}
	for _, m := range r.Media {
		if m.URI != "" {
			msg.Parts = append(msg.Parts, llm.ImagePart{URI: m.URI, MimeType: m.MimeType})
		}
	}
	msg.ToolCalls = append(msg.ToolCalls, r.ToolCalls...)
	return msg
}

// CtxMemory 实现 agentloop.Memory：引擎自持历史（D1b 无状态重放）。
// 记录带 Origin；P6 的 compaction 在 History() 出口投影，当前为直通。
type CtxMemory struct {
	mu      sync.Mutex
	msgs    []llm.Message
	origins []Origin
	// userTurns 计数用户轮次（每次 Append OriginUser 消息时 +1）。
	userTurns int
}

func NewCtxMemory() *CtxMemory { return &CtxMemory{} }

// History 实现 agentloop.Memory。
func (m *CtxMemory) History() []llm.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]llm.Message, len(m.msgs))
	copy(out, m.msgs)
	return out
}

// Append 实现 agentloop.Memory：来源按角色推导（引擎写入的只有
// assistant 回复与 tool 结果）。
func (m *CtxMemory) Append(msg llm.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	origin := OriginAssistant
	if msg.Role == llm.RoleTool {
		origin = OriginTool
	} else if msg.Role == llm.RoleUser {
		origin = OriginUser
	}
	m.origins = append(m.origins, origin)
	if msg.Role == llm.RoleUser {
		m.userTurns++
	}
}

// AppendWithOrigin 追加并记录来源（宿主层写入用户消息/注入时使用）。
func (m *CtxMemory) AppendWithOrigin(msg llm.Message, origin Origin) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	m.origins = append(m.origins, origin)
	if origin == OriginUser {
		m.userTurns++
	}
	return len(m.msgs) - 1
}

// UserTurns 返回当前用户轮次。
func (m *CtxMemory) UserTurns() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.userTurns
}

// RewindToUserTurn 删除第 n 轮用户消息及其后的全部内容。
// 返回是否发生截断。n 为 1 起；n <= 0 无操作。
func (m *CtxMemory) RewindToUserTurn(n int) bool {
	if n <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := 0
	cut := -1
	for i, msg := range m.msgs {
		if msg.Role == llm.RoleUser {
			seen++
			if seen == n {
				cut = i
				break
			}
		}
	}
	if cut < 0 {
		// 定位不到该轮次（越界）：视为回退到末尾，无操作。
		return true
	}
	// 截断 origins 与 msgs 保持平行；rewind 后按剩余来源重算用户轮次。
	if len(m.origins) >= cut {
		m.origins = m.origins[:cut]
	}
	m.msgs = m.msgs[:cut]
	turns := 0
	for _, o := range m.origins {
		if o == OriginUser {
			turns++
		}
	}
	m.userTurns = turns
	return true
}

// Origins 返回与 History 平行的来源标记（持久化用）。
func (m *CtxMemory) Origins() []Origin {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Origin, len(m.origins))
	copy(out, m.origins)
	return out
}

// Snapshot 返回 (消息, 来源) 平行数组副本。
func (m *CtxMemory) Snapshot() ([]llm.Message, []Origin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs := make([]llm.Message, len(m.msgs))
	copy(msgs, m.msgs)
	origins := make([]Origin, len(m.origins))
	copy(origins, m.origins)
	return msgs, origins
}

// Restore 用持久化记录重建内存（不触发 userTurns 计数副作用时手动同步）。
func (m *CtxMemory) Restore(records []Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = m.msgs[:0]
	m.origins = m.origins[:0]
	m.userTurns = 0
	for _, r := range records {
		m.msgs = append(m.msgs, r.ToMessage())
		m.origins = append(m.origins, r.Origin)
		if r.Origin == OriginUser {
			m.userTurns++
		}
	}
}
