//go:build unix

package agentfs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openForRead 打开已经过沙箱检查的文件。父目录用 O_NOFOLLOW 逐段打开，
// 最终文件名不再跟随符号链接；沙箱内的链接会先解析成真实路径再打开。
func (e Env) openForRead(abs string) (*os.File, error) {
	if err := e.refuseEscapingSymlink(abs); err != nil {
		return nil, err
	}
	real, err := sandboxReal(abs)
	if err != nil {
		return nil, err
	}
	dirfd, err := openDirNoFollow(filepath.Dir(real), false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirfd)
	if err := confirmDir(e, dirfd, filepath.Dir(real)); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(dirfd, filepath.Base(real), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, &SandboxError{Path: abs}
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), real), nil
}

// writeText 在解析后的父目录描述符里创建临时文件，再用 renameat 替换最终名字。
// rename 不跟随目标上的符号链接，所以最后一段被换成普通文件，而不是写到链接外面。
func (e Env) writeText(abs, content string) error {
	if err := e.refuseEscapingSymlink(abs); err != nil {
		return err
	}
	base := filepath.Base(abs)
	if !safeFinalName(base) {
		return &SandboxError{Path: abs}
	}
	parentReal, err := sandboxReal(filepath.Dir(abs))
	if err != nil {
		return err
	}
	if !e.withinResolved(parentReal) {
		return &SandboxError{Path: abs}
	}
	dirfd, err := openDirNoFollow(parentReal, true)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	if err := confirmDir(e, dirfd, parentReal); err != nil {
		return err
	}

	var tmp string
	var fd int
	for range 8 {
		tmp, err = tempBase()
		if err != nil {
			return err
		}
		fd, err = unix.Openat(dirfd, tmp, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
		if err == nil || !errors.Is(err, unix.EEXIST) {
			break
		}
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, writeErr := f.WriteString(content)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = unix.Unlinkat(dirfd, tmp, 0)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if err := unix.Renameat(dirfd, tmp, dirfd, base); err != nil {
		_ = unix.Unlinkat(dirfd, tmp, 0)
		return err
	}
	return nil
}

func (e Env) readDir(abs string) ([]os.DirEntry, error) {
	if err := e.refuseEscapingSymlink(abs); err != nil {
		return nil, err
	}
	real, err := sandboxReal(abs)
	if err != nil {
		return nil, err
	}
	if !e.withinResolved(real) {
		return nil, &SandboxError{Path: abs}
	}
	dirfd, err := openDirNoFollow(real, false)
	if err != nil {
		return nil, err
	}
	if err := confirmDir(e, dirfd, real); err != nil {
		unix.Close(dirfd)
		return nil, err
	}
	f := os.NewFile(uintptr(dirfd), real)
	defer f.Close()
	return f.ReadDir(-1)
}

// openDirNoFollow 从根开始逐段打开目录。某一段是符号链接时直接失败，
// 调用方握着的描述符不会在打开之后被换成另一个目录。
func openDirNoFollow(path string, create bool) (int, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return -1, fmt.Errorf("agentfs: 目录不是绝对路径: %s", path)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	rest := strings.TrimPrefix(path, string(filepath.Separator))
	if rest == "" {
		return fd, nil
	}
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			unix.Close(fd)
			return -1, &SandboxError{Path: path}
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil && create && errors.Is(openErr, os.ErrNotExist) {
			mkErr := unix.Mkdirat(fd, part, 0o755)
			if mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
				unix.Close(fd)
				return -1, mkErr
			}
			next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			unix.Close(fd)
			if errors.Is(openErr, unix.ELOOP) {
				return -1, &SandboxError{Path: path}
			}
			return -1, openErr
		}
		unix.Close(fd)
		fd = next
	}
	return fd, nil
}

// confirmDir 核对目录描述符仍然落在沙箱内。拿不到路径时比较设备和 inode。
func confirmDir(e Env, fd int, expect string) error {
	got, err := fdPath(fd)
	if err != nil {
		return confirmInode(fd, expect)
	}
	got = strings.TrimSuffix(got, " (deleted)")
	got = filepath.Clean(got)
	if !e.withinResolved(got) {
		return &SandboxError{Path: got}
	}
	return nil
}

func confirmInode(fd int, expect string) error {
	var stfd, stpath unix.Stat_t
	if err := unix.Fstat(fd, &stfd); err != nil {
		return err
	}
	if err := unix.Lstat(expect, &stpath); err != nil {
		return err
	}
	if stfd.Ino != stpath.Ino || stfd.Dev != stpath.Dev {
		return &SandboxError{Path: expect}
	}
	return nil
}

func safeFinalName(base string) bool {
	return base != "" && base != "." && base != ".." && !strings.ContainsRune(base, filepath.Separator)
}

func tempBase() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return ".agentfs-" + hex.EncodeToString(b[:]), nil
}
