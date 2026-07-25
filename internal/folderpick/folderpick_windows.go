//go:build windows

package folderpick

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// pathB64Prefix marks a UTF-8 path encoded as base64 so Chinese (and other
// non-ASCII) paths survive PowerShell console code-page mangling.
const pathB64Prefix = "PATHB64:"

// pickDirectory uses PowerShell FolderBrowserDialog (STA) so the tray/browser
// edition can open a real Windows folder picker without embedding WinForms.
//
// Selected paths are written as base64(UTF-8) to avoid GBK/CP936 → UTF-8
// corruption that otherwise breaks Chinese directory names.
func pickDirectory(ctx context.Context, start string) (string, error) {
	start = normalizeStart(start)
	script := `
Add-Type -AssemblyName System.Windows.Forms
$dialog = New-Object System.Windows.Forms.FolderBrowserDialog
$dialog.Description = '选择工作目录'
$dialog.ShowNewFolderButton = $true
$start = $env:GROK_SWITCH_FOLDER_START
if ($start -and (Test-Path -LiteralPath $start)) {
  $dialog.SelectedPath = $start
}
$top = New-Object System.Windows.Forms.Form
$top.TopMost = $true
$top.ShowInTaskbar = $false
$top.WindowState = 'Minimized'
$top.Opacity = 0
try {
  $result = $dialog.ShowDialog($top)
  if ($result -eq [System.Windows.Forms.DialogResult]::OK -and $dialog.SelectedPath) {
    # Prefer full path; encode as UTF-8 base64 so Go receives intact Unicode.
    $full = [System.IO.Path]::GetFullPath($dialog.SelectedPath)
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($full)
    $b64 = [Convert]::ToBase64String($bytes)
    [Console]::Out.Write('PATHB64:' + $b64)
  }
} finally {
  $dialog.Dispose()
  $top.Dispose()
}
`
	cmd := exec.CommandContext(ctx, "powershell",
		"-NoProfile",
		"-STA",
		"-WindowStyle", "Hidden",
		"-ExecutionPolicy", "Bypass",
		"-Command", script,
	)
	// Pass start via env (Windows Go uses UTF-16 for process env → intact Unicode).
	cmd.Env = append(os.Environ(), "GROK_SWITCH_FOLDER_START="+start)
	// Hide the PowerShell console window when launched from a GUI build.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("打开文件夹选择器失败: %s", detail)
	}
	path, err := decodePickerOutput(stdout.Bytes())
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", ErrCancelled
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("所选路径无效: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("所选路径不是目录: %s", path)
	}
	// Prefer absolute form for stable project matching / session cwd.
	if abs, absErr := filepath.Abs(path); absErr == nil {
		path = abs
	}
	return path, nil
}

// decodePickerOutput accepts PATHB64:<base64 utf-8> or a legacy plain path.
func decodePickerOutput(raw []byte) (string, error) {
	text := strings.TrimSpace(string(bytes.TrimSpace(raw)))
	if text == "" {
		return "", nil
	}
	if strings.HasPrefix(text, pathB64Prefix) {
		payload := strings.TrimSpace(strings.TrimPrefix(text, pathB64Prefix))
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "", fmt.Errorf("解析所选路径失败: %w", err)
		}
		if !utf8.Valid(decoded) {
			return "", fmt.Errorf("所选路径不是有效 UTF-8")
		}
		return string(decoded), nil
	}
	// Legacy plain stdout (ASCII-only paths usually OK; non-ASCII may be wrong).
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("所选路径编码无效（请升级后重试选择）")
	}
	return text, nil
}

func normalizeStart(start string) string {
	start = strings.TrimSpace(start)
	if start == "" {
		return ""
	}
	start = filepath.Clean(start)
	info, err := os.Stat(start)
	if err != nil {
		// If a file was pasted, open its parent.
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
