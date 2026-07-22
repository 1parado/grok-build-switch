package agentbridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

type SessionSummary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Cwd          string    `json:"cwd"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Model        string    `json:"model,omitempty"`
	AgentName    string    `json:"agent_name,omitempty"`
	MessageCount int       `json:"message_count"`
}

type HistoryMessage struct {
	Role    string         `json:"role"`
	Content string         `json:"content,omitempty"`
	Model   string         `json:"model,omitempty"`
	Tool    *ToolEvent     `json:"tool,omitempty"`
	Media   []MediaContent `json:"media,omitempty"`
}

type SessionHistory struct {
	Session  SessionSummary   `json:"session"`
	Messages []HistoryMessage `json:"messages"`
}

type storedSummary struct {
	Info struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"info"`
	SessionSummary string    `json:"session_summary"`
	GeneratedTitle string    `json:"generated_title"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastActiveAt   time.Time `json:"last_active_at"`
	CurrentModelID string    `json:"current_model_id"`
	AgentName      string    `json:"agent_name"`
	NumChatMessage int       `json:"num_chat_messages"`

	// CustomTitle is loaded from a grok_switch sidecar file and overrides the
	// Grok-generated title when set. It is not part of summary.json.
	CustomTitle string `json:"-"`
}

func (b *Bridge) ListStoredSessions(query string, limit int) ([]SessionSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 80
	}
	query = strings.ToLower(strings.TrimSpace(query))
	root := filepath.Join(b.grokHome, "sessions")
	cwdEntries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []SessionSummary{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 Grok 会话目录失败: %w", err)
	}
	sessions := make([]SessionSummary, 0, min(limit*2, 200))
	for _, cwdEntry := range cwdEntries {
		if !cwdEntry.IsDir() {
			continue
		}
		cwdPath := filepath.Join(root, cwdEntry.Name())
		sessionEntries, readErr := os.ReadDir(cwdPath)
		if readErr != nil {
			continue
		}
		for _, sessionEntry := range sessionEntries {
			if !sessionEntry.IsDir() {
				continue
			}
			dir := filepath.Join(cwdPath, sessionEntry.Name())
			summary, readErr := readStoredSummary(filepath.Join(dir, "summary.json"))
			if readErr != nil || summary.Info.ID == "" || summary.Info.Cwd == "" {
				continue
			}
			if strings.TrimSpace(summary.GeneratedTitle) == "" && strings.TrimSpace(summary.SessionSummary) == "" {
				continue
			}
			if info, statErr := os.Stat(summary.Info.Cwd); statErr != nil || !info.IsDir() {
				continue
			}
			item := summary.toSessionSummary()
			if query != "" && !strings.Contains(strings.ToLower(item.Title+" "+item.Cwd+" "+item.Model), query) {
				continue
			}
			sessions = append(sessions, item)
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt) })
	if len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return sessions, nil
}

func (b *Bridge) StoredSessionHistory(id string) (SessionHistory, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, `/\\`) {
		return SessionHistory{}, errors.New("会话 ID 无效")
	}
	dir, summary, err := b.findStoredSession(id)
	if err != nil {
		return SessionHistory{}, err
	}
	messages, err := readChatHistory(filepath.Join(dir, "chat_history.jsonl"))
	if err != nil {
		return SessionHistory{}, err
	}
	return SessionHistory{Session: summary.toSessionSummary(), Messages: messages}, nil
}

func (b *Bridge) findStoredSession(id string) (string, storedSummary, error) {
	root := filepath.Join(b.grokHome, "sessions")
	cwdEntries, err := os.ReadDir(root)
	if err != nil {
		return "", storedSummary{}, err
	}
	for _, cwdEntry := range cwdEntries {
		if !cwdEntry.IsDir() {
			continue
		}
		dir := filepath.Join(root, cwdEntry.Name(), id)
		summary, readErr := readStoredSummary(filepath.Join(dir, "summary.json"))
		if readErr == nil && summary.Info.ID == id {
			return dir, summary, nil
		}
	}
	return "", storedSummary{}, os.ErrNotExist
}

func readStoredSummary(path string) (storedSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return storedSummary{}, err
	}
	var summary storedSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return storedSummary{}, err
	}
	// Prefer a user-defined title stored in a sidecar file next to summary.json,
	// so renaming survives Grok regenerating its own summary metadata.
	if side, sideErr := os.ReadFile(filepath.Join(filepath.Dir(path), "grok_switch_title.json")); sideErr == nil {
		var override struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(side, &override) == nil {
			summary.CustomTitle = strings.TrimSpace(override.Title)
		}
	}
	return summary, nil
}

// RenameStoredSession persists a user-chosen title for a stored session in a
// sidecar file. An empty title clears the override and restores the original.
func (b *Bridge) RenameStoredSession(id, title string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, `/\\`) {
		return errors.New("会话 ID 无效")
	}
	title = strings.TrimSpace(title)
	dir, _, err := b.findStoredSession(id)
	if err != nil {
		return err
	}
	sidecar := filepath.Join(dir, "grok_switch_title.json")
	if title == "" {
		_ = os.Remove(sidecar)
		return nil
	}
	data, err := json.MarshalIndent(map[string]string{"title": title}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(sidecar, data, 0o600); err != nil {
		return fmt.Errorf("写入会话标题失败: %w", err)
	}
	return nil
}

func (s storedSummary) toSessionSummary() SessionSummary {
	title := strings.TrimSpace(s.CustomTitle)
	if title == "" {
		title = strings.TrimSpace(s.GeneratedTitle)
	}
	if title == "" {
		title = strings.TrimSpace(s.SessionSummary)
	}
	if title == "" {
		title = "未命名会话"
	}
	updated := s.UpdatedAt
	if s.LastActiveAt.After(updated) {
		updated = s.LastActiveAt
	}
	return SessionSummary{
		ID: s.Info.ID, Title: title, Cwd: s.Info.Cwd, CreatedAt: s.CreatedAt,
		UpdatedAt: updated, Model: s.CurrentModelID, AgentName: s.AgentName,
		MessageCount: s.NumChatMessage,
	}
}

func readChatHistory(path string) ([]HistoryMessage, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []HistoryMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	messages := make([]HistoryMessage, 0, 32)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		var entry struct {
			Type       string          `json:"type"`
			Content    json.RawMessage `json:"content"`
			ModelID    string          `json:"model_id"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"tool_calls"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		switch entry.Type {
		case "user":
			text := cleanStoredUserText(contentTextFromJSON(entry.Content))
			media := contentMediaFromJSON(entry.Content)
			if text != "" || len(media) > 0 {
				messages = append(messages, HistoryMessage{Role: "user", Content: text, Media: media})
			}
		case "assistant":
			text := strings.TrimSpace(contentTextFromJSON(entry.Content))
			media := contentMediaFromJSON(entry.Content)
			if text != "" || len(media) > 0 {
				messages = append(messages, HistoryMessage{Role: "assistant", Content: text, Model: entry.ModelID, Media: media})
			}
			for _, call := range entry.ToolCalls {
				var input any
				if json.Unmarshal([]byte(call.Arguments), &input) != nil {
					input = call.Arguments
				}
				messages = append(messages, HistoryMessage{Role: "tool", Tool: &ToolEvent{ID: call.ID, Title: call.Name, Status: "completed", RawInput: input}})
			}
		case "tool_result":
			media := contentMediaFromJSON(entry.Content)
			if len(media) == 0 {
				media = storedToolResultMedia(entry.Content)
			}
			messages = append(messages, HistoryMessage{
				Role: "tool_result", Content: contentTextFromJSON(entry.Content), Media: media,
				Tool: &ToolEvent{ID: entry.ToolCallID, Status: "completed"},
			})
		case "reasoning":
			parts := make([]string, 0, len(entry.Summary))
			for _, part := range entry.Summary {
				if part.Text != "" {
					parts = append(parts, part.Text)
				}
			}
			if len(parts) > 0 {
				messages = append(messages, HistoryMessage{Role: "thought", Content: strings.Join(parts, "\n")})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

func contentTextFromJSON(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	blocks := contentBlocksFromJSON(raw)
	if len(blocks) > 0 {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Text != nil && block.Text.Text != "" {
				parts = append(parts, block.Text.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func contentBlocksFromJSON(raw json.RawMessage) []acp.ContentBlock {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var blocks []acp.ContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	var block acp.ContentBlock
	if json.Unmarshal(raw, &block) == nil && (block.Text != nil || block.Image != nil || block.Audio != nil || block.ResourceLink != nil || block.Resource != nil) {
		return []acp.ContentBlock{block}
	}
	return nil
}

func contentMediaFromJSON(raw json.RawMessage) []MediaContent {
	blocks := contentBlocksFromJSON(raw)
	media := make([]MediaContent, 0, len(blocks))
	for _, block := range blocks {
		media = append(media, contentMedia(block)...)
	}
	return media
}

func storedToolResultMedia(raw json.RawMessage) []MediaContent {
	text := strings.TrimSpace(contentTextFromJSON(raw))
	if text == "" {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal([]byte(text), &payload) != nil {
		return nil
	}
	name, _ := payload["filename"].(string)
	for _, candidate := range []struct {
		key  string
		kind string
	}{
		{key: "path"}, {key: "image_path", kind: "image"}, {key: "video_path", kind: "video"},
		{key: "output_path"}, {key: "image_url", kind: "image"}, {key: "video_url", kind: "video"},
		{key: "url"}, {key: "uri"}, {key: "filename"},
	} {
		value, _ := payload[candidate.key].(string)
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		kind := candidate.kind
		if kind == "" {
			kind = mediaKind("", value)
		}
		if kind == "resource" {
			continue
		}
		return []MediaContent{{Kind: kind, URI: value, Name: strings.TrimSpace(name)}}
	}
	return nil
}

func cleanStoredUserText(text string) string {
	text = strings.TrimSpace(text)
	if start := strings.Index(text, "<user_query>"); start >= 0 {
		start += len("<user_query>")
		if end := strings.Index(text[start:], "</user_query>"); end >= 0 {
			return strings.TrimSpace(text[start : start+end])
		}
	}
	for _, marker := range []string{"<user_info>", "<git_status>", "<system-reminder>"} {
		if strings.Contains(text, marker) {
			return ""
		}
	}
	return text
}
