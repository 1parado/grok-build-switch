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
		`{"type":"assistant","content":[{"type":"text","text":"diagram ready"},{"type":"image","data":"aGVsbG8=","mimeType":"image/png"},{"type":"resource_link","name":"clip.mp4","uri":"https://cdn.example/clip.mp4","mimeType":"video/mp4"}],"model_id":"grok-4.5"}` + "\n" +
		`{"type":"tool_result","tool_call_id":"tool-image","content":"{\"path\":\"C:\\\\Users\\\\tester\\\\.grok\\\\sessions\\\\cwd\\\\session-1\\\\images\\\\2.jpg\",\"filename\":\"2.jpg\",\"session_folder\":\"images\"}"}` + "\n"
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
	if len(loaded.Messages) != 3 || loaded.Messages[0].Content != "draw a diagram" || loaded.Messages[1].Role != "assistant" {
		t.Fatalf("unexpected history: %#v", loaded.Messages)
	}
	if loaded.Messages[1].Content != "diagram ready" || len(loaded.Messages[1].Media) != 2 {
		t.Fatalf("structured history media was not restored: %#v", loaded.Messages[1])
	}
	if loaded.Messages[1].Media[0].Kind != "image" || loaded.Messages[1].Media[1].Kind != "video" {
		t.Fatalf("unexpected history media kinds: %#v", loaded.Messages[1].Media)
	}
	if len(loaded.Messages[2].Media) != 1 || loaded.Messages[2].Media[0].URI != `C:\Users\tester\.grok\sessions\cwd\session-1\images\2.jpg` {
		t.Fatalf("stored tool media was not restored once: %#v", loaded.Messages[2])
	}
}

func TestStoredSessionRejectsPathTraversal(t *testing.T) {
	bridge := New(t.TempDir(), filepath.Join(t.TempDir(), "agent.log"))
	if _, err := bridge.StoredSessionHistory("../summary.json"); err == nil {
		t.Fatal("expected invalid session id error")
	}
}

func TestListStoredSessionsMarksMissingCwd(t *testing.T) {
	grokHome := t.TempDir()
	sessionDir := filepath.Join(grokHome, "sessions", "encoded-cwd", "session-missing")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var summary storedSummary
	summary.Info.ID = "session-missing"
	summary.Info.Cwd = filepath.Join(t.TempDir(), "does-not-exist")
	summary.GeneratedTitle = "Orphan project"
	summary.UpdatedAt = time.Now()
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "summary.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	bridge := New(grokHome, filepath.Join(t.TempDir(), "agent.log"))
	sessions, err := bridge.ListStoredSessions("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !sessions[0].CwdMissing || sessions[0].ID != "session-missing" {
		t.Fatalf("expected cwd_missing session, got %#v", sessions)
	}
}

func TestDeleteStoredSession(t *testing.T) {
	grokHome := t.TempDir()
	projectDir := t.TempDir()
	sessionDir := filepath.Join(grokHome, "sessions", "encoded-cwd", "session-del")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var summary storedSummary
	summary.Info.ID = "session-del"
	summary.Info.Cwd = projectDir
	summary.GeneratedTitle = "To delete"
	summary.UpdatedAt = time.Now()
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "summary.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "chat_history.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bridge := New(grokHome, filepath.Join(t.TempDir(), "agent.log"))
	if err := bridge.DeleteStoredSession("session-del"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("session directory still present: %v", err)
	}
	sessions, err := bridge.ListStoredSessions("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected empty list after delete, got %#v", sessions)
	}
}

func TestStoredHistoryRestoresUserImageMedia(t *testing.T) {
	grokHome := t.TempDir()
	projectDir := t.TempDir()
	sessionDir := filepath.Join(grokHome, "sessions", "encoded-cwd", "session-img")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var summary storedSummary
	summary.Info.ID = "session-img"
	summary.Info.Cwd = projectDir
	summary.GeneratedTitle = "User image"
	summary.UpdatedAt = time.Now()
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	history := `{"type":"user","content":[{"type":"text","text":"<user_query>see this</user_query>"},{"type":"image","data":"aGVsbG8=","mimeType":"image/png"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "summary.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "chat_history.jsonl"), []byte(history), 0o644); err != nil {
		t.Fatal(err)
	}
	bridge := New(grokHome, filepath.Join(t.TempDir(), "agent.log"))
	loaded, err := bridge.StoredSessionHistory("session-img")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Role != "user" {
		t.Fatalf("unexpected history: %#v", loaded.Messages)
	}
	if loaded.Messages[0].Content != "see this" || len(loaded.Messages[0].Media) != 1 {
		t.Fatalf("user image media not restored: %#v", loaded.Messages[0])
	}
	if loaded.Messages[0].Media[0].Kind != "image" || loaded.Messages[0].Media[0].Data != "aGVsbG8=" {
		t.Fatalf("unexpected media: %#v", loaded.Messages[0].Media)
	}
}
