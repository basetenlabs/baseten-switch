package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/door"
	"github.com/basetenlabs/baseten-switch/gateway/internal/launchd"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

// TestMain makes the package's launchd seams hermetic: no test in
// this package ever consults the real ~/Library/LaunchAgents or runs
// the real launchctl, even on a machine with supervision installed.
// The menubar step of up/down is off for the same reason: no test may
// quit or relaunch the user's real menubar app or touch the real
// ~/Applications copy. Tests that exercise the step re-enable it with
// t.Setenv("BASETEN_SWITCH_MENUBAR", "") plus the runCmd/HOME/keg fixtures.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "baseten-launchagents")
	if err != nil {
		panic(err)
	}
	launchAgentsDir = func() string { return dir }
	launchdRunner = &fakeLaunchctl{} // empty: every call errors (nothing loaded)
	os.Setenv("BASETEN_SWITCH_MENUBAR", "off")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeLaunchctl fakes the launchctl runner: responses are matched by
// verb (args[0]). Queued responses (queue) are consumed first, in
// order, so a test can model a label that lingers for N polls and then
// disappears. Unmatched verbs return an error, which reads as "label
// not loaded" / "command failed".
type fakeLaunchctl struct {
	responses map[string]struct {
		out string
		err error
	}
	seq   map[string][]fakeResp
	hook  func(args []string)
	calls [][]string
}

type fakeResp struct {
	out string
	err error
}

func (f *fakeLaunchctl) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.hook != nil {
		f.hook(args)
	}
	if q := f.seq[args[0]]; len(q) > 0 {
		r := q[0]
		f.seq[args[0]] = q[1:]
		return r.out, r.err
	}
	if r, ok := f.responses[args[0]]; ok {
		return r.out, r.err
	}
	return "", errors.New("launchctl " + strings.Join(args, " ") + ": exit status 113")
}

func (f *fakeLaunchctl) queue(verb, out string, err error) {
	if f.seq == nil {
		f.seq = map[string][]fakeResp{}
	}
	f.seq[verb] = append(f.seq[verb], fakeResp{out, err})
}

func (f *fakeLaunchctl) respond(verb, out string, err error) {
	if f.responses == nil {
		f.responses = map[string]struct {
			out string
			err error
		}{}
	}
	f.responses[verb] = struct {
		out string
		err error
	}{out, err}
}

func (f *fakeLaunchctl) callsFor(verb string) [][]string {
	var out [][]string
	for _, c := range f.calls {
		if c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

// useFake swaps in a fresh fake runner and a fresh LaunchAgents dir
// for one test.
func useFake(t *testing.T) *fakeLaunchctl {
	t.Helper()
	f := &fakeLaunchctl{}
	oldR, oldD := launchdRunner, launchAgentsDir
	dir := t.TempDir()
	launchdRunner = f
	launchAgentsDir = func() string { return dir }
	t.Cleanup(func() { launchdRunner, launchAgentsDir = oldR, oldD })
	return f
}

func installFakePlist(t *testing.T, label string) string {
	t.Helper()
	pp := plistPathFor(label)
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pp, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return pp
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stderr = old
	return <-done
}

func TestSuperviseState(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		f := useFake(t)
		s := superviseState(launchd.RouterLabel)
		if s.installed || s.loaded || s.supervised() {
			t.Fatalf("state = %+v", s)
		}
		if len(f.calls) != 0 {
			t.Fatalf("launchctl consulted with no plist installed: %v", f.calls)
		}
	})
	t.Run("installed and loaded", func(t *testing.T) {
		f := useFake(t)
		f.respond("print", "ok", nil)
		installFakePlist(t, launchd.RouterLabel)
		s := superviseState(launchd.RouterLabel)
		if !s.supervised() {
			t.Fatalf("state = %+v", s)
		}
	})
	t.Run("installed not loaded", func(t *testing.T) {
		useFake(t)
		installFakePlist(t, launchd.RouterLabel)
		s := superviseState(launchd.RouterLabel)
		if !s.installed || s.loaded {
			t.Fatalf("state = %+v", s)
		}
	})
	t.Run("BASETEN_SWITCH_LAUNCHD=off disables detection", func(t *testing.T) {
		f := useFake(t)
		f.respond("print", "ok", nil)
		installFakePlist(t, launchd.RouterLabel)
		t.Setenv("BASETEN_SWITCH_LAUNCHD", "off")
		s := superviseState(launchd.RouterLabel)
		if s.installed || s.loaded {
			t.Fatalf("state = %+v", s)
		}
		if len(f.calls) != 0 {
			t.Fatalf("launchctl consulted under BASETEN_SWITCH_LAUNCHD=off: %v", f.calls)
		}
	})
}

