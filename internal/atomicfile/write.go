// Package atomicfile replaces a file by publishing a complete temp file.
//
// On Windows os.Rename does not replace an existing destination. The previous
// local pattern deleted that destination first, so a failed second rename left
// config, profile, and credential files gone. This writer moves the previous
// file aside and puts it back if the new file cannot be published.
package atomicfile

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Options controls directory creation and the published file.
// A zero FileMode leaves the temp file's default mode in place.
type Options struct {
	DirMode  os.FileMode
	FileMode os.FileMode
	Sync     bool
}

// renameFile is replaced by tests that simulate a failed publish.
var renameFile = os.Rename

// Write writes data to path, replacing an existing file without deleting it
// before the replacement is in place.
func Write(path string, data []byte, opt Options) error {
	dirMode := opt.DirMode
	if dirMode == 0 {
		dirMode = 0o755
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if opt.FileMode != 0 {
		if err := tmp.Chmod(opt.FileMode); err != nil && runtime.GOOS != "windows" {
			tmp.Close()
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if opt.Sync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replace(tmpName, path); err != nil {
		return err
	}
	if opt.FileMode != 0 {
		_ = os.Chmod(path, opt.FileMode)
	}
	return nil
}

func replace(tmpName, path string) error {
	if err := renameFile(tmpName, path); err == nil {
		return nil
	}
	bak, err := backupPath(path)
	if err != nil {
		return err
	}
	if err := renameFile(path, bak); err != nil {
		if os.IsNotExist(err) {
			return renameFile(tmpName, path)
		}
		return err
	}
	if err := renameFile(tmpName, path); err != nil {
		if restoreErr := renameFile(bak, path); restoreErr != nil {
			return fmt.Errorf("replace %s: %w (previous content left at %s)", path, err, bak)
		}
		return err
	}
	_ = os.Remove(bak)
	return nil
}

func backupPath(path string) (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return path + ".bak-" + hex.EncodeToString(buf[:]), nil
}
