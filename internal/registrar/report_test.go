package registrar

import (
	"strings"
	"testing"
)

func TestRegistrationErrorFormat(t *testing.T) {
	err := regErr(stageCFChallenge, "cf_timeout",
		"在 90s 内未能自动通过 Cloudflare 验证",
		"换代理节点",
		"状态=challenge · token长度=0")
	msg := err.Error()
	for _, needle := range []string{
		"[Cloudflare 人机验证]",
		"原因码: cf_timeout",
		"详情:",
		"建议:",
		"token长度=0",
	} {
		if !strings.Contains(msg, needle) {
			t.Fatalf("error missing %q: %s", needle, msg)
		}
	}
}

func TestAggregateJobFailures(t *testing.T) {
	results := []AccountResult{
		{Email: "a@x", Status: "failed", Error: "[Cloudflare 人机验证] 超时（原因码: cf_timeout） | 详情: x | 建议: y"},
		{Email: "b@x", Status: "failed", Error: "[Cloudflare 人机验证] 超时（原因码: cf_timeout） | 详情: z | 建议: y"},
		{Email: "c@x", Status: "failed", Error: "[提交邮箱] API 被拦（原因码: email_api_blocked）"},
		{Email: "d@x", Status: "success"},
	}
	got := aggregateJobFailures(results)
	if !strings.Contains(got, "没有账号注册成功：") {
		t.Fatalf("prefix missing: %s", got)
	}
	if !strings.Contains(got, "×2") {
		t.Fatalf("expected collapsed count: %s", got)
	}
	if !strings.Contains(got, "email_api_blocked") && !strings.Contains(got, "API 被拦") {
		t.Fatalf("expected second failure class: %s", got)
	}
	// details/hints should be stripped from aggregation key
	if strings.Contains(got, "建议:") {
		t.Fatalf("aggregation should drop 建议 tails: %s", got)
	}
}

func TestPageSnapshotSummary(t *testing.T) {
	snap := pageSnapshot{
		State:        "challenge",
		Title:        "Just a moment...",
		URL:          "https://accounts.x.ai/sign-up",
		HTTPStatus:   403,
		TokenLen:     0,
		HasClearance: false,
		HasWidget:    true,
		WidgetKind:   "iframe:challenges.cloudflare.com",
		BodyHint:     "Verify you are human",
	}
	sum := snap.Summary()
	for _, needle := range []string{"状态=challenge", "HTTP=403", "token长度=0", "clearance=无", "控件=有"} {
		if !strings.Contains(sum, needle) {
			t.Fatalf("summary missing %q: %s", needle, sum)
		}
	}
}

func TestInteractionOutcomeLogLine(t *testing.T) {
	line := interactionOutcome{
		OK: false, Reason: "clicked_but_no_token", WidgetFound: true,
		WidgetKind: "iframe", TokenLen: 0, PageState: "challenge",
	}.LogLine(3)
	if !strings.Contains(line, "[CF]") || !strings.Contains(line, "未通过") || !strings.Contains(line, "#3") {
		t.Fatalf("unexpected log line: %s", line)
	}
}

func TestClassifyBrowserChallenge(t *testing.T) {
	err := classifyBrowserChallenge(stageCFChallenge, &browserChallengeError{
		message: "注册页卡在 Cloudflare 人机验证（Just a moment）",
	})
	re, ok := err.(*registrationError)
	if !ok {
		t.Fatalf("type %T", err)
	}
	if re.Code != "cf_stuck" {
		t.Fatalf("code = %s", re.Code)
	}
}

func TestStageLabel(t *testing.T) {
	if stageLabel(stageProfile) != "资料页 Turnstile" {
		t.Fatal(stageLabel(stageProfile))
	}
}