// TestInstallPlan pins the --install domain classification: foreign
// baseten-switch labels (brew's etc.) refuse; our own labels are returned
// for the re-install (upgrade) path instead of refused.
func TestInstallPlan(t *testing.T) {
	cases := []struct {
		name     string
		printOut string
		wantOurs []string
		wantsErr []string // empty = no error
	}{
		{"clean domain", "services = {\n 0 - com.apple.Foo\n}", nil, nil},
		{"brew label refusal", `services = {
	42	0	homebrew.mxcl.baseten-switch
}`, nil, []string{"homebrew.mxcl.baseten-switch", "brew services stop"}},
		{"our labels re-install", `services = {
	42	0	co.baseten.switch.router
	-	0	co.baseten.switch.door
}`, []string{"co.baseten.switch.door", "co.baseten.switch.router"}, nil},
		{"our door label alone", `"co.baseten.switch.door" => {`,
			[]string{"co.baseten.switch.door"}, nil},
		{"ours plus brew still refuses", `services = {
	42	0	co.baseten.switch.router
	43	0	homebrew.mxcl.baseten-switch
}`, nil, []string{"homebrew.mxcl.baseten-switch"}},
		{"running menubar app is not supervision", `services = {
	812	0	application.co.baseten.switch.123139528.123139533
}`, nil, nil},
		{"menubar app plus ours re-install", `services = {
	812	0	application.co.baseten.switch.123139528.123139533
	42	0	co.baseten.switch.router
	-	0	co.baseten.switch.door
}`, []string{"co.baseten.switch.door", "co.baseten.switch.router"}, nil},
		{"menubar app plus brew still refuses", `services = {
	812	0	application.co.baseten.switch.123139528.123139533
	43	0	homebrew.mxcl.baseten-switch
}`, nil, []string{"homebrew.mxcl.baseten-switch"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ours, err := installPlan(tc.printOut)
			if len(tc.wantsErr) > 0 {
				if err == nil {
					t.Fatal("expected refusal")
				}
				for _, w := range tc.wantsErr {
					if !strings.Contains(err.Error(), w) {
						t.Fatalf("refusal %q missing %q", err.Error(), w)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if len(ours) != len(tc.wantOurs) {
				t.Fatalf("ours = %v want %v", ours, tc.wantOurs)
			}
			for i := range ours {
				if ours[i] != tc.wantOurs[i] {
					t.Fatalf("ours = %v want %v", ours, tc.wantOurs)
				}
			}
		})
	}
}

// TestCmdUpInstallRefusesOnConflict drives the full --install entry:
// the conflicting label is named and nothing is written or
// bootstrapped.
func TestCmdUpInstallRefusesOnConflict(t *testing.T) {
	f := useFake(t)
	f.respond("print", "services = {\n\t42\t0\thomebrew.mxcl.baseten-switch\n}\n", nil)
	lc := &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: "127.0.0.1:28786"}
	var rc int
	errOut := captureStderr(t, func() { rc = cmdUpInstall(lc) })
	if rc != 1 {
		t.Fatalf("rc = %d want 1", rc)
	}
	if !strings.Contains(errOut, "homebrew.mxcl.baseten-switch") {
		t.Fatalf("refusal does not name the conflicting label:\n%s", errOut)
	}
	if len(f.callsFor("bootstrap")) != 0 {
		t.Fatalf("bootstrap ran despite refusal: %v", f.calls)
	}
	if _, err := os.Stat(plistPathFor(launchd.RouterLabel)); !os.IsNotExist(err) {
		t.Fatal("plist written despite refusal")
	}
}

// TestCmdUpInstallReinstallsOurLabels drives the full --install entry
// when the loaded labels are exactly ours: instead of refusing, it
// re-renders the plists and re-bootstraps (bootout then bootstrap)
// each job, the upgrade path for plist content changes. Health gates
// are served by httptest fakes; the fake runner keeps launchctl
// hermetic.
func TestCmdUpInstallReinstallsOurLabels(t *testing.T) {
	f := useFake(t)
	// The domain listing answers once; every later print (the
	// post-bootout teardown polls) reads the labels as gone.
	f.queue("print", "services = {\n\t42\t0\tco.baseten.switch.router\n\t-\t0\tco.baseten.switch.door\n}\n", nil)
	f.respond("bootout", "", nil)
	f.respond("bootstrap", "", nil)

	admin := fakeAdmin(t)
	defer admin.Close()
	doorSrv := fakeDoor(t)
	defer doorSrv.Close()
	pidDir := t.TempDir()
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(pidDir, "gw.pid"))
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(pidDir, "door.pid"))

	lc := &lifecycleConfig{
		path:      "/tmp/gw.yaml",
		adminAddr: hostPort(t, admin.URL),
		doorSpecs: []door.Config{{ListenAddr: hostPort(t, doorSrv.URL), RouterTarget: "127.0.0.1:18081"}},
	}
	var rc int
	errOut := captureStderr(t, func() { rc = cmdUpInstall(lc) })
	if rc != 0 {
		t.Fatalf("rc = %d want 0\n%s", rc, errOut)
	}
	if strings.Contains(errOut, "Refusing") {
		t.Fatalf("re-install refused:\n%s", errOut)
	}
	if !strings.Contains(errOut, "re-rendering plists and re-bootstrapping") {
		t.Fatalf("missing upgrade-path report:\n%s", errOut)
	}
	for _, label := range []string{launchd.RouterLabel, launchd.DoorLabel} {
		if _, err := os.Stat(plistPathFor(label)); err != nil {
			t.Fatalf("plist %s not re-rendered: %v", label, err)
		}
	}
	if len(f.callsFor("bootout")) != 2 || len(f.callsFor("bootstrap")) != 2 {
		t.Fatalf("expected bootout+bootstrap for both labels, got %v", f.calls)
	}
	// The loaded components are launchd-owned: no SIGTERM stop path,
	// and the report says the bootout/bootstrap cycle restarts them.
	if !strings.Contains(errOut, "router: launchd-owned") || !strings.Contains(errOut, "door: launchd-owned") {
		t.Fatalf("missing launchd-owned skip reports:\n%s", errOut)
	}
}

