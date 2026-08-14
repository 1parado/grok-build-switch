package registrar

// relogin.go — 重新登录已有 x.ai/Grok 账号并重新铸造 CPA(OAuth) token。
//
// 背景：registrar 注册流程产生的 sso 会话 cookie 约 24 小时过期，关联的
// OAuth refresh_token 也会被服务端吊销。本文件复用注册管线里验证过的
// 浏览器/CF/Turnstile/设备码铸造能力，用邮箱+密码重新登录，拿到全新
// sso 会话后重新走 device flow 铸造新的 access_token + refresh_token，
// 并把新的 cookie 快照与 CPA 凭据文件写回原目录（与 finalizeRegistration
// 的产出格式完全一致，grok_pool / grokauth 可直接继续使用）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

const loginURL = "https://accounts.x.ai/login?redirect=grok-com"

// ReloginResult 描述单个账号重新登录+铸造的结果。
type ReloginResult struct {
	Email      string `json:"email"`
	Status     string `json:"status"` // success | failed
	Error      string `json:"error,omitempty"`
	MintMethod string `json:"mint_method,omitempty"`
	SSO        string `json:"sso,omitempty"`
	AuthFile   string `json:"auth_file,omitempty"`
	CookieFile string `json:"cookie_file,omitempty"`
}

// ReloginAccount 用 email+password 重新登录 x.ai，等待全新 sso cookie，
// 然后复用 CPA 设备码流程铸造新 OAuth token（access/refresh/id token），
// 并把 cookie 快照写进 cookieDir、CPA 凭据写进 authDir。
func ReloginAccount(parent context.Context, config Config, email, password, authDir, cookieDir string, log func(string)) (ReloginResult, error) {
	res := ReloginResult{Email: email, Status: "failed"}
	rl := func(msg string) {
		if log != nil {
			log("[重新登录] " + msg)
		}
	}
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)
	if email == "" || password == "" {
		res.Error = "邮箱或密码为空"
		return res, fmt.Errorf("邮箱或密码为空")
	}

	headless := browserHeadless(config)
	rl(fmt.Sprintf("启动浏览器（visible=%v）", !headless))
	session, err := startBrowser(parent, config, headless)
	if err != nil {
		res.Error = "启动浏览器失败: " + err.Error()
		return res, err
	}
	defer session.Close()

	ctx, cancel := context.WithTimeout(session.ctx, time.Duration(config.PageTimeoutSeconds)*time.Second)
	defer cancel()
	if config.ProxyURL != "" {
		rl("代理 " + chromiumProxyServer(config.ProxyURL))
	}

	// 1) 打开登录页（重试瞬时网络故障，与注册页一致）。
	if err := navigateLoginWithRetry(ctx, config.ProxyURL, rl); err != nil {
		res.Error = err.Error()
		return res, err
	}

	// 2) 处理 Cloudflare / Turnstile。
	rl("等待 Cloudflare/Turnstile 放行…")
	if err := waitForChallengeClear(ctx, session, 90*time.Second, rl); err != nil {
		snap := capturePageSnapshot(ctx, session)
		res.Error = "登录页被 Cloudflare 拦截: " + err.Error() + " | " + snap.Summary()
		return res, err
	}

	// 3) 确保邮箱输入框出现（必要时点击「使用邮箱登录」）。
	if err := ensureLoginEmailReady(ctx, loginEmailReadyScript, 40*time.Second); err != nil {
		snap := capturePageSnapshot(ctx, session)
		res.Error = err.Error() + " | " + snap.Summary()
		return res, err
	}

	// 4) 填邮箱并提交。
	rl("填写邮箱 " + email)
	if err := fillAndSubmitLoginEmail(ctx, email); err != nil {
		snap := capturePageSnapshot(ctx, session)
		res.Error = "提交邮箱失败: " + err.Error() + " | " + snap.Summary()
		return res, err
	}

	// 5) 等待密码输入框；若邮箱提交后直接拿到 SSO（个别流程），跳过密码步。
	passwordNeeded, err := waitForLoginPasswordOrSSO(ctx, 60*time.Second)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	if passwordNeeded {
		rl("输入密码")
		if err := fillAndSubmitLoginPassword(ctx, password); err != nil {
			snap := capturePageSnapshot(ctx, session)
			res.Error = "提交密码失败: " + err.Error() + " | " + snap.Summary()
			return res, err
		}
	}

	// 6) 等待 SSO cookie。
	rl("等待 SSO 会话…")
	sso, err := waitForSSOCookieWithHints(ctx, 90*time.Second)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	res.SSO = sso
	rl("已获取 SSO")

	// 7) 重新铸造 OAuth token（CPA 设备码流程）。
	mintCtx, cancelMint := mintContext(parent)
	defer cancelMint()
	tokens, method, err := mintFromSSO(mintCtx, session, sso, config.ProxyURL, config.PreferProtocolMint, config.ProtocolOnly, rl)
	if err != nil {
		hint := "请保持登录浏览器可见，确认 device/consent 页的真实「允许」操作"
		if parent != nil && parent.Err() != nil {
			hint = "任务已取消或整体超时"
		} else if mintCtx.Err() == context.DeadlineExceeded {
			hint = "铸造超时：检查授权页是否点到「允许」，以及代理是否稳定"
		}
		res.Error = err.Error() + " | " + hint
		return res, err
	}

	// 8) 写入 CPA 凭据 + cookie 快照（与注册产出同格式）。
	authPath, err := writeCPAAuth(authDir, email, tokens)
	if err != nil {
		res.Error = "写入 CPA 凭据失败: " + err.Error()
		return res, err
	}
	cookiePath, err := saveBrowserCookieSnapshot(session.ctx, cookieDir, email)
	if err != nil {
		res.Error = "写入 cookie 快照失败: " + err.Error()
		return res, err
	}

	res.Status = "success"
	res.MintMethod = method
	res.AuthFile = authPath
	res.CookieFile = cookiePath
	rl("成功 method=" + method + " auth=" + authPath + " cookie=" + cookiePath)
	return res, nil
}

