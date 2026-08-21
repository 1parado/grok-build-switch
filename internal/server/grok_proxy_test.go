package server

import (
	"net/http"
	"testing"
)

func TestIsPreviousResponseNotFoundMatchesZDRRejection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"zdr rejection", 404, `{"code":"not-found","error":"Previous response cannot be used for this organization due to Zero Data Retention"}`, true},
		{"not found", 400, `{"error":{"code":"previous_response_not_found","message":"Unknown previous response"}}`, true},
		{"not found plain", 404, `{"code":"previous_response_not_found"}`, true},
		{"unrelated 400", 400, `{"error":{"message":"invalid model"}}`, false},
		{"unrelated 404", 404, `{"error":"model not found"}`, false},
		{"200", 200, `{"id":"resp_1"}`, false},
	}
	for _, tc := range cases {
		if got := isPreviousResponseNotFound(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: isPreviousResponseNotFound(%d, %s) = %v, want %v", tc.name, tc.status, tc.body, got, tc.want)
		}
	}
}

func TestIsGrokInvalidEncryptedContent(t *testing.T) {
	if !isGrokInvalidEncryptedContent(400, []byte(`{"code":"invalid_encrypted_content"}`)) {
		t.Fatal("code invalid_encrypted_content not detected")
	}
	if !isGrokInvalidEncryptedContent(400, []byte(`{"code":"invalid-argument","error":"Could not decrypt the provided encrypted_content."}`)) {
		t.Fatal("decrypt message not detected")
	}
	if isGrokInvalidEncryptedContent(400, []byte(`{"error":"bad tool"}`)) {
		t.Fatal("unrelated 400 misdetected")
	}
	if isGrokInvalidEncryptedContent(500, []byte(`{"code":"invalid_encrypted_content"}`)) {
		t.Fatal("non-400 misdetected")
	}
}

func TestStripEncryptedReasoningRemovesEncryptedItems(t *testing.T) {
	body := []byte(`{"model":"grok-4.6","previous_response_id":"r1","input":[{"type":"reasoning","encrypted_content":"abc"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	next, changed := stripEncryptedReasoning(body)
	if !changed {
		t.Fatal("stripEncryptedReasoning reported no change")
	}
	if want := "abc"; containsBytes(next, want) {
		t.Fatalf("encrypted_content still present: %s", next)
	}
	if !containsBytes(next, "hi") {
		t.Fatalf("user message dropped: %s", next)
	}
}

func TestDropPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"grok-4.6","previous_response_id":"resp-1","input":"hello"}`)
	next, ok := dropPreviousResponseID(body)
	if !ok {
		t.Fatal("dropPreviousResponseID reported no change")
	}
	if containsBytes(next, "resp-1") {
		t.Fatalf("previous_response_id still present: %s", next)
	}
	if !containsBytes(next, "hello") {
		t.Fatalf("input lost: %s", next)
	}
}

func TestParseGrokRequestHintsDetectsToolOutput(t *testing.T) {
	hints := parseGrokRequestHints([]byte(`{"model":"grok-4.6","previous_response_id":"r1","input":[{"type":"function_call_output","call_id":"c1","output":"ok"}]}`))
	if !hints.HasToolOutput {
		t.Fatal("function_call_output not detected")
	}
	if hints.PreviousResponseID != "r1" || hints.Model != "grok-4.6" {
		t.Fatalf("hints = %#v", hints)
	}
	plain := parseGrokRequestHints([]byte(`{"model":"grok-4.6","input":[{"type":"message","role":"user"}]}`))
	if plain.HasToolOutput {
		t.Fatal("plain input misdetected as tool output")
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		want   bool
	}{
		{"standard upgrade", http.Header{"Upgrade": []string{"websocket"}, "Connection": []string{"Upgrade"}}, true},
		{"case insensitive", http.Header{"Upgrade": []string{"WebSocket"}, "Connection": []string{"keep-alive, Upgrade"}}, true},
		{"upgrade without connection token", http.Header{"Upgrade": []string{"websocket"}, "Connection": []string{"keep-alive"}}, false},
		{"connection only", http.Header{"Connection": []string{"Upgrade"}}, false},
		{"plain request", http.Header{"User-Agent": []string{"xai-grok-workspace/0.2.114"}}, false},
	}
	for _, tc := range cases {
		r := &http.Request{Header: tc.header}
		if got := isWebSocketUpgrade(r); got != tc.want {
			t.Errorf("%s: isWebSocketUpgrade = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func containsBytes(haystack []byte, needle string) bool {
	return len(needle) > 0 && indexOf(haystack, needle) >= 0
}

func indexOf(haystack []byte, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
