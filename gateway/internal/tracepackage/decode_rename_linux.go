//go:build linux

package tracepackage

import "golang.org/x/sys/unix"

func renameDecodeDirectoryNoReplace(fromFD int, from string, toFD int, to string) error {
	return unix.Renameat2(fromFD, from, toFD, to, unix.RENAME_NOREPLACE)
}
