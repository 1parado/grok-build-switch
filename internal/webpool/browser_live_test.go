package webpool

// 临时 live 验证：浏览器代理 + 页面拦截器自动 statsig。
// GS_BROWSER_TEST=1 时运行。验证后删除。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestBrowserProxyLiveChat(t *testing.T) {
	if os.Getenv("GS_BROWSER_TEST") == "" {
		t.Skip("需要 GS_BROWSER_TEST=1")
	}
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.SetBrowserPath("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
	if _, err := m.UpdateSettings(Settings{Enabled: true, ProxyURL: "http://127.0.0.1:7890"}); err != nil {
		t.Fatal(err)
	}

	// 用注册机最新快照的 SSO。
	data, readErr := os.ReadFile("/Users/shiaho/.grok_switch/registrar/cookies/tmpqe8zhycc2uj_shiaho.sbs-f4770314cdb3.json")
	if readErr != nil {
		t.Skip("需要真实 SSO cookie")
	}
	var snapshot struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if json.Unmarshal(data, &snapshot) != nil {
		t.Skip("解析 cookie 快照失败")
	}
	var ssoValue string
	for _, c := range snapshot.Cookies {
		if c.Name == "sso" {
			ssoValue = c.Value
		}
	}
	if ssoValue == "" {
		t.Skip("快照中无 SSO cookie")
	}
	imported, _, err := m.ImportManual(ssoValue)
	if err != nil || imported != 1 {
		t.Fatalf("导入失败: imported=%d err=%v", imported, err)
	}
	t.Logf("已导入真实 SSO（长度 %d）", len(ssoValue))

	var thinks, texts []string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	text, err := m.Chat(ctx, ChatOptions{
		Model:   "grok-4-reasoning",
		Message: "1+1等于几？只回答数字，先想一下",
	}, func(ev WebStreamEvent) error {
		if ev.ThinkDelta != "" {
			thinks = append(thinks, ev.ThinkDelta)
			t.Logf("THINK: %s", ev.ThinkDelta)
		}
		if ev.TextDelta != "" {
			texts = append(texts, ev.TextDelta)
			t.Logf("TEXT: %s", ev.TextDelta)
		}
		return nil
	})

	t.Logf("text=%q err=%v", text, err)
	t.Logf("thinks=%v texts=%v", thinks, texts)
	if len(thinks) > 0 {
		t.Log("✓ 思考已流出！")
	}
	if len(texts) > 0 {
		t.Log("✓ 正文已流出！")
	}
	if err != nil {
		t.Logf("错误详情: %v", err)
		_ = fmt.Sprint() // keep fmt
	}
}
