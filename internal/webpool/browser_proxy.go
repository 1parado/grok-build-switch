package webpool

// browser_proxy.go — 浏览器代理模式：用 chromedp 驱动真实 Chrome 打开
// grok.com 页面，在页面上下文执行 fetch() 调用 conversations/new。
//
// 这是唯一能同时通过 CF 全部四道防线的方案：
//   ① TLS 指纹（浏览器就是 Chrome）
//   ② h2 帧指纹（浏览器自己的 HTTP 栈）
//   ③ cf_clearance（浏览器自动解挑战）
//   ④ x-statsig-id（从页面 meta 签名，headers 与页面一致）
//
// 一个浏览器实例 + 一个 grok.com 页面，串行处理请求（互斥锁）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// BrowserProxy 管理浏览器代理会话。
type BrowserProxy struct {
	mu sync.Mutex

	browserPath string
	proxyURL    string
	// loginEmail / loginPassword 由调用方传入（注册机账本），
	// 浏览器启动时自动登录 accounts.x.ai 拿新鲜 SSO。
	loginEmail    string
	loginPassword string

	// statsigSignerRef 由 Manager 注入：页面就绪后计算 x-statsig-id。
	statsigSignerRef func(ctx context.Context) (string, error)

	allocCancel context.CancelFunc
	browserCtx  context.Context
	pageCancel  context.CancelFunc
	cancel      context.CancelFunc

	// pageURL 当前页面地址（grok.com 或 about:blank）。
	pageURL string
	// lastUsed 用于空闲回收。
	lastUsed time.Time
}

// NewBrowserProxy 构造（不启动浏览器，首次请求时按需启动）。
func NewBrowserProxy(browserPath, proxyURL string) *BrowserProxy {
	return &BrowserProxy{
		browserPath: browserPath,
		proxyURL:    proxyURL,
		lastUsed:    time.Now(),
	}
}

// SetStatsigSigner 注入 statsig 签名函数（页面就绪后调用）。
func (bp *BrowserProxy) SetStatsigSigner(fn func(ctx context.Context) (string, error)) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	bp.statsigSignerRef = fn
}

// SetLoginCredentials 设置登录凭据（邮箱 + 密码）。
func (bp *BrowserProxy) SetLoginCredentials(email, password string) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	bp.loginEmail = strings.TrimSpace(email)
	bp.loginPassword = strings.TrimSpace(password)
}

// UpdateProxy 更新代理（下次重启浏览器时生效）。
func (bp *BrowserProxy) UpdateProxy(proxyURL string) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	bp.proxyURL = proxyURL
}

// Close 关闭浏览器。
func (bp *BrowserProxy) Close() {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	bp.closeLocked()
}

func (bp *BrowserProxy) closeLocked() {
	if bp.cancel != nil {
		bp.cancel()
		bp.cancel = nil
	}
	if bp.pageCancel != nil {
		bp.pageCancel()
		bp.pageCancel = nil
	}
	if bp.allocCancel != nil {
		bp.allocCancel()
		bp.allocCancel = nil
	}
	bp.browserCtx = nil
	bp.pageURL = ""
}

