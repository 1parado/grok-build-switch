package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"grok_switch/internal/grokauth"
)

const grokProxyMaxBody = 32 << 20

var grokHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"host":                true,
}

type grokRequestHints struct {
	Model              string
	PreviousResponseID string
	PromptCacheKey     string
	HasToolOutput      bool
}

// isWebSocketUpgrade 识别 WebSocket 升级请求。手写转发会剥离 hop-by-hop 头
// （含 Connection/Upgrade），无法像旧的 httputil.ReverseProxy 一样隧道化
// WS；这类请求必须显式拒绝，避免客户端对着挂死的连接排障。
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func (s *Server) grokUpstreamBase() string {
	if strings.TrimSpace(s.GrokUpstream) != "" {
		return strings.TrimRight(strings.TrimSpace(s.GrokUpstream), "/")
	}
	return strings.TrimRight(grokauth.UpstreamURL(), "/")
}

func (s *Server) grokProxyAuthorized(r *http.Request) bool {
	if s.GrokPool != nil && s.GrokPool.Authorized(r) {
		return true
	}
	return s.GrokAuth != nil && s.GrokAuth.Authorized(r)
}

func (s *Server) grokProxyToken(ctx context.Context, r *http.Request, body []byte) (token, accountID string, lostContinuation bool, err error) {
	hints := parseGrokRequestHints(body)
	sessionID := firstNonEmptyServer(
		r.Header.Get("x-grok-conv-id"),
		r.Header.Get("x-grok-session-id"),
		r.Header.Get("x-session-id"),
		r.Header.Get("x-grok-agent-id"),
		hints.PromptCacheKey,
	)
	if s.GrokPool != nil && s.GrokPool.Authorized(r) {
		token, accountID, lostContinuation, err = s.GrokPool.NextTokenSticky(ctx, sessionID, hints.PreviousResponseID)
		if err != nil && s.singleGrokAuthConfigured() {
			accountID = ""
			lostContinuation = false
			token, err = s.GrokAuth.Token(ctx)
		}
		return token, accountID, lostContinuation, err
	}
	if s.GrokAuth != nil && s.GrokAuth.Authorized(r) {
		token, err = s.GrokAuth.Token(ctx)
		return token, "", false, err
	}
	return "", "", false, fmt.Errorf("无效的本地 API Key")
}

func (s *Server) rememberGrokProxySticky(r *http.Request, body []byte, accountID, responseID string) {
	if s.GrokPool == nil || accountID == "" {
		return
	}
	hints := parseGrokRequestHints(body)
	sessionID := firstNonEmptyServer(
		r.Header.Get("x-grok-conv-id"),
		r.Header.Get("x-grok-session-id"),
		r.Header.Get("x-session-id"),
		r.Header.Get("x-grok-agent-id"),
		hints.PromptCacheKey,
	)
	if sessionID != "" {
		s.GrokPool.BindSession(sessionID, accountID)
	}
	if responseID != "" {
		s.GrokPool.BindResponse(responseID, accountID)
	}
}

func (s *Server) forwardGrokUpstream(w http.ResponseWriter, r *http.Request, token, accountID string, body []byte, lostContinuation bool) {
	hints := parseGrokRequestHints(body)
	attemptBody := body
	if lostContinuation && !hints.HasToolOutput {
		if next, ok := dropPreviousResponseID(attemptBody); ok {
			attemptBody = next
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := s.doGrokUpstream(r, token, attemptBody, hints.Model)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				writeError(w, fmt.Errorf("Grok 上游请求失败: %w", err), http.StatusBadGateway)
			}
			return
		}

		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if s.GrokPool != nil && accountID != "" {
				s.GrokPool.ObserveResponse(accountID, resp.StatusCode, string(raw))
			}
			if attempt == 0 {
				if isGrokInvalidEncryptedContent(resp.StatusCode, raw) {
					if next, ok := stripEncryptedReasoning(attemptBody); ok {
						attemptBody = next
						continue
					}
				}
				if isPreviousResponseNotFound(resp.StatusCode, raw) && !hints.HasToolOutput {
					if next, ok := dropPreviousResponseID(attemptBody); ok {
						attemptBody = next
						continue
					}
				}
			}
			copyHeaderSkippingHop(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(raw)
			return
		}

		copyHeaderSkippingHop(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		responseID := copyGrokUpstreamBody(w, resp)
		_ = resp.Body.Close()
		if resp.StatusCode < 300 {
			s.rememberGrokProxySticky(r, body, accountID, responseID)
		}
		return
	}
}

func (s *Server) doGrokUpstream(r *http.Request, token string, body []byte, model string) (*http.Response, error) {
	suffix := strings.TrimPrefix(r.URL.Path, "/grok/v1")
	if suffix == "" {
		suffix = "/"
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, s.grokUpstreamBase()+suffix, reader)
	if err != nil {
		return nil, err
	}
	if r.URL.RawQuery != "" {
		req.URL.RawQuery = r.URL.RawQuery
	}
	copyHeaderSkippingHop(req.Header, r.Header)
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	grokauth.ApplyCLIProxyHeaders(req.Header, r.Header, model)
	req.Header.Set("Authorization", "Bearer "+token)
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if body != nil {
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	}
	req.Host = req.URL.Host

	client := &http.Client{}
	if s.GrokPool != nil {
		if transport := s.GrokPool.Transport(); transport != nil {
			client.Transport = transport
		}
	}
	return client.Do(req)
}

