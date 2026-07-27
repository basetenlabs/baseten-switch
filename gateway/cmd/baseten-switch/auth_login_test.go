package main

// auth login verb tests. Hermetic: the baseten CLI is a fixture-written
// shell script on a PATH confined to a temp dir (so the real exec path,
// including inherited stdio and exit-code passthrough, is exercised
// without any host binary), the SIGHUP goes through the signalRouter
// seam, and the identity print goes through printLoginIdentity so no
// network call ever happens.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
)

// fakeBasetenCLI writes an executable baseten script into a fresh dir
// and confines PATH to it. loginExit is the script's exit code for
// `auth login`. The --version probe is stubbed at the
// basetenCLIVersion seam instead of exec'ing the script: the probe is
// not the behavior under test, and the real exec's 2s timeout flaked
// under full-suite parallel load (seen 2026-07-13/14).
func fakeBasetenCLI(t *testing.T, loginExit int) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "baseten")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then echo "baseten 9.9.9"; exit 0; fi
if [ "$1" = "auth" ] && [ "$2" = "login" ]; then echo "device flow"; exit %d; fi
exit 64
`, loginExit)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	oldVersion := basetenCLIVersion
	basetenCLIVersion = func(string) string { return "baseten 9.9.9" }
	t.Cleanup(func() { basetenCLIVersion = oldVersion })
	return p
}

// setAuthLoginSeams replaces the SIGHUP and identity-print seams with
// recorders and returns pointers to what they saw.
func setAuthLoginSeams(t *testing.T) (signaled *[]int, identityCalls *int) {
	t.Helper()
	pids := &[]int{}
	calls := new(int)
	oldSignal := signalRouter
	signalRouter = func(pid int) error {
		*pids = append(*pids, pid)
		return nil
	}
	oldIdentity := printLoginIdentity
	printLoginIdentity = func() int {
		*calls++
		return 0
	}
	t.Cleanup(func() {
		signalRouter = oldSignal
		printLoginIdentity = oldIdentity
	})
	return pids, calls
}

func TestAuthLoginSuccessSighupsRouterAndPrintsIdentity(t *testing.T) {
	fakeBasetenCLI(t, 0)
	pids, calls := setAuthLoginSeams(t)
	pf := filepath.Join(t.TempDir(), "gw.pid")
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", pf)
	if err := pidfile.WriteAt(pf, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	var rc int
	errOut := captureStderr(t, func() { rc = cmdAuth([]string{"login"}) })
	if rc != 0 {
		t.Fatalf("cmdAuth login = %d, want 0\nstderr:\n%s", rc, errOut)
	}
	if len(*pids) != 1 || (*pids)[0] != os.Getpid() {
		t.Errorf("signaled pids = %v, want exactly [%d]", *pids, os.Getpid())
	}
	if *calls != 1 {
		t.Errorf("identity printed %d times, want 1", *calls)
	}
	if !strings.Contains(errOut, "router reloaded (SIGHUP") {
		t.Errorf("stderr missing the SIGHUP confirmation:\n%s", errOut)
	}
	// The verb names the binary and version that wrote the store (the
	// two-binary hazard).
	if !strings.Contains(errOut, "baseten 9.9.9") {
		t.Errorf("stderr missing the CLI version line:\n%s", errOut)
	}
}

func TestAuthLoginNoBinaryPrintsBrewHint(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir: no baseten anywhere
	pids, calls := setAuthLoginSeams(t)

	var rc int
	errOut := captureStderr(t, func() { rc = cmdAuth([]string{"login"}) })
	if rc != 1 {
		t.Fatalf("cmdAuth login = %d, want 1\nstderr:\n%s", rc, errOut)
	}
	if !strings.Contains(errOut, basetenBrewHint) {
		t.Errorf("stderr missing the brew install hint:\n%s", errOut)
	}
	if len(*pids) != 0 || *calls != 0 {
		t.Errorf("no login ran, but signals = %v identity calls = %d", *pids, *calls)
	}
}

func TestAuthLoginFailurePassesExitCodeThrough(t *testing.T) {
	fakeBasetenCLI(t, 3)
	pids, calls := setAuthLoginSeams(t)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "gw.pid"))

	var rc int
	captureStderr(t, func() { rc = cmdAuth([]string{"login"}) })
	if rc != 3 {
		t.Fatalf("cmdAuth login = %d, want the CLI's exit code 3", rc)
	}
	if len(*pids) != 0 {
		t.Errorf("failed login must not SIGHUP the router; signals = %v", *pids)
	}
	if *calls != 0 {
		t.Errorf("failed login must not print an identity; calls = %d", *calls)
	}
}

func TestAuthLoginRouterDownStillPrintsIdentity(t *testing.T) {
	fakeBasetenCLI(t, 0)
	pids, calls := setAuthLoginSeams(t)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "gw.pid")) // no pidfile: router down

	var rc int
	errOut := captureStderr(t, func() { rc = cmdAuth([]string{"login"}) })
	if rc != 0 {
		t.Fatalf("cmdAuth login = %d, want 0\nstderr:\n%s", rc, errOut)
	}
	if !strings.Contains(errOut, "router not running") {
		t.Errorf("stderr missing the router-down notice:\n%s", errOut)
	}
	if len(*pids) != 0 {
		t.Errorf("no router to signal, but signals = %v", *pids)
	}
	if *calls != 1 {
		t.Errorf("identity printed %d times, want 1", *calls)
	}
}

func TestAuthUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"login", "extra"}} {
		var rc int
		captureStderr(t, func() { rc = cmdAuth(args) })
		if rc != 2 {
			t.Errorf("cmdAuth(%v) = %d, want usage error 2", args, rc)
		}
	}
}
