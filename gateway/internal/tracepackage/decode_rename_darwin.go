//go:build darwin

package tracepackage

import "golang.org/x/sys/unix"

func renameDecodeDirectoryNoReplace(fromFD int, from string, toFD int, to string) error {
	return unix.RenameatxNp(fromFD, from, toFD, to, unix.RENAME_EXCL)
}
