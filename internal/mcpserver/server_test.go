package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

func TestLoopReturnsParseErrorForMalformedRequest(t *testing.T) {
	var out bytes.Buffer
	s := &server{
		in:  bufio.NewReader(bytes.NewBufferString("{\n")),
		out: bufio.NewWriter(&out),
	}
	if err := s.loop(); err != nil {
		t.Fatal(err)
	}
	var response rpcResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != -32700 {
		t.Fatalf("response error = %#v, want JSON-RPC parse error", response.Error)
	}
}