func copyGrokUpstreamBody(w http.ResponseWriter, resp *http.Response) string {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var collected []byte
	collectCap := 1 << 16
	responseID := ""
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
			if len(collected) < collectCap {
				need := collectCap - len(collected)
				if need > n {
					need = n
				}
				collected = append(collected, buf[:need]...)
				if responseID == "" {
					responseID = extractGrokResponseID(collected)
				}
			}
		}
		if err != nil {
			break
		}
	}
	if responseID == "" {
		responseID = extractGrokResponseID(collected)
	}
	return responseID
}

func parseGrokRequestHints(body []byte) grokRequestHints {
	var hints grokRequestHints
	if len(body) == 0 {
		return hints
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return hints
	}
	hints.Model = stringValue(raw["model"])
	hints.PreviousResponseID = stringValue(raw["previous_response_id"])
	hints.PromptCacheKey = stringValue(raw["prompt_cache_key"])
	hints.HasToolOutput = grokInputHasToolOutput(raw["input"])
	return hints
}

func grokInputHasToolOutput(input any) bool {
	switch typed := input.(type) {
	case []any:
		for _, item := range typed {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ := strings.ToLower(stringValue(obj["type"]))
			if typ == "function_call_output" || typ == "custom_tool_call_output" || typ == "item_reference" {
				return true
			}
		}
	case map[string]any:
		typ := strings.ToLower(stringValue(typed["type"]))
		if typ == "function_call_output" || typ == "custom_tool_call_output" {
			return true
		}
	}
	return false
}

func stringValue(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func dropPreviousResponseID(body []byte) ([]byte, bool) {
	if len(body) == 0 || !bytes.Contains(body, []byte("previous_response_id")) {
		return body, false
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return body, false
	}
	if _, ok := raw["previous_response_id"]; !ok {
		return body, false
	}
	delete(raw, "previous_response_id")
	out, err := json.Marshal(raw)
	if err != nil {
		return body, false
	}
	return out, true
}

func stripEncryptedReasoning(body []byte) ([]byte, bool) {
	if len(body) == 0 || !bytes.Contains(body, []byte("encrypted_content")) {
		return body, false
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return body, false
	}
	input, ok := raw["input"].([]any)
	if !ok {
		return body, false
	}
	changed := false
	next := make([]any, 0, len(input))
	for _, item := range input {
		obj, ok := item.(map[string]any)
		if !ok {
			next = append(next, item)
			continue
		}
		if strings.TrimSpace(stringValue(obj["type"])) != "reasoning" {
			next = append(next, item)
			continue
		}
		if _, has := obj["encrypted_content"]; has {
			delete(obj, "encrypted_content")
			changed = true
		}
		if grokReasoningHasSummary(obj) {
			next = append(next, obj)
		} else {
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	raw["input"] = next
	out, err := json.Marshal(raw)
	if err != nil {
		return body, false
	}
	return out, true
}

func grokReasoningHasSummary(obj map[string]any) bool {
	summary, ok := obj["summary"].([]any)
	if !ok {
		return strings.TrimSpace(stringValue(obj["summary"])) != ""
	}
	for _, part := range summary {
		partObj, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(stringValue(partObj["text"])) != "" {
			return true
		}
	}
	return false
}

func isGrokInvalidEncryptedContent(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	code, message := grokErrorCodeMessage(body)
	if strings.EqualFold(code, "invalid_encrypted_content") {
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	if code != "" && !strings.EqualFold(code, "invalid-argument") {
		return false
	}
	if code == "" && !strings.Contains(normalized, "decrypt") {
		return false
	}
	return strings.Contains(normalized, "encrypted_content") &&
		(strings.Contains(normalized, "decrypt") || strings.Contains(normalized, "unmodified"))
}

func isPreviousResponseNotFound(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusNotFound {
		return false
	}
	code, message := grokErrorCodeMessage(body)
	blob := strings.ToLower(code + " " + message + " " + string(body))
	// Match both "previous_response_id" and "previous response ..." phrasing;
	// xAI ZDR accounts reject continuation with "Previous response cannot be
	// used for this organization due to Zero Data Retention".
	compact := strings.ReplaceAll(strings.ReplaceAll(blob, "_", ""), " ", "")
	if !strings.Contains(compact, "previousresponse") {
		return false
	}
	return strings.Contains(compact, "notfound") || strings.Contains(blob, "not_found") ||
		strings.Contains(blob, "unknown") || strings.Contains(blob, "invalid") ||
		strings.Contains(blob, "cannot be used") || strings.Contains(blob, "zero data retention")
}

func grokErrorCodeMessage(body []byte) (string, string) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return "", string(body)
	}
	code := stringValue(payload["code"])
	switch errNode := payload["error"].(type) {
	case string:
		if code == "" {
			return "", errNode
		}
		return code, errNode
	case map[string]any:
		if code == "" {
			code = stringValue(errNode["code"])
		}
		return code, firstNonEmptyServer(stringValue(errNode["message"]), stringValue(errNode["error"]))
	default:
		return code, stringValue(payload["message"])
	}
}

func extractGrokResponseID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(line, &payload) != nil {
			continue
		}
		if id := grokResponseIDFromMap(payload); id != "" {
			return id
		}
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) == nil {
		return grokResponseIDFromMap(payload)
	}
	return ""
}

func grokResponseIDFromMap(payload map[string]any) string {
	if id := stringValue(payload["id"]); strings.HasPrefix(id, "resp_") {
		return id
	}
	if nested, ok := payload["response"].(map[string]any); ok {
		if id := stringValue(nested["id"]); strings.HasPrefix(id, "resp_") {
			return id
		}
	}
	return ""
}

func copyHeaderSkippingHop(dst, src http.Header) {
	for key, values := range src {
		if grokHopHeaders[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
