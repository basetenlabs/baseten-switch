//go:build darwin || linux

package tracepackage

import (
	"os"
	"syscall"
)

func openNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(
		path,
		syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
