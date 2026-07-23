package agentbridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoredSessionsAndHistory(t *testing.T) {
	grokHome := t.TempDir()
	sessionDir := filepath.Join(grokHome, "sessions", "encoded-cwd", "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectDir := t.TempDir()
	var summary storedSummary
	summary.Info.ID = "session-1"
	summary.Info.Cwd = projectDir
	summary.GeneratedTitle = "Markdown session"
	summary.CreatedAt, _ = time.Parse(time.RFC3339, "2026-07-18T01:00:00Z")
	summary.UpdatedAt, _ = time.Parse(time.RFC3339, "2026-07-18T02:00:00Z")
	summary.CurrentModelID = "grok-4.5"
	summary.NumChatMessage = 2
	summaryData, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	history := "" +
		`{"type":"user","content":[{"type":"text","text":"<system-reminder>ignore</system-reminder>"}]}` + "\n" +
		`{"type":"user","content":[{"type":"text","text":"<user_query>draw a diagram</user_query>"}]}` + "\n" +
		"{\"type\":\"assistant\",\"content\":\"```mermaid\\\\ngraph TD; A-->B\\\\n```\",\"model_id\":\"grok-4.5\"}\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "summary.json"), summaryData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "chat_history.jsonl"), []byte(history), 0o644); err != nil {
		t.Fatal(err)
	}
	bridge := New(grokHome, filepath.Join(t.TempDir(), "agent.log"))
	sessions, err := bridge.ListStoredSessions("markdown", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Model != "grok-4.5" {
		t.Fatalf("unexpected sessions: %#v", sessions)
	}
	loaded, err := bridge.StoredSessionHistory("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[0].Content != "draw a diagram" || loaded.Messages[1].Role != "assistant" {
		t.Fatalf("unexpected history: %#v", loaded.Messages)
	}
}

func TestStoredSessionRejectsPathTraversal(t *testing.T) {
	bridge := New(t.TempDir(), filepath.Join(t.TempDir(), "agent.log"))
	if _, err := bridge.StoredSessionHistory("../summary.json"); err == nil {
		t.Fatal("expected invalid session id error")
	}
}
