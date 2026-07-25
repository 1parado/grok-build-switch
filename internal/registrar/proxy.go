package registrar

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Proxy strategies for multi-line proxy pools (aligned with grokcli-2api).
const (
	ProxyStrategyRoundRobin = "round_robin"
	ProxyStrategyRandom     = "random"
	ProxyStrategySticky     = "sticky" // pick by account index, stable across retries of the same slot
)

// ProxyPool rotates outbound proxies for registration workers.
type ProxyPool struct {
	mu        sync.Mutex
	urls      []string
	strategy  string
	rr        int
	cooldown  time.Duration
	failedAt  map[string]time.Time
	failCount map[string]int
}

// ParseProxyList splits multi-proxy text (newlines / commas / semicolons).
// Lines starting with # are comments. Deduplicates while preserving order.
func ParseProxyList(text string) []string {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return nil
	}
	// Normalize separators but keep URL schemes intact.
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	chunks := make([]string, 0, 8)
	for _, line := range strings.Split(normalized, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// semicolon-separated
		if strings.Contains(line, ";") {
			for _, sub := range strings.Split(line, ";") {
				sub = strings.TrimSpace(sub)
				if sub != "" && !strings.HasPrefix(sub, "#") {
					chunks = append(chunks, sub)
				}
			}
			continue
		}
		// comma-separated list of proxies
		if strings.Contains(line, ",") {
			parts := strings.Split(line, ",")
			if len(parts) > 1 {
				ok := true
				for _, p := range parts {
					p = strings.TrimSpace(p)
					if p == "" {
						continue
					}
					// Heuristic: each segment should look like a host/url, not a query pair.
					if strings.Contains(p, "=") && !strings.Contains(p, "://") && !strings.Contains(p, ":") {
						ok = false
						break
					}
				}
				if ok {
					for _, p := range parts {
						p = strings.TrimSpace(p)
						if p != "" && !strings.HasPrefix(p, "#") {
							chunks = append(chunks, p)
						}
					}
					continue
				}
			}
		}
		chunks = append(chunks, line)
	}
	out := make([]string, 0, len(chunks))
	seen := map[string]bool{}
	for _, c := range chunks {
		normalizedURL, err := NormalizeProxyURL(c)
		if err != nil || normalizedURL == "" {
			continue
		}
		if seen[normalizedURL] {
			continue
		}
		seen[normalizedURL] = true
		out = append(out, normalizedURL)
	}
	return out
}

// NormalizeProxyURL accepts common residential formats:
//
//	http://host:port
//	http://user:pass@host:port
//	socks5://host:port
//	host:port
//	host:port:user:pass
//	scheme://host:port:user:pass
func NormalizeProxyURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("空代理")
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "soket5://") {
		s = "socks5://" + s[len("soket5://"):]
		lower = strings.ToLower(s)
	}
	if strings.HasPrefix(lower, "socket5://") {
		s = "socks5://" + s[len("socket5://"):]
		lower = strings.ToLower(s)
	}

	scheme := "http"
	rest := s
	if strings.Contains(s, "://") {
		parts := strings.SplitN(s, "://", 2)
		scheme = strings.ToLower(strings.TrimSpace(parts[0]))
		if scheme == "" {
			scheme = "http"
		}
		if scheme == "soket5" || scheme == "socket5" {
			scheme = "socks5"
		}
		rest = parts[1]
	}

	// host:port:user:pass (and scheme://host:port:user:pass)
	if !strings.Contains(rest, "@") {
		cols := strings.Split(rest, ":")
		if len(cols) == 4 {
			host, port, user, pass := cols[0], cols[1], cols[2], cols[3]
			if host != "" && port != "" {
				userInfo := url.UserPassword(user, pass)
				return fmt.Sprintf("%s://%s@%s:%s", scheme, userInfo.String(), host, port), nil
			}
		}
	}

	if !strings.Contains(s, "://") {
		s = scheme + "://" + rest
	} else {
		s = scheme + "://" + rest
	}
	parsed, err := url.Parse(s)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("代理地址无效: %s", raw)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("代理协议不支持: %s", parsed.Scheme)
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func NewProxyPool(config Config) *ProxyPool {
	urls := ParseProxyList(config.ProxyURL)
	strategy := strings.ToLower(strings.TrimSpace(config.ProxyStrategy))
	switch strategy {
	case ProxyStrategyRoundRobin, ProxyStrategyRandom, ProxyStrategySticky:
	default:
		strategy = ProxyStrategyRoundRobin
	}
	cooldown := time.Duration(config.ProxyCooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = 120 * time.Second
	}
	return &ProxyPool{
		urls:      urls,
		strategy:  strategy,
		cooldown:  cooldown,
		failedAt:  map[string]time.Time{},
		failCount: map[string]int{},
	}
}

// Primary returns the first configured proxy (or empty).
func (p *ProxyPool) Primary() string {
	if p == nil || len(p.urls) == 0 {
		return ""
	}
	return p.urls[0]
}

// Len returns pool size.
func (p *ProxyPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.urls)
}

// Pick selects a proxy for account index (0-based). Empty means direct connection.
func (p *ProxyPool) Pick(accountIndex int) string {
	if p == nil || len(p.urls) == 0 {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	available := p.availableLocked()
	if len(available) == 0 {
		// All cooling down — still return something so registration can try.
		available = append([]string(nil), p.urls...)
	}
	switch p.strategy {
	case ProxyStrategySticky:
		if accountIndex < 0 {
			accountIndex = 0
		}
		return available[accountIndex%len(available)]
	case ProxyStrategyRandom:
		// deterministic-enough without math/rand seed races: mix time + index
		n := time.Now().UnixNano() + int64(accountIndex)*997
		if n < 0 {
			n = -n
		}
		return available[int(n)%len(available)]
	default: // round_robin
		url := available[p.rr%len(available)]
		p.rr++
		return url
	}
}

func (p *ProxyPool) availableLocked() []string {
	now := time.Now()
	out := make([]string, 0, len(p.urls))
	for _, u := range p.urls {
		if until, ok := p.failedAt[u]; ok && now.Before(until) {
			continue
		}
		out = append(out, u)
	}
	return out
}

// ReportSuccess clears failure state for a proxy.
func (p *ProxyPool) ReportSuccess(proxyURL string) {
	if p == nil || proxyURL == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.failedAt, proxyURL)
	delete(p.failCount, proxyURL)
}

// ReportFailure marks a proxy for cooldown after repeated failures.
func (p *ProxyPool) ReportFailure(proxyURL string) {
	if p == nil || proxyURL == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failCount[proxyURL]++
	// Cool down after first hard failure to avoid hammering a bad exit IP.
	p.failedAt[proxyURL] = time.Now().Add(p.cooldown)
}

// RedactProxy hides credentials in logs.
func RedactProxy(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return "直连"
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return proxyURL
	}
	if parsed.User != nil {
		// Avoid url.UserPassword which percent-encodes "*".
		host := parsed.Host
		return parsed.Scheme + "://***:***@" + host
	}
	return parsed.String()
}

func normalizeProxyStrategy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ProxyStrategyRandom:
		return ProxyStrategyRandom
	case ProxyStrategySticky:
		return ProxyStrategySticky
	default:
		return ProxyStrategyRoundRobin
	}
}
