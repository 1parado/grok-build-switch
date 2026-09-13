package webpool

import (
	"bufio"
	"bytes"
	"context"
	crypto_rand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Web 通道客户端：调 grok.com /rest/app-chat/conversations/new 并解析流。
// 协议参考 grok2api 系实现（xLmiler/grok2api_python、chenyme/grok2api）。

const webChatPath = "/rest/app-chat/conversations/new"

// 与 grok2api 一致的对上游请求头。cf_clearance 绑定注册时的浏览器 UA，
// 因此这里固定 Chrome/133 一套（注册机浏览器同为 Chrome）。
var webChatHeaders = map[string]string{
	"User-Agent":         "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
	"Sec-Ch-Ua":          `"Not(A:Brand";v="99", "Google Chrome";v="133", "Chromium";v="133"`,
	"Sec-Ch-Ua-Platform": `"macOS"`,
	"Sec-Fetch-Dest":     "empty",
	"Sec-Fetch-Mode":     "cors",
	"Sec-Fetch-Site":     "same-origin",
	"Accept":             "*/*",
	"Accept-Language":    "zh-CN,zh;q=0.9",
	"Content-Type":       "text/plain;charset=UTF-8",
	"Origin":             "https://grok.com",
	"Referer":            "https://grok.com/",
	"Baggage":            "sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c",
	"x-statsig-id":       "ZTpUeXBlRXJyb3I6W1sidHlwZUVycm9yIl0sIm1lc3NhZ2UiLCJjYW5ub3QgcmVhZCBwcm9wZXJ0aWVzIG9mIHVuZGVmaW5lZCAocmVhZGluZyAnYWN0dWlvbicsIHRoaXMgaXMgYW4gaW50ZXJuYWwgZXJyb3IpIl0=",
}

// ChatOptions 是一次网页对话请求。
type ChatOptions struct {
	Model       string
	Message     string
	IsReasoning bool
}

// WebStreamEvent 是网页流解析出的事件增量。
type WebStreamEvent struct {
	ThinkDelta string // isThinking=true 的 token
	TextDelta  string // 正文 token
	SearchTool bool   // token 是 webSearch 动作（结构化，不外发）
}

// chatOutcome 把上游状态码映射为账号观测结果。
func chatOutcome(status int) (Outcome, error) {
	switch {
	case status == http.StatusForbidden:
		return OutcomeBlocked, fmt.Errorf("上游 403：Cloudflare/出口 IP 被屏蔽")
	case status == http.StatusTooManyRequests:
		return OutcomeRateLimited, fmt.Errorf("上游 429：该账号限流")
	case status == http.StatusUnauthorized:
		return OutcomeBlocked, fmt.Errorf("上游 401：SSO cookie 已失效")
	case status >= 400:
		return OutcomeFailure, fmt.Errorf("上游 HTTP %d", status)
	default:
		return OutcomeSuccess, nil
	}
}

// webChatRequest 构建 conversations/new 请求体（与 grok2api 对齐）。
type webChatRequest struct {
	Temporary             bool           `json:"temporary"`
	ModelName             string         `json:"modelName"`
	Message               string         `json:"message"`
	FileAttachments       []any          `json:"fileAttachments"`
	ImageAttachments      []any          `json:"imageAttachments"`
	DisableSearch         bool           `json:"disableSearch"`
	EnableImageGeneration bool           `json:"enableImageGeneration"`
	ReturnImageBytes      bool           `json:"returnImageBytes"`
	ReturnRawGrokInXaiReq bool           `json:"returnRawGrokInXaiRequest"`
	EnableImageStreaming  bool           `json:"enableImageStreaming"`
	ImageGenerationCount  int            `json:"imageGenerationCount"`
	ForceConcise          bool           `json:"forceConcise"`
	ToolOverrides         map[string]any `json:"toolOverrides"`
	EnableSideBySide      bool           `json:"enableSideBySide"`
	SendFinalMetadata     bool           `json:"sendFinalMetadata"`
	CustomPersonality     string         `json:"customPersonality"`
	DeepsearchPreset      string         `json:"deepsearchPreset"`
	IsReasoning           bool           `json:"isReasoning"`
	DisableTextFollowUps  bool           `json:"disableTextFollowUps"`
}

func buildWebChatRequest(opts ChatOptions) webChatRequest {
	return webChatRequest{
		Temporary:             true, // 无状态：多轮由调用方扁平化
		ModelName:             resolvedModel(opts.Model),
		Message:               opts.Message,
		FileAttachments:       []any{},
		ImageAttachments:      []any{},
		DisableSearch:         false,
		EnableImageGeneration: false,
		ReturnImageBytes:      false,
		ReturnRawGrokInXaiReq: false,
		EnableImageStreaming:  false,
		ImageGenerationCount:  1,
		ForceConcise:          false,
		ToolOverrides: map[string]any{
			"imageGen":     false,
			"webSearch":    false,
			"xSearch":      false,
			"xMediaSearch": false,
			"trendsSearch": false,
			"xPostAnalyze": false,
		},
		EnableSideBySide:     true,
		SendFinalMetadata:    true,
		CustomPersonality:    "",
		DeepsearchPreset:     "",
		IsReasoning:          resolvedReasoning(opts.Model) || opts.IsReasoning,
		DisableTextFollowUps: true,
	}
}

// resolvedModel / resolvedReasoning：opts.Model 允许传对外模型名
// （grok-4-reasoning）或裸网页模型名（grok-4），统一在此解析。
func resolvedModel(name string) string   { return ResolveModel(name).ModelName }
func resolvedReasoning(name string) bool { return ResolveModel(name).IsReasoning }

// chatError 携带账号观测分类的结构化错误。
type chatError struct {
	outcome Outcome
	msg     string
}

func (e *chatError) Error() string { return e.msg }

// chatOnce 用指定账号发一次请求并流式回调；返回累计文本与错误。
func (m *Manager) chatOnce(ctx context.Context, cookies *CookieSet, accountID string, opts ChatOptions, onEvent func(WebStreamEvent) error) (string, error) {
	body, err := json.Marshal(buildWebChatRequest(opts))
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.UpstreamBase()+webChatPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	for k, v := range webChatHeaders {
		req.Header.Set(k, v)
	}
	if shared := strings.TrimSpace(m.SharedClearance()); shared != "" && cookies.CFClearance == "" {
		withClearance := *cookies
		withClearance.CFClearance = shared
		cookies = &withClearance
	}
	req.Header.Set("Cookie", CookieHeader(cookies))
	req.Header.Set("x-xai-request-id", newRequestUUID())
	// x-statsig-id：按 (method, path) 签名，签名失败不阻塞（部分上游
	// 不强制校验；被拒时再失效重签）。
	if m.statsig != nil {
		if id, signErr := m.statsig.Sign(ctx, func() (string, error) {
			return m.fetchStatsigMeta(ctx, CookieHeader(cookies))
		}, http.MethodPost, webChatPath); signErr == nil {
			req.Header.Set("x-statsig-id", id)
		}
	}

	started := time.Now()
	resp, err := m.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 请求途中的 Set-Cookie（cf_bm 滚动）回写账号。
	if updates := collectSetCookies(resp); len(updates) > 0 {
		m.RefreshCookies(accountID, updates)
	}

	if resp.StatusCode != http.StatusOK {
		outcome, upErr := chatOutcome(resp.StatusCode)
		m.Observe(accountID, outcome, upErr.Error(), time.Since(started).Milliseconds())
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", &chatError{outcome: outcome, msg: fmt.Sprintf("%v：HTTP %d %s", upErr, resp.StatusCode, strings.TrimSpace(string(snippet)))}
	}

	var text strings.Builder
	err = parseWebStream(resp.Body, func(ev WebStreamEvent) error {
		if ev.TextDelta != "" {
			text.WriteString(ev.TextDelta)
		}
		if ev.ThinkDelta != "" || ev.TextDelta != "" {
			return onEvent(ev)
		}
		return nil
	})
	if err != nil {
		m.Observe(accountID, OutcomeFailure, err.Error(), time.Since(started).Milliseconds())
		return text.String(), err
	}
	m.Observe(accountID, OutcomeSuccess, "", time.Since(started).Milliseconds())
	return text.String(), nil
}

// Chat 执行一次网页对话：轮换选号，429/403/401/5xx 换号重试（最多 3 个账号）。
// 直接 HTTP 全部失败后，回退浏览器代理模式（真 Chrome 过 CF）。
func (m *Manager) Chat(ctx context.Context, opts ChatOptions, onEvent func(WebStreamEvent) error) (string, error) {
	excluded := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cookies, accountID, err := m.ExcludeNext(excluded)
		if err != nil {
			if lastErr != nil {
				break // 走浏览器回退
			}
			return "", err
		}
		text, chatErr := m.chatOnce(ctx, cookies, accountID, opts, onEvent)
		if chatErr == nil {
			return text, nil
		}
		lastErr = chatErr
		// 上下文取消直接终止；其它错误换下一个账号（stream 内错误多为限流）。
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var typed *chatError
		if errors.As(chatErr, &typed) {
			switch typed.outcome {
			case OutcomeRateLimited, OutcomeBlocked:
				excluded[accountID] = true
				continue
			}
		} else if strings.Contains(chatErr.Error(), "网页流错误") {
			// 流内 error（多为 RateLimitError）：该号大概率已到额度，换号。
			continue
		}
		return text, chatErr
	}

	// 浏览器代理回退：CF 四道防线由真 Chrome 处理。
	if m.browserProxy != nil {
		text, err := m.browserChat(ctx, opts, onEvent)
		if err == nil {
			return text, nil
		}
		// 浏览器代理的错误比直接 HTTP 的 403 更有诊断价值。
		fmt.Fprintf(os.Stderr, "grok_switch: webpool 浏览器代理失败: %v\n", err)
		lastErr = fmt.Errorf("浏览器代理: %v | 直接 HTTP: %v", err, lastErr)
	}
	return "", lastErr
}

