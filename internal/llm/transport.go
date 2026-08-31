package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout = 10 * time.Minute
	// sseIdleTimeout 是两次 SSE 事件之间的最大等待；上游停流超过它即失败。
	sseIdleTimeout = 120 * time.Second
	maxErrorBody   = 8 << 10
)

// transport 构造共享 HTTP 客户端（两个适配器复用）。
func newHTTPClient(target UpstreamTarget) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if strings.TrimSpace(target.ProxyURL) != "" {
		pu, err := url.Parse(target.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("解析代理地址失败: %w", err)
		}
		switch pu.Scheme {
		case "http", "https", "socks5", "socks5h":
			transport.Proxy = http.ProxyURL(pu)
		default:
			return nil, fmt.Errorf("不支持的代理协议 %q", pu.Scheme)
		}
	}
	timeout := target.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// doJSON 发起 POST JSON 请求，返回 resp（调用方负责关闭 body）。
// 4xx/5xx 不视为传输错误，由调用方按协议解析错误体。
func doJSON(ctx context.Context, client httpClient, target UpstreamTarget, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(target.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if target.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
	}
	for k, v := range target.ExtraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, wrapNetworkError(err)
	}
	return resp, nil
}

// wrapNetworkError 把传输层错误归类为 APIError{Kind: network/timeout}。
func wrapNetworkError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return NewAPIError("timeout", 0, err.Error(), 0)
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return NewAPIError("network", 0, err.Error(), 0)
}

// upstreamErrorBody 读取错误响应体并截断。
func upstreamErrorBody(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return strings.TrimSpace(string(body))
}

// retryAfterFrom 解析 Retry-After 响应头（秒数或 HTTP 日期）。
func retryAfterFrom(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := parseRetrySeconds(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// sseEvent 是一条 SSE 事件（event 字段 + data 负载）。
type sseEvent struct {
	Event string
	Data  []byte
}

// scanSSE 逐事件读取 SSE 流。上游连接按 sseIdleTimeout 卡死判定。
func scanSSE(ctx context.Context, resp *http.Response, handle func(sseEvent) error) error {
	idle := time.NewTimer(sseIdleTimeout)
	defer idle.Stop()
	events := make(chan sseEvent, 8)
	errCh := make(chan error, 1)

	go func() {
		defer close(events)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
		var event string
		var data bytes.Buffer
		flush := func() {
			if data.Len() == 0 && event == "" {
				return
			}
			payload := make([]byte, data.Len())
			copy(payload, data.Bytes())
			events <- sseEvent{Event: event, Data: payload}
			event = ""
			data.Reset()
		}
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				flush()
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		flush()
		if err := scanner.Err(); err != nil {
			errCh <- err
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case err := <-errCh:
			return NewAPIError("network", 0, err.Error(), 0)
		case <-idle.C:
			return NewAPIError("timeout", 0, "SSE 流空闲超时", 0)
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(sseIdleTimeout)
			if err := handle(ev); err != nil {
				return err
			}
		}
	}
}

// decodeJSONBody 把错误体按常见错误 envelope 解出人类可读消息。
func decodeJSONBody(body string) string {
	var raw any
	if json.Unmarshal([]byte(body), &raw) != nil {
		return body
	}
	return compactJSONError(raw)
}

func compactJSONError(raw any) string {
	switch v := raw.(type) {
	case map[string]any:
		// OpenAI 风格: {"error": {"message": ..., "code": ...}}
		if inner, ok := v["error"].(map[string]any); ok {
			msg, _ := inner["message"].(string)
			code, _ := inner["code"].(string)
			if code != "" {
				return fmt.Sprintf("%s [%s]", msg, code)
			}
			return msg
		}
		if msg, ok := v["message"].(string); ok {
			return msg
		}
		if detail, ok := v["detail"].(string); ok {
			return detail
		}
		b, _ := json.Marshal(v)
		return string(b)
	default:
		b, _ := json.Marshal(raw)
		return string(b)
	}
}
