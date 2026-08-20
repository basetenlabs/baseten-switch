//go:build darwin || linux

package claude

import (
	"fmt"
	"os"
	"syscall"
)

type fileIdentity struct {
	device uint64
	inode  uint64
	owner  uint32
}

func validateSourceDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("Claude Code projects root is not a non-symlink directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("Claude Code projects root is not owned by the current user")
	}
	return nil
}

func inspectSourceFile(path string) (os.FileInfo, fileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fileIdentity{}, fmt.Errorf("source is not a non-symlink regular file")
	}
	identity, err := identityFromInfo(info)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	if identity.owner != uint32(os.Getuid()) {
		return nil, fileIdentity{}, fmt.Errorf("source is not owned by the current user")
	}
	return info, identity, nil
}

func openSourceNoFollow(path string) (*os.File, os.FileInfo, fileIdentity, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fileIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fileIdentity{}, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fileIdentity{}, fmt.Errorf("source is not regular")
	}
	identity, err := identityFromInfo(info)
	if err != nil || identity.owner != uint32(os.Getuid()) {
		_ = file.Close()
		return nil, nil, fileIdentity{}, fmt.Errorf("source identity is unsafe")
	}
	return file, info, identity, nil
}

func identityFromInfo(info os.FileInfo) (fileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, fmt.Errorf("unsupported source identity")
	}
	return fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino), owner: stat.Uid}, nil
}