// anyAccountCookies 返回任意账号的 cookie（不检查可用性；浏览器代理
// 自己过 CF，不受 blocked/rate_limited 状态影响）。
func (m *Manager) anyAccountCookies() *CookieSet {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, acc := range m.state.Accounts {
		if file, err := m.loadAccountFile(acc.ID); err == nil && file.Cookies.SSO != "" {
			cookies := file.Cookies
			return &cookies
		}
	}
	return nil
}

// browserChat 通过浏览器代理执行一次聊天（自动管理 statsig 签名）。
// 浏览器代理不依赖账号的 CF 状态（浏览器自己过 CF），任何账号的
// cookie 都可用——直接 HTTP 的 403 标记的 blocked 账号在此不受影响。
func (m *Manager) browserChat(ctx context.Context, opts ChatOptions, onEvent func(WebStreamEvent) error) (string, error) {
	cookies := m.anyAccountCookies()
	if cookies == nil {
		return "", fmt.Errorf("号池没有账号凭据")
	}

	// UI 自动化方案：页面自己处理 statsig/CF，无需预计算签名。
	return m.browserProxy.Chat(ctx, cookies, opts, "", onEvent)
}

// newRequestUUID 生成浏览器风格请求 id。
func newRequestUUID() string {
	value := make([]byte, 16)
	if _, err := crypto_rand.Read(value); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

// collectSetCookies 从响应头提取需要回写的滚动 cookie。
func collectSetCookies(resp *http.Response) map[string]string {
	keep := map[string]bool{"__cf_bm": true, "cf_clearance": true, "__cuid": true, "xai_anon_id": true}
	out := map[string]string{}
	for _, line := range resp.Header.Values("Set-Cookie") {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if idx := strings.Index(value, ";"); idx >= 0 {
			value = value[:idx]
		}
		if keep[strings.TrimSpace(name)] && value != "" {
			out[strings.TrimSpace(name)] = value
		}
	}
	return out
}

// webStreamError：流内顶层 error 字段（多为限流）。
type webStreamError struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// parseWebStream 解析 grok.com 网页流：逐 JSON 对象（花括号深度法，
// 兼容字符串内换行与半行分块），提取 result.response 增量。
func parseWebStream(r io.Reader, onEvent func(WebStreamEvent) error) error {
	reader := bufio.NewReaderSize(r, 32*1024)
	var buf []byte
	depth := 0
	inString := false
	escaped := false

	emit := func(obj []byte) error {
		var envelope struct {
			Result struct {
				Response struct {
					Token      json.RawMessage `json:"token"`
					IsThinking bool            `json:"isThinking"`
					MessageTag string          `json:"messageTag"`
				} `json:"response"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(obj, &envelope) != nil {
			return nil // 心跳/非 JSON 对象
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			// error 字段可能是纯字符串（"RateLimitError"）或对象。
			var errMsg string
			if json.Unmarshal(envelope.Error, &errMsg) == nil && errMsg != "" {
				return fmt.Errorf("网页流错误: %s", errMsg)
			}
			var streamErr webStreamError
			if json.Unmarshal(envelope.Error, &streamErr) == nil && streamErr.Error != "" {
				return fmt.Errorf("网页流错误: %s", streamErr.Error)
			}
		}
		resp := envelope.Result.Response
		if len(resp.Token) == 0 {
			return nil
		}
		// token 可能是纯字符串，也可能是 {"action": "...", ...} 的结构体（搜索动作）。
		var text string
		if json.Unmarshal(resp.Token, &text) != nil {
			return nil // 结构化 token（webSearch 等），不外发
		}
		if text == "" {
			return nil
		}
		if resp.IsThinking {
			return onEvent(WebStreamEvent{ThinkDelta: text})
		}
		return onEvent(WebStreamEvent{TextDelta: text})
	}

	for {
		chunk := make([]byte, 32*1024)
		n, readErr := reader.Read(chunk)
		for i := 0; i < n; i++ {
			c := chunk[i]
			buf = append(buf, c)
			if escaped {
				escaped = false
				continue
			}
			switch {
			case inString && c == '\\':
				escaped = true
			case c == '"':
				inString = !inString
			case !inString && c == '{':
				depth++
			case !inString && c == '}':
				depth--
				if depth == 0 && len(buf) > 0 {
					if err := emit(buf); err != nil {
						return err
					}
					buf = buf[:0]
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
		if n == 0 {
			continue
		}
	}
	// 流自然结束：没有显式终止符（与 grok2api 观察一致）。
	return nil
}
