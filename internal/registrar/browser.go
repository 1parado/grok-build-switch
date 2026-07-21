package registrar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

const signupURL = "https://accounts.x.ai/sign-up?redirect=grok-com"

const emailResendInterval = 35 * time.Second

type browserChallengeError struct{ message string }

func (e *browserChallengeError) Error() string { return e.message }

type browserSession struct {
	ctx     context.Context
	cancel  context.CancelFunc
	cmd     *exec.Cmd
	profile string

	mu                      sync.Mutex
	lastDocumentStatus      int64
	createEmailCodeStatus   int64
	createEmailCodeSeen     bool
	createEmailCodeBodyHint string
	verifyEmailCodeSeen bool
	blockedAPI          string
}

func registerAccount(ctx context.Context, config Config, mailbox Mailbox, authDir string, log func(string)) (registrationOutcome, error) {
	// CreateEmailValidationCode is routinely blocked in headless/automation-heavy
	// sessions. Prefer visible Chrome (matches the working DrissionPage path).
	// "auto" also uses visible first; headless is only used when explicitly selected.
	headless := false
	switch config.BrowserMode {
	case "headless":
		headless = true
	case "auto", "visible", "":
		headless = false
	}
	return registerWithBrowser(ctx, config, mailbox, authDir, headless, log)
}

func isCreateEmailBlocked(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "createemailvalidationcode") || strings.Contains(msg, "验证码接口")
}

func registerWithBrowser(parent context.Context, config Config, mailbox Mailbox, authDir string, headless bool, log func(string)) (registrationOutcome, error) {
	session, err := startBrowser(parent, config, headless)
	if err != nil {
		return registrationOutcome{}, err
	}
	defer session.Close()
	ctx, cancel := context.WithTimeout(session.ctx, time.Duration(config.PageTimeoutSeconds)*time.Second)
	defer cancel()

	log(firstNonEmpty(map[bool]string{true: "启动无窗口浏览器", false: "启动可见浏览器"}[headless]))
	if config.ProxyURL != "" {
		log("浏览器代理: " + chromiumProxyServer(config.ProxyURL))
	}

	if err := chromedp.Run(ctx, chromedp.Navigate(signupURL), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return registrationOutcome{}, fmt.Errorf("打开注册页: %w", err)
	}
	if err := waitForChallengeClear(ctx, session, 45*time.Second, log); err != nil {
		return registrationOutcome{}, err
	}
	if err := assertSignupPageOK(ctx, session); err != nil {
		return registrationOutcome{}, err
	}
	time.Sleep(2 * time.Second)

	if err := clickEmailSignup(ctx, 25*time.Second); err != nil {
		if pageErr := assertSignupPageOK(ctx, session); pageErr != nil {
			return registrationOutcome{}, pageErr
		}
		return registrationOutcome{}, fmt.Errorf("点击邮箱注册: %w", err)
	}
	time.Sleep(800 * time.Millisecond)

	if err := submitEmailWithRetries(ctx, session, mailbox.Address(), log); err != nil {
		return registrationOutcome{}, err
	}
	log("邮箱已提交，等待验证码")

	code, err := waitMailboxCodeWithResend(
		ctx,
		mailbox,
		time.Duration(config.MailTimeoutSeconds)*time.Second,
		emailResendInterval,
		func(message string) {
			if log != nil {
				log(message)
				if apiErr := session.createEmailError(); apiErr != nil {
					log(apiErr.Error())
				}
			}
		},
		func(resendCtx context.Context) (bool, error) {
			session.resetCreateEmailStatus()
			clicked, clickErr := tryClickResend(resendCtx)
			if clickErr != nil || !clicked {
				return clicked, clickErr
			}
			if resultErr := waitCreateEmailResult(resendCtx, session, 20*time.Second); resultErr != nil {
				return true, resultErr
			}
			return true, nil
		},
		log,
	)
	if err != nil {
		if apiErr := session.createEmailError(); apiErr != nil {
			return registrationOutcome{}, apiErr
		}
		if pageErr := assertSignupPageOK(ctx, session); pageErr != nil {
			return registrationOutcome{}, fmt.Errorf("%w；同时注册页异常: %v", err, pageErr)
		}
		return registrationOutcome{}, err
	}
	if err := fillAndSubmitCode(ctx, code); err != nil {
		return registrationOutcome{}, fmt.Errorf("提交验证码: %w", err)
	}
	_ = waitVerifyEmailResult(ctx, session, 10*time.Second)
	log("验证码已提交")

	// Some flows already have SSO after email verify (no profile step).
	if earlySSO, ssoErr := waitForSSOCookie(ctx, 10*time.Second); ssoErr == nil && earlySSO != "" {
		return finalizeRegistration(ctx, session, config, mailbox, earlySSO, "", authDir, log)
	}

	given, family, password, err := randomProfile()
	if err != nil {
		return registrationOutcome{}, err
	}
	if err := fillProfileAndSubmit(ctx, given, family, password); err != nil {
		return registrationOutcome{}, err
	}
	log("注册资料已提交")

	sso, err := waitForSSOCookie(ctx, 120*time.Second)
	if err != nil {
		if headless {
			return registrationOutcome{}, &browserChallengeError{message: err.Error()}
		}
		return registrationOutcome{}, err
	}
	return finalizeRegistration(ctx, session, config, mailbox, sso, password, authDir, log)
}

