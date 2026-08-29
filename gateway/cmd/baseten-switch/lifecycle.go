// lifecycle.go implements the the lifecycle contract orchestrator:
// `baseten-switch up | down | status | restart`. up is a launcher, not a
// host: it validates the config, forks the router and the door as two
// independent detached daemons (own process groups, own pidfiles),
// gates readiness on /healthz + /doorz, prints the status summary and
// exits. No process exists whose death takes down both roles.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/door"
	"github.com/basetenlabs/baseten-switch/gateway/internal/launchd"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
)

const (
	routerHealthPath   = "/healthz"
	routerHealthMarker = "uptime_seconds"
	doorHealthPath     = "/doorz"
	doorHealthMarker   = "tripped"

	probeTimeout = 1 * time.Second
)

// readyTimeout bounds every health-gate wait. Package var so tests can
// shorten it.
var readyTimeout = 15 * time.Second

func doorPidfilePath() string { return pidfile.DoorPath() }

// lifecycleConfig is the resolved and validated system topology the
// lifecycle commands operate on.
type lifecycleConfig struct {
	path      string        // gateway.yaml path, pinned into children via BASETEN_SWITCH_CONFIG_PATH
	doorSpecs []door.Config // empty = no door: section (door not managed)
	adminAddr string        // router admin listener host:port
	notices   []string      // sticky-config notices for stderr
}

// resolveConfigPath resolves the gateway.yaml path exactly once:
// BASETEN_SWITCH_CONFIG_PATH, else the sticky recorded path, else the default
// (the lifecycle contract, `up` semantics 1). Shared by the lifecycle
// commands and the on/off switch verbs so every command agrees on
// which file it operates on.
func resolveConfigPath() (path string, notices []string) {
	path = os.Getenv("BASETEN_SWITCH_CONFIG_PATH")
	if path == "" {
		if p, notice := stickyConfigPath("", pidfile.ConfigStatePath(pidfile.Path())); p != "" || notice != "" {
			if notice != "" {
				notices = append(notices, notice)
			}
			path = p
		}
	}
	if path == "" {
		path = config.DefaultPath()
	}
	return path, notices
}

// resolveLifecycle resolves the config path exactly once
// (BASETEN_SWITCH_CONFIG_PATH, else the sticky recorded path, else the default)
// and validates it BEFORE anything starts. Missing or malformed config
// is a hard refusal naming the file and the fix; no half-started
// states (the lifecycle contract, `up` semantics 1-2).
func resolveLifecycle() (*lifecycleConfig, error) {
	path, notices := resolveConfigPath()
	f, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s", config.MissingConfigMessage(path))
		}
		return nil, fmt.Errorf("%s", config.MalformedConfigMessage(path, err))
	}
	lc := &lifecycleConfig{
		path:      path,
		adminAddr: envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr),
		notices:   notices,
	}
	if f.Door != nil && len(f.Door.Ports) > 0 {
		specs, err := door.SpecsFromConfig(f, door.SpecsOptions{})
		if err != nil {
			return nil, fmt.Errorf("%s", config.MalformedConfigMessage(path, fmt.Errorf("door section: %v", err)))
		}
		lc.doorSpecs = specs
	}
	return lc, nil
}

// --- port classification -------------------------------------------------

// portState classifies a component port: if it answers our health
// endpoint it is ours; else if a TCP connect succeeds a foreign
// process owns it; else it is down (the lifecycle contract, `up`
// semantics 3).
type portState int

const (
	portDown portState = iota
	portOurs
	portForeign
)

func (s portState) String() string {
	switch s {
	case portOurs:
		return "ours"
	case portForeign:
		return "foreign"
	}
	return "down"
}

func probePort(addr, path, marker string) portState {
	return probePortTimeout(addr, path, marker, probeTimeout)
}

func probePortTimeout(addr, path, marker string, timeout time.Duration) portState {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + addr + path)
	if err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode == 200 && (marker == "" || strings.Contains(string(body), marker)) {
			return portOurs
		}
		return portForeign
	}
	conn, derr := net.DialTimeout("tcp", addr, timeout)
	if derr == nil {
		conn.Close()
		return portForeign
	}
	return portDown
}

