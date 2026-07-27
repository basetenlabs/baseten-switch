package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/launchd"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

// fakeSpawn swaps the spawnDetached seam for a recorder so no test
// ever forks the test binary as a daemon. act runs in place of the
// real fork (nil = record only); the returned pid is os.Getpid() so a
// pidfile written from it classifies as alive.
func fakeSpawn(t *testing.T, act func(args []string)) *[][]string {
	t.Helper()
	old := spawnDetached
	var calls [][]string
	spawnDetached = func(logPath string, args []string, extraEnv map[string]string) (int, error) {
		calls = append(calls, args)
		if act != nil {
			act(args)
		}
		return os.Getpid(), nil
	}
	t.Cleanup(func() { spawnDetached = old })
	return &calls
}

// sleepChild forks a killable placeholder process and returns its pid,
// so stopComponent's SIGTERM lands on something real but harmless. The
// reaping goroutine matters: without it the child would stay a zombie
// (signal 0 still succeeds) and terminateGateway would never see it
// die.
func sleepChild(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd.Process.Pid
}

// versionedComponent is a hand-managed fake component server on a
// fixed addr whose reported version can be swapped in place, so a
// faked respawn can move the "running" version without rebinding the
// port.
type versionedComponent struct {
	mu sync.Mutex
	v  string
}

func (c *versionedComponent) setVersion(v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.v = v
}

func (c *versionedComponent) version() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.v
}

func startVersionedAt(t *testing.T, addr, ver string, handler func(v string, w http.ResponseWriter, r *http.Request)) (*versionedComponent, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	c := &versionedComponent{v: ver}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(c.version(), w, r)
	})}
	go srv.Serve(ln)
	stop := func() { srv.Close() }
	t.Cleanup(stop)
	return c, stop
}

func startFakeRouterAt(t *testing.T, addr, ver string) (*versionedComponent, func()) {
	t.Helper()
	return startVersionedAt(t, addr, ver, func(v string, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == routerHealthPath {
			fmt.Fprintf(w, `{"ok":true,"uptime_seconds":1,"version":%q}`, v)
			return
		}
		http.NotFound(w, r)
	})
}

func startFakeDoorAt(t *testing.T, addr, ver string) (*versionedComponent, func()) {
	t.Helper()
	return startVersionedAt(t, addr, ver, func(v string, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == doorHealthPath {
			fmt.Fprintf(w, `{"router":"127.0.0.1:0","tripped":false,"version":%q,"fallbacks":[]}`, v)
			return
		}
		http.NotFound(w, r)
	})
}

// upEnv pins every lifecycle path (config, admin addr, pidfiles, logs)
// into scratch locations and returns the pidfile paths.
func upEnv(t *testing.T, adminAddr, doorAddr, clientAddr string) (gwPf, doorPf string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gateway.yaml")
	yaml := "clients:\n  - name: claude-code\n    enabled: true\n    bind_addr: " + clientAddr +
		"\n    protocol_shape: anthropic\n"
	if doorAddr != "" {
		yaml += "door:\n  ports:\n    - bind_addr: " + doorAddr + "\n      router_addr: " + clientAddr + "\n"
	}
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	gwPf = filepath.Join(dir, "gw.pid")
	doorPf = filepath.Join(dir, "door.pid")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", cfg)
	t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", adminAddr)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", gwPf)
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", doorPf)
	t.Setenv("BASETEN_SWITCH_GATEWAY_LOG", filepath.Join(dir, "router.log"))
	t.Setenv("BASETEN_SWITCH_DOOR_LOG", filepath.Join(dir, "door.log"))
	return gwPf, doorPf
}

