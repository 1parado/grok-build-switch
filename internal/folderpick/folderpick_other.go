//go:build !windows

package folderpick

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func pickDirectory(ctx context.Context, start string) (string, error) {
	start = normalizeStart(start)
	var path string
	var err error
	switch runtime.GOOS {
	case "darwin":
		path, err = pickMacOS(ctx, start)
	default:
		path, err = pickLinux(ctx, start)
	}
	if err != nil {
		return "", err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return "", ErrCancelled
	}
	path = filepath.Clean(path)
	info, statErr := os.Stat(path)
	if statErr != nil {
		return "", fmt.Errorf("所选路径无效: %w", statErr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("所选路径不是目录: %s", path)
	}
	return path, nil
}

func pickMacOS(ctx context.Context, start string) (string, error) {
	// osascript: choose folder returns alias; POSIX path converts it.
	script := `set theFolder to choose folder with prompt "选择工作目录"`
	if start != "" {
		// Escape backslashes and quotes for AppleScript string.
		escaped := strings.ReplaceAll(start, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		script = fmt.Sprintf(`try
  set theFolder to choose folder with prompt "选择工作目录" default location POSIX file "%s"
on error
  set theFolder to choose folder with prompt "选择工作目录"
end try`, escaped)
	}
	script += `
POSIX path of theFolder`
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// User cancelled → exit code 1 with "User canceled."
		msg := strings.ToLower(stderr.String() + stdout.String())
		if strings.Contains(msg, "user canceled") || strings.Contains(msg, "user cancelled") {
			return "", ErrCancelled
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("打开文件夹选择器失败: %s", detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func pickLinux(ctx context.Context, start string) (string, error) {
	// Prefer zenity, then kdialog.
	if path, err := lookPath("zenity"); err == nil {
		args := []string{"--file-selection", "--directory", "--title=选择工作目录"}
		if start != "" {
			args = append(args, "--filename="+start+string(os.PathSeparator))
		}
		return runChooser(ctx, path, args...)
	}
	if path, err := lookPath("kdialog"); err == nil {
		args := []string{"--getexistingdirectory"}
		if start != "" {
			args = append(args, start)
		} else {
			args = append(args, os.Getenv("HOME"))
		}
		args = append(args, "--title", "选择工作目录")
		return runChooser(ctx, path, args...)
	}
	return "", fmt.Errorf("未找到文件夹选择器（请安装 zenity 或 kdialog）")
}

func runChooser(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// zenity/kdialog exit 1 on cancel.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 && strings.TrimSpace(stdout.String()) == "" {
			return "", ErrCancelled
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("打开文件夹选择器失败: %s", detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func normalizeStart(start string) string {
	start = strings.TrimSpace(start)
	if start == "" {
		return ""
	}
	start = filepath.Clean(start)
	info, err := os.Stat(start)
	if err != nil {
		parent := filepath.Dir(start)
		if pInfo, pErr := os.Stat(parent); pErr == nil && pInfo.IsDir() {
			return parent
		}
		return ""
	}
	if info.IsDir() {
		return start
	}
	return filepath.Dir(start)
}
