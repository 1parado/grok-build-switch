package grokpool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const inspectionClientVersion = "0.2.93"

type probeResponse struct {
	StatusCode int
	Body       string
}

type probeError struct {
	Code    string
	Message string
}

type classification struct {
	Name   string
	Action string
	Reason string
}

type probeOutcome struct {
	Response probeResponse
	Error    probeError
	Class    classification
}

func (m *Manager) StartInspection() error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return fmt.Errorf("巡检已在运行")
	}
	if len(m.state.Accounts) == 0 {
		m.mu.Unlock()
		return fmt.Errorf("号池中没有账号")
	}
	accounts := append([]Account(nil), m.state.Accounts...)
	workers := m.state.Settings.Workers
	ctx, cancel := context.WithCancel(context.Background())
	m.runCancel = cancel
	m.running = true
	m.doneCount = 0
	m.totalCount = len(accounts)
	m.nextRun = time.Time{}
	m.state.LastError = ""
	_ = m.saveLocked()
	m.mu.Unlock()
	m.signalWake()

	m.runWG.Add(1)
	go func() {
		defer m.runWG.Done()
		m.runInspection(ctx, accounts, workers)
	}()
	return nil
}

func (m *Manager) StopInspection() {
	m.mu.Lock()
	if m.runCancel != nil {
		m.runCancel()
	}
	m.mu.Unlock()
}

func (m *Manager) runInspection(ctx context.Context, accounts []Account, workers int) {
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
accountLoop:
	for _, account := range accounts {
		select {
		case <-ctx.Done():
			break accountLoop
		case sem <- struct{}{}:
		}
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			result := m.inspectAccount(ctx, account)
			m.recordInspection(result)
		}()
	}
	wg.Wait()

	m.mu.Lock()
	m.running = false
	m.runCancel = nil
	m.state.LastRun = time.Now().UTC()
	if ctx.Err() != nil {
		m.state.LastError = "巡检已停止"
	}
	if m.rerunRequested && m.state.Settings.Enabled && len(m.state.Accounts) > 0 {
		m.nextRun = time.Now().UTC()
		m.rerunRequested = false
	} else if m.state.Settings.Enabled && len(m.state.Accounts) > 0 {
		m.nextRun = m.state.LastRun.Add(time.Duration(m.state.Settings.IntervalMinutes) * time.Minute)
	} else {
		m.nextRun = time.Time{}
	}
	_ = m.saveLocked()
	m.mu.Unlock()
	m.signalWake()
}

func (m *Manager) recordInspection(result Account) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.state.Accounts {
		if m.state.Accounts[i].ID != result.ID {
			continue
		}
		result.Disabled = m.state.Accounts[i].Disabled
		result.FileName = firstNonEmpty(result.FileName, m.state.Accounts[i].FileName)
		result.ImportedAt = m.state.Accounts[i].ImportedAt
		m.state.Accounts[i] = result
		break
	}
	m.doneCount++
	_ = m.saveLocked()
}

// ObserveResponse lets the live proxy react before the next scheduled sweep.
// Only explicit upstream responses are recorded; generic 429/network noise stays eligible.
func (m *Manager) ObserveResponse(id string, status int, body string) {
	if strings.TrimSpace(id) == "" || status < 400 {
		return
	}
	parsed := extractProbeError(body)
	classified := classify(status, parsed.Code, parsed.Message, false)
	m.recordObservation(id, classified, parsed, status)
}

// ObserveStreamError 记录 HTTP 200 响应中流式错误事件暴露的账号健康信号
// （如免费额度耗尽、认证失效），让坏号即时隔离而不必等下一轮巡检。
// 无法识别的错误归类为 unknown，不落库，避免把网络噪音当成账号问题。
func (m *Manager) ObserveStreamError(id string, body string) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(body) == "" {
		return
	}
	parsed := extractProbeError(body)
	classified := classify(0, parsed.Code, parsed.Message, false)
	if classified.Name == "unknown" {
		return
	}
	m.recordObservation(id, classified, parsed, 0)
}

