// Package agentfs 是 Agent 的执行环境抽象（对标 Kimi Code 的 kaos 最小集）。
//
// 工具（internal/tools）只通过本包接口触碰文件系统与进程，local 实现即宿主机。
// 远期 SSH/容器执行环境在此接口后替换，工具代码不变（设计文档 §5）。
package agentfs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Env 是一次工具执行的上下文：工作目录 + 环境抽象。
type Env struct {
	// Cwd 是本会话允许操作的根目录（用户在 UI 里选择的工作目录）。
	Cwd string
	// SandboxExtra 是额外允许访问的绝对路径（如临时目录）；空则只有 Cwd。
	SandboxExtra []string
}

// Resolve 把相对/带 ~ 的路径解析为绝对路径并归一化。
func (e Env) Resolve(path string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return e.Cwd, nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p[1:], "/"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.Cwd, p)
	}
	return filepath.Clean(p), nil
}

// WithinSandbox 判断绝对路径是否落在允许范围内（Cwd 或 SandboxExtra 的前缀内）。
// Windows 大小写不敏感，这里按大小写折叠比较。
func (e Env) WithinSandbox(abs string) bool {
	abs = filepath.Clean(abs)
	if withinRoot(e.Cwd, abs) {
		return true
	}
	for _, extra := range e.SandboxExtra {
		if withinRoot(extra, abs) {
			return true
		}
	}
	return false
}

func withinRoot(root, target string) bool {
	if root == "" {
		return false
	}
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// SandboxError 是越界访问错误。
type SandboxError struct{ Path string }

func (e *SandboxError) Error() string {
	return fmt.Sprintf("路径越出工作目录沙箱: %s", e.Path)
}

// Guard 解析路径并强制沙箱。所有读写工具入口必须先走它。
func (e Env) Guard(path string) (string, error) {
	abs, err := e.Resolve(path)
	if err != nil {
		return "", err
	}
	if !e.WithinSandbox(abs) {
		return "", &SandboxError{Path: abs}
	}
	return abs, nil
}

// ReadText 读取文件（上限 maxBytes；0 表示默认 2MB）。
func (e Env) ReadText(abs string, maxBytes int64) (string, error) {
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.Size() > maxBytes {
		return "", fmt.Errorf("文件过大 (%d 字节)，上限 %d", st.Size(), maxBytes)
	}
	buf := make([]byte, st.Size())
	if _, err := f.Read(buf); err != nil && st.Size() > 0 {
		return "", err
	}
	return string(buf), nil
}

// WriteText 原子写入：写临时文件后 rename。
func (e Env) WriteText(abs, content string) error {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".agentfs-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.WriteString(content)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(tmpName)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	return os.Rename(tmpName, abs)
}

// ListDir 返回目录条目名（排序）。
func (e Env) ListDir(abs string) ([]string, error) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

// StatResult 是文件元信息。
type StatResult struct {
	Exists  bool
	IsDir   bool
	Size    int64
	ModTime int64 // Unix 秒
}

// Stat 探测路径。
func (e Env) Stat(abs string) StatResult {
	st, err := os.Stat(abs)
	if err != nil {
		return StatResult{}
	}
	return StatResult{Exists: true, IsDir: st.IsDir(), Size: st.Size(), ModTime: st.ModTime().Unix()}
}
