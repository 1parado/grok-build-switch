//go:build !windows

package grokpool

import "os"

func removeTree(path string) error {
	return os.RemoveAll(path)
}
