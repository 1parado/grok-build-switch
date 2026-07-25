package registrar

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Protocol registration talks to accounts.x.ai over HTTP/gRPC-web for the email
// verification steps (inspired by grok-build-auth xconsole_client), then completes
// profile / Turnstile / SSO in the browser when needed.

const (
	accountsOrigin     = "https://accounts.x.ai"
	protocolSignupURL  = "https://accounts.x.ai/sign-up?redirect=grok-com"
	rpcCreateEmailCode = "https://accounts.x.ai/auth_mgmt.AuthManagement/CreateEmailValidationCode"
	rpcVerifyEmailCode = "https://accounts.x.ai/auth_mgmt.AuthManagement/VerifyEmailValidationCode"
	connectESVersion   = "connect-es/2.1.1"
)

var (
	reNextAction = regexp.MustCompile(`(?i)"?next-action"?\s*[:=]\s*"([a-f0-9]{20,})"`)
	reSiteKey    = regexp.MustCompile(`(?i)(?:sitekey|data-sitekey)\s*[:=]\s*["'](0x[0-9A-Za-z_-]+)["']`)
)

type protocolClient struct {
	http       *http.Client
	jar        http.CookieJar
	signupURL  string
	userAgent  string
	nextAction string
	siteKey    string
}

func newProtocolClient(proxy string) (*protocolClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client, err := registrarHTTPClient(proxy)
	if err != nil {
		return nil, err
	}
	client.Jar = jar
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}
	return &protocolClient{
		http:      client,
		jar:       jar,
		signupURL: protocolSignupURL,
		userAgent: chromeUserAgent,
	}, nil
}

func (c *protocolClient) loadSignupPage(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.signupURL, nil)
	if err != nil {
		return err
	}
	c.applyBrowserHeaders(req, "")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("打开注册页: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	html := string(body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("注册页 HTTP %d", resp.StatusCode)
	}
	if m := reNextAction.FindStringSubmatch(html); len(m) > 1 {
		c.nextAction = m[1]
	}
	if m := reSiteKey.FindStringSubmatch(html); len(m) > 1 {
		c.siteKey = m[1]
	}
	// Managed challenge pages rarely expose next-action; still OK for gRPC email RPCs.
	return nil
}

func (c *protocolClient) createEmailValidationCode(ctx context.Context, email string) error {
	return c.grpcStringCall(ctx, rpcCreateEmailCode, []protoStringField{{1, email}})
}

func (c *protocolClient) verifyEmailValidationCode(ctx context.Context, email, code string) error {
	return c.grpcStringCall(ctx, rpcVerifyEmailCode, []protoStringField{
		{1, email},
		{2, code},
	})
}

type protoStringField struct {
	num   int
	value string
}

