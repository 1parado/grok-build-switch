package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"grok_switch/internal/agentbridge"
)

func TestWriteSessionLoadError(t *testing.T) {
	recorder := httptest.NewRecorder()
	status := agentbridge.Status{Running: true, State: "ready", SessionID: "fresh-session"}
	writeSessionLoadError(recorder, &agentbridge.SessionLoadError{
		Cause: errors.New("peer disconnected before response"),
	}, status)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	var response struct {
		Error           string             `json:"error"`
		Code            string             `json:"code"`
		ReadonlyHistory bool               `json:"readonly_history"`
		Recoverable     bool               `json:"recoverable"`
		EngineLoaded    bool               `json:"engine_loaded"`
		AgentRestarted  bool               `json:"agent_restarted"`
		Status          agentbridge.Status `json:"status"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != agentbridge.SessionLoadOverflowCode || !response.ReadonlyHistory || !response.Recoverable || response.EngineLoaded || !response.AgentRestarted {
		t.Fatalf("unexpected response: %#v", response)
	}
	if response.Status.SessionID != "fresh-session" || response.Status.State != "ready" {
		t.Fatalf("unexpected recovery status: %#v", response.Status)
	}
	if response.Error == "" {
		t.Fatal("expected a user-facing error message")
	}
}
