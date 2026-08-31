package agentfs

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ShellResult 是一次命令执行的结果。
type ShellResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut 标记命令因超时被终止。
	TimedOut bool
}

// Combined 返回合并输出（stdout 在前，stderr 在后），供工具回传。
func (r ShellResult) Combined() string {
	var b strings.Builder
	if r.Stdout != "" {
		b.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			b.WriteByte('\n')
		}
	}
	if r.Stderr != "" {
		b.WriteString(r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Shell 在指定目录执行命令。timeout<=0 用默认 120s。
// 每次调用独立 shell 环境（不跨调用保留 cwd/env/history，对齐 Kimi bash 语义）。
func (e Env) Shell(ctx context.Context, command, dir string, timeout time.Duration) ShellResult {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if dir == "" {
		dir = e.Cwd
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	var stdout, stderr strings.Builder
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(runCtx, "cmd", "/C", command)
		// CommandContext 默认只杀 cmd.exe 本身，子进程（如长命令）
		// 会变成孤儿并继续占着输出管道，导致 Run 一直等。改成杀整棵树。
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
		// 树杀后仍可能有进程短暂占着管道，设上限避免 Wait 无限阻塞。
		cmd.WaitDelay = 2 * time.Second
	} else {
		cmd = exec.CommandContext(runCtx, "/bin/sh", "-c", command)
	}
	cmd.Dir = dir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	res := ShellResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if runCtx.Err() != nil && errorsIs(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		return res
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.ExitCode = -1
			res.Stderr = res.Stderr + "\n" + err.Error()
		}
	}
	return res
}

func errorsIs(err, target error) bool {
	return err != nil && err.Error() == target.Error()
}

// OpenPath 用系统默认方式打开文件/目录（对应现有 tray.OpenBrowser / open-path 语义）。
func OpenPath(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
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