// portOwner names the pid(s) listening on addr via lsof, for foreign
// port reports. Best effort: machines without lsof (or without
// permission to see the owner) get an "unknown pid" label.
func portOwner(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "unknown pid"
	}
	out, err := exec.Command("lsof", "-ti", "tcp:"+port, "-sTCP:LISTEN").Output()
	pids := strings.Fields(string(out))
	if err != nil || len(pids) == 0 {
		return "unknown pid (lsof gave no answer)"
	}
	return "pid " + strings.Join(pids, ",")
}

// waitOurs polls addr+path until it answers as ours or the timeout
// expires. A transient "foreign" reading is retried, not fatal: a
// freshly spawned component can have its listener bound (TCP accepts)
// before its HTTP server starts serving, e.g. the router during
// pricing hydration. The caller classified the port as down before
// spawning, so a TCP-accepting owner here is almost certainly our
// child.
func waitOurs(addr, path, marker string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := portDown
	for time.Now().Before(deadline) {
		last = probePortTimeout(addr, path, marker, 500*time.Millisecond)
		if last == portOurs {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("no healthy response from http://%s%s within %s (last state: %s)", addr, path, timeout, last)
}

// --- up -------------------------------------------------------------------

func cmdUp(args []string) int {
	install, uninstall := false, false
	for _, a := range args {
		switch a {
		case "--install":
			install = true
		case "--uninstall":
			uninstall = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag for up: %s (usage: baseten-switch up [--install | --uninstall])\n", a)
			return 2
		}
	}
	if install && uninstall {
		fmt.Fprintln(os.Stderr, "up: --install and --uninstall are mutually exclusive")
		return 2
	}
	if uninstall {
		// Uninstall needs no config: it only boots out the labels and
		// removes the plist files.
		return cmdUpUninstall()
	}
	lc, err := resolveLifecycle()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if install {
		for _, n := range lc.notices {
			fmt.Fprintln(os.Stderr, n)
		}
		fmt.Fprintf(os.Stderr, "config: %s\n", lc.path)
		return cmdUpInstall(lc)
	}
	for _, n := range lc.notices {
		fmt.Fprintln(os.Stderr, n)
	}
	fmt.Fprintf(os.Stderr, "config: %s\n", lc.path)

	failures := 0
	failures += upRouter(lc)
	if len(lc.doorSpecs) > 0 {
		failures += upDoor(lc)
	} else {
		fmt.Fprintf(os.Stderr, "door: not configured (no door: section in %s); skipping\n", lc.path)
	}
	failures += upMenubar()

	fmt.Fprintln(os.Stderr, "")
	rc := printStatus(lc, os.Stdout, false)
	if failures > 0 && rc == 0 {
		rc = 1
	}
	return rc
}

// adoptResult classifies the outcome of an adoption attempt so the
// callers agree on what happens next.
type adoptResult int

const (
	adoptCurrent adoptResult = iota // running this binary's version; caller reports leave-alone
	adoptStopped                    // stale and stopped; caller falls through to its start path
	adoptSkipped                    // left alone on purpose (message printed); not a failure
	adoptFailed                     // adoption needed but refused or the stop failed (message printed)
)

// adoptComponent restarts a running component whose reported version
// differs from this binary's, so a plain `up` after an upgrade moves
// every component onto the new binary. It performs only the stop half
// (launchctl bootout for a supervised component, the pidfile SIGTERM
// path otherwise: the same machinery down uses); the caller falls
// through to its normal start path, which re-bootstraps a supervised
// job or respawns, then confirms the adoption via verifyAdoption. The
// version is fetched from healthAddrs[0]. Preconditions, checked
// before anything is announced or stopped:
//
//   - The version fetch must succeed. The component just answered the
//     health probe as ours, so a failed fetch is a transient error,
//     not a pre-stamping binary; restarting on it would bounce a
//     current component. A 200 response with no version field is the
//     real pre-stamping signal and is stale by definition.
//   - With a plist installed, the restart path is upSupervised, which
//     re-bootstraps the plist's recorded binary, not the invoking one.
//     When those differ the bounce can never converge (every up would
//     boot the same stale binary out and back in), so refuse loudly.
//   - Without launchd supervision the stop needs a live pidfile;
//     an unmanaged process is not ours to kill (mirrors the web rule),
//     so it is left alone and status keeps flagging the skew.
func adoptComponent(name, label, pf string, healthAddrs []string, healthPath, marker string) adoptResult {
	running, err := runningVersionAt(healthAddrs[0], healthPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: running, but the version probe on %s failed (%v); leaving it alone (rerun 'baseten-switch up' to retry)\n", name, healthPath, err)
		return adoptSkipped
	}
	if running == version.Version {
		return adoptCurrent
	}
	was := running
	if was == "" {
		was = "unknown"
	}
	s := superviseState(label)
	if s.installed {
		if plistBin := launchd.ProgramBinary(plistPathFor(label)); plistBin != "" {
			if ourBin, perr := launchd.StableProgramPath(); perr == nil && plistBin != ourBin {
				fmt.Fprintf(os.Stderr, "%s: running %s, but the launchd job %s runs %s, not this binary (%s); restarting would relaunch the same stale binary. Rerun 'baseten-switch up' from %s, or 'baseten-switch up --install' from this binary to move the supervision.\n",
					name, was, label, plistBin, ourBin, plistBin)
				return adoptFailed
			}
		}
	}
	if !s.supervised() {
		if state, _ := classifyPidfile(pf); state != pidfileAlive {
			fmt.Fprintf(os.Stderr, "%s: running %s but not managed by up (no live pidfile at %s, no loaded launchd job); leaving it alone ('status' keeps flagging the skew). Stop that process yourself, then rerun 'baseten-switch up'.\n",
				name, was, pf)
			return adoptSkipped
		}
	}
	fmt.Fprintf(os.Stderr, "%s: adopting %s (was running %s)\n", name, version.Version, was)
	if downComponent(name, label, pf, healthAddrs, healthPath, marker) != 0 {
		return adoptFailed
	}
	return adoptStopped
}

// verifyAdoption re-reads the component's reported version after an
// adoption restart. The readiness gates check the health marker alone,
// so a restart that did not actually move the version (a stale process
// that kept the port while the pidfile pid was killed, a plist edge the
// ProgramBinary check could not see) would otherwise print success and
// exit 0 while the old binary keeps serving.
func verifyAdoption(name, addr, path string) int {
	running, err := runningVersionAt(addr, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: restarted, but the version probe failed (%v); run 'baseten-switch status' to confirm the adoption\n", name, err)
		return 1
	}
	if running != version.Version {
		if running == "" {
			running = "unknown"
		}
		fmt.Fprintf(os.Stderr, "%s: restarted, but it still reports %s (wanted %s); the restart did not adopt this binary. Check the executable path in 'baseten-switch status'.\n",
			name, running, version.Version)
		return 1
	}
	return 0
}

// upRouter starts the router unless it is already healthy and current.
// A healthy router reporting a different version than this binary is
// restarted onto it (version adoption). A foreign owner of the admin
// port is reported with its pid, distinctly from "down".
func upRouter(lc *lifecycleConfig) int {
	adopting := false
	switch probePort(lc.adminAddr, routerHealthPath, routerHealthMarker) {
	case portOurs:
		switch adoptComponent("router", launchd.RouterLabel, gatewayPidfilePath(),
			[]string{lc.adminAddr}, routerHealthPath, routerHealthMarker) {
		case adoptCurrent:
			fmt.Fprintf(os.Stderr, "router: already up on %s; leaving it alone\n", lc.adminAddr)
			return 0
		case adoptSkipped:
			return 0
		case adoptFailed:
			return 1
		}
		adopting = true
		// Stopped for adoption; fall through to the normal start path.
	case portForeign:
		fmt.Fprintf(os.Stderr, "router: a foreign process (%s) owns %s; not starting. Free the port or change BASETEN_SWITCH_ADMIN_ADDR.\n",
			portOwner(lc.adminAddr), lc.adminAddr)
		return 1
	}
	// An installed LaunchAgent means launchd owns this component:
	// re-bootstrap a booted-out job instead of spawning an unmanaged
	// process next to the plist.
	if rc, handled := upSupervised("router", launchd.RouterLabel, []string{lc.adminAddr}, routerHealthPath, routerHealthMarker); handled {
		if rc == 0 && adopting {
			rc = verifyAdoption("router", lc.adminAddr, routerHealthPath)
		}
		return rc
	}
	logPath := gatewayLogPath()
	logOffset := fileSize(logPath)
	adminPort := portOf(lc.adminAddr)
	args := []string{"gateway", "start", "--foreground"}
	if adminPort != "" {
		args = append(args, "--port", adminPort)
	}
	pid, err := spawnDetached(logPath, args, map[string]string{"BASETEN_SWITCH_CONFIG_PATH": lc.path})
	if err != nil {
		fmt.Fprintf(os.Stderr, "router: %v\n", err)
		return 1
	}
	if err := waitOurs(lc.adminAddr, routerHealthPath, routerHealthMarker, readyTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "router: did not become healthy: %v (log: %s)\n", err, logPath)
		printLogWarnings(logPath, logOffset)
		return 1
	}
	if p := pidfile.ReadFromSafe(gatewayPidfilePath()); p != 0 {
		pid = p
	}
	fmt.Fprintf(os.Stderr, "router: up (pid %d) admin %s (log: %s)\n", pid, lc.adminAddr, logPath)
	printLogWarnings(logPath, logOffset)
	if adopting {
		return verifyAdoption("router", lc.adminAddr, routerHealthPath)
	}
	return 0
}

// upDoor starts the front door unless every configured door port is
// already answering /doorz as ours with this binary's version; a
// healthy door reporting a different version is restarted onto it
// (version adoption).
func upDoor(lc *lifecycleConfig) int {
	adopting := false
	allOurs := true
	foreign := []string{}
	for _, sp := range lc.doorSpecs {
		st := probePort(sp.ListenAddr, doorHealthPath, doorHealthMarker)
		if st != portOurs {
			allOurs = false
		}
		if st == portForeign {
			foreign = append(foreign, sp.ListenAddr)
		}
	}
	if allOurs {
		switch adoptComponent("door", launchd.DoorLabel, doorPidfilePath(),
			doorAddrs(lc.doorSpecs), doorHealthPath, doorHealthMarker) {
		case adoptCurrent:
			fmt.Fprintf(os.Stderr, "door: already up on %s; leaving it alone\n", doorAddrList(lc.doorSpecs))
			return 0
		case adoptSkipped:
			return 0
		case adoptFailed:
			return 1
		}
		adopting = true
		// Stopped for adoption; fall through to the normal start path.
	}
	if len(foreign) > 0 {
		for _, addr := range foreign {
			fmt.Fprintf(os.Stderr, "door: a foreign process (%s) owns %s; not starting. Free the port or fix door.ports in %s.\n",
				portOwner(addr), addr, lc.path)
		}
		return 1
	}
	if rc, handled := upSupervised("door", launchd.DoorLabel, doorAddrs(lc.doorSpecs), doorHealthPath, doorHealthMarker); handled {
		if rc == 0 && adopting {
			rc = verifyAdoption("door", lc.doorSpecs[0].ListenAddr, doorHealthPath)
		}
		return rc
	}
	if state, pid := classifyPidfile(doorPidfilePath()); state == pidfileAlive {
		fmt.Fprintf(os.Stderr, "door: process alive (pid %d per %s) but not all ports answer %s; investigate (log: %s)\n",
			pid, doorPidfilePath(), doorHealthPath, doorLogPath())
		return 1
	}
	logPath := doorLogPath()
	if _, err := spawnDetached(logPath, []string{"door"}, map[string]string{"BASETEN_SWITCH_CONFIG_PATH": lc.path}); err != nil {
		fmt.Fprintf(os.Stderr, "door: %v\n", err)
		return 1
	}
	for _, sp := range lc.doorSpecs {
		if err := waitOurs(sp.ListenAddr, doorHealthPath, doorHealthMarker, readyTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "door: did not become healthy: %v (log: %s)\n", err, logPath)
			return 1
		}
	}
	pid := pidfile.ReadFromSafe(doorPidfilePath())
	fmt.Fprintf(os.Stderr, "door: up (pid %d) on %s (log: %s)\n", pid, doorAddrList(lc.doorSpecs), logPath)
	if adopting {
		return verifyAdoption("door", lc.doorSpecs[0].ListenAddr, doorHealthPath)
	}
	return 0
}

