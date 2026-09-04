// doctor.go implements `baseten-switch doctor`, the read-only chain
// diagnostic. It walks the request path in order (binary, config,
// auth, router, door, claude wiring, codex wiring, supervision, e2e
// probe, telemetry), reports one ok|warn|fail|skip finding per check,
// and names the FIRST failed link with a concrete fix, so a broken
// setup never needs a manual link-by-link back-and-forth.
//
// doctor NEVER mutates config, journal/terminal recovery state, router state,
// or installed settings. It may create or secure coordination metadata while
// acquiring the mutation status lock. Exit codes: 0 = no check failed (warns
// allowed), 1 = at least one FAIL, 2 = usage error. The one exception is the
// opt-in `--fix` repair loop (doctor_fix.go), which mutates only by running
// existing baseten-switch verbs as confirmed child processes; runDoctor
// itself stays read-only with respect to managed state in both modes.
//
// Test override points (the same seams the rest of the CLI uses):
// BASETEN_SWITCH_CONFIG_PATH, BASETEN_SWITCH_ADMIN_ADDR, BASETEN_SWITCH_GATEWAY_PIDFILE,
// BASETEN_SWITCH_DOOR_PIDFILE, BASETEN_SWITCH_CLAUDE_SETTINGS, BASETEN_SWITCH_CODEX_HOME,
// BASETEN_SWITCH_BACKUP_DIR, BASETEN_SWITCH_AUTH_FILE + BASETEN_SWITCH_AUTH_NO_KEYRING, BASETEN_SWITCH_ENV_FILE,
// BASETEN_SWITCH_LAUNCHD=off.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/door"
	"github.com/basetenlabs/baseten-switch/gateway/internal/launchd"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/route"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

const (
	docOK   = "ok"
	docWarn = "warn"
	docFail = "fail"
	docSkip = "skip"
)

// doctorInspectRoutingMutationStatus is a seam for exercising the doctor's
// rendering and repair policy independently from journal and router I/O.
var doctorInspectRoutingMutationStatus = inspectRoutingMutationStatus

