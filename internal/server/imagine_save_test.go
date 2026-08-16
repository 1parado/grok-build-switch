package server

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestImagineGenerateOne 验证 generateOne 全链路：生图 + 过滤缩略图 + 落盘 + 返回 URL。
func TestImagineGenerateOne(t *testing.T) {
	dataDir := imagineTestDataDir(t)
	eng := NewImagineEngine(dataDir)
	if eng.AccountCount() == 0 {
		t.Skip("no accounts")
	}
	idx := eng.AccountCount() - 2
	if v := os.Getenv("ACCT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < eng.AccountCount() {
			idx = n
		}
	}
	acc := eng.accounts[idx]
	t.Logf("using account index=%d id=%s", idx, acc.id)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res := eng.generateOne(ctx, acc, "a red sports car on a neon street", "grok-imagine-image", "1:1")
	if !res.OK {
		t.Fatalf("generateOne failed: code=%s msg=%s account=%s", res.ErrCode, res.ErrMsg, res.Account)
	}
	if len(res.Images) == 0 {
		t.Fatalf("generateOne returned OK but no image saved")
	}
	// 校验文件确实落盘
	rel := res.Images[0]
	fsPath := filepath.Join(eng.outputsDir, filepath.Base(rel))
	if _, err := os.Stat(fsPath); err != nil {
		t.Fatalf("saved file missing: %s (%v)", fsPath, err)
	}
	t.Logf("OK model=%s %dx%d account=%s url=%s", res.ModelName, res.Width, res.Height, res.Account, res.Images[0])
}