// ensurePage 确保 grok.com 页面就绪（首次请求时启动浏览器）。
// 流程：无 cookie 加载 grok.com → CF 自动过（Turnstile 扩展）→
// statsig meta 出现后设置 SSO cookie（不刷新，fetch 自动携带）。
// 调用方必须持有 bp.mu。
func (bp *BrowserProxy) ensurePage(parent context.Context, cookies *CookieSet) error {
	// 已有页面：检查是否还在 grok.com（非挑战页）。
	if bp.browserCtx != nil && bp.browserCtx.Err() == nil {
		if strings.Contains(bp.pageURL, "grok.com") {
			var pageTitle string
			if err := chromedp.Run(bp.browserCtx, chromedp.Title(&pageTitle)); err == nil &&
				pageTitle != "" && !strings.Contains(pageTitle, "请稍候") && !strings.Contains(pageTitle, "Just a moment") {
				// 页面活着：确保 SSO cookie 已设置。
				bp.setSSOCookies(bp.browserCtx, cookies)
				bp.lastUsed = time.Now()
				return nil
			}
		}
		bp.closeLocked()
	}

	browserPath := bp.browserPath
	if browserPath == "" {
		browserPath = findChromeBinary()
	}
	if browserPath == "" {
		return fmt.Errorf("未找到 Chrome 浏览器（请在注册机设置中配置浏览器路径）")
	}

	profile, err := os.MkdirTemp("", "grok-switch-webproxy-*")
	if err != nil {
		return err
	}

	// Turnstile 扩展：反自动化检测 + 自动点 CF 验证码。
	extensionDir, extErr := materializeTurnstilePatch(profile)
	if extErr != nil {
		_ = os.RemoveAll(profile)
		return fmt.Errorf("创建 Turnstile 扩展失败: %w", extErr)
	}

	// 手动启动 Chrome（与注册机相同：不带 --enable-automation，
	// CF 能检测 chromedp 默认注入的自动化标志）。
	port, portErr := freeTCPPortForBrowser()
	if portErr != nil {
		_ = os.RemoveAll(profile)
		return portErr
	}
	chromeArgs := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir=" + profile,
		"--disable-gpu",
		"--disable-software-rasterizer",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--disable-images",
		"--mute-audio",
		"--disable-background-networking",
		"--no-first-run",
		"--no-default-browser-check",
		"--hide-crash-restore-bubble",
		"--disable-infobars",
		"--disable-suggestions-ui",
		"--disable-features=PrivacySandboxSettings4",
		"--disable-popup-blocking",
		"--load-extension=" + extensionDir,
	}
	if bp.proxyURL != "" {
		chromeArgs = append(chromeArgs, "--proxy-server="+bp.proxyURL)
	}
	chromeCmd := exec.CommandContext(parent, browserPath, chromeArgs...)
	if startErr := chromeCmd.Start(); startErr != nil {
		_ = os.RemoveAll(profile)
		return fmt.Errorf("启动 Chrome 失败: %w", startErr)
	}
	// Chrome 启动需要 1-2 秒。
	select {
	case <-parent.Done():
		_ = chromeCmd.Process.Kill()
		_ = os.RemoveAll(profile)
		return parent.Err()
	case <-time.After(2 * time.Second):
	}

	// 通过 CDP 远程连接（不用 ExecAllocator，避免注入自动化标志）。
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(parent,
		fmt.Sprintf("http://127.0.0.1:%d", port))
	browserCtx, cancel := chromedp.NewContext(allocCtx)

	// 登录 accounts.x.ai 拿新鲜 SSO（有凭据时），然后跳 grok.com。
	if bp.loginEmail != "" && bp.loginPassword != "" {
		if loginErr := bp.browserLogin(browserCtx); loginErr != nil {
			fmt.Fprintf(os.Stderr, "[webpool] 浏览器登录失败（%v），回退 cookie 模式\n", loginErr)
		}
	}

	// 打开 grok.com（SSO 已由登录设置，或由调用方 cookie 补充）。
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate("https://grok.com/"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		allocCancel()
		cancel()
		_ = os.RemoveAll(profile)
		return fmt.Errorf("打开 grok.com 失败: %w", err)
	}

	// 等待 CF 挑战通过（title 从"请稍候…"变为实际页面名 = 已过盾）。
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var pageURL, pageTitle string
		_ = chromedp.Run(browserCtx, chromedp.Location(&pageURL))
		_ = chromedp.Run(browserCtx, chromedp.Title(&pageTitle))
		// "请稍候…" 是 CF 挑战页的 title；变为其它 = 挑战已通过。
		if strings.Contains(pageURL, "grok.com") && pageTitle != "" &&
			!strings.Contains(pageTitle, "请稍候") && !strings.Contains(pageTitle, "Just a moment") {
			// CF 已过：设置 SSO cookie 后刷新页面（登录态的 chat
			// 界面才会加载 statsig SDK）。
			bp.setSSOCookies(browserCtx, cookies)
			if err := chromedp.Run(browserCtx,
				chromedp.Reload(),
				chromedp.WaitReady("body", chromedp.ByQuery),
			); err != nil {
				// 刷新失败不阻塞：fetch 仍可用当前页面发。
				fmt.Fprintf(os.Stderr, "[webpool-debug] 刷新失败: %v\n", err)
			}
			// 验证 cookie 是否真的写进去了。
			var cookieCheck string
			_ = chromedp.Run(browserCtx, chromedp.Evaluate(
				`document.cookie`, &cookieCheck))
			fmt.Fprintf(os.Stderr, "[webpool-debug] cookies after set: %s\n", cookieCheck[:min(len(cookieCheck), 200)])
			bp.allocCancel = allocCancel
			bp.browserCtx = browserCtx
			bp.cancel = cancel
			bp.pageURL = pageURL
			bp.lastUsed = time.Now()
			return nil
		}
		select {
		case <-browserCtx.Done():
			allocCancel()
			cancel()
			_ = os.RemoveAll(profile)
			return fmt.Errorf("等待 grok.com 页面加载超时")
		case <-time.After(2 * time.Second):
		}
	}

	allocCancel()
	cancel()
	_ = os.RemoveAll(profile)
	return fmt.Errorf("grok.com CF 挑战未在 90 秒内通过")
}

