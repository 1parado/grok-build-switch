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
	"time"

	"github.com/chromedp/chromedp"
	"grok_switch/internal/cpamint"
)

const (
	deviceVerifyURL  = "https://auth.x.ai/oauth2/device/verify"
	deviceApproveURL = "https://auth.x.ai/oauth2/device/approve"
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

func mintFromSSO(ctx context.Context, browser *browserSession, sso, proxy string, preferProtocol, protocolOnly bool, log func(string)) (mintTokens, string, error) {
	if preferProtocol || protocolOnly {
		tokens, err := mintProtocol(ctx, sso, proxy, log)
		if err == nil {
			return tokens, "protocol", nil
		}
		log("协议铸造失败：" + err.Error())
		if protocolOnly {
			return mintTokens{}, "", err
		}
	}
	tokens, err := mintBrowser(ctx, browser, proxy, log)
	if err != nil {
		return mintTokens{}, "", err
	}
	return tokens, "browser", nil
}

func mintProtocol(ctx context.Context, sso, proxy string, log func(string)) (mintTokens, error) {
	jar, _ := cookiejar.New(nil)
	client, err := registrarHTTPClient(proxy)
	if err != nil {
		return mintTokens{}, err
	}
	client.Jar = jar
	for _, host := range []string{"https://accounts.x.ai/", "https://auth.x.ai/", "https://grok.com/"} {
		u, _ := url.Parse(host)
		jar.SetCookies(u, []*http.Cookie{{Name: "sso", Value: sso, Path: "/"}, {Name: "sso-rw", Value: sso, Path: "/"}})
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://accounts.x.ai/", nil)
	if err != nil {
		return mintTokens{}, err
	}
	request.Header.Set("User-Agent", chromeUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return mintTokens{}, fmt.Errorf("验证 SSO: %w", err)
	}
	response.Body.Close()
	if strings.Contains(response.Request.URL.Path, "sign-in") || strings.Contains(response.Request.URL.Path, "sign-up") {
		return mintTokens{}, fmt.Errorf("SSO 已失效")
	}
	device, err := requestDevice(ctx, client)
	if err != nil {
		return mintTokens{}, err
	}
	log("协议 device code 已获取")
	if err := visitDevice(ctx, client, device); err != nil {
		return mintTokens{}, err
	}
	if err := postDeviceAction(ctx, client, deviceVerifyURL, url.Values{"user_code": {device.UserCode}}); err != nil {
		return mintTokens{}, fmt.Errorf("device verify: %w", err)
	}
	if err := postDeviceAction(ctx, client, deviceApproveURL, url.Values{
		"user_code": {device.UserCode}, "action": {"allow"}, "principal_type": {"User"}, "principal_id": {""},
	}); err != nil {
		return mintTokens{}, fmt.Errorf("device approve: %w", err)
	}
	return pollToken(ctx, client, device)
}

func requestDevice(ctx context.Context, client *http.Client) (deviceCode, error) {
	form := url.Values{"client_id": {cpamint.ClientID}, "scope": {cpamint.Scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cpamint.DeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return deviceCode{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", chromeUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return deviceCode{}, err
	}
	defer resp.Body.Close()
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return deviceCode{}, err
	}
	if resp.StatusCode != http.StatusOK || payload.DeviceCode == "" || payload.UserCode == "" {
		return deviceCode{}, fmt.Errorf("device code 返回 %s", resp.Status)
	}
	if payload.ExpiresIn <= 0 {
		payload.ExpiresIn = 1800
	}
	if payload.Interval < 1 {
		payload.Interval = 5
	}
	if payload.VerificationURI == "" {
		payload.VerificationURI = "https://accounts.x.ai/oauth2/device"
	}
	if payload.VerificationURIComplete == "" {
		payload.VerificationURIComplete = payload.VerificationURI + "?user_code=" + url.QueryEscape(payload.UserCode)
	}
	return deviceCode{payload.DeviceCode, payload.UserCode, payload.VerificationURI, payload.VerificationURIComplete, payload.ExpiresIn, payload.Interval}, nil
}

func visitDevice(ctx context.Context, client *http.Client, device deviceCode) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, device.VerificationURIComplete, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", chromeUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func postDeviceAction(ctx context.Context, client *http.Client, endpoint string, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", chromeUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 || (!strings.Contains(resp.Request.URL.Path, "consent") && !strings.Contains(resp.Request.URL.Path, "done") && !strings.Contains(strings.ToLower(string(body)), "consent") && !strings.Contains(strings.ToLower(string(body)), "authorized")) {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return nil
}

func pollToken(ctx context.Context, client *http.Client, device deviceCode) (mintTokens, error) {
	deadline := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	sleep := time.Duration(device.Interval) * time.Second
	for time.Now().Before(deadline) {
		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {device.DeviceCode}, "client_id": {cpamint.ClientID},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cpamint.TokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return mintTokens{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", chromeUserAgent)
		resp, err := client.Do(req)
		if err != nil {
			return mintTokens{}, err
		}
		var payload struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			ExpiresIn    int    `json:"expires_in"`
			Error        string `json:"error"`
			Description  string `json:"error_description"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload)
		resp.Body.Close()
		if err != nil {
			return mintTokens{}, err
		}
		if resp.StatusCode == http.StatusOK && payload.AccessToken != "" && payload.RefreshToken != "" {
			return mintTokens{payload.AccessToken, payload.RefreshToken, payload.IDToken, payload.ExpiresIn}, nil
		}
		if payload.Error == "access_denied" || payload.Error == "expired_token" {
			return mintTokens{}, fmt.Errorf("设备授权失败: %s", payload.Description)
		}
		select {
		case <-ctx.Done():
			return mintTokens{}, ctx.Err()
		case <-time.After(sleep):
		}
	}
	return mintTokens{}, fmt.Errorf("设备授权超时")
}

func mintBrowser(ctx context.Context, browser *browserSession, proxy string, log func(string)) (mintTokens, error) {
	client, err := registrarHTTPClient(proxy)
	if err != nil {
		return mintTokens{}, err
	}
	device, err := requestDevice(ctx, client)
	if err != nil {
		return mintTokens{}, err
	}
	if err := chromedp.Run(browser.ctx, chromedp.Navigate(device.VerificationURIComplete), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return mintTokens{}, err
	}
	if _, err := evalUntil(browser.ctx, `(() => {const nodes=[...document.querySelectorAll('button,a,[role="button"]')];const allow=nodes.find(n=>/^(允许|allow)$/i.test((n.innerText||n.textContent||'').trim()));if(allow)return 'consent';const b=nodes.find(n=>/继续|continue/i.test(n.innerText||n.textContent||''));if(b){b.click();return 'clicked'}return 'wait'})()`, 30*time.Second, func(value string) bool { return value == "clicked" || value == "consent" }); err != nil {
		log("设备页继续按钮未找到，继续等待授权页")
	}
	if err := clickConsentReal(browser.ctx, 90*time.Second); err != nil {
		return mintTokens{}, err
	}
	return pollToken(ctx, client, device)
}

func clickConsentReal(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var pageState string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
const text=(document.body&&document.body.innerText||'').toLowerCase();
if(text.includes('设备已授权')||text.includes('device authorized'))return 'done';
const cookie=[...document.querySelectorAll('button,a,[role="button"]')].find(n=>/^(全部允许|接受所有 cookie|accept all cookies|reject all|全部拒绝)$/i.test((n.innerText||n.textContent||'').trim()));
if(cookie){cookie.click();return 'cookie';}
const next=[...document.querySelectorAll('button,a,[role="button"]')].find(n=>/^(继续|continue)$/i.test((n.innerText||n.textContent||'').trim()));
if(next){next.click();return 'continue';}
return 'wait';
})()`, &pageState))
		if pageState == "done" {
			return nil
		}
		var point struct{ X, Y float64 }
		err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {const b=[...document.querySelectorAll('button,input[type="submit"],a')].find(n=>/^(允许|allow)$/i.test((n.innerText||n.value||'').trim()));if(!b)return null;const r=b.getBoundingClientRect();return {X:r.left+r.width/2,Y:r.top+r.height/2};})()`, &point))
		if err == nil && point.X > 0 && point.Y > 0 {
			if err := chromedp.Run(ctx, chromedp.MouseClickXY(point.X, point.Y)); err != nil {
				return err
			}
			time.Sleep(time.Second)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("未找到 OAuth 允许按钮")
}

func writeCPAAuth(authDir, email string, tokens mintTokens) (string, error) {
	auth, err := cpamint.BuildAuthFile(email, tokens.AccessToken, tokens.RefreshToken, tokens.IDToken, tokens.ExpiresIn, cpamint.DefaultBaseURL)
	if err != nil {
		return "", err
	}
	path, _, err := cpamint.WriteAuthFile(authDir, auth)
	return filepath.Clean(path), err
}


