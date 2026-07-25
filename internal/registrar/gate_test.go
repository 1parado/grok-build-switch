package registrar

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEffectiveBrowserConcurrencySingleProxy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Workers = 3
	cfg.ProxyURL = "http://127.0.0.1:7897"
	pool := NewProxyPool(cfg)
	if n := effectiveBrowserConcurrency(cfg, pool); n != 1 {
		t.Fatalf("single proxy concurrency = %d, want 1", n)
	}
}

func TestEffectiveBrowserConcurrencyMultiProxy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Workers = 3
	cfg.ProxyURL = "http://127.0.0.1:7897\nhttp://127.0.0.1:7898\nhttp://127.0.0.1:7899"
	pool := NewProxyPool(cfg)
	if n := effectiveBrowserConcurrency(cfg, pool); n != 3 {
		t.Fatalf("multi proxy concurrency = %d, want 3", n)
	}
}

func TestShouldCoolProxySkipsLoopbackTransient(t *testing.T) {
	err := errors.New("page load error net::ERR_CONNECTION_CLOSED")
	if shouldCoolProxy("http://127.0.0.1:7897", NewProxyPool(Config{ProxyURL: "http://127.0.0.1:7897"}), err) {
		t.Fatal("should not cool loopback proxy on transient nav error")
	}
	if shouldCoolProxy("http://1.2.3.4:8080", NewProxyPool(Config{ProxyURL: "http://1.2.3.4:8080"}), err) {
		t.Fatal("should not cool sole proxy on transient nav error")
	}
	multi := NewProxyPool(Config{ProxyURL: "http://1.2.3.4:8080\nhttp://5.6.7.8:8080"})
	if !shouldCoolProxy("http://1.2.3.4:8080", multi, errors.New("403 forbidden permanent")) {
		t.Fatal("non-transient with multi pool should cool")
	}
}

func TestBrowserGateSerializes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Workers = 3
	cfg.ProxyURL = "http://127.0.0.1:7897"
	gate := newBrowserGate(cfg, NewProxyPool(cfg))
	if gate.Limit() != 1 {
		t.Fatalf("limit = %d", gate.Limit())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := gate.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	// Second acquire should block until release; use short timeout.
	blocked, cancel2 := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel2()
	if err := gate.Acquire(blocked); err == nil {
		t.Fatal("expected second acquire to block/timeout while first held")
	}
	gate.Release()
	// After release, stagger delay still applies before the next browser may start.
	if err := gate.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	gate.Release()
}

func TestIsTransientNavError(t *testing.T) {
	if !isTransientNavError(errors.New("page load error net::ERR_CONNECTION_CLOSED")) {
		t.Fatal("expected transient")
	}
	if isTransientNavError(errors.New("selector not found")) {
		t.Fatal("selector should not be transient")
	}
}
