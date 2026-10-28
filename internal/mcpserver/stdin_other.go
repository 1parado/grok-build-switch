//go:build !unix

package mcpserver

import (
	"context"
	"os"
)

// noticeStdinClosed 在非 Unix 上只等超时。这里没有可移植的 POLLHUP。
func noticeStdinClosed(ctx context.Context, _ *os.File, _ context.CancelFunc) {
	<-ctx.Done()
}
