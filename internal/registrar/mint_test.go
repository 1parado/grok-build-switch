package registrar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMintContextIndependentOfBrowserCancel(t *testing.T) {
	jobCtx, jobCancel := context.WithCancel(context.Background())
	defer jobCancel()

	browserCtx, browserCancel := context.WithCancel(context.Background())
	browserCancel() // simulate Chrome/CDP disconnect after SSO
	if browserCtx.Err() == nil {
		t.Fatal("expected browser context canceled")
	}

	mintCtx, mintCancel := mintContext(jobCtx)
	defer mintCancel()
	if err := mintCtx.Err(); err != nil {
		t.Fatalf("mint context should survive browser cancel: %v", err)
	}

	jobCancel()
	select {
	case <-mintCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("mint context should cancel when job is stopped")
	}
}

func TestMintBrowserContextCancelsWhenMintExpires(t *testing.T) {
	mintCtx, cancelMint := context.WithCancel(context.Background())
	defer cancelMint()
	browserCtx, cancelBrowser := context.WithCancel(context.Background())
	defer cancelBrowser()

	ctx, cancel := mintBrowserContext(mintCtx, browserCtx)
	defer cancel()
	cancelMint()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("browser context was not cancelled by mint context")
	}
}

func TestExchangeDeviceTokenOneShot(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// First call: brief pending race; second: tokens (no long poll).
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "a",
			"refresh_token": "r",
			"id_token":      "i",
			"expires_in":    120,
		})
	}))
	defer server.Close()

	browserCtx, browserCancel := context.WithCancel(context.Background())
	defer browserCancel()
	mintCtx, mintCancel := mintContext(context.Background())
	defer mintCancel()

	var logs []string
	go func() {
		time.Sleep(50 * time.Millisecond)
		browserCancel()
	}()

	start := time.Now()
	tokens, err := exchangeDeviceToken(mintCtx, rewriteClient(server.URL), deviceCode{
		DeviceCode: "d",
		UserCode:   "U-1",
		ExpiresIn:  1800,
		Interval:   5,
	}, func(msg string) { logs = append(logs, msg) })
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "a" || tokens.RefreshToken != "r" {
		t.Fatalf("tokens = %#v", tokens)
	}
	if browserCtx.Err() == nil {
		t.Fatal("expected browser context canceled during exchange")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("exchange took too long (must not poll expires_in): %s", elapsed)
	}
	if calls.Load() > int32(tokenExchangeAttempts) {
		t.Fatalf("too many token calls: %d", calls.Load())
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "成功") {
		t.Fatalf("expected success log, got: %s", joined)
	}
}

func TestRequestDeviceParsesErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad client"}`))
	}))
	defer server.Close()

	_, err := requestDevice(context.Background(), rewriteClient(server.URL))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("error = %v", err)
	}
}

func TestExchangeDeviceTokenNoLongPollOnPending(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
	}))
	defer server.Close()

	start := time.Now()
	_, err := exchangeDeviceToken(context.Background(), rewriteClient(server.URL), deviceCode{
		DeviceCode: "d",
		UserCode:   "U",
		ExpiresIn:  1800,
		Interval:   5,
	}, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected pending failure")
	}
	if !strings.Contains(err.Error(), "未授权") && !strings.Contains(err.Error(), "authorization_pending") && !strings.Contains(err.Error(), "轮询") {
		t.Fatalf("error = %v", err)
	}
	maxAttempts := tokenExchangeAttempts + 8
	if calls.Load() > int32(maxAttempts) {
		t.Fatalf("calls = %d want <= %d", calls.Load(), maxAttempts)
	}
	// Short pending retries only — must not approach expires_in minutes.
	if elapsed > 45*time.Second {
		t.Fatalf("must not long-poll: elapsed %s", elapsed)
	}
}

func TestBrowserAlive(t *testing.T) {
	if browserAlive(nil) {
		t.Fatal("nil browser should be dead")
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &browserSession{ctx: ctx}
	if !browserAlive(session) {
		t.Fatal("live browser should be alive")
	}
	cancel()
	if browserAlive(session) {
		t.Fatal("canceled browser should be dead")
	}
}

func TestMintFromSSODeadBrowserFailsWithoutProtocolFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := &browserSession{ctx: ctx}
	// Dead browser must fail locally without attempting protocol form submission.
	_, _, err := mintFromSSO(context.Background(), session, "sso", "", false, false, nil)
	if err == nil {
		t.Fatal("expected browser unavailable failure")
	}
	if !strings.Contains(err.Error(), "浏览器会话不可用") {
		t.Fatalf("error = %v", err)
	}
}

