// supervise.go implements the launchd supervision layer of the
// lifecycle commands (the lifecycle contract, "launchd supervision:
// up --install"): `up --install` writes and bootstraps two user
// LaunchAgents, `up --uninstall` boots them out and removes them,
// `down` boots out supervised components instead of SIGTERMing them
// (SIGTERM under KeepAlive is just a restart), and plain `up`
// re-bootstraps a booted-out-but-installed job.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/launchd"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

// launchdRunner and launchAgentsDir are package vars so tests can
// substitute a fake runner and a scratch directory; no unit test ever
// touches the live launchd domain or ~/Library/LaunchAgents.
var (
	launchdRunner   launchd.Runner = launchd.ExecRunner{}
	launchAgentsDir                = func() string { return homeJoin("Library", "LaunchAgents") }
)

// launchdDisabled turns off every launchd interaction (supervision
// detection, install, uninstall). BASETEN_SWITCH_LAUNCHD=off is the operational
// toggle for environments without launchd or where supervision is
// unwanted (containers, CI, non-darwin). check.sh is one consumer: it
// sets it for the scratch up/down/status round trip so the gate never
// runs launchctl operations against the user's session.
func launchdDisabled() bool { return os.Getenv("BASETEN_SWITCH_LAUNCHD") == "off" }

func plistPathFor(label string) string {
	return filepath.Join(launchAgentsDir(), label+".plist")
}

// supervision is the per-component launchd state: installed = the
// plist file exists; loaded = launchctl reports the label in the gui
// domain. A component is supervised only when both hold.
type supervision struct {
	installed bool
	loaded    bool
}

func (s supervision) supervised() bool { return s.installed && s.loaded }

func superviseState(label string) supervision {
	if launchdDisabled() {
		return supervision{}
	}
	var s supervision
	if _, err := os.Stat(plistPathFor(label)); err == nil {
		s.installed = true
	}
	if s.installed {
		s.loaded = launchd.Loaded(launchdRunner, label)
	}
	return s
}

// supervisionText renders the per-component supervision state for the
// status output ("launchd <label>" vs "manual").
func supervisionText(label string) string {
	s := superviseState(label)
	switch {
	case s.supervised():
		return "launchd " + label
	case s.installed:
		return "launchd plist installed, not loaded"
	default:
		return "manual"
	}
}

// versionSkewLine renders the "restart to adopt" line when the
// version a running process reports differs from this binary's own
// version. Empty string means no skew. A process that reports no
// version at all predates version stamping and is skewed by
// definition.
func versionSkewLine(running string) string {
	if running == version.Version ||
		(strings.HasPrefix(version.Version, "v") && running == strings.TrimPrefix(version.Version, "v")) {
		return ""
	}
	if running == "" {
		running = "unknown"
	}
	return fmt.Sprintf("restart to adopt %s (running %s)", version.Version, running)
}

