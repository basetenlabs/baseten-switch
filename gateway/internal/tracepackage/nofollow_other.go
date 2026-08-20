//go:build !darwin && !linux

package tracepackage

import (
	"errors"
	"os"
)

func openNoFollow(string) (*os.File, error) {
	return nil, errors.New("no-follow file opens are unsupported on this platform")
}