// finalizeRegistration mints CPA tokens from the SSO cookie and writes the auth
// file. Shared by the skip-profile path (SSO already present after verify) and
// the normal path (SSO obtained after profile submission).
func finalizeRegistration(ctx context.Context, session *browserSession, config Config, mailbox Mailbox, sso, password, authDir string, log func(string)) (registrationOutcome, error) {
	log("已获取 SSO，开始 CPA 铸造")
	tokens, method, err := mintFromSSO(ctx, session, sso, config.ProxyURL, config.PreferProtocolMint, config.ProtocolOnly, log)
	if err != nil {
		return registrationOutcome{}, err
	}
	authPath, err := writeCPAAuth(authDir, mailbox.Address(), tokens)
	if err != nil {
		return registrationOutcome{}, err
	}
	return registrationOutcome{
		Email: mailbox.Address(), Password: password, SSO: sso,
		MintMethod: method, AuthFile: authPath,
	}, nil
}

type mailboxCodeResult struct {
	code string
	err  error
}

func waitMailboxCodeWithResend(
	ctx context.Context,
	mailbox Mailbox,
	timeout time.Duration,
	resendInterval time.Duration,
	mailLog func(string),
	resend func(context.Context) (bool, error),
	log func(string),
) (string, error) {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	result := make(chan mailboxCodeResult, 1)
	go func() {
		code, err := mailbox.WaitCode(waitCtx, timeout, mailLog)
		result <- mailboxCodeResult{code: code, err: err}
	}()

	if resend == nil || resendInterval <= 0 {
		select {
		case value := <-result:
			return value.code, value.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	timer := time.NewTimer(resendInterval)
	defer timer.Stop()
	for {
		select {
		case value := <-result:
			return value.code, value.err
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			clicked, err := resend(waitCtx)
			if log != nil {
				switch {
				case err != nil:
					log("触发重新发送验证码失败，继续等待: " + err.Error())
				case clicked:
					log("已触发重新发送验证码")
				}
			}
			timer.Reset(resendInterval)
		}
	}
}

func submitEmailWithRetries(ctx context.Context, session *browserSession, email string, log func(string)) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		session.resetCreateEmailStatus()
		if err := fillAndSubmitEmail(ctx, email); err != nil {
			lastErr = err
			continue
		}
		// Wait for the signup RPC that actually triggers the mail.
		if err := waitCreateEmailResult(ctx, session, 20*time.Second); err != nil {
			lastErr = err
			if attempt < 3 {
				if isCreateEmailBlocked(err) {
					log(fmt.Sprintf("CreateEmailValidationCode 第 %d 次被拦截，等待后重试", attempt))
				} else {
					log(fmt.Sprintf("第 %d 次邮箱提交未生效，重新填写并提交", attempt))
				}
				// Give Turnstile / CF cookies a chance to settle, then resubmit.
				_ = waitForChallengeClear(ctx, session, 20*time.Second, log)
				time.Sleep(time.Duration(attempt) * 1500 * time.Millisecond)
				// Prefer a resend click if the UI already advanced; otherwise retype.
				if clicked, _ := tryClickResend(ctx); clicked {
					log("已点击重新发送验证码")
					if err2 := waitCreateEmailResult(ctx, session, 20*time.Second); err2 == nil {
						return nil
					} else {
						lastErr = err2
					}
				}
				continue
			}
			return err
		}
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("提交邮箱失败")
}

func startBrowser(parent context.Context, config Config, headless bool) (*browserSession, error) {
	browserPath := config.BrowserPath
	if browserPath == "" {
		browserPath = findBrowser()
	}
	if browserPath == "" {
		return nil, fmt.Errorf("未找到 Chrome / Edge，请在注册设置中填写浏览器路径")
	}
	profile, err := os.MkdirTemp("", "grok-switch-register-*")
	if err != nil {
		return nil, err
	}
	extensionDir, err := materializeTurnstileExtension(profile)
	if err != nil {
		_ = os.RemoveAll(profile)
		return nil, err
	}

	port, err := freeTCPPort()
	if err != nil {
		_ = os.RemoveAll(profile)
		return nil, err
	}

	// Launch Chrome ourselves so chromedp does not inject --enable-automation.
	// Flags match the working Python registrar (CHROMIUM_SLIM_FLAGS in grok_register_ttk.py),
	// including --disable-gpu — that path registers successfully; keep parity.
	args := []string{
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
	}
	if !headless {
		args = append(args,
			"--load-extension="+extensionDir,
			"--disable-extensions-except="+extensionDir,
		)
	} else {
		args = append(args, "--headless=new")
	}
	if proxy := chromiumProxyServer(config.ProxyURL); proxy != "" {
		args = append(args, "--proxy-server="+proxy)
	}

	cmd := exec.Command(browserPath, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(profile)
		return nil, fmt.Errorf("启动浏览器进程: %w", err)
	}

	wsURL, err := waitDebuggerURL(port, 20*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = os.RemoveAll(profile)
		return nil, fmt.Errorf("等待浏览器调试端口: %w", err)
	}

	allocator, cancelAllocator := chromedp.NewRemoteAllocator(parent, wsURL)
	browserCtx, cancelBrowser := chromedp.NewContext(allocator)
	session := &browserSession{cmd: cmd, profile: profile}
	cancel := func() {
		cancelBrowser()
		cancelAllocator()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		_ = os.RemoveAll(profile)
	}
	session.cancel = cancel
	session.ctx = browserCtx

	if err := chromedp.Run(browserCtx,
		network.Enable(),
		// x.ai signup page bundles rely on eval(), which its own script-src CSP
		// blocks (e.g. 0wh5dysf.51j~.js), so the "confirm email" button silently
		// does nothing. Bypass CSP for this tab so their handlers run normally.
		page.Enable(),
		page.SetBypassCSP(true),
		chromedp.ActionFunc(func(ctx context.Context) error {
			chromedp.ListenTarget(ctx, func(ev interface{}) {
				switch e := ev.(type) {
				case *network.EventResponseReceived:
					status := e.Response.Status
					u := e.Response.URL
					if e.Type == network.ResourceTypeDocument {
						session.mu.Lock()
						session.lastDocumentStatus = status
						session.mu.Unlock()
					}
					if strings.Contains(u, "CreateEmailValidationCode") {
						session.mu.Lock()
						session.createEmailCodeStatus = status
						session.createEmailCodeSeen = true
						if status >= 400 {
							session.blockedAPI = "CreateEmailValidationCode"
							session.createEmailCodeBodyHint = fmt.Sprintf("HTTP %d", status)
						}
						session.mu.Unlock()
					}
					if strings.Contains(u, "VerifyEmailValidationCode") {
						session.mu.Lock()
						session.verifyEmailCodeSeen = true
						session.mu.Unlock()
					}
					if status == 403 && (strings.Contains(u, "auth_mgmt") || strings.Contains(u, "accounts.x.ai") || strings.Contains(u, "auth.x.ai")) {
						session.mu.Lock()
						if session.blockedAPI == "" {
							session.blockedAPI = u
						}
						session.mu.Unlock()
					}
				case *network.EventLoadingFailed:
					// ignore; status tracked via responses
				}
			})
			return nil
		}),
	); err != nil {
		cancel()
		return nil, fmt.Errorf("连接浏览器: %w", err)
	}
	return session, nil
}

func freeTCPPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitDebuggerURL(port int, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(150 * time.Millisecond)
			continue
		}
		var payload struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err == nil && strings.TrimSpace(payload.WebSocketDebuggerURL) != "" {
			return payload.WebSocketDebuggerURL, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("调试端点未返回 webSocketDebuggerUrl")
		}
		time.Sleep(150 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("超时")
	}
	return "", lastErr
}