// runningVersionAt fetches the version a live component reports on its
// health endpoint. A non-nil error means the fetch itself failed
// (timeout, connection error, non-200); "" with a nil error means the
// component answered but reported no version, i.e. a pre-version-
// stamping binary. Adoption must not conflate the two: a fetch error
// against a component that just answered its health probe is
// transient, not proof of staleness.
func runningVersionAt(addr, path string) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := getJSON(addr, path, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// installPlan inspects a `launchctl print gui/<uid>` domain listing
// ahead of --install. Any label mentioning baseten-switch that is not ours
// (a brew services job etc.) is a hard refusal naming the exact
// label: never a second KeepAlive job over the same ports. Our own
// labels are returned instead of refused: --install re-renders the
// plists and re-bootstraps them (bootout then bootstrap), which is
// the upgrade path for plist content changes.
func installPlan(printOut string) (ours []string, err error) {
	for _, l := range launchd.LabelsMentioning(printOut, "baseten-switch") {
		if l == launchd.RouterLabel || l == launchd.DoorLabel {
			ours = append(ours, l)
			continue
		}
		// macOS assigns ephemeral "application.<bundle-id>.<n>.<n>"
		// labels to every normally launched GUI app; the menubar app
		// (co.baseten.switch) always carries one while open.
		// Those are not KeepAlive jobs and cannot fight over the
		// ports, so they are not supervision conflicts.
		if strings.HasPrefix(l, "application.") {
			continue
		}
		return nil, fmt.Errorf("existing launchd supervision found: %s. Refusing to install a second KeepAlive job over the same ports; stop it first (brew services jobs: 'brew services stop baseten-switch')", l)
	}
	return ours, nil
}

// routerJob and doorJob are the two embedded LaunchAgent definitions
// (the lifecycle contract): RunAtLoad + KeepAlive, BASETEN_SWITCH_CONFIG_PATH and
// PATH pinned, stdout/stderr appended to the standard component logs.
func routerJob(lc *lifecycleConfig, bin string) launchd.Job {
	return launchd.Job{
		Label:            launchd.RouterLabel,
		ProgramArguments: []string{bin, "gateway", "start", "--foreground"},
		Env:              jobEnv(lc),
		LogPath:          gatewayLogPath(),
	}
}

func doorJob(lc *lifecycleConfig, bin string) launchd.Job {
	return launchd.Job{
		Label:            launchd.DoorLabel,
		ProgramArguments: []string{bin, "door"},
		Env:              jobEnv(lc),
		LogPath:          doorLogPath(),
	}
}

func jobEnv(lc *lifecycleConfig) map[string]string {
	path := lc.path
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return map[string]string{
		"BASETEN_SWITCH_CONFIG_PATH": path,
		"PATH":                       os.Getenv("PATH"),
	}
}

// installAgents writes the plists (0644) into the LaunchAgents dir
// and bootstraps each into the gui domain. Each label is booted out
// first (a not-loaded label is a tolerated noop) so a re-install
// adopts new plist content instead of failing on the loaded job: this
// is what makes `up --install` idempotent for our own labels, the
// upgrade path. The door agent is written only when the config has a
// door: section (a KeepAlive door job with no door config would
// crash-loop).
func installAgents(lc *lifecycleConfig, bin string) error {
	if err := os.MkdirAll(launchAgentsDir(), 0o755); err != nil {
		return fmt.Errorf("create %s: %v", launchAgentsDir(), err)
	}
	jobs := []launchd.Job{routerJob(lc, bin)}
	if len(lc.doorSpecs) > 0 {
		jobs = append(jobs, doorJob(lc, bin))
	}
	for _, j := range jobs {
		pp := plistPathFor(j.Label)
		if err := os.WriteFile(pp, []byte(launchd.RenderPlist(j)), 0o644); err != nil {
			return fmt.Errorf("write %s: %v", pp, err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", pp)
		wasLoaded, err := launchd.Bootout(launchdRunner, j.Label)
		if err != nil {
			return fmt.Errorf("bootout %s: %v", j.Label, err)
		}
		if wasLoaded {
			fmt.Fprintf(os.Stderr, "booted out loaded %s (re-bootstrap adopts the new plist)\n", j.Label)
			// Bootstrapping while the old instance of the label is
			// still tearing down fails or is torn down with it.
			if err := waitLabelGone(j.Label, bootoutGoneTimeout); err != nil {
				return fmt.Errorf("bootout %s: %v", j.Label, err)
			}
		}
		if err := launchd.Bootstrap(launchdRunner, pp); err != nil {
			return fmt.Errorf("bootstrap %s: %v", j.Label, err)
		}
		fmt.Fprintf(os.Stderr, "bootstrapped %s/%s\n", launchd.GuiTarget(), j.Label)
	}
	return nil
}

// cmdUpInstall implements `baseten-switch up --install`: detect existing
// supervision first (refuse on any foreign baseten-switch label, never
// double-supervise; our own labels re-render and re-bootstrap, the
// upgrade path), stop running unmanaged components cleanly so launchd
// owns them from then on, write + bootstrap the agents, gate on
// health, print status.
func cmdUpInstall(lc *lifecycleConfig) int {
	if launchdDisabled() {
		fmt.Fprintln(os.Stderr, "up --install: launchd interaction is disabled (BASETEN_SWITCH_LAUNCHD=off)")
		return 1
	}
	// The agents pin only BASETEN_SWITCH_CONFIG_PATH and PATH; a session that
	// depends on a non-default admin address would supervise a
	// different topology than it runs. Fail loud instead.
	if env := os.Getenv("BASETEN_SWITCH_ADMIN_ADDR"); env != "" && env != gateway.DefaultAdminAddr {
		fmt.Fprintf(os.Stderr, "up --install: BASETEN_SWITCH_ADMIN_ADDR=%s is set but launchd jobs run with only BASETEN_SWITCH_CONFIG_PATH and PATH pinned; unset it (default %s) before installing\n", env, gateway.DefaultAdminAddr)
		return 1
	}
	out, err := launchdRunner.Run("print", launchd.GuiTarget())
	if err != nil {
		fmt.Fprintf(os.Stderr, "up --install: could not list the launchd domain: %v\n", err)
		return 1
	}
	ours, err := installPlan(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "up --install: %v\n", err)
		return 1
	}
	oursLoaded := map[string]bool{}
	for _, l := range ours {
		oursLoaded[l] = true
	}
	if len(ours) > 0 {
		fmt.Fprintf(os.Stderr, "up --install: launchd jobs already loaded (%s); re-rendering plists and re-bootstrapping\n", strings.Join(ours, ", "))
	}
	bin, err := launchd.StableProgramPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "up --install: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "installing LaunchAgents (binary %s, config %s)\n", bin, lc.path)

	// Stop running unmanaged components cleanly before bootstrap so
	// launchd owns them from then on. stopComponent is a reported
	// noop when nothing is running and a refusal (nonzero) when a
	// component answers health without a live pidfile. A component
	// whose label is already loaded is launchd-owned: skip the
	// SIGTERM (KeepAlive would just restart it); installAgents boots
	// the label out before re-bootstrapping.
	addrs := doorAddrs(lc.doorSpecs)
	if len(lc.doorSpecs) > 0 {
		if oursLoaded[launchd.DoorLabel] {
			fmt.Fprintf(os.Stderr, "door: launchd-owned (%s); bootout + re-bootstrap restarts it\n", launchd.DoorLabel)
		} else if stopComponent("door", doorPidfilePath(), addrs, doorHealthPath, doorHealthMarker) != 0 {
			fmt.Fprintln(os.Stderr, "up --install: aborted; could not stop the running door cleanly")
			return 1
		}
	} else {
		fmt.Fprintf(os.Stderr, "door: not configured (no door: section in %s); installing the router agent only\n", lc.path)
	}
	if oursLoaded[launchd.RouterLabel] {
		fmt.Fprintf(os.Stderr, "router: launchd-owned (%s); bootout + re-bootstrap restarts it\n", launchd.RouterLabel)
	} else if stopComponent("router", gatewayPidfilePath(), []string{lc.adminAddr}, routerHealthPath, routerHealthMarker) != 0 {
		fmt.Fprintln(os.Stderr, "up --install: aborted; could not stop the running router cleanly")
		return 1
	}

	if err := installAgents(lc, bin); err != nil {
		fmt.Fprintf(os.Stderr, "up --install: %v\n", err)
		return 1
	}

	failures := 0
	if err := waitOurs(lc.adminAddr, routerHealthPath, routerHealthMarker, readyTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "router: did not become healthy under launchd: %v (log: %s)\n", err, gatewayLogPath())
		failures++
	}
	for _, addr := range addrs {
		if err := waitOurs(addr, doorHealthPath, doorHealthMarker, readyTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "door: did not become healthy under launchd: %v (log: %s)\n", err, doorLogPath())
			failures++
		}
	}
	// The fresh-install one-liner is `up --install`, so it drives the
	// menubar app to running-and-current exactly like plain up.
	failures += upMenubar()

	fmt.Fprintln(os.Stderr, "")
	rc := printStatus(lc, os.Stdout, false)
	if failures > 0 && rc == 0 {
		rc = 1
	}
	return rc
}

