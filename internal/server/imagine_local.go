package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"grok_switch/internal/netproxy"
)

// imagine_local.go —— 基于 grok.com 网页端 /imagine WebSocket 的内部生图引擎。
//
// 与 existing /imagine/v1（外部生图 API 反代）不同，这里直接用 grok_switch 自己
// 管理的账号（registrar/cookies 里的浏览器 Cookie）走 grok.com 网页端协议生图，
// 并在多账号之间自动轮询。所有路径都基于 s.Paths.DataDir（= <home>/.grok_switch），
// 因此任何人运行都会自动使用自己的账号目录，无需硬编码。

const (
	imagineOutputSubdir  = "imagine_outputs" // 本地磁盘目录名
	imagineOutputURLPath = "imagine-output"  // HTTP 路由前缀（与 server.go 保持一致）
)

// imagineCookie 对应浏览器导出的单条 Cookie。
type imagineCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

type imagineCookieFile struct {
	Cookies []imagineCookie `json:"cookies"`
}

// imagineAccount 是一个本地生图账号（来自 registrar/cookies 下的一个 JSON 文件）。
type imagineAccount struct {
	id           string
	file         string
	cookies      map[string]string // name -> value（仅 grok.com/x.ai 相关）
	mu           sync.Mutex
	busy         bool      // 正在执行一次生图（并行任务跳过，避免触发上游并发限制）
	exhausted    bool      // 额度耗尽；exhaustedAt 超过 TTL 后自动恢复重试
	exhaustedAt  time.Time // 零值表示非耗尽状态
	successCount int
	failCount    int
	lastError    string
	lastUsed     int64
}

// imagineExhaustedTTL 之后自动重新尝试已耗尽的账号（额度通常按日/按小时恢复）。
const imagineExhaustedTTL = time.Hour

// ImagineEngine 管理账号池并负责单次生图。
type ImagineEngine struct {
	dataDir    string
	outputsDir string
	accounts   []*imagineAccount
	index      int
	mu         sync.Mutex
	proxyURL   string
	transport  http.RoundTripper
}

// NewImagineEngine 构造引擎。dataDir 通常为 s.Paths.DataDir。
func NewImagineEngine(dataDir string) *ImagineEngine {
	eng := &ImagineEngine{
		dataDir:    dataDir,
		outputsDir: filepath.Join(dataDir, imagineOutputSubdir),
		proxyURL:   resolveImagineProxy(),
	}
	if eng.proxyURL != "" {
		if _, tr, err := netproxy.BuildTransport(eng.proxyURL); err == nil {
			eng.transport = tr
		}
	}
	_ = os.MkdirAll(eng.outputsDir, 0o755)
	eng.loadAccounts()
	return eng
}

// resolveImagineProxy 决定 WebSocket/刷新请求走哪个代理。
// 优先级：IMAGINE_PROXY > HTTPS_PROXY > HTTP_PROXY > 默认 Clash 127.0.0.1:7897。
func resolveImagineProxy() string {
	for _, env := range []string{"IMAGINE_PROXY", "HTTPS_PROXY", "HTTP_PROXY"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return "http://127.0.0.1:7897/"
}

// AccountCount 返回可用账号数。
func (e *ImagineEngine) AccountCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.accounts)
}

// ReloadAccounts 重新扫描 cookie 目录：新注册的账号无需重启即可参与生图。
// 已知账号的成功/失败统计与耗尽状态会被保留。
func (e *ImagineEngine) ReloadAccounts() {
	fresh := &ImagineEngine{dataDir: e.dataDir, outputsDir: e.outputsDir}
	fresh.loadAccounts()
	if len(fresh.accounts) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	known := make(map[string]*imagineAccount, len(e.accounts))
	for _, acc := range e.accounts {
		known[acc.id] = acc
	}
	merged := make([]*imagineAccount, 0, len(fresh.accounts))
	for _, acc := range fresh.accounts {
		if old, ok := known[acc.id]; ok {
			merged = append(merged, old)
			continue
		}
		merged = append(merged, acc)
	}
	e.accounts = merged
}