func (m *Manager) recordObservation(id string, classified classification, parsed probeError, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.state.Accounts {
		account := &m.state.Accounts[i]
		if account.ID != id {
			continue
		}
		account.Classification = classified.Name
		account.Action = classified.Action
		account.Reason = classified.Reason
		if status > 0 {
			account.HTTPStatus = status
		}
		account.ErrorCode = parsed.Code
		account.ErrorMessage = parsed.Message
		account.LastInspected = time.Now().UTC()
		_ = m.saveLocked()
		return
	}
}

func (m *Manager) inspectAccount(ctx context.Context, account Account) Account {
	result := account
	result.LastInspected = time.Now().UTC()
	result.HTTPStatus = 0
	result.ErrorCode = ""
	result.ErrorMessage = ""

	store := m.accountStore(account.ID)
	token, err := store.Token(ctx)
	if err != nil {
		result.ErrorMessage = err.Error()
		blob := strings.ToLower(err.Error())
		if containsAny(blob, "invalid_grant", "已过期", "没有 refresh_token", "missing refresh") {
			result.Classification = "reauth"
			result.Action = "delete"
			result.Reason = "认证已过期或刷新失败"
		} else {
			result.Classification = "probe_error"
			result.Action = "keep"
			result.Reason = "读取或刷新 token 失败"
		}
		return result
	}
	if status, statusErr := store.Status(); statusErr == nil {
		result.Email = firstNonEmpty(status.Email, result.Email)
		result.ExpiresAt = status.ExpiresAt
	}

	model := "grok-4.6"
	modelsResp, modelsErr := m.doProbe(ctx, http.MethodGet, m.upstreamURL+"/models", token, nil)
	if modelsErr == nil && modelsResp.StatusCode >= 200 && modelsResp.StatusCode < 300 {
		model = pickModel(modelsResp.Body)
	}
	result.Model = model

	responseBody, _ := json.Marshal(map[string]any{
		"model":             model,
		"input":             "ping",
		"stream":            false,
		"max_output_tokens": 1,
	})
	primary, err := m.doProbe(ctx, http.MethodPost, m.upstreamURL+"/responses", token, responseBody)
	if err != nil {
		result.Classification = "probe_error"
		result.Action = "keep"
		result.Reason = "探测请求失败"
		result.ErrorMessage = err.Error()
		return result
	}
	parsed := extractProbeError(primary.Body)
	if primary.StatusCode == http.StatusTooManyRequests && !isFreeUsageExhausted(parsed.Code, parsed.Message) {
		select {
		case <-ctx.Done():
			result.Classification = "probe_error"
			result.Action = "keep"
			result.Reason = "巡检已停止"
			result.ErrorMessage = ctx.Err().Error()
			return result
		case <-time.After(350 * time.Millisecond):
		}
		if retry, retryErr := m.doProbe(ctx, http.MethodPost, m.upstreamURL+"/responses", token, responseBody); retryErr == nil {
			primary = retry
		}
	}

	outcome := classifyResponse(primary, account.Disabled)
	switch primary.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		fallbackBody, _ := json.Marshal(map[string]any{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": "ping"}},
			"stream":   false,
		})
		if fallback, fallbackErr := m.doProbe(ctx, http.MethodPost, m.upstreamURL+"/chat/completions", token, fallbackBody); fallbackErr == nil {
			outcome = resolveOutcomes(outcome, classifyResponse(fallback, account.Disabled))
		}
	}

	result.HTTPStatus = outcome.Response.StatusCode
	if outcome.Response.StatusCode < 200 || outcome.Response.StatusCode >= 300 {
		result.ErrorCode = outcome.Error.Code
		result.ErrorMessage = outcome.Error.Message
	}
	result.Classification = outcome.Class.Name
	result.Action = outcome.Class.Action
	result.Reason = outcome.Class.Reason
	return result
}

func (m *Manager) doProbe(ctx context.Context, method, rawURL, token string, body []byte) (probeResponse, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return probeResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", inspectionClientVersion)
	req.Header.Set("User-Agent", "xai-grok-workspace/"+inspectionClientVersion)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return probeResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return probeResponse{}, err
	}
	return probeResponse{StatusCode: resp.StatusCode, Body: string(raw)}, nil
}

