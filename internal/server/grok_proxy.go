package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"grok_switch/internal/grokauth"
)

const grokProxyMaxBody = 32 << 20

// 单次客户端请求的最多上游尝试次数：非池模式保留一次同账号内容修复重试；
// 池模式额外允许跨账号故障转移（最多换 2 个号）。
const (
	grokProxyMaxAttemptsSingle = 2
	grokProxyMaxAttemptsPool   = 3
)

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
	sessionID := grokSessionKey(r, hints)
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

// grokSessionKey 提取请求的会话亲和键：优先 Grok CLI 的会话头，其次
// OpenAI 兼容客户端写在 prompt_cache_key 里的会话标识。
func grokSessionKey(r *http.Request, hints grokRequestHints) string {
	return firstNonEmptyServer(
		r.Header.Get("x-grok-conv-id"),
		r.Header.Get("x-grok-session-id"),
		r.Header.Get("x-session-id"),
		r.Header.Get("x-grok-agent-id"),
		hints.PromptCacheKey,
	)
}

func (s *Server) rememberGrokProxySticky(r *http.Request, body []byte, accountID, responseID string) {
	if s.GrokPool == nil || accountID == "" {
		return
	}
	hints := parseGrokRequestHints(body)
	if sessionID := grokSessionKey(r, hints); sessionID != "" {
		s.GrokPool.BindSession(sessionID, accountID)
	}
	if responseID != "" {
		s.GrokPool.BindResponse(responseID, accountID)
	}
}

// grokShouldFailover 判断上游状态码是否值得换号重试：认证失效、额度/权限
// 拒绝、限流与服务端故障都可能是单账号问题；400 类由同账号内容修复处理，
// 404 通常是模型级确定性错误，换号无意义。
func grokShouldFailover(statusCode int) bool {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	}
	return statusCode >= 500
}

// setGrokSwitchHeaders 暴露号池服务账号与换号次数，方便用户在客户端
// 侧排查"请求被哪个账号服务、是否发生过故障转移"。仅池模式调用。
func setGrokSwitchHeaders(w http.ResponseWriter, accountID string, failovers int) {
	if accountID == "" {
		return
	}
	short := accountID
	if len(short) > 8 {
		short = short[:8]
	}
	w.Header().Set("X-Grok-Switch-Account", short)
	if failovers > 0 {
		w.Header().Set("X-Grok-Switch-Failovers", strconv.Itoa(failovers))
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

	poolMode := s.GrokPool != nil && accountID != ""
	maxAttempts := grokProxyMaxAttemptsSingle
	if poolMode {
		maxAttempts = grokProxyMaxAttemptsPool
	}
	excluded := make(map[string]bool)
	sessionKey := grokSessionKey(r, hints)
	failovers := 0

	for attempt := 0; attempt < maxAttempts; attempt++ {
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
			if poolMode {
				s.GrokPool.ObserveResponse(accountID, resp.StatusCode, string(raw))
			}
			// 同账号内容修复：加密推理失效 / 续接引用丢失时剔除后重发。
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
			// 跨账号透明故障转移：换健康号重发，客户端无感。全部尝试都
			// 发生在向客户端写出任何字节之前，重试是安全的。
			if poolMode && attempt+1 < maxAttempts && grokShouldFailover(resp.StatusCode) {
				excluded[accountID] = true
				nextToken, nextID, _, pickErr := s.GrokPool.NextTokenStickyExcluding(r.Context(), sessionKey, hints.PreviousResponseID, excluded)
				if pickErr != nil || nextID == "" {
					// 没有备选账号：透传原始上游错误。
					setGrokSwitchHeaders(w, accountID, failovers)
					copyHeaderSkippingHop(w.Header(), resp.Header)
					w.WriteHeader(resp.StatusCode)
					_, _ = w.Write(raw)
					return
				}
				fmt.Fprintf(os.Stderr, "grok_switch: grok 代理故障转移 (HTTP %d)，更换账号重试\n", resp.StatusCode)
				failovers++
				token = nextToken
				accountID = nextID
				// 新账号不认识旧续接引用，能安全丢弃就先丢，省一次往返。
				if !hints.HasToolOutput {
					if next, ok := dropPreviousResponseID(attemptBody); ok {
						attemptBody = next
					}
				}
				continue
			}
			setGrokSwitchHeaders(w, accountID, failovers)
			copyHeaderSkippingHop(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(raw)
			return
		}

		setGrokSwitchHeaders(w, accountID, failovers)
		copyHeaderSkippingHop(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		responseID, streamError := copyGrokUpstreamBody(w, resp)
		_ = resp.Body.Close()
		if poolMode && streamError != "" {
			s.GrokPool.ObserveStreamError(accountID, streamError)
		}
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

// copyGrokUpstreamBody 流式回传上游响应，同时从已收集前缀里提取
// resp_ ID 与流内错误事件负载。流内错误（HTTP 200 但事件报错）返回给
// 调用方做账号健康观测；正常完成时第二个返回值为空串。
func copyGrokUpstreamBody(w http.ResponseWriter, resp *http.Response) (string, string) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var collected []byte
	collectCap := 1 << 16
	responseID := ""
	streamError := ""
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
				if streamError == "" {
					streamError = extractGrokStreamError(collected)
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
	if streamError == "" {
		streamError = extractGrokStreamError(collected)
	}
	return responseID, streamError
}

// extractGrokStreamError 在已收集的响应前缀里找 SSE 错误事件
// （type=error / response.failed），返回可归类的错误负载文本。
func extractGrokStreamError(raw []byte) string {
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
		switch strings.ToLower(stringValue(payload["type"])) {
		case "error", "response.failed":
		default:
			continue
		}
		code, message := grokErrorCodeFromMap(payload)
		if nested, ok := payload["response"].(map[string]any); ok {
			nestedCode, nestedMessage := grokErrorCodeFromMap(nested)
			code = firstNonEmptyServer(code, nestedCode)
			message = firstNonEmptyServer(message, nestedMessage)
		}
		if combined := strings.TrimSpace(code + " " + message); combined != "" {
			return truncateTextServer(combined, 1000)
		}
		return truncateTextServer(string(line), 1000)
	}
	return ""
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
	return grokErrorCodeFromMap(payload)
}

func grokErrorCodeFromMap(payload map[string]any) (string, string) {
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