// loadAccounts 扫描 <dataDir>/registrar/cookies/*.json。
func (e *ImagineEngine) loadAccounts() {
	cookieDir := filepath.Join(e.dataDir, "registrar", "cookies")
	entries, err := os.ReadDir(cookieDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[imagine] cookie dir not found: %v\n", err)
		return
	}
	keep := map[string]bool{
		"sso": true, "sso-rw": true, "cf_clearance": true,
		"__cf_bm": true, "__cuid": true, "xai_anon_id": true,
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(cookieDir, ent.Name()))
		if err != nil {
			continue
		}
		var cf imagineCookieFile
		if err := json.Unmarshal(raw, &cf); err != nil {
			continue
		}
		cookies := map[string]string{}
		for _, c := range cf.Cookies {
			if !keep[c.Name] {
				continue
			}
			dom := strings.ToLower(c.Domain)
			if strings.Contains(dom, "grok.com") || strings.Contains(dom, "x.ai") {
				cookies[c.Name] = c.Value
			}
		}
		if len(cookies) == 0 {
			continue
		}
		e.accounts = append(e.accounts, &imagineAccount{
			id:      strings.TrimSuffix(ent.Name(), ".json"),
			file:    ent.Name(),
			cookies: cookies,
		})
	}
	fmt.Fprintf(os.Stderr, "[imagine] loaded %d accounts from %s\n", len(e.accounts), cookieDir)
}

// nextLocked 返回一个可立即使用的账号并推进游标；找不到返回 nil。
// 跳过 busy（正在生图）的账号；exhausted 账号超过 TTL 后自动恢复。
// 调用方必须持有 e.mu。
func (e *ImagineEngine) nextLocked() *imagineAccount {
	if len(e.accounts) == 0 {
		return nil
	}
	now := time.Now()
	start := e.index
	for {
		acc := e.accounts[e.index]
		e.index = (e.index + 1) % len(e.accounts)
		eligible := false
		acc.mu.Lock()
		if acc.exhausted && !acc.exhaustedAt.IsZero() && now.Sub(acc.exhaustedAt) > imagineExhaustedTTL {
			acc.exhausted = false
			acc.exhaustedAt = time.Time{}
		}
		if !acc.exhausted && !acc.busy {
			eligible = true
		}
		acc.mu.Unlock()
		if eligible {
			return acc
		}
		if e.index == start {
			return nil
		}
	}
}