// cmdUpUninstall implements `baseten-switch up --uninstall`: boot both
// labels out (a not-loaded label is tolerated), remove the plists,
// and report, leaving a manually startable system. Idempotent.
func cmdUpUninstall() int {
	if launchdDisabled() {
		fmt.Fprintln(os.Stderr, "up --uninstall: launchd interaction is disabled (BASETEN_SWITCH_LAUNCHD=off)")
		return 1
	}
	rc := 0
	anything := false
	for _, label := range []string{launchd.DoorLabel, launchd.RouterLabel} {
		wasLoaded, err := launchd.Bootout(launchdRunner, label)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "%s: bootout failed: %v\n", label, err)
			rc = 1
		case wasLoaded:
			fmt.Fprintf(os.Stderr, "%s: booted out\n", label)
			anything = true
		}
		pp := plistPathFor(label)
		switch err := os.Remove(pp); {
		case err == nil:
			fmt.Fprintf(os.Stderr, "%s: removed %s\n", label, pp)
			anything = true
		case !os.IsNotExist(err):
			fmt.Fprintf(os.Stderr, "%s: remove %s: %v\n", label, pp, err)
			rc = 1
		}
	}
	if !anything && rc == 0 {
		fmt.Fprintln(os.Stderr, "launchd supervision was not installed; nothing to do")
	} else if rc == 0 {
		fmt.Fprintln(os.Stderr, "launchd supervision removed; 'baseten-switch up' now starts unmanaged processes again")
	}
	return rc
}

