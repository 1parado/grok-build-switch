package agentbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func TestIsSessionLoadOverflow(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "queue overflow", err: errors.New("notification queue overflow"), want: true},
		{name: "peer disconnected in wrapped rpc error", err: fmt.Errorf("恢复失败: %w", errors.New("peer disconnected before response")), want: true},
		{name: "connection closed", err: errors.New("peer connection closed"), want: true},
		{name: "typed load error", err: &SessionLoadError{Cause: errors.New("unexpected eof")}, want: true},
		{name: "ordinary load failure", err: errors.New("session not found"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsSessionLoadOverflow(test.err); got != test.want {
				t.Fatalf("IsSessionLoadOverflow(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestSessionLoadErrorMessageReflectsRecovery(t *testing.T) {
	recovered := (&SessionLoadError{Cause: errors.New("connection closed")}).Error()
	if !strings.Contains(recovered, "Agent 已自动重启") {
		t.Fatalf("recovered error did not explain recovery: %q", recovered)
	}
	failed := (&SessionLoadError{Cause: errors.New("connection closed"), RecoveryErr: errors.New("start failed")}).Error()
	if !strings.Contains(failed, "自动重启失败") || !strings.Contains(failed, "start failed") {
		t.Fatalf("failed recovery error did not include cause: %q", failed)
	}
}

func TestRetryExtensionBroadcastsState(t *testing.T) {
	bridge := New(t.TempDir(), filepath.Join(t.TempDir(), "agent.log"))
	id, events := bridge.Subscribe()
	defer bridge.Unsubscribe(id)
	params := json.RawMessage(`{
		"sessionId":"session-1",
		"update":{"sessionUpdate":"retry_state","type":"retrying","attempt":2,"max_retries":15,"reason":"upstream unavailable"}
	}`)
	if _, err := bridge.HandleExtensionMethod(context.Background(), "_x.ai/session/update", params); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Type != "retry_state" || event.Retry == nil || event.Retry.State != "retrying" || event.Retry.Attempt != 2 {
			t.Fatalf("unexpected event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for retry event")
	}
}

func TestSessionUpdateBroadcastsAssistantChunk(t *testing.T) {
	bridge := New(t.TempDir(), filepath.Join(t.TempDir(), "agent.log"))
	id, events := bridge.Subscribe()
	defer bridge.Unsubscribe(id)

	err := bridge.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1",
		Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			SessionUpdate: "agent_message_chunk",
			Content:       acp.TextBlock("hello"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Type != "assistant_chunk" || event.Text != "hello" || event.SessionID != "session-1" {
			t.Fatalf("unexpected event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for assistant event")
	}
}

func TestSessionUpdateBroadcastsStructuredAssistantMedia(t *testing.T) {
	bridge := New(t.TempDir(), filepath.Join(t.TempDir(), "agent.log"))
	id, events := bridge.Subscribe()
	defer bridge.Unsubscribe(id)

	image := acp.ImageBlock("aGVsbG8=", "image/png")
	uri := "https://cdn.example/generated.png"
	image.Image.Uri = &uri
	if err := bridge.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-media",
		Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			SessionUpdate: "agent_message_chunk",
			Content:       image,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-events:
		if event.Type != "assistant_media" || len(event.Media) != 1 {
			t.Fatalf("unexpected media event: %#v", event)
		}
		media := event.Media[0]
		if media.Kind != "image" || media.MimeType != "image/png" || media.Data != "aGVsbG8=" || media.URI != uri {
			t.Fatalf("unexpected media payload: %#v", media)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for assistant media event")
	}
}

func TestSessionUpdateIncludesToolResourceMedia(t *testing.T) {
	bridge := New(t.TempDir(), filepath.Join(t.TempDir(), "agent.log"))
	id, events := bridge.Subscribe()
	defer bridge.Unsubscribe(id)

	video := acp.ResourceLinkBlock("generated.mp4", "https://cdn.example/generated.mp4")
	mimeType := "video/mp4"
	video.ResourceLink.MimeType = &mimeType
	if err := bridge.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-media",
		Update: acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
			SessionUpdate: "tool_call_update",
			ToolCallId:    "tool-media",
			Content:       []acp.ToolCallContent{acp.ToolContent(video)},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-events:
		if event.Type != "tool_update" || event.Tool == nil || len(event.Tool.Media) != 1 {
			t.Fatalf("unexpected tool event: %#v", event)
		}
		if media := event.Tool.Media[0]; media.Kind != "video" || media.URI != "https://cdn.example/generated.mp4" {
			t.Fatalf("unexpected tool media: %#v", media)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for tool media event")
	}
}

func TestPermissionResponseSelectsAllowOnce(t *testing.T) {
	bridge := New(t.TempDir(), filepath.Join(t.TempDir(), "agent.log"))
	id, events := bridge.Subscribe()
	defer bridge.Unsubscribe(id)
	response := make(chan acp.RequestPermissionResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := bridge.RequestPermission(context.Background(), acp.RequestPermissionRequest{
			SessionId: "session-1",
			ToolCall:  acp.ToolCallUpdate{ToolCallId: "tool-1"},
			Options: []acp.PermissionOption{
				{OptionId: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
				{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
			},
		})
		if err != nil {
			errCh <- err
			return
		}
		response <- result
	}()

	var requestID string
	select {
	case event := <-events:
		if event.Type != "permission_request" || event.Permission == nil {
			t.Fatalf("unexpected event: %#v", event)
		}
		requestID = event.Permission.RequestID
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission event")
	}
	if err := bridge.RespondPermission(requestID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	case result := <-response:
		if result.Outcome.Selected == nil || result.Outcome.Selected.OptionId != "allow" {
			t.Fatalf("unexpected response: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission response")
	}
}

func TestSessionAutoApproveSkipsPrompt(t *testing.T) {
	bridge := New(t.TempDir(), filepath.Join(t.TempDir(), "agent.log"))
	bridge.SetSessionAutoApprove(true)
	if !bridge.Status().SessionAutoApprove {
		t.Fatal("expected session auto approve enabled")
	}
	result, err := bridge.RequestPermission(context.Background(), acp.RequestPermissionRequest{
		SessionId: "session-1",
		ToolCall:  acp.ToolCallUpdate{ToolCallId: "tool-1"},
		Options: []acp.PermissionOption{
			{OptionId: "allow-always", Name: "Always", Kind: acp.PermissionOptionKindAllowAlways},
			{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Selected == nil || result.Outcome.Selected.OptionId != "allow-always" {
		t.Fatalf("expected allow-always, got %#v", result)
	}
}

func TestCancelPromptWhenIdle(t *testing.T) {
	bridge := New(t.TempDir(), filepath.Join(t.TempDir(), "agent.log"))
	if err := bridge.CancelPrompt(); !errors.Is(err, ErrNotRunning) {
		// Idle bridge is not running; also accept the "no generation" path after a mock busy flag.
		if err == nil || !strings.Contains(err.Error(), "没有正在生成") && !errors.Is(err, ErrNotRunning) {
			t.Fatalf("unexpected cancel error: %v", err)
		}
	}
}

func TestPickPermissionOptionPrefersAlwaysWhenRequested(t *testing.T) {
	options := []acp.PermissionOption{
		{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: "always", Name: "Always", Kind: acp.PermissionOptionKindAllowAlways},
	}
	id, ok := pickPermissionOption(options, true, true)
	if !ok || id != "always" {
		t.Fatalf("prefer always: got %q ok=%v", id, ok)
	}
	id, ok = pickPermissionOption(options, true, false)
	if !ok || id != "allow" {
		t.Fatalf("prefer once: got %q ok=%v", id, ok)
	}
}

func TestRealGrokAgentInitializeAndNewSession(t *testing.T) {
	if os.Getenv("GROK_INTEGRATION") != "1" {
		t.Skip("set GROK_INTEGRATION=1 to test the installed Grok Build agent")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	grokHome := filepath.Join(home, ".grok")
	bridge := New(grokHome, filepath.Join(t.TempDir(), "agent.log"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := bridge.Start(ctx, StartOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	defer bridge.Stop()
	status := bridge.Status()
	if !status.Running || status.State != "ready" || status.SessionID == "" {
		t.Fatalf("unexpected status: %#v", status)
	}
	sessionID := status.SessionID
	cwd := status.Cwd
	if err := bridge.Stop(); err != nil {
		t.Fatal(err)
	}
	status = bridge.Status()
	if status.Running || status.State != "idle" || status.Error != "" {
		t.Fatalf("unexpected stopped status: %#v", status)
	}
	loaded := New(grokHome, filepath.Join(t.TempDir(), "loaded-agent.log"))
	if err := loaded.Start(ctx, StartOptions{Cwd: cwd, SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	defer loaded.Stop()
	loadedStatus := loaded.Status()
	if loadedStatus.SessionID != sessionID || loadedStatus.State != "ready" || loadedStatus.Model == "" {
		t.Fatalf("unexpected loaded status: %#v", loadedStatus)
	}
}
