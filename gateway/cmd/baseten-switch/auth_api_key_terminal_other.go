//go:build !darwin && !linux

package main

import (
	"fmt"
	"io"
	"os"
)

func readAPIKeyTerminal(_ *os.File, _ io.Writer, _ int) ([]byte, error) {
	return nil, fmt.Errorf("hidden terminal input is unavailable; redirect a key file into stdin")
}