func doorAddrs(specs []door.Config) []string {
	out := make([]string, 0, len(specs))
	for _, sp := range specs {
		out = append(out, sp.ListenAddr)
	}
	return out
}

func doorAddrList(specs []door.Config) string {
	return strings.Join(doorAddrs(specs), ", ")
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// printLogWarnings surfaces preflight warning lines the router wrote
// during this boot (from offset onward) so `up` is loud about
// unresolved placeholders and missing credentials.
func printLogWarnings(logPath string, offset int64) {
	f, err := os.Open(logPath)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	shown := 0
	for sc.Scan() && shown < 20 {
		line := sc.Text()
		if strings.Contains(strings.ToLower(line), "warning") {
			fmt.Fprintf(os.Stderr, "  %s\n", line)
			shown++
		}
	}
}

// --- down -----------------------------------------------------------------

func cmdDown(args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "unknown flag for down: %s (usage: baseten-switch down)\n", args[0])
		return 2
	}
	lc, err := resolveLifecycle()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rc := 0
	// Door then router (the lifecycle contract, `down` semantics).
	// downComponent picks launchctl bootout for launchd-supervised
	// components and the pidfile SIGTERM path otherwise.
	if downComponent("door", launchd.DoorLabel, doorPidfilePath(), doorAddrs(lc.doorSpecs), doorHealthPath, doorHealthMarker) != 0 {
		rc = 1
	}
	if downComponent("router", launchd.RouterLabel, gatewayPidfilePath(), []string{lc.adminAddr}, routerHealthPath, routerHealthMarker) != 0 {
		rc = 1
	}
	// Menubar last (darwin only; silent no-op when not running) so the
	// app can render the components going down until the end. The app
	// is optional UX: a quit that fails (a hung instance ignoring TERM)
	// must not fail a down whose servers all stopped, and must never
	// abort the bring-up half of restart with the servers already down.
	if downMenubar() != 0 {
		fmt.Fprintln(os.Stderr, "menubar: quit failed; continuing (the app is optional and does not gate down)")
	}
	return rc
}

