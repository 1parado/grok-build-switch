//go:build !windows

package registrar

import "os/exec"

func hidePythonConsole(cmd *exec.Cmd) {}