// browserLogin 自动登录 accounts.x.ai：填邮箱 → 填密码 → 等 SSO。
func (bp *BrowserProxy) browserLogin(ctx context.Context) error {
	// 1. 打开登录页。
	if err := chromedp.Run(ctx,
		chromedp.Navigate("https://accounts.x.ai/login?redirect=grok-com"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("打开登录页: %w", err)
	}

	// 2. 等 CF 放行（title 不再是"请稍候…"）。
	loginDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(loginDeadline) {
		var title string
		_ = chromedp.Run(ctx, chromedp.Title(&title))
		if title != "" && !strings.Contains(title, "请稍候") && !strings.Contains(title, "Just a moment") {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	// 3. 等邮箱输入框出现。
	emailDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(emailDeadline) {
		var ready bool
		_ = chromedp.Run(ctx, chromedp.Evaluate(
			`!!document.querySelector('input[type="email"], input[name="email"]')`, &ready))
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}

	// 4. 填邮箱并提交。
	emailJSON, _ := json.Marshal(bp.loginEmail)
	if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => {
  const input = document.querySelector('input[type="email"], input[name="email"]');
  if (!input) return 'no-input';
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
  setter.call(input, %s);
  input.dispatchEvent(new Event('input', {bubbles: true}));
  input.dispatchEvent(new Event('change', {bubbles: true}));
  // 找提交按钮。
  const btns = [...document.querySelectorAll('button')];
  const submit = btns.find(b => ['继续','下一步','continue','next','submit'].includes((b.innerText||'').trim().toLowerCase()));
  if (submit && !submit.disabled) { submit.click(); return 'submitted'; }
  // 或按 Enter。
  input.dispatchEvent(new KeyboardEvent('keydown', {key: 'Enter', code: 'Enter', keyCode: 13, bubbles: true}));
  return 'enter';
})()`, string(emailJSON)), nil)); err != nil {
		return fmt.Errorf("填邮箱: %w", err)
	}

	// 5. 等密码框出现（60 秒）。
	passDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(passDeadline) {
		var ready bool
		_ = chromedp.Run(ctx, chromedp.Evaluate(
			`!!document.querySelector('input[type="password"]')`, &ready))
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}

	// 6. 填密码并提交（合成 click 会被忽略，需要真实鼠标事件）。
	passJSON, _ := json.Marshal(bp.loginPassword)
	if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => {
  const input = document.querySelector('input[type="password"]');
  if (!input) return 'no-input';
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
  setter.call(input, %s);
  input.dispatchEvent(new Event('input', {bubbles: true}));
  input.dispatchEvent(new Event('change', {bubbles: true}));
  return 'filled';
})()`, string(passJSON)), nil)); err != nil {
		return fmt.Errorf("填密码: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	// 真实鼠标点击「登录」按钮（合成 click 被 SPA 忽略）。
	var btnRect []float64
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
  const btns = [...document.querySelectorAll('button')];
  const login = btns.find(b => ['登录','login','log in','signin','sign in'].includes((b.innerText||'').trim().toLowerCase()));
  if (!login) return null;
  const r = login.getBoundingClientRect();
  return [r.x, r.y, r.width, r.height];
})()`, &btnRect))
	if len(btnRect) == 4 {
		_ = chromedp.Run(ctx, chromedp.MouseClickXY(btnRect[0]+btnRect[2]/2, btnRect[1]+btnRect[3]/2))
	}

	// 7. 等 SSO cookie 出现（登录成功的标志）。
	ssoDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(ssoDeadline) {
		var hasSSO bool
		_ = chromedp.Run(ctx, chromedp.Evaluate(
			`document.cookie.includes('sso=')`, &hasSSO))
		if hasSSO {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	return fmt.Errorf("登录超时（SSO cookie 未出现）")
}

// setSSOCookies 在页面上下文设置 SSO cookie（后续 fetch 自动携带）。
func (bp *BrowserProxy) setSSOCookies(ctx context.Context, cookies *CookieSet) {
	if cookies == nil {
		return
	}
	_ = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, c := range []struct{ name, value string }{
			{"sso", cookies.SSO},
			{"sso-rw", cookies.SSORW},
		} {
			if c.value == "" {
				continue
			}
			_ = network.SetCookie(c.name, c.value).
				WithDomain(".grok.com").WithPath("/").Do(ctx)
		}
		return nil
	}))
}

// Chat 通过浏览器页面 UI 自动化执行一次聊天：
// 输入到 tiptap 编辑器 → 点击发送按钮 → 页面自己处理 statsig/CF →
// 从网络事件拦截流式回复（isThinking → ThinkDelta）。
// 这是绕过 CF 四道防线 + statsig 签名的最终方案（页面就是客户端）。
func (bp *BrowserProxy) Chat(parent context.Context, cookies *CookieSet, opts ChatOptions, _ string, onEvent func(WebStreamEvent) error) (string, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if err := bp.ensurePage(parent, cookies); err != nil {
		return "", err
	}

	ctx := bp.browserCtx
	if ctx == nil {
		return "", fmt.Errorf("浏览器会话不可用")
	}

	// ===== 网络拦截：捕获聊天请求的流式响应体 =====
	responseCh := make(chan string, 256) // 流式 chunk 通道
	doneCh := make(chan error, 1)
	var streamMu sync.Mutex
	streamActive := false

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			if strings.Contains(e.Request.URL, "conversations/new") && e.Request.Method == "POST" {
				streamMu.Lock()
				streamActive = true
				streamMu.Unlock()
			}
		case *network.EventDataReceived:
			streamMu.Lock()
			active := streamActive
			streamMu.Unlock()
			if active {
				// 网络事件只通知有数据，实际内容从页面 DOM 读
			}
		}
	})
	_ = responseCh
	_ = doneCh

	// ===== 1. TOS gate 处理（每次都检查，SPA overlay 可能重新出现） =====
	var pageURL string
	_ = chromedp.Run(ctx, chromedp.Location(&pageURL))
	// 等 SPA 渲染稳定
	time.Sleep(3 * time.Second)
	_ = chromedp.Run(ctx, chromedp.Location(&pageURL))

	// TOS gate：JS click「知道了」
	var tosHandled bool
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const btns = [...document.querySelectorAll('button')];
		const target = btns.find(b => {
			const t = (b.innerText || '').trim();
			return t === '知道了' || t === 'Got it' || t === 'I understand';
		});
		if (target) { target.click(); return true; }
		return false;
	})()`, &tosHandled))
	if tosHandled {
		time.Sleep(5 * time.Second) // 等跳转
	}

	// ===== 2. 关闭 Cookie 横幅（可能挡住编辑器） =====
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const btns = [...document.querySelectorAll('button')];
		for (const b of btns) {
			const t = (b.innerText || '').trim();
			if (t === '全部拒绝' || t === 'Reject all') { b.click(); return true; }
		}
		return false;
	})()`, nil))
	time.Sleep(500 * time.Millisecond)

	// ===== 3. 在 tiptap 编辑器输入消息（execCommand insertText） =====
	msgJSON, _ := json.Marshal(opts.Message)
	var fillResult string
	if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => {
		const editor = document.querySelector('.tiptap');
		if (!editor) return 'no-editor';
		editor.focus();
		// 清空已有内容
		const selection = window.getSelection();
		const range = document.createRange();
		range.selectNodeContents(editor);
		selection.removeAllRanges();
		selection.addRange(range);
		// 插入消息（触发 ProseMirror input）
		document.execCommand('insertText', false, %s);
		return 'filled: ' + editor.innerText.slice(0, 30);
	})()`, string(msgJSON)), &fillResult)); err != nil {
		return "", fmt.Errorf("输入消息失败: %w", err)
	}
	if !strings.HasPrefix(fillResult, "filled") {
		return "", fmt.Errorf("编辑器不可用: %s", fillResult)
	}
	time.Sleep(800 * time.Millisecond) // 让 React 处理输入

	// ===== 4. 点击发送按钮（编辑器容器内右下的按钮） =====
	var sendBtnRect []float64
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const editor = document.querySelector('.tiptap');
		if (!editor) return null;
		const er = editor.getBoundingClientRect();
		// 找编辑器右侧/下方的非禁用按钮（发送按钮通常带 SVG 箭头）
		const btns = [...document.querySelectorAll('button')].filter(b => {
			if (b.disabled) return false;
			const r = b.getBoundingClientRect();
			if (r.width < 10 || r.height < 10) return false;
			// 在编辑器附近（右侧或下方 60px 内）
			const nearHorizontally = r.x >= er.x && r.x < er.x + er.width + 100;
			const nearVertically = r.y >= er.y - 20 && r.y < er.y + er.height + 80;
			return nearHorizontally && nearVertically;
		});
		if (btns.length === 0) return null;
		const btn = btns[btns.length - 1];
		const r = btn.getBoundingClientRect();
		return [r.x, r.y, r.width, r.height];
	})()`, &sendBtnRect))

	if len(sendBtnRect) != 4 {
		return "", fmt.Errorf("未找到发送按钮")
	}
	if err := chromedp.Run(ctx, chromedp.MouseClickXY(sendBtnRect[0]+sendBtnRect[2]/2, sendBtnRect[1]+sendBtnRect[3]/2)); err != nil {
		return "", fmt.Errorf("点击发送失败: %w", err)
	}

	// ===== 5. 轮询页面 DOM 读取流式回复 =====
	// grok.com 的聊天界面会渲染 markdown 消息（含思考块）。
	// 思考内容在 DOM 中有专门的样式类。
	var text strings.Builder
	var lastReadText string
	var lastReadThink string
	deadline := time.Now().Add(3 * time.Minute)

	for time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)

		// 从 DOM 读思考 + 正文（分别选取）
		var currentText string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
			// 思考块
			const thinkSelectors = ['[class*="thinking"]', '[class*="reasoning"]', '[class*="thought"]'];
			let think = '';
			for (const sel of thinkSelectors) {
				document.querySelectorAll(sel).forEach(el => {
					const t = (el.innerText || '').trim();
					if (t.length > think.length && t.length < 5000) think = t;
				});
			}
			// 正文：聊天历史里的最后一条 assistant 消息
			// 关键：排除编辑器（用户输入框）——它不在消息列表容器里
			const editor = document.querySelector('.tiptap');
			const editorText = editor ? (editor.innerText || '').trim() : '';
			const containerSelectors = ['[class*="message-list"]', '[class*="conversation"]', 
			                           '[class*="chat-history"]', 'main', '[role="log"]'];
			let text = '';
			for (const sel of containerSelectors) {
				const container = document.querySelector(sel);
				if (!container) continue;
				// 容器内的 markdown 块（不含编辑器）
				const msgs = container.querySelectorAll('.markdown, [class*="message-content"]');
				for (let i = msgs.length - 1; i >= 0; i--) {
					const t = (msgs[i].innerText || '').trim();
					// 排除：空、等于编辑器内容（用户刚输入的）、过短
					if (t.length > 0 && t !== editorText && t.length > 1) {
						text = t;
						break;
					}
				}
				if (text) break;
			}
			return JSON.stringify({think: think, text: text});
		})()`, &currentText))
		// currentText 是 JSON：{think, text}
		var parsed struct {
			Think string `json:"think"`
			Text  string `json:"text"`
		}
		if json.Unmarshal([]byte(currentText), &parsed) == nil {
			// 思考增量
			if len(parsed.Think) > len(lastReadThink) {
				delta := parsed.Think[len(lastReadThink):]
				if delta != "" {
					if err := onEvent(WebStreamEvent{ThinkDelta: delta}); err != nil {
						return text.String(), err
					}
					lastReadThink = parsed.Think
				}
			}
			// 正文增量
			if len(parsed.Text) > len(lastReadText) {
				delta := parsed.Text[len(lastReadText):]
				if delta != "" {
					if err := onEvent(WebStreamEvent{TextDelta: delta}); err != nil {
						return text.String(), err
					}
					text.WriteString(delta)
					lastReadText = parsed.Text
				}
			}
		}

		// 完成检测：思考块已折叠 + 正文不再增长（连续 5 秒无变化）
		// 简化版：等正文稳定 5 秒后结束
		if lastReadText != "" {
			time.Sleep(5 * time.Second)
			var verifyText string
			_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
				const editor = document.querySelector('.tiptap');
				const editorText = editor ? (editor.innerText || '').trim() : '';
				const containers = ['[class*="message-list"]', '[class*="conversation"]', 'main'];
				for (const sel of containers) {
					const c = document.querySelector(sel);
					if (!c) continue;
					const msgs = c.querySelectorAll('.markdown, [class*="message-content"]');
					for (let i = msgs.length - 1; i >= 0; i--) {
						const t = (msgs[i].innerText || '').trim();
						if (t && t !== editorText) return t;
					}
				}
				return '';
			})()`, &verifyText))
			if verifyText == lastReadText {
				break // 正文稳定，完成
			}
			// 继续读增量
			if len(verifyText) > len(lastReadText) {
				delta := verifyText[len(lastReadText):]
				_ = onEvent(WebStreamEvent{TextDelta: delta})
				text.WriteString(delta)
				lastReadText = verifyText
			}
		}

		select {
		case <-parent.Done():
			return text.String(), parent.Err()
		default:
		}
	}

	return text.String(), nil
}

// evalPageJSON 在页面上下文执行表达式并返回 JSON 字符串。
func evalPageJSON(ctx context.Context, expr string) (string, error) {
	var result string
	err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result))
	return result, err
}

// parseWebChunk 解析一段流式文本中的 JSON 事件。
func parseWebChunk(chunk string) ([]WebStreamEvent, error) {
	var events []WebStreamEvent
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var envelope struct {
			Result struct {
				Response struct {
					Token      json.RawMessage `json:"token"`
					IsThinking bool            `json:"isThinking"`
				} `json:"response"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(line), &envelope) != nil {
			continue
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			var errStr string
			if json.Unmarshal(envelope.Error, &errStr) == nil && errStr != "" {
				return events, fmt.Errorf("网页流错误: %s", errStr)
			}
		}
		resp := envelope.Result.Response
		if len(resp.Token) == 0 {
			continue
		}
		var text string
		if json.Unmarshal(resp.Token, &text) != nil {
			continue // 结构化 token（webSearch 等）
		}
		if text == "" {
			continue
		}
		if resp.IsThinking {
			events = append(events, WebStreamEvent{ThinkDelta: text})
		} else {
			events = append(events, WebStreamEvent{TextDelta: text})
		}
	}
	return events, nil
}

