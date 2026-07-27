package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

// fakeCmds swaps the exec seam for a recorder. versions maps an
// Info.plist path to the version plutil should report; empty string
// means plutil errors for that path.
func fakeCmds(t *testing.T, versions map[string]string) *[]string {
	t.Helper()
	var calls []string
	old := runCmd
	runCmd = func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case strings.HasSuffix(name, "plutil"):
			v := versions[args[len(args)-1]]
			if v == "" {
				return "", os.ErrNotExist
			}
			return v, nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { runCmd = old })
	return &calls
}

func mkApp(t *testing.T, dir string) string {
	t.Helper()
	app := filepath.Join(dir, menubarAppName)
	if err := os.MkdirAll(filepath.Join(app, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return app
}

func plist(app string) string { return filepath.Join(app, "Contents", "Info.plist") }

func writeMenubarPayload(t *testing.T, payload string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(payload), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(payload)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	entries := []struct {
		name string
		mode os.FileMode
	}{
		{menubarAppName + "/Contents/Info.plist", 0o644},
		{menubarAppName + "/Contents/MacOS/" + menubarProcName, 0o755},
	}
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		header.SetMode(entry.mode)
		writer, createErr := zw.CreateHeader(header)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := writer.Write([]byte("test")); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCmdMenubarHomebrewPayloadFirstInstall(t *testing.T) {
	forceMenubarGOOS(t, "darwin")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASETEN_SWITCH_MENUBAR_APP", "")

	root := t.TempDir()
	prefix := filepath.Join(root, "Cellar", "baseten-switch", "0.2.0")
	executable := filepath.Join(prefix, "bin", "baseten-switch")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(prefix, "share", "baseten-switch", menubarPayloadName)
	writeMenubarPayload(t, payload)

	oldExecutable := menubarExecutable
	menubarExecutable = func() (string, error) { return executable, nil }
	t.Cleanup(func() { menubarExecutable = oldExecutable })
	oldKegs := menubarKegPaths
	menubarKegPaths = nil
	t.Cleanup(func() { menubarKegPaths = oldKegs })

	var calls []string
	oldRunCmd := runCmd
	runCmd = func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case strings.HasSuffix(name, "ditto"):
			stage := args[len(args)-1]
			app := filepath.Join(stage, menubarAppName)
			if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(plist(app), []byte("plist"), 0o644); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(app, "Contents", "MacOS", menubarProcName), []byte("app"), 0o755)
		case strings.HasSuffix(name, "plutil"):
			switch args[1] {
			case "CFBundleIdentifier":
				return "co.baseten.switch", nil
			case "CFBundleDisplayName":
				return "Baseten Switch", nil
			case "CFBundleExecutable":
				return menubarProcName, nil
			case "CFBundleShortVersionString":
				return "0.2.0", nil
			}
		case strings.HasSuffix(name, "codesign"), strings.HasSuffix(name, "open"):
			return "", nil
		case strings.HasSuffix(name, "pgrep"):
			return "", errors.New("exit status 1")
		}
		return "", nil
	}
	t.Cleanup(func() { runCmd = oldRunCmd })

	if rc := cmdMenubar(nil); rc != 0 {
		t.Fatalf("rc = %d want 0; calls:\n%s", rc, strings.Join(calls, "\n"))
	}
	resolvedPayload, _, ok := homebrewMenubarPayload()
	if !ok {
		t.Fatal("test executable did not resolve as Homebrew")
	}
	dst := filepath.Join(home, "Applications", menubarAppName)
	if _, err := os.Stat(filepath.Join(dst, "Contents", "MacOS", menubarProcName)); err != nil {
		t.Fatalf("materialized app missing: %v", err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"ditto -x -k " + resolvedPayload, "codesign --verify --deep --strict", "open " + dst} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in calls:\n%s", want, joined)
		}
	}
}

func TestHomebrewMenubarPayloadResolvesExecutableSymlink(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "Cellar", "baseten-switch", "1.2.3")
	executable := filepath.Join(prefix, "bin", "baseten-switch")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin", "baseten-switch")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	oldExecutable := menubarExecutable
	menubarExecutable = func() (string, error) { return link, nil }
	t.Cleanup(func() { menubarExecutable = oldExecutable })

	got, gotPrefix, ok := homebrewMenubarPayload()
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.Dir(filepath.Dir(resolvedExecutable))
	want := filepath.Join(wantPrefix, "share", "baseten-switch", menubarPayloadName)
	if !ok || got != want || gotPrefix != wantPrefix {
		t.Fatalf("payload, prefix, homebrew = %q, %q, %v; want %q, %q, true", got, gotPrefix, ok, want, wantPrefix)
	}
}

func TestPreflightMenubarPayloadRejectsTraversal(t *testing.T) {
	payload := filepath.Join(t.TempDir(), menubarPayloadName)
	file, err := os.Create(payload)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	writer, err := zw.Create("../escape")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("bad")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := preflightMenubarPayload(payload); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("err = %v, want unsafe path", err)
	}
}

