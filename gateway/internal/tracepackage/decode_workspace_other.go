//go:build !darwin && !linux

package tracepackage

import (
	"errors"
	"os"
)

type decodeWorkspace struct {
	root      *os.Root
	stagePath string
}

func createDecodeWorkspace(string) (*decodeWorkspace, error) {
	return nil, errors.New("trace package decode: descriptor-relative output is unsupported")
}
func randomDecodeStageName() (string, error) {
	return "", errors.New("trace package decode: descriptor-relative output is unsupported")
}

func (workspace *decodeWorkspace) cleanup() error { return nil }
func (workspace *decodeWorkspace) publish() error {
	return errors.New("trace package decode: descriptor-relative output is unsupported")
}
