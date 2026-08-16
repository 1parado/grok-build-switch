package server

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func imagineTestDataDir(t *testing.T) string {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	dataDir := filepath.Join(home, ".grok_switch")
	if _, err := os.Stat(filepath.Join(dataDir, "registrar", "cookies")); err != nil {
		t.Skip("no registrar cookies")
	}
	return dataDir
}

// TestImagineProxyConnectivity 验证代理 transport 能发 HTTPS 请求到 grok.com。
func TestImagineProxyConnectivity(t *testing.T) {
	dataDir := imagineTestDataDir(t)
	eng := NewImagineEngine(dataDir)
	client := &http.Client{Timeout: 20 * time.Second, Transport: eng.transport}
	if eng.transport == nil {
		// 直连（无代理）。
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{}}
	}
	req, _ := http.NewRequest(http.MethodGet, "https://grok.com/imagine", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy https GET failed: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	t.Logf("grok.com/imagine -> %d, body.head=%q", resp.StatusCode, string(b))
}

// TestImagineGenerate 调用带账号轮询的 Generate：自动跳过被限流的账号，
// 验证 Go WS 协议端到端可用。
func TestImagineGenerate(t *testing.T) {
	dataDir := imagineTestDataDir(t)
	eng := NewImagineEngine(dataDir)
	if eng.AccountCount() == 0 {
		t.Skip("no accounts")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res := eng.Generate(ctx, "a cute orange cat on a blue sofa", "grok-imagine-image", "1:1")
	if !res.OK {
		t.Fatalf("generate failed: code=%s msg=%s account=%s", res.ErrCode, res.ErrMsg, res.Account)
	}
	t.Logf("OK model=%s %dx%d account=%s images=%d", res.ModelName, res.Width, res.Height, res.Account, len(res.Images))
	for i, u := range res.Images {
		t.Logf("  image[%d] %s", i, u)
	}
}
