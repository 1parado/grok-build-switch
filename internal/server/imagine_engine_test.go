package server

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestImagineEngineFresh 直接走引擎的 wsGenerate（已含 SetReadLimit），
// 用某个未被烧掉的新鲜账号验证能拿到图片 blob。
// ACCT 环境变量指定账号下标（默认用最后一个，确保未使用过）。
// 活体测试会真实调用 grok.com 并消耗账号额度，仅在显式开启时运行。
func TestImagineEngineFresh(t *testing.T) {
	if os.Getenv("GROK_SWITCH_LIVE_IMAGINE") == "" {
		t.Skip("live test consumes real quota; set GROK_SWITCH_LIVE_IMAGINE=1 to enable")
	}
	dataDir := imagineTestDataDir(t)
	eng := NewImagineEngine(dataDir)
	if eng.AccountCount() == 0 {
		t.Skip("no accounts")
	}
	idx := eng.AccountCount() - 1
	if v := os.Getenv("ACCT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < eng.AccountCount() {
			idx = n
		}
	}
	acc := eng.accounts[idx]
	t.Logf("using account index=%d id=%s", idx, acc.id)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	inner := eng.wsGenerate(ctx, acc, "a cute orange cat on a blue sofa", "grok-imagine-image", "1:1")
	if !inner.OK && len(inner.Blobs) == 0 {
		t.Fatalf("engine gen failed: code=%s msg=%s blobs=%d", inner.ErrCode, inner.ErrMsg, len(inner.Blobs))
	}
	t.Logf("OK model=%s %dx%d blobs=%d", inner.ModelName, inner.Width, inner.Height, len(inner.Blobs))
	for i, b := range inner.Blobs {
		t.Logf("  blob[%d] image_id=%s order=%d dataLen=%d", i, b.ImageID, b.Order, len(b.Data))
	}
}
