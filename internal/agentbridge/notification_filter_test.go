package agentbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func TestSessionLoadNotificationFilterDropsReplayFlood(t *testing.T) {
	var input strings.Builder
	const standardUpdates = 6000
	for i := 0; i < standardUpdates; i++ {
		fmt.Fprintf(&input, `{"jsonrpc":"2.0","method":"session/update","params":{"seq":%d}}`+"\n", i)
	}
	input.WriteString(`{"jsonrpc":"2.0","method":"_x.ai/session/update","params":{"seq":6000}}` + "\n")
	input.WriteString(`{"jsonrpc":"2.0","method":"custom/event","params":{}}` + "\n")
	input.WriteString(`{"jsonrpc":"2.0","id":7,"method":"session/update","params":{}}` + "\n")
	input.WriteString(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")

	var suppress atomic.Bool
	suppress.Store(true)
	filter := newSessionLoadNotificationFilter(strings.NewReader(input.String()), &suppress)
	output, err := io.ReadAll(filter)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if strings.Contains(text, `"seq"`) {
		t.Fatalf("replay notification leaked through filter: %s", text)
	}
	for _, retained := range []string{`"method":"custom/event"`, `"id":7`, `"id":1`} {
		if !strings.Contains(text, retained) {
			t.Fatalf("expected retained message %s in %s", retained, text)
		}
	}
	if got, want := filter.Dropped(), uint64(standardUpdates+1); got != want {
		t.Fatalf("dropped = %d, want %d", got, want)
	}
}

func TestSessionLoadNotificationFilterIsTransparentOutsideLoad(t *testing.T) {
	input := `{"jsonrpc":"2.0","method":"session/update","params":{"value":"kept"}}` + "\n"
	var suppress atomic.Bool
	filter := newSessionLoadNotificationFilter(strings.NewReader(input), &suppress)
	output, err := io.ReadAll(filter)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != input || filter.Dropped() != 0 {
		t.Fatalf("filter changed normal traffic: output=%q dropped=%d", output, filter.Dropped())
	}
}

func TestACPClientSurvivesLargeSessionReplay(t *testing.T) {
	requestsReader, requestsWriter := io.Pipe()
	responsesReader, responsesWriter := io.Pipe()
	t.Cleanup(func() {
		_ = requestsReader.Close()
		_ = requestsWriter.Close()
		_ = responsesReader.Close()
		_ = responsesWriter.Close()
	})

	bridge := New(t.TempDir(), "")
	bridge.suppressUpdates.Store(true)
	filter := newSessionLoadNotificationFilter(responsesReader, &bridge.suppressUpdates)
	client := acp.NewClientSideConnection(bridge, requestsWriter, filter)

	peerErr := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(requestsReader).ReadBytes('\n')
		if err != nil {
			peerErr <- err
			return
		}
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &request); err != nil {
			peerErr <- err
			return
		}
		for i := 0; i < 10000; i++ {
			if _, err := fmt.Fprintf(responsesWriter, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"large","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%d"}}}}`+"\n", i); err != nil {
				peerErr <- err
				return
			}
		}
		_, err = fmt.Fprintf(responsesWriter, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", request.ID)
		peerErr <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.LoadSession(ctx, acp.LoadSessionRequest{SessionId: "large", Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("LoadSession failed under replay flood: %v", err)
	}
	if err := <-peerErr; err != nil {
		t.Fatal(err)
	}
	if got := filter.Dropped(); got != 10000 {
		t.Fatalf("dropped = %d, want 10000", got)
	}
}
