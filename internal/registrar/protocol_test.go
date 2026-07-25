package registrar

import (
	"testing"
)

func TestEncodeProtoAndGRPCWebFrame(t *testing.T) {
	payload := encodeProtoStrings([]protoStringField{{1, "test@example.com"}})
	if len(payload) < 5 {
		t.Fatalf("payload too short: %v", payload)
	}
	// field 1, wire type 2 => tag 0x0a
	if payload[0] != 0x0a {
		t.Fatalf("tag byte = %#x, want 0x0a", payload[0])
	}
	frame := encodeGRPCWebFrame(payload)
	if len(frame) != 5+len(payload) || frame[0] != 0 {
		t.Fatalf("bad frame len/flag: len=%d flag=%d", len(frame), frame[0])
	}
}

func TestParseGRPCWebStatusOK(t *testing.T) {
	// trailer-only frame: flag 0x80, "grpc-status:0\r\n"
	trailer := []byte("grpc-status:0\r\n")
	frame := make([]byte, 5+len(trailer))
	frame[0] = 0x80
	frame[1] = 0
	frame[2] = 0
	frame[3] = 0
	frame[4] = byte(len(trailer))
	copy(frame[5:], trailer)
	status, msg := parseGRPCWebStatus(frame)
	if status != 0 || msg != "" {
		t.Fatalf("status=%d msg=%q", status, msg)
	}
}

func TestParseGRPCWebStatusError(t *testing.T) {
	trailer := []byte("grpc-status:7\r\ngrpc-message:permission%20denied\r\n")
	frame := make([]byte, 5+len(trailer))
	frame[0] = 0x80
	frame[4] = byte(len(trailer))
	copy(frame[5:], trailer)
	status, msg := parseGRPCWebStatus(frame)
	if status != 7 {
		t.Fatalf("status=%d", status)
	}
	if msg != "permission denied" {
		t.Fatalf("msg=%q", msg)
	}
}