// TestCmdUpAdoptsStaleComponents drives the whole-system adoption
// matrix end to end over the spawn seam: a first up finds router and
// door both running an older version, restarts each
// onto this binary with one adoption line per component, and exits 0;
// a second up finds everything current and is a complete no-op (no
// adoption lines, no new spawns). The menubar step stays off (TestMain
// seam); it has its own matrix in menubar_test.go.
func TestCmdUpAdoptsStaleComponents(t *testing.T) {
	useFake(t) // hermetic launchd: nothing installed, so the unsupervised paths run
	adminAddr := closedPortAddr(t)
	doorAddr := closedPortAddr(t)
	clientAddr := closedPortAddr(t)
	gwPf, doorPf := upEnv(t, adminAddr, doorAddr, clientAddr)

	router, _ := startFakeRouterAt(t, adminAddr, "v0.0.9-old")
	doorC, _ := startFakeDoorAt(t, doorAddr, "v0.0.9-old")

	// Each stale component's pidfile holds a live killable child so the
	// pidfile SIGTERM stop path runs for real.
	for _, pf := range []string{gwPf, doorPf} {
		if err := pidfile.WriteAt(pf, sleepChild(t)); err != nil {
			t.Fatal(err)
		}
	}

	spawns := fakeSpawn(t, func(args []string) {
		// The faked respawn moves the running version onto this binary,
		// exactly what a real restart onto the new executable does.
		switch args[0] {
		case "gateway":
			router.setVersion(version.Version)
		case "door":
			doorC.setVersion(version.Version)
		}
	})

	var rc int
	errOut := captureStderr(t, func() { rc = cmdUp(nil) })
	if rc != 0 {
		t.Fatalf("first up rc = %d want 0\n%s", rc, errOut)
	}
	for _, name := range []string{"router", "door"} {
		want := fmt.Sprintf("%s: adopting %s (was running v0.0.9-old)", name, version.Version)
		if !strings.Contains(errOut, want) {
			t.Fatalf("missing adoption line %q:\n%s", want, errOut)
		}
	}
	if len(*spawns) != 2 {
		t.Fatalf("spawn calls = %v want gateway, door", *spawns)
	}
	for i, wantVerb := range []string{"gateway", "door"} {
		if (*spawns)[i][0] != wantVerb {
			t.Fatalf("spawn %d = %v want %s", i, (*spawns)[i], wantVerb)
		}
	}

	// Second up: everything current, so a complete no-op.
	errOut = captureStderr(t, func() { rc = cmdUp(nil) })
	if rc != 0 {
		t.Fatalf("second up rc = %d want 0\n%s", rc, errOut)
	}
	if strings.Contains(errOut, "adopting") {
		t.Fatalf("second up adopted a current component:\n%s", errOut)
	}
	for _, want := range []string{"router: already up", "door: already up"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("second up missing %q:\n%s", want, errOut)
		}
	}
	if len(*spawns) != 2 {
		t.Fatalf("second up spawned: %v", *spawns)
	}
}

// TestCmdUpStartsWhenDown covers the not-running row of the matrix:
// up starts router and door over the spawn seam.
func TestCmdUpStartsWhenDown(t *testing.T) {
	useFake(t)
	adminAddr := closedPortAddr(t)
	doorAddr := closedPortAddr(t)
	clientAddr := closedPortAddr(t)
	upEnv(t, adminAddr, doorAddr, clientAddr)

	spawns := fakeSpawn(t, func(args []string) {
		switch args[0] {
		case "gateway":
			startFakeRouterAt(t, adminAddr, version.Version)
		case "door":
			startFakeDoorAt(t, doorAddr, version.Version)
		}
	})

	var rc int
	errOut := captureStderr(t, func() { rc = cmdUp(nil) })
	if rc != 0 {
		t.Fatalf("rc = %d want 0\n%s", rc, errOut)
	}
	if strings.Contains(errOut, "adopting") {
		t.Fatalf("up adopted with nothing running:\n%s", errOut)
	}
	if len(*spawns) != 2 || (*spawns)[0][0] != "gateway" || (*spawns)[1][0] != "door" {
		t.Fatalf("spawn calls = %v want gateway then door", *spawns)
	}
}

