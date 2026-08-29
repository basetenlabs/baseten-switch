//go:build !darwin && !linux

package claude

import (
	"errors"
	"os"
)

type fileIdentity struct{}

func validateSourceDirectory(string) error {
	return errors.New("Claude Code native capture requires file identity support")
}

func inspectSourceFile(string) (os.FileInfo, fileIdentity, error) {
	return nil, fileIdentity{}, errors.New("Claude Code native capture requires file identity support")
}

func openSourceNoFollow(string) (*os.File, os.FileInfo, fileIdentity, error) {
	return nil, nil, fileIdentity{}, errors.New("Claude Code native capture requires file identity support")
}
