package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStickyConfigPath(t *testing.T) {
	dir := t.TempDir()
	cfgReal := filepath.Join(dir, "gateway.yaml")
	writeFile(t, cfgReal, "global:\n  routing_enabled: false\n")
	cfgGone := filepath.Join(dir, "deleted.yaml")

	stateWith := func(content string) string {
		p := filepath.Join(t.TempDir(), "gateway.config-path")
		writeFile(t, p, content)
		return p
	}

	cases := []struct {
		name        string
		explicitEnv string
		statePath   string
		wantPath    string
		wantNotice  string // substring; "" = no notice
	}{
		{
			name:        "explicit env wins over state file",
			explicitEnv: "/explicit/gateway.yaml",
			statePath:   stateWith(cfgReal + "\n"),
			wantPath:    "",
			wantNotice:  "",
		},
		{
			name:       "state file missing falls through silently",
			statePath:  filepath.Join(t.TempDir(), "no-such-state"),
			wantPath:   "",
			wantNotice: "",
		},
		{
			name:       "state file empty falls through silently",
			statePath:  stateWith("\n"),
			wantPath:   "",
			wantNotice: "",
		},
		{
			name:       "state file points at readable config: reuse with notice",
			statePath:  stateWith(cfgReal + "\n"),
			wantPath:   cfgReal,
			wantNotice: "reusing config path",
		},
		{
			name:       "state file points at deleted config: warn and fall through",
			statePath:  stateWith(cfgGone + "\n"),
			wantPath:   "",
			wantNotice: "missing or not a regular file",
		},
		{
			name:       "state file points at a directory: warn and fall through",
			statePath:  stateWith(dir + "\n"),
			wantPath:   "",
			wantNotice: "missing or not a regular file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPath, gotNotice := stickyConfigPath(tc.explicitEnv, tc.statePath)
			if gotPath != tc.wantPath {
				t.Errorf("path = %q want %q", gotPath, tc.wantPath)
			}
			if tc.wantNotice == "" && gotNotice != "" {
				t.Errorf("unexpected notice: %q", gotNotice)
			}
			if tc.wantNotice != "" && !strings.Contains(gotNotice, tc.wantNotice) {
				t.Errorf("notice %q does not contain %q", gotNotice, tc.wantNotice)
			}
		})
	}
}

func TestStickyConfigPathUnreadableConfig(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; permission checks are bypassed")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "locked.yaml")
	writeFile(t, cfg, "global: {}\n")
	if err := os.Chmod(cfg, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg, 0o644) })
	state := filepath.Join(dir, "gateway.config-path")
	writeFile(t, state, cfg+"\n")
	gotPath, gotNotice := stickyConfigPath("", state)
	if gotPath != "" {
		t.Errorf("expected fall-through for unreadable config, got %q", gotPath)
	}
	if !strings.Contains(gotNotice, "not readable") {
		t.Errorf("notice %q does not mention unreadable config", gotNotice)
	}
}

func TestStickyConfigPathUnreadableStateFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; permission checks are bypassed")
	}
	state := filepath.Join(t.TempDir(), "gateway.config-path")
	writeFile(t, state, "/whatever\n")
	if err := os.Chmod(state, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o644) })
	gotPath, gotNotice := stickyConfigPath("", state)
	if gotPath != "" {
		t.Errorf("expected fall-through for unreadable state file, got %q", gotPath)
	}
	if !strings.Contains(gotNotice, "unreadable") {
		t.Errorf("notice %q does not mention unreadable state file", gotNotice)
	}
}

func deadPid() int {
	pid := 9999999
	for pid == os.Getpid() {
		pid++
	}
	return pid
}

func TestClassifyPidfile(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T) string // returns pidfile path
		want    pidfileState
		wantPid int
	}{
		{
			name:  "missing pidfile",
			setup: func(t *testing.T) string { return filepath.Join(t.TempDir(), "gw.pid") },
			want:  pidfileMissing,
		},
		{
			name: "corrupt pidfile",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "gw.pid")
				writeFile(t, p, "not-a-number\n")
				return p
			},
			want: pidfileCorrupt,
		},
		{
			name: "dead pid",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "gw.pid")
				writeFile(t, p, fmt.Sprintf("%d\n", deadPid()))
				return p
			},
			want:    pidfileDead,
			wantPid: deadPid(),
		},
		{
			name: "alive pid",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "gw.pid")
				writeFile(t, p, fmt.Sprintf("%d\n", os.Getpid()))
				return p
			},
			want:    pidfileAlive,
			wantPid: os.Getpid(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, pid := classifyPidfile(tc.setup(t))
			if state != tc.want {
				t.Errorf("state = %d want %d", state, tc.want)
			}
			if pid != tc.wantPid {
				t.Errorf("pid = %d want %d", pid, tc.wantPid)
			}
		})
	}
}
