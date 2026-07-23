package registrar

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// Registration pipeline stages (used in logs and structured errors).
const (
	stageBrowserStart = "browser_start"
	stageOpenSignup   = "open_signup"
	stageCFChallenge  = "cf_challenge"
	stageEmailSignup  = "email_signup"
	stageEmailSubmit  = "email_submit"
	stageMailWait     = "mail_wait"
	stageCodeSubmit   = "code_submit"
	stageProfile      = "profile_turnstile"
	stageSSO          = "sso"
	stageMint         = "mint"
)

func stageLabel(stage string) string {
	switch stage {
	case stageBrowserStart:
		return "启动浏览器"
	case stageOpenSignup:
		return "打开注册页"
	case stageCFChallenge:
		return "Cloudflare 人机验证"
	case stageEmailSignup:
		return "选择邮箱注册"
	case stageEmailSubmit:
		return "提交邮箱"
	case stageMailWait:
		return "等待邮件验证码"
	case stageCodeSubmit:
		return "提交邮箱验证码"
	case stageProfile:
		return "资料页 Turnstile"
	case stageSSO:
		return "获取 SSO"
	case stageMint:
		return "CPA 铸造"
	default:
		if stage == "" {
			return "注册"
		}
		return stage
	}
}

// pageSnapshot is a point-in-time read of CF / signup page signals for logs and errors.
type pageSnapshot struct {
	State        string `json:"state"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	HTTPStatus   int64  `json:"http_status"`
	TokenLen     int    `json:"token_len"`
	HasClearance bool   `json:"has_clearance"`
	HasWidget    bool   `json:"has_widget"`
	WidgetKind   string `json:"widget_kind"`
	BodyHint     string `json:"body_hint"`
}

func (s pageSnapshot) Summary() string {
	widget := "无"
	if s.HasWidget {
		widget = "有"
		if s.WidgetKind != "" {
			widget += "(" + s.WidgetKind + ")"
		}
	}
	clearance := "无"
	if s.HasClearance {
		clearance = "有"
	}
	title := strings.TrimSpace(s.Title)
	if len(title) > 48 {
		title = title[:48] + "…"
	}
	hint := strings.TrimSpace(s.BodyHint)
	if len(hint) > 80 {
		hint = hint[:80] + "…"
	}
	parts := []string{
		fmt.Sprintf("状态=%s", firstNonEmpty(s.State, "unknown")),
		fmt.Sprintf("HTTP=%d", s.HTTPStatus),
		fmt.Sprintf("token长度=%d", s.TokenLen),
		fmt.Sprintf("clearance=%s", clearance),
		fmt.Sprintf("控件=%s", widget),
	}
	if title != "" {
		parts = append(parts, "title="+quoteShort(title))
	}
	if s.URL != "" {
		parts = append(parts, "url="+trimURL(s.URL))
	}
	if hint != "" {
		parts = append(parts, "页面摘要="+quoteShort(hint))
	}
	return strings.Join(parts, " · ")
}

func quoteShort(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.Join(strings.Fields(value), " ")
	return `"` + value + `"`
}

func trimURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 96 {
		return raw[:96] + "…"
	}
	return raw
}

func capturePageSnapshot(ctx context.Context, session *browserSession) pageSnapshot {
	var snap pageSnapshot
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
const text = (document.body && document.body.innerText || '').replace(/\s+/g, ' ').trim();
const title = document.title || '';
const href = location.href || '';
const low = (title + ' ' + text).toLowerCase();
let state = 'wait';
if (low.includes('just a moment') || low.includes('checking your browser') || low.includes('verify you are human') || low.includes('needs to review the security')) {
  state = 'challenge';
} else if (low.includes('403 forbidden') || low.includes('access denied') || low.includes('sorry, you have been blocked') || low.includes('attention required')) {
  state = 'blocked';
} else {
  const token = String((document.querySelector('input[name="cf-turnstile-response"]') || {}).value || '').trim();
  if (token.length >= 80) state = 'ready';
  else {
    const email = [...document.querySelectorAll('input[type="email"],input[name="email"],button,a')].some(n => n && n.getBoundingClientRect().width > 0);
    state = email ? 'ready' : 'wait';
  }
}
const tokenLen = String((document.querySelector('input[name="cf-turnstile-response"]') || {}).value || '').trim().length;
const widget = document.querySelector('iframe[src*="challenges.cloudflare.com"],iframe[src*="turnstile"],div.cf-turnstile,[data-sitekey],#cf-turnstile');
let widgetKind = '';
if (widget) {
  widgetKind = widget.tagName.toLowerCase();
  if (widget.getAttribute('src')) widgetKind += ':' + String(widget.getAttribute('src')).slice(0, 48);
  else if (widget.getAttribute('data-sitekey')) widgetKind += ':sitekey';
}
return {
  state,
  title,
  url: href,
  token_len: tokenLen,
  has_widget: !!widget,
  widget_kind: widgetKind,
  body_hint: text.slice(0, 120),
};
})()`, &snap))
	if session != nil {
		session.mu.Lock()
		snap.HTTPStatus = session.lastDocumentStatus
		session.mu.Unlock()
	}
	if ok, _ := hasCloudflareClearance(ctx); ok {
		snap.HasClearance = true
	}
	// Prefer live pageChallengeState when evaluate partially failed.
	if snap.State == "" {
		if state, err := pageChallengeState(ctx); err == nil {
			snap.State = state
		}
	}
	return snap
}