func TestCmdUpInstallDisabled(t *testing.T) {
	useFake(t)
	t.Setenv("BASETEN_SWITCH_LAUNCHD", "off")
	var rc int
	errOut := captureStderr(t, func() { rc = cmdUpInstall(&lifecycleConfig{path: "/tmp/gw.yaml"}) })
	if rc != 1 || !strings.Contains(errOut, "BASETEN_SWITCH_LAUNCHD=off") {
		t.Fatalf("rc = %d out = %s", rc, errOut)
	}
}

// TestInstallAgents pins the plist writing + bootstrap mechanics:
// files land 0644 in the LaunchAgents dir with the important keys
// (including the Background Activity grouping key on BOTH agents),
// and each label is booted out (tolerated noop when not loaded) then
// bootstrapped into the gui domain.
func TestInstallAgents(t *testing.T) {
	f := useFake(t)
	f.respond("bootstrap", "", nil)
	f.respond("bootout", "Boot-out failed: 3: No such process", errors.New("exit status 3"))
	lc := &lifecycleConfig{
		path:      "/tmp/gw.yaml",
		adminAddr: "127.0.0.1:28786",
		doorSpecs: []door.Config{{ListenAddr: "127.0.0.1:28081", RouterTarget: "127.0.0.1:28181"}},
	}
	errOut := captureStderr(t, func() {
		if err := installAgents(lc, "/Users/x/.local/bin/baseten-switch"); err != nil {
			t.Errorf("installAgents: %v", err)
		}
	})
	if t.Failed() {
		t.Fatalf("stderr:\n%s", errOut)
	}
	boots := f.callsFor("bootstrap")
	if len(boots) != 2 {
		t.Fatalf("expected 2 bootstraps, got %v", f.calls)
	}
	bootouts := f.callsFor("bootout")
	if len(bootouts) != 2 {
		t.Fatalf("expected 2 bootouts (one per label, before bootstrap), got %v", f.calls)
	}
	// Per-label ordering: bootout, then bootstrap (calls interleave
	// as bootout/bootstrap pairs).
	if f.calls[0][0] != "bootout" || f.calls[1][0] != "bootstrap" ||
		f.calls[2][0] != "bootout" || f.calls[3][0] != "bootstrap" {
		t.Fatalf("expected bootout-then-bootstrap per label, got %v", f.calls)
	}
	for i, label := range []string{launchd.RouterLabel, launchd.DoorLabel} {
		pp := plistPathFor(label)
		st, err := os.Stat(pp)
		if err != nil {
			t.Fatalf("plist not written: %v", err)
		}
		if st.Mode().Perm() != 0o644 {
			t.Fatalf("plist mode = %v want 0644", st.Mode().Perm())
		}
		b, _ := os.ReadFile(pp)
		content := string(b)
		wants := []string{label, "/Users/x/.local/bin/baseten-switch", "BASETEN_SWITCH_CONFIG_PATH", "/tmp/gw.yaml",
			"<key>RunAtLoad</key>", "<key>KeepAlive</key>", "<key>PATH</key>",
			"<key>AssociatedBundleIdentifiers</key>", "<string>" + launchd.ToggleBundleID + "</string>"}
		if label == launchd.RouterLabel {
			wants = append(wants, "<string>gateway</string>", "<string>start</string>", "<string>--foreground</string>")
		} else {
			wants = append(wants, "<string>door</string>")
		}
		for _, w := range wants {
			if !strings.Contains(content, w) {
				t.Fatalf("plist %s missing %q:\n%s", label, w, content)
			}
		}
		if bootouts[i][1] != launchd.GuiTarget()+"/"+label {
			t.Fatalf("bootout call = %v want [bootout %s/%s]", bootouts[i], launchd.GuiTarget(), label)
		}
		if boots[i][1] != launchd.GuiTarget() || boots[i][2] != pp {
			t.Fatalf("bootstrap call = %v want [bootstrap %s %s]", boots[i], launchd.GuiTarget(), pp)
		}
	}
}

