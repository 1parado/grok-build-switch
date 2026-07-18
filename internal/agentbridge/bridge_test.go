package agentbridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

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
