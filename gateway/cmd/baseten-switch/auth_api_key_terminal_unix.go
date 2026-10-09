//go:build darwin || linux

package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Read a bounded line without echo or the terminal's canonical line-length limit.
// Restore with input flushing so an unfinished paste cannot reach the shell.
func readAPIKeyTerminal(input *os.File, prompt io.Writer, limit int) (key []byte, err error) {
	fd := int(input.Fd())
	original, err := unix.IoctlGetTermios(fd, apiKeyGetTermios)
	if err != nil {
		return nil, err
	}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT, unix.SIGTSTP)
	defer signal.Stop(interrupts)
	state := *original
	state.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.IEXTEN
	state.Cc[unix.VMIN] = 1
	state.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, apiKeySetTermios, &state); err != nil {
		return nil, err
	}
	defer func() {
		if restoreErr := unix.IoctlSetTermios(fd, apiKeySetTermios, original); restoreErr != nil {
			key, err = nil, restoreErr
		}
		fmt.Fprintln(prompt)
	}()
	if _, err := fmt.Fprint(prompt, "API key (input hidden; Enter to save): "); err != nil {
		return nil, err
	}
	var b [1]byte
	for {
		select {
		case <-interrupts:
			return nil, fmt.Errorf("input canceled")
		default:
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR || n == 0 && err == nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		n, err = unix.Read(fd, b[:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.EOF
		}
		switch b[0] {
		case '\n', '\r':
			return key, nil
		case 3, 4, 26: // Cancel on Ctrl+C, Ctrl+D, or Ctrl+Z even with ISIG disabled.
			return nil, fmt.Errorf("input canceled")
		case 8, 127:
			_, size := utf8.DecodeLastRune(key)
			key = key[:len(key)-size]
		case 21: // Ctrl+U clears the line.
			key = key[:0]
		default:
			if len(key) == limit {
				return nil, fmt.Errorf("input exceeds %d bytes", limit)
			}
			key = append(key, b[0])
		}
	}
}