// TestInstallAgentsNoDoorSection: no door: section means no door
// agent (a KeepAlive door job with no config would crash-loop).
func TestInstallAgentsNoDoorSection(t *testing.T) {
	f := useFake(t)
	f.respond("bootstrap", "", nil)
	f.respond("bootout", "Boot-out failed: 3: No such process", errors.New("exit status 3"))
	lc := &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: "127.0.0.1:28786"}
	captureStderr(t, func() {
		if err := installAgents(lc, "/x/baseten-switch"); err != nil {
			t.Errorf("installAgents: %v", err)
		}
	})
	if len(f.callsFor("bootstrap")) != 1 {
		t.Fatalf("expected 1 bootstrap, got %v", f.calls)
	}
	if _, err := os.Stat(plistPathFor(launchd.DoorLabel)); !os.IsNotExist(err) {
		t.Fatal("door plist written without a door: section")
	}
}

// TestDownComponentBootoutVsSigterm: a supervised component is booted
// out via launchctl (and down says so); an unsupervised one goes down
// the pidfile SIGTERM path (here: reported not running).
func TestDownComponentBootoutVsSigterm(t *testing.T) {
	t.Run("supervised uses bootout", func(t *testing.T) {
		f := useFake(t)
		// Loaded at the supervision check; gone on the first
		// post-bootout teardown poll.
		f.queue("print", "ok", nil)
		f.respond("bootout", "", nil)
		installFakePlist(t, launchd.DoorLabel)
		pf := filepath.Join(t.TempDir(), "door.pid")
		var rc int
		errOut := captureStderr(t, func() {
			rc = downComponent("door", launchd.DoorLabel, pf, []string{closedPortAddr(t)}, doorHealthPath, doorHealthMarker)
		})
		if rc != 0 {
			t.Fatalf("rc = %d\n%s", rc, errOut)
		}
		boots := f.callsFor("bootout")
		if len(boots) != 1 || boots[0][1] != launchd.GuiTarget()+"/"+launchd.DoorLabel {
			t.Fatalf("bootout calls = %v", f.calls)
		}
		if !strings.Contains(errOut, "bootout") || !strings.Contains(errOut, launchd.DoorLabel) {
			t.Fatalf("down did not say it used bootout:\n%s", errOut)
		}
	})
	t.Run("unsupervised falls through to pidfile path", func(t *testing.T) {
		f := useFake(t)
		pf := filepath.Join(t.TempDir(), "door.pid")
		var rc int
		errOut := captureStderr(t, func() {
			rc = downComponent("door", launchd.DoorLabel, pf, []string{closedPortAddr(t)}, doorHealthPath, doorHealthMarker)
		})
		if rc != 0 {
			t.Fatalf("rc = %d\n%s", rc, errOut)
		}
		if len(f.callsFor("bootout")) != 0 {
			t.Fatalf("bootout ran for unsupervised component: %v", f.calls)
		}
		if !strings.Contains(errOut, "not running") {
			t.Fatalf("expected pidfile-path noop:\n%s", errOut)
		}
	})
	t.Run("installed but not loaded stops directly", func(t *testing.T) {
		f := useFake(t) // print errors: not loaded
		installFakePlist(t, launchd.DoorLabel)
		pf := filepath.Join(t.TempDir(), "door.pid")
		var rc int
		errOut := captureStderr(t, func() {
			rc = downComponent("door", launchd.DoorLabel, pf, []string{closedPortAddr(t)}, doorHealthPath, doorHealthMarker)
		})
		if rc != 0 {
			t.Fatalf("rc = %d\n%s", rc, errOut)
		}
		if len(f.callsFor("bootout")) != 0 {
			t.Fatalf("bootout ran for a not-loaded label: %v", f.calls)
		}
	})
}