// registrationError is a user-facing failure with stage + reason + hint.
type registrationError struct {
	Stage   string
	Code    string
	Message string
	Hint    string
	Detail  string
}

func (e *registrationError) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(stageLabel(e.Stage))
	b.WriteString("] ")
	b.WriteString(strings.TrimSpace(e.Message))
	if e.Code != "" {
		b.WriteString("（原因码: ")
		b.WriteString(e.Code)
		b.WriteString("）")
	}
	if strings.TrimSpace(e.Detail) != "" {
		b.WriteString(" | 详情: ")
		b.WriteString(strings.TrimSpace(e.Detail))
	}
	if strings.TrimSpace(e.Hint) != "" {
		b.WriteString(" | 建议: ")
		b.WriteString(strings.TrimSpace(e.Hint))
	}
	return b.String()
}

func regErr(stage, code, message, hint, detail string) error {
	return &registrationError{
		Stage:   stage,
		Code:    code,
		Message: message,
		Hint:    hint,
		Detail:  detail,
	}
}

func regErrf(stage, code, hint, detail, format string, args ...any) error {
	return regErr(stage, code, fmt.Sprintf(format, args...), hint, detail)
}

func (e *registrationError) withDetail(detail string) *registrationError {
	if e == nil {
		return nil
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return e
	}
	if e.Detail == "" {
		e.Detail = detail
	} else if !strings.Contains(e.Detail, detail) {
		e.Detail = e.Detail + " · " + detail
	}
	return e
}

func wrapStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*registrationError); ok {
		return err
	}
	if be, ok := err.(*browserChallengeError); ok {
		return classifyBrowserChallenge(stage, be)
	}
	msg := err.Error()
	// context errors
	if strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "context canceled") {
		return regErr(stage, "timeout_or_cancel", msg, "检查页面超时设置、代理连通性，或停止后重试", "")
	}
	return regErr(stage, "error", msg, hintForMessage(msg), "")
}

func classifyBrowserChallenge(stage string, err *browserChallengeError) error {
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "just a moment") || strings.Contains(low, "人机验证"):
		return regErr(stageCFChallenge, "cf_stuck", msg,
			"代理 IP 可能被标记；可换节点、改用可见浏览器，或稍后再试", msg)
	case strings.Contains(low, "403") || strings.Contains(low, "access denied") || strings.Contains(low, "blocked"):
		return regErr(stageCFChallenge, "cf_blocked", msg,
			"当前 IP/环境被硬拦截，请更换代理或降低并发后重试", msg)
	case strings.Contains(low, "createemailvalidationcode"):
		return regErr(stageEmailSubmit, "email_api_blocked", msg,
			"邮箱验证码接口被拦，通常是 Turnstile 未通过；等待自动通过或检查代理后重试", msg)
	case strings.Contains(low, "turnstile"):
		return regErr(stageProfile, "turnstile_failed", msg,
			"资料页 Turnstile 未拿到有效 token，请确认可见窗口中验证控件可点击", msg)
	default:
		if stage == "" {
			stage = stageCFChallenge
		}
		return regErr(stage, "browser_challenge", msg, hintForMessage(msg), msg)
	}
}

