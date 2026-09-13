package webpool

// statsig.go — grok.com 网页 API 的 x-statsig-id 签名。
//
// grok.com 应用层校验 x-statsig-id（statsig 反bot SDK，混淆 JS 运行时
// 计算）。签名流程与 chenyme/grok2api 一致：
//  1. 带账号 cookie GET grok.com/index，提取
//     <meta name="grok-site―verification" content="...">
//  2. POST {method, path, environment:{metaContent}} 到签名服务
//     （默认 https://grok.wodf.de/sign，可配置），返回 x-statsig-id
//  3. 按 (method, path) 缓存 1 小时；失效错误时主动失效并重签

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultStatsigSignerURL = "https://grok.wodf.de/sign"
	statsigCacheTTL         = time.Hour
)

var statsigMetaRegex = regexp.MustCompile(
	`(?is)<meta[^>]+name=["']grok-site―verification["'][^>]+content=["']([^"']+)["']`)
var statsigMetaRegexReverse = regexp.MustCompile(
	`(?is)<meta[^>]+content=["']([^"']+)["'][^>]+name=["']grok-site―verification["']`)

// statsigSigner 签名并缓存 x-statsig-id。
type statsigSigner struct {
	signerURL string
	client    *http.Client
	mu        sync.Mutex
	entries   map[string]statsigCacheEntry
}

type statsigCacheEntry struct {
	value     string
	expiresAt time.Time
}

func newStatsigSigner(signerURL string) *statsigSigner {
	if strings.TrimSpace(signerURL) == "" {
		signerURL = defaultStatsigSignerURL
	}
	return &statsigSigner{
		signerURL: signerURL,
		client:    &http.Client{Timeout: 15 * time.Second},
		entries:   map[string]statsigCacheEntry{},
	}
}

// fetchMetaContent 用账号 cookie 抓 grok.com 首页并提取验证 meta。
func (m *Manager) fetchStatsigMeta(ctx context.Context, cookieHeader string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.UpstreamBase()+"/index", nil)
	if err != nil {
		return "", err
	}
	for k, v := range webChatHeaders {
		req.Header.Set(k, v)
	}
	if strings.TrimSpace(cookieHeader) != "" {
		req.Header.Set("Cookie", cookieHeader)
	}
	resp, err := m.Do(req)
	if err != nil {
		return "", fmt.Errorf("抓取 grok.com/index: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("抓取 grok.com/index 返回 %d", resp.StatusCode)
	}
	if match := statsigMetaRegex.FindSubmatch(body); match != nil {
		return strings.TrimSpace(string(match[1])), nil
	}
	if match := statsigMetaRegexReverse.FindSubmatch(body); match != nil {
		return strings.TrimSpace(string(match[1])), nil
	}
	return "", fmt.Errorf("grok.com/index 缺少 grok-site―verification")
}

// requestSignature 调签名服务换取 x-statsig-id。
func (s *statsigSigner) requestSignature(ctx context.Context, method, path, metaContent string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"method": strings.ToUpper(strings.TrimSpace(method)),
		"path":   path,
		"environment": map[string]string{
			"metaContent": metaContent,
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.signerURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("签名服务请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("签名服务返回 %d", resp.StatusCode)
	}
	var value struct {
		StatsigID string `json:"x-statsig-id"`
	}
	if json.Unmarshal(body, &value) != nil || strings.TrimSpace(value.StatsigID) == "" {
		return "", fmt.Errorf("签名服务响应无效: %s", string(body))
	}
	return value.StatsigID, nil
}

// Sign 返回（缓存的或新签的）x-statsig-id。
func (s *statsigSigner) Sign(ctx context.Context, fetchMeta func() (string, error), method, path string) (string, error) {
	key := strings.ToUpper(strings.TrimSpace(method)) + "\x00" + path
	s.mu.Lock()
	if entry, ok := s.entries[key]; ok && time.Now().Before(entry.expiresAt) && entry.value != "" {
		s.mu.Unlock()
		return entry.value, nil
	}
	s.mu.Unlock()

	meta, err := fetchMeta()
	if err != nil {
		return "", err
	}
	signature, err := s.requestSignature(ctx, method, path, meta)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.entries[key] = statsigCacheEntry{value: signature, expiresAt: time.Now().Add(statsigCacheTTL)}
	s.mu.Unlock()
	return signature, nil
}

// Invalidate 失效指定签名（签名被拒时重签用）。
func (s *statsigSigner) Invalidate(method, path string) {
	key := strings.ToUpper(strings.TrimSpace(method)) + "\x00" + path
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}
