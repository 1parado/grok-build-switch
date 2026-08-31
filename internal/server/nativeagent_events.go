package server

// nativeEventTranslator 把 agentloop.Event 翻译成 agentbridge.Event（WS 协议），
// 同时把 assistant/tool 消息持久化到 transcript。
// 翻译表（UI 合同，见 ui/app.js handleAgentEvent）：
//
//	text_delta       → assistant_chunk
//	think_delta      → thought_chunk
//	tool_call        → tool_call（回显参数）
//	tool_result      → tool_update（含结果）
//	usage            → （仅录制，不推 UI）
//	step_* / turn_*  → （UI 由 turn_done 收口）
import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"grok_switch/internal/agentbridge"
	"grok_switch/internal/agentkit"
	"grok_switch/internal/agentloop"
	"grok_switch/internal/llm"
	"grok_switch/internal/tools"
)

type nativeEventTranslator struct {
	svc       *nativeAgentService
	sessionID string
	turnID    string
	registry  *tools.Registry

	// 累积当前步的 assistant 文本/思考，turn 结束时一次性落盘。
	mu           sync.Mutex
	textBuf      strings.Builder
	thinkBuf     strings.Builder
	pendingCalls []llm.ToolCall
	usage        *turnUsageHolder
}

func (t *nativeEventTranslator) Dispatch(ev agentloop.Event) {
	switch ev.Type {
	case agentloop.EventTextDelta:
		t.mu.Lock()
		t.textBuf.WriteString(ev.Text)
		t.mu.Unlock()
		t.svc.broadcast(agentbridge.Event{Type: "assistant_chunk", SessionID: t.sessionID, Text: ev.Text})
	case agentloop.EventThinkDelta:
		t.mu.Lock()
		t.thinkBuf.WriteString(ev.Think)
		t.mu.Unlock()
		t.svc.broadcast(agentbridge.Event{Type: "thought_chunk", SessionID: t.sessionID, Text: ev.Think})
	case agentloop.EventToolCall:
		if ev.ToolCall != nil {
			t.mu.Lock()
			t.pendingCalls = append(t.pendingCalls, llm.ToolCall{ID: ev.ToolCall.ID, Name: ev.ToolCall.Name, Arguments: ev.ToolCall.Arguments})
			t.mu.Unlock()
		}
		// UI 的 tool_call 渲染在 ACP 侧带 Status；原生引擎派发时即"运行中"。
		if ev.ToolCall != nil {
			t.svc.broadcast(agentbridge.Event{
				Type: "tool_call", SessionID: t.sessionID,
				Tool: &agentbridge.ToolEvent{
					ID: ev.ToolCall.ID, Title: ev.ToolCall.Name, Kind: ev.ToolCall.Name,
					Status: "in_progress", RawInput: json.RawMessage(ev.ToolCall.Arguments),
				},
			})
		}
	case agentloop.EventToolResult:
		if ev.ToolResult != nil {
			t.svc.broadcast(agentbridge.Event{
				Type: "tool_update", SessionID: t.sessionID,
				Tool: &agentbridge.ToolEvent{
					ID: ev.ToolResult.ID, Title: ev.ToolResult.Name, Kind: ev.ToolResult.Name,
					Status:    toolEventStatus(ev.ToolResult.IsError),
					RawOutput: ev.ToolResult.Output,
				},
			})
			// 工具结果落盘（含回显的 call 参数）。
			t.mu.Lock()
			var call *llm.ToolCall
			for i := range t.pendingCalls {
				if t.pendingCalls[i].ID == ev.ToolResult.ID {
					call = &t.pendingCalls[i]
					t.pendingCalls = append(t.pendingCalls[:i], t.pendingCalls[i+1:]...)
					break
				}
			}
			t.mu.Unlock()
			if call != nil {
				t.svc.persist(t.sessionID, agentkit.Record{
					Origin: agentkit.OriginAssistant, Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{{ID: call.ID, Name: call.Name, Arguments: call.Arguments}},
					TurnID:    t.turnID,
				})
			}
			t.svc.persist(t.sessionID, agentkit.Record{
				Origin: agentkit.OriginTool, Role: llm.RoleTool,
				ToolCallID: ev.ToolResult.ID,
				Text:       agentloop.ToolResult{Output: ev.ToolResult.Output, IsError: ev.ToolResult.IsError, Truncated: ev.ToolResult.Truncated}.ResultJSON(),
				TurnID:     t.turnID,
			})
		}
	case agentloop.EventUsage:
		if ev.Usage != nil && t.usage != nil {
			t.usage.record(*ev.Usage)
		}
	case agentloop.EventTurnEnd, agentloop.EventTurnInterrupt:
		// flush 累积文本（一段 assistant 回复一次落盘）。
		t.mu.Lock()
		text := t.textBuf.String()
		think := t.thinkBuf.String()
		t.textBuf.Reset()
		t.thinkBuf.Reset()
		t.mu.Unlock()
		if text != "" || think != "" {
			t.svc.persist(t.sessionID, agentkit.Record{
				Origin: agentkit.OriginAssistant, Role: llm.RoleAssistant,
				Text: text, Thinking: think, TurnID: t.turnID,
			})
		}
	}
}

func toolEventStatus(isErr bool) string {
	if isErr {
		return "failed"
	}
	return "completed"
}

// turnUsageHolder 记录本 turn 最近一步的真实 input tokens（D3：无状态重放下
// 该值 ≈ 当前全量历史体积，是 compaction 的触发依据）。
type turnUsageHolder struct {
	mu     sync.Mutex
	tokens int64
}

func (h *turnUsageHolder) record(u llm.TokenUsage) {
	h.mu.Lock()
	h.tokens = u.InputTotal()
	h.mu.Unlock()
}

func (h *turnUsageHolder) lastInputTokens() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tokens
}

// compactViaLLM 用当前 Provider 生成一次压实摘要（非流式单调用）。
func compactViaLLM(ctx context.Context, provider llm.Provider, dropped []llm.Message) (string, error) {
	const maxInputChars = 60000
	instruction := "请把以下是一段被折叠的 agent 对话历史压缩成一份交接摘要，供后续对话接续使用。" +
		"摘要必须保留：用户的原始任务与目标、已完成的步骤与结果、重要文件路径与关键决定、" +
		"未完成的事项与下一步。不要新增对话中不存在的内容。直接输出摘要正文。"
	input := agentkit.CompactInputText(dropped, maxInputChars)
	res, err := provider.Generate(ctx, "", nil, []llm.Message{{
		Role:  llm.RoleUser,
		Parts: []llm.ContentPart{llm.TextPart{Text: instruction + "\n\n<folded_history>\n" + input + "\n</folded_history>"}},
	}}, llm.GenerateOptions{})
	if err != nil {
		return "", err
	}
	return res.Message.Text(), nil
}
