//go:build linux

package agentfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

func fdPath(fd int) (string, error) {
	p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("agentfs: fd path %q", p)
	}
	return p, nil
}