// AccountPassword 从注册机账本（accounts_cli.txt，格式 邮箱----密码----jwt）
// 里按邮箱查找保存的密码；找不到返回 false。供「刷新 cookie」等按账号
// 重新登录的入口使用。
func (s *Service) AccountPassword(email string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	data, _ := os.ReadFile(s.accountsPath)
	lookup := strings.ToLower(strings.TrimSpace(email))
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Split(line, "----")
		if len(parts) >= 2 && strings.ToLower(strings.TrimSpace(parts[0])) == lookup {
			if pw := strings.TrimSpace(parts[1]); pw != "" {
				return pw, true
			}
		}
	}
	return "", false
}

// CookieDir 返回注册机 cookie 快照目录（refresh 后导出的新 cookie 也写这里）。
func (s *Service) CookieDir() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cookieDir
}

// navigateLoginWithRetry 与 navigateSignupWithRetry 同款：打开登录页并重试瞬时网络故障。
func navigateLoginWithRetry(ctx context.Context, proxy string, log func(string)) error {
	const maxAttempts = 3
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		last = chromedp.Run(ctx,
			chromedp.Navigate(loginURL),
			chromedp.WaitReady("body", chromedp.ByQuery),
		)
		if last == nil {
			if attempt > 1 && log != nil {
				log(fmt.Sprintf("第 %d 次打开登录页成功", attempt))
			}
			return nil
		}
		if !isTransientNavError(last) {
			return fmt.Errorf("打开登录页失败: %w", last)
		}
		if attempt < maxAttempts {
			if log != nil {
				log(fmt.Sprintf("打开登录页失败（%s），%d/%d 重试…", last.Error(), attempt, maxAttempts))
			}
			select {
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			case <-ctx.Done():
				return fmt.Errorf("打开登录页失败: %w", ctx.Err())
			}
			continue
		}
	}
	return fmt.Errorf("打开登录页失败: %v", last)
}

// ensureLoginEmailReady：若邮箱输入框可见直接返回；否则尝试点击「使用邮箱登录」。
func ensureLoginEmailReady(ctx context.Context, script string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		if err := chromedp.Run(ctx, chromedp.Evaluate(script, &last)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			last = "eval-error"
		}
		switch last {
		case "ready":
			return nil
		case "clicked":
			appeared, _ := waitForEmailInput(ctx, 5*time.Second)
			if appeared {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	return fmt.Errorf("未找到邮箱输入框（登录页可能改版或仍被拦截，last=%s）", last)
}

func fillAndSubmitLoginEmail(ctx context.Context, email string) error {
	emailJSON, _ := json.Marshal(email)
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(fillLoginEmailScript, string(emailJSON)), &state)); err != nil {
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
		if err := chromedp.Run(ctx, chromedp.Evaluate(submitLoginEmailScript, &submitState)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			submitState = "eval-error"
		}
		if submitState == "clicked" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	return fmt.Errorf("邮箱输入框未出现或提交按钮不可用")
}

// waitForLoginPasswordOrSSO：邮箱提交后，等待密码输入框出现（返回 true），
// 或直接拿到 sso cookie（返回 false，表示无需密码步）。
func waitForLoginPasswordOrSSO(ctx context.Context, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var state string
		if err := chromedp.Run(ctx, chromedp.Evaluate(loginPasswordReadyScript, &state)); err == nil && state == "ready" {
			return true, nil
		}
		var sso string
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			cookies, err := storage.GetCookies().Do(ctx)
			if err != nil {
				return err
			}
			for _, c := range cookies {
				if c.Name == "sso" && c.Value != "" {
					sso = c.Value
					return nil
				}
			}
			return nil
		})); err == nil && sso != "" {
			return false, nil
		}
		var fail string
		if err := chromedp.Run(ctx, chromedp.Evaluate(loginFailureScript, &fail)); err == nil && fail != "" {
			var parsed struct {
				Found []string `json:"found"`
				OTP   bool     `json:"otp"`
			}
			if json.Unmarshal([]byte(fail), &parsed) == nil {
				if parsed.OTP {
					detail := strings.Join(parsed.Found, ", ")
					if detail == "" {
						detail = "检测到验证码输入框"
					}
					return false, fmt.Errorf("登录要求邮箱验证码（OTP），需用临时邮箱收码，本次跳过: %s", detail)
				}
				if len(parsed.Found) > 0 {
					return false, fmt.Errorf("登录被拒: %s", strings.Join(parsed.Found, ", "))
				}
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(600 * time.Millisecond):
		}
	}
	return false, fmt.Errorf("邮箱提交后未出现密码输入框，也未拿到 SSO（可能要求邮箱验证码或账号异常）")
}

