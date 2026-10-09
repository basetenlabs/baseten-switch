//go:build darwin || linux

package main

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
	"golang.org/x/sys/unix"
)

func TestSavedAPIKeyTerminalCommandEnter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "absent.pid"))
	master, slave := apiKeyTestPTY(t)
	result := make(chan int, 1)
	go func() { result <- cmdAuthAPIKey([]string{"set"}, slave, slave, slave) }()
	if err := master.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(master)
	if prompt, err := reader.ReadString(':'); err != nil || !strings.Contains(prompt, "input hidden") {
		t.Fatalf("missing command prompt: %v", err)
	}
	const key = "synthetic-terminal-key"
	if _, err := master.Write([]byte(key + "\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-result:
		if code != 0 {
			t.Fatalf("set returned %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Enter did not complete the command")
	}
	if got, err := auth.LoadSavedAPIKey(path); err != nil || got != key {
		t.Fatal("terminal key was not saved")
	}
	for i := 0; i < 3; i++ {
		line, err := reader.ReadString('\n')
		if err != nil || strings.Contains(line, key) {
			t.Fatal("key echoed or expected output missing")
		}
	}
}

func TestAPIKeyTerminalInput(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		interrupt         bool
		wantError         bool
	}{
		{name: "Enter saves", input: "synthetic-key\n", want: "synthetic-key"},
		{name: "long key is not truncated", input: strings.Repeat("x", 2048) + "\n", want: strings.Repeat("x", 2048)},
		{name: "backspace", input: "synthetic-xy\x7fz\n", want: "synthetic-xz"},
		{name: "Unicode backspace", input: "synthetic-é\x7fz\n", want: "synthetic-z"},
		{name: "flush paste after newline", input: "synthetic-key\nunused-paste\n", want: "synthetic-key"},
		{name: "clear line", input: "discard\x15synthetic-key\n", want: "synthetic-key"},
		{name: "empty line", input: "\n"},
		{name: "EOF cancels", input: "partial\x04", wantError: true},
		{name: "interrupt cancels", interrupt: true, wantError: true},
		{name: "oversized", input: strings.Repeat("x", 4097), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := apiKeyTestPTY(t)
			original, err := unix.IoctlGetTermios(int(slave.Fd()), apiKeyGetTermios)
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan struct {
				key []byte
				err error
			}, 1)
			go func() {
				key, err := readAPIKeyTerminal(slave, slave, 4096)
				result <- struct {
					key []byte
					err error
				}{key, err}
			}()
			if err := master.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(master)
			if prompt, err := reader.ReadString(':'); err != nil || !strings.Contains(prompt, "input hidden") {
				t.Fatalf("missing prompt: %v", err)
			}
			if tc.interrupt {
				if err := unix.Kill(os.Getpid(), unix.SIGINT); err != nil {
					t.Fatal(err)
				}
			} else if _, err := master.Write([]byte(tc.input)); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-result:
				if (got.err != nil) != tc.wantError || string(got.key) != tc.want {
					t.Fatal("unexpected input result")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("terminal input did not finish")
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), apiKeyGetTermios)
			if err != nil || !reflect.DeepEqual(original, after) {
				t.Fatal("terminal settings not restored")
			}
			fds := []unix.PollFd{{Fd: int32(slave.Fd()), Events: unix.POLLIN}}
			if n, err := unix.Poll(fds, 0); err != nil || n != 0 {
				t.Fatal("unread input was left for the shell")
			}
			if output, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(output) != "" {
				t.Fatal("input was echoed or terminal newline missing")
			}
		})
	}
}
