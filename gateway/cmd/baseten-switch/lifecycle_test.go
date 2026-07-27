package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/door"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

// hostPort strips the scheme from an httptest server URL.
func hostPort(t *testing.T, url string) string {
	t.Helper()
	return strings.TrimPrefix(url, "http://")
}

// closedPortAddr returns a 127.0.0.1 address that nothing listens on.
func closedPortAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestResolveLifecycleRefusals(t *testing.T) {
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "gw.pid"))
	cases := []struct {
		name    string
		content string // "" = do not create the file
		wants   []string
	}{
		{"missing config", "", []string{"baseten-switch config init", "gateway.example.yaml", "BASETEN_SWITCH_CONFIG_PATH"}},
		{"malformed yaml", "clients: [::nope", []string{"malformed"}},
		{"broken door section", `
clients:
  - name: c
    enabled: true
    bind_addr: 127.0.0.1:28181
door:
  cooldown: -3s
  ports:
    - bind_addr: 127.0.0.1:28081
      router_addr: 127.0.0.1:28181
`, []string{"malformed", "door"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gateway.yaml")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
			_, err := resolveLifecycle()
			if err == nil {
				t.Fatal("expected refusal")
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("refusal %q does not name the file %q", err.Error(), path)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

func TestResolveLifecycleValidConfig(t *testing.T) {
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "gw.pid"))
	t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", "127.0.0.1:28786")
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	yaml := `
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:28181
    protocol_shape: anthropic
door:
  ports:
    - bind_addr: 127.0.0.1:28081
      router_addr: 127.0.0.1:28181
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	lc, err := resolveLifecycle()
	if err != nil {
		t.Fatal(err)
	}
	if lc.path != path {
		t.Fatalf("path = %q want %q", lc.path, path)
	}
	if lc.adminAddr != "127.0.0.1:28786" {
		t.Fatalf("adminAddr = %q", lc.adminAddr)
	}
	if len(lc.doorSpecs) != 1 || lc.doorSpecs[0].ListenAddr != "127.0.0.1:28081" {
		t.Fatalf("doorSpecs = %+v", lc.doorSpecs)
	}
}

func TestResolveLifecycleNoDoorSection(t *testing.T) {
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "gw.pid"))
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte("clients: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	lc, err := resolveLifecycle()
	if err != nil {
		t.Fatal(err)
	}
	if len(lc.doorSpecs) != 0 {
		t.Fatalf("expected no door specs, got %+v", lc.doorSpecs)
	}
}

// TestProbePort pins the portable ours/foreign/down classification:
// answers our health endpoint = ours; TCP accepts but is not our
// endpoint = foreign; connection refused = down.
func TestProbePort(t *testing.T) {
	ours := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/doorz" {
			fmt.Fprint(w, `{"tripped":false}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer ours.Close()
	foreignHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer foreignHTTP.Close()
	wrongBody := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "welcome to something else")
	}))
	defer wrongBody.Close()

	cases := []struct {
		name   string
		addr   string
		path   string
		marker string
		want   portState
	}{
		{"ours", hostPort(t, ours.URL), "/doorz", "tripped", portOurs},
		{"foreign 404", hostPort(t, foreignHTTP.URL), "/doorz", "tripped", portForeign},
		{"foreign wrong body", hostPort(t, wrongBody.URL), "/doorz", "tripped", portForeign},
		{"down", closedPortAddr(t), "/doorz", "tripped", portDown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := probePort(tc.addr, tc.path, tc.marker); got != tc.want {
				t.Fatalf("probePort(%s%s) = %s want %s", tc.addr, tc.path, got, tc.want)
			}
		})
	}
}