// TestUpRouterAdoptsSupervisedStale pins the supervised half of the
// adoption matrix: a launchd-owned router running a stale version is
// booted out and re-bootstrapped onto this binary (the same path
// up --install uses), never SIGTERMed.
func TestUpRouterAdoptsSupervisedStale(t *testing.T) {
	f := useFake(t)
	adminAddr := closedPortAddr(t)
	pidDir := t.TempDir()
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(pidDir, "gw.pid"))
	t.Setenv("BASETEN_SWITCH_GATEWAY_LOG", filepath.Join(pidDir, "router.log"))
	installFakePlist(t, launchd.RouterLabel)
	f.respond("bootout", "", nil)
	f.respond("bootstrap", "", nil)
	// Loaded at adoptComponent's precondition check and again at
	// downComponent's supervision check; every later print (the
	// post-bootout teardown polls, upSupervised's check) falls through
	// to the default error, reading the label as gone.
	f.queue("print", "ok", nil)
	f.queue("print", "ok", nil)

	_, stopStale := startFakeRouterAt(t, adminAddr, "v0.0.9-old")
	f.hook = func(args []string) {
		switch args[0] {
		case "bootout":
			// The bootout tears down the stale process...
			stopStale()
		case "bootstrap":
			// ...and the re-bootstrap brings up the new binary.
			startFakeRouterAt(t, adminAddr, version.Version)
		}
	}

	lc := &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: adminAddr}
	var rc int
	errOut := captureStderr(t, func() { rc = upRouter(lc) })
	if rc != 0 {
		t.Fatalf("rc = %d want 0\n%s", rc, errOut)
	}
	want := fmt.Sprintf("router: adopting %s (was running v0.0.9-old)", version.Version)
	if !strings.Contains(errOut, want) {
		t.Fatalf("missing adoption line %q:\n%s", want, errOut)
	}
	if len(f.callsFor("bootout")) != 1 || len(f.callsFor("bootstrap")) != 1 {
		t.Fatalf("expected one bootout and one bootstrap, got %v", f.calls)
	}
	bootoutAt, bootstrapAt := -1, -1
	for i, c := range f.calls {
		switch c[0] {
		case "bootout":
			bootoutAt = i
		case "bootstrap":
			bootstrapAt = i
		}
	}
	if bootoutAt > bootstrapAt {
		t.Fatalf("bootstrap before bootout: %v", f.calls)
	}
}

// TestUpRouterLeavesUnmanagedStaleAlone: a stale router answering
// health with no live pidfile and no launchd job is not ours to stop
// (stopComponent would refuse). up leaves it alone with a skew note
// and exits 0, mirroring the web rule, instead of announcing an
// adoption it cannot perform.
func TestUpRouterLeavesUnmanagedStaleAlone(t *testing.T) {
	useFake(t) // hermetic launchd: nothing installed
	adminAddr := closedPortAddr(t)
	pidDir := t.TempDir()
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(pidDir, "gw.pid")) // never written
	t.Setenv("BASETEN_SWITCH_GATEWAY_LOG", filepath.Join(pidDir, "router.log"))

	startFakeRouterAt(t, adminAddr, "v0.0.9-old")

	spawns := fakeSpawn(t, nil)
	lc := &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: adminAddr}
	var rc int
	errOut := captureStderr(t, func() { rc = upRouter(lc) })
	if rc != 0 {
		t.Fatalf("rc = %d want 0\n%s", rc, errOut)
	}
	if !strings.Contains(errOut, "not managed by up") {
		t.Fatalf("missing leave-alone report:\n%s", errOut)
	}
	if strings.Contains(errOut, "adopting") || strings.Contains(errOut, "refusing to guess") {
		t.Fatalf("announced or attempted a stop it cannot perform:\n%s", errOut)
	}
	if len(*spawns) != 0 {
		t.Fatalf("router was respawned: %v", *spawns)
	}
}