// waitForAccount 等待一个空闲账号：批量生图的并发数可能超过账号数，
// 多出的任务在此排队，而不是误报「全部耗尽」。若所有账号都因额度耗尽
// （且没有任务占用）则立即返回 nil。
func (e *ImagineEngine) waitForAccount(ctx context.Context) *imagineAccount {
	for {
		e.mu.Lock()
		acc := e.nextLocked()
		e.mu.Unlock()
		if acc != nil {
			return acc
		}
		e.mu.Lock()
		idleUnavailable := true
		for _, a := range e.accounts {
			a.mu.Lock()
			if a.busy && !a.exhausted {
				idleUnavailable = false // 还有任务在跑，等它释放
			}
			a.mu.Unlock()
		}
		e.mu.Unlock()
		if idleUnavailable {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func (e *ImagineEngine) markResult(acc *imagineAccount, ok bool, code, msg string) {
	acc.mu.Lock()
	acc.lastUsed = time.Now().Unix()
	if ok {
		acc.successCount++
		acc.lastError = ""
	} else {
		acc.failCount++
		acc.lastError = code + ": " + msg
		switch code {
		case "usage_pool_exhausted", "usage_limit_reached", "concurrency_limit":
			acc.exhausted = true
			acc.exhaustedAt = time.Now()
			fmt.Fprintf(os.Stderr, "[imagine] account %s marked exhausted: %s\n", acc.id, code)
		}
	}
	acc.mu.Unlock()
}

func (acc *imagineAccount) cookieHeader() string {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	parts := make([]string, 0, len(acc.cookies))
	for k, v := range acc.cookies {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

// refreshCookies 通过 GET https://grok.com/imagine 刷新 Cloudflare/SSO Cookie。
func (e *ImagineEngine) refreshCookies(acc *imagineAccount) {
	client := &http.Client{Timeout: 15 * time.Second}
	if e.transport != nil {
		client.Transport = e.transport
	}
	req, err := http.NewRequest(http.MethodGet, "https://grok.com/imagine", nil)
	if err != nil {
		return
	}
	req.Header.Set("Cookie", acc.cookieHeader())
	req.Header.Set("Origin", "https://grok.com")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	for _, sc := range resp.Header["Set-Cookie"] {
		name := strings.Split(sc, "=")[0]
		name = strings.TrimSpace(name)
		switch name {
		case "cf_clearance", "__cf_bm", "sso", "sso-rw":
			val := strings.SplitN(sc, ";", 2)[0]
			val = strings.TrimPrefix(val, name+"=")
			if val != "" {
				acc.mu.Lock()
				acc.cookies[name] = val
				acc.mu.Unlock()
			}
		}
	}
}

// ImagineResult 是一次生图的聚合结果。
type ImagineResult struct {
	OK        bool     `json:"ok"`
	Images    []string `json:"images"` // 保存后的文件 URL
	ModelName string   `json:"model_name"`
	Width     int      `json:"width"`
	Height    int      `json:"height"`
	Account   string   `json:"account"`
	ErrCode   string   `json:"err_code,omitempty"`
	ErrMsg    string   `json:"err_msg,omitempty"`
	SavedTo   string   `json:"saved_to,omitempty"` // 额外复制到下载目录的路径
}

// Generate 使用账号轮询生成一张图片。prompt 为提示词，model 为
// grok-imagine-image/quality，aspectRatio 形如 "1:1"/"16:9"/"9:16"/"4:3"。
// 可安全并发调用：并发数超过账号数时，多出的调用在 waitForAccount 排队，
// 不会把同一账号同时打到上游（避免触发并发限制误标耗尽）。
func (e *ImagineEngine) Generate(ctx context.Context, prompt, model, aspectRatio string) ImagineResult {
	e.mu.Lock()
	total := len(e.accounts)
	e.mu.Unlock()
	if total == 0 {
		return ImagineResult{OK: false, ErrCode: "no_accounts", ErrMsg: "未找到任何可用账号（registrar/cookies 为空或无效）"}
	}
	for attempt := 0; attempt < total; attempt++ {
		if err := ctx.Err(); err != nil {
			return ImagineResult{OK: false, ErrCode: "canceled", ErrMsg: "生图已取消或超时"}
		}
		acc := e.waitForAccount(ctx)
		if acc == nil {
			return ImagineResult{OK: false, ErrCode: "all_exhausted", ErrMsg: "所有账号均已耗尽额度"}
		}
		acc.mu.Lock()
		acc.busy = true
		acc.mu.Unlock()
		res := e.generateOne(ctx, acc, prompt, model, aspectRatio)
		e.markResult(acc, res.OK, res.ErrCode, res.ErrMsg)
		acc.mu.Lock()
		acc.busy = false
		acc.mu.Unlock()
		if res.OK {
			return res
		}
		fmt.Fprintf(os.Stderr, "[imagine] %s failed: %s %s\n", acc.id, res.ErrCode, res.ErrMsg)
		// 额度类错误已标记 exhausted，继续轮询；其它错误也重试下一个账号。
	}
	return ImagineResult{OK: false, ErrCode: "all_failed", ErrMsg: "所有账号均生图失败"}
}

// imagineGenInner 是单次生图的内部结果（图片仍是 base64）。
type imagineGenInner struct {
	OK        bool
	Blobs     []imagineBlob
	ModelName string
	Width     int
	Height    int
	ErrCode   string
	ErrMsg    string
}

type imagineBlob struct {
	Data    string
	ImageID string
	Order   int
}

func (e *ImagineEngine) generateOne(ctx context.Context, acc *imagineAccount, prompt, model, aspectRatio string) ImagineResult {
	e.refreshCookies(acc)
	inner := e.wsGenerate(ctx, acc, prompt, model, aspectRatio)
	if !inner.OK {
		return ImagineResult{OK: false, ErrCode: inner.ErrCode, ErrMsg: inner.ErrMsg, Account: acc.id}
	}
	// 保存图片到 outputsDir，过滤掉过小的缩略图，只保留最大的一张。
	saved := []string{}
	type dec struct {
		b   imagineBlob
		buf []byte
	}
	var decoded []dec
	for _, b := range inner.Blobs {
		buf, err := decodeBase64Strict(b.Data)
		if err != nil {
			continue
		}
		decoded = append(decoded, dec{b: b, buf: buf})
	}
	if len(decoded) == 0 {
		return ImagineResult{OK: false, ErrCode: "no_image", ErrMsg: "未收到图片数据", Account: acc.id}
	}
	// 保留体积最大的一张（num_generations=1 时即最终图，其余为缩略图）。
	keep := decoded[0]
	for _, d := range decoded[1:] {
		if len(d.buf) > len(keep.buf) {
			keep = d
		}
	}
	ext := "jpg"
	if strings.HasPrefix(keep.b.Data, "iVBOR") {
		ext = "png"
	}
	ts := time.Now().UnixNano()
	filename := fmt.Sprintf("%d_%s_0.%s", ts, sanitizeAccountID(acc.id), ext)
	outPath := filepath.Join(e.outputsDir, filename)
	if err := os.WriteFile(outPath, keep.buf, 0o644); err != nil {
		return ImagineResult{OK: false, ErrCode: "write_error", ErrMsg: err.Error(), Account: acc.id}
	}
	writeImagineSidecar(outPath, prompt, model, aspectRatio, inner, acc.id)
	saved = append(saved, "/"+imagineOutputURLPath+"/"+filename)
	return ImagineResult{
		OK:        true,
		Images:    saved,
		ModelName: inner.ModelName,
		Width:     inner.Width,
		Height:    inner.Height,
		Account:   acc.id,
	}
}

// wsGenerate 通过 WebSocket 完成一次 grok.com /imagine 会话。
func (e *ImagineEngine) wsGenerate(ctx context.Context, acc *imagineAccount, prompt, model, aspectRatio string) imagineGenInner {
	reqID := randomHex(16)
	dialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	header := http.Header{}
	header.Set("Cookie", acc.cookieHeader())
	header.Set("Origin", "https://grok.com")
	header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	header.Set("x-xai-request-id", reqID)
	header.Set("Accept-Language", "en-US,en;q=0.9")

	dialOpts := &websocket.DialOptions{
		HTTPHeader: header,
	}
	if e.transport != nil {
		dialOpts.HTTPClient = &http.Client{Transport: e.transport}
	}

	conn, _, err := websocket.Dial(dialCtx, "wss://grok.com/ws/imagine/listen", dialOpts)
	if err != nil {
		return imagineGenInner{ErrCode: "ws_dial", ErrMsg: err.Error()}
	}
	// 图片 blob 可能很大（数百 KB 的 base64），放宽 coder/websocket 默认 32KB 读上限。
	conn.SetReadLimit(16 << 20)
	defer conn.Close(websocket.StatusNormalClosure, "")

	result := imagineGenInner{}
	var finished, completed bool
	finish := func(r imagineGenInner) {
		if finished {
			return
		}
		finished = true
		result = r
	}

	// 发送顺序：open 后立即发 update_session，350ms 后发 input_text。
	props := imagineProps(model, aspectRatio, false)
	_ = conn.Write(dialCtx, websocket.MessageText, mustJSON(imagineEnvelope{
		Type:      "conversation.item.create",
		Timestamp: time.Now().UnixMilli(),
		Item: imagineItem{
			Type:    "message",
			Content: []imagineContent{{Type: "update_session", Properties: props}},
		},
	}))
	go func() {
		time.Sleep(350 * time.Millisecond)
		inp := imagineProps(model, aspectRatio, true)
		_ = conn.Write(dialCtx, websocket.MessageText, mustJSON(imagineEnvelope{
			Type:      "conversation.item.create",
			Timestamp: time.Now().UnixMilli(),
			Item: imagineItem{
				Type: "message",
				Content: []imagineContent{{
					Type:       "input_text",
					RequestID:  reqID,
					Text:       prompt,
					Properties: inp,
				}},
			},
		}))
	}()

	readCtx, readCancel := context.WithCancel(dialCtx)
	defer readCancel()
	for {
		mt, data, rErr := conn.Read(readCtx)
		if rErr != nil {
			if !finished {
				if completed && len(result.Blobs) > 0 {
					result.OK = true
					finish(result)
				} else if result.ErrCode == "" {
					if completed {
						finish(imagineGenInner{ErrCode: "no_image", ErrMsg: "完成但未收到图片"})
					} else {
						finish(imagineGenInner{ErrCode: "closed", ErrMsg: "连接在读消息前关闭: " + rErr.Error()})
					}
				}
			}
			break
		}
		if mt != websocket.MessageText {
			continue
		}
		var msg imagineInbound
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "session":
			// 服务端会话建立，等待客户端请求。
		case "error":
			// 服务端直接返回错误（如 rate_limit_exhausted / usage_pool_exhausted）。
			if msg.ErrCode != "" {
				finish(imagineGenInner{ErrCode: msg.ErrCode, ErrMsg: msg.ErrMsg})
				break
			}
		case "image":
			if msg.Blob != "" {
				result.Blobs = append(result.Blobs, imagineBlob{Data: msg.Blob, ImageID: msg.ImageID, Order: msg.Order})
			}
		case "json":
			if msg.ModelName != "" {
				result.ModelName = msg.ModelName
			}
			if msg.Width != 0 {
				result.Width = msg.Width
			}
			if msg.Height != 0 {
				result.Height = msg.Height
			}
			if msg.ErrCode != "" {
				finish(imagineGenInner{ErrCode: msg.ErrCode, ErrMsg: orEmpty(msg.ErrMsg, msg.ErrMessage)})
				break
			}
			if msg.CurrentStatus == "completed" {
				completed = true
				if len(result.Blobs) > 0 {
					result.OK = true
					finish(result)
					break
				}
				// 图片 blob 尚未到达：标记 completed，继续读取直到连接关闭再结算。
			}
		}
		if finished {
			break
		}
	}
	return result
}

// imagineProps 构造请求属性（与 grok.com 网页端一致）。
func imagineProps(model, aspectRatio string, isInitial bool) map[string]any {
	quality := model == "grok-imagine-image-quality"
	res := "1k"
	if strings.Contains(model, "quality") {
		res = "1k"
	}
	return map[string]any{
		"is_initial":          isInitial,
		"image_model_name":    model,
		"enable_side_by_side": false,
		"enable_pro":          quality,
		"resolution_name":     res,
		"aspect_ratio":        aspectRatio,
		"num_generations":     1,
	}
}

// ---- 小工具 ----

// ImagineMeta 是随图片一起落盘的元数据 sidecar（<图片文件名>.json），
// 画廊刷新后据此恢复提示词、模型等信息。
type ImagineMeta struct {
	Prompt    string    `json:"prompt"`
	Model     string    `json:"model,omitempty"`
	Aspect    string    `json:"aspect,omitempty"`
	Account   string    `json:"account,omitempty"`
	Width     int       `json:"width,omitempty"`
	Height    int       `json:"height,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func writeImagineSidecar(imagePath, prompt, model, aspect string, inner imagineGenInner, accountID string) {
	modelName := orEmpty(inner.ModelName, model)
	meta := ImagineMeta{
		Prompt:    prompt,
		Model:     modelName,
		Aspect:    aspect,
		Account:   accountID,
		Width:     inner.Width,
		Height:    inner.Height,
		CreatedAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(imagePath+".json", data, 0o644)
}

type imagineEnvelope struct {
	Type      string      `json:"type"`
	Timestamp int64       `json:"timestamp"`
	Item      imagineItem `json:"item"`
}

type imagineItem struct {
	Type    string           `json:"type"`
	Content []imagineContent `json:"content"`
}

type imagineContent struct {
	Type       string         `json:"type,omitempty"`
	RequestID  string         `json:"requestId,omitempty"`
	Text       string         `json:"text,omitempty"`
	Properties map[string]any `json:"properties,omitempty"`
}

type imagineInbound struct {
	Type          string `json:"type"`
	Blob          string `json:"blob"`
	ImageID       string `json:"image_id"`
	Order         int    `json:"order"`
	ModelName     string `json:"model_name"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	ErrCode       string `json:"err_code"`
	ErrMsg        string `json:"err_msg"`
	ErrMessage    string `json:"err_message"`
	CurrentStatus string `json:"current_status"`
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func orEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func sanitizeAccountID(id string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return r.Replace(id)
}

// decodeBase64Strict 解码标准或 URL-safe base64（图片 blob 可能带 data: 前缀）。
func decodeBase64Strict(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, ","); idx >= 0 && strings.HasPrefix(s, "data:") {
		s = s[idx+1:]
	}
	s = strings.TrimRight(s, "=")
	// 长度补齐到 4 的倍数。
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	if buf, err := base64.StdEncoding.DecodeString(s); err == nil {
		return buf, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}
