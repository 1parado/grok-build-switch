//go:build unix

package mcpserver

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// noticeStdinClosed 看到 POLLHUP 就取消。不读取 stdin，避免和主循环的 bufio 抢数据。
func noticeStdinClosed(ctx context.Context, stdin *os.File, cancel context.CancelFunc) {
	fd := int32(stdin.Fd())
	for {
		if ctx.Err() != nil {
			return
		}
		fds := []unix.PollFd{{
			Fd:     fd,
			Events: unix.POLLHUP | unix.POLLERR,
		}}
		_, err := unix.Poll(fds, 200)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return
		}
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			cancel()
			return
		}
	}
}