// stopComponent stops one managed process via its pidfile (SIGTERM,
// escalate to SIGKILL), regardless of who started it. When the pidfile
// is stale or absent but our health endpoint still answers on a
// component port, it refuses with a clear message rather than guessing
// which process to kill.
func stopComponent(name, pf string, healthAddrs []string, healthPath, marker string) int {
	switch state, pid := classifyPidfile(pf); state {
	case pidfileAlive:
		if err := terminateGateway(pid); err != nil {
			fmt.Fprintf(os.Stderr, "%s: stop failed: %v\n", name, err)
			return 1
		}
		os.Remove(pf)
		fmt.Fprintf(os.Stderr, "%s: stopped (pid %d)\n", name, pid)
		return 0
	case pidfileCorrupt:
		os.Remove(pf)
		fmt.Fprintf(os.Stderr, "%s: pidfile %s was corrupt; removed\n", name, pf)
	case pidfileDead:
		os.Remove(pf)
		fmt.Fprintf(os.Stderr, "%s: pid %d not running; removed stale pidfile\n", name, pid)
	}
	for _, addr := range healthAddrs {
		if probePort(addr, healthPath, marker) == portOurs {
			fmt.Fprintf(os.Stderr,
				"%s: still answers on %s but there is no live pidfile at %s; refusing to guess which process to kill (owner: %s). Stop that process yourself, or restore its pidfile and rerun down.\n",
				name, addr, pf, portOwner(addr))
			return 1
		}
	}
	fmt.Fprintf(os.Stderr, "%s: not running\n", name)
	return 0
}

