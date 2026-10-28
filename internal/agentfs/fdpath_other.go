//go:build unix && !linux && !darwin

package agentfs

import "errors"

func fdPath(fd int) (string, error) {
	return "", errors.New("agentfs: fd path unavailable")
}
