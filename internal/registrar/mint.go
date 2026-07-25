package registrar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"grok_switch/internal/cpamint"
)

const (
	// Device API: auth.x.ai (device/code, token, verify, approve).
	// Human UI: accounts.x.ai (verification + consent pages).
	// Overall mint budget — registration already finished; mint must not hang.
	defaultMintTimeout = 4 * time.Minute
	// Short exchange after protocol path claims approve (pending race only).
	// Browser path uses pollDeviceToken (token poll = source of truth).
	tokenExchangeAttempts = 12
	tokenExchangeRetryGap = 1500 * time.Millisecond
	// Parallel token poll window while browser drives consent UI.
	// Keep these slightly longer than a slow SPA + cookie banner + Turnstile.
	browserTokenPollMax   = 130 * time.Second
	browserConsentTimeout = 120 * time.Second
	// After a real OAuth「允许」click, wait this long for token before any form fallback.
	// Premature form POST after SPA click yields "Invalid or expired code" and burns the device_code.
	postAllowTokenGrace = 22 * time.Second
	mintBodyLogLimit    = 240
)

type deviceCode struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
	Interval                int
}

type mintTokens struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresIn    int
}

// mintContext returns a deadline context for CPA minting that is intentionally
// NOT derived from the browser/CDP context. Chrome close or CDP disconnect must
// not cancel the pure-HTTP device flow. Parent is the job context so Stop() still works.
func mintContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, defaultMintTimeout)
}