// --- restart ----------------------------------------------------------------

func cmdRestart(args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "unknown flag for restart: %s (usage: baseten-switch restart)\n", args[0])
		return 2
	}
	if rc := cmdDown(nil); rc != 0 {
		fmt.Fprintln(os.Stderr, "restart aborted: down did not complete cleanly")
		return rc
	}
	return cmdUp(nil)
}

// --- status ---------------------------------------------------------------

func cmdStatus(args []string) int {
	// Normalize -v to --verbose so the shared simpleFlagSet (which only
	// strips the double-dash prefix) accepts the short form.
	for i, a := range args {
		if a == "-v" {
			args[i] = "--verbose"
		}
	}
	fs := newSimpleFlagSet()
	fs.bool("verbose", "", false, "print full detail (labels, paths, admin addr, config path, menubar bundle path, logs)")
	if err := fs.parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "usage: baseten-switch status [--verbose|-v]")
		return 2
	}
	lc, err := resolveLifecycle()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return printStatus(lc, os.Stdout, fs.lookupBool("verbose"))
}

// printStatus renders the aggregate summary (the lifecycle contract,
// `status`) and returns 0 only when every managed component is up:
// the router and the door (when configured). Foreign port ownership is called out and counts
// as not-up. The Menubar row (darwin only) is informational and never
// affects the exit code.
//
// The default (compact) output is one line per component plus auth and
// the per-client switch rows: name, up/DOWN, pid, running version, the
// door tripped state, the menubar staleness/DOWN/not-
// installed states with fix hints, the Auth line, and any "restart to
// adopt" skew lines. Dropped from the pre-verbose output: launchd
// labels, binary paths, the admin addr, the config path, the menubar
// bundle path, and the Logs block. verbose=true restores every dropped
// fact so nothing is only-in-verbose.
func printStatus(lc *lifecycleConfig, out io.Writer, verbose bool) int {
	down := 0

	// Router.
	routerState := probePort(lc.adminAddr, routerHealthPath, routerHealthMarker)
	switch routerState {
	case portOurs:
		pid := pidfile.ReadFromSafe(gatewayPidfilePath())
		if verbose {
			fmt.Fprintf(out, "Router:  up (pid %d, %s) admin %s  config %s\n",
				pid, supervisionText(launchd.RouterLabel), lc.adminAddr, lc.path)
			if exe := processExePath(pid); exe != "" {
				fmt.Fprintf(out, "         %s\n", exe)
			}
		} else {
			fmt.Fprintf(out, "Router:  up (pid %d)\n", pid)
		}
		// A failed version fetch is transient (the health probe just
		// answered), so it is not rendered as skew.
		if rv, err := runningVersionAt(lc.adminAddr, routerHealthPath); err == nil {
			if skew := versionSkewLine(rv); skew != "" {
				fmt.Fprintf(out, "         %s\n", skew)
			}
		}
		for _, row := range routerClientRows(lc.adminAddr) {
			fmt.Fprintf(out, "         %s\n", row)
		}
	case portForeign:
		if verbose {
			fmt.Fprintf(out, "Router:  FOREIGN: %s owns %s (not our gateway)  config %s\n",
				portOwner(lc.adminAddr), lc.adminAddr, lc.path)
		} else {
			fmt.Fprintf(out, "Router:  FOREIGN: %s owns %s (not our gateway)\n",
				portOwner(lc.adminAddr), lc.adminAddr)
		}
		down++
	default:
		if verbose {
			fmt.Fprintf(out, "Router:  DOWN  admin %s  config %s\n", lc.adminAddr, lc.path)
		} else {
			fmt.Fprintf(out, "Router:  DOWN\n")
		}
		if s := superviseState(launchd.RouterLabel); s.installed && !s.loaded {
			fmt.Fprintf(out, "         launchd plist installed but not loaded ('baseten-switch up' re-bootstraps %s)\n", launchd.RouterLabel)
		}
		down++
	}

	// Door.
	if len(lc.doorSpecs) == 0 {
		if verbose {
			fmt.Fprintf(out, "Door:    not configured (no door: section in %s)\n", lc.path)
		} else {
			fmt.Fprintf(out, "Door:    not configured\n")
		}
	} else {
		lines, ok := doorStatusLines(lc.doorSpecs, verbose)
		if !ok {
			down++
		}
		for i, line := range lines {
			if i == 0 {
				fmt.Fprintf(out, "Door:    %s\n", line)
			} else {
				fmt.Fprintf(out, "         %s\n", line)
			}
		}
	}

	// Menubar (darwin only; the row is omitted entirely off-darwin and
	// under BASETEN_SWITCH_MENUBAR=off). Deliberately NOT counted in `down`: the
	// exit code means "servers healthy" and scripts gate on it, while
	// the menubar app is a UI convenience, not a served component. The
	// visible DOWN row with its fix hint makes the optional component
	// discoverable; the exit code stays 0.
	menubarLines := menubarStatusLines()
	if !verbose {
		// Compact: keep the state line and any actionable skew line; drop
		// only the bundle path detail (the second element). The skew
		// line, when present, is the third element and must stay so the
		// "restart to adopt" hint is shown for the menubar just as it is
		// for router and door (the lifecycle contract: the skew
		// lines are "actionable, always shown").
		compact := []string{}
		for i, line := range menubarLines {
			if i == 1 {
				continue // bundle path: verbose-only
			}
			compact = append(compact, line)
		}
		menubarLines = compact
	}
	for i, line := range menubarLines {
		if i == 0 {
			fmt.Fprintf(out, "Menubar: %s\n", line)
		} else {
			fmt.Fprintf(out, "         %s\n", line)
		}
	}

	// Auth.
	fmt.Fprintf(out, "Auth:    %s\n", authLine(lc.adminAddr, routerState == portOurs))
	if line := authUnavailableFallbackLine(lc.adminAddr, routerState == portOurs); line != "" {
		fmt.Fprintf(out, "Fallback: %s\n", line)
	}

	// Logs (verbose only).
	if verbose {
		fmt.Fprintf(out, "Logs:    router %s\n", gatewayLogPath())
		fmt.Fprintf(out, "         door   %s\n", doorLogPath())
	}

	if down > 0 {
		return 1
	}
	return 0
}