// findChromeBinary 查找本机 Chrome。
func findChromeBinary() string {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium-browser",
		"/usr/bin/chromium",
		"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// IsAlive 报告浏览器是否可用。
func (bp *BrowserProxy) IsAlive() bool {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.browserCtx != nil && bp.browserCtx.Err() == nil && strings.Contains(bp.pageURL, "grok.com")
}

// turnstilePatchManifest / turnstilePatchJS 与注册机共用同一套
// Turnstile 补丁（反自动化检测 + 自动点验证码）。
const turnstilePatchManifest = `{
    "manifest_version": 2,
    "name": "Turnstile Patch",
    "version": "1.0.0",
    "description": "Patch browser automation detection for Turnstile",
    "content_scripts": [
        {
            "matches": ["<all_urls>"],
            "js": ["content.js"],
            "run_at": "document_start",
            "all_frames": true
        }
    ],
    "permissions": ["activeTab"]
}`

const turnstilePatchJS = `// Turnstile Patch
(function () {
    "use strict";
    try {
        Object.defineProperty(navigator, "webdriver", {
            get: function () { return false; },
            configurable: true,
        });
    } catch (e) {}
    try {
        if (window.chrome && window.chrome.runtime) {
            delete window.chrome.runtime.onConnect;
            delete window.chrome.runtime.onMessage;
        }
    } catch (e) {}
    try {
        var origQuery = navigator.permissions.query.bind(navigator.permissions);
        navigator.permissions.query = function (params) {
            if (params.name === "notifications") {
                return Promise.resolve({ state: Notification.permission });
            }
            return origQuery(params);
        };
    } catch (e) {}
    try {
        Object.defineProperty(navigator, "plugins", {
            get: function () { return [1, 2, 3, 4, 5]; },
            configurable: true,
        });
    } catch (e) {}
    try {
        Object.defineProperty(navigator, "languages", {
            get: function () { return ["en-US", "en"]; },
            configurable: true,
        });
    } catch (e) {}
    if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", autoClickTurnstile);
    } else {
        autoClickTurnstile();
    }
    function autoClickTurnstile() {
        var checkCount = 0;
        var maxChecks = 100;
        var timer = setInterval(function () {
            checkCount++;
            if (checkCount > maxChecks) {
                clearInterval(timer);
                return;
            }
            try {
                var iframes = document.querySelectorAll(
                    'iframe[src*="challenges.cloudflare.com"], iframe[src*="turnstile"]'
                );
                for (var i = 0; i < iframes.length; i++) {
                    var iframe = iframes[i];
                    try {
                        var body = iframe.contentDocument || iframe.contentWindow.document;
                        var checkbox = body.querySelector(
                            'input[type="checkbox"], .mark, #cf-chl-widget-nomu1_resp'
                        );
                        if (checkbox && !checkbox.checked) {
                            checkbox.click();
                        }
                    } catch (e) {
                        try {
                            iframe.contentWindow.postMessage({ type: "turnstile-auto-click" }, "*");
                        } catch (e2) {}
                    }
                }
                if (window.turnstile && typeof window.turnstile.getResponse === "function") {
                    var resp = window.turnstile.getResponse();
                    if (resp && resp.length > 0) {
                        clearInterval(timer);
                    }
                }
            } catch (e) {}
        }, 500);
    }
})();`

// materializeTurnstilePatch 在 profile 目录内写入扩展文件。
func materializeTurnstilePatch(profile string) (string, error) {
	dir := filepath.Join(profile, "turnstilePatch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	files := map[string]string{
		"manifest.json": turnstilePatchManifest,
		"content.js":    turnstilePatchJS,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// freeTCPPortForBrowser 找一个可用 TCP 端口。
func freeTCPPortForBrowser() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

var _ = http.MethodPost
