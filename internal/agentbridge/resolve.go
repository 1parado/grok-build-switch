package agentbridge

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func ResolveExecutable(override, grokHome string) (string, error) {
	candidates := make([]string, 0, 4)
	if strings.TrimSpace(override) != "" {
		candidates = append(candidates, strings.TrimSpace(override))
	}
	for _, name := range []string{"grok", "grok.exe"} {
		if resolved, err := exec.LookPath(name); err == nil {
			candidates = append(candidates, resolved)
		}
	}
	if grokHome != "" {
		name := "grok"
		if runtime.GOOS == "windows" {
			name = "grok.exe"
		}
		candidates = append(candidates, filepath.Join(grokHome, "bin", name))
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		absolute, err := filepath.Abs(candidate)
		if err != nil || seen[absolute] {
			continue
		}
		seen[absolute] = true
		info, err := os.Stat(absolute)
		if err == nil && !info.IsDir() {
			return absolute, nil
		}
	}
	if strings.TrimSpace(override) != "" {
		return "", fmt.Errorf("找不到指定的 Grok Build 可执行文件: %s", override)
	}
	return "", fmt.Errorf("未找到 grok，请先安装 Grok Build 并确认 grok --version 可用")
}