func statErr(path string) (os.FileInfo, bool) {
	st, err := os.Stat(path)
	return st, err == nil
}

// routerClientRows fetches /v1/admin/status and renders one row per
// enabled client: name, port, effective route, switch position
// (ON = Baseten).
func routerClientRows(adminAddr string) []string {
	var payload struct {
		Clients []struct {
			Name     string `json:"name"`
			Enabled  bool   `json:"enabled"`
			BindAddr string `json:"bind_addr"`
			Route    string `json:"effective_route"`
			Shape    string `json:"protocol_shape"`
		} `json:"clients"`
	}
	if err := getJSON(adminAddr, "/v1/admin/status", &payload); err != nil {
		return []string{fmt.Sprintf("(admin status unavailable: %v)", err)}
	}
	rows := []string{}
	for _, c := range payload.Clients {
		if !c.Enabled {
			continue
		}
		sw := "OFF"
		if c.Route == "baseten" {
			sw = "ON"
		}
		port := portOf(c.BindAddr)
		if port == "" {
			port = c.BindAddr
		}
		rows = append(rows, fmt.Sprintf("%-13s %-6s %-10s switch %s", c.Name, port, c.Route, sw))
	}
	return rows
}

// doorStatusLines probes every configured door port and renders one
// line per port, plus a supervision note and (at most one) version
// skew line for the door process. ok is false when any port is not
// answering as ours. verbose controls whether the launchd label and
// the executable path detail line are included (compact status drops
// both).
func doorStatusLines(specs []door.Config, verbose bool) (lines []string, ok bool) {
	ok = true
	pid := pidfile.ReadFromSafe(doorPidfilePath())
	supText := supervisionText(launchd.DoorLabel)
	skew, skewSet := "", false
	exeShown := false
	for _, sp := range specs {
		switch probePort(sp.ListenAddr, doorHealthPath, doorHealthMarker) {
		case portOurs:
			var z struct {
				Router    string          `json:"router"`
				Tripped   bool            `json:"tripped"`
				Fallbacks json.RawMessage `json:"fallbacks"`
				Version   string          `json:"version"`
			}
			detail := ""
			if err := getJSON(sp.ListenAddr, doorHealthPath, &z); err == nil {
				tripped := "not tripped"
				if z.Tripped {
					tripped = "TRIPPED"
				}
				var rules []json.RawMessage
				_ = json.Unmarshal(z.Fallbacks, &rules)
				detail = fmt.Sprintf("%s -> %s  %s  %d fallback rules", portOf(sp.ListenAddr), z.Router, tripped, len(rules))
				if !skewSet {
					skew, skewSet = versionSkewLine(z.Version), true
				}
			} else {
				detail = fmt.Sprintf("%s -> %s", portOf(sp.ListenAddr), sp.RouterTarget)
			}
			if verbose {
				lines = append(lines, fmt.Sprintf("up (pid %d, %s)  %s", pid, supText, detail))
			} else {
				lines = append(lines, fmt.Sprintf("up (pid %d)  %s", pid, detail))
			}
			if verbose && !exeShown {
				if exe := processExePath(pid); exe != "" {
					lines = append(lines, exe)
				}
				exeShown = true
			}
		case portForeign:
			ok = false
			lines = append(lines, fmt.Sprintf("FOREIGN: %s owns %s (not our door)", portOwner(sp.ListenAddr), sp.ListenAddr))
		default:
			ok = false
			lines = append(lines, fmt.Sprintf("DOWN  %s -> %s", sp.ListenAddr, sp.RouterTarget))
		}
	}
	if skew != "" {
		lines = append(lines, skew)
	}
	if s := superviseState(launchd.DoorLabel); !ok && s.installed && !s.loaded {
		lines = append(lines, "launchd plist installed but not loaded ('baseten-switch up' re-bootstraps "+launchd.DoorLabel+")")
	}
	return lines, ok
}