func TestCmdMenubar(t *testing.T) {
	if os.Getenv("HOME") == "" {
		t.Skip("needs HOME")
	}
	cases := []struct {
		name      string
		setup     func(t *testing.T, home string) (versions map[string]string)
		envApp    func(t *testing.T) string // returns BASETEN_SWITCH_MENUBAR_APP value or ""
		noKeg     bool
		wantRC    int
		wantCalls []string // substrings expected in order-insensitive fashion
		skipCalls []string // substrings that must NOT appear
	}{
		{
			name: "keg present, no home copy: installs and opens",
			setup: func(t *testing.T, home string) map[string]string {
				return map[string]string{} // versions unused: dst absent forces refresh
			},
			wantRC:    0,
			wantCalls: []string{"ditto", "open"},
		},
		{
			name: "versions equal: opens without copying",
			setup: func(t *testing.T, home string) map[string]string {
				dst := mkApp(t, filepath.Join(home, "Applications"))
				return map[string]string{plist(dst): "0.1.2", "KEGPLIST": "0.1.2"}
			},
			wantRC:    0,
			wantCalls: []string{"open"},
			skipCalls: []string{"ditto"},
		},
		{
			name: "keg newer: refreshes the copy",
			setup: func(t *testing.T, home string) map[string]string {
				dst := mkApp(t, filepath.Join(home, "Applications"))
				return map[string]string{plist(dst): "0.1.1", "KEGPLIST": "0.1.2"}
			},
			wantRC:    0,
			wantCalls: []string{"ditto", "open"},
		},
		{
			name:   "no keg, no copy: install hint error",
			noKeg:  true,
			setup:  func(t *testing.T, home string) map[string]string { return nil },
			wantRC: 1,
			skipCalls: []string{
				"open", "ditto",
			},
		},
		{
			name:  "env override opens in place",
			setup: func(t *testing.T, home string) map[string]string { return nil },
			envApp: func(t *testing.T) string {
				return mkApp(t, t.TempDir())
			},
			wantRC:    0,
			wantCalls: []string{"open"},
			skipCalls: []string{"ditto"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The darwin-only gate sits ahead of the open/refresh flow
			// under test; force it so the flow runs on any host.
			forceMenubarGOOS(t, "darwin")
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("BASETEN_SWITCH_MENUBAR_APP", "")
			os.Unsetenv("BASETEN_SWITCH_MENUBAR_APP")

			versions := map[string]string{}
			if tc.setup != nil {
				if v := tc.setup(t, home); v != nil {
					versions = v
				}
			}

			oldKegs := menubarKegPaths
			if tc.noKeg {
				menubarKegPaths = []string{filepath.Join(t.TempDir(), "absent", menubarAppName)}
			} else {
				keg := mkApp(t, t.TempDir())
				if v, ok := versions["KEGPLIST"]; ok {
					versions[plist(keg)] = v
					delete(versions, "KEGPLIST")
				}
				menubarKegPaths = []string{keg}
			}
			t.Cleanup(func() { menubarKegPaths = oldKegs })

			if tc.envApp != nil {
				t.Setenv("BASETEN_SWITCH_MENUBAR_APP", tc.envApp(t))
			}

			calls := fakeCmds(t, versions)
			rc := cmdMenubar(nil)
			if rc != tc.wantRC {
				t.Fatalf("rc = %d want %d (calls: %v)", rc, tc.wantRC, *calls)
			}
			joined := strings.Join(*calls, "\n")
			for _, w := range tc.wantCalls {
				if !strings.Contains(joined, w) {
					t.Fatalf("missing call %q in:\n%s", w, joined)
				}
			}
			for _, s := range tc.skipCalls {
				if strings.Contains(joined, s) {
					t.Fatalf("unexpected call %q in:\n%s", s, joined)
				}
			}
		})
	}
}

func TestCmdMenubarUsage(t *testing.T) {
	if rc := cmdMenubar([]string{"extra"}); rc != 2 {
		t.Fatalf("rc = %d want 2", rc)
	}
}

// forceMenubarGOOS pins the GOOS seam for one test so the darwin-only
// paths (and the non-darwin skip) are testable on any host.
func forceMenubarGOOS(t *testing.T, goos string) {
	t.Helper()
	old := menubarGOOS
	menubarGOOS = goos
	t.Cleanup(func() { menubarGOOS = old })
}

// procFake models the BasetenSwitch process over the runCmd seam: pgrep
// answers from the live/dead state, pkill flips it to dead (after
// linger more pgrep polls when set, so the quit wait loop is really
// exercised), ps -o lstart= answers lstart and ps -o comm= answers
// comm (error when the field is empty), ditto copies the faked bundle
// version from src to dst, plutil answers from the versions map (error
// when absent), everything else succeeds.
type procFake struct {
	calls    []string
	running  bool
	killed   bool   // pkill was sent
	linger   int    // pgrep polls after pkill before the process reads as gone
	lstart   string // ps -o lstart= output for the running instance
	comm     string // ps -o comm= output (the instance's executable path)
	versions map[string]string
}