func TestPollDeviceTokenStopsImmediatelyOnAccessDenied(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "Access denied",
		})
	}))
	defer server.Close()

	_, err := pollDeviceToken(context.Background(), rewriteClient(server.URL), deviceCode{
		DeviceCode: "denied-device",
		UserCode:   "DENIED",
		Interval:   1,
	}, nil, 30*time.Second, time.Second, 12, nil)
	if err == nil || !isDeviceAuthDenied(err) {
		t.Fatalf("expected terminal device denial, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("denied grant must stop after one token response, calls=%d", got)
	}
}

func TestTokenPollPacerSpeedUp(t *testing.T) {
	p := &tokenPollPacer{interval: 5 * time.Second}
	if p.get() != 5*time.Second {
		t.Fatalf("get = %s", p.get())
	}
	p.speedUp()
	if p.get() != 1000*time.Millisecond {
		t.Fatalf("after speedUp get = %s", p.get())
	}
	// Second speedUp must not go below floor.
	p.speedUp()
	if p.get() != 1000*time.Millisecond {
		t.Fatalf("floor broken: %s", p.get())
	}
}

func TestPollIntervalForDevice(t *testing.T) {
	if got := pollIntervalForDevice(deviceCode{Interval: 5}); got != 5*time.Second {
		t.Fatalf("got %s", got)
	}
	if got := pollIntervalForDevice(deviceCode{Interval: 0}); got != 3*time.Second {
		t.Fatalf("default got %s", got)
	}
}

func TestIsCloudflareChallengeIgnoresNextSPA(t *testing.T) {
	// Real accounts.x.ai device page is a Next.js shell that may mention cloudflare in assets.
	spa := strings.ToLower(`<!DOCTYPE html><html lang="en"><head><link href="/_next/static/chunks/34.js">cloudflare insights</head><body>创建您的账户</body></html>`)
	if isCloudflareChallenge(spa, 200) {
		t.Fatal("Next.js SPA must not be treated as Cloudflare challenge")
	}
	if !isCloudflareChallenge("attention required! | cloudflare just a moment", 403) {
		t.Fatal("real CF interstitial should be detected")
	}
}

func TestBodyHasDeviceAuthorizedStrict(t *testing.T) {
	spa := strings.ToLower(`<!DOCTYPE html><html><head><link href="/_next/static/chunks/34.js"></head><body>consent authorize route</body></html>`)
	if bodyHasDeviceAuthorized(spa) {
		t.Fatal("SPA shell with loose 'authorize' must NOT count as authorized")
	}
	if !bodyHasDeviceAuthorized(strings.ToLower("设备已授权 你可以关闭此窗口")) {
		t.Fatal("expected 设备已授权 marker")
	}
	if !bodyHasDeviceAuthorized("device authorized successfully") {
		t.Fatal("expected device authorized marker")
	}
}

func TestPostDeviceApproveAcceptsSPADoneAsProvisional(t *testing.T) {
	// Real xAI approve often ends on accounts SPA HTML even when authorize succeeded.
	// That must be provisional OK; token exchange decides.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="en" class="inter_b2991b2-module__9mH_6q__variable antialiased"><head><link rel="stylesheet" href="/_next/static/chunks/34.js"/></head><body></body></html>`))
	}))
	defer server.Close()

	err := postDeviceAction(context.Background(), rewriteClient(server.URL), server.URL+"/oauth2/device/approve", url.Values{
		"user_code": {"ABCD-EFGH"},
		"action":    {"allow"},
	}, nil)
	if err != nil {
		t.Fatalf("SPA HTML after approve should be provisional OK, got %v", err)
	}
}

func TestPostDeviceApproveRejectsInvalidCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`Invalid or expired code`))
	}))
	defer server.Close()
	err := postDeviceAction(context.Background(), rewriteClient(server.URL), server.URL+"/oauth2/device/approve", url.Values{
		"user_code": {"ABCD-EFGH"},
		"action":    {"allow"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "拒绝") && !strings.Contains(strings.ToLower(err.Error()), "invalid") {
		t.Fatalf("expected invalid code rejection, got %v", err)
	}
}

func TestBodyHasDeviceAuthorizedRejectsSPAShell(t *testing.T) {
	spa := strings.ToLower(`<!DOCTYPE html><html class="antialiased"><head><link href="/_next/static/chunks/34.js"/></head><body>device authorized successfully 设备已授权</body></html>`)
	if bodyHasDeviceAuthorized(spa) {
		t.Fatal("SPA shell with i18n phrases must NOT count as authorized")
	}
}

func rewriteClient(target string) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			out, err := http.NewRequestWithContext(req.Context(), req.Method, target, req.Body)
			if err != nil {
				return nil, err
			}
			out.Header = req.Header.Clone()
			return http.DefaultTransport.RoundTrip(out)
		}),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
