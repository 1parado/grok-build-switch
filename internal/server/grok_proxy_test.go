package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"grok_switch/internal/grokauth"
	"grok_switch/internal/grokpool"
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

func TestGrokShouldFailover(t *testing.T) {
	cases := map[int]bool{
		http.StatusUnauthorized:        true,
		http.StatusPaymentRequired:     true,
		http.StatusForbidden:           true,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
		http.StatusBadRequest:          false,
		http.StatusNotFound:            false,
		http.StatusOK:                  false,
	}
	for status, want := range cases {
		if got := grokShouldFailover(status); got != want {
			t.Errorf("grokShouldFailover(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestExtractGrokStreamError(t *testing.T) {
	failed := extractGrokStreamError([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"free-usage-exhausted\",\"message\":\"used all free usage\"}}}\n"))
	if !strings.Contains(failed, "free-usage-exhausted") {
		t.Fatalf("response.failed not extracted: %q", failed)
	}
	plain := extractGrokStreamError([]byte("data: {\"type\":\"error\",\"error\":\"boom\"}\n"))
	if plain != "boom" {
		t.Fatalf("plain error event = %q", plain)
	}
	if got := extractGrokStreamError([]byte("data: {\"type\":\"response.created\"}\n")); got != "" {
		t.Fatalf("normal event misdetected: %q", got)
	}
	if got := extractGrokStreamError([]byte(": keep-alive\n\n")); got != "" {
		t.Fatalf("comment line misdetected: %q", got)
	}
}

// isGrokProxyCall 区分代理链路请求与号池后台巡检探测：两者在测试里共用
// 被替换的 http.DefaultTransport，巡检固定写 0.2.93 客户端版本，代理链路
// 统一钉在 grokauth.CLIClientVersion。
func isGrokProxyCall(request *http.Request) bool {
	return request.Header.Get("x-grok-client-version") == grokauth.CLIClientVersion
}

// probeOKResponse 给后台巡检探测返回健康结果，保持账号可用且不污染断言。
func probeOKResponse(request *http.Request) (*http.Response, error) {
	body := `{"data":[{"id":"grok-4.6"}]}`
	if request.Method == http.MethodPost {
		body = `{"id":"resp_probe","status":"completed"}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}, nil
}

// waitForInitialInspection 等待 Import 触发的后台巡检全部落库，
// 避免巡检结果异步覆盖测试断言的账号状态。
func waitForInitialInspection(t *testing.T, pool *grokpool.Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		accounts := pool.Status().Accounts
		done := 0
		for _, account := range accounts {
			if !account.LastInspected.IsZero() {
				done++
			}
		}
		if len(accounts) >= want && done >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background inspection did not finish for %d accounts", want)
}

func newFailoverTestPool(t *testing.T, tokens ...string) *grokpool.Manager {
	t.Helper()
	dir := t.TempDir()
	pool, err := grokpool.NewManager(filepath.Join(dir, "pool"))
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	files := make([]grokpool.ImportFile, 0, len(tokens))
	for i, token := range tokens {
		files = append(files, grokpool.ImportFile{
			Name:    fmt.Sprintf("acc-%d.json", i),
			Content: fmt.Sprintf(`{"type":"xai","access_token":%q,"expired":%q,"email":"acc-%d@example.com"}`, token, expiry, i),
		})
	}
	if _, err := pool.Import(files); err != nil {
		t.Fatal(err)
	}
	return pool
}

func newProxiedGrokRequest(pool *grokpool.Manager, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+pool.Status().LocalAPIKey)
	return request
}

func TestGrokProxyFailsOverToNextPoolAccountOnRateLimit(t *testing.T) {
	pool := newFailoverTestPool(t, "pool-a", "pool-b")

	var upstreamAuths []string
	previousTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if !isGrokProxyCall(request) {
			return probeOKResponse(request)
		}
		upstreamAuths = append(upstreamAuths, request.Header.Get("Authorization"))
		if len(upstreamAuths) == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Status:     "429 Too Many Requests",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"code":"rate_limited","error":"Too Many Requests"}`)),
				Request:    request,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_ok"}`)),
			Request:    request,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	waitForInitialInspection(t, pool, pool.Status().Summary.Total)

	server := &Server{GrokPool: pool}
	recorder := httptest.NewRecorder()
	server.handleGrokProxy(recorder, newProxiedGrokRequest(pool, `{"model":"grok-4.6","input":"hi"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(upstreamAuths) != 2 || upstreamAuths[0] == upstreamAuths[1] {
		t.Fatalf("upstream calls = %v, want two different bearer tokens", upstreamAuths)
	}

	// 成功后粘性绑定应落到新账号：连续两次按 resp_ok 取号必须命中同一账号。
	_, firstBound, _, err := pool.NextTokenSticky(t.Context(), "", "resp_ok")
	if err != nil {
		t.Fatal(err)
	}
	_, secondBound, _, err := pool.NextTokenSticky(t.Context(), "", "resp_ok")
	if err != nil {
		t.Fatal(err)
	}
	if firstBound == "" || firstBound != secondBound {
		t.Fatalf("resp_ok binding unstable after failover: %q vs %q", firstBound, secondBound)
	}
}

func TestGrokProxyPassesThroughWhenNoAlternativeAccount(t *testing.T) {
	pool := newFailoverTestPool(t, "pool-only")

	upstreamCalls := 0
	previousTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if !isGrokProxyCall(request) {
			return probeOKResponse(request)
		}
		upstreamCalls++
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"code":"rate_limited","error":"Too Many Requests"}`)),
			Request:    request,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	waitForInitialInspection(t, pool, pool.Status().Summary.Total)

	server := &Server{GrokPool: pool}
	recorder := httptest.NewRecorder()
	server.handleGrokProxy(recorder, newProxiedGrokRequest(pool, `{"model":"grok-4.6","input":"hi"}`))

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("proxy status = %d, want passthrough 429", recorder.Code)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no alternative account)", upstreamCalls)
	}
}

func TestGrokProxyObservesStreamErrorClassification(t *testing.T) {
	pool := newFailoverTestPool(t, "pool-exhausted")

	previousTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if !isGrokProxyCall(request) {
			return probeOKResponse(request)
		}
		body := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"free-usage-exhausted\",\"message\":\"You have used all the included free usage\"}}}\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	waitForInitialInspection(t, pool, pool.Status().Summary.Total)

	server := &Server{GrokPool: pool}
	recorder := httptest.NewRecorder()
	server.handleGrokProxy(recorder, newProxiedGrokRequest(pool, `{"model":"grok-4.6","input":"hi"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	status := pool.Status()
	if len(status.Accounts) != 1 || status.Accounts[0].Classification != "quota_exhausted" {
		t.Fatalf("classification after stream error = %#v", status.Accounts)
	}
}