// TestCmdUpUninstallIdempotent: uninstall on a clean system is a
// reported noop; uninstall over installed agents boots out and
// removes; a second run is again a noop. Exit 0 throughout.
func TestCmdUpUninstallIdempotent(t *testing.T) {
	f := useFake(t)
	// Nothing installed, bootout says not loaded.
	f.respond("bootout", "Boot-out failed: 3: No such process", errors.New("exit status 3"))
	var rc int
	errOut := captureStderr(t, func() { rc = cmdUpUninstall() })
	if rc != 0 || !strings.Contains(errOut, "nothing to do") {
		t.Fatalf("rc = %d out:\n%s", rc, errOut)
	}

	// Install both plists and mark them loaded.
	installFakePlist(t, launchd.RouterLabel)
	installFakePlist(t, launchd.DoorLabel)
	f.respond("bootout", "", nil)
	errOut = captureStderr(t, func() { rc = cmdUpUninstall() })
	if rc != 0 {
		t.Fatalf("rc = %d out:\n%s", rc, errOut)
	}
	for _, label := range []string{launchd.RouterLabel, launchd.DoorLabel} {
		if _, err := os.Stat(plistPathFor(label)); !os.IsNotExist(err) {
			t.Fatalf("plist %s not removed", label)
		}
		if !strings.Contains(errOut, label+": booted out") {
			t.Fatalf("missing bootout report for %s:\n%s", label, errOut)
		}
	}

	// Second run: back to a clean noop.
	f.respond("bootout", "Boot-out failed: 3: No such process", errors.New("exit status 3"))
	errOut = captureStderr(t, func() { rc = cmdUpUninstall() })
	if rc != 0 || !strings.Contains(errOut, "nothing to do") {
		t.Fatalf("second uninstall rc = %d out:\n%s", rc, errOut)
	}
}

// TestUpSupervised: an installed-but-not-loaded job is re-bootstrapped
// by plain up; no plist means handled=false (caller spawns).
func TestUpSupervised(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		useFake(t)
		rc, handled := upSupervised("door", launchd.DoorLabel, nil, doorHealthPath, doorHealthMarker)
		if handled || rc != 0 {
			t.Fatalf("rc=%d handled=%v", rc, handled)
		}
	})
	t.Run("installed not loaded re-bootstraps", func(t *testing.T) {
		f := useFake(t)
		f.respond("bootstrap", "", nil)
		pp := installFakePlist(t, launchd.DoorLabel)
		srv := fakeDoor(t)
		defer srv.Close()
		var rc int
		var handled bool
		errOut := captureStderr(t, func() {
			rc, handled = upSupervised("door", launchd.DoorLabel, []string{hostPort(t, srv.URL)}, doorHealthPath, doorHealthMarker)
		})
		if !handled || rc != 0 {
			t.Fatalf("rc=%d handled=%v\n%s", rc, handled, errOut)
		}
		boots := f.callsFor("bootstrap")
		if len(boots) != 1 || boots[0][2] != pp {
			t.Fatalf("bootstrap calls = %v", f.calls)
		}
		if !strings.Contains(errOut, "re-bootstrapping") {
			t.Fatalf("missing re-bootstrap report:\n%s", errOut)
		}
	})
}