// TestUpRouterRefusesSupervisedBinaryMismatch: a launchd-supervised
// stale router whose plist records a different binary than the one
// running up is a loud refusal, never a bounce: the re-bootstrap would
// relaunch the same stale binary, so every up would kill and restart
// live infrastructure without ever converging.
func TestUpRouterRefusesSupervisedBinaryMismatch(t *testing.T) {
	f := useFake(t)
	adminAddr := closedPortAddr(t)
	pidDir := t.TempDir()
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(pidDir, "gw.pid"))
	t.Setenv("BASETEN_SWITCH_GATEWAY_LOG", filepath.Join(pidDir, "router.log"))

	pp := plistPathFor(launchd.RouterLabel)
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := launchd.RenderPlist(launchd.Job{
		Label:            launchd.RouterLabel,
		ProgramArguments: []string{"/somewhere/else/baseten-switch", "gateway", "start", "--foreground"},
		LogPath:          filepath.Join(pidDir, "router.log"),
	})
	if err := os.WriteFile(pp, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	f.respond("print", "ok", nil) // label loaded

	startFakeRouterAt(t, adminAddr, "v0.0.9-old")

	spawns := fakeSpawn(t, nil)
	lc := &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: adminAddr}
	var rc int
	errOut := captureStderr(t, func() { rc = upRouter(lc) })
	if rc != 1 {
		t.Fatalf("rc = %d want 1\n%s", rc, errOut)
	}
	if !strings.Contains(errOut, "/somewhere/else/baseten-switch") || !strings.Contains(errOut, "relaunch the same stale binary") {
		t.Fatalf("missing binary-mismatch refusal naming the plist binary:\n%s", errOut)
	}
	if strings.Contains(errOut, "adopting") {
		t.Fatalf("announced an adoption it refused:\n%s", errOut)
	}
	if len(f.callsFor("bootout")) != 0 || len(f.callsFor("bootstrap")) != 0 || len(*spawns) != 0 {
		t.Fatalf("the running router was bounced: launchctl %v spawns %v", f.calls, *spawns)
	}
}

// TestUpRouterAdoptionVerifiesVersion: after an adoption restart the
// reported version is re-checked. Here the pidfile pid dies but a
// stale process keeps the port, so the health gate passes while the
// old binary keeps serving; up must fail loudly instead of printing
// success.
func TestUpRouterAdoptionVerifiesVersion(t *testing.T) {
	useFake(t)
	adminAddr := closedPortAddr(t)
	pidDir := t.TempDir()
	gwPf := filepath.Join(pidDir, "gw.pid")
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", gwPf)
	t.Setenv("BASETEN_SWITCH_GATEWAY_LOG", filepath.Join(pidDir, "router.log"))
	if err := pidfile.WriteAt(gwPf, sleepChild(t)); err != nil {
		t.Fatal(err)
	}

	startFakeRouterAt(t, adminAddr, "v0.0.9-old")

	// The faked respawn does NOT move the version: the stale server
	// keeps the port and keeps reporting the old version.
	spawns := fakeSpawn(t, nil)
	lc := &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: adminAddr}
	var rc int
	errOut := captureStderr(t, func() { rc = upRouter(lc) })
	if rc != 1 {
		t.Fatalf("rc = %d want 1\n%s", rc, errOut)
	}
	if !strings.Contains(errOut, "still reports v0.0.9-old") {
		t.Fatalf("missing failed-adoption report:\n%s", errOut)
	}
	if len(*spawns) != 1 {
		t.Fatalf("spawns = %v want one gateway respawn", *spawns)
	}
}

// TestUpRouterSkipsAdoptionOnVersionProbeError: the router answers the
// health probe as ours, then the separate version fetch fails (here: a
// 500). That is a transient error, not a pre-stamping binary; up must
// leave the component alone instead of bouncing a healthy router.
func TestUpRouterSkipsAdoptionOnVersionProbeError(t *testing.T) {
	useFake(t)
	adminAddr := closedPortAddr(t)
	pidDir := t.TempDir()
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(pidDir, "gw.pid"))
	t.Setenv("BASETEN_SWITCH_GATEWAY_LOG", filepath.Join(pidDir, "router.log"))

	// First request (the health probe) answers healthy; every later
	// request (the version fetch) fails.
	var mu sync.Mutex
	served := 0
	startVersionedAt(t, adminAddr, "v0.0.9-old", func(v string, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served++
		n := served
		mu.Unlock()
		if n > 1 {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"uptime_seconds":1,"version":%q}`, v)
	})

	spawns := fakeSpawn(t, nil)
	lc := &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: adminAddr}
	var rc int
	errOut := captureStderr(t, func() { rc = upRouter(lc) })
	if rc != 0 {
		t.Fatalf("rc = %d want 0\n%s", rc, errOut)
	}
	if !strings.Contains(errOut, "version probe") || !strings.Contains(errOut, "leaving it alone") {
		t.Fatalf("missing probe-failure leave-alone report:\n%s", errOut)
	}
	if strings.Contains(errOut, "adopting") || len(*spawns) != 0 {
		t.Fatalf("bounced a component on a transient probe failure (spawns %v):\n%s", *spawns, errOut)
	}
}
