//go:build windows

package registrar

import (
	"os/exec"
	"syscall"
)

func hidePythonConsole(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