func (c *protocolClient) grpcStringCall(ctx context.Context, endpoint string, fields []protoStringField) error {
	payload := encodeProtoStrings(fields)
	frame := encodeGRPCWebFrame(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(frame))
	if err != nil {
		return err
	}
	c.applyBrowserHeaders(req, c.signupURL)
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("X-Grpc-Web", "1")
	req.Header.Set("X-User-Agent", connectESVersion)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", accountsOrigin)
	req.Header.Set("Referer", c.signupURL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("协议接口 HTTP %d（代理/环境可能被拦）", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("协议接口 HTTP %d", resp.StatusCode)
	}
	status, message := parseGRPCWebStatus(raw)
	if status != 0 {
		if message == "" {
			message = fmt.Sprintf("grpc-status=%d", status)
		}
		return fmt.Errorf("协议调用失败: %s", message)
	}
	return nil
}

func (c *protocolClient) applyBrowserHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("sec-ch-ua", `"Chromium";v="131", "Not_A Brand";v="24", "Google Chrome";v="131"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	}
}

func encodeProtoStrings(fields []protoStringField) []byte {
	var out []byte
	for _, field := range fields {
		out = append(out, encodeProtoString(field.num, field.value)...)
	}
	return out
}

func encodeProtoString(fieldNo int, text string) []byte {
	raw := []byte(text)
	tag := encodeVarint(uint64(fieldNo<<3 | 2))
	length := encodeVarint(uint64(len(raw)))
	out := make([]byte, 0, len(tag)+len(length)+len(raw))
	out = append(out, tag...)
	out = append(out, length...)
	out = append(out, raw...)
	return out
}

func encodeVarint(value uint64) []byte {
	var out []byte
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	out = append(out, byte(value))
	return out
}

func encodeGRPCWebFrame(message []byte) []byte {
	// flag(0) + big-endian length + protobuf payload
	frame := make([]byte, 5+len(message))
	frame[0] = 0
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(message)))
	copy(frame[5:], message)
	return frame
}

func parseGRPCWebStatus(raw []byte) (int, string) {
	// Default OK when empty body (some endpoints return only trailers).
	if len(raw) == 0 {
		return 0, ""
	}
	i := 0
	grpcStatus := 0
	grpcMessage := ""
	for i+5 <= len(raw) {
		flag := raw[i]
		length := int(binary.BigEndian.Uint32(raw[i+1 : i+5]))
		i += 5
		if length < 0 || i+length > len(raw) {
			break
		}
		payload := raw[i : i+length]
		i += length
		if flag&0x80 != 0 {
			// Trailer frame: HTTP/1 style headers.
			text := string(payload)
			for _, line := range strings.Split(text, "\r\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				lower := strings.ToLower(line)
				if strings.HasPrefix(lower, "grpc-status:") {
					fmt.Sscanf(strings.TrimSpace(line[len("grpc-status:"):]), "%d", &grpcStatus)
				}
				if strings.HasPrefix(lower, "grpc-message:") {
					grpcMessage = strings.TrimSpace(line[len("grpc-message:"):])
					if unesc, err := url.QueryUnescape(grpcMessage); err == nil {
						grpcMessage = unesc
					}
				}
			}
		}
	}
	return grpcStatus, grpcMessage
}

// registerWithProtocol runs email verification over gRPC-web, then finishes the
// account in a browser session (profile + Turnstile + SSO) using the same proxy.
// Full browserless create_account still needs a live Turnstile token provider.
func registerWithProtocol(parent context.Context, config Config, mailbox Mailbox, authDir string, log func(string)) (registrationOutcome, error) {
	proxy := strings.TrimSpace(config.ProxyURL)
	logStage(log, stageEmailSubmit, "协议路径：HTTP/gRPC-web 邮箱验证（代理 "+RedactProxy(proxy)+"）")

	client, err := newProtocolClient(proxy)
	if err != nil {
		return registrationOutcome{}, wrapStage(stageBrowserStart, err)
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(config.PageTimeoutSeconds)*time.Second)
	defer cancel()

	if err := client.loadSignupPage(ctx); err != nil {
		return registrationOutcome{}, wrapStage(stageOpenSignup, err)
	}
	logStage(log, stageOpenSignup, "协议注册页已加载")

	email := mailbox.Address()
	if err := client.createEmailValidationCode(ctx, email); err != nil {
		return registrationOutcome{}, regErr(stageEmailSubmit, "protocol_create_email_failed", err.Error(),
			"协议 CreateEmailValidationCode 失败，可换代理或改用浏览器注册", "")
	}
	logStage(log, stageEmailSubmit, "协议已请求邮箱验证码")

	code, err := waitMailboxCodeWithResend(
		ctx,
		mailbox,
		time.Duration(config.MailTimeoutSeconds)*time.Second,
		emailResendInterval,
		func(message string) {
			if log != nil {
				logStage(log, stageMailWait, message)
			}
		},
		func(resendCtx context.Context) (bool, error) {
			if err := client.createEmailValidationCode(resendCtx, email); err != nil {
				return false, err
			}
			return true, nil
		},
		log,
	)
	if err != nil {
		return registrationOutcome{}, regErr(stageMailWait, "protocol_mail_timeout", err.Error(),
			"确认临时邮箱与协议发信接口正常", "")
	}
	logStage(log, stageCodeSubmit, "协议路径已收到验证码")

	if err := client.verifyEmailValidationCode(ctx, email, code); err != nil {
		return registrationOutcome{}, regErr(stageCodeSubmit, "protocol_verify_failed", err.Error(),
			"验证码可能过期，或协议会话 cookie 不完整", "")
	}
	logStage(log, stageCodeSubmit, "协议邮箱验证通过，转浏览器完成资料/Turnstile")

	// Complete profile + Turnstile + SSO in browser; email step already done server-side
	// when cookies transfer successfully. Caller decides whether to fall back to a
	// full browser signup on failure.
	return completeProtocolInBrowser(parent, config, mailbox, authDir, client, log)
}

func browserHeadless(config Config) bool {
	return strings.EqualFold(strings.TrimSpace(config.BrowserMode), "headless")
}

func completeProtocolInBrowser(parent context.Context, config Config, mailbox Mailbox, authDir string, proto *protocolClient, log func(string)) (registrationOutcome, error) {
	session, err := startBrowser(parent, config, browserHeadless(config))
	if err != nil {
		return registrationOutcome{}, wrapStage(stageBrowserStart, err)
	}
	defer session.Close()
	ctx, cancel := context.WithTimeout(session.ctx, time.Duration(config.PageTimeoutSeconds)*time.Second)
	defer cancel()

	// Best-effort: inject cookies from the protocol jar so the browser inherits the session.
	if err := injectJarCookies(ctx, proto); err != nil {
		logStage(log, stageBrowserStart, "注入协议 cookie 失败："+err.Error())
	}

	logStage(log, stageOpenSignup, "浏览器打开注册页以完成资料步骤")
	if err := navigateSignupWithRetry(ctx, config.ProxyURL, log); err != nil {
		return registrationOutcome{}, err
	}
	if err := waitForChallengeClear(ctx, session, 60*time.Second, log); err != nil {
		return registrationOutcome{}, err
	}

	// If email form is still shown, fill with the same address (session may not have transferred).
	if err := submitEmailIfNeeded(ctx, session, mailbox.Address(), log); err != nil {
		logStage(log, stageEmailSubmit, "协议 cookie 未续上邮箱步骤，浏览器补提交："+err.Error())
	}

	// Prefer waiting for profile form or early SSO.
	if earlySSO, ssoErr := waitForSSOCookie(ctx, 8*time.Second); ssoErr == nil && earlySSO != "" {
		logStage(log, stageSSO, "协议+浏览器路径已拿到 SSO")
		return finalizeRegistration(parent, session, config, mailbox, earlySSO, "", authDir, log)
	}

	given, family, password, err := randomProfile()
	if err != nil {
		return registrationOutcome{}, wrapStage(stageProfile, err)
	}
	logStage(log, stageProfile, "填写注册资料并处理 Turnstile")
	if err := fillProfileAndSubmit(ctx, given, family, password); err != nil {
		// Profile may not be ready — fall back to full browser registration.
		return registrationOutcome{}, err
	}
	sso, err := waitForSSOCookie(ctx, 120*time.Second)
	if err != nil {
		return registrationOutcome{}, err
	}
	return finalizeRegistration(parent, session, config, mailbox, sso, password, authDir, log)
}

func injectJarCookies(ctx context.Context, proto *protocolClient) error {
	if proto == nil || proto.jar == nil {
		return nil
	}
	targets := []string{
		"https://accounts.x.ai/",
		"https://auth.x.ai/",
		"https://grok.com/",
	}
	for _, raw := range targets {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		for _, cookie := range proto.jar.Cookies(u) {
			domain := cookie.Domain
			if domain == "" {
				domain = u.Hostname()
			}
			path := cookie.Path
			if path == "" {
				path = "/"
			}
			name, value := cookie.Name, cookie.Value
			secure := cookie.Secure || strings.HasPrefix(raw, "https")
			httpOnly := cookie.HttpOnly
			if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				return network.SetCookie(name, value).
					WithDomain(domain).
					WithPath(path).
					WithHTTPOnly(httpOnly).
					WithSecure(secure).
					Do(ctx)
			})); err != nil {
				return err
			}
		}
	}
	return nil
}

func submitEmailIfNeeded(ctx context.Context, session *browserSession, email string, log func(string)) error {
	// Detect whether an email field is still visible; if so, run the normal browser email path.
	var needsEmail bool
	_ = chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`(() => {
		const input = document.querySelector('input[type="email"], input[name*="email" i], input[autocomplete="email"]');
		return !!(input && input.offsetParent !== null);
	})()`, &needsEmail))
	if !needsEmail {
		return nil
	}
	logStage(log, stageEmailSignup, "浏览器仍需邮箱步骤，继续页面流程")
	if err := clickEmailSignup(ctx, 15*time.Second); err != nil {
		// Button may already be on email form.
		_ = err
	}
	return submitEmailWithRetries(ctx, session, email, log)
}