func installProcFake(t *testing.T, running bool, versions map[string]string) *procFake {
	t.Helper()
	if versions == nil {
		versions = map[string]string{}
	}
	f := &procFake{running: running, versions: versions}
	old := runCmd
	runCmd = func(name string, args ...string) (string, error) {
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		switch {
		case strings.HasSuffix(name, "pgrep"):
			if f.killed && f.linger > 0 {
				f.linger--
				if f.linger == 0 {
					f.running = false
				}
			}
			if f.running {
				return "123", nil
			}
			return "", errors.New("exit status 1")
		case strings.HasSuffix(name, "pkill"):
			f.killed = true
			if f.linger == 0 {
				f.running = false
			}
			return "", nil
		case strings.HasSuffix(name, "plutil"):
			v := f.versions[args[len(args)-1]]
			if v == "" {
				return "", os.ErrNotExist
			}
			return v, nil
		case strings.HasSuffix(name, "ditto"):
			f.versions[plist(args[1])] = f.versions[plist(args[0])]
			return "", nil
		case strings.HasSuffix(name, "ps"):
			if len(args) > 1 && args[1] == "comm=" {
				if f.comm == "" {
					return "", errors.New("exit status 1")
				}
				return f.comm, nil
			}
			if f.lstart == "" {
				return "", errors.New("exit status 1")
			}
			return f.lstart, nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { runCmd = old })
	return f
}

// lstartAt renders t the way ps -o lstart= does.
func lstartAt(t time.Time) string { return t.Format("Mon Jan _2 15:04:05 2006") }

// cmdIs reports whether recorded call c invokes the named tool. The
// recorded string is "path args...", and temp-dir arguments can embed
// tool names (a test named ...open... puts "open" in every path), so
// only the command token may be matched.
func cmdIs(c, tool string) bool {
	name, _, _ := strings.Cut(c, " ")
	return strings.HasSuffix(name, tool)
}

// quitOpenOrder returns the indices of the pkill and open calls (-1
// when absent) plus the number of pgrep polls between them.
func quitOpenOrder(calls []string) (killAt, openAt, polls int) {
	killAt, openAt = -1, -1
	for i, c := range calls {
		switch {
		case cmdIs(c, "pkill"):
			killAt = i
		case cmdIs(c, "open"):
			openAt = i
		case cmdIs(c, "pgrep") && killAt != -1 && openAt == -1:
			polls++
		}
	}
	return killAt, openAt, polls
}

// menubarFixture pins HOME to a scratch dir, enables the up/down
// menubar step (TestMain defaults it off), and forces darwin.
func menubarFixture(t *testing.T) (home string) {
	t.Helper()
	forceMenubarGOOS(t, "darwin")
	t.Setenv("BASETEN_SWITCH_MENUBAR", "")
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASETEN_SWITCH_MENUBAR_APP", "")
	os.Unsetenv("BASETEN_SWITCH_MENUBAR_APP")
	return home
}

// useKeg points the keg lookup at a scratch bundle and returns it.
func useKeg(t *testing.T) string {
	t.Helper()
	keg := mkApp(t, t.TempDir())
	old := menubarKegPaths
	menubarKegPaths = []string{keg}
	t.Cleanup(func() { menubarKegPaths = old })
	return keg
}

// TestUpMenubar covers the menubar rows of the up matrix: not running
// (installed and opened), running and current (a strict no-op, the
// recursion-safety property for the app's own "Start Baseten Switch"
// shell-out of `up`), running stale (graceful quit, then the refreshed
// copy opened, in that order), a missing app (skip, never a bring-up
// failure), and the non-darwin / BASETEN_SWITCH_MENUBAR=off silent skips.
func TestUpMenubar(t *testing.T) {
	t.Run("not running: installs and opens", func(t *testing.T) {
		menubarFixture(t)
		keg := useKeg(t)
		f := installProcFake(t, false, map[string]string{plist(keg): "0.1.2"})
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 {
			t.Fatalf("rc = %d want 0\n%s", rc, errOut)
		}
		joined := strings.Join(f.calls, "\n")
		for _, w := range []string{"ditto", "open"} {
			if !strings.Contains(joined, w) {
				t.Fatalf("missing call %q in:\n%s", w, joined)
			}
		}
		if strings.Contains(joined, "pkill") {
			t.Fatalf("killed with nothing running:\n%s", joined)
		}
	})

	t.Run("running and current: no-op", func(t *testing.T) {
		home := menubarFixture(t)
		keg := useKeg(t)
		dst := mkApp(t, filepath.Join(home, "Applications"))
		f := installProcFake(t, true, map[string]string{plist(keg): "0.1.2", plist(dst): "0.1.2"})
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 {
			t.Fatalf("rc = %d want 0\n%s", rc, errOut)
		}
		if !strings.Contains(errOut, "running and current") {
			t.Fatalf("missing no-op report:\n%s", errOut)
		}
		joined := strings.Join(f.calls, "\n")
		for _, s := range []string{"pkill", "open", "ditto"} {
			if strings.Contains(joined, s) {
				t.Fatalf("unexpected call %q on a current running app:\n%s", s, joined)
			}
		}
	})

	t.Run("running stale: quit then open", func(t *testing.T) {
		home := menubarFixture(t)
		keg := useKeg(t)
		dst := mkApp(t, filepath.Join(home, "Applications"))
		f := installProcFake(t, true, map[string]string{plist(keg): "0.1.2", plist(dst): "0.1.1"})
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 {
			t.Fatalf("rc = %d want 0\n%s", rc, errOut)
		}
		if !strings.Contains(errOut, "menubar: adopting 0.1.2 (was 0.1.1)") {
			t.Fatalf("missing adoption line:\n%s", errOut)
		}
		killAt, openAt, _ := quitOpenOrder(f.calls)
		if killAt == -1 || openAt == -1 || killAt > openAt {
			t.Fatalf("expected quit before open, calls:\n%s", strings.Join(f.calls, "\n"))
		}
	})

	t.Run("running stale: open waits for the quit to land", func(t *testing.T) {
		home := menubarFixture(t)
		keg := useKeg(t)
		dst := mkApp(t, filepath.Join(home, "Applications"))
		f := installProcFake(t, true, map[string]string{plist(keg): "0.1.2", plist(dst): "0.1.1"})
		// The old instance stays in the process table for three pgrep
		// polls after the pkill: opening earlier would trip the app's
		// single-instance guard and leave the stale instance in place.
		f.linger = 3
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 {
			t.Fatalf("rc = %d want 0\n%s", rc, errOut)
		}
		killAt, openAt, polls := quitOpenOrder(f.calls)
		if killAt == -1 || openAt == -1 || killAt > openAt {
			t.Fatalf("expected quit before open, calls:\n%s", strings.Join(f.calls, "\n"))
		}
		if polls < 3 {
			t.Fatalf("open after only %d pgrep polls; the lingering instance was still alive:\n%s", polls, strings.Join(f.calls, "\n"))
		}
	})

	t.Run("quit timeout: fails without opening over the survivor", func(t *testing.T) {
		home := menubarFixture(t)
		keg := useKeg(t)
		dst := mkApp(t, filepath.Join(home, "Applications"))
		f := installProcFake(t, true, map[string]string{plist(keg): "0.1.2", plist(dst): "0.1.1"})
		f.linger = 1 << 30 // ignores TERM: never leaves the process table
		oldTO := menubarQuitTimeout
		menubarQuitTimeout = 300 * time.Millisecond
		t.Cleanup(func() { menubarQuitTimeout = oldTO })
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 1 {
			t.Fatalf("rc = %d want 1\n%s", rc, errOut)
		}
		if !strings.Contains(errOut, "still running") {
			t.Fatalf("missing quit-timeout report:\n%s", errOut)
		}
		if _, openAt, _ := quitOpenOrder(f.calls); openAt != -1 {
			t.Fatalf("opened over a still-running instance:\n%s", strings.Join(f.calls, "\n"))
		}
	})

	t.Run("stale instance, current disk copy: adopted via start time", func(t *testing.T) {
		// The disk copy is already current (a `baseten-switch menubar` or a
		// failed earlier adoption consumed the refresh), but the running
		// instance started before it was installed: the refresh bit
		// alone would misread this as running-and-current forever.
		home := menubarFixture(t)
		keg := useKeg(t)
		dst := mkApp(t, filepath.Join(home, "Applications"))
		f := installProcFake(t, true, map[string]string{plist(keg): "0.1.2", plist(dst): "0.1.2"})
		f.lstart = lstartAt(time.Now().Add(-24 * time.Hour))
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 {
			t.Fatalf("rc = %d want 0\n%s", rc, errOut)
		}
		if !strings.Contains(errOut, "predates the installed copy") {
			t.Fatalf("missing stale-instance adoption report:\n%s", errOut)
		}
		killAt, openAt, _ := quitOpenOrder(f.calls)
		if killAt == -1 || openAt == -1 || killAt > openAt {
			t.Fatalf("expected quit before open, calls:\n%s", strings.Join(f.calls, "\n"))
		}
		for _, c := range f.calls {
			if cmdIs(c, "ditto") {
				t.Fatalf("refreshed a current copy:\n%s", strings.Join(f.calls, "\n"))
			}
		}
	})

	t.Run("instance started after the install: no-op", func(t *testing.T) {
		home := menubarFixture(t)
		keg := useKeg(t)
		dst := mkApp(t, filepath.Join(home, "Applications"))
		f := installProcFake(t, true, map[string]string{plist(keg): "0.1.2", plist(dst): "0.1.2"})
		f.lstart = lstartAt(time.Now().Add(time.Hour))
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 {
			t.Fatalf("rc = %d want 0\n%s", rc, errOut)
		}
		if !strings.Contains(errOut, "running and current") {
			t.Fatalf("missing no-op report:\n%s", errOut)
		}
		if joined := strings.Join(f.calls, "\n"); strings.Contains(joined, "pkill") {
			t.Fatalf("quit a current instance:\n%s", joined)
		}
	})

	t.Run("unreadable keg version: loud skip, never a bounce", func(t *testing.T) {
		// A keg whose bundle version cannot be read must not refresh
		// (the copied bundle would be version-less too, so every up
		// would replace-and-bounce forever, breaking the
		// running-and-current no-op that keeps the app's own `up`
		// shell-out safe).
		home := menubarFixture(t)
		useKeg(t) // its Info.plist is absent from versions: plutil errors
		dst := mkApp(t, filepath.Join(home, "Applications"))
		f := installProcFake(t, true, map[string]string{plist(dst): "0.1.1"})
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 {
			t.Fatalf("rc = %d want 0\n%s", rc, errOut)
		}
		if !strings.Contains(errOut, "cannot read the bundle version") {
			t.Fatalf("missing loud skip report:\n%s", errOut)
		}
		joined := strings.Join(f.calls, "\n")
		for _, s := range []string{"ditto", "pkill"} {
			if strings.Contains(joined, s) {
				t.Fatalf("unexpected call %q on an unreadable keg:\n%s", s, joined)
			}
		}
	})

	t.Run("app missing: skip without failing", func(t *testing.T) {
		menubarFixture(t)
		old := menubarKegPaths
		menubarKegPaths = []string{filepath.Join(t.TempDir(), "absent", menubarAppName)}
		t.Cleanup(func() { menubarKegPaths = old })
		f := installProcFake(t, false, nil)
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 {
			t.Fatalf("rc = %d want 0 (the app is optional)\n%s", rc, errOut)
		}
		if !strings.Contains(errOut, "skipping") || !strings.Contains(errOut, "brew install") {
			t.Fatalf("missing skip report with the install hint:\n%s", errOut)
		}
		if joined := strings.Join(f.calls, "\n"); strings.Contains(joined, "open") {
			t.Fatalf("opened a missing app:\n%s", joined)
		}
	})

	t.Run("keg without app: skip with the honest message", func(t *testing.T) {
		// An Intel install of a pre-universal release has the keg but
		// no bundle inside: up must still skip (the app is optional),
		// and must not suggest brew-installing the package that is
		// already installed.
		menubarFixture(t)
		kegDir := t.TempDir()
		old := menubarKegPaths
		menubarKegPaths = []string{filepath.Join(kegDir, menubarAppName)}
		t.Cleanup(func() { menubarKegPaths = old })
		f := installProcFake(t, false, nil)
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 {
			t.Fatalf("rc = %d want 0 (the app is optional)\n%s", rc, errOut)
		}
		if !strings.Contains(errOut, "skipping") || !strings.Contains(errOut, "no menubar app bundle") || !strings.Contains(errOut, kegDir) {
			t.Fatalf("missing keg-without-app skip report naming %s:\n%s", kegDir, errOut)
		}
		if strings.Contains(errOut, "brew install") {
			t.Fatalf("install hint on an already-installed keg:\n%s", errOut)
		}
		for _, c := range f.calls {
			if cmdIs(c, "open") {
				t.Fatalf("opened a missing app: %s", c)
			}
		}
	})

	t.Run("non-darwin: silent skip", func(t *testing.T) {
		menubarFixture(t)
		forceMenubarGOOS(t, "linux")
		f := installProcFake(t, true, nil)
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 || errOut != "" || len(f.calls) != 0 {
			t.Fatalf("rc = %d calls = %v out = %q", rc, f.calls, errOut)
		}
	})

	t.Run("BASETEN_SWITCH_MENUBAR=off: silent skip", func(t *testing.T) {
		menubarFixture(t)
		t.Setenv("BASETEN_SWITCH_MENUBAR", "off")
		f := installProcFake(t, true, nil)
		var rc int
		errOut := captureStderr(t, func() { rc = upMenubar() })
		if rc != 0 || errOut != "" || len(f.calls) != 0 {
			t.Fatalf("rc = %d calls = %v out = %q", rc, f.calls, errOut)
		}
	})
}

// TestCmdMenubarQuitsStaleRunningInstance: the verb shares the up
// step's quit-then-open. `open` on a running app only activates the
// old instance (single-instance guard), so without the quit a
// refreshed copy would never take over, and the survivor would read as
// running-and-current to every later up.
func TestCmdMenubarQuitsStaleRunningInstance(t *testing.T) {
	home := menubarFixture(t)
	keg := useKeg(t)
	dst := mkApp(t, filepath.Join(home, "Applications"))
	f := installProcFake(t, true, map[string]string{plist(keg): "0.1.2", plist(dst): "0.1.1"})
	var rc int
	errOut := captureStderr(t, func() { rc = cmdMenubar(nil) })
	if rc != 0 {
		t.Fatalf("rc = %d want 0\n%s", rc, errOut)
	}
	killAt, openAt, _ := quitOpenOrder(f.calls)
	if killAt == -1 || openAt == -1 || killAt > openAt {
		t.Fatalf("expected quit before open, calls:\n%s", strings.Join(f.calls, "\n"))
	}
}

// TestCmdMenubarKegWithoutApp: a keg dir that exists with no bundle
// inside (a pre-universal release installed on an Intel Mac) gets the
// honest no-bundle error naming the keg checked, never the
// brew-install hint, which would tell the user to install what they
// just installed.
func TestCmdMenubarKegWithoutApp(t *testing.T) {
	menubarFixture(t)
	kegDir := t.TempDir() // the keg exists, but ships no app bundle
	old := menubarKegPaths
	menubarKegPaths = []string{filepath.Join(kegDir, menubarAppName)}
	t.Cleanup(func() { menubarKegPaths = old })
	f := installProcFake(t, false, nil)
	var rc int
	errOut := captureStderr(t, func() { rc = cmdMenubar(nil) })
	if rc != 1 {
		t.Fatalf("rc = %d want 1\n%s", rc, errOut)
	}
	if !strings.Contains(errOut, "no menubar app bundle") || !strings.Contains(errOut, kegDir) {
		t.Fatalf("missing keg-without-app report naming %s:\n%s", kegDir, errOut)
	}
	if strings.Contains(errOut, "brew install") {
		t.Fatalf("install hint on an already-installed keg:\n%s", errOut)
	}
	for _, c := range f.calls {
		if cmdIs(c, "open") || cmdIs(c, "ditto") {
			t.Fatalf("unexpected call with no bundle to open: %s", c)
		}
	}
}

// TestCmdMenubarKegWithoutAppFallsBackToCopy: an existing
// ~/Applications copy still wins over a keg that ships no bundle, so
// the new error never shadows a working install.
func TestCmdMenubarKegWithoutAppFallsBackToCopy(t *testing.T) {
	home := menubarFixture(t)
	kegDir := t.TempDir()
	old := menubarKegPaths
	menubarKegPaths = []string{filepath.Join(kegDir, menubarAppName)}
	t.Cleanup(func() { menubarKegPaths = old })
	dst := mkApp(t, filepath.Join(home, "Applications"))
	f := installProcFake(t, false, map[string]string{plist(dst): "0.1.2"})
	var rc int
	errOut := captureStderr(t, func() { rc = cmdMenubar(nil) })
	if rc != 0 {
		t.Fatalf("rc = %d want 0\n%s", rc, errOut)
	}
	opened := false
	for _, c := range f.calls {
		if cmdIs(c, "ditto") {
			t.Fatalf("refreshed from a keg with no bundle: %s", c)
		}
		if cmdIs(c, "open") {
			opened = true
		}
	}
	if !opened {
		t.Fatalf("did not open the ~/Applications copy, calls:\n%s", strings.Join(f.calls, "\n"))
	}
}

// TestDownMenubar: down quits a running app gracefully, is silent when
// nothing runs, and skips off darwin.
func TestDownMenubar(t *testing.T) {
	t.Run("running: quits", func(t *testing.T) {
		menubarFixture(t)
		f := installProcFake(t, true, nil)
		var rc int
		errOut := captureStderr(t, func() { rc = downMenubar() })
		if rc != 0 {
			t.Fatalf("rc = %d want 0\n%s", rc, errOut)
		}
		if !strings.Contains(errOut, "menubar: quit") {
			t.Fatalf("missing quit report:\n%s", errOut)
		}
		joined := strings.Join(f.calls, "\n")
		if strings.Count(joined, "pkill") != 1 {
			t.Fatalf("expected exactly one pkill:\n%s", joined)
		}
		// pgrep/pkill must be scoped to the current uid: an unscoped
		// match reads another user's instance as ours, which pkill can
		// never signal, so down would wait out a quit that cannot land.
		for _, c := range f.calls {
			if (strings.Contains(c, "pgrep") || strings.Contains(c, "pkill")) && !strings.Contains(c, "-U ") {
				t.Fatalf("process probe not scoped to the current user: %s", c)
			}
		}
	})
	t.Run("not running: silent no-op", func(t *testing.T) {
		menubarFixture(t)
		f := installProcFake(t, false, nil)
		var rc int
		errOut := captureStderr(t, func() { rc = downMenubar() })
		if rc != 0 || errOut != "" {
			t.Fatalf("rc = %d out = %q", rc, errOut)
		}
		if joined := strings.Join(f.calls, "\n"); strings.Contains(joined, "pkill") {
			t.Fatalf("killed with nothing running:\n%s", joined)
		}
	})
	t.Run("non-darwin: silent skip", func(t *testing.T) {
		menubarFixture(t)
		forceMenubarGOOS(t, "linux")
		f := installProcFake(t, true, nil)
		if rc := downMenubar(); rc != 0 || len(f.calls) != 0 {
			t.Fatalf("rc = %d calls = %v", rc, f.calls)
		}
	})
}

// TestMenubarStatusLines covers the read-only probe behind the
// `status` Menubar row: omitted (nil) off-darwin and under
// BASETEN_SWITCH_MENUBAR=off, a DOWN line with the fix hint when the app is
// installed but nothing runs, a not-installed line with the install
// hint when there is no bundle for `up` to open (the fix hint would be
// a dead end there: upMenubar skips a missing app with exit 0), the
// upgrade hint instead when the keg exists but ships no bundle (a
// pre-universal release on an Intel Mac), the up
// line with pid, bundle version, and bundle path (from the process
// table), a stale line when the running instance predates the
// installed copy (the on-disk version is not what the process runs),
// plus the skew line only for a readable, differing bundle version.
func TestMenubarStatusLines(t *testing.T) {
	const bundle = "/Users/example/Applications/Baseten Switch.app"
	const comm = bundle + "/Contents/MacOS/BasetenSwitch"

	t.Run("non-darwin: omitted, no probes", func(t *testing.T) {
		menubarFixture(t)
		forceMenubarGOOS(t, "linux")
		f := installProcFake(t, true, nil)
		if lines := menubarStatusLines(); lines != nil || len(f.calls) != 0 {
			t.Fatalf("lines = %v calls = %v", lines, f.calls)
		}
	})
	t.Run("BASETEN_SWITCH_MENUBAR=off: omitted, no probes", func(t *testing.T) {
		menubarFixture(t)
		t.Setenv("BASETEN_SWITCH_MENUBAR", "off")
		f := installProcFake(t, true, nil)
		if lines := menubarStatusLines(); lines != nil || len(f.calls) != 0 {
			t.Fatalf("lines = %v calls = %v", lines, f.calls)
		}
	})
	t.Run("installed, not running: DOWN with fix hint", func(t *testing.T) {
		home := menubarFixture(t)
		useKeg(t) // keg absent would read as not installed on CI hosts
		mkApp(t, filepath.Join(home, "Applications"))
		installProcFake(t, false, nil)
		lines := menubarStatusLines()
		if len(lines) != 1 || lines[0] != "DOWN (fix: baseten-switch up)" {
			t.Fatalf("lines = %v", lines)
		}
	})
	t.Run("not installed: install hint, not the dead-end fix hint", func(t *testing.T) {
		// No keg, no ~/Applications copy: `baseten-switch up` skips a
		// missing app (exit 0), so "fix: baseten-switch up" could never
		// clear this row; the install hint can.
		menubarFixture(t)
		old := menubarKegPaths
		menubarKegPaths = []string{filepath.Join(t.TempDir(), "absent", menubarAppName)}
		t.Cleanup(func() { menubarKegPaths = old })
		installProcFake(t, false, nil)
		lines := menubarStatusLines()
		if len(lines) != 1 || lines[0] != "not installed (install: brew install basetenlabs/baseten/baseten-switch)" {
			t.Fatalf("lines = %v", lines)
		}
	})
	t.Run("keg without app: upgrade hint, not the install hint", func(t *testing.T) {
		// The keg dir exists but ships no bundle (a pre-universal
		// release installed on an Intel Mac): brew-install would tell
		// the user to install the package they already have, the same
		// mistake resolveMenubarTarget's keg-without-app error exists
		// to avoid; the row must point at the upgrade instead.
		menubarFixture(t)
		kegDir := t.TempDir()
		old := menubarKegPaths
		menubarKegPaths = []string{filepath.Join(kegDir, menubarAppName)}
		t.Cleanup(func() { menubarKegPaths = old })
		installProcFake(t, false, nil)
		lines := menubarStatusLines()
		want := "not installed (keg " + kegDir + " has no app bundle; upgrade: brew upgrade baseten-switch)"
		if len(lines) != 1 || lines[0] != want {
			t.Fatalf("lines = %v want [%q]", lines, want)
		}
		if strings.Contains(strings.Join(lines, "\n"), "brew install") {
			t.Fatalf("install hint on an already-installed keg: %v", lines)
		}
	})
	t.Run("running and current: pid, version, bundle path, no skew", func(t *testing.T) {
		menubarFixture(t)
		f := installProcFake(t, true, map[string]string{plist(bundle): version.Version})
		f.comm = comm
		lines := menubarStatusLines()
		want := fmt.Sprintf("up (pid 123) %s", version.Version)
		if len(lines) != 2 || lines[0] != want || lines[1] != bundle {
			t.Fatalf("lines = %v want [%q %q]", lines, want, bundle)
		}
	})
	t.Run("running stale: skew line names both versions", func(t *testing.T) {
		menubarFixture(t)
		f := installProcFake(t, true, map[string]string{plist(bundle): "v0.0.9"})
		f.comm = comm
		lines := menubarStatusLines()
		if len(lines) != 3 || lines[0] != "up (pid 123) v0.0.9" || lines[1] != bundle {
			t.Fatalf("lines = %v", lines)
		}
		if !strings.Contains(lines[2], "restart to adopt "+version.Version) || !strings.Contains(lines[2], "v0.0.9") {
			t.Fatalf("skew line = %q", lines[2])
		}
	})
	t.Run("stale instance under a refreshed copy: stale line, not the disk version", func(t *testing.T) {
		// `up` refreshed the copy in place but its quit timed out and
		// the old instance survived: the version on disk is the NEW
		// one, and reporting it as the running version would read the
		// survivor as current, contradicting up's own lstart-based
		// staleness signal (menubarInstanceStale).
		menubarFixture(t)
		real := mkApp(t, t.TempDir()) // exists on disk so the install mtime is comparable
		f := installProcFake(t, true, map[string]string{plist(real): "0.1.15"})
		f.comm = filepath.Join(real, "Contents", "MacOS", "BasetenSwitch")
		f.lstart = lstartAt(time.Now().Add(-24 * time.Hour)) // started before the copy was installed
		lines := menubarStatusLines()
		if len(lines) != 2 || lines[1] != real {
			t.Fatalf("lines = %v", lines)
		}
		want := "up (pid 123) STALE: instance predates the installed copy (0.1.15 on disk; fix: baseten-switch up)"
		if lines[0] != want {
			t.Fatalf("stale line = %q want %q", lines[0], want)
		}
	})
	t.Run("instance started after the install: normal row, no stale line", func(t *testing.T) {
		menubarFixture(t)
		real := mkApp(t, t.TempDir())
		f := installProcFake(t, true, map[string]string{plist(real): version.Version})
		f.comm = filepath.Join(real, "Contents", "MacOS", "BasetenSwitch")
		f.lstart = lstartAt(time.Now().Add(time.Hour))
		lines := menubarStatusLines()
		want := fmt.Sprintf("up (pid 123) %s", version.Version)
		if len(lines) != 2 || lines[0] != want || lines[1] != real {
			t.Fatalf("lines = %v want [%q %q]", lines, want, real)
		}
	})
	t.Run("bare binary outside a bundle: pid only", func(t *testing.T) {
		menubarFixture(t)
		f := installProcFake(t, true, nil)
		f.comm = "/Users/example/baseten/mac/BasetenSwitch/.build/release/BasetenSwitch"
		lines := menubarStatusLines()
		if len(lines) != 1 || lines[0] != "up (pid 123)" {
			t.Fatalf("lines = %v", lines)
		}
	})
	t.Run("unreadable bundle version: unknown, no skew line", func(t *testing.T) {
		menubarFixture(t)
		f := installProcFake(t, true, nil) // plutil errors for every path
		f.comm = comm
		lines := menubarStatusLines()
		if len(lines) != 2 || lines[0] != "up (pid 123) unknown" || lines[1] != bundle {
			t.Fatalf("lines = %v", lines)
		}
	})
}

// TestCmdDownMenubarQuitFailureDoesNotFailDown: the menubar app is
// optional UX. A quit that cannot land (a hung instance ignoring TERM)
// must not fail a down whose servers all stopped; a nonzero down here
// would make cmdRestart abort AFTER its destructive half, leaving the
// whole system down over the optional app.
func TestCmdDownMenubarQuitFailureDoesNotFailDown(t *testing.T) {
	useFake(t)
	menubarFixture(t)
	f := installProcFake(t, true, nil)
	f.linger = 1 << 30 // hung: never leaves the process table
	oldTO := menubarQuitTimeout
	menubarQuitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { menubarQuitTimeout = oldTO })

	adminAddr := closedPortAddr(t)
	upEnv(t, adminAddr, "", closedPortAddr(t))

	var rc int
	errOut := captureStderr(t, func() { rc = cmdDown(nil) })
	if rc != 0 {
		t.Fatalf("rc = %d want 0 (the app is optional)\n%s", rc, errOut)
	}
	if !strings.Contains(errOut, "does not gate down") {
		t.Fatalf("missing continue-past-menubar report:\n%s", errOut)
	}
}