type doctorCheck struct {
	Section string `json:"section"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Finding string `json:"finding"`
	Fix     string `json:"fix,omitempty"`

	// fixArgv is the self-exec argv `doctor --fix` may run for this
	// check. Unexported: the --json shape is pinned and must stay
	// byte-identical.
	fixArgv []string
}

// addCheck records one check result. fixArgv marks the fix as
// automatable by `doctor --fix` via self-exec; the fix loop consults
// it only on FAILs, so argv on a warn is dormant by design (warns are
// never auto-fixed).
type addCheck func(section, name, status, finding, fix string, fixArgv ...string)

type doctorReport struct {
	Checks       []doctorCheck `json:"checks"`
	FirstFailure string        `json:"first_failure,omitempty"`
	ExitCode     int           `json:"exit_code"`
}

type doctorOpts struct {
	probe      bool
	yes        bool
	timeoutSec int
}

func cmdDoctor(args []string) int {
	fs := newSimpleFlagSet()
	fs.bool("json", "", false, "emit the check array as JSON")
	fs.bool("probe", "", false, "fire a 1-token real request through each healthy door port (incurs cost)")
	fs.bool("fix", "", false, "interactive repair: confirm-and-apply automatable fixes, re-running the chain after each")
	fs.bool("yes", "", false, "auto-confirm every prompt (--probe consent and --fix prompts; for scripts/CI)")
	fs.bool("verbose", "", false, "print every passing check instead of compressing all-ok sections")
	fs.int("timeout", 5, "per-probe HTTP timeout in seconds")
	if err := fs.parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "usage: baseten-switch doctor [--json] [--probe] [--verbose] [--fix] [--yes]")
		return 2
	}
	o := doctorOpts{
		probe:      fs.lookupBool("probe"),
		yes:        fs.lookupBool("yes"),
		timeoutSec: fs.lookupInt("timeout"),
	}
	if fs.lookupBool("fix") {
		// --fix renders reports incrementally and prompts; a machine
		// consumer must use plain --json and apply fixes itself.
		if fs.lookupBool("json") {
			fmt.Fprintln(os.Stderr, "doctor: --fix cannot be combined with --json")
			return 2
		}
		// Recursion guard: fix verbs run as children with this set, so
		// a verb that itself invokes doctor --fix cannot loop forever.
		if os.Getenv(doctorFixGuardEnv) != "" {
			fmt.Fprintf(os.Stderr, "doctor: refusing --fix with %s set (already inside a doctor --fix run)\n", doctorFixGuardEnv)
			return 2
		}
		if !o.yes && !doctorStdinIsTTY() {
			fmt.Fprintln(os.Stderr, "doctor: --fix needs an interactive terminal to confirm each fix; pass --yes to auto-confirm (for scripts/CI)")
			return 2
		}
		return runDoctorFix(o, fs.lookupBool("verbose"))
	}
	rep := runDoctor(o)
	if fs.lookupBool("json") {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
	} else {
		printDoctorReport(os.Stdout, rep, fs.lookupBool("verbose"))
	}
	return rep.ExitCode
}

// runDoctor executes every check in request-path order and returns the full
// report. State gathering does not mutate managed state; mutation status may
// acquire its coordination lock.
func runDoctor(o doctorOpts) doctorReport {
	rep := doctorReport{}
	var add addCheck = func(section, name, status, finding, fix string, fixArgv ...string) {
		rep.Checks = append(rep.Checks, doctorCheck{Section: section, Name: name, Status: status, Finding: finding, Fix: fix, fixArgv: fixArgv})
		if status == docFail && rep.FirstFailure == "" {
			rep.FirstFailure = section + "/" + name
		}
	}

	adminAddr := envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)
	envFilePath := envDefault("BASETEN_SWITCH_ENV_FILE", config.EnvFilePath())
	envFile, _ := config.LoadEnvFile(envFilePath)

	// Resolve and load the config up front: several later sections need
	// it, but its checks are reported in chain position (section 2).
	cfgPath, cfgSource := doctorConfigPath()
	f, cfgErr := config.Load(cfgPath)
	if cfgErr != nil {
		f = nil
	}

	// Door specs mirror what the door process derives from the same
	// file; skipped entries (router_addr matching no enabled client)
	// are a config finding, collected via Logf.
	var doorSpecs []door.Config
	var doorSpecNotices []string
	if f != nil && f.Door != nil && len(f.Door.Ports) > 0 {
		specs, err := door.SpecsFromConfig(f, door.SpecsOptions{Logf: func(format string, args ...any) {
			doorSpecNotices = append(doorSpecNotices, fmt.Sprintf(format, args...))
		}})
		if err != nil {
			doorSpecNotices = append(doorSpecNotices, err.Error())
		} else {
			doorSpecs = specs
		}
	}

	// --- 1. binary --------------------------------------------------------
	add("binary", "version", docOK, "baseten-switch "+version.Version, "")
	doctorBinarySkew(add, adminAddr, doorSpecs)
	doctorMenubarBinaryCheck(add, adminAddr, doorSpecs)

	// --- 2. config --------------------------------------------------------
	add("config", "path", docOK, fmt.Sprintf("%s (%s)", cfgPath, cfgSource), "")
	unresolved := doctorConfigChecks(add, cfgPath, cfgErr, f, envFile, envFilePath, doorSpecNotices, len(doorSpecs))

	// The router probe runs before section 3: the auth health check
	// reads the running router's refresh outcomes, which the store-only
	// signin check cannot see.
	routerState := probePort(adminAddr, routerHealthPath, routerHealthMarker)

	// --- 3. auth ----------------------------------------------------------
	routerAuth, routerAuthKnown := doctorRouterAuthSnapshot(adminAddr, routerState == portOurs)
	storeAuth := doctorAuthCheck(add, f, unresolved, envFile, envFilePath, routerAuth, routerAuthKnown)
	doctorAuthHealthCheck(add, routerState == portOurs, routerAuth, routerAuthKnown, storeAuth)
	doctorBasetenCLICheck(add)

	// --- 4. router --------------------------------------------------------
	// Inspect mutation recovery before attaching any automatic lifecycle
	// repair. Starting the router can make the config currently on disk
	// authoritative, so an unfinished or unreadable mutation must suppress
	// every `up` fix until recovery has been handled explicitly.
	mutationStatus, mutationStatusErr := doctorInspectRoutingMutationStatus(cfgPath)
	allowStartupFix := mutationStatusErr == nil && mutationStatus.Classification == mutationStatusNone
	st := doctorRouterChecks(add, adminAddr, routerState, allowStartupFix)
	doctorTraceRuntimeCheck(add, cfgPath, cfgErr, f, st)
	doctorMutationRecoveryCheckForStatus(add, mutationStatus, mutationStatusErr)
	doctorRouterClientChecks(add, st)
	boundAddrs := map[string]bool{}
	if st != nil {
		for _, c := range st.Clients {
			if c.Enabled && c.CurrentlyBound {
				boundAddrs[c.BindAddr] = true
			}
		}
	}

	// --- 5. door ----------------------------------------------------------
	doorUp := doctorDoorChecks(add, doorSpecs, st != nil, boundAddrs, allowStartupFix)

	// Resolve the telemetry store path once: the claude subagents traffic
	// check (section 6) and the telemetry section (9) both use it.
	telPath := config.DefaultTelemetryDir()
	if f != nil && f.Global.TelemetryDir != "" {
		telPath = config.ExpandPath(f.Global.TelemetryDir)
	}

	// --- 6. claude wiring -------------------------------------------------
	doctorClaudeChecks(add, f, telPath)

	// --- 6b. codex wiring -------------------------------------------------
	doctorCodexChecks(add, f, envFile, envFilePath)

	// --- 7. supervision ---------------------------------------------------
	doctorSupervisionChecks(add, routerState, doorSpecs, doorUp, allowStartupFix)

	// --- 8. e2e -----------------------------------------------------------
	doctorE2EChecks(add, o, doorSpecs, doorUp, f, telPath)

	// --- 9. telemetry -----------------------------------------------------
	doctorTelemetryChecks(add, f, telPath)

	if rep.FirstFailure != "" {
		rep.ExitCode = 1
	}
	return rep
}

func doctorTraceRuntimeCheck(add addCheck, cfgPath string, cfgErr error, file *config.File, status *doctorAdminStatus) {
	if cfgErr != nil || file == nil {
		add("router", "trace_capture", docSkip, "config did not load", "")
		return
	}
	policy, err := config.ResolveTraceCapture(file)
	if err != nil {
		add("router", "trace_capture", docSkip, "trace capture policy is invalid", "")
		return
	}
	if status == nil {
		add("router", "trace_capture", docSkip, "router status unavailable", "")
		return
	}
	if status.ConfigPath == "" {
		add("router", "trace_capture", docSkip, "router does not expose active config identity", "")
		return
	}
	if canonicalPath(status.ConfigPath) != canonicalPath(cfgPath) {
		add("router", "trace_capture", docFail, "running router uses a different config", "restart the router with the selected config")
		return
	}
	runtime := status.TraceCapture
	if policy.Enabled && (!runtime.Enabled || runtime.State != "enabled") {
		finding := fmt.Sprintf("trace capture is configured but runtime state is %s", displayTraceStatusValue(runtime.State))
		if runtime.LastError != nil && *runtime.LastError != "" {
			finding += " (" + *runtime.LastError + ")"
		}
		add("router", "trace_capture", docFail, finding, "baseten-switch traces status")
		return
	}
	if !policy.Enabled && runtime.Enabled {
		add("router", "trace_capture", docFail, "trace capture is active although the selected config disables it", "reload or restart the router")
		return
	}
	if policy.Enabled {
		add("router", "trace_capture", docWarn, "high-sensitivity trace capture is active", "baseten-switch traces disable")
		return
	}
	add("router", "trace_capture", docOK, "trace capture is disabled", "")
}

// doctorConfigPath resolves the config path exactly like
// resolveConfigPath (BASETEN_SWITCH_CONFIG_PATH, else the sticky recorded path,
// else the default) and names which source won.
func doctorConfigPath() (path, source string) {
	if p := os.Getenv("BASETEN_SWITCH_CONFIG_PATH"); p != "" {
		return p, "BASETEN_SWITCH_CONFIG_PATH"
	}
	if p, _ := stickyConfigPath("", pidfile.ConfigStatePath(pidfile.Path())); p != "" {
		return p, "sticky path from the last gateway run"
	}
	return config.DefaultPath(), "default path"
}

// doctorBinarySkew compares the versions reported by the running
// router and door against this binary (reusing the status command's
// skew detection). Skew is a warn: the processes keep serving, but
// they are not the code the user just installed.
func doctorBinarySkew(add addCheck, adminAddr string, doorSpecs []door.Config) {
	var skews, running []string
	if probePort(adminAddr, routerHealthPath, routerHealthMarker) == portOurs {
		v, _ := runningVersionAt(adminAddr, routerHealthPath)
		running = append(running, fmt.Sprintf("router pid %d %s", pidfile.ReadFromSafe(gatewayPidfilePath()), orDash(v)))
		if s := versionSkewLine(v); s != "" {
			skews = append(skews, "router: "+s)
		}
	}
	for _, sp := range doorSpecs {
		if probePort(sp.ListenAddr, doorHealthPath, doorHealthMarker) != portOurs {
			continue
		}
		v, _ := runningVersionAt(sp.ListenAddr, doorHealthPath)
		running = append(running, fmt.Sprintf("door pid %d %s", pidfile.ReadFromSafe(doorPidfilePath()), orDash(v)))
		if s := versionSkewLine(v); s != "" {
			skews = append(skews, "door: "+s)
		}
		break // one door process serves every port; one comparison suffices
	}
	switch {
	case len(running) == 0:
		add("binary", "skew", docSkip, "no running router/door to compare versions against", "")
	case len(skews) > 0:
		add("binary", "skew", docWarn, strings.Join(skews, "; "), "baseten-switch restart")
	default:
		add("binary", "skew", docOK, "running processes match this binary ("+strings.Join(running, ", ")+")", "")
	}
}

// doctorMenubarBinaryCheck resolves the binary the menubar app would
// launch (its documented lookup order: $BASETEN_SWITCH_GATEWAY_BIN,
// ~/.local/bin/baseten-switch, brew opt paths) and warns when its version
// differs from the running components. This catches "the next Open
// Dashboard or Start click starts a stale component" before the click
// against the running router and door. ok when the resolved version
// matches or when nothing is running; WARN on mismatch; skip when no
// binary resolves or --version fails.
func doctorMenubarBinaryCheck(add addCheck, adminAddr string, doorSpecs []door.Config) {
	resolved := resolveMenubarBinary()
	if resolved == "" {
		add("binary", "menubar", docSkip, "no baseten-switch binary found in the menubar lookup order ($BASETEN_SWITCH_GATEWAY_BIN, ~/.local/bin, brew opt); the menubar cannot start components", "")
		return
	}
	menubarVer := menubarBinaryVersion(resolved)
	if menubarVer == "" {
		add("binary", "menubar", docWarn,
			fmt.Sprintf("resolved the menubar binary at %s but '%s --version' did not print a version; the binary may be broken or too old for --version", resolved, resolved),
			"reinstall baseten-switch with brew reinstall basetenlabs/baseten/baseten-switch")
		return
	}

	// Collect the running components' versions to compare against.
	// Matches doctorBinarySkew's set: router and door.
	var running []string
	if probePort(adminAddr, routerHealthPath, routerHealthMarker) == portOurs {
		if v, _ := runningVersionAt(adminAddr, routerHealthPath); v != "" {
			running = append(running, v)
		}
	}
	for _, sp := range doorSpecs {
		if probePort(sp.ListenAddr, doorHealthPath, doorHealthMarker) != portOurs {
			continue
		}
		if v, _ := runningVersionAt(sp.ListenAddr, doorHealthPath); v != "" {
			running = append(running, v)
		}
		break
	}
	if len(running) == 0 {
		add("binary", "menubar", docOK, fmt.Sprintf("menubar would launch %s from %s (nothing running to compare against)", menubarVer, resolved), "")
		return
	}
	// ok when the menubar version matches every running component.
	mismatch := false
	for _, v := range running {
		if v != menubarVer {
			mismatch = true
			break
		}
	}
	if !mismatch {
		add("binary", "menubar", docOK, fmt.Sprintf("menubar would launch %s from %s, matching the running components", menubarVer, resolved), "")
		return
	}
	add("binary", "menubar", docWarn,
		fmt.Sprintf("menubar would launch %s from %s but the running components are %s; the next Open Dashboard or Start click starts a stale component", menubarVer, resolved, strings.Join(running, ", ")),
		"brew reinstall basetenlabs/baseten/baseten-switch")
}

// doctorConfigChecks runs the config-section checks and returns the
// unresolved placeholder names (config placeholders minus the process
// environment minus the gateway env file), which the auth section
// cross-references.
func doctorConfigChecks(add addCheck, cfgPath string, cfgErr error, f *config.File, envFile map[string]string, envFilePath string, doorSpecNotices []string, doorSpecCount int) []string {
	if cfgErr != nil {
		if errors.Is(cfgErr, os.ErrNotExist) {
			add("config", "load", docFail, "no gateway config at "+cfgPath,
				"baseten-switch config init   (or set BASETEN_SWITCH_CONFIG_PATH to an existing config; reference: config/gateway.example.yaml)")
		} else {
			add("config", "load", docFail, fmt.Sprintf("config does not load: %v", cfgErr),
				"repair the file (reference: config/schema.md and config/gateway.example.yaml)")
		}
		for _, name := range []string{"clients", "door", "placeholders"} {
			add("config", name, docSkip, "config did not load", "")
		}
		return nil
	}
	add("config", "load", docOK, "loaded "+cfgPath, "")
	if tracePolicy, traceErr := config.ResolveTraceCapture(f); traceErr != nil {
		add("config", "trace_capture", docFail, traceErr.Error(), "fix global.trace_capture in "+cfgPath)
	} else if tracePolicy.Enabled {
		add("config", "trace_capture", docWarn,
			"high-sensitivity local trace capture is enabled; "+traceSensitiveWarning,
			"baseten-switch traces disable")
	}
	if paths, err := tracecapture.ResolveRuntimePaths(config.ExpandPath(cfgPath)); err == nil {
		if exports, inspectErr := tracecapture.InspectRuntimeExports(paths); inspectErr != nil {
			add("config", "trace_exports", docWarn, "retained trace packages could not be inspected", "baseten-switch traces status")
		} else if exports.PackageCount > 0 || exports.QuarantineCount > 0 {
			add("config", "trace_exports", docWarn,
				fmt.Sprintf("retained trace data: %d package(s), %d package bytes, %d quarantine item(s), %d quarantine bytes", exports.PackageCount, exports.PackageBytes, exports.QuarantineCount, exports.QuarantineBytes),
				"baseten-switch traces purge --packages --yes")
		} else {
			add("config", "trace_exports", docOK, "no retained trace packages or quarantine items", "")
		}
	}

	enabled := 0
	for _, c := range f.Clients {
		if c.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		add("config", "clients", docFail, "no enabled clients in "+cfgPath,
			"set enabled: true on a client in "+cfgPath+" (reference: config/gateway.example.yaml)")
	} else {
		add("config", "clients", docOK, fmt.Sprintf("%d enabled client(s)", enabled), "")
	}

	switch {
	case f.Door == nil || len(f.Door.Ports) == 0:
		add("config", "door", docFail, "no door: section (or no door.ports) in "+cfgPath,
			"add a door: section mapping a bind_addr to each client listener (reference: config/gateway.example.yaml)")
	case doorSpecCount == 0:
		add("config", "door", docFail, "door.ports has no usable entry: "+strings.Join(doorSpecNotices, "; "),
			"fix door.ports in "+cfgPath+" so each router_addr matches an enabled client's bind_addr")
	case len(doorSpecNotices) > 0:
		add("config", "door", docFail, strings.Join(doorSpecNotices, "; "),
			"fix door.ports in "+cfgPath+" so each router_addr matches an enabled client's bind_addr")
	default:
		add("config", "door", docOK, fmt.Sprintf("door section present (%d port(s))", doorSpecCount), "")
	}

	// Placeholder resolution mirrors the gateway preflight, plus the
	// env file the gateway loads into its environment before that
	// check runs (loadDotEnv).
	var unresolved []string
	for _, name := range gateway.UnresolvedPlaceholders(f) {
		if envFile[name] == "" {
			unresolved = append(unresolved, name)
		}
	}
	if len(unresolved) > 0 {
		add("config", "placeholders", docWarn,
			fmt.Sprintf("unresolved ${VAR} reference(s): %s (they expand to empty)", strings.Join(unresolved, ", ")),
			fmt.Sprintf("add %s=... to %s or export it in the gateway's environment", unresolved[0], envFilePath))
	} else {
		add("config", "placeholders", docOK, "all ${VAR} references resolve", "")
	}

	return unresolved
}

// doctorAuthCheck reports the signed-in state from the credential
// store (the same store the gateway reads). Not-signed-in is a FAIL
// only when something actually depends on a Baseten credential: a
// baseten-routed client, or an unresolved auth placeholder.
type doctorStoreAuth struct {
	profile  string
	authType string
	ready    bool
}

func doctorAuthCheck(add addCheck, f *config.File, unresolved []string, envFile map[string]string, envFilePath string, routerAuth doctorRouterAuth, routerAuthKnown bool) doctorStoreAuth {
	profile := doctorEnvValue("BASETEN_SWITCH_OAUTH_PROFILE", envFile)
	if routerAuthKnown {
		profile = routerAuth.Profile
	}
	tok, loc, err := auth.LoadDetailed(profile)
	var ak *auth.APIKeyProfileError
	switch {
	case err != nil && errors.As(err, &ak):
		if ak.Key != "" {
			add("auth", "signin", docOK, fmt.Sprintf("signed in with API key (profile %q)", ak.Profile), "")
			return doctorStoreAuth{profile: ak.Profile, authType: "API-key", ready: true}
		} else {
			add("auth", "signin", docWarn, fmt.Sprintf("profile %q uses API key auth but the key is not readable", ak.Profile),
				"check the keyring or auth.json, or run 'baseten auth login'")
		}
		return doctorStoreAuth{profile: ak.Profile, authType: "API-key"}
	case errors.Is(err, auth.ErrStoreUnreadable), errors.Is(err, auth.ErrStoreMalformed):
		add("auth", "signin", docFail, err.Error(), "repair the credential store or run 'baseten auth login'")
		return doctorStoreAuth{}
	case err != nil:
		add("auth", "signin", docWarn, fmt.Sprintf("credential store check failed: %v", err), "baseten auth login")
		return doctorStoreAuth{}
	case tok != nil:
		loadedProfile := profile
		if loc != nil && loc.Account != "" {
			loadedProfile = loc.Account
		}
		if !tok.Expiry.IsZero() && tok.Expiry.Before(time.Now()) {
			add("auth", "signin", docWarn,
				fmt.Sprintf("signed in (OAuth) but the access token expired %s; a refresh is attempted on the next request", tok.Expiry.UTC().Format(time.RFC3339)),
				"baseten-switch whoami --refresh   (verifies the refresh path; re-run 'baseten auth login' if it fails)",
				"whoami", "--refresh")
		} else {
			add("auth", "signin", docOK, "signed in (OAuth)", "")
		}
		return doctorStoreAuth{profile: loadedProfile, authType: "OAuth", ready: true}
	}
	if routerAuthKnown && routerAuth.SignedIn {
		authType := strings.TrimSpace(routerAuth.AuthType)
		if authType == "" {
			authType = "credential"
		}
		finding := fmt.Sprintf("the running router is signed in with %s auth", authType)
		if routerAuth.Profile != "" {
			finding += fmt.Sprintf(" (profile %q)", routerAuth.Profile)
		}
		add("auth", "signin", docOK, finding, "")
		return doctorStoreAuth{}
	}

	// A reachable router is authoritative for environment fallback state.
	// Launchd does not inherit later shell exports, and the Switch env file can
	// also change after the process starts. Local values are used only offline.
	if routerAuthKnown {
		if routerAuth.FallbackInUse {
			add("auth", "signin", docOK, "no profile sign-in, but the running router's enabled BASETEN_API_KEY fallback is usable", "")
			return doctorStoreAuth{}
		}
		if authVars := doctorAuthPlaceholders(f, unresolved); len(authVars) > 0 {
			add("auth", "signin", docFail,
				fmt.Sprintf("the running router is signed out and gateway.yaml references ${%s} which is unset; baseten-routed requests have no credential", strings.Join(authVars, "}, ${")),
				fmt.Sprintf("baseten auth login   (or set %s=... in %s)", authVars[0], envFilePath))
			return doctorStoreAuth{}
		}
		finding := "the running router has no usable profile credential"
		if routerAuth.FallbackEnabled {
			finding += " and API-key fallback is enabled but has no usable key loaded"
		} else {
			finding += " and API-key fallback is disabled"
		}
		if doctorEnvValue("BASETEN_API_KEY", envFile) != "" && doctorAPIKeyFallbackEnabled(envFile) {
			finding += "; the current shell's BASETEN_API_KEY fallback is not loaded by the running router"
		}
		status := docWarn
		if names := doctorBasetenRouted(f); len(names) > 0 {
			status = docFail
			finding += "; baseten-routed requests have no usable credential: " + strings.Join(names, ", ")
		}
		add("auth", "signin", status, finding,
			"baseten-switch up   (reload the running router's credential state, or run 'baseten auth login')")
		return doctorStoreAuth{}
	}

	// Not signed in. The environment fallback is usable only when both the
	// key and its explicit opt-in flag resolve, matching the router.
	apiKey := doctorEnvValue("BASETEN_API_KEY", envFile)
	apiKeyFallback := doctorAPIKeyFallbackEnabled(envFile)
	if apiKey != "" && apiKeyFallback {
		add("auth", "signin", docOK, "no profile sign-in, but the enabled BASETEN_API_KEY fallback is usable", "")
		return doctorStoreAuth{}
	}
	if apiKey != "" {
		finding := "BASETEN_API_KEY is set but ignored because BASETEN_SWITCH_API_KEY_FALLBACK is not enabled"
		status := docWarn
		if names := doctorBasetenRouted(f); len(names) > 0 {
			status = docFail
			finding += "; baseten-routed requests have no usable credential: " + strings.Join(names, ", ")
		}
		add("auth", "signin", status, finding,
			fmt.Sprintf("set BASETEN_SWITCH_API_KEY_FALLBACK=1 in %s, or run 'baseten auth login'", envFilePath))
		return doctorStoreAuth{}
	}
	bothFixes := fmt.Sprintf("baseten auth login   (or set the key in %s)", envFilePath)
	if authVars := doctorAuthPlaceholders(f, unresolved); len(authVars) > 0 {
		add("auth", "signin", docFail,
			fmt.Sprintf("not signed in (no OAuth, no API key) and gateway.yaml references ${%s} which is unset; baseten-routed requests have no credential", strings.Join(authVars, "}, ${")),
			fmt.Sprintf("baseten auth login   (or set %s=... in %s)", authVars[0], envFilePath))
		return doctorStoreAuth{}
	}
	if names := doctorBasetenRouted(f); len(names) > 0 {
		add("auth", "signin", docFail,
			"not signed in (no OAuth, no API key); these clients cannot use Baseten, and a configured native fallback may serve their requests instead: "+strings.Join(names, ", "),
			bothFixes)
		return doctorStoreAuth{}
	}
	add("auth", "signin", docWarn, "not signed in (no enabled client routes to baseten, so nothing breaks yet)", "baseten auth login")
	return doctorStoreAuth{}
}

// doctorEnvValue mirrors the gateway's process-environment-first lookup over
// the Switch env file without mutating the doctor process environment.
func doctorEnvValue(name string, envFile map[string]string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	value := envFile[name]
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		value = value[1 : len(value)-1]
	}
	return value
}

type doctorRouterAuth struct {
	SignedIn         bool   `json:"signed_in"`
	AuthType         string `json:"auth_type"`
	Profile          string `json:"profile"`
	Health           string `json:"health"`
	LastRefreshError string `json:"last_refresh_error"`
	LastRefreshErrAt string `json:"last_refresh_error_at"`
	FallbackEnabled  bool   `json:"fallback_enabled"`
	FallbackInUse    bool   `json:"fallback_in_use"`
}

// doctorRouterAuthSnapshot reads the local-only auth projection from the
// general status endpoint. It avoids /v1/admin/auth/status, whose identity
// fields may require a synchronous upstream request. When unavailable, doctor
// retains its offline environment-based checks.
func doctorRouterAuthSnapshot(adminAddr string, routerUp bool) (doctorRouterAuth, bool) {
	if !routerUp {
		return doctorRouterAuth{}, false
	}
	body, ok := doctorAdminGetOK(&http.Client{Timeout: 2 * time.Second}, adminAddr, "/v1/admin/status")
	if !ok {
		return doctorRouterAuth{}, false
	}
	var payload struct {
		Auth *doctorRouterAuth `json:"auth"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil || payload.Auth == nil {
		return doctorRouterAuth{}, false
	}
	return *payload.Auth, true
}