func fillAndSubmitLoginPassword(ctx context.Context, password string) error {
	passwordJSON, _ := json.Marshal(password)
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(fillLoginPasswordScript, string(passwordJSON)), &state)); err != nil {
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
		time.Sleep(500 * time.Millisecond)
		var submitState string
		if err := chromedp.Run(ctx, chromedp.Evaluate(submitLoginPasswordScript, &submitState)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			submitState = "eval-error"
		}
		if submitState == "clicked" || submitState == "enter" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	return fmt.Errorf("密码输入框未出现或提交按钮不可用")
}

// waitForSSOCookieWithHints：等待 sso cookie；超时后用失败检测脚本补充原因。
func waitForSSOCookieWithHints(ctx context.Context, timeout time.Duration) (string, error) {
	sso, err := waitForSSOCookie(ctx, timeout)
	if err == nil && sso != "" {
		return sso, nil
	}
	var fail string
	hints := ""
	if fErr := chromedp.Run(ctx, chromedp.Evaluate(loginFailureScript, &fail)); fErr == nil && fail != "" {
		var parsed struct {
			Found []string `json:"found"`
			OTP   bool     `json:"otp"`
		}
		if json.Unmarshal([]byte(fail), &parsed) == nil && len(parsed.Found) > 0 {
			hints = " | 页面检测: " + strings.Join(parsed.Found, ", ")
			if parsed.OTP {
				hints += "（要求邮箱验证码）"
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("等待 SSO cookie 超时：%v%s", err, hints)
	}
	return "", fmt.Errorf("等待 SSO cookie 超时%s", hints)
}

// ---------- 登录页操作脚本（与注册流程同一套 JS 写法） ----------

// loginEmailReadyScript：邮箱输入框可见→ready；否则点「使用邮箱登录」→clicked；都无→missing。
const loginEmailReadyScript = `(() => {
const visible=n=>n&&n.getBoundingClientRect().width>0&&n.getBoundingClientRect().height>0&&!n.disabled&&!n.readOnly;
const inputs=[...document.querySelectorAll('input[data-testid="email"],input[name="email"],input[type="email"],input[autocomplete="email"]')];
if(inputs.some(n=>visible(n))) return 'ready';
const nodes=[...document.querySelectorAll('button,a,[role="button"]')];
const clickable=n=>{if(!n||n.disabled||n.getAttribute('aria-disabled')==='true')return false;const s=getComputedStyle(n),r=n.getBoundingClientRect();return s.display!=='none'&&s.visibility!=='hidden'&&r.width>0&&r.height>0;};
const target=nodes.find(n=>{if(!clickable(n))return false;const t=(n.innerText||n.textContent||'').replace(/\s+/g,'').toLowerCase();return t.includes('使用邮箱')||t.includes('邮箱登录')||t==='email'||t.includes('signinwithemail')||t.includes('continuewithemail');});
if(!target)return 'missing';
target.click();
return 'clicked';
})()`

// fillLoginEmailScript：React 友好的原生 setter 填邮箱。
const fillLoginEmailScript = `(() => {
const value = String(%s || '').trim();
const visible=n=>n&&n.getBoundingClientRect().width>0&&n.getBoundingClientRect().height>0;
const input=[...document.querySelectorAll('input[data-testid="email"],input[name="email"],input[type="email"],input[autocomplete="email"]')].find(n=>visible(n)&&!n.disabled&&!n.readOnly);
if(!input)return 'not-ready';
const setter=Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype,'value')?.set;
if(setter)setter.call(input,value);else input.value=value;
input.dispatchEvent(new Event('input',{bubbles:true}));
input.dispatchEvent(new Event('change',{bubbles:true}));
input.focus();
return String(input.value||'').trim()===value?'filled':'mismatch';
})()`

// submitLoginEmailScript：点提交按钮（[type=submit] 或 继续/下一步 文案）。
const submitLoginEmailScript = `(() => {
const nodes=[...document.querySelectorAll('button,a,[role="button"],input[type="submit"]')];
const clickable=n=>{if(!n||n.disabled||n.getAttribute('aria-disabled')==='true')return false;const s=getComputedStyle(n),r=n.getBoundingClientRect();return s.display!=='none'&&s.visibility!=='hidden'&&r.width>0&&r.height>0;};
let target=nodes.find(n=>n.tagName==='BUTTON'&&n.type==='submit'&&clickable(n));
if(!target)target=nodes.find(n=>{if(!clickable(n))return false;const t=(n.innerText||n.textContent||'').replace(/\s+/g,'').toLowerCase();return t==='继续'||t==='下一步'||t==='continue'||t==='next'||t==='nextstep';});
if(!target)return 'missing';
target.click();
return 'clicked';
})()`

// loginPasswordReadyScript：密码输入框是否可见。
const loginPasswordReadyScript = `(() => {
const visible=n=>n&&n.getBoundingClientRect().width>0&&n.getBoundingClientRect().height>0;
return [...document.querySelectorAll('input[type="password"]')].some(n=>visible(n)&&!n.disabled&&!n.readOnly)?'ready':'waiting';
})()`

// fillLoginPasswordScript：原生 setter 填密码。
const fillLoginPasswordScript = `(() => {
const value = String(%s || '').trim();
const visible=n=>n&&n.getBoundingClientRect().width>0&&n.getBoundingClientRect().height>0;
const input=[...document.querySelectorAll('input[type="password"]')].find(n=>visible(n)&&!n.disabled&&!n.readOnly);
if(!input)return 'not-ready';
const setter=Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype,'value')?.set;
if(setter)setter.call(input,value);else input.value=value;
input.dispatchEvent(new Event('input',{bubbles:true}));
input.dispatchEvent(new Event('change',{bubbles:true}));
input.focus();
return String(input.value||'').trim()===value?'filled':'mismatch';
})()`

// submitLoginPasswordScript：点提交按钮；找不到按钮时对激活输入框派发 Enter。
const submitLoginPasswordScript = `(() => {
const nodes=[...document.querySelectorAll('button,a,[role="button"],input[type="submit"]')];
const clickable=n=>{if(!n||n.disabled||n.getAttribute('aria-disabled')==='true')return false;const s=getComputedStyle(n),r=n.getBoundingClientRect();return s.display!=='none'&&s.visibility!=='hidden'&&r.width>0&&r.height>0;};
let target=nodes.find(n=>n.tagName==='BUTTON'&&n.type==='submit'&&clickable(n));
if(!target)target=nodes.find(n=>{if(!clickable(n))return false;const t=(n.innerText||n.textContent||'').replace(/\s+/g,'').toLowerCase();return t==='登录'||t==='继续'||t==='signin'||t==='login'||t==='log in'||t==='登录到grok';});
if(!target){
  const inp=document.activeElement;
  if(inp&&(inp.tagName==='INPUT'||inp.tagName==='TEXTAREA')){
    inp.dispatchEvent(new KeyboardEvent('keydown',{key:'Enter',code:'Enter',keyCode:13,which:13,bubbles:true}));
    inp.dispatchEvent(new KeyboardEvent('keyup',{key:'Enter',code:'Enter',keyCode:13,which:13,bubbles:true}));
  }
  return 'enter';
}
target.click();
return 'clicked';
})()`

// loginFailureScript：收集登录失败提示与 OTP 输入框状态。
const loginFailureScript = `(() => {
const t=(document.body.innerText||'').replace(/\s+/g,' ').toLowerCase();
const hints=['password is incorrect','incorrect password','密码不正确','密码错误','密码无效','该邮箱未注册','email is not registered','没有与此邮箱关联','too many attempts','尝试次数过多','请求过于频繁','账户被锁定','account locked','验证码错误','invalid verification code','登录失败','登录不成功'];
const found=hints.filter(h=>t.includes(h));
const otp=[...document.querySelectorAll('input[data-input-otp="true"],input[name="code"],input[autocomplete="one-time-code"],input[inputmode="numeric"]')].some(n=>n&&n.getBoundingClientRect().width>0&&n.getBoundingClientRect().height>0);
return JSON.stringify({found,otp});
})()`
