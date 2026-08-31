package agentfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// shellSleep 返回阻塞约 n 秒的当前平台命令（cmd 没有 sleep）。
// 2>&1 把子进程的管道句柄重定向到 nul，避免超时断父进程后
// Run() 因等待子进程关闭管道而阻塞到命令自然结束。
func shellSleep(n int) string {
	if runtime.GOOS == "windows" {
		// ping -n N+1 127.0.0.1 每个间隔约 1 秒，总计约 N 秒。
		return fmt.Sprintf("ping -n %d 127.0.0.1 >nul 2>&1", n+1)
	}
	return fmt.Sprintf("sleep %d", n)
}

// shellPwd 返回打印当前工作目录的当前平台命令（cmd 里等价是 cd）。
func shellPwd() string {
	if runtime.GOOS == "windows" {
		return "cd"
	}
	return "pwd"
}

// exitCmd 返回以指定码退出的当前平台命令（cmd 的 exit 同样可用）。
func exitCmd(code int) string {
	return fmt.Sprintf("exit %d", code)
}

func TestGuardResolvesAndEnforces(t *testing.T) {
	cwd := t.TempDir()
	env := Env{Cwd: cwd}

	abs, err := env.Guard("sub/file.go")
	if err != nil {
		t.Fatal(err)
	}
	if abs != filepath.Join(cwd, "sub", "file.go") {
		t.Fatalf("相对路径解析错误: %s", abs)
	}

	if _, err := env.Guard("../outside.txt"); err == nil {
		t.Fatal("越出 Cwd 应被拒绝")
	} else if _, ok := err.(*SandboxError); !ok {
		t.Fatalf("应为 SandboxError: %T", err)
	}

	// 额外沙箱路径允许访问。
	extra := t.TempDir()
	env2 := Env{Cwd: cwd, SandboxExtra: []string{extra}}
	if _, err := env2.Guard(filepath.Join(extra, "data.bin")); err != nil {
		t.Fatalf("SandboxExtra 应放行: %v", err)
	}
}

func TestGuardSiblingDirectoryRejected(t *testing.T) {
	cwd := t.TempDir()
	sibling := t.TempDir() // 同级目录，路径前缀相近
	env := Env{Cwd: cwd}
	// /tmp/A vs /tmp/B：前缀比较不安全，Rel 必须识别。
	if _, err := env.Guard(filepath.Join(sibling, "x.txt")); err == nil {
		t.Fatal("同级目录应被拒绝（前缀混淆防护）")
	}
}

func TestWriteReadAtomic(t *testing.T) {
	env := Env{Cwd: t.TempDir()}
	abs, _ := env.Guard("nested/dir/file.txt")
	if err := env.WriteText(abs, "hello 引擎"); err != nil {
		t.Fatal(err)
	}
	got, err := env.ReadText(abs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello 引擎" {
		t.Fatalf("内容不符: %q", got)
	}
	// 无残留临时文件。
	entries, _ := os.ReadDir(filepath.Dir(abs))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".agentfs-") {
			t.Fatalf("临时文件残留: %s", e.Name())
		}
	}
}

func TestReadTextTooLarge(t *testing.T) {
	env := Env{Cwd: t.TempDir()}
	abs, _ := env.Guard("big.txt")
	if err := env.WriteText(abs, strings.Repeat("x", 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.ReadText(abs, 50); err == nil {
		t.Fatal("超上限应报错")
	}
}

func TestShellRunAndTimeout(t *testing.T) {
	env := Env{Cwd: t.TempDir()}

	res := env.Shell(context.Background(), "echo hello", "", time.Minute)
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("echo 失败: %+v", res)
	}

	start := time.Now()
	res = env.Shell(context.Background(), shellSleep(5), "", 200*time.Millisecond)
	if !res.TimedOut {
		t.Fatalf("应超时: %+v", res)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("超时未及时终止")
	}
}

func TestShellNonZeroExit(t *testing.T) {
	env := Env{Cwd: t.TempDir()}
	res := env.Shell(context.Background(), exitCmd(3), "", time.Minute)
	if res.ExitCode != 3 {
		t.Fatalf("退出码错误: %+v", res)
	}
}

func TestShellCwdParameter(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	env := Env{Cwd: a}
	res := env.Shell(context.Background(), shellPwd(), b, time.Minute)
	if !strings.Contains(res.Stdout, filepath.Base(b)) {
		t.Fatalf("cwd 参数未生效: %+v (want base %s)", res, filepath.Base(b))
	}
}

func TestWithinSandboxDotDotInside(t *testing.T) {
	// cwd/sub/.. 归一化后仍在 cwd 内。
	env := Env{Cwd: t.TempDir()}
	abs, err := env.Guard("sub/../ok.txt")
	if err != nil {
		t.Fatalf("cwd 内的 .. 应放行: %v", err)
	}
	if !strings.HasSuffix(abs, "ok.txt") || strings.Contains(abs, "..") {
		t.Fatalf("路径未归一化: %s", abs)
	}
}