func chromiumProxyServer(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return strings.TrimSpace(raw)
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + parsed.Host
}

func materializeTurnstileExtension(profile string) (string, error) {
	dir := filepath.Join(profile, "turnstilePatch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	files := map[string]string{
		"manifest.json": turnstileManifest,
		"content.js":    turnstileContentJS,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func (s *browserSession) Close() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

func (s *browserSession) resetCreateEmailStatus() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createEmailCodeStatus = 0
	s.createEmailCodeSeen = false
	s.createEmailCodeBodyHint = ""
	if s.blockedAPI == "CreateEmailValidationCode" {
		s.blockedAPI = ""
	}
}

func (s *browserSession) createEmailError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.createEmailCodeSeen {
		return nil
	}
	if s.createEmailCodeStatus == 0 || s.createEmailCodeStatus == 200 {
		return nil
	}
	return &browserChallengeError{message: fmt.Sprintf(
		"验证码接口 CreateEmailValidationCode 返回 HTTP %d（被 Cloudflare/xAI 拦截，邮件不会发出）。请使用可见浏览器，确认代理节点可正常访问 accounts.x.ai，并在页面上完成 Turnstile 后重试",
		s.createEmailCodeStatus,
	)}
}

func waitVerifyEmailResult(ctx context.Context, session *browserSession, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		seen := session.verifyEmailCodeSeen
		session.mu.Unlock()
		if seen {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

func waitCreateEmailResult(ctx context.Context, session *browserSession, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		seen := session.createEmailCodeSeen
		status := session.createEmailCodeStatus
		session.mu.Unlock()
		if seen {
			if status == 200 || status == 0 {
				return nil
			}
			return &browserChallengeError{message: fmt.Sprintf(
				"验证码接口 CreateEmailValidationCode 返回 HTTP %d（邮件不会发送）",
				status,
			)}
		}
		// If UI already advanced to code input, treat as success even if we missed the RPC.
		var advanced bool
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
const visible=n=>n&&n.getBoundingClientRect().width>0&&n.getBoundingClientRect().height>0;
const code=[...document.querySelectorAll('input[data-input-otp="true"],input[name="code"],input[autocomplete="one-time-code"],input[inputmode="numeric"]')].some(n=>visible(n));
const text=(document.body&&document.body.innerText||'').toLowerCase();
return code || text.includes('verification code') || text.includes('验证码') || text.includes('check your email') || text.includes('查看邮箱');
})()`, &advanced))
		if advanced {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("提交邮箱后未观察到 CreateEmailValidationCode 请求，页面也未进入验证码步骤")
}

func waitForChallengeClear(ctx context.Context, session *browserSession, timeout time.Duration, log func(string)) error {
	deadline := time.Now().Add(timeout)
	logged := false
	for time.Now().Before(deadline) {
		var state string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
const text=(document.body&&document.body.innerText||'').toLowerCase();
const title=(document.title||'').toLowerCase();
if(title.includes('just a moment')||text.includes('just a moment')||text.includes('checking your browser')||text.includes('verify you are human'))return 'challenge';
if(title.includes('403')||text.includes('403 forbidden')||text.includes('access denied')||text.includes('sorry, you have been blocked'))return 'blocked';
const email=[...document.querySelectorAll('input[type="email"],input[name="email"],button,a')].some(n=>n&&n.getBoundingClientRect().width>0);
return email?'ready':'wait';
})()`, &state))
		switch state {
		case "ready":
			return nil
		case "blocked":
			return assertSignupPageOK(ctx, session)
		case "challenge":
			if !logged && log != nil {
				log("等待 Cloudflare 人机验证通过（请在可见窗口中完成勾选）")
				logged = true
			}
		}
		if err := assertSignupPageOK(ctx, session); err != nil && !strings.Contains(err.Error(), "Just a moment") {
			// keep waiting on soft challenge states
			if state != "challenge" && state != "wait" {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err := assertSignupPageOK(ctx, session); err != nil {
		return err
	}
	return nil
}

func assertSignupPageOK(ctx context.Context, session *browserSession) error {
	var title, href, bodyText string
	_ = chromedp.Run(ctx,
		chromedp.Location(&href),
		chromedp.Title(&title),
		chromedp.Evaluate(`(() => (document.body && (document.body.innerText||'')).slice(0,500))()`, &bodyText),
	)
	combined := strings.ToLower(title + "\n" + bodyText + "\n" + href)
	session.mu.Lock()
	status := session.lastDocumentStatus
	session.mu.Unlock()
	if status == 403 || strings.Contains(combined, "403 forbidden") || strings.Contains(combined, "access denied") ||
		strings.Contains(combined, "sorry, you have been blocked") || strings.Contains(combined, "attention required") {
		return &browserChallengeError{message: fmt.Sprintf(
			"注册页被拦截 HTTP=%d title=%q url=%s",
			status, title, href,
		)}
	}
	if strings.Contains(combined, "just a moment") || strings.Contains(combined, "checking your browser") {
		return &browserChallengeError{message: "注册页卡在 Cloudflare 人机验证（Just a moment）"}
	}
	return nil
}

func clickEmailSignup(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var state string
		err := chromedp.Run(ctx, chromedp.Evaluate(clickEmailSignupScript, &state))
		if state == "clicked" {
			appeared, waitErr := waitForEmailInput(ctx, 4*time.Second)
			if appeared {
				return nil
			}
			if waitErr != nil {
				return waitErr
			}
		} else if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	return fmt.Errorf("未找到邮箱注册按钮")
}

func waitForEmailInput(ctx context.Context, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var visible bool
		err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
const visible=n=>n&&n.getBoundingClientRect().width>0&&n.getBoundingClientRect().height>0;
return [...document.querySelectorAll('input[data-testid="email"],input[name="email"],input[type="email"],input[autocomplete="email"]')].some(n=>visible(n)&&!n.disabled&&!n.readOnly);
})()`, &visible))
		if err == nil && visible {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return false, nil
}

func fillAndSubmitEmail(ctx context.Context, email string) error {
	deadline := time.Now().Add(35 * time.Second)
	emailJSON, _ := json.Marshal(email)
	for time.Now().Before(deadline) {
		var state string
		err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(fillEmailValueScript, string(emailJSON)), &state))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			state = "not-ready"
		}
		if state != "filled" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
			continue
		}
		time.Sleep(800 * time.Millisecond)
		var submitState string
		if err := chromedp.Run(ctx, chromedp.Evaluate(submitEmailScript, &submitState)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if submitState == "submitted" {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("邮箱输入框未出现")
}

// fillAndSubmitCode mirrors grok_register_ttk.fill_code_and_submit:
// JS native value setter (React-friendly), then confirm click, then 1.5s settle.
func fillAndSubmitCode(ctx context.Context, code string) error {
	clean := strings.ReplaceAll(strings.TrimSpace(code), "-", "")
	if clean == "" {
		return fmt.Errorf("验证码为空")
	}
	codeJSON, _ := json.Marshal(clean)
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		var filled string
		if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(fillCodeOnlyScript, string(codeJSON)), &filled)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			filled = "not-ready"
		}
		switch {
		case filled == "not-ready" || filled == "empty-code":
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			continue
		case strings.Contains(filled, "failed"):
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		var clicked string
		_ = chromedp.Run(ctx, chromedp.Evaluate(confirmEmailClickScript, &clicked))
		// Match Python: treat "clicked" and "no-button" as done (auto-submit UIs exist).
		if clicked == "clicked" || clicked == "no-button" {
			// Python human_sleep(1.5) — let VerifyEmailValidationCode + SPA transition settle.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(1500 * time.Millisecond):
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("验证码已获取，但自动填写/提交失败")
}

// fillProfileAndSubmit mirrors grok_register_ttk.fill_profile_and_submit:
// 2s Turnstile warm-up, JS form fill, wait for CF token, secondary Turnstile retry.
func fillProfileAndSubmit(ctx context.Context, given, family, password string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}

	deadline := time.Now().Add(120 * time.Second)
	formFilledOnce := false
	var waitCFSince time.Time
	var lastCFRetry time.Time
	values, _ := json.Marshal([]string{given, family, password})

	for time.Now().Before(deadline) {
		if !formFilledOnce {
			var filled string
			if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(fillProfileOnlyScript, string(values)), &filled)); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				filled = "not-ready"
			}
			switch {
			case strings.HasPrefix(filled, "wait-cloudflare"):
				formFilledOnce = true
				if waitCFSince.IsZero() {
					waitCFSince = time.Now()
				}
				if time.Since(waitCFSince) >= 12*time.Second && time.Since(lastCFRetry) >= 8*time.Second {
					_, _ = retryTurnstileToken(ctx)
					lastCFRetry = time.Now()
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(800 * time.Millisecond):
				}
				continue
			case filled == "ready-to-submit" || filled == "filled-no-submit":
				formFilledOnce = true
			case filled == "fill-failed", filled == "not-ready":
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
		}

		var submitState string
		if err := chromedp.Run(ctx, chromedp.Evaluate(submitProfileScript, &submitState)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			submitState = "no-submit-button"
		}
		if strings.HasPrefix(submitState, "wait-cloudflare") {
			if waitCFSince.IsZero() {
				waitCFSince = time.Now()
			}
			if time.Since(waitCFSince) >= 12*time.Second && time.Since(lastCFRetry) >= 8*time.Second {
				_, _ = retryTurnstileToken(ctx)
				lastCFRetry = time.Now()
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(800 * time.Millisecond):
			}
			continue
		}
		if submitState == "submitted" {
			return nil
		}
		waitCFSince = time.Time{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return &browserChallengeError{message: "最终注册页资料填写失败（Turnstile 未通过或资料页未提交）"}
}

// retryTurnstileToken mirrors getTurnstileToken + token inject from the Python registrar.
func retryTurnstileToken(ctx context.Context) (int, error) {
	var ignored bool
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
try { if (window.turnstile && typeof turnstile.reset === 'function') turnstile.reset(); } catch (e) {}
return true;
})()`, &ignored))

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var token string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
try {
  const byInput = String((document.querySelector('input[name="cf-turnstile-response"]') || {}).value || '').trim();
  if (byInput) return byInput;
  if (window.turnstile && typeof turnstile.getResponse === 'function') {
    return String(turnstile.getResponse() || '').trim();
  }
  return '';
} catch (e) { return ''; }
})()`, &token))
		token = strings.TrimSpace(token)
		if len(token) >= 80 {
			return injectTurnstileToken(ctx, token)
		}
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
const nodes = Array.from(document.querySelectorAll('div,span,iframe')).filter((n) => {
  const txt = (n.className || '') + ' ' + (n.id || '') + ' ' + (n.getAttribute?.('src') || '');
  return String(txt).toLowerCase().includes('turnstile');
});
if (nodes.length && typeof nodes[0].click === 'function') nodes[0].click();
const iframes = document.querySelectorAll('iframe[src*="challenges.cloudflare.com"], iframe[src*="turnstile"]');
for (const iframe of iframes) {
  try { iframe.contentWindow.postMessage({ type: 'turnstile-auto-click' }, '*'); } catch (e) {}
}
return true;
})()`, &ignored))
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return 0, fmt.Errorf("Turnstile 获取 token 失败")
}