// upSupervised lets plain `up` handle an installed LaunchAgent: a
// booted-out job is re-bootstrapped; a loaded job is left to launchd
// (KeepAlive restarts crashes) and only waited on. handled=false
// means no plist is installed and the caller should spawn the
// component itself.
func upSupervised(name, label string, healthAddrs []string, healthPath, marker string) (rc int, handled bool) {
	s := superviseState(label)
	if !s.installed {
		return 0, false
	}
	if s.loaded {
		fmt.Fprintf(os.Stderr, "%s: launchd job %s is loaded; waiting for launchd to bring it up\n", name, label)
	} else {
		pp := plistPathFor(label)
		fmt.Fprintf(os.Stderr, "%s: launchd plist installed; re-bootstrapping %s\n", name, pp)
		if err := launchd.Bootstrap(launchdRunner, pp); err != nil {
			fmt.Fprintf(os.Stderr, "%s: bootstrap failed: %v\n", name, err)
			return 1, true
		}
	}
	for _, addr := range healthAddrs {
		if err := waitOurs(addr, healthPath, marker, readyTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "%s: did not become healthy under launchd: %v (label %s)\n", name, err, label)
			// A label that is gone again will never be started by
			// launchd; "did not become healthy" alone would misread
			// as a slow boot instead of a missing bootstrap.
			if !launchd.Loaded(launchdRunner, label) {
				fmt.Fprintf(os.Stderr, "%s: label %s is no longer loaded; launchd will not start it. Run 'baseten-switch up' to re-bootstrap.\n", name, label)
			}
			return 1, true
		}
	}
	fmt.Fprintf(os.Stderr, "%s: up under launchd (%s)\n", name, label)
	return 0, true
}

// downComponent stops one component the right way for its supervision
// state: launchctl bootout for a supervised component (SIGTERM under
// KeepAlive is just a restart, and down says so), the pidfile SIGTERM
// path otherwise.
func downComponent(name, label, pf string, healthAddrs []string, healthPath, marker string) int {
	s := superviseState(label)
	if !s.supervised() {
		if s.installed {
			fmt.Fprintf(os.Stderr, "%s: launchd plist installed but %s not loaded; stopping directly\n", name, label)
		}
		return stopComponent(name, pf, healthAddrs, healthPath, marker)
	}
	fmt.Fprintf(os.Stderr, "%s: launchd-supervised (%s); SIGTERM would just restart it, using launchctl bootout\n", name, label)
	if _, err := launchd.Bootout(launchdRunner, label); err != nil {
		fmt.Fprintf(os.Stderr, "%s: bootout failed: %v\n", name, err)
		return 1
	}
	// bootout returns before the label leaves the domain. down must
	// not report success until the label is gone: a follow-up up (the
	// restart path) that runs `launchctl print` against the lingering
	// label reads it as loaded and skips the re-bootstrap, leaving the
	// component booted out with nothing to start it.
	if err := waitLabelGone(label, bootoutGoneTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v; a follow-up 'baseten-switch up' would see the label as loaded and skip the re-bootstrap. Retry 'baseten-switch down'.\n", name, err)
		return 1
	}
	for _, addr := range healthAddrs {
		if err := waitGone(addr, healthPath, marker, readyTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "%s: still answering after bootout: %v\n", name, err)
			return 1
		}
	}
	// The process removes its own pidfile on clean exit; tidy up a
	// stale one if the exit raced.
	if state, _ := classifyPidfile(pf); state == pidfileDead || state == pidfileCorrupt {
		os.Remove(pf)
	}
	fmt.Fprintf(os.Stderr, "%s: booted out of launchd (plist stays installed; 'baseten-switch up' re-bootstraps, 'baseten-switch up --uninstall' removes)\n", name)
	return 0
}

// bootoutGoneTimeout bounds waitLabelGone. Package var so tests can
// shorten it.
var bootoutGoneTimeout = 10 * time.Second

// waitLabelGone polls the gui domain until launchd no longer lists
// label. launchctl bootout is asynchronous: it returns while the label
// is still registered and launchd is signaling and reaping the
// process, and the component's health port closes before the label is
// unregistered, so observing the port (waitGone) does not observe
// bootout completion. Every bootout that a bootstrap may follow must
// wait on this or the bootstrap is skipped or fails.
func waitLabelGone(label string, timeout time.Duration) error {
	if launchdDisabled() {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		if !launchd.Loaded(launchdRunner, label) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("launchd still lists %s/%s after %s; bootout did not complete", launchd.GuiTarget(), label, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitGone polls until addr stops answering our health endpoint.
func waitGone(addr, path, marker string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if probePortTimeout(addr, path, marker, 500*time.Millisecond) != portOurs {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("http://%s%s still answers as ours after %s", addr, path, timeout)
}