func TestVersionSkewLine(t *testing.T) {
	cases := []struct {
		running string
		want    string
	}{
		{version.Version, ""},
		{strings.TrimPrefix(version.Version, "v"), ""},
		{"v0.3.0-abc", fmt.Sprintf("restart to adopt %s (running v0.3.0-abc)", version.Version)},
		{"", fmt.Sprintf("restart to adopt %s (running unknown)", version.Version)},
	}
	for _, tc := range cases {
		if got := versionSkewLine(tc.running); got != tc.want {
			t.Errorf("versionSkewLine(%q) = %q want %q", tc.running, got, tc.want)
		}
	}
}

// TestPrintStatusVersionSkew: a router reporting a different version
// than this binary gets a "restart to adopt" line in status.
func TestPrintStatusVersionSkew(t *testing.T) {
	useFake(t)
	admin := fakeAdminWithVersion(t, "v9.9.9-old")
	defer admin.Close()
	pidDir := t.TempDir()
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(pidDir, "gw.pid"))
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(pidDir, "door.pid"))
	lc := &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: hostPort(t, admin.URL)}
	var buf strings.Builder
	printStatus(lc, &buf, false)
	want := fmt.Sprintf("restart to adopt %s (running v9.9.9-old)", version.Version)
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("status missing %q:\n%s", want, buf.String())
	}
}

// TestPrintStatusNoSkewLine: matching versions render no skew line.
func TestPrintStatusNoSkewLine(t *testing.T) {
	useFake(t)
	admin := fakeAdminWithVersion(t, version.Version)
	defer admin.Close()
	pidDir := t.TempDir()
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(pidDir, "gw.pid"))
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(pidDir, "door.pid"))
	lc := &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: hostPort(t, admin.URL)}
	var buf strings.Builder
	printStatus(lc, &buf, false)
	if strings.Contains(buf.String(), "restart to adopt") {
		t.Fatalf("unexpected skew line:\n%s", buf.String())
	}
}

// TestPrintStatusSupervisionState: a supervised router renders its
// launchd label (verbose only); a manual one says manual (verbose only).
// Compact mode drops the supervision text from the Router up line.
func TestPrintStatusSupervisionState(t *testing.T) {
	admin := fakeAdminWithVersion(t, version.Version)
	defer admin.Close()
	pidDir := t.TempDir()
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(pidDir, "gw.pid"))
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(pidDir, "door.pid"))
	lc := &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: hostPort(t, admin.URL)}

	t.Run("manual (verbose)", func(t *testing.T) {
		useFake(t)
		var buf strings.Builder
		printStatus(lc, &buf, true)
		if !strings.Contains(buf.String(), "Router:  up (pid 0, manual)") {
			t.Fatalf("missing manual supervision state:\n%s", buf.String())
		}
	})
	t.Run("manual (compact drops label)", func(t *testing.T) {
		useFake(t)
		var buf strings.Builder
		printStatus(lc, &buf, false)
		if strings.Contains(buf.String(), "Router:  up (pid 0, manual)") {
			t.Fatalf("compact output should not include supervision label:\n%s", buf.String())
		}
		if !strings.Contains(buf.String(), "Router:  up (pid 0)") {
			t.Fatalf("compact output missing router up line:\n%s", buf.String())
		}
	})
	t.Run("supervised (verbose)", func(t *testing.T) {
		f := useFake(t)
		f.respond("print", "ok", nil)
		installFakePlist(t, launchd.RouterLabel)
		var buf strings.Builder
		printStatus(lc, &buf, true)
		if !strings.Contains(buf.String(), "launchd "+launchd.RouterLabel) {
			t.Fatalf("missing launchd supervision state:\n%s", buf.String())
		}
	})
}

