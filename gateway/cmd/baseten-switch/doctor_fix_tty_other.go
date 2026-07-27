//go:build !darwin && !linux

package main

import "os"

// isTerminal falls back to the char-device heuristic on platforms
// without a termios probe wired up. Known limitation: /dev/null-style
// null devices stat as char devices and pass this check.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
