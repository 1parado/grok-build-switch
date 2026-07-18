//go:build !windows

package agentbridge

import (
	"os"
	"os/exec"
)

func configureCommand(_ *exec.Cmd) {}

func attachProcessTree(_ *os.Process) (func(), error) {
	return func() {}, nil
}