// shortenTimeout swaps a package timeout var for one test.
func shortenTimeout(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

// TestDownComponentWaitsForBootoutCompletion pins the restart-race
// fix: launchctl bootout returns while the label is still registered,
// so down must keep polling `launchctl print gui/<uid>/<label>` until
// the label is gone before reporting success. The fake label lingers
// for two polls after the bootout, then disappears.
func TestDownComponentWaitsForBootoutCompletion(t *testing.T) {
	f := useFake(t)
	f.respond("bootout", "", nil)
	// One print for the supervision check, two lingering polls after
	// bootout; the queue then falls through to the default error
	// (label gone).
	f.queue("print", "ok", nil)
	f.queue("print", "ok", nil)
	f.queue("print", "ok", nil)
	installFakePlist(t, launchd.RouterLabel)
	pf := filepath.Join(t.TempDir(), "gw.pid")
	var rc int
	errOut := captureStderr(t, func() {
		rc = downComponent("router", launchd.RouterLabel, pf, []string{closedPortAddr(t)}, routerHealthPath, routerHealthMarker)
	})
	if rc != 0 {
		t.Fatalf("rc = %d want 0\n%s", rc, errOut)
	}
	if len(f.callsFor("bootout")) != 1 {
		t.Fatalf("bootout calls = %v", f.calls)
	}
	// Supervision check + 2 lingering polls + the poll that saw the
	// label gone = at least 4 prints, all but the first after bootout.
	prints := f.callsFor("print")
	if len(prints) < 4 {
		t.Fatalf("down did not poll for label teardown after bootout: %v", f.calls)
	}
	if f.calls[1][0] != "bootout" {
		t.Fatalf("expected bootout right after the supervision check, got %v", f.calls)
	}
}

// TestDownComponentBootoutNeverCompletes: a label that never leaves
// the domain makes down fail loudly instead of reporting a success
// that a follow-up up would misread.
func TestDownComponentBootoutNeverCompletes(t *testing.T) {
	f := useFake(t)
	f.respond("print", "ok", nil) // label loaded forever
	f.respond("bootout", "", nil)
	shortenTimeout(t, &bootoutGoneTimeout, 300*time.Millisecond)
	installFakePlist(t, launchd.RouterLabel)
	pf := filepath.Join(t.TempDir(), "gw.pid")
	var rc int
	errOut := captureStderr(t, func() {
		rc = downComponent("router", launchd.RouterLabel, pf, []string{closedPortAddr(t)}, routerHealthPath, routerHealthMarker)
	})
	if rc != 1 {
		t.Fatalf("rc = %d want 1\n%s", rc, errOut)
	}
	if !strings.Contains(errOut, "bootout did not complete") || !strings.Contains(errOut, launchd.RouterLabel) {
		t.Fatalf("timeout not reported loudly:\n%s", errOut)
	}
}

// TestWaitLabelGoneDisabled: BASETEN_SWITCH_LAUNCHD=off must short-circuit
// without consulting launchctl.
func TestWaitLabelGoneDisabled(t *testing.T) {
	f := useFake(t)
	f.respond("print", "ok", nil)
	t.Setenv("BASETEN_SWITCH_LAUNCHD", "off")
	if err := waitLabelGone(launchd.RouterLabel, time.Second); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("launchctl consulted under BASETEN_SWITCH_LAUNCHD=off: %v", f.calls)
	}
}

// TestUpSupervisedBootstrapFailureLoud: a bootstrap error is printed
// with the launchctl output and exits nonzero.
func TestUpSupervisedBootstrapFailureLoud(t *testing.T) {
	f := useFake(t)
	f.respond("bootstrap", "Bootstrap failed: 5: Input/output error", errors.New("exit status 5"))
	installFakePlist(t, launchd.RouterLabel)
	var rc int
	var handled bool
	errOut := captureStderr(t, func() {
		rc, handled = upSupervised("router", launchd.RouterLabel, nil, routerHealthPath, routerHealthMarker)
	})
	if !handled || rc != 1 {
		t.Fatalf("rc=%d handled=%v\n%s", rc, handled, errOut)
	}
	if !strings.Contains(errOut, "bootstrap failed") || !strings.Contains(errOut, "Input/output error") {
		t.Fatalf("bootstrap failure not loud:\n%s", errOut)
	}
}