func hintForMessage(msg string) string {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "未找到 chrome") || strings.Contains(low, "浏览器"):
		return "在注册设置中填写 Chrome/Edge 路径"
	case strings.Contains(low, "proxy") || strings.Contains(low, "代理"):
		return "检查代理地址与连通性"
	case strings.Contains(low, "验证码") && strings.Contains(low, "未收到"):
		return "检查临时邮箱 API/Hotmail 凭证，并确认 CreateEmailValidationCode 未被拦截"
	case strings.Contains(low, "sso"):
		return "注册可能未完成或资料页提交失败，查看此前 Cloudflare/Turnstile 日志"
	case strings.Contains(low, "turnstile") || strings.Contains(low, "cloudflare") || strings.Contains(low, "just a moment"):
		return "换干净代理、降低并发，优先可见浏览器；仍失败则稍后重试"
	default:
		return "查看完整任务日志定位阶段"
	}
}

func logCF(log func(string), event string, snap pageSnapshot) {
	if log == nil {
		return
	}
	log(fmt.Sprintf("[CF] %s | %s", event, snap.Summary()))
}

func logStage(log func(string), stage, message string) {
	if log == nil {
		return
	}
	log(fmt.Sprintf("[%s] %s", stageLabel(stage), message))
}

// interactionOutcome describes one auto-pass attempt for clearer logs.
type interactionOutcome struct {
	OK           bool
	Reason       string
	WidgetFound  bool
	WidgetKind   string
	TokenLen     int
	HasClearance bool
	PageState    string
	Err          error
}

func (o interactionOutcome) LogLine(attempt int) string {
	widget := "无控件"
	if o.WidgetFound {
		widget = "有控件"
		if o.WidgetKind != "" {
			widget += "(" + shortWidget(o.WidgetKind) + ")"
		}
	}
	status := "未通过"
	if o.OK {
		status = "通过"
	}
	reason := o.Reason
	if reason == "" {
		if o.OK {
			reason = "ok"
		} else {
			reason = "pending"
		}
	}
	line := fmt.Sprintf("[CF] 自动交互 #%d → %s · 原因=%s · %s · token长度=%d · clearance=%v · 页面=%s",
		attempt, status, reason, widget, o.TokenLen, o.HasClearance, firstNonEmpty(o.PageState, "?"))
	if o.Err != nil {
		line += " · 错误=" + o.Err.Error()
	}
	return line
}

func shortWidget(kind string) string {
	kind = strings.TrimSpace(kind)
	if len(kind) > 40 {
		return kind[:40] + "…"
	}
	return kind
}

func aggregateJobFailures(results []AccountResult) string {
	if len(results) == 0 {
		return "没有账号注册成功"
	}
	seen := map[string]int{}
	var order []string
	for _, r := range results {
		if r.Status == "success" {
			continue
		}
		msg := strings.TrimSpace(r.Error)
		if msg == "" {
			msg = "未知错误"
		}
		// Collapse near-identical long errors by stage prefix + code when present.
		key := simplifyFailureKey(msg)
		if _, ok := seen[key]; !ok {
			order = append(order, key)
		}
		seen[key]++
	}
	if len(order) == 0 {
		return "没有账号注册成功"
	}
	parts := make([]string, 0, len(order))
	for _, key := range order {
		if n := seen[key]; n > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", key, n))
		} else {
			parts = append(parts, key)
		}
	}
	// Keep job.Error readable in the progress line.
	joined := strings.Join(parts, "；")
	if len(joined) > 360 {
		joined = joined[:360] + "…"
	}
	return "没有账号注册成功：" + joined
}

func simplifyFailureKey(msg string) string {
	// Prefer "[阶段] 消息（原因码: x）" head without long 详情/建议 tails for aggregation.
	if i := strings.Index(msg, " | 详情:"); i > 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	if i := strings.Index(msg, " | 建议:"); i > 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	if len(msg) > 160 {
		return msg[:160] + "…"
	}
	return msg
}

// cfStateLabel maps internal state codes to Chinese for UI logs.
func cfStateLabel(state string) string {
	switch state {
	case "ready":
		return "已通过/可操作"
	case "challenge":
		return "需人机验证"
	case "blocked":
		return "被拦截"
	case "wait":
		return "页面加载中"
	default:
		return state
	}
}

func elapsedLabel(since time.Time) string {
	if since.IsZero() {
		return "0s"
	}
	return time.Since(since).Truncate(time.Second).String()
}
