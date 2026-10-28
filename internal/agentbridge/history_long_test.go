package agentbridge

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadChatHistoryKeepsNeighborsOfALongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat_history.jsonl")
	var b strings.Builder
	b.WriteString(`{"type":"user","content":"before"}` + "\n")
	b.WriteString(`{"type":"user","content":"`)
	b.WriteString(strings.Repeat("x", (4<<20)+32))
	b.WriteString("\"}\n")
	b.WriteString(`{"type":"user","content":"after"}` + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, err := readChatHistory(path)
	if err != nil {
		t.Fatalf("一条超长记录不应让整段历史读失败: %v", err)
	}
	var texts []string
	for _, msg := range msgs {
		if msg.Role == "user" {
			texts = append(texts, msg.Content)
		}
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "before") || !strings.Contains(joined, "after") {
		t.Fatalf("超长记录两侧的消息丢了: %#v", texts)
	}
}

func TestNextHistoryLineSkipsOverMax(t *testing.T) {
	raw := "short\n" + strings.Repeat("y", 20) + "\nlater\n"
	reader := bufio.NewReader(strings.NewReader(raw))
	var got []string
	for {
		line, err := nextHistoryLine(reader, 8)
		if len(line) > 0 {
			got = append(got, string(line))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, ",") != "short,later" {
		t.Fatalf("超限行应被跳过且后续行保留: %#v", got)
	}
}