// mintBrowserContext preserves chromedp's target values from browserCtx while
// also cancelling Chrome operations when the independent mint context expires.
// A plain child of browserCtx can otherwise wait forever in a CDP navigation
// even after the HTTP token flow has reached its timeout.
func mintBrowserContext(mintCtx, browserCtx context.Context) (context.Context, context.CancelFunc) {
	if browserCtx == nil {
		if mintCtx == nil {
			return context.WithCancel(context.Background())
		}
		return context.WithCancel(mintCtx)
	}
	ctx, cancel := context.WithCancel(browserCtx)
	if mintCtx == nil {
		return ctx, cancel
	}
	go func() {
		select {
		case <-mintCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func mintFromSSO(ctx context.Context, browser *browserSession, sso, proxy string, _ bool, _ bool, log func(string)) (mintTokens, string, error) {
	// Use one real browser authorization attempt. Protocol verify/approve posts and
	// cross-strategy retries can turn an explicit OAuth denial into noisy retries.
	if !browserAlive(browser) {
		return mintTokens{}, "", fmt.Errorf("浏览器会话不可用，无法完成 CPA 授权")
	}
	mintLog(log, "CPA 授权：浏览器真实点击一次，token 端点作为唯一成功标准…")
	tokens, err := mintBrowser(ctx, browser, proxy, sso, log)
	if err != nil {
		if isDeviceAuthDenied(err) {
			mintLog(log, "授权服务器明确拒绝该账号/device grant，已停止，不再重试或提交备用表单")
			return mintTokens{}, "", fmt.Errorf("授权服务器明确拒绝该账号/device grant：%w", err)
		}
		return mintTokens{}, "", fmt.Errorf("浏览器 CPA 授权失败：%w", err)
	}
	return tokens, "browser", nil
}

func browserAlive(browser *browserSession) bool {
	return browser != nil && browser.ctx != nil && browser.ctx.Err() == nil
}

func mintProtocol(ctx context.Context, sso, proxy string, log func(string), browser *browserSession) (mintTokens, error) {
	if err := ctx.Err(); err != nil {
		return mintTokens{}, mintCancelErr(ctx)
	}
	jar, _ := cookiejar.New(nil)
	client, err := registrarHTTPClient(proxy)
	if err != nil {
		return mintTokens{}, err
	}
	client.Jar = jar
	// Mirror cpa_xai/protocol_mint.py cookie domains so device verify/approve see SSO.
	for _, host := range []string{
		"https://accounts.x.ai/",
		"https://auth.x.ai/",
		"https://www.x.ai/",
		"https://x.ai/",
		"https://grok.com/",
	} {
		u, _ := url.Parse(host)
		jar.SetCookies(u, []*http.Cookie{
			{Name: "sso", Value: sso, Path: "/", Secure: true, HttpOnly: true},
			{Name: "sso-rw", Value: sso, Path: "/", Secure: true, HttpOnly: true},
		})
	}
	// Prefer live browser cookies when available (CF clearance + full session).
	if browserAlive(browser) {
		if n, cErr := injectBrowserCookiesIntoJar(browser.ctx, jar); cErr != nil {
			mintLog(log, "导出浏览器 cookie 警告："+cErr.Error())
		} else if n > 0 {
			mintLog(log, fmt.Sprintf("已导入浏览器 cookie %d 条到协议客户端", n))
		}
	}

	mintLog(log, "验证 SSO cookie…")
	var response *http.Response
	var lastSSOErr error
	for attempt := 1; attempt <= 3; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://accounts.x.ai/", nil)
		if err != nil {
			return mintTokens{}, err
		}
		request.Header.Set("User-Agent", chromeUserAgent)
		response, err = client.Do(request)
		if err == nil {
			lastSSOErr = nil
			break
		}
		lastSSOErr = err
		if !isTransientMintNetError(err) || attempt == 3 {
			return mintTokens{}, fmt.Errorf("验证 SSO: %w", err)
		}
		select {
		case <-ctx.Done():
			return mintTokens{}, mintCancelErr(ctx)
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
	}
	if lastSSOErr != nil {
		return mintTokens{}, fmt.Errorf("验证 SSO: %w", lastSSOErr)
	}
	response.Body.Close()
	finalPath := response.Request.URL.Path
	mintLog(log, fmt.Sprintf("SSO 校验完成 HTTP %s path=%s", response.Status, finalPath))
	if strings.Contains(finalPath, "sign-in") || strings.Contains(finalPath, "sign-up") {
		return mintTokens{}, fmt.Errorf("SSO 已失效（跳转到 %s）", finalPath)
	}

	mintLog(log, "请求 device code… endpoint="+cpamint.DeviceCodeURL)
	device, err := requestDevice(ctx, client)
	if err != nil {
		return mintTokens{}, err
	}
	mintLog(log, fmt.Sprintf(
		"device code 已获取 user_code=%s expires_in=%ds interval=%ds api=%s verify_uri=%s",
		device.UserCode, device.ExpiresIn, device.Interval, cpamint.DeviceCodeURL, device.VerificationURI,
	))

	// With a live browser: ONE device_code, SPA authorize, then token.
	// Never request a second device_code inside the browser helper.
	if browserAlive(browser) {
		mintLog(log, "浏览器串行 device-flow（单码 SPA 点「允许」→ token）…")
		tokens, err := mintBrowserNativeWithClient(ctx, browser, client, device, log)
		if err == nil {
			return tokens, nil
		}
		mintLog(log, "浏览器串行 device-flow 失败："+err.Error())
		return mintTokens{}, err
	}

	// Pure HTTP path (no Chrome): rarely works without Chrome TLS (curl_cffi).
	// Still attempt for protocol_only / headless-less recovery.
	mintLog(log, "HTTP 协议路径（无浏览器）：GET verify-uri → verify → approve → token…")
	if err := visitDevice(ctx, client, device, log); err != nil {
		mintLog(log, "GET verification_uri："+err.Error())
	}
	mintLog(log, fmt.Sprintf("HTTP 提交 device verify… user_code=%s", device.UserCode))
	if err := postDeviceAction(ctx, client, cpamint.DeviceVerifyURL, url.Values{"user_code": {device.UserCode}}, log); err != nil {
		mintLog(log, "device verify："+err.Error())
	}
	mintLog(log, "HTTP 提交 device approve（allow）…")
	approveVals := url.Values{
		"user_code":      {device.UserCode},
		"action":         {"allow"},
		"principal_type": {"User"},
		"principal_id":   {""},
	}
	if err := postDeviceAction(ctx, client, cpamint.DeviceApproveURL, approveVals, log); err != nil {
		mintLog(log, "device approve："+err.Error())
	}
	mintLog(log, "HTTP authorize 已提交，换取 token…")
	return exchangeDeviceToken(ctx, client, device, log)
}

// mintBrowserNativeWithClient: proven free-account flow (cpa_xai/browser_confirm + live logs):
//
//	ONE device_code → verification_uri_complete → cookie 全部允许 → consent
//	→ set form action=allow → REAL mouse click「允许」→ token
//
// Hard rules from success logs:
//   - SPA real click with action=allow is the path that yields tokens
//   - Form-after-success often returns "invalid/expired" (code already used) — OK
//   - Pure Go HTTP approve → SPA /done is a FALSE positive (Access denied)
//   - Never request a second device_code here
func mintBrowserNativeWithClient(ctx context.Context, browser *browserSession, client *http.Client, device deviceCode, log func(string)) (mintTokens, error) {
	return mintBrowserNativeWithDevice(ctx, browser, client, device, log)
}

// authorizeDeviceInBrowser drives accounts.x.ai device/consent until stopCh closes
// (token success) or timeout. Sets form action=allow before every OAuth click.
// forceForm: lead with SPA form action=allow on first consent (last-retry path).
func authorizeDeviceInBrowser(parent, browserCtx context.Context, device deviceCode, log func(string), timeout time.Duration, stopCh <-chan struct{}, pacer *tokenPollPacer, netWatch *consentNetWatch, forceForm bool) error {
	if browserCtx == nil || browserCtx.Err() != nil {
		return fmt.Errorf("浏览器不可用")
	}
	if netWatch == nil {
		netWatch = newConsentNetWatch()
	}
	if pacer == nil {
		pacer = &tokenPollPacer{interval: pollIntervalForDevice(device)}
	}
	deadline := time.Now().Add(timeout)
	consentURL := cpamint.DeviceConsentURL + "?user_code=" + url.QueryEscape(device.UserCode)
	clickedAllow := 0
	formTried := false
	sawAuthorized := false

	// Open prefilled device page once (single device_code for this entire attempt).
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(device.VerificationURIComplete),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("打开 device 页: %w", err)
	}
	time.Sleep(800 * time.Millisecond)
	// Cookie first, then optional force form lead-in (still same device_code).
	_ = realClickExact(browserCtx, []string{"全部允许", "Accept all cookies", "Accept All", "Allow all"})
	_ = fillUserCodeIfNeeded(browserCtx, device.UserCode)
	_ = realClickExact(browserCtx, []string{"继续", "Continue", "Next", "下一步"})
	time.Sleep(900 * time.Millisecond)
	if forceForm {
		mintLog(log, "forceForm：consent 页优先 SPA form action=allow…")
		_ = ensureConsentFormAllow(browserCtx)
		_ = realClickExact(browserCtx, oauthAllowLabels())
		if err := browserSPAFormAllow(browserCtx, device, log); err != nil {
			mintLog(log, "forceForm SPA form："+err.Error())
		} else {
			formTried = true
			pacer.speedUp()
		}
		time.Sleep(1500 * time.Millisecond)
	}

	for time.Now().Before(deadline) {
		select {
		case <-stopCh:
			return nil
		default:
		}
		if parent != nil && parent.Err() != nil {
			return mintCancelErr(parent)
		}
		if browserCtx.Err() != nil {
			return fmt.Errorf("浏览器在授权过程中断开: %w", browserCtx.Err())
		}

		var snap struct {
			URL   string `json:"url"`
			Text  string `json:"text"`
			Phase string `json:"phase"`
		}
		_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
const text=(document.body&&(document.body.innerText||document.body.textContent)||'').trim();
const url=location.href||'';
const low=text.toLowerCase();
let phase='wait';
if(text.includes('设备已授权')||low.includes('device authorized')||low.includes('device has been authorized')) phase='authorized';
else if(url.includes('device/done')||url.includes('/oauth2/device/done')) phase='done_shell';
else if(url.includes('sign-in')||url.includes('sign-up')||low.includes('sign in to')||text.includes('登录到')||text.includes('登录您的')) phase='signin';
else if(text.includes('隐私偏好')||text.includes('全部允许')||/accept all cookies/i.test(text)) phase='cookie';
else if(url.includes('/consent')||text.includes('授权 Grok')||text.includes('Authorize Grok')||text.includes('Grok Build')||text.includes('请求访问')||text.includes('wants to access')) phase='consent';
else if(document.querySelector("input[name='user_code']")||(url.includes('/oauth2/device')&&!url.includes('consent')&&!url.includes('done'))) phase='device';
return {url, text: text.slice(0,800), phase};
})()`, &snap))

		switch snap.Phase {
		case "authorized":
			if pageShowsDeviceAuthorized(browserCtx) {
				if !sawAuthorized {
					sawAuthorized = true
					pacer.speedUp()
					mintLog(log, "页面正文确认「设备已授权」url="+shortEndpoint(snap.URL))
				}
				if err := sleepConsent(parent, stopCh, 800*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			snap.Phase = "done_shell"
			fallthrough
		case "done_shell":
			// SPA /done alone is NOT success. If we clicked allow, wait for token.
			if clickedAllow > 0 || sawAuthorized || netWatch.sawApprove() {
				pacer.speedUp()
				if err := sleepConsent(parent, stopCh, 1200*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			// No click evidence — try SPA form submit once.
			if !formTried {
				mintLog(log, "仅 SPA /done 空壳且无授权证据 → SPA form action=allow 兜底")
				if err := browserSPAFormAllow(browserCtx, device, log); err != nil {
					mintLog(log, "SPA form 兜底："+err.Error())
				} else {
					pacer.speedUp()
				}
				formTried = true
			}
			if err := sleepConsent(parent, stopCh, 1000*time.Millisecond); err != nil {
				return err
			}
			continue
		case "signin":
			return fmt.Errorf("授权页要求重新登录（SSO 未带到 accounts.x.ai）url=%s", snap.URL)
		case "cookie":
			if err := realClickExact(browserCtx, []string{"全部允许", "接受所有 cookie", "Accept all cookies", "Accept All", "Allow all"}); err == nil {
				mintLog(log, "已点击 Cookie「全部允许」")
				if err := sleepConsent(parent, stopCh, 700*time.Millisecond); err != nil {
					return err
				}
				continue
			}
		case "device":
			_ = fillUserCodeIfNeeded(browserCtx, device.UserCode)
			if err := realClickExact(browserCtx, []string{"继续", "Continue", "Next", "下一步"}); err == nil {
				mintLog(log, "已点击 device 页「继续」")
				if err := sleepConsent(parent, stopCh, 1100*time.Millisecond); err != nil {
					return err
				}
				continue
			}
		case "consent":
			// Cookie modal can overlay consent — never click 允许 under it.
			if strings.Contains(snap.Text, "隐私偏好") || strings.Contains(snap.Text, "全部允许") {
				_ = realClickExact(browserCtx, []string{"全部允许", "Accept all cookies", "Accept All"})
				if err := sleepConsent(parent, stopCh, 600*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			// CRITICAL: React consent form needs action=allow before click.
			// Empty action → Invalid action / Access denied.
			_ = ensureConsentFormAllow(browserCtx)
			_ = waitAllowButtonReady(browserCtx, parent, 10*time.Second, stopCh)
			if err := realClickExact(browserCtx, oauthAllowLabels()); err == nil {
				clickedAllow++
				before := netWatch.approveCount()
				pacer.speedUp()
				mintLog(log, fmt.Sprintf("已真实点击 OAuth「允许」(action=allow)(#%d)", clickedAllow))
				if err := sleepConsent(parent, stopCh, 3*time.Second); err != nil {
					return err
				}
				select {
				case <-stopCh:
					return nil
				default:
				}
				if pageShowsDeviceAuthorized(browserCtx) {
					sawAuthorized = true
					mintLog(log, "点击后可见「设备已授权」")
					continue
				}
				if netWatch.approveCount() > before {
					mintLog(log, "网络层已观察到 approve/allow，等待 token…")
					if err := sleepConsent(parent, stopCh, 8*time.Second); err != nil {
						return err
					}
					continue
				}
				// No network / no authorized text — SPA form submit (same page, not auth.x.ai nav).
				if clickedAllow == 1 && !formTried {
					mintLog(log, "点击后无授权证据，SPA form.requestSubmit(action=allow)…")
					if sErr := browserSPAFormAllow(browserCtx, device, log); sErr != nil {
						mintLog(log, "SPA form："+sErr.Error())
					}
					formTried = true
					if err := sleepConsent(parent, stopCh, 4*time.Second); err != nil {
						return err
					}
				} else if clickedAllow < 3 {
					mintLog(log, "再次点击「允许」…")
					_ = ensureConsentFormAllow(browserCtx)
					_ = realClickExact(browserCtx, oauthAllowLabels())
					if err := sleepConsent(parent, stopCh, 3*time.Second); err != nil {
						return err
					}
				}
				continue
			}
			// Button missing — navigate consent once, then SPA form.
			if !strings.Contains(snap.URL, "/consent") {
				_ = chromedp.Run(browserCtx, chromedp.Navigate(consentURL), chromedp.WaitReady("body", chromedp.ByQuery))
				_ = waitDeviceUIReady(browserCtx, parent, 8*time.Second, stopCh)
			} else if !formTried && clickedAllow == 0 {
				mintLog(log, "consent 页未找到「允许」按钮，SPA form 兜底…")
				if err := browserSPAFormAllow(browserCtx, device, log); err != nil {
					mintLog(log, "SPA form："+err.Error())
				}
				formTried = true
			}
		default:
			_ = fillUserCodeIfNeeded(browserCtx, device.UserCode)
			_ = realClickExact(browserCtx, []string{"继续", "Continue", "Next", "下一步"})
			_ = realClickExact(browserCtx, []string{"全部允许", "Accept all cookies", "Accept All"})
			if waitAllowButtonReady(browserCtx, parent, 2*time.Second, stopCh) == nil {
				_ = ensureConsentFormAllow(browserCtx)
				_ = realClickExact(browserCtx, oauthAllowLabels())
			}
			if clickedAllow == 0 && time.Now().After(deadline.Add(-timeout+8*time.Second)) {
				// After ~8s still unknown phase — force consent URL.
				_ = chromedp.Run(browserCtx, chromedp.Navigate(consentURL), chromedp.WaitReady("body", chromedp.ByQuery))
			}
		}
		if err := sleepConsent(parent, stopCh, 400*time.Millisecond); err != nil {
			return err
		}
	}
	select {
	case <-stopCh:
		return nil
	default:
	}
	if sawAuthorized {
		return fmt.Errorf("页面已显示「设备已授权」但 token 轮询未成功")
	}
	if clickedAllow > 0 || formTried {
		return fmt.Errorf("已尝试授权（点击=%d form=%v）但服务端未确认（勿信 SPA /done）", clickedAllow, formTried)
	}
	return fmt.Errorf("超时未完成 device-flow 授权（未点到「允许」）")
}

// browserSPAFormAllow forces a full navigation POST to auth.x.ai/device/approve.
// Relative SPA form posts often land on /done HTML without authorizing the device_code.
func browserSPAFormAllow(browserCtx context.Context, device deviceCode, log func(string)) error {
	return browserAbsoluteDeviceAuthorize(browserCtx, device, log)
}

// exchangeDeviceTokenOnce is a single token POST (no multi-attempt loop).
func exchangeDeviceTokenOnce(ctx context.Context, client *http.Client, device deviceCode) (mintTokens, error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {device.DeviceCode},
		"client_id":   {cpamint.ClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cpamint.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return mintTokens{}, err
	}
	setAccountsOAuthHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return mintTokens{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return mintTokens{}, fmt.Errorf("parse token: %w body=%s", err, compactMintBody(body))
	}
	if resp.StatusCode == http.StatusOK && payload.AccessToken != "" && payload.RefreshToken != "" {
		return mintTokens{payload.AccessToken, payload.RefreshToken, payload.IDToken, payload.ExpiresIn}, nil
	}
	errCode := strings.TrimSpace(payload.Error)
	desc := strings.TrimSpace(payload.Description)
	if errCode == "" {
		errCode = fmt.Sprintf("http_%d", resp.StatusCode)
	}
	return mintTokens{}, fmt.Errorf("%s %s", errCode, desc)
}

// injectBrowserCookiesIntoJar copies Chrome cookies for xAI hosts into the HTTP jar.
func injectBrowserCookiesIntoJar(browserCtx context.Context, jar http.CookieJar) (int, error) {
	if browserCtx == nil || jar == nil {
		return 0, fmt.Errorf("nil browser/jar")
	}
	var cookies []*network.Cookie
	if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs([]string{
			"https://accounts.x.ai/",
			"https://auth.x.ai/",
			"https://grok.com/",
			"https://x.ai/",
		}).Do(ctx)
		return err
	})); err != nil {
		return 0, err
	}
	n := 0
	// Apply each cookie against the hosts that need it.
	targets := []*url.URL{
		mustParseURL("https://accounts.x.ai/"),
		mustParseURL("https://auth.x.ai/"),
		mustParseURL("https://grok.com/"),
		mustParseURL("https://x.ai/"),
	}
	for _, c := range cookies {
		if c == nil || c.Name == "" {
			continue
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		hc := &http.Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Path:     path,
			Domain:   c.Domain,
			Secure:   c.Secure,
			HttpOnly: c.HTTPOnly,
		}
		for _, u := range targets {
			jar.SetCookies(u, []*http.Cookie{hc})
			n++
		}
	}
	return n, nil
}

func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func requestDevice(ctx context.Context, client *http.Client) (deviceCode, error) {
	// Proxy blips (EOF through Clash) are common right after a long signup session.
	// Retry a few times instead of failing the whole mint.
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		code, err := requestDeviceOnce(ctx, client)
		if err == nil {
			return code, nil
		}
		lastErr = err
		if !isTransientMintNetError(err) || attempt == 4 {
			break
		}
		wait := time.Duration(attempt) * 600 * time.Millisecond
		select {
		case <-ctx.Done():
			return deviceCode{}, mintCancelErr(ctx)
		case <-time.After(wait):
		}
	}
	return deviceCode{}, lastErr
}

func requestDeviceOnce(ctx context.Context, client *http.Client) (deviceCode, error) {
	form := url.Values{"client_id": {cpamint.ClientID}, "scope": {cpamint.Scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cpamint.DeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return deviceCode{}, err
	}
	setAccountsOAuthHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return deviceCode{}, fmt.Errorf("请求 device code: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return deviceCode{}, fmt.Errorf("读取 device code 响应: %w", err)
	}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		VerificationURL         string `json:"verification_url"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	trim := strings.TrimSpace(string(body))
	if strings.HasPrefix(trim, "<") {
		return deviceCode{}, fmt.Errorf("device code 返回 HTML 而非 JSON（HTTP %s）— API 应为 auth.x.ai/oauth2/device/code，body=%s", resp.Status, compactMintBody(body))
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return deviceCode{}, fmt.Errorf("解析 device code 响应 HTTP %s: %w body=%s", resp.Status, err, compactMintBody(body))
	}
	if resp.StatusCode != http.StatusOK || payload.DeviceCode == "" || payload.UserCode == "" {
		if payload.Error != "" {
			return deviceCode{}, fmt.Errorf("device code 返回 %s error=%s desc=%s", resp.Status, payload.Error, payload.ErrorDescription)
		}
		return deviceCode{}, fmt.Errorf("device code 返回 %s body=%s", resp.Status, compactMintBody(body))
	}
	if payload.ExpiresIn <= 0 {
		payload.ExpiresIn = 1800
	}
	if payload.Interval < 1 {
		payload.Interval = 5
	}
	if payload.VerificationURI == "" {
		payload.VerificationURI = firstNonEmptyMint(payload.VerificationURL, cpamint.VerificationURIDefault)
	}
	if payload.VerificationURIComplete == "" {
		payload.VerificationURIComplete = payload.VerificationURI + "?user_code=" + url.QueryEscape(payload.UserCode)
	}
	return deviceCode{payload.DeviceCode, payload.UserCode, payload.VerificationURI, payload.VerificationURIComplete, payload.ExpiresIn, payload.Interval}, nil
}

func isTransientMintNetError(err error) bool {
	if err == nil {
		return false
	}
	if isTransientNavError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{
		"eof", "broken pipe", "connection reset", "connection refused",
		"i/o timeout", "tls:", "wsarecv", "wsasend", "forcibly closed",
		"unexpected eof", "http2:", "stream error",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}

// approveDeviceConsent walks the accounts.x.ai consent surface with SSO cookies:
// GET consent page, then POST action=allow.
func approveDeviceConsent(ctx context.Context, client *http.Client, device deviceCode, log func(string)) error {
	consentPage := cpamint.DeviceConsentURL + "?user_code=" + url.QueryEscape(device.UserCode)
	if err := visitURL(ctx, client, consentPage, log); err != nil {
		return fmt.Errorf("打开 consent 页: %w", err)
	}
	// Primary: form POST to consent endpoint (new surface).
	err := postDeviceAction(ctx, client, cpamint.DeviceConsentURL, url.Values{
		"user_code":      {device.UserCode},
		"action":         {"allow"},
		"principal_type": {"User"},
		"principal_id":   {""},
	}, log)
	if err == nil {
		return nil
	}
	mintLog(log, "consent POST 未确认成功，尝试带 query 的 consent POST："+err.Error())
	// Fallback: POST the same path that the browser would load (some builds only accept this).
	return postDeviceAction(ctx, client, consentPage, url.Values{
		"user_code":      {device.UserCode},
		"action":         {"allow"},
		"principal_type": {"User"},
		"principal_id":   {""},
	}, log)
}

func setAccountsOAuthHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json, text/html, */*")
	// Device/token API is on auth.x.ai; consent UI is on accounts.x.ai.
	host := ""
	if req.URL != nil {
		host = strings.ToLower(req.URL.Host)
	}
	path := ""
	if req.URL != nil {
		path = req.URL.Path
	}
	if strings.Contains(host, "accounts.x.ai") {
		req.Header.Set("Origin", "https://accounts.x.ai")
		req.Header.Set("Referer", "https://accounts.x.ai/")
	} else if strings.Contains(path, "device/approve") || strings.Contains(path, "device/verify") {
		// Form approve/verify expects a device-flow referer chain.
		req.Header.Set("Origin", "https://auth.x.ai")
		req.Header.Set("Referer", "https://auth.x.ai/oauth2/device/verify")
	} else {
		req.Header.Set("Origin", "https://auth.x.ai")
		req.Header.Set("Referer", "https://auth.x.ai/")
	}
	req.Header.Set("User-Agent", chromeUserAgent)
}

func firstNonEmptyMint(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func visitDevice(ctx context.Context, client *http.Client, device deviceCode, log func(string)) error {
	return visitURL(ctx, client, device.VerificationURIComplete, log)
}

func visitURL(ctx context.Context, client *http.Client, rawURL string, log func(string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", chromeUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Referer", "https://accounts.x.ai/")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("访问 %s: %w", shortEndpoint(rawURL), err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	finalURL := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	mintLog(log, fmt.Sprintf("%s → HTTP %s final=%s body=%s", shortEndpoint(rawURL), resp.Status, finalURL, compactMintBody(body)))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s HTTP %s", shortEndpoint(rawURL), resp.Status)
	}
	return nil
}

func postDeviceAction(ctx context.Context, client *http.Client, endpoint string, form url.Values, log func(string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	setAccountsOAuthHeaders(req)
	req.Header.Set("Accept", "text/html,application/json")
	req.Header.Set("Referer", cpamint.DeviceConsentURL+"?user_code="+url.QueryEscape(form.Get("user_code")))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	finalPath := ""
	finalURL := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalPath = resp.Request.URL.Path
		finalURL = resp.Request.URL.String()
	}
	lowBody := strings.ToLower(string(body))
	// Real CF interstitial only (Next.js SPA often embeds "cloudflare" in JS — ignore that).
	if isCloudflareChallenge(lowBody, resp.StatusCode) {
		return fmt.Errorf("HTTP %s Cloudflare 人机验证页 path=%s", resp.Status, finalPath)
	}
	if strings.Contains(finalPath, "sign-in") || strings.Contains(finalPath, "sign-up") {
		return fmt.Errorf("HTTP %s SSO 会话无效 path=%s body=%s", resp.Status, finalPath, compactMintBody(body))
	}
	// protocol_mint.py: landing on /oauth2/device/consent after verify = success.
	okPath := strings.Contains(finalPath, "consent") ||
		strings.Contains(finalPath, "done") ||
		strings.Contains(finalPath, "/oauth2/device") ||
		strings.Contains(finalURL, "authorized")
	okBody := strings.Contains(lowBody, "authorized") ||
		strings.Contains(lowBody, "设备已授权") ||
		strings.Contains(lowBody, "device authorized") ||
		strings.Contains(lowBody, "authorize grok") ||
		strings.Contains(lowBody, "授权 grok") ||
		(strings.Contains(lowBody, "consent") && !strings.Contains(lowBody, "error"))
	// Empty 2xx body after allow is acceptable only on consent/approve endpoints.
	okEmpty := resp.StatusCode >= 200 && resp.StatusCode < 300 &&
		len(strings.TrimSpace(string(body))) < 8 &&
		(strings.Contains(endpoint, "consent") || strings.Contains(endpoint, "approve") || strings.Contains(endpoint, "verify"))
	// Strong authorize markers only — SPA HTML often contains loose words like
	// "consent" / "authorized" in JS bundles and must NOT count as success.
	okAuthorized := bodyHasDeviceAuthorized(lowBody)
	// Next.js always returns the same HTML shell for any path — path=/done alone is NOT proof.
	// Only trust SPA shell on verify→consent; approve success is decided by token exchange.
	okSPAConsent := resp.StatusCode >= 200 && resp.StatusCode < 400 &&
		strings.Contains(finalPath, "consent") &&
		strings.Contains(endpoint, "verify") &&
		isNextSPAShell(lowBody)
	okStatus := resp.StatusCode >= 200 && resp.StatusCode < 400
	mintLog(log, fmt.Sprintf("%s → HTTP %s path=%s body=%s", shortEndpoint(endpoint), resp.Status, finalPath, compactMintBody(body)))
	// approve / consent POST: hard-fail only on explicit non-SPA errors.
	// Success is decided later by the token endpoint. SPA HTML on /done is NORMAL
	// (accounts.x.ai Next.js shell after a real authorize redirect) and embeds
	// i18n strings like "access denied" / "expired" that must NOT hard-fail.
	isApprovePost := strings.Contains(endpoint, "approve") || strings.Contains(endpoint, "/device/consent")
	if isApprovePost {
		if okStatus && isNextSPAShell(lowBody) {
			return nil // provisional OK — token exchange decides
		}
		// Plain-text / JSON error pages only (not SPA shells).
		if !isNextSPAShell(lowBody) {
			if strings.Contains(lowBody, "invalid or expired") ||
				strings.Contains(lowBody, "access denied") ||
				strings.Contains(lowBody, "invalid_grant") {
				return fmt.Errorf("HTTP %s path=%s 授权被拒绝 body=%s", resp.Status, finalPath, compactMintBody(body))
			}
		}
		if okStatus {
			return nil
		}
		return fmt.Errorf("HTTP %s path=%s body=%s", resp.Status, finalPath, compactMintBody(body))
	}
	// verify: consent path / SPA shell / authorized markers are fine.
	if !okStatus || (!okPath && !okBody && !okEmpty && !okSPAConsent && !okAuthorized) {
		return fmt.Errorf("HTTP %s path=%s body=%s", resp.Status, finalPath, compactMintBody(body))
	}
	return nil
}

func bodyHasDeviceAuthorized(lowBody string) bool {
	// Next.js account pages embed i18n dictionaries that mention these phrases
	// even when authorization never happened — never trust the full HTML shell.
	if isNextSPAShell(lowBody) {
		return false
	}
	return strings.Contains(lowBody, "设备已授权") ||
		strings.Contains(lowBody, "device authorized") ||
		strings.Contains(lowBody, "device has been authorized") ||
		strings.Contains(lowBody, "you have been authorized")
}

func isNextSPAShell(lowBody string) bool {
	return strings.Contains(lowBody, "/_next/static") ||
		strings.Contains(lowBody, "inter_b2991b2") ||
		(strings.Contains(lowBody, "<!doctype html") && strings.Contains(lowBody, "antialiased"))
}

// isCloudflareChallenge detects the real "Just a moment" interstitial, not
// normal accounts.x.ai Next.js pages that merely mention cloudflare in assets.
func isCloudflareChallenge(lowBody string, status int) bool {
	if strings.Contains(lowBody, "attention required") ||
		strings.Contains(lowBody, "just a moment") ||
		strings.Contains(lowBody, "cf-browser-verification") ||
		strings.Contains(lowBody, "cf-challenge") ||
		strings.Contains(lowBody, "checking your browser") {
		return true
	}
	// Bare "cloudflare" alone is NOT enough (false positive on SPA HTML).
	if status == 403 && strings.Contains(lowBody, "cloudflare") {
		return true
	}
	return false
}

// exchangeDeviceToken performs a short OAuth token exchange after the protocol
// path submits approve. There is intentionally NO multi-minute expires_in poll.
// Success = access+refresh tokens; SPA HTML is ignored.
func exchangeDeviceToken(ctx context.Context, client *http.Client, device deviceCode, log func(string)) (mintTokens, error) {
	// Slightly longer window: approve→token can lag a few seconds after SPA redirect.
	attempts := tokenExchangeAttempts + 8
	maxWait := time.Duration(attempts)*tokenExchangeRetryGap + 5*time.Second
	return pollDeviceToken(ctx, client, device, log, maxWait, tokenExchangeRetryGap, attempts, nil)
}

// tokenPollPacer lets the UI speed up polling after a real「允许」click while
// still respecting a minimum interval (RFC 8628 + slow_down handling).
type tokenPollPacer struct {
	mu       sync.Mutex
	interval time.Duration
}

func pollIntervalForDevice(device deviceCode) time.Duration {
	interval := time.Duration(device.Interval) * time.Second
	if interval < 2*time.Second {
		interval = 5 * time.Second
	}
	if interval > 10*time.Second {
		interval = 5 * time.Second
	}
	return interval
}

func (p *tokenPollPacer) get() time.Duration {
	if p == nil {
		return 5 * time.Second
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.interval < time.Second {
		return 5 * time.Second
	}
	return p.interval
}

func (p *tokenPollPacer) speedUp() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Floor at 1.5s — aggressive enough after allow, still polite to the token endpoint.
	if p.interval > 1500*time.Millisecond {
		p.interval = 1500 * time.Millisecond
	}
}

// pollDeviceToken polls the token endpoint until tokens arrive, hard auth error,
// deadline, maxAttempts, or stopCh is closed. authorization_pending / slow_down
// keep going; invalid_grant Access denied is terminal for this device_code.
// maxAttempts <= 0 means unlimited (until maxWait).
func pollDeviceToken(ctx context.Context, client *http.Client, device deviceCode, log func(string), maxWait, interval time.Duration, maxAttempts int, stopCh <-chan struct{}) (mintTokens, error) {
	return pollDeviceTokenPaced(ctx, client, device, log, maxWait, &tokenPollPacer{interval: interval}, maxAttempts, stopCh)
}

func pollDeviceTokenPaced(ctx context.Context, client *http.Client, device deviceCode, log func(string), maxWait time.Duration, pacer *tokenPollPacer, maxAttempts int, stopCh <-chan struct{}) (mintTokens, error) {
	if maxWait < 5*time.Second {
		maxWait = 5 * time.Second
	}
	if pacer == nil {
		pacer = &tokenPollPacer{interval: pollIntervalForDevice(device)}
	} else if pacer.get() < 200*time.Millisecond {
		pacer = &tokenPollPacer{interval: pollIntervalForDevice(device)}
	}
	deadline := time.Now().Add(maxWait)
	var lastStatus string
	attempt := 0
	for time.Now().Before(deadline) {
		if maxAttempts > 0 && attempt >= maxAttempts {
			break
		}
		if err := ctx.Err(); err != nil {
			return mintTokens{}, mintCancelErr(ctx)
		}
		select {
		case <-stopCh:
			return mintTokens{}, fmt.Errorf("token 轮询已停止")
		default:
		}
		attempt++
		interval := pacer.get()
		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {device.DeviceCode},
			"client_id":   {cpamint.ClientID},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cpamint.TokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return mintTokens{}, err
		}
		setAccountsOAuthHeaders(req)
		resp, err := client.Do(req)
		if err != nil {
			lastStatus = "network:" + err.Error()
			mintLog(log, fmt.Sprintf("token 轮询 #%d 网络错误：%s", attempt, err.Error()))
			if !isTransientMintNetError(err) {
				return mintTokens{}, fmt.Errorf("换取 token 网络失败: %w", err)
			}
			if err := waitPoll(ctx, stopCh, interval); err != nil {
				return mintTokens{}, err
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if strings.HasPrefix(strings.TrimSpace(string(body)), "<") {
			return mintTokens{}, fmt.Errorf("token 端点返回 HTML（HTTP %s），请确认 TokenURL=%s", resp.Status, cpamint.TokenURL)
		}
		var payload struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			ExpiresIn    int    `json:"expires_in"`
			Error        string `json:"error"`
			Description  string `json:"error_description"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return mintTokens{}, fmt.Errorf("解析 token 响应 HTTP %s: %w body=%s", resp.Status, err, compactMintBody(body))
		}
		if resp.StatusCode == http.StatusOK && payload.AccessToken != "" && payload.RefreshToken != "" {
			mintLog(log, fmt.Sprintf("token 换取成功 expires_in=%ds", payload.ExpiresIn))
			return mintTokens{payload.AccessToken, payload.RefreshToken, payload.IDToken, payload.ExpiresIn}, nil
		}
		errCode := strings.TrimSpace(payload.Error)
		desc := strings.TrimSpace(payload.Description)
		lastStatus = errCode
		if errCode == "" {
			lastStatus = fmt.Sprintf("http_%s", resp.Status)
		}
		if isDeviceAuthDeniedCode(errCode, desc) {
			if desc == "" {
				desc = errCode
			}
			// Terminal for this device_code — UI "done" was a false positive.
			return mintTokens{}, fmt.Errorf("设备授权未生效（%s %s）：页面可能未真实点到「允许」", errCode, desc)
		}
		if errCode == "authorization_pending" || errCode == "slow_down" {
			if attempt == 1 || attempt%3 == 0 {
				mintLog(log, fmt.Sprintf("token 轮询 #%d 状态=%s interval=%s（等待服务端确认授权）", attempt, errCode, pacer.get()))
			}
			wait := pacer.get()
			if errCode == "slow_down" {
				// Honor server backoff and keep the floor raised briefly.
				wait = pacer.get() + 3*time.Second
				pacer.mu.Lock()
				if pacer.interval < 3*time.Second {
					pacer.interval = 3 * time.Second
				}
				pacer.mu.Unlock()
			}
			if err := waitPoll(ctx, stopCh, wait); err != nil {
				return mintTokens{}, err
			}
			continue
		}
		// Soft HTTP / unknown: brief retry then fail.
		if resp.StatusCode >= 500 {
			mintLog(log, fmt.Sprintf("token 轮询 #%d 软错误 HTTP %s", attempt, resp.Status))
			if err := waitPoll(ctx, stopCh, pacer.get()); err != nil {
				return mintTokens{}, err
			}
			continue
		}
		return mintTokens{}, fmt.Errorf("换取 token 失败: %s %s", lastStatus, desc)
	}
	return mintTokens{}, fmt.Errorf("设备未授权完成（%s）。请确认浏览器出现「设备已授权」", lastStatus)
}

func waitPoll(ctx context.Context, stopCh <-chan struct{}, d time.Duration) error {
	if d < 200*time.Millisecond {
		d = 200 * time.Millisecond
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return mintCancelErr(ctx)
	case <-stopCh:
		return fmt.Errorf("token 轮询已停止")
	case <-timer.C:
		return nil
	}
}

func isDeviceAuthDeniedCode(errCode, desc string) bool {
	errCode = strings.TrimSpace(errCode)
	lowDesc := strings.ToLower(desc)
	if errCode == "access_denied" || errCode == "expired_token" {
		return true
	}
	if errCode == "invalid_grant" && strings.Contains(lowDesc, "access denied") {
		return true
	}
	return false
}

func isDeviceAuthDenied(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access denied") ||
		strings.Contains(msg, "设备授权未生效") ||
		strings.Contains(msg, "access_denied")
}

// pollToken is kept as a thin alias name for tests that still call exchange semantics.
func pollToken(ctx context.Context, client *http.Client, device deviceCode, log func(string), _ time.Duration) (mintTokens, error) {
	return exchangeDeviceToken(ctx, client, device, log)
}

func mintBrowser(ctx context.Context, browser *browserSession, proxy, sso string, log func(string)) (mintTokens, error) {
	if !browserAlive(browser) {
		return mintTokens{}, fmt.Errorf("浏览器会话不可用，无法浏览器铸造")
	}
	client, err := registrarHTTPClient(proxy)
	if err != nil {
		return mintTokens{}, err
	}
	return mintBrowserOnce(ctx, browser, client, sso, log)
}

func mintBrowserOnce(ctx context.Context, browser *browserSession, client *http.Client, sso string, log func(string)) (mintTokens, error) {
	// Light inject only — avoid warm navigate that cools a just-registered session.
	if sso = strings.TrimSpace(sso); sso != "" {
		if err := injectBrowserSSO(browser.ctx, sso); err != nil {
			mintLog(log, "注入 SSO cookie 警告："+err.Error())
		} else {
			mintLog(log, "已向浏览器注入 SSO cookie（轻量）")
		}
	}
	// Prefer device_code from the same Chrome session that will authorize (same exit IP/TLS).
	device, err := requestDeviceInBrowser(browser.ctx, log)
	if err != nil {
		mintLog(log, "浏览器申请 device code 失败，回退 Go HTTP："+err.Error())
		device, err = requestDevice(ctx, client)
		if err != nil {
			return mintTokens{}, err
		}
	}
	mintLog(log, fmt.Sprintf("device code 已获取 user_code=%s expires_in=%ds（单码、不重试）", device.UserCode, device.ExpiresIn))
	return mintBrowserNativeWithDevice(ctx, browser, client, device, log)
}

// requestDeviceInBrowser obtains a device_code via fetch from auth.x.ai inside Chrome.
func requestDeviceInBrowser(browserCtx context.Context, log func(string)) (deviceCode, error) {
	if browserCtx == nil || browserCtx.Err() != nil {
		return deviceCode{}, fmt.Errorf("浏览器不可用")
	}
	_ = chromedp.Run(browserCtx,
		chromedp.Navigate("https://auth.x.ai/"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
	time.Sleep(400 * time.Millisecond)
	script := fmt.Sprintf(`(async () => {
  const body = new URLSearchParams({
    client_id: %q,
    scope: %q,
  });
  const r = await fetch(%q, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/x-www-form-urlencoded',
      'Accept': 'application/json',
    },
    body: body.toString(),
    credentials: 'include',
  });
  const text = await r.text();
  return JSON.stringify({ status: r.status, body: text.slice(0, 4000) });
})()`, cpamint.ClientID, cpamint.Scope, cpamint.DeviceCodeURL)
	raw, err := evalAsyncJSON(browserCtx, script)
	if err != nil {
		return deviceCode{}, err
	}
	var wrap struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return deviceCode{}, fmt.Errorf("parse device wrap: %w", err)
	}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	if err := json.Unmarshal([]byte(wrap.Body), &payload); err != nil {
		return deviceCode{}, fmt.Errorf("parse device body: %w body=%s", err, compactMintBody([]byte(wrap.Body)))
	}
	if wrap.Status != 200 || payload.DeviceCode == "" || payload.UserCode == "" {
		if payload.Error != "" {
			return deviceCode{}, fmt.Errorf("device code %s: %s", payload.Error, payload.ErrorDescription)
		}
		return deviceCode{}, fmt.Errorf("device code HTTP %d body=%s", wrap.Status, compactMintBody([]byte(wrap.Body)))
	}
	if payload.ExpiresIn <= 0 {
		payload.ExpiresIn = 1800
	}
	if payload.Interval < 1 {
		payload.Interval = 5
	}
	if payload.VerificationURI == "" {
		payload.VerificationURI = cpamint.VerificationURIDefault
	}
	if payload.VerificationURIComplete == "" {
		payload.VerificationURIComplete = payload.VerificationURI + "?user_code=" + url.QueryEscape(payload.UserCode)
	}
	mintLog(log, "浏览器内已申请 device code user_code="+payload.UserCode)
	return deviceCode{
		DeviceCode:              payload.DeviceCode,
		UserCode:                payload.UserCode,
		VerificationURI:         payload.VerificationURI,
		VerificationURIComplete: payload.VerificationURIComplete,
		ExpiresIn:               payload.ExpiresIn,
		Interval:                payload.Interval,
	}, nil
}

// mintBrowserNativeWithDevice: SERIAL authorize then token (proven success pattern).
//
// Live-log lessons:
//   - SPA /done + 「设备已授权」text is often a FALSE positive (i18n / empty shell)
//   - Parallel token poll must stop the UI when it hits access_denied (don't wait 2min)
//   - SPA form.requestSubmit without absolute auth.x.ai action may not authorize
//   - Real authorize: MouseClickNode「允许」and/or browser form POST to
//     https://auth.x.ai/oauth2/device/{verify,approve}
//   - Token endpoint is the ONLY success criterion
func mintBrowserNativeWithDevice(ctx context.Context, browser *browserSession, client *http.Client, device deviceCode, log func(string)) (mintTokens, error) {
	if !browserAlive(browser) {
		return mintTokens{}, fmt.Errorf("浏览器不可用")
	}
	if client == nil {
		return mintTokens{}, fmt.Errorf("http client nil")
	}
	if strings.TrimSpace(device.DeviceCode) == "" || strings.TrimSpace(device.UserCode) == "" {
		var err error
		device, err = requestDevice(ctx, client)
		if err != nil {
			return mintTokens{}, err
		}
	}
	if device.VerificationURIComplete == "" {
		device.VerificationURIComplete = cpamint.VerificationURIDefault + "?user_code=" + url.QueryEscape(device.UserCode)
	}
	// Every Chrome operation must be bounded by the mint deadline. A hung CDP
	// form navigation previously outlived the five-minute mint context and kept
	// the registration job stuck forever, preventing protocol fallbacks.
	browserCtx, cancelBrowser := mintBrowserContext(ctx, browser.ctx)
	defer cancelBrowser()
	_ = chromedp.Run(browserCtx, network.Enable())
	netWatch := newConsentNetWatch()
	_ = installConsentNetWatch(browserCtx, netWatch)

	mintLog(log, fmt.Sprintf("使用 device user_code=%s（串行：点允许后立刻换 token）", device.UserCode))

	// 1) SPA path: open device page → cookie → continue → real click「允许」.
	//    CRITICAL: poll token IMMEDIATELY after click (success logs: token works
	//    right after SPA allow; delayed absolute form can race/cancel the session).
	if err := authorizeDeviceClickOnly(ctx, browserCtx, device, log, netWatch); err != nil {
		mintLog(log, "SPA 点击授权："+err.Error())
	}

	mintLog(log, "点击步骤结束，立即换 token…")
	tokens, err := pollDeviceToken(ctx, client, device, log, 18*time.Second, 1500*time.Millisecond, 12, nil)
	if err == nil {
		mintLog(log, fmt.Sprintf("token 换取成功 expires_in=%ds", tokens.ExpiresIn))
		return tokens, nil
	}
	mintLog(log, "token 端点未签发凭据，已停止："+err.Error())
	return mintTokens{}, err
}

// authorizeDeviceClickOnly: human SPA path only (no absolute form). Caller polls token next.
func authorizeDeviceClickOnly(parent, browserCtx context.Context, device deviceCode, log func(string), netWatch *consentNetWatch) error {
	if browserCtx == nil || browserCtx.Err() != nil {
		return fmt.Errorf("浏览器不可用")
	}
	if netWatch == nil {
		netWatch = newConsentNetWatch()
	}
	consentURL := cpamint.DeviceConsentURL + "?user_code=" + url.QueryEscape(device.UserCode)

	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(device.VerificationURIComplete),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("打开 device 页: %w", err)
	}
	time.Sleep(900 * time.Millisecond)
	_ = realClickExact(browserCtx, []string{"全部允许", "接受所有 cookie", "Accept all cookies", "Accept All", "Allow all"})
	time.Sleep(400 * time.Millisecond)
	_ = fillUserCodeIfNeeded(browserCtx, device.UserCode)
	if err := realClickExact(browserCtx, []string{"继续", "Continue", "Next", "下一步"}); err == nil {
		mintLog(log, "已点击 device 页「继续」")
		time.Sleep(1200 * time.Millisecond)
	}
	var href string
	_ = chromedp.Run(browserCtx, chromedp.Location(&href))
	if !strings.Contains(href, "/consent") && !strings.Contains(href, "/done") {
		_ = chromedp.Run(browserCtx, chromedp.Navigate(consentURL), chromedp.WaitReady("body", chromedp.ByQuery))
		time.Sleep(900 * time.Millisecond)
	}
	_ = realClickExact(browserCtx, []string{"全部允许", "Accept all cookies", "Accept All", "Allow all"})
	time.Sleep(400 * time.Millisecond)

	_ = waitAllowButtonReady(browserCtx, parent, 12*time.Second, nil)
	if parent != nil && parent.Err() != nil {
		return mintCancelErr(parent)
	}
	if browserCtx.Err() != nil {
		return fmt.Errorf("浏览器断开: %w", browserCtx.Err())
	}
	_ = realClickExact(browserCtx, []string{"全部允许", "Accept all cookies", "Accept All"})
	before := netWatch.approveCount()
	if err := clickOAuthAllowRobust(browserCtx); err != nil {
		return fmt.Errorf("未点到 OAuth「允许」: %w", err)
	}
	mintLog(log, "已真实点击 OAuth「允许」（单次）")
	// Brief settle only — token endpoint decides whether authorization succeeded.
	time.Sleep(1800 * time.Millisecond)
	if netWatch.approveCount() > before {
		mintLog(log, "网络层观察到真实 device/approve 请求")
	}
	if pageShowsDeviceAuthorized(browserCtx) {
		mintLog(log, "页面出现「设备已授权」文案（仅由 token 端点确认结果）")
	}
	return nil
}

// authorizeDeviceSerial opens the device page, dismisses cookies, real-clicks
// OAuth allow, then posts verify+approve to auth.x.ai absolute URLs via Chrome.
func authorizeDeviceSerial(parent, browserCtx context.Context, device deviceCode, log func(string), forceForm bool, netWatch *consentNetWatch) error {
	if browserCtx == nil || browserCtx.Err() != nil {
		return fmt.Errorf("浏览器不可用")
	}
	if netWatch == nil {
		netWatch = newConsentNetWatch()
	}
	consentURL := cpamint.DeviceConsentURL + "?user_code=" + url.QueryEscape(device.UserCode)

	// Open prefilled verification page (same as human / browser_confirm).
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(device.VerificationURIComplete),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("打开 device 页: %w", err)
	}
	time.Sleep(900 * time.Millisecond)
	_ = realClickExact(browserCtx, []string{"全部允许", "接受所有 cookie", "Accept all cookies", "Accept All", "Allow all"})
	time.Sleep(400 * time.Millisecond)
	_ = fillUserCodeIfNeeded(browserCtx, device.UserCode)
	if err := realClickExact(browserCtx, []string{"继续", "Continue", "Next", "下一步"}); err == nil {
		mintLog(log, "已点击 device 页「继续」")
		time.Sleep(1200 * time.Millisecond)
	}

	// Land on consent if not already there.
	var href string
	_ = chromedp.Run(browserCtx, chromedp.Location(&href))
	if !strings.Contains(href, "/consent") && !strings.Contains(href, "/done") {
		_ = chromedp.Run(browserCtx, chromedp.Navigate(consentURL), chromedp.WaitReady("body", chromedp.ByQuery))
		time.Sleep(900 * time.Millisecond)
	}
	_ = realClickExact(browserCtx, []string{"全部允许", "Accept all cookies", "Accept All", "Allow all"})
	time.Sleep(400 * time.Millisecond)

	if forceForm {
		mintLog(log, "forceForm：先走 auth.x.ai 绝对路径 verify/approve…")
		_ = browserAbsoluteDeviceAuthorize(browserCtx, device, log)
	}

	// Real OAuth allow click (MouseClickNode preferred).
	// Do NOT immediately form-POST after click — success path is click-only then token.
	// Absolute form is a fallback only when still pending (caller handles re-approve).
	_ = waitAllowButtonReady(browserCtx, parent, 12*time.Second, nil)
	clicked := false
	for try := 1; try <= 3; try++ {
		if parent != nil && parent.Err() != nil {
			return mintCancelErr(parent)
		}
		if browserCtx.Err() != nil {
			return fmt.Errorf("浏览器断开: %w", browserCtx.Err())
		}
		// Cookie overlay may reappear.
		_ = realClickExact(browserCtx, []string{"全部允许", "Accept all cookies", "Accept All"})
		_ = ensureConsentFormAllow(browserCtx)
		before := netWatch.approveCount()
		if err := clickOAuthAllowRobust(browserCtx); err != nil {
			mintLog(log, fmt.Sprintf("第 %d 次未点到「允许」: %v", try, err))
			_ = chromedp.Run(browserCtx, chromedp.Navigate(consentURL), chromedp.WaitReady("body", chromedp.ByQuery))
			time.Sleep(800 * time.Millisecond)
			continue
		}
		clicked = true
		mintLog(log, fmt.Sprintf("已真实点击 OAuth「允许」(try=%d)", try))
		time.Sleep(2800 * time.Millisecond)
		if netWatch.approveCount() > before {
			mintLog(log, "网络层观察到真实 device/approve 请求")
			// Still run absolute form — SPA network noise is common; form is cheap.
		}
		if pageShowsDeviceAuthorized(browserCtx) {
			mintLog(log, "页面出现「设备已授权」文案（仅供参考，继续绝对路径 approve）")
		}
		// Proven success path: SPA click then absolute approve (invalid/expired = already ok).
		break
	}

	// Always finish with absolute auth.x.ai verify+approve (Chrome cookies + TLS).
	// If SPA already authorized, approve returns invalid/expired — token still works.
	mintLog(log, "浏览器绝对路径 POST auth.x.ai verify → approve…")
	if err := browserAbsoluteDeviceAuthorize(browserCtx, device, log); err != nil {
		mintLog(log, "绝对路径 approve："+err.Error())
		if !clicked {
			return fmt.Errorf("未点到「允许」且绝对路径 form 失败: %w", err)
		}
	}
	return nil
}

// browserAbsoluteDeviceAuthorize posts verify + approve to auth.x.ai absolute URLs
// from the live Chrome session (SSO cookies + Chrome TLS). This bypasses the
// accounts.x.ai SPA shell that returns fake /done HTML without authorizing.
func browserAbsoluteDeviceAuthorize(browserCtx context.Context, device deviceCode, log func(string)) error {
	if browserCtx == nil || browserCtx.Err() != nil {
		return fmt.Errorf("浏览器不可用")
	}
	userCode := strings.TrimSpace(device.UserCode)
	if userCode == "" {
		return fmt.Errorf("empty user_code")
	}
	var href string
	_ = chromedp.Run(browserCtx, chromedp.Location(&href))
	if !strings.Contains(href, "accounts.x.ai") && !strings.Contains(href, "auth.x.ai") {
		_ = chromedp.Run(browserCtx,
			chromedp.Navigate("https://accounts.x.ai/account"),
			chromedp.WaitReady("body", chromedp.ByQuery),
		)
		time.Sleep(400 * time.Millisecond)
	}

	mintLog(log, "浏览器 form POST "+shortEndpoint(cpamint.DeviceVerifyURL)+" user_code="+userCode)
	if err := browserSubmitFormTimed(browserCtx, cpamint.DeviceVerifyURL, map[string]string{
		"user_code": userCode,
	}, 12*time.Second); err != nil {
		return fmt.Errorf("verify form: %w", err)
	}
	time.Sleep(1200 * time.Millisecond)
	_ = realClickExact(browserCtx, []string{"全部允许", "Accept all cookies", "Accept All"})
	_ = ensureConsentFormAllow(browserCtx)
	if err := clickOAuthAllowRobust(browserCtx); err == nil {
		mintLog(log, "verify 后真实点击了 OAuth「允许」")
		time.Sleep(1200 * time.Millisecond)
	}

	mintLog(log, "浏览器 form POST "+shortEndpoint(cpamint.DeviceApproveURL)+" action=allow")
	if err := browserSubmitFormTimed(browserCtx, cpamint.DeviceApproveURL, map[string]string{
		"user_code":      userCode,
		"action":         "allow",
		"principal_type": "User",
		"principal_id":   "",
	}, 12*time.Second); err != nil {
		return fmt.Errorf("approve form: %w", err)
	}
	time.Sleep(1200 * time.Millisecond)
	var finalURL, text string
	_ = chromedp.Run(browserCtx,
		chromedp.Location(&finalURL),
		chromedp.Evaluate(`((document.body&&document.body.innerText)||'').replace(/\s+/g,' ').trim().slice(0,320)`, &text),
	)
	low := strings.ToLower(text)
	if strings.Contains(text, "您的设备已获授权") || strings.Contains(text, "设备已授权") ||
		strings.Contains(low, "has been authorized") || strings.Contains(low, "device authorized") {
		mintLog(log, "approve 后可见真实授权文案 url="+shortEndpoint(finalURL))
		return nil
	}
	if strings.Contains(low, "invalid or expired") || strings.Contains(text, "无效") || strings.Contains(text, "过期") {
		mintLog(log, "approve 返回无效/过期（可能 SPA 已授权）："+compactMintBody([]byte(text)))
		return nil
	}
	mintLog(log, fmt.Sprintf("approve form 完成 url=%s text=%s", shortEndpoint(finalURL), compactMintBody([]byte(text))))
	return nil
}

// exchangeDeviceTokenInBrowser POSTs the token endpoint from auth.x.ai origin via
// Chrome fetch (avoids Go-HTTP TLS/proxy path mismatches after browser authorize).
func exchangeDeviceTokenInBrowser(browserCtx context.Context, device deviceCode, log func(string)) (mintTokens, error) {
	if browserCtx == nil || browserCtx.Err() != nil {
		return mintTokens{}, fmt.Errorf("浏览器不可用")
	}
	// Must be on auth.x.ai for same-origin fetch without CORS issues.
	_ = chromedp.Run(browserCtx,
		chromedp.Navigate("https://auth.x.ai/"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
	time.Sleep(400 * time.Millisecond)

	script := fmt.Sprintf(`(async () => {
  const body = new URLSearchParams({
    grant_type: 'urn:ietf:params:oauth:grant-type:device_code',
    device_code: %q,
    client_id: %q,
  });
  const r = await fetch(%q, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/x-www-form-urlencoded',
      'Accept': 'application/json',
    },
    body: body.toString(),
    credentials: 'include',
  });
  const text = await r.text();
  return JSON.stringify({ status: r.status, body: text.slice(0, 2000) });
})()`, device.DeviceCode, cpamint.ClientID, cpamint.TokenURL)

	// Retry a few times for pending.
	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		if browserCtx.Err() != nil {
			return mintTokens{}, browserCtx.Err()
		}
		raw, err := evalAsyncJSON(browserCtx, script)
		if err != nil {
			lastErr = err
			time.Sleep(1500 * time.Millisecond)
			continue
		}
		var wrap struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
		}
		if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
			lastErr = fmt.Errorf("parse wrap: %w raw=%s", err, compactMintBody([]byte(raw)))
			time.Sleep(1200 * time.Millisecond)
			continue
		}
		var payload struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			ExpiresIn    int    `json:"expires_in"`
			Error        string `json:"error"`
			Description  string `json:"error_description"`
		}
		if err := json.Unmarshal([]byte(wrap.Body), &payload); err != nil {
			lastErr = fmt.Errorf("parse token body: %w body=%s", err, compactMintBody([]byte(wrap.Body)))
			time.Sleep(1200 * time.Millisecond)
			continue
		}
		if wrap.Status == 200 && payload.AccessToken != "" && payload.RefreshToken != "" {
			return mintTokens{payload.AccessToken, payload.RefreshToken, payload.IDToken, payload.ExpiresIn}, nil
		}
		errCode := strings.TrimSpace(payload.Error)
		desc := strings.TrimSpace(payload.Description)
		lastErr = fmt.Errorf("%s %s", errCode, desc)
		mintLog(log, fmt.Sprintf("浏览器 token #%d：%s %s", attempt, errCode, desc))
		if isDeviceAuthDeniedCode(errCode, desc) {
			return mintTokens{}, lastErr
		}
		if errCode == "authorization_pending" || errCode == "slow_down" {
			wait := 1500 * time.Millisecond
			if errCode == "slow_down" {
				wait = 3 * time.Second
			}
			time.Sleep(wait)
			continue
		}
		break
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("浏览器 token 失败")
	}
	return mintTokens{}, lastErr
}

// evalAsyncJSON runs an async JS expression that returns a JSON string.
// chromedp may deliver Promise results as objects; normalize to string.
func evalAsyncJSON(browserCtx context.Context, asyncExpr string) (string, error) {
	// Wrap so the result is always a string even if the runtime returns an object.
	wrapped := `Promise.resolve((` + asyncExpr + `)).then((v) => {
  if (v == null) return '';
  if (typeof v === 'string') return v;
  try { return JSON.stringify(v); } catch (e) { return String(v); }
})`
	var raw interface{}
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(wrapped, &raw)); err != nil {
		return "", err
	}
	switch v := raw.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("eval result type %T: %v", raw, err)
		}
		// If we double-encoded a string, unwrap.
		var s string
		if json.Unmarshal(b, &s) == nil && (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) {
			return s, nil
		}
		return string(b), nil
	}
}

// browserSubmitFormTimed submits a form and waits up to timeout for navigation.
func browserSubmitFormTimed(browserCtx context.Context, action string, fields map[string]string, timeout time.Duration) error {
	if timeout < 3*time.Second {
		timeout = 3 * time.Second
	}
	var before string
	_ = chromedp.Run(browserCtx, chromedp.Location(&before))
	if err := browserSubmitForm(browserCtx, action, fields); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if browserCtx.Err() != nil {
			return browserCtx.Err()
		}
		var href string
		_ = chromedp.Run(browserCtx, chromedp.Location(&href))
		if href != "" && href != before {
			_ = chromedp.Run(browserCtx, chromedp.WaitReady("body", chromedp.ByQuery))
			return nil
		}
		// Also accept body changes without URL change.
		time.Sleep(300 * time.Millisecond)
	}
	// Timed out waiting for navigation — page may still have processed the POST.
	_ = chromedp.Run(browserCtx, chromedp.WaitReady("body", chromedp.ByQuery))
	return nil
}

// clickOAuthAllowRobust uses only browser mouse interaction. It intentionally
// does not rewrite form fields or submit a synthetic form.
func clickOAuthAllowRobust(browserCtx context.Context) error {
	if browserCtx == nil || browserCtx.Err() != nil {
		return fmt.Errorf("浏览器不可用")
	}

	// XPath exact labels — never match 全部允许.
	xpaths := []string{
		`//button[normalize-space(.)="允许"]`,
		`//button[normalize-space(.)="Allow"]`,
		`//button[normalize-space(.)="Authorize"]`,
		`//button[normalize-space(.)="Approve"]`,
		`//button[normalize-space(.)="授权"]`,
		`//button[normalize-space(.)="同意"]`,
		`//button[normalize-space(.)="Allow access"]`,
		`//*[@role="button" and normalize-space(.)="允许"]`,
		`//*[@role="button" and normalize-space(.)="Allow"]`,
		`//input[@type="submit" and (@value="允许" or @value="Allow" or @value="Authorize")]`,
	}
	for _, xp := range xpaths {
		var nodes []*cdp.Node
		if err := chromedp.Run(browserCtx, chromedp.Nodes(xp, &nodes, chromedp.BySearch)); err != nil {
			continue
		}
		for _, n := range nodes {
			if n == nil {
				continue
			}
			// Scroll + real node click (more reliable than raw coordinates for React).
			if err := chromedp.Run(browserCtx,
				chromedp.MouseClickNode(n),
			); err == nil {
				return nil
			}
		}
	}
	// Coordinate fallback.
	return realClickExact(browserCtx, oauthAllowLabels())
}

func warmBrowserSSO(browserCtx context.Context, sso string, log func(string)) error {
	sso = strings.TrimSpace(sso)
	if sso != "" {
		if err := injectBrowserSSO(browserCtx, sso); err != nil {
			mintLog(log, "注入 SSO cookie 警告："+err.Error())
		} else {
			mintLog(log, "已向浏览器注入 SSO cookie")
		}
	}
	// Stay on accounts.x.ai only (device consent host). Never follow redirects to
	// grok.com here — that can drop the accounts session used for consent.
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate("https://accounts.x.ai/"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return err
	}
	time.Sleep(700 * time.Millisecond)
	if sso != "" {
		_ = injectBrowserSSO(browserCtx, sso)
		// Re-navigate after cookie inject so accounts.x.ai sees the session.
		_ = chromedp.Run(browserCtx,
			chromedp.Navigate("https://accounts.x.ai/account"),
			chromedp.WaitReady("body", chromedp.ByQuery),
		)
		time.Sleep(800 * time.Millisecond)
	}
	var href string
	_ = chromedp.Run(browserCtx, chromedp.Location(&href))
	// If bounced to grok.com, re-inject and force accounts.x.ai again.
	if strings.Contains(href, "grok.com") {
		mintLog(log, "SSO 预热被重定向到 grok.com，强制回到 accounts.x.ai…")
		if sso != "" {
			_ = injectBrowserSSO(browserCtx, sso)
		}
		_ = chromedp.Run(browserCtx,
			chromedp.Navigate("https://accounts.x.ai/account"),
			chromedp.WaitReady("body", chromedp.ByQuery),
		)
		time.Sleep(800 * time.Millisecond)
		_ = chromedp.Run(browserCtx, chromedp.Location(&href))
	}
	if strings.Contains(href, "sign-in") || strings.Contains(href, "sign-up") {
		if sso != "" {
			_ = injectBrowserSSO(browserCtx, sso)
			_ = chromedp.Run(browserCtx, chromedp.Reload(), chromedp.WaitReady("body", chromedp.ByQuery))
			time.Sleep(800 * time.Millisecond)
			_ = chromedp.Run(browserCtx, chromedp.Location(&href))
		}
		if strings.Contains(href, "sign-in") || strings.Contains(href, "sign-up") {
			return fmt.Errorf("SSO 预热后仍在登录页 url=%s", shortEndpoint(href))
		}
	}
	mintLog(log, "SSO 预热完成 path="+shortEndpoint(href))
	return nil
}

func injectBrowserSSO(browserCtx context.Context, sso string) error {
	sso = strings.TrimSpace(sso)
	if sso == "" {
		return fmt.Errorf("empty sso")
	}
	// Prefer host-scoped cookies (more reliable than leading-dot alone on Chromium).
	domains := []string{".x.ai", "x.ai", "accounts.x.ai", ".accounts.x.ai", "auth.x.ai", ".auth.x.ai", "grok.com", ".grok.com"}
	exp := cdp.TimeSinceEpoch(time.Now().Add(24 * time.Hour))
	actions := []chromedp.Action{network.Enable()}
	for _, domain := range domains {
		d := domain
		actions = append(actions,
			chromedp.ActionFunc(func(ctx context.Context) error {
				return network.SetCookie("sso", sso).
					WithDomain(d).
					WithPath("/").
					WithSecure(true).
					WithHTTPOnly(true).
					WithExpires(&exp).
					Do(ctx)
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				return network.SetCookie("sso-rw", sso).
					WithDomain(d).
					WithPath("/").
					WithSecure(true).
					WithHTTPOnly(true).
					WithExpires(&exp).
					Do(ctx)
			}),
		)
	}
	return chromedp.Run(browserCtx, actions...)
}

// driveDeviceConsentUI follows hard rules:
//   - Cookie banner first: exact「全部允许」(never bare「允许」)
//   - OAuth consent: REAL mouse events on「允许」only when button is enabled
//   - Token poll (stopCh) is source of truth — URL /device/done alone is NOT success
//   - Network watch records approve/allow POSTs; page text is never enough
//   - Form POST only when SPA never produced a real allow click (form-after-click burns codes)
func driveDeviceConsentUI(browserCtx, parent context.Context, device deviceCode, log func(string), timeout time.Duration, stopCh <-chan struct{}, forceForm bool, pacer *tokenPollPacer, netWatch *consentNetWatch) error {
	deadline := time.Now().Add(timeout)
	consentURL := cpamint.DeviceConsentURL + "?user_code=" + url.QueryEscape(device.UserCode)
	clickedAllow := 0
	formTried := false
	sawAuthorizedText := false
	navConsentOnce := false
	noButtonSince := time.Now()
	if netWatch == nil {
		netWatch = newConsentNetWatch()
	}

	// Last-resort path only: lead with form (fresh device_code on this attempt).
	if forceForm {
		mintLog(log, "本轮以浏览器 form POST 优先（末次兜底）…")
		if err := browserFormDeviceAuthorize(browserCtx, device, log); err != nil {
			mintLog(log, "form 授权警告："+err.Error())
		} else {
			pacer.speedUp()
		}
		formTried = true
	}

	for time.Now().Before(deadline) {
		select {
		case <-stopCh:
			// Token poll succeeded (or was cancelled after success path).
			return nil
		default:
		}
		if err := parent.Err(); err != nil {
			return mintCancelErr(parent)
		}
		if err := browserCtx.Err(); err != nil {
			return fmt.Errorf("浏览器在授权过程中断开: %w", err)
		}

		var snap struct {
			URL   string `json:"url"`
			Text  string `json:"text"`
			Phase string `json:"phase"`
		}
		_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
const text=(document.body&&(document.body.innerText||document.body.textContent)||'').trim();
const url=location.href||'';
const low=text.toLowerCase();
let phase='wait';
// REAL success text only — bare /device/done SPA shell is "done_shell" (not success).
if(text.includes('设备已授权')||low.includes('device authorized')||low.includes('device has been authorized')) phase='authorized';
else if(url.includes('device/done')||url.includes('/oauth2/device/done')) phase='done_shell';
else if(url.includes('sign-in')||url.includes('sign-up')||low.includes('sign in to')||text.includes('登录到')||text.includes('登录您的')) phase='signin';
else if(text.includes('隐私偏好')||text.includes('全部允许')||/accept all cookies/i.test(text)) phase='cookie';
else if(url.includes('/consent')||text.includes('授权 Grok')||text.includes('Authorize Grok')||text.includes('Grok Build')||text.includes('请求访问')||text.includes('wants to access')) phase='consent';
else if(document.querySelector("input[name='user_code']")||(url.includes('/oauth2/device')&&!url.includes('consent')&&!url.includes('done'))) phase='device';
return {url, text: text.slice(0,800), phase};
})()`, &snap))

		switch snap.Phase {
		case "authorized":
			// Only trust strict visible marker (not i18n blobs in Next.js shells).
			if pageShowsDeviceAuthorized(browserCtx) {
				if !sawAuthorizedText {
					sawAuthorizedText = true
					pacer.speedUp()
					mintLog(log, "页面正文确认「设备已授权」url="+shortEndpoint(snap.URL)+"（等待 token 轮询）")
				}
			} else {
				// Phase detector saw a substring in the SPA bundle — treat as done_shell.
				snap.Phase = "done_shell"
			}
			if snap.Phase == "authorized" {
				if err := sleepConsent(parent, stopCh, 700*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			fallthrough
		case "done_shell":
			// SPA navigated to /done without authorized text.
			// If we already clicked allow, wait for token — do NOT form-POST (burns code).
			if clickedAllow > 0 {
				pacer.speedUp()
				if err := sleepConsent(parent, stopCh, 1200*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			if !formTried {
				mintLog(log, "仅 SPA /done 空壳且未点到「允许」→ form POST 真正授权")
				if err := browserFormDeviceAuthorize(browserCtx, device, log); err != nil {
					mintLog(log, "form 授权警告："+err.Error())
				} else {
					pacer.speedUp()
				}
				formTried = true
			}
			if err := sleepConsent(parent, stopCh, 1200*time.Millisecond); err != nil {
				return err
			}
			continue
		case "signin":
			return fmt.Errorf("授权页要求重新登录（SSO 未带到 accounts.x.ai）url=%s", snap.URL)
		case "cookie":
			if err := realClickExact(browserCtx, []string{"全部允许", "接受所有 cookie", "Accept all cookies", "Accept All", "Allow all"}); err == nil {
				mintLog(log, "已点击 Cookie「全部允许」")
				if err := sleepConsent(parent, stopCh, 700*time.Millisecond); err != nil {
					return err
				}
				continue
			}
		case "device":
			_ = fillUserCodeIfNeeded(browserCtx, device.UserCode)
			if err := realClickExact(browserCtx, []string{"继续", "Continue", "Next", "下一步"}); err == nil {
				mintLog(log, "已点击 device 页「继续」")
				if err := sleepConsent(parent, stopCh, 1000*time.Millisecond); err != nil {
					return err
				}
				continue
			}
		case "consent":
			// Cookie modal can overlay consent — never click 允许 under it.
			if strings.Contains(snap.Text, "隐私偏好") || strings.Contains(snap.Text, "全部允许") {
				_ = realClickExact(browserCtx, []string{"全部允许", "Accept all cookies", "Accept All"})
				if err := sleepConsent(parent, stopCh, 600*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			// Wait until an enabled OAuth allow control is actually present.
			if err := waitAllowButtonReady(browserCtx, parent, 8*time.Second, stopCh); err != nil {
				mintLog(log, "等待「允许」按钮："+err.Error())
			}
			if err := realClickExact(browserCtx, oauthAllowLabels()); err == nil {
				clickedAllow++
				beforeApprove := netWatch.approveCount()
				pacer.speedUp()
				mintLog(log, fmt.Sprintf("已真实点击 OAuth「允许」(#%d) — 等待网络 authorize + token（%s 内不做 form POST）", clickedAllow, postAllowTokenGrace))
				// Phase 1: short wait for network approve signal or token success.
				if err := sleepConsent(parent, stopCh, 5*time.Second); err != nil {
					return err
				}
				select {
				case <-stopCh:
					return nil
				default:
				}
				if netWatch.approveCount() > beforeApprove {
					mintLog(log, "网络层已观察到 approve/allow 请求，继续等 token…")
				} else if clickedAllow < 3 {
					// Still pending and no network approve — re-click once (common SPA miss).
					mintLog(log, "5s 内未见 authorize 网络请求，再次点击「允许」…")
					_ = realClickExact(browserCtx, oauthAllowLabels())
				}
				// Phase 2: remaining grace without form POST.
				remain := postAllowTokenGrace - 5*time.Second
				if remain < 3*time.Second {
					remain = 8 * time.Second
				}
				if err := sleepConsent(parent, stopCh, remain); err != nil {
					return err
				}
				select {
				case <-stopCh:
					return nil
				default:
				}
				continue
			}
			// Ensure hidden action=allow then real-click again.
			_ = ensureConsentFormAllow(browserCtx)
			if !navConsentOnce && !strings.Contains(snap.URL, "/consent") {
				navConsentOnce = true
				_ = chromedp.Run(browserCtx, chromedp.Navigate(consentURL), chromedp.WaitReady("body", chromedp.ByQuery))
				_ = waitDeviceUIReady(browserCtx, parent, 8*time.Second, stopCh)
			} else if !formTried && clickedAllow == 0 && time.Since(noButtonSince) > 18*time.Second {
				mintLog(log, "长时间未找到「允许」按钮，form POST 兜底…")
				if err := browserFormDeviceAuthorize(browserCtx, device, log); err != nil {
					mintLog(log, "form 授权警告："+err.Error())
				} else {
					pacer.speedUp()
				}
				formTried = true
			}
		default:
			_ = fillUserCodeIfNeeded(browserCtx, device.UserCode)
			_ = realClickExact(browserCtx, []string{"继续", "Continue", "Next", "下一步"})
			if waitAllowButtonReady(browserCtx, parent, 2*time.Second, stopCh) == nil {
				_ = realClickExact(browserCtx, oauthAllowLabels())
			}
			if clickedAllow == 0 && !navConsentOnce && time.Since(noButtonSince) > 6*time.Second {
				navConsentOnce = true
				_ = chromedp.Run(browserCtx, chromedp.Navigate(consentURL), chromedp.WaitReady("body", chromedp.ByQuery))
			} else if !formTried && clickedAllow == 0 && time.Since(noButtonSince) > 25*time.Second {
				mintLog(log, "页面相位不明且无点击，form POST 兜底…")
				if err := browserFormDeviceAuthorize(browserCtx, device, log); err != nil {
					mintLog(log, "form 授权警告："+err.Error())
				} else {
					pacer.speedUp()
				}
				formTried = true
			}
		}
		if err := sleepConsent(parent, stopCh, 400*time.Millisecond); err != nil {
			return err
		}
	}
	select {
	case <-stopCh:
		return nil
	default:
	}
	if sawAuthorizedText {
		return fmt.Errorf("页面已显示「设备已授权」但 token 轮询未成功（可能代理/token 端点异常）")
	}
	if clickedAllow > 0 || formTried {
		return fmt.Errorf("已尝试授权（点击=%d form=%v）但服务端未确认（勿信 SPA /done）", clickedAllow, formTried)
	}
	return fmt.Errorf("超时未完成 device-flow 授权（未点到「允许」）")
}

func oauthAllowLabels() []string {
	return []string{"允许", "Allow", "Authorize", "Approve", "Allow access", "授权", "同意"}
}

func sleepConsent(parent context.Context, stopCh <-chan struct{}, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-stopCh:
		return nil
	case <-parent.Done():
		return mintCancelErr(parent)
	case <-timer.C:
		return nil
	}
}

// browserFormDeviceAuthorize is the absolute auth.x.ai verify+approve path.
func browserFormDeviceAuthorize(browserCtx context.Context, device deviceCode, log func(string)) error {
	return browserAbsoluteDeviceAuthorize(browserCtx, device, log)
}

func pageShowsDeviceAuthorized(browserCtx context.Context) bool {
	// Extremely strict: only short heading-level nodes. Next.js shells embed i18n
	// strings and /done often shows "authorized" copy without a real grant.
	// Callers MUST still treat token poll as the only success criterion.
	var ok bool
	_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
const url = location.href || '';
if (!(url.includes('/device/done') || url.includes('/oauth2/device/done'))) return false;
const nodes = [...document.querySelectorAll('h1,h2,h3,[role="status"]')];
for (const n of nodes) {
  const t = (n.innerText||'').replace(/\s+/g,' ').trim();
  if (!t || t.length > 40) continue;
  const low = t.toLowerCase();
  if (t === '设备已授权' || low === 'device authorized' || low === 'device has been authorized') return true;
}
return false;
})()`, &ok))
	return ok
}

// consentNetWatch observes browser network traffic for device approve/allow signals.
// Token poll remains the only success criterion; this only guides re-click timing.
type consentNetWatch struct {
	mu       sync.Mutex
	approves int
	lastURL  string
}

func newConsentNetWatch() *consentNetWatch { return &consentNetWatch{} }

func (w *consentNetWatch) note(url string, status int64) {
	if w == nil {
		return
	}
	u := strings.ToLower(strings.TrimSpace(url))
	if u == "" || strings.Contains(u, "/oauth2/token") || strings.Contains(u, "device/code") {
		return
	}
	// Only real approve endpoints — never count SPA /consent page loads
	// (those produced false "网络层观察到 approve" and skipped real form work).
	isApprove := strings.Contains(u, "/oauth2/device/approve") ||
		strings.Contains(u, "device/approve") ||
		strings.Contains(u, "action=allow")
	if !isApprove {
		return
	}
	w.mu.Lock()
	w.approves++
	w.lastURL = url
	w.mu.Unlock()
}

func (w *consentNetWatch) sawApprove() bool { return w != nil && w.approveCount() > 0 }

func (w *consentNetWatch) approveCount() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.approves
}

func installConsentNetWatch(browserCtx context.Context, watch *consentNetWatch) error {
	if browserCtx == nil || watch == nil {
		return fmt.Errorf("nil watch")
	}
	return chromedp.Run(browserCtx,
		network.Enable(),
		chromedp.ActionFunc(func(ctx context.Context) error {
			chromedp.ListenTarget(ctx, func(ev interface{}) {
				switch e := ev.(type) {
				case *network.EventRequestWillBeSent:
					if e.Request == nil {
						return
					}
					u := e.Request.URL
					method := strings.ToUpper(e.Request.Method)
					if method == "POST" || method == "PUT" {
						watch.note(u, 0)
					}
				case *network.EventResponseReceived:
					if e.Response.URL == "" {
						return
					}
					watch.note(e.Response.URL, e.Response.Status)
				}
			})
			return nil
		}),
	)
}

func waitAllowButtonReady(browserCtx, parent context.Context, timeout time.Duration, stopCh <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-stopCh:
			return nil
		default:
		}
		if parent != nil && parent.Err() != nil {
			return mintCancelErr(parent)
		}
		if browserCtx != nil && browserCtx.Err() != nil {
			return browserCtx.Err()
		}
		var ready bool
		_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
const labels = ['允许','Allow','Authorize','Approve','授权','同意','Allow access'];
const nodes = [...document.querySelectorAll('button,input[type="submit"],a,[role="button"]')];
for (const n of nodes) {
  const t = (n.innerText||n.value||n.getAttribute('aria-label')||'').replace(/\s+/g,' ').trim();
  if (!t) continue;
  if (t.includes('全部允许') || t === 'Allow all' || /accept all cookies/i.test(t)) continue;
  const hit = labels.some(l => t === l || t.startsWith(l) || t.endsWith(l));
  if (!hit) continue;
  if (n.disabled || n.getAttribute('aria-disabled') === 'true') continue;
  const r = n.getBoundingClientRect();
  if (r.width < 2 || r.height < 2) continue;
  const style = getComputedStyle(n);
  if (style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity) === 0 || style.pointerEvents === 'none') continue;
  return true;
}
return false;
})()`, &ready))
		if ready {
			return nil
		}
		if err := sleepConsent(parent, stopCh, 300*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("未出现可点击的「允许」按钮")
}

func waitDeviceUIReady(browserCtx, parent context.Context, timeout time.Duration, stopCh <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-stopCh:
			return nil
		default:
		}
		if parent != nil && parent.Err() != nil {
			return mintCancelErr(parent)
		}
		if browserCtx.Err() != nil {
			return browserCtx.Err()
		}
		var ready bool
		_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
const text=(document.body&&(document.body.innerText||document.body.textContent)||'');
if (/全部允许|Accept all cookies|继续|Continue|允许|Allow|Authorize|Grok Build|user_code/i.test(text)) return true;
if (document.querySelector("input[name='user_code']")) return true;
const btns=[...document.querySelectorAll('button,input[type="submit"],[role="button"]')];
return btns.some(b => {
  const t=(b.innerText||b.value||'').replace(/\s+/g,' ').trim();
  return t==='允许'||t==='Allow'||t==='继续'||t==='Continue'||t==='全部允许';
});
})()`, &ready))
		if ready {
			return nil
		}
		if err := sleepConsent(parent, stopCh, 350*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("等待可交互控件超时")
}

func fillUserCodeIfNeeded(browserCtx context.Context, userCode string) error {
	userCode = strings.TrimSpace(userCode)
	if userCode == "" || browserCtx == nil || browserCtx.Err() != nil {
		return fmt.Errorf("skip")
	}
	codeJSON, _ := json.Marshal(userCode)
	var filled bool
	err := chromedp.Run(browserCtx, chromedp.Evaluate(fmt.Sprintf(`(() => {
const code = %s;
const input = document.querySelector("input[name='user_code'], input#user_code, input[autocomplete='one-time-code']");
if (!input) return false;
const cur = (input.value || '').trim();
if (cur === code) return false;
const native = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
if (native) native.call(input, code); else input.value = code;
input.dispatchEvent(new Event('input', {bubbles:true}));
input.dispatchEvent(new Event('change', {bubbles:true}));
return true;
})()`, string(codeJSON)), &filled))
	if err != nil {
		return err
	}
	if filled {
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

func browserSubmitForm(browserCtx context.Context, action string, fields map[string]string) error {
	// Full-page form navigation (no CORS). Credentials/cookies go with the browser.
	fieldJSON, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	actionJSON, err := json.Marshal(action)
	if err != nil {
		return err
	}
	script := fmt.Sprintf(`(() => {
const action = %s;
const fields = %s;
const form = document.createElement('form');
form.method = 'POST';
form.action = action;
form.style.display = 'none';
for (const [k, v] of Object.entries(fields)) {
  const input = document.createElement('input');
  input.type = 'hidden';
  input.name = k;
  input.value = v == null ? '' : String(v);
  form.appendChild(input);
}
document.body.appendChild(form);
form.submit();
return true;
})()`, string(actionJSON), string(fieldJSON))
	return chromedp.Run(browserCtx, chromedp.Evaluate(script, nil))
}

func ensureConsentFormAllow(browserCtx context.Context) error {
	return chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
const forms = Array.from(document.querySelectorAll('form'));
const f = forms.find((x) => {
  const t = (x.innerText || '');
  return t.includes('Grok Build') || t.includes('允许') || t.includes('Allow') || t.includes('Authorize');
}) || null;
if (!f) return false;
const ft = (f.innerText || '');
if (ft.includes('隐私偏好') || ft.includes('全部允许') || /cookie/i.test(ft)) return false;
let a = f.querySelector('input[name=action]');
if (!a) {
  a = document.createElement('input');
  a.type = 'hidden';
  a.name = 'action';
  f.appendChild(a);
}
a.value = 'allow';
return true;
})()`, nil))
}

// realClickExact finds a visible control by exact/near-exact label and dispatches
// real mouse move→press→release without rewriting or submitting page forms.
func realClickExact(browserCtx context.Context, labels []string) error {
	if len(labels) == 0 {
		return fmt.Errorf("no labels")
	}
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, fmt.Sprintf("%q", l))
	}
	script := fmt.Sprintf(`(() => {
const want=[%s];
const wantAllow = want.some(w => w==='允许' || w==='Allow' || w==='Authorize' || w==='Approve' || w==='授权' || w==='同意' || w==='Allow access');
const wantCookie = want.some(w => w.includes('全部允许') || /accept all cookies/i.test(w));
const nodes=[...document.querySelectorAll('button,input[type="submit"],a,[role="button"]')];
const match = (t) => {
  if (!t) return false;
  if (want.includes(t)) return true;
  for (const w of want) {
    if (!w) continue;
    if (t === w || t.startsWith(w+' ') || t.endsWith(' '+w)) return true;
    // Exact short labels only — do not substring-match 允许 inside 全部允许.
    if (t === w) return true;
  }
  return false;
};
const score = (n, t) => {
  let s = 0;
  const root = (n.closest('main,form,section,article,[role="dialog"],[data-testid]') || n.parentElement || n);
  const ctx = ((root && (root.innerText||'')) || '').slice(0, 500);
  if (/Grok|Grok Build|device|授权|访问|access your/i.test(ctx)) s += 5;
  if (/隐私|cookie|Cookie|全部允许/i.test(ctx) && wantAllow && !wantCookie) s -= 8;
  if (t === '允许' || t === 'Allow' || t === 'Authorize') s += 3;
  // Prefer buttons inside a form that already has action=allow.
  const form = n.closest('form');
  if (form) {
    const a = form.querySelector('input[name="action"]');
    if (a && String(a.value||'').toLowerCase() === 'allow') s += 4;
  }
  return s;
};
let best = null;
let bestScore = -999;
for (const n of nodes) {
  const t=(n.innerText||n.value||n.getAttribute('aria-label')||n.textContent||'').replace(/\s+/g,' ').trim();
  if (!match(t)) continue;
  if (wantAllow && !wantCookie && (t === '全部允许' || t.includes('全部允许') || t === 'Allow all' || t === 'Allow All' || /accept all cookies/i.test(t))) continue;
  if (n.disabled || n.getAttribute('aria-disabled')==='true') continue;
  try { n.scrollIntoView({block:'center', inline:'center'}); } catch(e) {}
  const r=n.getBoundingClientRect();
  if (r.width<2||r.height<2) continue;
  if (r.bottom<0||r.right<0||r.top>innerHeight||r.left>innerWidth) continue;
  const style=getComputedStyle(n);
  if (style.display==='none'||style.visibility==='hidden'||Number(style.opacity)===0||style.pointerEvents==='none') continue;
  const sc = score(n, t);
  if (sc > bestScore) {
    bestScore = sc;
    best = {X:r.left+r.width/2, Y:r.top+r.height/2, T:t, S:sc};
  }
}
if (best && wantAllow && !wantCookie && best.S < 0) return null;
// Focus the best node so React sees a real activation path.
if (best) {
  for (const n of nodes) {
    const t=(n.innerText||n.value||n.getAttribute('aria-label')||'').replace(/\s+/g,' ').trim();
    if (t !== best.T) continue;
    try { n.focus(); } catch(e) {}
    break;
  }
}
return best;
})()`, strings.Join(parts, ","))
	var point struct {
		X float64 `json:"X"`
		Y float64 `json:"Y"`
		T string  `json:"T"`
		S float64 `json:"S"`
	}
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(script, &point)); err != nil {
		return err
	}
	if point.X <= 0 || point.Y <= 0 || point.T == "" {
		return fmt.Errorf("button not found")
	}
	// Re-read geometry immediately before click (SPA may re-layout).
	var point2 struct {
		X float64 `json:"X"`
		Y float64 `json:"Y"`
	}
	_ = chromedp.Run(browserCtx, chromedp.Evaluate(fmt.Sprintf(`(() => {
const want = %q;
const nodes=[...document.querySelectorAll('button,input[type="submit"],a,[role="button"]')];
for (const n of nodes) {
  const t=(n.innerText||n.value||n.getAttribute('aria-label')||'').replace(/\s+/g,' ').trim();
  if (t !== want) continue;
  try { n.scrollIntoView({block:'center', inline:'center'}); } catch(e) {}
  const r=n.getBoundingClientRect();
  if (r.width<2||r.height<2) continue;
  return {X:r.left+r.width/2, Y:r.top+r.height/2};
}
return {X:0,Y:0};
})()`, point.T), &point2))
	if point2.X > 0 && point2.Y > 0 {
		point.X, point.Y = point2.X, point2.Y
	}
	// Short precise mouse sequence — long bezier paths miss re-rendered SPA buttons.
	if err := chromedp.Run(browserCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			if err := input.DispatchMouseEvent(input.MouseMoved, point.X, point.Y).Do(ctx); err != nil {
				return err
			}
			time.Sleep(50 * time.Millisecond)
			if err := input.DispatchMouseEvent(input.MousePressed, point.X, point.Y).
				WithButton(input.Left).
				WithClickCount(1).
				Do(ctx); err != nil {
				return err
			}
			time.Sleep(40 * time.Millisecond)
			return input.DispatchMouseEvent(input.MouseReleased, point.X, point.Y).
				WithButton(input.Left).
				WithClickCount(1).
				Do(ctx)
		}),
	); err != nil {
		return err
	}
	return nil
}

func writeCPAAuth(authDir, email string, tokens mintTokens) (string, error) {
	auth, err := cpamint.BuildAuthFile(email, tokens.AccessToken, tokens.RefreshToken, tokens.IDToken, tokens.ExpiresIn, cpamint.DefaultBaseURL)
	if err != nil {
		return "", err
	}
	path, _, err := cpamint.WriteAuthFile(authDir, auth)
	return filepath.Clean(path), err
}

func mintLog(log func(string), message string) {
	if log == nil {
		return
	}
	logStage(log, stageMint, message)
}

func mintCancelErr(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("铸造已取消")
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("铸造超时: %w", ctx.Err())
	}
	return fmt.Errorf("铸造已取消: %w", ctx.Err())
}

func browserMintErr(browser *browserSession, err error, step string) error {
	if err == nil {
		return nil
	}
	if browser != nil && browser.ctx != nil && browser.ctx.Err() != nil {
		return fmt.Errorf("%s 失败：浏览器/CDP 已断开（%v）: %w", step, browser.ctx.Err(), err)
	}
	return fmt.Errorf("%s 失败: %w", step, err)
}

func compactMintBody(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if s == "" {
		return "(empty)"
	}
	if len(s) > mintBodyLogLimit {
		return s[:mintBodyLogLimit] + "…"
	}
	return s
}

func shortEndpoint(endpoint string) string {
	if i := strings.LastIndex(endpoint, "/"); i >= 0 && i+1 < len(endpoint) {
		return endpoint[i+1:]
	}
	return endpoint
}

func secondsUntil(deadline time.Time) int {
	sec := int(time.Until(deadline).Seconds())
	if sec < 0 {
		return 0
	}
	return sec
}