func classifyResponse(response probeResponse, disabled bool) probeOutcome {
	parsed := extractProbeError(response.Body)
	return probeOutcome{
		Response: response,
		Error:    parsed,
		Class:    classify(response.StatusCode, parsed.Code, parsed.Message, disabled),
	}
}

func resolveOutcomes(primary, fallback probeOutcome) probeOutcome {
	switch primary.Class.Name {
	case "reauth", "quota_exhausted", "permission_denied":
		if fallback.Class.Name == "healthy" {
			primary.Class.Reason += "；备用接口结果不一致，按主探测判定"
		}
		return primary
	default:
		return fallback
	}
}

func classify(status int, code, message string, disabled bool) classification {
	blob := strings.ToLower(strings.TrimSpace(code + " " + message))
	if status == http.StatusUnauthorized || containsAny(blob, "token is expired", "token has been invalidated", "invalid_grant", "unauthorized") {
		return classification{Name: "reauth", Action: "delete", Reason: "认证已过期或失效"}
	}
	if isFreeUsageExhausted(code, message) {
		action := "disable"
		if disabled {
			action = "keep"
		}
		return classification{Name: "quota_exhausted", Action: action, Reason: "免费额度已用尽"}
	}
	if status == http.StatusTooManyRequests {
		return classification{Name: "probe_error", Action: "keep", Reason: "临时限流 (HTTP 429)，稍后重试"}
	}
	if status == http.StatusPaymentRequired || status == http.StatusForbidden || containsAny(blob,
		"permission-denied", "chat endpoint is denied", "deactivated", "suspended", "banned") {
		action := "disable"
		if disabled {
			action = "keep"
		}
		return classification{Name: "permission_denied", Action: action, Reason: fmt.Sprintf("对话权限被拒绝 (HTTP %d)", status)}
	}
	if status == http.StatusNotFound || containsAny(blob, "not-found", "does not exist", "no access to it") {
		return classification{Name: "model_unavailable", Action: "keep", Reason: "测试模型不可用"}
	}
	if status >= 200 && status < 300 {
		action := "keep"
		if disabled {
			action = "enable"
		}
		return classification{Name: "healthy", Action: action, Reason: "对话测试成功"}
	}
	if status > 0 {
		return classification{Name: "probe_error", Action: "keep", Reason: fmt.Sprintf("探测失败 (HTTP %d)", status)}
	}
	return classification{Name: "unknown", Action: "keep", Reason: "无法可靠分类"}
}

func extractProbeError(body string) probeError {
	body = strings.TrimSpace(body)
	if body == "" {
		return probeError{}
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return probeError{Message: compactProbeText(body)}
	}
	code := stringField(data["code"])
	message := ""
	switch value := data["error"].(type) {
	case map[string]any:
		code = firstNonEmpty(code, stringField(value["code"]))
		message = firstNonEmpty(stringField(value["message"]), stringField(value["error"]))
	case string:
		message = value
	}
	message = compactProbeText(firstNonEmpty(message, stringField(data["message"]), body))
	return probeError{Code: code, Message: message}
}

func compactProbeText(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 1000 {
		return text[:1000] + "…"
	}
	return text
}

func isFreeUsageExhausted(code, message string) bool {
	return containsAny(strings.ToLower(code+" "+message),
		"free-usage-exhausted",
		"used all the included free usage",
		"included free usage has been exhausted",
	)
}

func containsAny(text string, values ...string) bool {
	text = strings.ToLower(text)
	for _, value := range values {
		if value != "" && strings.Contains(text, strings.ToLower(value)) {
			return true
		}
	}
	return false
}

func pickModel(body string) string {
	var payload struct {
		Data []struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &payload)
	ids := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if id := firstNonEmpty(item.ID, item.Model); id != "" {
			ids = append(ids, id)
		}
	}
	for _, preferred := range []string{"grok-4.6", "grok-4.6-build-free", "grok-4", "grok-3-mini"} {
		for _, id := range ids {
			if id == preferred {
				return id
			}
		}
	}
	if len(ids) > 0 {
		return ids[0]
	}
	return "grok-4.6"
}

func stringField(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
