//go:build !unix

package agentfs

import (
	"os"

	"grok_switch/internal/atomicfile"
)

func (e Env) openForRead(abs string) (*os.File, error) {
	if err := e.refuseEscapingSymlink(abs); err != nil {
		return nil, err
	}
	return os.Open(abs)
}

// writeText 先拒绝越界符号链接，再做原子替换。Windows 没有 openat，
// 目录替换沿用 atomicfile，避免写失败时把原文件挪走。
func (e Env) writeText(abs, content string) error {
	if err := e.refuseEscapingSymlink(abs); err != nil {
		return err
	}
	return atomicfile.Write(abs, []byte(content), atomicfile.Options{DirMode: 0o755})
}

func (e Env) readDir(abs string) ([]os.DirEntry, error) {
	if err := e.refuseEscapingSymlink(abs); err != nil {
		return nil, err
	}
	return os.ReadDir(abs)
}
