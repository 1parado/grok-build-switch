package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTopLevelPreviewShape(t *testing.T) {
	cwd := t.TempDir()
	_ = os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("x"), 0o644)
	_ = os.Mkdir(filepath.Join(cwd, "proj"), 0o755)
	_ = os.WriteFile(filepath.Join(cwd, ".DS_Store"), []byte("junk"), 0o644)

	out := topLevelPreview(cwd)
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "proj/") {
		t.Fatalf("预览缺条目: %q", out)
	}
	if strings.Contains(out, ".DS_Store") {
		t.Fatalf(".DS_Store 应被过滤: %q", out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "a.txt") {
		t.Fatalf("条目应为排序后的纯名字列表: %q", out)
	}
}

func TestTopLevelPreviewTruncates(t *testing.T) {
	cwd := t.TempDir()
	for i := 0; i < 60; i++ {
		_ = os.WriteFile(filepath.Join(cwd, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o644)
	}
	out := topLevelPreview(cwd)
	if got := strings.Count(out, "\n"); got != 41 { // 40 条 + 1 行省略提示
		t.Fatalf("应 40 条 + 省略行，得到 %d 行: %q", got, out)
	}
	if !strings.Contains(out, "60") {
		t.Fatalf("省略行应含总数: %q", out)
	}
}

func TestBuildEnvSection(t *testing.T) {
	cwd := t.TempDir()
	_ = os.Mkdir(filepath.Join(cwd, "src"), 0o755)
	out := buildEnvSection(cwd)
	for _, want := range []string{"# 环境", "工作目录: " + cwd, "平台:", "src/"} {
		if !strings.Contains(out, want) {
			t.Fatalf("环境块缺 %q:\n%s", want, out)
		}
	}
}
