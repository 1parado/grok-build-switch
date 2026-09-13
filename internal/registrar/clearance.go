package registrar

// clearance.go — 采集 grok.com 的 Cloudflare 通行证（cf_clearance）。
//
// Web 通道号池（webpool）以 Chrome TLS 指纹直调 grok.com，但 cf_clearance
// 与出口 IP / 浏览器会话绑定且会过期；这里用注册机的可见浏览器实地
// 打开 grok.com 过盾（必要时实点 Turnstile），把新鲜 clearance 交给
// webpool 兜底使用。

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// HarvestGrokComClearance 启动可见浏览器访问 grok.com，等待 Cloudflare
// 挑战通过并返回新鲜的 cf_clearance。浏览器对用户可见——必要时用户
// 可手动点击验证码，采集最长等待 timeout。
func HarvestGrokComClearance(parent context.Context, config Config, timeout time.Duration, log func(string)) (string, error) {
	// 返回值格式："cf_clearance\x00User-Agent"（调用方拆分保存）。
	if log == nil {
		log = func(string) {}
	}
	session, err := startBrowser(parent, config, false)
	if err != nil {
		return "", fmt.Errorf("启动浏览器失败: %w", err)
	}
	defer session.Close()
	ctx, cancel := context.WithTimeout(session.ctx, timeout)
	defer cancel()

	log("打开 grok.com…")
	if err := chromedp.Run(ctx,
		chromedp.Navigate("https://grok.com/"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return "", fmt.Errorf("打开 grok.com 失败: %w", err)
	}

	var userAgent string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`navigator.userAgent`, &userAgent))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var cookies []*network.Cookie
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(cctx context.Context) error {
			var err error
			cookies, err = network.GetCookies().Do(cctx)
			return err
		})); err == nil {
			for _, c := range cookies {
				if c.Name == "cf_clearance" && c.Value != "" {
					log("已采集 cf_clearance（UA=" + userAgent + "）")
					return c.Value + "\x00" + userAgent, nil
				}
			}
		}
		// Turnstile 复选框：实点挑战 iframe 中心。
		var rect []float64
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		  const f = document.querySelector('iframe[src*="challenges.cloudflare.com"]');
		  if (!f) return null;
		  const r = f.getBoundingClientRect();
		  return [r.x, r.y, r.width, r.height];
		})()`, &rect))
		if len(rect) == 4 {
			log(fmt.Sprintf("发现挑战 iframe，尝试点击 @%.0f,%.0f…", rect[0]+rect[2]/2, rect[1]+rect[3]/2))
			_ = chromedp.Run(ctx, chromedp.MouseClickXY(rect[0]+rect[2]/2, rect[1]+rect[3]/2))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-parent.Done():
			return "", parent.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return "", fmt.Errorf("等待 Cloudflare 通行证超时（浏览器保持可见，可手动完成验证后重试）")
}