func doctorAPIKeyFallbackEnabled(envFile map[string]string) bool {
	switch strings.ToLower(doctorEnvValue("BASETEN_SWITCH_API_KEY_FALLBACK", envFile)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// doctorAuthHealthCheck reads the running router's credential health
// (auth.health in /v1/admin/status, derived from real refresh
// outcomes). The store-based signin check above cannot detect a dead
// refresh token: the credential is present, but the token endpoint rejects
// it. The current admin contract requires the health field.
func doctorAuthHealthCheck(add addCheck, routerUp bool, routerAuth doctorRouterAuth, routerAuthKnown bool, store doctorStoreAuth) {
	if !routerUp {
		add("auth", "health", docSkip, "router not up; credential health is derived from the running router's refresh outcomes", "")
		return
	}
	if !routerAuthKnown {
		add("auth", "health", docSkip, "admin /v1/admin/status auth projection unavailable; cannot read credential health", "")
		return
	}
	health := routerAuth.Health
	if health == "" {
		add("auth", "health", docFail,
			"router auth status omitted the required health field",
			"baseten-switch up   (restart the router onto the current binary)")
		return
	}
	// last_refresh_error embeds token-endpoint response bytes verbatim
	// (gateway noteAuthRefresh stores err.Error(), and x/oauth2 includes
	// the raw body); a hostile or misconfigured endpoint could otherwise
	// inject newlines/ANSI sequences that forge or hide check lines in
	// the report, or flood the terminal.
	lastErr := sanitizeAdminText(routerAuth.LastRefreshError, 200)
	lastErrAt := sanitizeAdminText(routerAuth.LastRefreshErrAt, 40)
	routerSignedIn := routerAuth.SignedIn
	if store.ready && (!routerSignedIn || health == "signed_out") {
		storedCredential := store.authType + " credential"
		if store.profile != "" {
			storedCredential = fmt.Sprintf("%s profile %q", store.authType, store.profile)
		}
		add("auth", "health", docFail,
			fmt.Sprintf("the credential store has readable %s, but the running router reports signed out", storedCredential),
			"baseten-switch up   (adopt the current router binary and reload the selected profile)")
		return
	}
	switch health {
	case "refresh_failed":
		detail := ""
		if lastErr != "" {
			detail = ": " + lastErr
		}
		if lastErrAt != "" {
			detail += " (at " + lastErrAt + ")"
		}
		add("auth", "health", docFail,
			"the router's Baseten credential is dead: the token endpoint rejects its refresh token"+detail+"; baseten-routed requests fail (or silently fall back) until reauth",
			"baseten-switch auth login   (or 'baseten auth login', then SIGHUP the router)")
	case "error":
		detail := ""
		if lastErr != "" {
			detail = ": " + lastErr
		}
		add("auth", "health", docWarn,
			"the router's last token refresh failed transiently (network or endpoint error, not a rejected credential)"+detail,
			"usually self-clears on the next refresh; re-run doctor, and check connectivity if it persists")
	case "signed_out":
		add("auth", "health", docSkip, "router has no credential loaded (see auth/signin)", "")
	default:
		add("auth", "health", docOK, "router reports credential health "+health, "")
	}
}

// sanitizeAdminText makes a server-influenced admin-payload string safe
// for terminal output: control characters (newlines, ANSI escape
// introducers) collapse to spaces and the result is capped at max runes.
func sanitizeAdminText(v any, max int) string {
	s, _ := v.(string)
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max {
			b.WriteString("...")
			break
		}
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// doctorBasetenCLICheck scans PATH for Baseten CLI installs. More than one
// distinct binary creates ambiguous login and credential ownership, so the
// warning names paths and versions. PATH order decides which
// `baseten auth login` runs.
func doctorBasetenCLICheck(add addCheck) {
	paths := scanBasetenCLIs()
	switch len(paths) {
	case 0:
		add("auth", "cli", docSkip, "no baseten CLI on PATH ('baseten-switch auth login' needs it; install: "+basetenBrewHint+")", "")
	case 1:
		add("auth", "cli", docOK, fmt.Sprintf("one baseten CLI on PATH: %s (%s)", paths[0], orDash(basetenCLIVersion(paths[0]))), "")
	default:
		var labeled []string
		versions := map[string]bool{}
		for _, p := range paths {
			v := basetenCLIVersion(p)
			versions[v] = true
			labeled = append(labeled, fmt.Sprintf("%s (%s)", p, orDash(v)))
		}
		finding := fmt.Sprintf("%d baseten CLIs on PATH: %s", len(paths), strings.Join(labeled, ", "))
		if len(versions) > 1 {
			finding += "; the versions disagree, and different versions write incompatible credential-store formats (a login with one strands the other's consumers)"
		} else {
			finding += "; duplicate installs invite a version split, and different versions write incompatible credential-store formats"
		}
		add("auth", "cli", docWarn, finding,
			"keep one baseten CLI and remove the stale copies (PATH order decides which one 'baseten auth login' runs)")
	}
}

// doctorAuthPlaceholders filters the unresolved placeholder names to
// those referenced from an auth position (global.auth values or a
// client auth_token value).
func doctorAuthPlaceholders(f *config.File, unresolved []string) []string {
	if f == nil {
		return nil
	}
	var vals []string
	for _, v := range f.Global.Auth {
		vals = append(vals, v)
	}
	for _, c := range f.Clients {
		if c.AuthToken != nil {
			vals = append(vals, c.AuthToken.Value)
		}
	}
	var out []string
	for _, n := range unresolved {
		for _, v := range vals {
			if strings.Contains(v, "${"+n+"}") {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// doctorBasetenRouted names enabled clients that can use Baseten under
// the one global routing gate or through a Baseten fallback.
func doctorBasetenRouted(f *config.File) []string {
	if f == nil {
		return nil
	}
	var out []string
	routingEnabled := f.Global.RoutingEnabled != nil && *f.Global.RoutingEnabled
	for _, c := range f.Clients {
		if !c.Enabled {
			continue
		}
		if routingEnabled || c.FallbackRoute == "baseten" {
			out = append(out, c.Name)
		}
	}
	return out
}

// doctorRouterChecks probes the admin listener (ours/foreign/down, the
// probePort semantics `up` uses) and parses the admin status. Returns the
// parsed status (nil when unavailable) for the mutation, client, and door
// wiring checks.
func doctorRouterChecks(add addCheck, adminAddr string, state portState, allowStartupFix bool) *doctorAdminStatus {
	switch state {
	case portForeign:
		add("router", "reachable", docFail,
			fmt.Sprintf("a foreign process (%s) owns %s (answers, but not our router)", portOwner(adminAddr), adminAddr),
			"free the port or change BASETEN_SWITCH_ADMIN_ADDR")
	case portDown:
		if allowStartupFix {
			add("router", "reachable", docFail, fmt.Sprintf("router not running (nothing listens on %s)", adminAddr),
				"baseten-switch up", "up")
		} else {
			add("router", "reachable", docFail, fmt.Sprintf("router not running (nothing listens on %s)", adminAddr),
				"resolve the routing mutation recovery state before running 'baseten-switch up'")
		}
	default:
		var h struct {
			UptimeSeconds int64 `json:"uptime_seconds"`
		}
		pid := pidfile.ReadFromSafe(gatewayPidfilePath())
		detail := fmt.Sprintf("healthy at %s (pid %d)", adminAddr, pid)
		if getJSON(adminAddr, routerHealthPath, &h) == nil {
			detail += fmt.Sprintf(" (uptime %s)", (time.Duration(h.UptimeSeconds) * time.Second).String())
		}
		if exe := processExePath(pid); exe != "" {
			detail += " " + exe
		}
		add("router", "reachable", docOK, detail, "")
	}
	if state != portOurs {
		add("router", "status", docSkip, "router not up", "")
		return nil
	}

	httpC := &http.Client{Timeout: 2 * time.Second}
	body, ok := doctorAdminGetOK(httpC, adminAddr, "/v1/admin/status")
	if !ok {
		add("router", "status", docFail, fmt.Sprintf("admin /v1/admin/status unreachable at %s", adminAddr),
			"check the router log; restart with 'baseten-switch restart'")
		return nil
	}
	var st doctorAdminStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		add("router", "status", docFail, fmt.Sprintf("admin status does not parse: %v", err),
			"version mismatch between CLI and router? restart with 'baseten-switch restart'")
		return nil
	}
	add("router", "status", docOK, "admin status parses", "")
	return &st
}

// doctorMutationRecoveryCheck reports the read-only status classification.
// It intentionally exposes only reviewed messages and never raw paths, target
// values, journal bytes, or underlying filesystem and admin errors.
func doctorMutationRecoveryCheck(add addCheck, configPath string) {
	status, err := doctorInspectRoutingMutationStatus(configPath)
	doctorMutationRecoveryCheckForStatus(add, status, err)
}

func doctorMutationRecoveryCheckForStatus(add addCheck, status routingMutationStatus, err error) {
	if err != nil {
		add("router", "mutation_recovery", docFail,
			"routing mutation recovery state could not be inspected",
			"run 'baseten-switch mutation status' and preserve the recovery files for manual review")
		return
	}

	switch status.Classification {
	case mutationStatusNone:
		add("router", "mutation_recovery", docOK, "no unfinished routing mutation", "")
	case mutationStatusDesiredActive:
		add("router", "mutation_recovery", docFail,
			"a previous routing change is active but its recovery record still needs cleanup",
			"baseten-switch mutation recover", "mutation", "recover")
	case mutationStatusPriorActive:
		add("router", "mutation_recovery", docFail,
			"a previous routing change was not applied; the prior routing state is active and its recovery record still needs cleanup",
			"baseten-switch mutation recover", "mutation", "recover")
	case mutationStatusCleanupPending:
		add("router", "mutation_recovery", docFail,
			"a completed routing change has stale recovery cleanup state",
			"baseten-switch mutation recover", "mutation", "recover")
	case mutationStatusDesiredPending:
		add("router", "mutation_recovery", docFail,
			"a pending routing change is present but is not the active router configuration",
			"review 'baseten-switch mutation status'; explicit reconciliation is required")
	case mutationStatusPriorPending:
		add("router", "mutation_recovery", docFail,
			"a pending routing change still has its prior configuration on disk",
			"review 'baseten-switch mutation status'; explicit reconciliation is required")
	case mutationStatusRouterUnavailable:
		add("router", "mutation_recovery", docFail,
			"the router is unavailable to confirm a pending routing change",
			"start the router, then rerun 'baseten-switch doctor --fix'")
	case mutationStatusRouterUnsupported:
		add("router", "mutation_recovery", docFail,
			"the running router cannot report authoritative mutation recovery state",
			"restart with the current baseten-switch binary, then rerun doctor")
	case mutationStatusExternalChange:
		add("router", "mutation_recovery", docFail,
			"the configuration changed outside the pending routing operation; both states were preserved",
			"review 'baseten-switch mutation status' before explicit reconciliation")
	case mutationStatusCommitRecoveryRequired:
		add("router", "mutation_recovery", docFail,
			"an interrupted exact-config commit requires explicit recovery",
			"review 'baseten-switch mutation status' before explicit reconciliation")
	case mutationStatusJournalInvalid:
		add("router", "mutation_recovery", docFail,
			"routing mutation recovery state is invalid",
			"run 'baseten-switch mutation status' and preserve the recovery files for manual review")
	case mutationStatusJournalConflict:
		add("router", "mutation_recovery", docFail,
			"multiple or conflicting routing mutation records require manual recovery",
			"run 'baseten-switch mutation status' and preserve the recovery files for manual review")
	default:
		add("router", "mutation_recovery", docFail,
			"routing mutation recovery state has an unsupported classification",
			"run 'baseten-switch mutation status' and preserve the recovery files for manual review")
	}
}

// doctorRouterClientChecks checks every enabled client: bound, valid route,
// sane fallback_route, and a listener that actually accepts TCP.
func doctorRouterClientChecks(add addCheck, st *doctorAdminStatus) {
	if st == nil {
		add("router", "clients", docSkip, "admin status unavailable", "")
		return
	}

	checked := 0
	for _, c := range st.Clients {
		if !c.Enabled {
			continue
		}
		checked++
		name := "client:" + c.Name
		switch {
		case !c.CurrentlyBound:
			add("router", name, docFail, fmt.Sprintf("%s is enabled but its listener %s is not bound", c.Name, c.BindAddr),
				"check the router log "+gatewayLogPath()+"; restart with 'baseten-switch restart'")
		case !route.Valid(c.Route):
			add("router", name, docFail, fmt.Sprintf("%s reports invalid effective route %q", c.Name, c.Route),
				"restart the router with the current CLI; if config validation fails, run 'baseten-switch config reset --yes'")
		case c.FallbackRoute != "" && (!route.Valid(c.FallbackRoute) || c.FallbackRoute == "monitor"):
			add("router", name, docFail, fmt.Sprintf("%s has unusable fallback_route %q (effective route %q)", c.Name, c.FallbackRoute, c.Route),
				"set fallback_route to the client's protocol-native provider, or remove it")
		case !dialOK(c.BindAddr):
			add("router", name, docFail, fmt.Sprintf("%s reports bound but %s does not accept TCP", c.Name, c.BindAddr),
				"restart with 'baseten-switch restart'")
		case c.FallbackRoute == c.Route:
			// effective_route == fallback_route is the designed global-Off
			// state. The saved fallback is dormant until routing is On.
			add("router", name, docOK, fmt.Sprintf("%s bound on %s, effective route %s (fallback dormant while native)", c.Name, c.BindAddr, c.Route), "")
		default:
			add("router", name, docOK, fmt.Sprintf("%s bound on %s, effective route %s", c.Name, c.BindAddr, c.Route), "")
		}
	}
	if checked == 0 {
		add("router", "clients", docSkip, "no enabled clients to check (see config section)", "")
	}
}

func dialOK(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// doctorDoorChecks probes every configured door port (/doorz, same
// ours/foreign/down semantics) and verifies the live door's router
// target is an addr some bound client actually listens on. Returns
// which ports answered as ours, for the supervision and e2e sections.
func doctorDoorChecks(add addCheck, doorSpecs []door.Config, haveStatus bool, boundAddrs map[string]bool, allowStartupFix bool) map[string]bool {
	doorUp := map[string]bool{}
	if len(doorSpecs) == 0 {
		add("door", "ports", docSkip, "no usable door ports (see config section)", "")
		return doorUp
	}
	for _, sp := range doorSpecs {
		port := portOf(sp.ListenAddr)
		switch probePort(sp.ListenAddr, doorHealthPath, doorHealthMarker) {
		case portDown:
			if allowStartupFix {
				add("door", "doorz:"+port, docFail, fmt.Sprintf("door not running (nothing listens on %s)", sp.ListenAddr), "baseten-switch up", "up")
			} else {
				add("door", "doorz:"+port, docFail, fmt.Sprintf("door not running (nothing listens on %s)", sp.ListenAddr),
					"resolve the routing mutation recovery state before running 'baseten-switch up'")
			}
			add("door", "wiring:"+port, docSkip, "door not up", "")
			continue
		case portForeign:
			add("door", "doorz:"+port, docFail,
				fmt.Sprintf("a foreign process (%s) owns %s (answers, but not our door)", portOwner(sp.ListenAddr), sp.ListenAddr),
				"free the port or fix door.ports in gateway.yaml")
			add("door", "wiring:"+port, docSkip, "door not up", "")
			continue
		}
		doorUp[sp.ListenAddr] = true
		var z struct {
			Router              string `json:"router"`
			Tripped             bool   `json:"tripped"`
			CooldownRemainingMS int64  `json:"cooldown_remaining_ms"`
		}
		if err := getJSON(sp.ListenAddr, doorHealthPath, &z); err != nil {
			add("door", "doorz:"+port, docWarn, fmt.Sprintf("/doorz answered but does not parse: %v", err), "baseten-switch restart")
			add("door", "wiring:"+port, docSkip, "doorz payload unavailable", "")
			continue
		}
		if z.Tripped {
			cooldown := (time.Duration(z.CooldownRemainingMS) * time.Millisecond).Round(100 * time.Millisecond)
			add("door", "doorz:"+port, docWarn,
				fmt.Sprintf("door %s is TRIPPED (%s cooldown remaining); the native provider is serving, not the gateway", sp.ListenAddr, cooldown),
				"check the router ('baseten-switch status'); the door retries it after the cooldown")
		} else {
			pid := pidfile.ReadFromSafe(doorPidfilePath())
			detail := fmt.Sprintf("up on %s -> %s (not tripped) (pid %d)", sp.ListenAddr, z.Router, pid)
			if exe := processExePath(pid); exe != "" {
				detail += " " + exe
			}
			add("door", "doorz:"+port, docOK, detail, "")
		}
		switch {
		case !haveStatus:
			add("door", "wiring:"+port, docSkip, "router status unavailable; cannot verify the door's router target", "")
		case boundAddrs[z.Router]:
			add("door", "wiring:"+port, docOK, fmt.Sprintf("door forwards to %s, which a bound client listens on", z.Router), "")
		default:
			add("door", "wiring:"+port, docFail,
				fmt.Sprintf("door %s forwards to router addr %s but no bound client listens there", sp.ListenAddr, z.Router),
				"point door.ports router_addr at an enabled client's bind_addr in gateway.yaml, then 'baseten-switch restart'")
		}
	}
	return doorUp
}

// doctorClaudeChecks verifies the Claude Code wiring through the
// adapter's own seams: settings parse, the three `claude on` env
// values are complete, the shell environment does not override the
// base URL wrongly (sessions inherit the shell env, which wins over
// settings), the subagent split-routing config is valid, and the
// on-state backup is intact. f is the parsed gateway config (nil when
// it failed to load); telPath is the pre-resolved telemetry store path
// for the subagent traffic windowed scan.
func doctorClaudeChecks(add addCheck, f *config.File, telPath string) {
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		reason := fmt.Sprintf("cannot resolve the claude door port: %v", err)
		for _, name := range []string{"settings", "base_url", "attribution_header", "tool_search", "shell_env", "subagents", "model_routes", "model_env", "model_picker", "backup"} {
			add("claude", name, docSkip, reason, "")
		}
		return
	}
	root, _, existed, err := loadClaudeSettings(a.settingsPath)
	if err != nil {
		add("claude", "settings", docFail, err.Error(), "fix the JSON in "+a.settingsPath+" by hand")
		for _, name := range []string{"base_url", "attribution_header", "tool_search", "shell_env", "subagents"} {
			add("claude", name, docSkip, "settings unreadable", "")
		}
		// The model_routes/model_env checks still run: the config-validity
		// half needs only gateway.yaml, and the settings-env half no-ops on
		// a nil root (the process-env scan still applies), so both checks
		// are present in every report shape.
		doctorModelRoutesCheck(add, a, f, nil, false)
		add("claude", "backup", docSkip, "settings unreadable", "")
		return
	}
	var cur string
	var curSet bool
	var env map[string]any
	if existed {
		var envErr error
		env, envErr = settingsEnv(root)
		if envErr != nil {
			add("claude", "settings", docFail, envErr.Error(), "fix the env block in "+a.settingsPath+" by hand")
			for _, name := range []string{"base_url", "attribution_header", "tool_search", "shell_env", "subagents"} {
				add("claude", name, docSkip, "settings env block unreadable", "")
			}
			// As above: config validity needs only gateway.yaml, and the
			// env-slot scan degrades to process env when the settings env
			// block is unreadable (settingsEnv fails again inside).
			doctorModelRoutesCheck(add, a, f, root, existed)
			add("claude", "backup", docSkip, "settings env block unreadable", "")
			return
		}
		cur, curSet = envString(env, claudeManagedEnvKey)
		add("claude", "settings", docOK, "parsed "+a.settingsPath, "")
	} else {
		add("claude", "settings", docOK, "no settings file at "+a.settingsPath, "")
	}

	// `claude on` is self-exec automatable only when the settings file
	// exists (and the adapter resolved a door port, or we would have
	// skipped above); creating a settings file stays a manual decision.
	var claudeOnArgv []string
	if existed {
		claudeOnArgv = []string{"claude", "on"}
	}
	if !curSet {
		add("claude", "base_url", docFail,
			fmt.Sprintf("no %s in %s: Claude Code goes direct to Anthropic and no session routes through the gateway", claudeManagedEnvKey, a.settingsPath),
			"baseten-switch claude on", claudeOnArgv...)
	} else if st, fnd := doctorBaseURLTarget(a, cur); st == docOK {
		add("claude", "base_url", docOK, "settings point at the door ("+cur+")", "")
	} else {
		add("claude", "base_url", docFail, "claude "+fnd, "baseten-switch claude on", claudeOnArgv...)
	}
	for _, check := range []struct {
		name string
		key  string
	}{
		{name: "attribution_header", key: claudeAttributionEnvKey},
		{name: "tool_search", key: claudeToolSearchEnvKey},
	} {
		got, ok := envString(env, check.key)
		want := a.desiredClaudeEnvValue(check.key)
		switch {
		case !ok:
			add("claude", check.name, docFail,
				fmt.Sprintf("%s is missing from %s", check.key, a.settingsPath),
				"baseten-switch claude on", claudeOnArgv...)
		case got != want:
			add("claude", check.name, docFail,
				fmt.Sprintf("%s=%q in %s, want %q", check.key, got, a.settingsPath, want),
				"baseten-switch claude on", claudeOnArgv...)
		default:
			add("claude", check.name, docOK, fmt.Sprintf("%s=%q", check.key, got), "")
		}
	}

	shell := os.Getenv(claudeManagedEnvKey)
	switch {
	case shell == "":
		add("claude", "shell_env", docOK, claudeManagedEnvKey+" not set in this shell (settings value governs)", "")
	default:
		if st, fnd := doctorBaseURLTarget(a, shell); st != docOK {
			add("claude", "shell_env", docFail,
				fmt.Sprintf("shell %s=%s overrides settings and %s", claudeManagedEnvKey, shell, fnd),
				fmt.Sprintf("unset %s in this shell (or export %s=%s)", claudeManagedEnvKey, claudeManagedEnvKey, a.desiredURL()))
		} else if curSet && shell != cur {
			add("claude", "shell_env", docWarn,
				fmt.Sprintf("shell %s (%s) differs from settings (%s); sessions inherit the shell value, which wins", claudeManagedEnvKey, shell, cur),
				"unset "+claudeManagedEnvKey+" in the shell or align it with the settings value")
		} else {
			add("claude", "shell_env", docOK, "shell "+claudeManagedEnvKey+" points at the door", "")
		}
	}

	// Subagent split-routing reads from gateway.yaml (config-side), not
	// settings. ok when unset or enabled with a valid target; FAIL on a
	// hand-edited invalid target (the router would refuse the config);
	// WARN enabled-but-wiring-off; WARN settings/process env var present;
	// WARN enabled with recent traffic but no subagent rows in 24h. All
	// manual, no fixArgv.
	wired := curSet && a.isGatewayURL(cur)
	doctorSubagentsCheck(add, a, f, wired, telPath)
	// Family route pins (config/schema.md): config-side validity,
	// the all-families-pinned warn, and the harness env-slot warn. All
	// manual, no fixArgv.
	doctorModelRoutesCheck(add, a, f, root, existed)
	doctorModelPickerCheck(add, a)

	if !(curSet && a.isGatewayURL(cur)) {
		add("claude", "backup", docSkip, "not gateway-managed; no backup expected", "")
		return
	}
	bak, err := loadClaudeBackup(a.backupPath)
	switch {
	case err != nil:
		add("claude", "backup", docWarn, fmt.Sprintf("backup unreadable: %v", err),
			"'baseten-switch claude off' will strip rather than restore; inspect "+a.backupPath)
	case bak == nil:
		add("claude", "backup", docWarn, "gateway-managed but no backup recorded (managed before the adapter, or backup lost)",
			"'baseten-switch claude off' will strip the gateway value rather than restore an original")
	case a.poisonedBackup(bak):
		add("claude", "backup", docWarn, "backup itself points at a gateway port (poisoned); 'claude off' will discard it",
			"baseten-switch claude off   (discards the poisoned backup safely)")
	default:
		add("claude", "backup", docOK, "backup present ("+a.backupPath+")", "")
	}
}

// doctorSubagentsCheck is the config-side subagent split-routing check.
// It reads subagent_model/subagent_routing from gateway.yaml (via the
// adapter's resolved client), validates the target class, and emits up
// to three findings: the config check, the env-var double-management
// warn, and the windowed traffic warn. All manual, no fixArgv.
func doctorSubagentsCheck(add addCheck, a *claudeAdapter, f *config.File, wired bool, telPath string) {
	var subModel, subRouting string
	if f != nil {
		for i := range f.Clients {
			if f.Clients[i].Name == a.clientName {
				subModel = f.Clients[i].SubagentModel
				subRouting = f.Clients[i].SubagentRouting
				break
			}
		}
	}
	enabled := subModel != "" && subRouting != "off"

	// Config validity: ok when unset or enabled with a valid target;
	// FAIL on a hand-edited invalid target (router would refuse it);
	// FAIL on routing set while model empty (router would refuse it).
	switch {
	case subModel == "" && subRouting != "":
		add("claude", "subagents", docFail,
			fmt.Sprintf("subagent_routing=%s is set but subagent_model is empty in gateway.yaml; the router will refuse this config", subRouting),
			"baseten-switch claude subagents <model>, or remove subagent_routing in gateway.yaml and SIGHUP the router")
	case subModel == "":
		add("claude", "subagents", docOK, "no subagent_model in gateway.yaml (inherit: subagents follow the main model)", "")
	case !validSubagentTarget(subModel, a.modelAliases):
		aliasList := "no model_aliases are configured for the " + a.clientName + " client"
		if len(a.modelAliases) > 0 {
			aliasList = "configured model_aliases are [" + strings.Join(sortedAliasIDs(a.modelAliases), ", ") + "]"
		}
		add("claude", "subagents", docFail,
			fmt.Sprintf("subagent_model=%s in gateway.yaml is invalid; %s; the router will refuse this config", subModel, aliasList),
			"baseten-switch claude subagents <model>, or fix subagent_model in gateway.yaml and SIGHUP the router")
	case enabled && !wired:
		add("claude", "subagents", docWarn,
			fmt.Sprintf("subagent routing is enabled (subagent_model=%s) but %s does not point at the door; the toggle has no effect until 'baseten-switch claude on'", subModel, claudeManagedEnvKey),
			"baseten-switch claude on")
	default:
		// subagent_routing=off means inherit: subagent requests follow
		// the main thread's routing ladder like any other request. The
		// state displays as inherit; only the config literal is "off".
		if subRouting == "off" {
			add("claude", "subagents", docOK, fmt.Sprintf("subagent_model=%s, routing inherit (subagents follow the main model)", subModel), "")
		} else {
			add("claude", "subagents", docOK, fmt.Sprintf("subagent_model=%s, routing on", subModel), "")
		}
	}

	// Env-var double management: CLAUDE_CODE_SUBAGENT_MODEL in settings
	// env or the process env. A single always-present check so the name
	// does not vary by source; ok when absent from both, warn when present
	// in either or both (naming the source(s)).
	var sources []string
	if root, _, existed, err := loadClaudeSettings(a.settingsPath); err == nil && existed {
		if env, envErr := settingsEnv(root); envErr == nil {
			if v, ok := envString(env, claudeSubagentEnvKey); ok && v != "" {
				sources = append(sources, fmt.Sprintf("%s (settings env: %q)", a.settingsPath, v))
			}
		}
	}
	if v := os.Getenv(claudeSubagentEnvKey); v != "" {
		sources = append(sources, fmt.Sprintf("process env (%q)", v))
	}
	if len(sources) > 0 {
		add("claude", "subagent_env", docWarn,
			fmt.Sprintf("%s is set in %s; the gateway rewrite overrides it while toggled on, and the pin still routes explicitly while toggled off. Suggest removing it ('baseten-switch claude off' strips a gateway-owned value, or unset it in the shell).", claudeSubagentEnvKey, strings.Join(sources, ", ")),
			"baseten-switch claude off, or remove "+claudeSubagentEnvKey+" from the settings env block / unset it in the shell")
	} else {
		add("claude", "subagent_env", docOK, claudeSubagentEnvKey+" not set in settings or the process env", "")
	}

	// Windowed traffic check: enabled with recent request rows but no
	// subagent rows in the last 24h. Names both the benign cause (no
	// agentic runs) and the risk (Claude Code dropped the header).
	if enabled && telPath != "" {
		if finding, warn := doctorSubagentTrafficWarn(telPath); warn {
			add("claude", "subagent_traffic", docWarn, finding,
				"run an agentic task (e.g. Claude Code Explore) and re-check; if subagent traffic still shows the parent model, a Claude Code update may have dropped the x-claude-code-agent-id header (the env-based fallback is documented in the Claude Code integration contract)")
		} else if finding != "" {
			add("claude", "subagent_traffic", docOK, finding, "")
		} else {
			add("claude", "subagent_traffic", docOK, "no recent request rows to compare", "")
		}
	} else {
		add("claude", "subagent_traffic", docSkip, "subagent routing not enabled or no telemetry path", "")
	}
}

// validSubagentTarget reports whether a subagent_model value is valid
// against the client's model_aliases: an alias must be configured; a
// slug or native id is always valid by class. Mirrors the CLI verb's
// classifySubagentModel + alias check.
func validSubagentTarget(model string, aliases map[string]string) bool {
	class, ok := classifySubagentModel(model)
	if !ok {
		return false
	}
	if class == subagentClassAlias {
		_, configured := aliases[model]
		return configured
	}
	return true
}

// doctorSubagentTrafficWarn scans the telemetry v1 store for request rows in
// the last 24h and reports whether any carry subagent=true. Returns a
// finding string and true when the warn condition holds (recent rows
// but no subagent rows). This is a self-contained windowed scan.
func doctorSubagentTrafficWarn(telDir string) (finding string, warn bool) {
	events, err := telemetry.ReadEvents(telDir)
	if err != nil || len(events) == 0 {
		return "", false
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	var recent, subagent int
	for _, event := range events {
		if event.CompletedAt.Before(cutoff) {
			continue
		}
		recent++
		if event.Subagent {
			subagent++
		}
	}
	if recent == 0 {
		return "no request rows in the last 24h", false
	}
	if subagent > 0 {
		return fmt.Sprintf("%d request rows in the last 24h, %d with subagent traffic", recent, subagent), false
	}
	return fmt.Sprintf("%d request rows in the last 24h but none carry the x-claude-code-agent-id header (subagent=true); either no agentic runs lately, or a Claude Code update dropped the header (env-based fallback documented in the Claude Code integration contract)", recent), true
}

// doctorModelRoutesCheck is the config-side family route pin check
// (config/schema.md). It reads model_routes from gateway.yaml,
// validates every key and target against the same rules the router
// enforces at load (FAIL on a hand-edited invalid entry, naming the
// router refusal), and WARNs when ANTHROPIC_MODEL or any
// ANTHROPIC_DEFAULT_*_MODEL is present in the settings env block or the
// process env (they change what the harness requests upstream of family
// pins). All manual, no fixArgv. root/existed are the already-loaded
// settings so the env scan reuses the read.
func doctorModelRoutesCheck(add addCheck, a *claudeAdapter, f *config.File, root map[string]any, existed bool) {
	var routes map[string]string
	if f != nil {
		for i := range f.Clients {
			if f.Clients[i].Name == a.clientName {
				routes = f.Clients[i].ModelRoutes
				break
			}
		}
	}

	// Config validity: ok when empty; FAIL on any invalid key or target
	// (the router would refuse the config at load). Mirrors the CLI verb's
	// validation so a hand-edit reports the same refusal.
	if len(routes) == 0 {
		add("claude", "model_routes", docOK, "no model_routes in gateway.yaml (families follow the switch)", "")
	} else {
		var bad []string
		for _, k := range sortedRouteKeys(routes) {
			v := routes[k]
			if problem := routeKeyProblem(k); problem != "" {
				bad = append(bad, problem)
				continue
			}
			if !validRouteTarget(v, a.modelAliases) {
				aliasList := "no model_aliases are configured for the " + a.clientName + " client"
				if len(a.modelAliases) > 0 {
					aliasList = "configured model_aliases are [" + strings.Join(sortedAliasIDs(a.modelAliases), ", ") + "]"
				}
				bad = append(bad, fmt.Sprintf("target %q for key %q is invalid; %s", v, k, aliasList))
			}
		}
		if len(bad) > 0 {
			add("claude", "model_routes", docFail,
				fmt.Sprintf("model_routes in gateway.yaml has invalid entries: %s; the router will refuse this config", strings.Join(bad, "; ")),
				"baseten-switch claude route <family> <target|default>, or fix model_routes in gateway.yaml and SIGHUP the router")
		} else {
			pinnedFamilies := 0
			for _, fam := range claudeFamilies {
				if _, ok := routes[fam]; ok {
					pinnedFamilies++
				}
			}
			add("claude", "model_routes", docOK, fmt.Sprintf("model_routes valid (%d pin(s), %d family pin(s))", len(routes), pinnedFamilies), "")
		}
	}

	// Harness env-slot warn: ANTHROPIC_MODEL or any
	// ANTHROPIC_DEFAULT_*_MODEL in settings env or process env changes
	// what the harness requests upstream of family pins (the fireconnect
	// survey hazard). A single always-present check so the name does not
	// vary by source; ok when none set. The finding names only the vars
	// actually present (the sources list carries per-source detail).
	present, sources := routeEnvSlotFindings(root, existed)
	if len(sources) > 0 {
		add("claude", "model_env", docWarn,
			fmt.Sprintf("%s set in %s; these change what the harness requests upstream of family pins (double management). Suggest removing them.", strings.Join(present, ", "), strings.Join(sources, ", ")),
			"remove the env vars from the settings env block / unset them in the shell")
	} else {
		add("claude", "model_env", docOK, "no ANTHROPIC_MODEL or ANTHROPIC_DEFAULT_*_MODEL env vars set", "")
	}
}

// sortedRouteKeys returns the model_routes keys in sorted order for
// deterministic findings.
func sortedRouteKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// routeKeyProblem returns the reason the router would refuse a
// model_routes key at load, or "" when the key is valid.
func routeKeyProblem(key string) string {
	if isFamilyKey(key) {
		return ""
	}
	return fmt.Sprintf("key %q is not a supported family (%s)", key, strings.Join(claudeFamilies, ", "))
}

// codexDoctorCheckNames lists the codex-section checks in report
// order, so the skip-all paths emit the same report shape as a full
// run.
var codexDoctorCheckNames = []string{"overlay", "client", "auth_token", "config_toml", "backup", "strip_knob"}

// doctorCodexChecks verifies the codex wiring through the codex
// adapter's own seams (BASETEN_SWITCH_CODEX_HOME, BASETEN_SWITCH_BACKUP_DIR): the overlay
// file state and its base_url target, the gateway client's
// enabled/parked state against an installed overlay (parked with the
// overlay installed means 'codex --profile baseten' points at a dead
// route, the one FAIL combination), the Switch-managed
// CODEX_AUTH_TOKEN placeholder in the gateway env file, the additive
// invariant on the user's
// config.toml (read-only peek; baseten-switch never writes that file), the
// on-state backup, and the responses_strip_tool_types knob on a
// baseten-routed client. The whole section skips when no openai-shape
// client is configured: codex wiring is additive, so its absence is a
// legitimate default, never a failure.
func doctorCodexChecks(add addCheck, f *config.File, envFile map[string]string, envFilePath string) {
	skipAll := func(reason string) {
		for _, name := range codexDoctorCheckNames {
			add("codex", name, docSkip, reason, "")
		}
	}
	if f == nil {
		skipAll("gateway config did not load (see config section)")
		return
	}
	hasOpenAI := false
	for _, c := range f.Clients {
		if c.ProtocolShape == "openai" {
			hasOpenAI = true
			break
		}
	}
	if !hasOpenAI {
		// Checked before codexDoorPort: its missing-client error carries
		// the multi-line paste block, which does not belong in a finding.
		skipAll("no openai-shape client in gateway.yaml (codex integration not configured; see the Codex integration contract)")
		return
	}
	client, port, ports, err := codexDoorPort(f)
	if err != nil {
		skipAll(fmt.Sprintf("cannot resolve the codex door port: %v", err))
		return
	}
	codexHome := envDefault("BASETEN_SWITCH_CODEX_HOME", homeJoin(".codex"))
	backupRoot := envDefault("BASETEN_SWITCH_BACKUP_DIR", homeJoin(".config", "baseten-switch", "backups"))
	overlayPath := filepath.Join(codexHome, codexOverlayName)
	// Doctor only classifies files, so the adapter needs no model slug
	// and no in/out streams; cmdCodex-only fields stay zero.
	a := &codexAdapter{
		overlayPath:   overlayPath,
		backupPath:    codexBackupPath(backupRoot, overlayPath),
		envFilePath:   envFilePath,
		clientName:    client.Name,
		clientEnabled: client.Enabled,
		desiredPort:   port,
		gatewayPorts:  ports,
	}

	raw, existed, readErr := readCodexOverlay(a.overlayPath)
	if readErr != nil {
		add("codex", "overlay", docWarn, readErr.Error(), "fix the permissions on "+a.overlayPath)
		for _, name := range []string{"client", "auth_token", "backup"} {
			add("codex", name, docSkip, "overlay unreadable", "")
		}
		// config_toml and strip_knob do not depend on the overlay.
		doctorCodexConfigTomlCheck(add, codexHome)
		doctorCodexStripKnobCheck(add, f, client)
		return
	}
	sh := parseCodexOverlay(raw)
	ours := existed && codexOursShaped(sh)
	switch {
	case !existed:
		add("codex", "overlay", docOK, "no overlay at "+a.overlayPath+" (additive default state; 'baseten-switch codex on' installs it)", "")
	case !ours:
		add("codex", "overlay", docWarn,
			fmt.Sprintf("%s exists but is not baseten-switch-managed; 'codex --profile baseten' loads it instead of the gateway overlay ('baseten-switch codex on' refuses to overwrite it)", a.overlayPath),
			"move the file aside and re-run 'baseten-switch codex on'")
	default:
		if st, fnd := doctorPortTarget(sh.baseURL, a.desiredPort); st == docOK {
			add("codex", "overlay", docOK, "overlay points at the door ("+sh.baseURL+") and uses the compatibility model", "")
		} else {
			add("codex", "overlay", docFail, "codex overlay "+fnd, "baseten-switch codex on", "codex", "on")
		}
	}

	switch {
	case client.Enabled:
		add("codex", "client", docOK, fmt.Sprintf("gateway %s client enabled (listener %s)", client.Name, client.BindAddr), "")
	case ours:
		// The parked flip needs interactive consent (the door-port rebind
		// side effect), so no fixArgv: `codex on` under --fix would read
		// EOF and decline.
		add("codex", "client", docFail,
			fmt.Sprintf("the overlay is installed but the gateway %s client is parked (enabled: false); 'codex --profile baseten' points at a dead route", client.Name),
			"baseten-switch codex on   (offers to enable it), or set enabled: true on the "+client.Name+" client in gateway.yaml and SIGHUP the router")
	default:
		add("codex", "client", docOK, fmt.Sprintf("gateway %s client parked (enabled: false); no managed overlay depends on it ('baseten-switch codex on' offers to enable it)", client.Name), "")
	}

	switch {
	case !ours:
		add("codex", "auth_token", docSkip, "no managed overlay depends on the gateway Codex placeholder", "")
	case envFile[codexManagedEnvKey] != "":
		add("codex", "auth_token", docOK, "Switch-managed gateway placeholder is present in "+envFilePath, "")
	default:
		add("codex", "auth_token", docFail,
			fmt.Sprintf("Switch-managed gateway placeholder %s is missing from %s; gateway.yaml interpolates it for the local Codex client, while the Codex profile itself requires no shell token", codexManagedEnvKey, envFilePath),
			"baseten-switch codex on   (writes the gateway placeholder to the Switch env file)", "codex", "on")
	}

	doctorCodexConfigTomlCheck(add, codexHome)

	if managed := ours && a.isGatewayURL(sh.baseURL); !managed {
		add("codex", "backup", docSkip, "not gateway-managed; no backup expected", "")
	} else if bak, bakErr := loadCodexBackup(a.backupPath); bakErr != nil {
		add("codex", "backup", docWarn, fmt.Sprintf("backup unreadable: %v", bakErr),
			"'baseten-switch codex off' will remove the overlay rather than restore; inspect "+a.backupPath)
	} else if bak == nil {
		add("codex", "backup", docWarn, "gateway-managed but no backup recorded (managed before the adapter, or backup lost)",
			"'baseten-switch codex off' will remove the overlay rather than restore an original")
	} else if a.poisonedBackup(bak) {
		add("codex", "backup", docWarn, "backup itself points at a gateway port (poisoned); 'codex off' will discard it",
			"baseten-switch codex off   (discards the poisoned backup safely)")
	} else {
		add("codex", "backup", docOK, "backup present ("+a.backupPath+")", "")
	}

	doctorCodexStripKnobCheck(add, f, client)
}

// doctorCodexConfigTomlCheck is the additive-invariant peek at the
// user's config.toml (READ-ONLY; the adapter never writes that file):
// a root model_provider = "baseten" means every codex session routes
// through the gateway, not just `--profile baseten` opt-ins. Absence of
// the file or the key is the invariant holding.
func doctorCodexConfigTomlCheck(add addCheck, codexHome string) {
	cfgToml := filepath.Join(codexHome, "config.toml")
	b, err := os.ReadFile(cfgToml)
	switch {
	case os.IsNotExist(err):
		add("codex", "config_toml", docOK, "no "+cfgToml+" (additive invariant holds)", "")
	case err != nil:
		add("codex", "config_toml", docSkip, fmt.Sprintf("cannot read %s: %v", cfgToml, err), "")
	case codexRootModelProvider(b) == "baseten":
		add("codex", "config_toml", docWarn,
			fmt.Sprintf("root model_provider in %s is \"baseten\": EVERY codex session routes through the gateway, not just 'codex --profile baseten' (the additive invariant is flipped; baseten-switch never writes this file)", cfgToml),
			"remove the model_provider = \"baseten\" line from "+cfgToml+" and opt in per session with 'codex --profile baseten'")
	default:
		add("codex", "config_toml", docOK, "root model_provider in "+cfgToml+" does not point at the gateway (additive invariant holds)", "")
	}
}

// doctorCodexStripKnobCheck reports the emergency tool denylist and validates
// the Responses compatibility block. The check name is retained for stable
// doctor report consumers. Saved policy is reported even while global routing
// is Off because it becomes active the next time the operator turns the switch
// On.
func doctorCodexStripKnobCheck(add addCheck, _ *config.File, client *config.Client) {
	compatibility, err := config.ResolveResponsesCompatibility(client.ResponsesCompatibility)
	if err != nil {
		add("codex", "strip_knob", docWarn,
			fmt.Sprintf("invalid responses_compatibility on the %s client: %v", client.Name, err),
			"correct the responses_compatibility block in gateway.yaml and SIGHUP the router")
		return
	}

	var findings []string
	var fixes []string
	if compatibility.AdditionalToolsInput == config.ResponsesCompatibilityModeOn {
		findings = append(findings, "experimental responses_compatibility.additional_tools_input is enabled")
		fixes = append(fixes, "set responses_compatibility.additional_tools_input: off")
	}
	if len(client.ResponsesStripToolTypes) > 0 {
		findings = append(findings,
			fmt.Sprintf("responses_strip_tool_types removes [%s]", strings.Join(client.ResponsesStripToolTypes, ", ")))
		fixes = append(fixes, "set responses_strip_tool_types: []")
	}
	if len(findings) == 0 {
		add("codex", "strip_knob", docOK,
			fmt.Sprintf("%s client uses the safe Responses compatibility defaults and has no explicit tool denylist", client.Name), "")
		return
	}

	add("codex", "strip_knob", docWarn,
		fmt.Sprintf("%s on the %s client; these overrides may degrade Codex capabilities or custom-tool semantics", strings.Join(findings, "; "), client.Name),
		strings.Join(fixes, "; ")+" in gateway.yaml and SIGHUP the router")
}

// codexRootModelProvider extracts the ROOT-scope model_provider value
// from a config.toml: only lines before the first [table] header
// count. Deliberately not a TOML parser (dependency policy), the same
// line-scan convention as parseCodexOverlay.
func codexRootModelProvider(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "model_provider" {
			continue
		}
		return codexTOMLValue(v)
	}
	return ""
}

// doctorBaseURLTarget classifies a base URL against the claude door
// port. Non-ok returns a subject-less finding fragment ("points at
// :NNNN but the door listens on :MMMM") so the settings and shell-env
// checks can prefix their own subject.
func doctorBaseURLTarget(a *claudeAdapter, raw string) (status, finding string) {
	return doctorPortTarget(raw, a.desiredPort)
}

// doctorPortTarget is the shape-independent half of the base URL
// classification: the claude settings checks and the codex overlay
// check both compare a harness-side URL against their door port.
func doctorPortTarget(raw, desiredPort string) (status, finding string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return docFail, fmt.Sprintf("points at unparseable URL %q but the door listens on :%s", raw, desiredPort)
	}
	host, port := u.Hostname(), u.Port()
	if (host == "127.0.0.1" || host == "localhost") && port == desiredPort {
		return docOK, ""
	}
	if port != "" {
		return docFail, fmt.Sprintf("points at :%s but the door listens on :%s", port, desiredPort)
	}
	return docFail, fmt.Sprintf("points at %s but the door listens on :%s", raw, desiredPort)
}

// doctorSupervisionChecks reports the launchd state per label. Skipped
// entirely under BASETEN_SWITCH_LAUNCHD=off or off darwin, matching the rest of
// the CLI's launchd seam.
func doctorSupervisionChecks(add addCheck, routerState portState, doorSpecs []door.Config, doorUp map[string]bool, allowStartupFix bool) {
	if runtime.GOOS != "darwin" || launchdDisabled() {
		add("supervision", "launchd", docSkip, "launchd interaction disabled (BASETEN_SWITCH_LAUNCHD=off or non-darwin)", "")
		return
	}
	type comp struct {
		name  string
		label string
		up    bool
	}
	comps := []comp{{"router", launchd.RouterLabel, routerState == portOurs}}
	if len(doorSpecs) > 0 {
		comps = append(comps, comp{"door", launchd.DoorLabel, len(doorUp) > 0})
	}
	for _, c := range comps {
		s := superviseState(c.label)
		switch {
		case s.supervised():
			add("supervision", c.name, docOK, "launchd job "+c.label+" loaded", "")
		case s.installed:
			if allowStartupFix {
				add("supervision", c.name, docWarn,
					"launchd plist installed but "+c.label+" is not loaded (a login-items approval in System Settings may be pending)",
					"baseten-switch up   (re-bootstraps the job; approve it in System Settings > General > Login Items if prompted)",
					"up")
			} else {
				add("supervision", c.name, docWarn,
					"launchd plist installed but "+c.label+" is not loaded (a login-items approval in System Settings may be pending)",
					"resolve the routing mutation recovery state before re-bootstrapping the job")
			}
		case c.up:
			add("supervision", c.name, docWarn, c.name+" is running unsupervised (no LaunchAgent installed)",
				"baseten-switch up --install")
		default:
			add("supervision", c.name, docSkip, "no LaunchAgent installed and "+c.name+" is not running", "")
		}
	}
}

// doctorE2EChecks fires live requests through the door ports (not the
// router listeners), so a pass proves the full
// harness-facing path. Real requests cost real money: without --probe
// this is a skip, and with it a consent prompt runs unless --yes. After
// each probe, the served-route check asserts the
// answer actually came over the CONFIGURED route: the door's failover
// (and the router's fallback_route) can make a probe pass while the
// configured path is dead (impact map, the credential-refresh contract).
func doctorE2EChecks(add addCheck, o doctorOpts, doorSpecs []door.Config, doorUp map[string]bool, f *config.File, telPath string) {
	if !o.probe {
		add("e2e", "probe", docSkip, "run with --probe for a live request", "")
		return
	}
	var targets []doctorProbeTarget
	routerTargets := map[string]string{} // door listen addr -> router target addr
	for _, sp := range doorSpecs {
		if !doorUp[sp.ListenAddr] {
			continue
		}
		shape := "anthropic"
		if sp.Shape == door.ShapeOpenAI {
			shape = "openai"
		}
		targets = append(targets, doctorProbeTarget{
			ProtocolShape: shape,
			BindAddr:      sp.ListenAddr,
		})
		routerTargets[sp.ListenAddr] = sp.RouterTarget
	}
	if len(targets) == 0 {
		add("e2e", "probe", docSkip, "no healthy door port to probe", "")
		return
	}
	if !o.yes {
		confirmed, err := confirmDoctorProbes(targets)
		if err != nil || !confirmed {
			add("e2e", "probe", docSkip, "probe declined", "")
			return
		}
	}
	httpC := &http.Client{Timeout: time.Duration(o.timeoutSec) * time.Second}
	for _, t := range targets {
		port := portOf(t.BindAddr)
		probeStart := time.Now()
		p := doctorProbeClient(httpC, t)
		if doctorProbeOK(p) && p.FallbackTrigger == "auth_unavailable" {
			add("e2e", "probe:"+port, docFail,
				fmt.Sprintf("1-token request succeeded through native fallback because Baseten auth was unavailable (status %d, %dms, model %s%s)", p.Status, p.LatencyMs, orDash(p.Model), doctorProbeResponseSuffix(p)),
				"configure a usable Baseten credential, then rerun the probe")
		} else if doctorProbeOK(p) {
			add("e2e", "probe:"+port, docOK,
				fmt.Sprintf("1-token request through the door succeeded (status %d, %dms, model %s%s)", p.Status, p.LatencyMs, orDash(p.Model), doctorProbeResponseSuffix(p)), "")
		} else {
			add("e2e", "probe:"+port, docFail,
				fmt.Sprintf("1-token request through the door failed (status %d%s): %s", p.Status, doctorProbeResponseSuffix(p), orDash(p.Error)),
				"baseten-switch status   (then check the router and door logs)")
		}
		doctorProbeRouteCheck(add, port, p, probeStart, f, routerTargets[t.BindAddr], t.ProtocolShape, telPath)
	}
}

func doctorProbeResponseSuffix(p *doctorProbeResult) string {
	var details []string
	switch p.DoorVia {
	case "router", "fallback":
		details = append(details, "served via "+p.DoorVia)
	}
	if trigger := sanitizeAdminText(p.FallbackTrigger, 80); trigger != "" {
		details = append(details, "fallback trigger "+strconv.Quote(trigger))
	}
	if len(details) == 0 {
		return ""
	}
	return ", " + strings.Join(details, ", ")
}

// doctorProbeRouteCheck asserts the probe was served by the route the
// config designates for the probe's model, so failover can no longer make
// --probe pass while the configured path is dead. Evidence, in preference
// order: the door's X-Baseten-Switch-Door response header (internal/door relay;
// "fallback" means the door replayed the probe against the native
// provider and the router never saw it), then the probe's own telemetry
// row. The expected route comes from gateway.ExpectedPrimaryRoute: a
// model_routes mapping moves the designed route for the probe's model, and a
// telemetry route_effective value alone cannot distinguish that mapping from
// the router's fallback_route. The current door contract requires the
// X-Baseten-Switch-Door stamp so the probe can be attributed safely.
func doctorProbeRouteCheck(add addCheck, port string, p *doctorProbeResult, probeStart time.Time, f *config.File, routerTarget, shape, telPath string) {
	name := "route:" + port
	cli, ok := doctorClientFor(f, routerTarget, shape)
	if !ok {
		add("e2e", name, docSkip, "cannot resolve the configured client for door target "+routerTarget+" (see config section)", "")
		return
	}
	globalRoutingEnabled := false
	if f != nil && f.Global.RoutingEnabled != nil {
		globalRoutingEnabled = *f.Global.RoutingEnabled
	}
	configured := gateway.ExpectedPrimaryRoute(cli, globalRoutingEnabled, "")
	probeModel := doctorProbeRequestModel(shape)
	expected := gateway.ExpectedPrimaryRoute(cli, globalRoutingEnabled, probeModel)
	if p.DoorVia == "fallback" {
		outcome := "the probe passed"
		if !doctorProbeOK(p) {
			outcome = fmt.Sprintf("the probe failed with status %d", p.Status)
		}
		add("e2e", name, docFail,
			fmt.Sprintf("%s, and the door's native failover served it (X-Baseten-Switch-Door: fallback), not the configured route %q", outcome, configured),
			"baseten-switch status   (the door tripped; check the router and the auth/health check)")
		return
	}
	if p.DoorVia != "router" {
		if !doctorProbeOK(p) && p.Status == 0 {
			add("e2e", name, docSkip, "the failed probe returned no HTTP response; no served route can be attributed", "")
			return
		}
		add("e2e", name, docFail,
			"the door response omitted the required X-Baseten-Switch-Door routing stamp",
			"baseten-switch up   (restart the door onto the current binary)")
		return
	}
	row, ok := probeTelemetryRow(telPath, cli.Name, probeModel, p.Status, probeStart)
	if !ok {
		add("e2e", name, docSkip, "no telemetry row recorded for the probe; cannot verify the served route", "")
		return
	}
	served := row.EffectiveProvider
	if served == "" {
		served = row.ConfiguredRoute
	}
	if served != expected {
		trigger := doctorFallbackTrigger(row)
		if !doctorProbeOK(p) {
			add("e2e", name, docFail,
				fmt.Sprintf("the failed probe reached route %q while the configured route is %q%s", served, expected, trigger),
				"check the router log "+gatewayLogPath()+" and the auth/health check; the primary upstream is failing")
			return
		}
		add("e2e", name, docFail,
			fmt.Sprintf("the probe passed but route %q served it while the configured route is %q%s; the %s path is broken behind a passing probe", served, expected, trigger, expected),
			"check the router log "+gatewayLogPath()+" and the auth/health check; the primary upstream is failing")
		return
	}
	if !doctorProbeOK(p) {
		add("e2e", name, docOK,
			fmt.Sprintf("the failed probe reached the configured route (%s), status %d%s", expected, p.Status, doctorFallbackTrigger(row)), "")
		return
	}
	if expected != configured {
		add("e2e", name, docOK,
			fmt.Sprintf("served route matches the model_routes pin for the probe model %s (%s); the configured route %q was not exercised", probeModel, expected, configured), "")
		return
	}
	add("e2e", name, docOK, fmt.Sprintf("served route matches the configured route (%s)", configured), "")
}

func doctorFallbackTrigger(row telemetry.EventV1) string {
	if row.Fallback.Trigger == nil {
		return ""
	}
	trigger := sanitizeAdminText(*row.Fallback.Trigger, 80)
	if trigger == "" {
		return ""
	}
	return " (fallback trigger " + strconv.Quote(trigger) + ")"
}

// doctorClientFor picks the enabled client the probe exercised: same
// bind_addr AND protocol shape. A door port can front a bind_addr shared
// by an anthropic-shape and an openai-shape client (the router serves
// them as one listener group); matching bind_addr alone can name the
// other shape's client, whose route and telemetry rows are the wrong
// comparison for this probe.
func doctorClientFor(f *config.File, routerTarget, shape string) (config.Client, bool) {
	if f == nil {
		return config.Client{}, false
	}
	for _, c := range f.Clients {
		cShape := c.ProtocolShape
		if cShape == "" {
			cShape = "anthropic"
		}
		if !c.Enabled || c.BindAddr != routerTarget || cShape != shape {
			continue
		}
		return c, true
	}
	return config.Client{}, false
}

// probeRowWait bounds the wait for the probe's telemetry event: the router
// writes it after relaying the response, so doctor can read the store
// before the row lands. Package var so tests shrink it.
var probeRowWait = 2 * time.Second

// probeTelemetryRow finds the probe's own request event: the newest event at
// or after since (1s slop for scheduling) matching the probe's
// client, requested model, and status. Doctor targets a LIVE gateway, so
// rows from concurrent harness traffic land in the same window; requiring
// the model and status match keeps a concurrent request's row from being
// read as the probe's. An identical concurrent request (same client,
// model, status, window) can still collide; residual and accepted.
func probeTelemetryRow(telDir, clientName, model string, status int, since time.Time) (telemetry.EventV1, bool) {
	deadline := time.Now().Add(probeRowWait)
	for {
		cutoff := since.Add(-time.Second)
		events, _ := telemetry.ReadEvents(telDir)
		for i := len(events) - 1; i >= 0; i-- {
			event := events[i]
			if event.CompletedAt.Before(cutoff) {
				break
			}
			if clientName != "" && event.Client != clientName {
				continue
			}
			if event.RequestedModel != model ||
				event.Status == nil ||
				*event.Status != status {
				continue
			}
			return event, true
		}
		if time.Now().After(deadline) {
			return telemetry.EventV1{}, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// doctorTelemetryChecks verifies the telemetry v1 store is where the
// gateway would write it and reports the age of the last request event.
// Honest and simple: doctor cannot know whether traffic was expected,
// so absence is a warn (nothing logged yet, or wrong path), never a
// fail. telPath is pre-resolved by runDoctor so the claude section can
// share it.
func doctorTelemetryChecks(add addCheck, f *config.File, telPath string) {
	info, err := os.Stat(telPath)
	if err != nil {
		add("telemetry", "log", docWarn, "no telemetry store at "+telPath+" (nothing logged through the gateway yet, or wrong path)",
			"send a request through the gateway and re-run doctor")
		add("telemetry", "freshness", docSkip, "no telemetry store to read", "")
		// Log paths for the two daemons are named
		// so doctor is the deep surface that carries every dropped status
		// fact. An ok row: the paths are resolved, not probed for existence
		// (the daemons append, so absence is normal on a fresh install).
		// Added last so the compressed non-verbose view leads with the
		// actual health probe (telemetry/log writability), not static
		// path strings.
		add("telemetry", "logs", docOK,
			fmt.Sprintf("router %s, door %s", gatewayLogPath(), doorLogPath()), "")
		return
	}
	_, segmentErr := telemetry.DiscoverSegments(telPath)
	if !info.IsDir() {
		segmentErr = fmt.Errorf("path is not a directory")
	}
	if segmentErr != nil || info.Mode().Perm()&0o222 == 0 {
		if segmentErr == nil {
			segmentErr = fmt.Errorf("directory has no writable permission bits")
		}
		add("telemetry", "log", docWarn, fmt.Sprintf("telemetry store %s is not writable: %v", telPath, segmentErr),
			"fix the directory permissions (the router creates and appends telemetry segments)")
	} else {
		add("telemetry", "log", docOK, "telemetry store writable at "+telPath, "")
	}
	events, readErr := telemetry.ReadEvents(telPath)
	if readErr != nil {
		add("telemetry", "freshness", docWarn, "telemetry store unreadable: "+readErr.Error(),
			"check telemetry directory and segment permissions")
	} else if len(events) == 0 {
		add("telemetry", "freshness", docOK, "store present, no request events yet", "")
	} else {
		last := events[len(events)-1].CompletedAt
		age := time.Since(last).Round(time.Second)
		add("telemetry", "freshness", docOK, fmt.Sprintf("last request logged %s ago (%d events)", age, len(events)), "")
	}
	// Log paths last (see the no-log branch above for the rationale).
	add("telemetry", "logs", docOK,
		fmt.Sprintf("router %s, door %s", gatewayLogPath(), doorLogPath()), "")
}

// printDoctorReport renders the human output: section-grouped check
// lines, then either the DIAGNOSIS paragraph naming the first failure
// or the all-passed summary. Without verbose, a section whose checks
// all passed compresses to one line.
func printDoctorReport(w io.Writer, rep doctorReport, verbose bool) {
	var order []string
	bySection := map[string][]doctorCheck{}
	for _, c := range rep.Checks {
		if _, ok := bySection[c.Section]; !ok {
			order = append(order, c.Section)
		}
		bySection[c.Section] = append(bySection[c.Section], c)
	}
	for _, section := range order {
		checks := bySection[section]
		allOK := true
		for _, c := range checks {
			if c.Status != docOK {
				allOK = false
				break
			}
		}
		if allOK && !verbose {
			line := fmt.Sprintf("%s %s: %s", doctorTag(docOK), section, checks[0].Finding)
			if len(checks) > 1 {
				line += fmt.Sprintf("  (+%d more ok)", len(checks)-1)
			}
			fmt.Fprintln(w, line)
			continue
		}
		for _, c := range checks {
			label := section
			if verbose {
				label = section + "/" + c.Name
			}
			fmt.Fprintf(w, "%s %s: %s\n", doctorTag(c.Status), label, c.Finding)
			if c.Fix != "" && (c.Status == docFail || c.Status == docWarn) {
				fmt.Fprintf(w, "       fix: %s\n", c.Fix)
			}
		}
	}
	fmt.Fprintln(w)
	if rep.FirstFailure != "" {
		for _, c := range rep.Checks {
			if c.Section+"/"+c.Name == rep.FirstFailure {
				fmt.Fprintf(w, "DIAGNOSIS: first broken link is %s: %s\n", rep.FirstFailure, c.Finding)
				if c.Fix != "" {
					fmt.Fprintf(w, "  fix: %s\n", c.Fix)
				}
				break
			}
		}
		return
	}
	okN, warnN := 0, 0
	for _, c := range rep.Checks {
		switch c.Status {
		case docOK:
			okN++
		case docWarn:
			warnN++
		}
	}
	fmt.Fprintf(w, "all checks passed (%d ok, %d warn)\n", okN, warnN)
}

func doctorTag(status string) string {
	return fmt.Sprintf("%-6s", "["+status+"]")
}