func injectTurnstileToken(ctx context.Context, token string) (int, error) {
	tokenJSON, _ := json.Marshal(token)
	var n int
	err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => {
const token = String(%s || '').trim();
const cfInput = document.querySelector('input[name="cf-turnstile-response"]');
if (!cfInput || !token) return 0;
const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
if (nativeSetter) nativeSetter.call(cfInput, token);
else cfInput.value = token;
cfInput.dispatchEvent(new Event('input', { bubbles: true }));
cfInput.dispatchEvent(new Event('change', { bubbles: true }));
return String(cfInput.value || '').trim().length;
})()`, string(tokenJSON)), &n))
	return n, err
}

func tryClickResend(ctx context.Context) (bool, error) {
	var point struct {
		X  float64 `json:"x"`
		Y  float64 `json:"y"`
		OK bool    `json:"ok"`
	}
	err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
const nodes=[...document.querySelectorAll('button,a,[role="button"]')];
const visible=x=>{if(!x||x.disabled||x.getAttribute('aria-disabled')==='true')return false;const s=getComputedStyle(x),r=x.getBoundingClientRect();return s.display!=='none'&&s.visibility!=='hidden'&&r.width>0&&r.height>0;};
const n=nodes.find(x=>{const t=(x.innerText||x.textContent||'').replace(/\s+/g,'').toLowerCase();return visible(x)&&(t.includes('resend')||t.includes('重新发送')||t.includes('再次发送')||t.includes('重发'));});
if(!n)return {ok:false,x:0,y:0};
const r=n.getBoundingClientRect();
return {ok:true,x:r.left+r.width/2,y:r.top+r.height/2};
})()`, &point))
	if err != nil || !point.OK {
		return false, err
	}
	if err := chromedp.Run(ctx, chromedp.MouseClickXY(point.X, point.Y)); err != nil {
		return false, err
	}
	return true, nil
}