// TestUpSupervisedLabelVanishes: up sees a loaded label (the bootout
// still in flight), waits, and the label disappears instead of coming
// up. The failure must say the label is gone, not just "not healthy".
func TestUpSupervisedLabelVanishes(t *testing.T) {
	f := useFake(t)
	f.queue("print", "ok", nil) // loaded at the supervision check, gone after
	installFakePlist(t, launchd.RouterLabel)
	shortenTimeout(t, &readyTimeout, 200*time.Millisecond)
	var rc int
	var handled bool
	errOut := captureStderr(t, func() {
		rc, handled = upSupervised("router", launchd.RouterLabel, []string{closedPortAddr(t)}, routerHealthPath, routerHealthMarker)
	})
	if !handled || rc != 1 {
		t.Fatalf("rc=%d handled=%v\n%s", rc, handled, errOut)
	}
	if !strings.Contains(errOut, "no longer loaded") {
		t.Fatalf("vanished label not reported:\n%s", errOut)
	}
	if len(f.callsFor("bootstrap")) != 0 {
		t.Fatalf("unexpected bootstrap: %v", f.calls)
	}
}

// TestCmdRestartSupervisedRebootstrapsOnce drives the full restart
// sequence over the exec seam against a lingering label: down polls
// until the label is gone, up then sees installed-but-not-loaded and
// bootstraps exactly once, and the run exits 0 once the router
// answers health.
func TestCmdRestartSupervisedRebootstrapsOnce(t *testing.T) {
	f := useFake(t)
	adminAddr := closedPortAddr(t)
	cfg := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(cfg, []byte("clients: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pidDir := t.TempDir()
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", cfg)
	t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", adminAddr)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(pidDir, "gw.pid"))
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(pidDir, "door.pid"))

	installFakePlist(t, launchd.RouterLabel)
	f.respond("bootout", "", nil)
	f.respond("bootstrap", "", nil)
	// down's supervision check plus two lingering teardown polls; the
	// queue then falls through to the default error, so down's next
	// poll and up's supervision check both read the label as gone.
	f.queue("print", "ok", nil)
	f.queue("print", "ok", nil)
	f.queue("print", "ok", nil)
	// The router only answers health after up bootstraps it.
	var srv *http.Server
	f.hook = func(args []string) {
		if args[0] != "bootstrap" || srv != nil {
			return
		}
		ln, err := net.Listen("tcp", adminAddr)
		if err != nil {
			t.Errorf("listen %s: %v", adminAddr, err)
			return
		}
		mux := http.NewServeMux()
		mux.HandleFunc(routerHealthPath, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"ok":true,"uptime_seconds":1,"version":"dev"}`)
		})
		srv = &http.Server{Handler: mux}
		go srv.Serve(ln)
	}
	t.Cleanup(func() {
		if srv != nil {
			srv.Close()
		}
	})

	var rc int
	errOut := captureStderr(t, func() { rc = cmdRestart(nil) })
	if rc != 0 {
		t.Fatalf("restart rc = %d want 0\n%s", rc, errOut)
	}
	boots := f.callsFor("bootstrap")
	if len(boots) != 1 || boots[0][2] != plistPathFor(launchd.RouterLabel) {
		t.Fatalf("expected exactly one router bootstrap, got %v", f.calls)
	}
	if len(f.callsFor("bootout")) != 1 {
		t.Fatalf("expected exactly one bootout, got %v", f.calls)
	}
	if !strings.Contains(errOut, "re-bootstrapping") {
		t.Fatalf("up did not take the re-bootstrap path:\n%s", errOut)
	}
}

// fakeAdminWithVersion is fakeAdmin with a configurable healthz
// version, for skew rendering tests.
func fakeAdminWithVersion(t *testing.T, v string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":true,"uptime_seconds":42,"version":%q}`, v)
	})
	mux.HandleFunc("/v1/admin/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"clients":[]}`)
	})
	mux.HandleFunc("/v1/admin/auth/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"signed_in":true,"email":"user@example.com"}`)
	})
	return httptest.NewServer(mux)
}
