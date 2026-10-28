//go:build windows

package grokpool

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// syscall 没有导出 ERROR_SHARING_VIOLATION，数值与 Win32 一致。
const errorSharingViolation syscall.Errno = 32

// Go 的 testing.TempDir 只对 Access Denied 和 Sharing Violation 重试两秒。
// Windows 上杀毒软件会让刚写完的文件处于 delete-pending，RemoveAll 直接得到
// ERROR_DIR_NOT_EMPTY，测试因此失败。这里把这三种错误都再试几秒。
func removeTree(path string) error {
	deadline := time.Now().Add(5 * time.Second)
	var err error
	for {
		err = os.RemoveAll(path)
		if err == nil || !windowsRemoveRetryable(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func windowsRemoveRetryable(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ERROR_ACCESS_DENIED, syscall.ERROR_DIR_NOT_EMPTY, errorSharingViolation:
		return true
	default:
		return false
	}
}
