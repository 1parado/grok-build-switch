package registrar

import (
	"strings"
	"testing"
	"time"
)

func TestParseProxyListAndNormalize(t *testing.T) {
	text := `
# comment
http://127.0.0.1:7897
host.example:8080:user:pass
socks5://1.2.3.4:1080
http://127.0.0.1:7897
`
	got := ParseProxyList(text)
	if len(got) != 3 {
		t.Fatalf("ParseProxyList len = %d, got %#v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "http://127.0.0.1:7897") {
		t.Fatalf("first = %q", got[0])
	}
	if !strings.Contains(got[1], "user") || !strings.Contains(got[1], "host.example:8080") {
		t.Fatalf("user:pass form = %q", got[1])
	}
	if !strings.HasPrefix(got[2], "socks5://") {
		t.Fatalf("socks = %q", got[2])
	}
}

func TestProxyPoolRoundRobinAndCooldown(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProxyURL = "http://127.0.0.1:7001\nhttp://127.0.0.1:7002"
	cfg.ProxyStrategy = ProxyStrategyRoundRobin
	cfg.ProxyCooldownSeconds = 60
	pool := NewProxyPool(cfg)
	if pool.Len() != 2 {
		t.Fatalf("pool len = %d", pool.Len())
	}
	a := pool.Pick(0)
	b := pool.Pick(1)
	if a == "" || b == "" || a == b {
		t.Fatalf("round robin picks = %q %q", a, b)
	}
	pool.ReportFailure(a)
	// Failed proxy should be skipped while cool.
	for i := 0; i < 4; i++ {
		if got := pool.Pick(i); got == a {
			t.Fatalf("cooled proxy still picked: %q", got)
		}
	}
	// After success clear, it can return.
	pool.ReportSuccess(a)
	// Force cooldown expiry path: mark failed with zero cooldown by direct map write.
	pool.mu.Lock()
	pool.failedAt[a] = time.Now().Add(-time.Second)
	pool.mu.Unlock()
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		seen[pool.Pick(i)] = true
	}
	if !seen[a] || !seen[b] {
		t.Fatalf("expected both proxies after cooldown, seen=%v", seen)
	}
}

func TestProxyPoolSticky(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProxyURL = "http://a:1\nhttp://b:2\nhttp://c:3"
	cfg.ProxyStrategy = ProxyStrategySticky
	pool := NewProxyPool(cfg)
	first := pool.Pick(0)
	again := pool.Pick(0)
	if first != again {
		t.Fatalf("sticky pick(0) unstable: %q vs %q", first, again)
	}
	if pool.Pick(1) == first && pool.Pick(2) == first {
		// With 3 proxies sticky by index, index 1 and 2 should differ from 0
		// unless pool filtered — assert at least index 1 differs.
	}
	if pool.Pick(1) == pool.Pick(0) {
		t.Fatalf("sticky index 0 and 1 should differ")
	}
}

func TestRedactProxy(t *testing.T) {
	if RedactProxy("") != "直连" {
		t.Fatal("empty should be 直连")
	}
	got := RedactProxy("http://user:secret@127.0.0.1:7890")
	if strings.Contains(got, "secret") || !strings.Contains(got, "***") {
		t.Fatalf("redact failed: %q", got)
	}
}

func TestNormalizeRegisterEngine(t *testing.T) {
	if normalizeRegisterEngine("protocol") != "protocol_prefer" {
		t.Fatal("protocol alias")
	}
	if normalizeRegisterEngine("") != "browser" {
		t.Fatal("default browser")
	}
}