// authLine summarizes the router's auth state from the admin API.
func authLine(adminAddr string, routerUp bool) string {
	if !routerUp {
		return "unknown (router not up)"
	}
	var a struct {
		SignedIn       bool   `json:"signed_in"`
		AuthType       string `json:"auth_type"`
		Profile        string `json:"profile"`
		Email          string `json:"email"`
		FallbackInUse  bool   `json:"fallback_in_use"`
		FallbackEnable bool   `json:"fallback_enabled"`
	}
	if err := getJSON(adminAddr, "/v1/admin/auth/status", &a); err != nil {
		return fmt.Sprintf("unknown (auth status unavailable: %v)", err)
	}
	switch {
	case a.SignedIn && a.AuthType == "api_key" && a.Profile != "":
		return a.Profile + " API key"
	case a.SignedIn && a.AuthType == "api_key":
		return "API key"
	case a.SignedIn && a.Profile != "":
		return a.Profile + " OAuth"
	case a.SignedIn && a.Email != "":
		return a.Email + " OAuth"
	case a.SignedIn:
		// Routers predating auth_type only supported OAuth profiles.
		return "OAuth"
	case a.FallbackInUse:
		return "API-key fallback in use"
	default:
		return "not signed in (run 'baseten auth login')"
	}
}

// authUnavailableFallbackLine surfaces otherwise successful requests that
// bypassed Baseten because the router had no usable credential. Older routers
// omit this additive status object, so an empty or unavailable value renders
// no extra line.
func authUnavailableFallbackLine(adminAddr string, routerUp bool) string {
	if !routerUp {
		return ""
	}
	var payload struct {
		AuthUnavailableFallback struct {
			Count      uint64 `json:"count"`
			LastAt     string `json:"last_at"`
			LastClient string `json:"last_client"`
			LastRoute  string `json:"last_route"`
		} `json:"auth_unavailable_fallback"`
	}
	if err := getJSON(adminAddr, "/v1/admin/status", &payload); err != nil || payload.AuthUnavailableFallback.Count == 0 {
		return ""
	}
	fallback := payload.AuthUnavailableFallback
	detail := ""
	if fallback.LastClient != "" && fallback.LastRoute != "" {
		detail = fmt.Sprintf("; last %s -> %s", fallback.LastClient, fallback.LastRoute)
	}
	if fallback.LastAt != "" {
		detail += " at " + fallback.LastAt
	}
	return fmt.Sprintf(
		"%d request(s) used native fallback because Baseten auth was unavailable since router start%s",
		fallback.Count,
		detail,
	)
}

func getJSON(addr, path string, v any) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s%s returned %d", addr, path, resp.StatusCode)
	}
	return json.Unmarshal(body, v)
}