func waitForSSOCookie(ctx context.Context, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var value string
		err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			cookies, err := storage.GetCookies().Do(ctx)
			if err != nil {
				return err
			}
			for _, cookie := range cookies {
				if cookie.Name == "sso" && cookie.Value != "" {
					value = cookie.Value
					break
				}
			}
			return nil
		}))
		if err == nil && value != "" {
			return value, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", fmt.Errorf("等待 SSO cookie 超时")
}

func evalUntil(ctx context.Context, script string, timeout time.Duration, accept func(string) bool) (string, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		if err := chromedp.Run(ctx, chromedp.Evaluate(script, &last)); err == nil && accept(last) {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return last, fmt.Errorf("页面操作超时，最后状态: %s", last)
}

func randomProfile() (string, string, string, error) {
	givens := []string{"Neo", "Ethan", "Liam", "Noah", "Lucas", "Mason", "Ryan", "Leo", "Owen", "Aiden", "Kai", "Evan"}
	families := []string{"Lin", "Wang", "Zhao", "Liu", "Chen", "Zhang", "Xu", "Sun", "Guo", "He", "Yang", "Wu"}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", "", "", err
	}
	password := "N" + hex.EncodeToString(random[:5]) + "!a7#" + hex.EncodeToString(random[5:9])
	return givens[int(random[9])%len(givens)], families[int(random[10])%len(families)], password, nil
}

const chromeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"

const turnstileManifest = `{
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

const turnstileContentJS = `// Turnstile Patch
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
})();
`

const clickEmailSignupScript = `(() => {
const nodes=[...document.querySelectorAll('button,a,[role="button"]')];
const visible=x=>{if(!x||x.disabled||x.getAttribute('aria-disabled')==='true')return false;const s=getComputedStyle(x),r=x.getBoundingClientRect();return s.display!=='none'&&s.visibility!=='hidden'&&r.width>0&&r.height>0;};
const labels=new Set(['使用邮箱注册','邮箱注册','signupwithemail','continuewithemail']);
const n=nodes.find(x=>visible(x)&&labels.has((x.innerText||x.textContent||'').replace(/\s+/g,'').toLowerCase()));
if(!n)return 'not-ready'; n.click(); return 'clicked';
})()`

const fillEmailValueScript = `(() => {
const email=%s;
const visible=node=>{if(!node)return false;const style=getComputedStyle(node);const rect=node.getBoundingClientRect();return style.display!=='none'&&style.visibility!=='hidden'&&style.opacity!=='0'&&rect.width>0&&rect.height>0;};
const input=[...document.querySelectorAll('input[data-testid="email"],input[name="email"],input[type="email"],input[autocomplete="email"]')].find(node=>visible(node)&&!node.disabled&&!node.readOnly);
if(!input)return 'not-ready';
input.focus();input.click();
const setter=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value')?.set;
const tracker=input._valueTracker;
if(tracker)tracker.setValue('');
if(setter)setter.call(input,email);else input.value=email;
input.dispatchEvent(new Event('focus',{bubbles:true}));
input.dispatchEvent(new InputEvent('beforeinput',{bubbles:true,data:email,inputType:'insertText'}));
input.dispatchEvent(new InputEvent('input',{bubbles:true,data:email,inputType:'insertText'}));
input.dispatchEvent(new Event('change',{bubbles:true}));
input.dispatchEvent(new Event('blur',{bubbles:true}));
return (input.value||'').trim()===email?'filled':'value-mismatch';
})()`

const submitEmailScript = `(() => {
const visible=node=>{if(!node)return false;const style=getComputedStyle(node);const rect=node.getBoundingClientRect();return style.display!=='none'&&style.visibility!=='hidden'&&style.opacity!=='0'&&rect.width>0&&rect.height>0;};
const input=[...document.querySelectorAll('input[data-testid="email"],input[name="email"],input[type="email"],input[autocomplete="email"]')].find(node=>visible(node)&&!node.disabled&&!node.readOnly);
if(!input||!input.checkValidity()||!(input.value||'').trim())return 'invalid-email';
const buttons=[...document.querySelectorAll('button[type="submit"],button')].filter(node=>visible(node)&&!node.disabled&&node.getAttribute('aria-disabled')!=='true');
const button=buttons.find(node=>{const text=(node.innerText||node.textContent||'').replace(/\s+/g,'').toLowerCase();return text==='注册'||text.includes('注册')||text.includes('signup')||text.includes('continue')||text.includes('next');});
if(!button)return 'no-submit';
button.click();
return 'submitted';
})()`

// fillCodeOnlyScript — same approach as Python fill_code_and_submit (native setter + events).
// %s is a JSON-encoded code string.
const fillCodeOnlyScript = `(() => {
const code = String(%s || '').trim();
if (!code) return 'empty-code';
function isVisible(node) {
  if (!node) return false;
  const style = window.getComputedStyle(node);
  if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
  const rect = node.getBoundingClientRect();
  return rect.width > 0 && rect.height > 0;
}
function setInputValue(input, value) {
  const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
  const tracker = input._valueTracker;
  if (tracker) tracker.setValue('');
  if (nativeSetter) nativeSetter.call(input, value);
  else input.value = value;
  input.dispatchEvent(new InputEvent('beforeinput', { bubbles: true, data: value, inputType: 'insertText' }));
  input.dispatchEvent(new InputEvent('input', { bubbles: true, data: value, inputType: 'insertText' }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
}
const aggregate = Array.from(document.querySelectorAll(
  'input[data-input-otp="true"], input[name="code"], input[autocomplete="one-time-code"], input[inputmode="numeric"], input[inputmode="text"]'
)).find((node) => isVisible(node) && !node.disabled && !node.readOnly && Number(node.maxLength || 6) > 1);
if (aggregate) {
  aggregate.focus();
  aggregate.click();
  setInputValue(aggregate, code);
  return String(aggregate.value || '').replace(/\s+/g, '') ? 'filled-aggregate' : 'aggregate-failed';
}
const otpBoxes = Array.from(document.querySelectorAll('input')).filter((node) => {
  if (!isVisible(node) || node.disabled || node.readOnly) return false;
  const maxLength = Number(node.maxLength || 0);
  const ac = String(node.autocomplete || '').toLowerCase();
  return maxLength === 1 || ac === 'one-time-code';
});
if (otpBoxes.length >= code.length) {
  for (let i = 0; i < code.length; i += 1) {
    const ch = code[i] || '';
    const box = otpBoxes[i];
    box.focus();
    box.click();
    setInputValue(box, ch);
    box.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, key: ch }));
    box.dispatchEvent(new KeyboardEvent('keyup', { bubbles: true, key: ch }));
  }
  const merged = otpBoxes.slice(0, code.length).map((x) => String(x.value || '').trim()).join('');
  return merged.length ? 'filled-boxes' : 'boxes-failed';
}
return 'not-ready';
})()`

// confirmEmailClickScript — same selectors/labels as Python fill_code_and_submit.
const confirmEmailClickScript = `(() => {
function isVisible(node) {
  if (!node) return false;
  const style = window.getComputedStyle(node);
  if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
  const rect = node.getBoundingClientRect();
  return rect.width > 0 && rect.height > 0;
}
const buttons = Array.from(document.querySelectorAll('button[type="submit"], button')).filter((node) => {
  return isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true';
});
const btn = buttons.find((node) => {
  const t = (node.innerText || node.textContent || '').replace(/\s+/g, '').toLowerCase();
  return (
    t.includes('确认邮箱') ||
    t.includes('继续') ||
    t.includes('下一步') ||
    t.includes('confirm') ||
    t.includes('continue') ||
    t.includes('next')
  );
});
if (!btn) return 'no-button';
btn.focus();
btn.click();
return 'clicked';
})()`

// fillProfileOnlyScript — fill name/password only; do not submit (Python form_filled_once path).
// %s is JSON array [given, family, password].
const fillProfileOnlyScript = `(() => {
const [givenName, familyName, password] = %s;
function isVisible(node) {
  if (!node) return false;
  const style = window.getComputedStyle(node);
  if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
  const rect = node.getBoundingClientRect();
  return rect.width > 0 && rect.height > 0;
}
function pickInput(selector) {
  return Array.from(document.querySelectorAll(selector)).find((node) => {
    return isVisible(node) && !node.disabled && !node.readOnly;
  }) || null;
}
function setInputValue(input, value) {
  if (!input) return false;
  input.focus();
  input.click();
  const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
  const tracker = input._valueTracker;
  if (tracker) tracker.setValue('');
  if (nativeSetter) nativeSetter.call(input, value);
  else input.value = value;
  input.dispatchEvent(new InputEvent('beforeinput', { bubbles: true, data: value, inputType: 'insertText' }));
  input.dispatchEvent(new InputEvent('input', { bubbles: true, data: value, inputType: 'insertText' }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
  input.blur();
  return String(input.value || '').trim() === String(value || '').trim();
}
const givenInput = pickInput('input[data-testid="givenName"], input[name="givenName"], input[autocomplete="given-name"], input[aria-label*="名"]');
const familyInput = pickInput('input[data-testid="familyName"], input[name="familyName"], input[autocomplete="family-name"], input[aria-label*="姓"]');
const passwordInput = pickInput('input[data-testid="password"], input[name="password"], input[type="password"], input[autocomplete="new-password"]');
if (!givenInput || !familyInput || !passwordInput) return 'not-ready';
const ok1 = setInputValue(givenInput, givenName);
const ok2 = setInputValue(familyInput, familyName);
const ok3 = setInputValue(passwordInput, password);
if (!ok1 || !ok2 || !ok3) return 'fill-failed';
const buttons = Array.from(document.querySelectorAll('button[type="submit"], button')).filter((node) => {
  return isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true';
});
const submitBtn = buttons.find((node) => {
  const t = (node.innerText || node.textContent || '').replace(/\s+/g, '').toLowerCase();
  return t.includes('完成注册') || t.includes('创建账户') || t.includes('sign up') || t.includes('createaccount');
});
const cfInput = document.querySelector('input[name="cf-turnstile-response"]');
const cfPresent = !!cfInput
  || !!document.querySelector('iframe[src*="turnstile"], div.cf-turnstile, [data-sitekey], script[src*="turnstile"]');
if (cfPresent) {
  const token = String((cfInput && cfInput.value) || '').trim();
  if (token.length < 80) return 'wait-cloudflare:' + token.length;
}
if (submitBtn) return 'ready-to-submit';
return 'filled-no-submit';
})()`

// submitProfileScript — submit only after Turnstile token is ready (Python second loop).
const submitProfileScript = `(() => {
function isVisible(node) {
  if (!node) return false;
  const style = window.getComputedStyle(node);
  if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
  const rect = node.getBoundingClientRect();
  return rect.width > 0 && rect.height > 0;
}
const cfInput = document.querySelector('input[name="cf-turnstile-response"]');
const cfPresent = !!cfInput
  || !!document.querySelector('iframe[src*="turnstile"], div.cf-turnstile, [data-sitekey], script[src*="turnstile"]');
if (cfPresent) {
  const token = String((cfInput && cfInput.value) || '').trim();
  if (token.length < 80) return 'wait-cloudflare:' + token.length;
}
const buttons = Array.from(document.querySelectorAll('button[type="submit"], button')).filter((node) => {
  return isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true';
});
const submitBtn = buttons.find((node) => {
  const t = (node.innerText || node.textContent || '').replace(/\s+/g, '').toLowerCase();
  return t.includes('完成注册') || t.includes('创建账户') || t.includes('sign up') || t.includes('createaccount');
});
if (!submitBtn) return 'no-submit-button';
submitBtn.focus();
submitBtn.click();
return 'submitted';
})()`