// TestCmdUpRunsMenubarStep pins the wiring: a plain up with every
// server current still runs the menubar step (here: the
// running-and-current no-op).
func TestCmdUpRunsMenubarStep(t *testing.T) {
	useFake(t)
	home := menubarFixture(t)
	keg := useKeg(t)
	dst := mkApp(t, filepath.Join(home, "Applications"))
	installProcFake(t, true, map[string]string{plist(keg): "0.1.2", plist(dst): "0.1.2"})

	adminAddr := closedPortAddr(t)
	upEnv(t, adminAddr, "", closedPortAddr(t))
	startFakeRouterAt(t, adminAddr, version.Version)
	spawns := fakeSpawn(t, nil)

	var rc int
	errOut := captureStderr(t, func() { rc = cmdUp(nil) })
	if rc != 0 {
		t.Fatalf("rc = %d want 0\n%s", rc, errOut)
	}
	if !strings.Contains(errOut, "menubar: running and current") {
		t.Fatalf("up did not run the menubar step:\n%s", errOut)
	}
	if len(*spawns) != 0 {
		t.Fatalf("unexpected spawns: %v", *spawns)
	}
}

// TestCmdMenubarWhich: --which prints the binary the menubar app will
// resolve (the version_probe.go lookup order) so tooling like
// Development tooling asks the binary instead of re-deriving the order.
func TestCmdMenubarWhich(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "baseten-switch")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_GATEWAY_BIN", bin)

	out, rc := captureStdout(t, func() int { return cmdMenubar([]string{"--which"}) })
	if rc != 0 {
		t.Fatalf("rc = %d want 0", rc)
	}
	if strings.TrimSpace(out) != bin {
		t.Fatalf("--which printed %q want %q", strings.TrimSpace(out), bin)
	}
}

// TestCmdMenubarWhichNothingResolves: --which fails loudly when no
// candidate in the lookup order exists.
func TestCmdMenubarWhichNothingResolves(t *testing.T) {
	t.Setenv("BASETEN_SWITCH_GATEWAY_BIN", "")
	t.Setenv("HOME", t.TempDir())

	oldBrew := menubarBrewPaths
	menubarBrewPaths = []string{filepath.Join(t.TempDir(), "absent", "baseten-switch")}
	t.Cleanup(func() { menubarBrewPaths = oldBrew })

	oldLocal := menubarLocalBin
	menubarLocalBin = func() string { return filepath.Join(t.TempDir(), "absent-local", "baseten-switch") }
	t.Cleanup(func() { menubarLocalBin = oldLocal })

	if rc := cmdMenubar([]string{"--which"}); rc != 1 {
		t.Fatalf("rc = %d want 1", rc)
	}
}
