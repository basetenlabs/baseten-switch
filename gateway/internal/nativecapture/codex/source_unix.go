//go:build unix

package codex

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

type fileIdentity struct {
	device uint64
	inode  uint64
}

func identityFromInfo(info os.FileInfo) (fileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, errors.New("cannot determine native source identity")
	}
	if int(stat.Uid) != os.Getuid() {
		return fileIdentity{}, errors.New("native source is not owned by the current user")
	}
	return fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}

func openSourceNoFollow(path string) (*os.File, os.FileInfo, fileIdentity, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fileIdentity{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fileIdentity{}, errors.New("native source must be a regular non-symlink file")
	}
	identity, err := identityFromInfo(before)
	if err != nil {
		return nil, nil, fileIdentity{}, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fileIdentity{}, fmt.Errorf("open native source: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, nil, fileIdentity{}, errors.New("open native source")
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, nil, fileIdentity{}, errors.New("native source changed while opening")
	}
	afterIdentity, err := identityFromInfo(after)
	if err != nil || afterIdentity != identity {
		_ = file.Close()
		return nil, nil, fileIdentity{}, errors.New("native source identity changed while opening")
	}
	return file, after, identity, nil
}
