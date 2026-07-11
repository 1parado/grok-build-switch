package notify

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Info shows a non-blocking desktop notification when supported.
func Info(title, message string) {
	title = sanitize(title)
	message = sanitize(message)
	if title == "" {
		title = "grok_switch"
	}
	switch runtime.GOOS {
	case "windows":
		notifyWindows(title, message)
	case "darwin":
		_ = exec.Command("osascript", "-e", fmt.Sprintf(`display notification %q with title %q`, message, title)).Start()
	default:
		if path, err := exec.LookPath("notify-send"); err == nil {
			_ = exec.Command(path, title, message).Start()
		}
	}
}

func OpenPath(path string) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

func CopyText(text string) error {
	if text == "" {
		return fmt.Errorf("empty text")
	}
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("powershell", "-NoProfile", "-Command", "Set-Clipboard -Value $env:GROK_SWITCH_CLIP")
		cmd.Env = append(cmd.Environ(), "GROK_SWITCH_CLIP="+text)
		return cmd.Run()
	case "darwin":
		cmd := exec.Command("pbcopy")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	default:
		if path, err := exec.LookPath("xclip"); err == nil {
			cmd := exec.Command(path, "-selection", "clipboard")
			cmd.Stdin = strings.NewReader(text)
			return cmd.Run()
		}
		if path, err := exec.LookPath("xsel"); err == nil {
			cmd := exec.Command(path, "--clipboard", "--input")
			cmd.Stdin = strings.NewReader(text)
			return cmd.Run()
		}
		return fmt.Errorf("no clipboard utility found")
	}
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func notifyWindows(title, message string) {
	// Balloon tip via WinForms — no extra dependencies, works on Windows 10/11.
	ps := `
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$n = New-Object System.Windows.Forms.NotifyIcon
$n.Icon = [System.Drawing.SystemIcons]::Information
$n.Visible = $true
$n.BalloonTipTitle = $env:GROK_SWITCH_TITLE
$n.BalloonTipText = $env:GROK_SWITCH_MSG
$n.BalloonTipIcon = [System.Windows.Forms.ToolTipIcon]::Info
$n.ShowBalloonTip(3500)
Start-Sleep -Milliseconds 3800
$n.Dispose()
`
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", ps)
	cmd.Env = append(cmd.Environ(),
		"GROK_SWITCH_TITLE="+title,
		"GROK_SWITCH_MSG="+message,
	)
	_ = cmd.Start()
}
