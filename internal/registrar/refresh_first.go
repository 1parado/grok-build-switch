package registrar

// refresh_first.go — 两段式刷新：优先用文件里已有的 refresh_token 直接续期
// （秒级、无浏览器），失败（吊销/过期/网络/缺文件）才回退到浏览器重新登录铸造。
//
// 背景：xAI 的 refresh token 在重新登录铸造后是**旋转式**的，可直接用于
// token 端点续期（实际验证：连续两轮 refresh 均成功，每轮颁发新的
// access_token + 旋转后的 refresh_token）。因此"刷新"应优先走续期，
// 只有续期失败（例如旧 token 被服务端吊销 invalid_grant）才需要浏览器。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	refreshEndpointDefault = "https://auth.x.ai/oauth2/token"
	refreshDefaultClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	maxRefreshBody         = 1 << 20
)

// RefreshTokenFromFile 用 cpa_auths 凭据文件（xai-<email>.json）里保存的
// refresh_token 直接调 token 端点续期。成功则把新 access_token / 旋转后的
// refresh_token / 过期时间写回原文件并返回 nil；任何失败返回 error，
// 调用方据此回退浏览器重新登录。
func RefreshTokenFromFile(config Config, authFile string) error {
	data, err := os.ReadFile(authFile)
	if err != nil {
		return fmt.Errorf("读取凭据文件失败: %w", err)
	}
	cred := map[string]any{}
	if err := json.Unmarshal(data, &cred); err != nil {
		return fmt.Errorf("解析凭据文件失败: %w", err)
	}
	refresh := strings.TrimSpace(anyString(cred["refresh_token"]))
	if refresh == "" {
		return fmt.Errorf("凭据文件没有 refresh_token")
	}
	endpoint := strings.TrimSpace(anyString(cred["token_endpoint"]))
	if endpoint == "" {
		endpoint = refreshEndpointDefault
	}
	clientID := strings.TrimSpace(anyString(cred["client_id"]))
	if clientID == "" {
		clientID = refreshDefaultClientID
	}

	client, err := registrarHTTPClient(config.ProxyURL)
	if err != nil {
		return fmt.Errorf("构建代理客户端失败: %w", err)
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refresh},
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("创建刷新请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("调用 token 端点失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBody))
	if err != nil {
		return fmt.Errorf("读取刷新响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("token 端点返回 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("解析刷新响应失败: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return fmt.Errorf("刷新响应缺少 access_token")
	}

	expiresIn := payload.ExpiresIn
	if expiresIn <= 0 {
		if v, ok := cred["expires_in"].(float64); ok && v > 0 {
			expiresIn = int(v)
		} else {
			expiresIn = 21600
		}
	}
	now := time.Now().UTC()
	cred["access_token"] = strings.TrimSpace(payload.AccessToken)
	if strings.TrimSpace(payload.RefreshToken) != "" {
		cred["refresh_token"] = strings.TrimSpace(payload.RefreshToken)
	}
	if v := strings.TrimSpace(payload.IDToken); v != "" {
		cred["id_token"] = v
	}
	cred["token_type"] = "Bearer"
	cred["expires_in"] = expiresIn
	cred["expired"] = now.Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
	cred["last_refresh"] = now.Format(time.RFC3339)

	out, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化凭据失败: %w", err)
	}
	if err := atomicWrite(authFile, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("写回凭据文件失败: %w", err)
	}
	return nil
}

// RefreshAccount 两段式刷新单个账号：
//
//  1. 先用现有凭据文件（authDir/xai-<email>.json）里的 refresh_token 直接续期
//     （秒级、不弹浏览器）；
//  2. 续期失败（吊销/过期/网络/文件缺失/无 refresh_token）才回退到
//     ReloginAccount 走浏览器重新登录 + 铸造。
//
// 文件名约定与 ReloginAccount 的 writeCPAAuth 完全一致（xai-<email>.json），
// 因此 WebUI「刷新」按钮、CLI 批量刷新与号池自动续期共用同一份凭据文件。
func RefreshAccount(parent context.Context, config Config, email, password, authDir, cookieDir string, log func(string)) (ReloginResult, error) {
	email = strings.TrimSpace(email)
	res := ReloginResult{Email: email, Status: "failed"}
	if email == "" {
		res.Error = "邮箱为空"
		return res, fmt.Errorf("邮箱为空")
	}
	authFile := filepath.Join(authDir, "xai-"+email+".json")
	if err := RefreshTokenFromFile(config, authFile); err == nil {
		res.Status = "success"
		res.MintMethod = "refresh"
		res.AuthFile = authFile
		if log != nil {
			log(fmt.Sprintf("[两段式] %s：refresh_token 直接续期成功，无需浏览器", email))
		}
		return res, nil
	} else if log != nil {
		log(fmt.Sprintf("[两段式] %s：文件续期失败（%v）→ 回退浏览器重新登录…", email, err))
	}
	rollback, rollbackErr := ReloginAccount(parent, config, email, password, authDir, cookieDir, log)
	if rollbackErr != nil && rollback.Status == "" {
		rollback.Status = "failed"
		rollback.Error = rollbackErr.Error()
	}
	return rollback, rollbackErr
}

func anyString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}
