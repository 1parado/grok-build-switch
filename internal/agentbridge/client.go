package agentbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	acp "github.com/coder/acp-go-sdk"
)

func (b *Bridge) HandleExtensionMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	if method != "_x.ai/session/update" {
		return nil, acp.NewMethodNotFound(method)
	}
	var notification struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(params, &notification); err != nil {
		return nil, err
	}
	var kind struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(notification.Update, &kind); err != nil {
		return nil, err
	}
	if kind.SessionUpdate != "retry_state" {
		return nil, nil
	}
	var update struct {
		Type          string `json:"type"`
		Attempt       int    `json:"attempt,omitempty"`
		MaxRetries    int    `json:"max_retries,omitempty"`
		Attempts      int    `json:"attempts,omitempty"`
		Reason        string `json:"reason,omitempty"`
		ErrorType     string `json:"error_type,omitempty"`
		Message       string `json:"message,omitempty"`
		IsRateLimited bool   `json:"is_rate_limited,omitempty"`
	}
	if err := json.Unmarshal(notification.Update, &update); err != nil {
		return nil, err
	}
	retry := RetryEvent{
		State: update.Type, Attempt: update.Attempt, MaxRetries: update.MaxRetries,
		Attempts: update.Attempts, Reason: update.Reason, ErrorType: update.ErrorType,
		Message: update.Message, IsRateLimited: update.IsRateLimited,
	}
	b.broadcast(Event{Type: "retry_state", SessionID: notification.SessionID, Retry: &retry})
	return nil, nil
}

func (b *Bridge) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	if b.suppressUpdates.Load() {
		return nil
	}
	sessionID := string(params.SessionId)
	update := params.Update
	switch {
	case update.AgentMessageChunk != nil:
		if text := contentText(update.AgentMessageChunk.Content); text != "" {
			b.broadcast(Event{Type: "assistant_chunk", SessionID: sessionID, Text: text})
		}
	case update.AgentThoughtChunk != nil:
		if text := contentText(update.AgentThoughtChunk.Content); text != "" {
			b.broadcast(Event{Type: "thought_chunk", SessionID: sessionID, Text: text})
		}
	case update.ToolCall != nil:
		call := update.ToolCall
		b.broadcast(Event{Type: "tool_call", SessionID: sessionID, Tool: &ToolEvent{
			ID: string(call.ToolCallId), Title: call.Title, Kind: string(call.Kind),
			Status: string(call.Status), RawInput: call.RawInput, RawOutput: call.RawOutput,
		}})
	case update.ToolCallUpdate != nil:
		call := update.ToolCallUpdate
		tool := &ToolEvent{ID: string(call.ToolCallId), RawInput: call.RawInput, RawOutput: call.RawOutput}
		if call.Title != nil {
			tool.Title = *call.Title
		}
		if call.Kind != nil {
			tool.Kind = string(*call.Kind)
		}
		if call.Status != nil {
			tool.Status = string(*call.Status)
		}
		b.broadcast(Event{Type: "tool_update", SessionID: sessionID, Tool: tool})
	}
	return nil
}

func contentText(content acp.ContentBlock) string {
	if content.Text == nil {
		return ""
	}
	return content.Text.Text
}

func (b *Bridge) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	requestID := fmt.Sprintf("permission-%d", b.permCounter.Add(1))
	pending := &pendingPermission{options: params.Options, result: make(chan acp.PermissionOptionId, 1)}
	options := make([]PermissionOption, 0, len(params.Options))
	for _, option := range params.Options {
		options = append(options, PermissionOption{ID: string(option.OptionId), Name: option.Name, Kind: string(option.Kind)})
	}
	title := "工具执行请求"
	if params.ToolCall.Title != nil && *params.ToolCall.Title != "" {
		title = *params.ToolCall.Title
	}
	tool := ToolEvent{ID: string(params.ToolCall.ToolCallId), Title: title, RawInput: params.ToolCall.RawInput}
	if params.ToolCall.Kind != nil {
		tool.Kind = string(*params.ToolCall.Kind)
	}
	if params.ToolCall.Status != nil {
		tool.Status = string(*params.ToolCall.Status)
	}

	b.mu.Lock()
	b.permissions[requestID] = pending
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.permissions, requestID)
		b.mu.Unlock()
	}()
	b.broadcast(Event{Type: "permission_request", SessionID: string(params.SessionId), Permission: &PermissionEvent{
		RequestID: requestID, Summary: title, Tool: tool, Options: options,
	}})

	select {
	case optionID := <-pending.result:
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
			Selected: &acp.RequestPermissionOutcomeSelected{OptionId: optionID, Outcome: "selected"},
		}}, nil
	case <-ctx.Done():
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
			Cancelled: &acp.RequestPermissionOutcomeCancelled{Outcome: "cancelled"},
		}}, nil
	}
}

func (b *Bridge) RespondPermission(requestID string, allow bool) error {
	b.mu.RLock()
	pending := b.permissions[requestID]
	b.mu.RUnlock()
	if pending == nil {
		return ErrPermissionNotFound
	}
	var selected *acp.PermissionOption
	for i := range pending.options {
		kind := pending.options[i].Kind
		if allow && (kind == acp.PermissionOptionKindAllowOnce || kind == acp.PermissionOptionKindAllowAlways) {
			selected = &pending.options[i]
			if kind == acp.PermissionOptionKindAllowOnce {
				break
			}
		}
		if !allow && (kind == acp.PermissionOptionKindRejectOnce || kind == acp.PermissionOptionKindRejectAlways) {
			selected = &pending.options[i]
			if kind == acp.PermissionOptionKindRejectOnce {
				break
			}
		}
	}
	if selected == nil {
		return errors.New("Agent 未提供对应的权限选项")
	}
	select {
	case pending.result <- selected.OptionId:
		return nil
	default:
		return ErrPermissionNotFound
	}
}

func (b *Bridge) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.New("grok_switch 未启用客户端文件读取")
}

func (b *Bridge) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("grok_switch 未启用客户端文件写入")
}

func (b *Bridge) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("grok_switch 未启用客户端终端")
}

func (b *Bridge) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.New("grok_switch 未启用客户端终端")
}

func (b *Bridge) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.New("grok_switch 未启用客户端终端")
}

func (b *Bridge) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.New("grok_switch 未启用客户端终端")
}

func (b *Bridge) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.New("grok_switch 未启用客户端终端")
}
