package registrar

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// browserGate serializes Chrome sessions so a single local proxy (Clash etc.)
// is not hammered by multiple concurrent Chromium processes.
type browserGate struct {
	sem     chan struct{}
	stagger time.Duration
	mu      sync.Mutex
	last    time.Time
	limit   int
}

func newBrowserGate(config Config, pool *ProxyPool) *browserGate {
	limit := effectiveBrowserConcurrency(config, pool)
	stagger := effectiveStagger(config, pool)
	return &browserGate{
		sem:     make(chan struct{}, limit),
		stagger: stagger,
		limit:   limit,
	}
}

func effectiveBrowserConcurrency(config Config, pool *ProxyPool) int {
	workers := config.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > 30 {
		workers = 30
	}
	proxies := 0
	if pool != nil {
		proxies = pool.Len()
	}
	// 放开单代理/直连的并发锁：允许在单个 Clash 端口（负载均衡多端口）下并行。
	// 原逻辑 proxies <= 1 时强制 return 1 会锁死并发；现在尊重用户配置的 workers。
	// 配合 ClashRotator（通过 Clash API 在每个账号注册前切换到不同节点），
	// 单端口也能让连续注册走不同出口 IP。
	if proxies <= 1 {
		return workers
	}
	if proxies < workers {
		return proxies
	}
	return workers
}

func effectiveStagger(config Config, pool *ProxyPool) time.Duration {
	if pool != nil && pool.Len() <= 1 {
		// Serial mode: the previous browser has already closed by the time the
		// next one launches, so a short gap is enough to release the proxy port.
		return 2 * time.Second
	}
	return 2 * time.Second
}

func (g *browserGate) Limit() int {
	if g == nil {
		return 1
	}
	return g.limit
}

func (g *browserGate) Stagger() time.Duration {
	if g == nil {
		return 0
	}
	return g.stagger
}

func (g *browserGate) Acquire(ctx context.Context) error {
	if g == nil {
		return nil
	}
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	wait := time.Duration(0)
	if !g.last.IsZero() && g.stagger > 0 {
		wait = time.Until(g.last.Add(g.stagger))
	}
	g.mu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			g.Release()
			return ctx.Err()
		}
	}
	g.mu.Lock()
	g.last = time.Now()
	g.mu.Unlock()
	return nil
}

func (g *browserGate) Release() {
	if g == nil {
		return
	}
	select {
	case <-g.sem:
	default:
	}
}

func (g *browserGate) Describe() string {
	if g == nil {
		return "浏览器并发=1"
	}
	return fmt.Sprintf("浏览器并发≤%d，启动间隔≥%s", g.limit, g.stagger.Round(time.Second))
}

// isTransientNavError reports network/proxy blips worth retrying.
func isTransientNavError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{
		"err_connection_closed",
		"err_connection_reset",
		"err_connection_refused",
		"err_connection_aborted",
		"err_connection_timed_out",
		"err_timed_out",
		"err_proxy_connection_failed",
		"err_tunnel_connection_failed",
		"err_network_changed",
		"err_internet_disconnected",
		"err_empty_response",
		"err_ssl_protocol_error",
		"err_name_not_resolved",
		"net::err_",
		"connection reset",
		"connection refused",
		"i/o timeout",
		"context deadline exceeded",
		"eof",
		"broken pipe",
		"unexpected eof",
		"wsarecv",
		"forcibly closed",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}

func isLoopbackProxy(proxyURL string) bool {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return false
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// shouldCoolProxy decides whether a failure should take the proxy out of rotation.
func shouldCoolProxy(proxyURL string, pool *ProxyPool, err error) bool {
	if proxyURL == "" || err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Mint/consent SPA failures are not proxy health signals.
	for _, frag := range []string{
		"mint_failed", "device verify", "device approve", "consent",
		"cloudflare", "换取 token", "设备未授权", "sso 会话",
		"turnstile", "profile_or_turnstile", "context canceled", "context cancelled",
		"curl_cffi", "device_code", "access denied", "invalid_grant",
	} {
		if strings.Contains(msg, frag) {
			return false
		}
	}
	// Never cool the only proxy for a transient blip — user has nothing else to use.
	if pool != nil && pool.Len() <= 1 && isTransientNavError(err) {
		return false
	}
	// Local Clash/V2Ray: never cool on CDP/browser disconnects either.
	if isLoopbackProxy(proxyURL) {
		if isTransientNavError(err) {
			return false
		}
		// Single local proxy + browser automation noise.
		if pool != nil && pool.Len() <= 1 {
			return false
		}
	}
	return true
}

func navigateHint(err error, proxy string, concurrentNote string) string {
	parts := []string{}
	if isTransientNavError(err) {
		parts = append(parts, "网络/代理连接被中断（常见于多开浏览器抢同一本地代理）")
	}
	if isLoopbackProxy(proxy) {
		parts = append(parts, "请确认 Clash/系统代理 "+RedactProxy(proxy)+" 已开启且允许局域网连接")
	} else if proxy != "" {
		parts = append(parts, "检查代理 "+RedactProxy(proxy)+" 是否可用")
	}
	if concurrentNote != "" {
		parts = append(parts, concurrentNote)
	}
	parts = append(parts, "建议并发=1 或错峰后再试")
	return strings.Join(parts, "；")
}
