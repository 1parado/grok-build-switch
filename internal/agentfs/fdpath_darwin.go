//go:build darwin

package agentfs

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

func fdPath(fd int) (string, error) {
	buf := make([]byte, 1024)
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), uintptr(unix.F_GETPATH), uintptr(unsafe.Pointer(&buf[0])))
	runtime.KeepAlive(buf)
	if errno != 0 {
		return "", errno
	}
	return unix.ByteSliceToString(buf), nil
}
