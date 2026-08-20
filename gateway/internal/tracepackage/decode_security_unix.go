//go:build darwin || linux

package tracepackage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type fileIdentity struct {
	device  uint64
	inode   uint64
	owner   uint32
	size    int64
	mode    fs.FileMode
	modNano int64
}

func identityFromFileInfo(info os.FileInfo) (fileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, errors.New("trace package decode: file identity is unavailable")
	}
	return fileIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), owner: stat.Uid,
		size: info.Size(), mode: info.Mode(), modNano: info.ModTime().UnixNano(),
	}, nil
}

func currentOwnerID() uint32 { return uint32(os.Getuid()) }

func pathHasDotDot(value string) bool {
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

func rejectSymlinkComponents(path string, includeFinal bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, volume+string(filepath.Separator))
	current := volume + string(filepath.Separator)
	parts := strings.Split(rest, string(filepath.Separator))
	for index, part := range parts {
		if part == "" {
			continue
		}
		if !includeFinal && index == len(parts)-1 {
			break
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("trace package decode: inspect path component: %w", err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return errors.New("trace package decode: path must not traverse a symlink")
		}
	}
	return nil
}

func openSecurePackage(value string) (*os.File, fileIdentity, string, error) {
	if strings.TrimSpace(value) == "" || value == "-" || pathHasDotDot(value) {
		return nil, fileIdentity{}, "", errors.New("trace package decode: invalid package path")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return nil, fileIdentity{}, "", err
	}
	parent, err := openSecureDecodeDirectory(filepath.Dir(abs))
	if err != nil {
		return nil, fileIdentity{}, "", err
	}
	defer parent.close()
	fd, err := unix.Openat(int(parent.file.Fd()), filepath.Base(abs), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fileIdentity{}, "", fmt.Errorf("trace package decode: open package without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), abs)
	after, err := file.Stat()
	visible, pathErr := os.Lstat(abs)
	componentErr := rejectSymlinkComponents(abs, true)
	if err != nil || pathErr != nil || componentErr != nil || !after.Mode().IsRegular() ||
		visible.Mode()&fs.ModeSymlink != 0 || !os.SameFile(after, visible) {
		_ = file.Close()
		return nil, fileIdentity{}, "", errors.New("trace package decode: package changed while opening")
	}
	if after.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, fileIdentity{}, "", errors.New("trace package decode: package must have owner-only permissions")
	}
	identity, err := identityFromFileInfo(after)
	if err != nil || identity.owner != currentOwnerID() || identity.size > MaxZIPBytes {
		_ = file.Close()
		if err != nil {
			return nil, fileIdentity{}, "", err
		}
		if identity.owner != currentOwnerID() {
			return nil, fileIdentity{}, "", errors.New("trace package decode: package must be owned by the current user")
		}
		return nil, fileIdentity{}, "", fmt.Errorf("%w: source ZIP", ErrLimitExceeded)
	}
	return file, identity, filepath.Clean(abs), nil
}

func sameIdentity(info os.FileInfo, identity fileIdentity) bool {
	got, err := identityFromFileInfo(info)
	return err == nil && got == identity
}

func sameObjectIdentity(info os.FileInfo, identity fileIdentity) bool {
	got, err := identityFromFileInfo(info)
	return err == nil && got.device == identity.device && got.inode == identity.inode &&
		got.owner == identity.owner && got.mode.Type() == identity.mode.Type()
}
