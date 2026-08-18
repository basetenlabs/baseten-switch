//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package main

import (
	"errors"
	"os"
)

func openTraceSelectorNoFollow(string) (*os.File, os.FileInfo, error) {
	return nil, nil, errors.New("native session selector identity checks are unsupported on this platform")
}
