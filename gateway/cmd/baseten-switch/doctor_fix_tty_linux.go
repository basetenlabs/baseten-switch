//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is an interactive terminal, using the
// POSIX isatty(3) probe: tcgetattr (TCGETS on linux) succeeds only on
// a terminal. A ModeCharDevice check is NOT sufficient: /dev/null is
// a character device, so `doctor --fix </dev/null` would wrongly
// count as interactive. Stdlib syscall only (dependency policy).
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
