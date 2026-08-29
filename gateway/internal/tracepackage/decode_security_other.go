//go:build !darwin && !linux

package tracepackage

import (
	"errors"
	"io/fs"
	"os"
)

type fileIdentity struct {
	size int64
	mode fs.FileMode
}

func identityFromFileInfo(os.FileInfo) (fileIdentity, error) {
	return fileIdentity{}, errors.New("trace package decode: secure identity checks are unsupported")
}
func openSecurePackage(string) (*os.File, fileIdentity, string, error) {
	return nil, fileIdentity{}, "", errors.New("trace package decode: secure package opens are unsupported")
}
func sameIdentity(os.FileInfo, fileIdentity) bool       { return false }
func sameObjectIdentity(os.FileInfo, fileIdentity) bool { return false }
func pathHasDotDot(string) bool                         { return true }
func rejectSymlinkComponents(string, bool) error {
	return errors.New("trace package decode: secure path checks are unsupported")
}