// fakeAdmin serves the three admin endpoints status uses.
func fakeAdmin(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"uptime_seconds":42,"version":"dev"}`)
	})
	mux.HandleFunc("/v1/admin/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"clients":[
			{"name":"claude-code","enabled":true,"bind_addr":"127.0.0.1:18081","effective_route":"baseten","protocol_shape":"anthropic"},
			{"name":"codex","enabled":true,"bind_addr":"127.0.0.1:18081","effective_route":"openai","protocol_shape":"openai"},
			{"name":"parked","enabled":false,"bind_addr":"127.0.0.1:18082","effective_route":"baseten","protocol_shape":"openai"}
		]}`)
	})
	mux.HandleFunc("/v1/admin/auth/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"signed_in":true,"email":"user@example.com","fallback_enabled":false,"fallback_in_use":false}`)
	})
	return httptest.NewServer(mux)
}

func fakeDoor(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/doorz" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"port":8081,"router":"127.0.0.1:18081","tripped":false,"version":"dev",
			"fallbacks":[{"prefix":"/v1/messages"},{"prefix":"/v1/chat/completions"},{"prefix":"/v1/responses"}]}`)
	}))
}

// TestPrintStatusExitCodes drives the status renderer against
// httptest-backed components: exit 0 only when every managed
// component is up; DOWN and FOREIGN states exit nonzero and are named.
func TestPrintStatusExitCodes(t *testing.T) {
	admin := fakeAdmin(t)
	defer admin.Close()
	doorSrv := fakeDoor(t)
	defer doorSrv.Close()
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer foreign.Close()

	pidDir := t.TempDir()
	gwPid := filepath.Join(pidDir, "gw.pid")
	doorPid := filepath.Join(pidDir, "door.pid")
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", gwPid)
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", doorPid)
	if err := pidfile.WriteAt(gwPid, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := pidfile.WriteAt(doorPid, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	doorSpec := func(addr string) []door.Config {
		return []door.Config{{ListenAddr: addr, RouterTarget: "127.0.0.1:18081"}}
	}

	cases := []struct {
		name     string
		lc       *lifecycleConfig
		wantExit int
		wants    []string
	}{
		{
			"all up",
			&lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: hostPort(t, admin.URL), doorSpecs: doorSpec(hostPort(t, doorSrv.URL))},
			0,
			[]string{"Router:  up", "claude-code", "switch ON", "codex", "switch OFF",
				"Door:    up", "not tripped", "3 fallback rules",
				"Auth:    signed in (OAuth, user@example.com)"},
		},
		{
			"router down",
			&lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: closedPortAddr(t), doorSpecs: doorSpec(hostPort(t, doorSrv.URL))},
			1,
			[]string{"Router:  DOWN", "Door:    up", "Auth:    unknown"},
		},
		{
			"door down",
			&lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: hostPort(t, admin.URL), doorSpecs: doorSpec(closedPortAddr(t))},
			1,
			[]string{"Router:  up", "DOWN"},
		},
		{
			"door foreign",
			&lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: hostPort(t, admin.URL), doorSpecs: doorSpec(hostPort(t, foreign.URL))},
			1,
			[]string{"Router:  up", "FOREIGN"},
		},
		{
			"door not configured",
			&lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: hostPort(t, admin.URL)},
			0,
			[]string{"Router:  up", "Door:    not configured"},
		},
		{
			"disabled clients hidden",
			&lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: hostPort(t, admin.URL)},
			0,
			[]string{"claude-code"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := printStatus(tc.lc, &buf, false)
			if got != tc.wantExit {
				t.Fatalf("exit = %d want %d\n%s", got, tc.wantExit, buf.String())
			}
			for _, want := range tc.wants {
				if !strings.Contains(buf.String(), want) {
					t.Fatalf("output missing %q:\n%s", want, buf.String())
				}
			}
			if strings.Contains(buf.String(), "parked") {
				t.Fatalf("disabled client rendered:\n%s", buf.String())
			}
		})
	}
}

// TestPrintStatusVerbose pins the --verbose output: every fact dropped
// from the compact default must appear in verbose mode. The compact
// default drops launchd labels, binary paths, the admin addr, the
// config path, the menubar bundle path, and the Logs block. This test
// drives a fully-up system and asserts each dropped fact is present in
// verbose and absent in compact.
func TestPrintStatusVerbose(t *testing.T) {
	admin := fakeAdmin(t)
	defer admin.Close()
	doorSrv := fakeDoor(t)
	defer doorSrv.Close()
	pidDir := t.TempDir()
	gwPid := filepath.Join(pidDir, "gw.pid")
	doorPid := filepath.Join(pidDir, "door.pid")
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", gwPid)
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", doorPid)
	if err := pidfile.WriteAt(gwPid, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := pidfile.WriteAt(doorPid, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	// Menubar fixture: pin HOME, enable the menubar row (darwin), and
	// fake the running process so the menubar bundle path (a verbose-only
	// fact) is present in the output.
	menubarFixture(t)
	const mbBundle = "/Users/example/Applications/Baseten Switch.app"
	const mbComm = mbBundle + "/Contents/MacOS/BasetenSwitch"
	mbFake := installProcFake(t, true, map[string]string{plist(mbBundle): version.Version})
	mbFake.comm = mbComm

	const cfgPath = "/tmp/gw-verbose.yaml"
	lc := &lifecycleConfig{
		path:      cfgPath,
		adminAddr: hostPort(t, admin.URL),
		doorSpecs: []door.Config{{ListenAddr: hostPort(t, doorSrv.URL), RouterTarget: "127.0.0.1:18081"}},
	}

	// Verbose: every dropped fact present.
	var vbuf bytes.Buffer
	if rc := printStatus(lc, &vbuf, true); rc != 0 {
		t.Fatalf("verbose rc = %d want 0\n%s", rc, vbuf.String())
	}
	verbose := vbuf.String()
	for _, want := range []string{
		"admin " + hostPort(t, admin.URL), // admin addr
		"config " + cfgPath,               // config path
		"Logs:    router",                 // logs block
		"door  ",                          // door log in logs block
		"manual",                          // launchd label / supervision text
		mbBundle,                          // menubar bundle path
	} {
		if !strings.Contains(verbose, want) {
			t.Fatalf("verbose output missing %q:\n%s", want, verbose)
		}
	}
	// The binary/exe path is verbose-only on darwin (processExePath returns
	// "" off-darwin). When present, it must appear in verbose only.
	if exePath := processExePath(os.Getpid()); exePath != "" {
		if !strings.Contains(verbose, exePath) {
			t.Fatalf("verbose output missing exe path %q:\n%s", exePath, verbose)
		}
	}

	// Compact: every dropped fact absent.
	var cbuf bytes.Buffer
	printStatus(lc, &cbuf, false)
	compact := cbuf.String()
	for _, notWant := range []string{
		"admin " + hostPort(t, admin.URL), // admin addr
		"config " + cfgPath,               // config path
		"Logs:",                           // logs block
		"manual",                          // launchd label / supervision text
		mbBundle,                          // menubar bundle path
	} {
		if strings.Contains(compact, notWant) {
			t.Fatalf("compact output should not contain %q:\n%s", notWant, compact)
		}
	}
	if exePath := processExePath(os.Getpid()); exePath != "" {
		if strings.Contains(compact, exePath) {
			t.Fatalf("compact output should not contain exe path %q:\n%s", exePath, compact)
		}
	}
}

// TestPrintStatusMenubarRow pins the status Menubar row end to end:
// the up row carries the pid and bundle version with the bundle path
// on its own indented line (verbose only; compact keeps just the state
// line), a stale bundle gets the shared skew line (verbose only), and
// the row disappears entirely under BASETEN_SWITCH_MENUBAR=off and off-darwin. A
// DOWN menubar keeps exit code 0: scripts gate on status for server
// health, and the app is a UI convenience, not a managed availability
// component.
func TestPrintStatusMenubarRow(t *testing.T) {
	admin := fakeAdmin(t)
	defer admin.Close()

	const bundle = "/Users/example/Applications/Baseten Switch.app"
	const comm = bundle + "/Contents/MacOS/BasetenSwitch"

	setup := func(t *testing.T) *lifecycleConfig {
		pidDir := t.TempDir()
		t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(pidDir, "gw.pid"))
		t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(pidDir, "door.pid"))
		return &lifecycleConfig{path: "/tmp/gw.yaml", adminAddr: hostPort(t, admin.URL)}
	}

	t.Run("running current: row with version and indented bundle path (verbose)", func(t *testing.T) {
		lc := setup(t)
		menubarFixture(t)
		f := installProcFake(t, true, map[string]string{plist(bundle): version.Version})
		f.comm = comm
		var buf bytes.Buffer
		if rc := printStatus(lc, &buf, true); rc != 0 {
			t.Fatalf("rc = %d want 0\n%s", rc, buf.String())
		}
		out := buf.String()
		if !strings.Contains(out, fmt.Sprintf("Menubar: up (pid 123) %s\n", version.Version)) {
			t.Fatalf("missing menubar up row:\n%s", out)
		}
		if !strings.Contains(out, "\n         "+bundle+"\n") {
			t.Fatalf("bundle path not on its own indented line:\n%s", out)
		}
	})

	t.Run("running current: compact drops bundle path", func(t *testing.T) {
		lc := setup(t)
		menubarFixture(t)
		f := installProcFake(t, true, map[string]string{plist(bundle): version.Version})
		f.comm = comm
		var buf bytes.Buffer
		printStatus(lc, &buf, false)
		out := buf.String()
		if !strings.Contains(out, fmt.Sprintf("Menubar: up (pid 123) %s\n", version.Version)) {
			t.Fatalf("missing menubar up row:\n%s", out)
		}
		if strings.Contains(out, "\n         "+bundle+"\n") {
			t.Fatalf("compact output should not include bundle path:\n%s", out)
		}
	})

	t.Run("running stale: skew line under the row (verbose)", func(t *testing.T) {
		lc := setup(t)
		menubarFixture(t)
		f := installProcFake(t, true, map[string]string{plist(bundle): "v0.0.9"})
		f.comm = comm
		var buf bytes.Buffer
		if rc := printStatus(lc, &buf, true); rc != 0 {
			t.Fatalf("rc = %d want 0 (skew is not down)\n%s", rc, buf.String())
		}
		out := buf.String()
		if !strings.Contains(out, "Menubar: up (pid 123) v0.0.9\n") {
			t.Fatalf("missing stale menubar row:\n%s", out)
		}
		if !strings.Contains(out, "restart to adopt "+version.Version+" (running v0.0.9)") {
			t.Fatalf("missing menubar skew line:\n%s", out)
		}
	})

	t.Run("running stale: compact keeps state line and skew line", func(t *testing.T) {
		lc := setup(t)
		menubarFixture(t)
		f := installProcFake(t, true, map[string]string{plist(bundle): "v0.0.9"})
		f.comm = comm
		var buf bytes.Buffer
		printStatus(lc, &buf, false)
		out := buf.String()
		if !strings.Contains(out, "Menubar: up (pid 123) v0.0.9\n") {
			t.Fatalf("missing stale menubar state line:\n%s", out)
		}
		if !strings.Contains(out, "restart to adopt "+version.Version+" (running v0.0.9)") {
			t.Fatalf("compact output must include menubar skew line (actionable, always shown):\n%s", out)
		}
		// The bundle path is still dropped in compact mode.
		if strings.Contains(out, "\n         "+bundle+"\n") {
			t.Fatalf("compact output should not include bundle path:\n%s", out)
		}
	})

	t.Run("down: visible fix hint, exit stays 0", func(t *testing.T) {
		lc := setup(t)
		home := menubarFixture(t)
		mkApp(t, filepath.Join(home, "Applications"))
		useKeg(t)
		installProcFake(t, false, nil)
		var buf bytes.Buffer
		if rc := printStatus(lc, &buf, false); rc != 0 {
			t.Fatalf("rc = %d want 0 (a down menubar must not fail status)\n%s", rc, buf.String())
		}
		if !strings.Contains(buf.String(), "Menubar: DOWN (fix: baseten-switch up)\n") {
			t.Fatalf("missing menubar DOWN row:\n%s", buf.String())
		}
	})

	t.Run("BASETEN_SWITCH_MENUBAR=off: row omitted", func(t *testing.T) {
		lc := setup(t)
		menubarFixture(t)
		t.Setenv("BASETEN_SWITCH_MENUBAR", "off")
		installProcFake(t, true, nil)
		var buf bytes.Buffer
		printStatus(lc, &buf, false)
		if strings.Contains(buf.String(), "Menubar:") {
			t.Fatalf("menubar row rendered under BASETEN_SWITCH_MENUBAR=off:\n%s", buf.String())
		}
	})

	t.Run("non-darwin: row omitted", func(t *testing.T) {
		lc := setup(t)
		menubarFixture(t)
		forceMenubarGOOS(t, "linux")
		installProcFake(t, true, nil)
		var buf bytes.Buffer
		printStatus(lc, &buf, false)
		if strings.Contains(buf.String(), "Menubar:") {
			t.Fatalf("menubar row rendered off-darwin:\n%s", buf.String())
		}
	})
}

// TestStopComponent covers the pidfile handling for a second managed
// process: stale and corrupt pidfiles are cleaned as a noop, and a
// component that still answers health without a live pidfile is a
// refusal, never a guess.
func TestStopComponent(t *testing.T) {
	t.Run("missing pidfile no health", func(t *testing.T) {
		pf := filepath.Join(t.TempDir(), "door.pid")
		if rc := stopComponent("door", pf, []string{closedPortAddr(t)}, doorHealthPath, doorHealthMarker); rc != 0 {
			t.Fatalf("rc = %d want 0", rc)
		}
	})
	t.Run("stale pidfile removed", func(t *testing.T) {
		pf := filepath.Join(t.TempDir(), "door.pid")
		if err := pidfile.WriteAt(pf, deadPid()); err != nil {
			t.Fatal(err)
		}
		if rc := stopComponent("door", pf, nil, doorHealthPath, doorHealthMarker); rc != 0 {
			t.Fatalf("rc = %d want 0", rc)
		}
		if _, err := os.Stat(pf); !os.IsNotExist(err) {
			t.Fatal("stale pidfile not removed")
		}
	})
	t.Run("corrupt pidfile removed", func(t *testing.T) {
		pf := filepath.Join(t.TempDir(), "door.pid")
		if err := os.WriteFile(pf, []byte("gibberish\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if rc := stopComponent("door", pf, nil, doorHealthPath, doorHealthMarker); rc != 0 {
			t.Fatalf("rc = %d want 0", rc)
		}
		if _, err := os.Stat(pf); !os.IsNotExist(err) {
			t.Fatal("corrupt pidfile not removed")
		}
	})
	t.Run("health answers without pidfile refuses", func(t *testing.T) {
		srv := fakeDoor(t)
		defer srv.Close()
		pf := filepath.Join(t.TempDir(), "door.pid")
		if rc := stopComponent("door", pf, []string{hostPort(t, srv.URL)}, doorHealthPath, doorHealthMarker); rc != 1 {
			t.Fatalf("rc = %d want 1 (refusal)", rc)
		}
	})
	t.Run("stale pidfile but health answers refuses", func(t *testing.T) {
		srv := fakeDoor(t)
		defer srv.Close()
		pf := filepath.Join(t.TempDir(), "door.pid")
		if err := pidfile.WriteAt(pf, deadPid()); err != nil {
			t.Fatal(err)
		}
		if rc := stopComponent("door", pf, []string{hostPort(t, srv.URL)}, doorHealthPath, doorHealthMarker); rc != 1 {
			t.Fatalf("rc = %d want 1 (refusal)", rc)
		}
	})
}

func TestEnvWith(t *testing.T) {
	t.Setenv("BASETEN_SWITCH_LIFECYCLE_TEST_KEY", "old")
	env := envWith(map[string]string{"BASETEN_SWITCH_LIFECYCLE_TEST_KEY": "new"})
	found := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "BASETEN_SWITCH_LIFECYCLE_TEST_KEY=") {
			found++
			if kv != "BASETEN_SWITCH_LIFECYCLE_TEST_KEY=new" {
				t.Fatalf("override not applied: %q", kv)
			}
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one entry, got %d", found)
	}
}
