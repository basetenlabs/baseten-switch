//go:build !unix

package codex

import (
	"errors"
	"os"
)

type fileIdentity struct{}

func identityFromInfo(os.FileInfo) (fileIdentity, error) {
	return fileIdentity{}, errors.New("Codex native capture is unsupported on this platform")
}

func openSourceNoFollow(string) (*os.File, os.FileInfo, fileIdentity, error) {
	return nil, nil, fileIdentity{}, errors.New("Codex native capture is unsupported on this platform")
}
