package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/doorcli"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

func main() {
	if os.Getenv("BASETEN_SWITCH_PRIVATE_RUNTIME") == "1" {
		// Preview is a credential and prompt-bearing runtime rooted in a
		// private directory. Apply the mask before the CLI, router, door, or
		// process can create pidfiles, logs, state, or settings.
		syscall.Umask(0o077)
	}
	args, err := normalizeMutationInvocation(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(dispatch(args))
}

// dispatch handles help before handing control to any command. Some command
// handlers intentionally ignore extra arguments, so this ordering is also a
// safety boundary: `gateway stop --help`, for example, must never reach the
// stop implementation.
func dispatch(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage()
		return 0
	case "-v", "--version":
		fmt.Printf("baseten-switch %s\n", version.Version)
		return 0
	}
	if commandHelpRequested(args) {
		if printCommandHelp(os.Stderr, args[0]) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
		usage()
		return 2
	}
	// The door short-circuits before any router-related initialization
	// so the front-door process stays minimal (the lifecycle contract).
	if args[0] == "door" {
		return doorcli.Run(args[1:])
	}
	switch args[0] {
	case "gateway":
		return cmdGateway(args[1:])
	case "up":
		return cmdUp(args[1:])
	case "down":
		return cmdDown(args[1:])
	case "uninstall":
		return cmdUninstall(args[1:])
	case "restart":
		return cmdRestart(args[1:])
	case "on":
		return cmdOn(args[1:])
	case "off":
		return cmdOff(args[1:])
	case "mutation":
		return cmdMutation(args[1:])
	case "config":
		return cmdConfig(args[1:])
	case "setup":
		return cmdSetup(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "spend":
		return cmdSpend(args[1:])
	case "traces":
		return cmdTraces(args[1:])
	case "healthz":
		return cmdHealthz(args[1:])
	case "whoami":
		return cmdWhoami(args[1:])
	case "auth":
		return cmdAuth(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "claude":
		return cmdClaude(args[1:])
	case "codex":
		return cmdCodex(args[1:])
	case "pi":
		return cmdPi(args[1:])
	case "menubar":
		return cmdMenubar(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
		usage()
		return 2
	}
}

type commandHelp struct {
	name    string
	summary string
	detail  string
}

var commandHelpEntries = []commandHelp{
	{"up", "Start the door and router, adopting the current binary", `Usage: baseten-switch up [--install | --uninstall]

Validate the config, then start the door and router as detached daemons.
Healthy components are left alone. Components running an older version are
restarted onto this binary and re-verified. On macOS, the menubar app is
installed or refreshed and driven to running-and-current. Readiness gates on
/healthz and /doorz, then the command prints the status summary.

  --install    Install two launchd user agents with RunAtLoad and KeepAlive
  --uninstall  Boot out and remove the launchd user agents

Environment:
  BASETEN_SWITCH_CONFIG_PATH     Explicit gateway.yaml path
  BASETEN_SWITCH_LAUNCHD=off     Disable launchd supervision and install/uninstall support
  BASETEN_SWITCH_MENUBAR=off     Skip the automatic macOS menubar app step
  BASETEN_SWITCH_DOOR_PIDFILE    Door pidfile path
  BASETEN_SWITCH_DOOR_LOG        Door log path
`},
	{"down", "Stop the door, router, and optional macOS menubar app", `Usage: baseten-switch down

Stop the door, then the router, using their pidfiles. A clean stop uses
SIGTERM and escalates to SIGKILL when needed. launchd-supervised components
are booted out instead. The optional macOS menubar app is quit last; a menubar
failure is reported but does not fail an otherwise successful shutdown.

BASETEN_SWITCH_LAUNCHD=off disables launchd interaction. BASETEN_SWITCH_MENUBAR=off disables the
menubar shutdown step.
`},
	{"uninstall", "Remove the current Baseten Switch installation safely", `Usage: baseten-switch uninstall [--dry-run] [--purge --yes]

Restore only provably managed Claude Code and Codex settings, stop the current
door and router, remove the current launchd agents and runtime residue, and
quit the current menubar app. Config, secrets, telemetry, captured traces,
trace packages, logs, and backups are retained by default. Baseten CLI
credentials and keychain entries are never removed.

  --dry-run  Print every intended action without changing anything
  --purge    Also remove ~/.config/baseten-switch permanently
  --yes      Explicitly confirm --purge

The macOS app owns its SMAppService login item. If the CLI cannot unregister
that item safely, uninstall prints the required manual action and leaves the
app bundle in place.
`},
	{"status", "Show aggregate component, auth, and client routing health", `Usage: baseten-switch status [--verbose|-v]

Print router, door, menubar, authentication, and per-client switch state.
Exit 0 only when every managed component is up.

  --verbose, -v  Add launchd labels, binary and config paths, admin address,
                 menubar bundle path, and logs
`},
	{"restart", "Stop and start the system using the same config", `Usage: baseten-switch restart

Run down, then up with the same config. The start half is skipped if shutdown
does not complete cleanly.
`},
	{"on", "Enable global Baseten routing", `Usage: baseten-switch on [mutation options]

Enable routing by editing only global.routing_enabled. Client arguments are
rejected. Machine callers may place these options before or after the verb:

  --json
  --operation-id ID
  --if-active-token TOKEN
  --if-config-hash HASH
`},
	{"off", "Disable global Baseten routing", `Usage: baseten-switch off [mutation options]

Disable routing by editing only global.routing_enabled. Client arguments are
rejected. Machine callers may place these options before or after the verb:

  --json
  --operation-id ID
  --if-active-token TOKEN
  --if-config-hash HASH
`},
	{"mutation", "Inspect or recover a routing policy mutation", `Usage:
  baseten-switch mutation status [--json]
  baseten-switch mutation recover [--json]
  baseten-switch mutation reconcile <operation-id> [--json]

status classifies local recovery state without changing config or routing.
recover removes only recovery state whose exact outcome is already confirmed.
reconcile may finish or safely roll back the named interrupted mutation.
`},
	{"menubar", "Install or refresh and open the macOS menubar app", `Usage: baseten-switch menubar [--which]

Install or refresh the menubar app in ~/Applications from the Homebrew keg,
then open it. The installed location is Spotlight-indexable.

  --which  Print the baseten-switch binary the menubar app will use
`},
	{"door", "Run the foreground front door", ""},
	{"gateway", "Manage the local router process directly", `Usage: baseten-switch gateway <start|stop|restart|status> [--port INT] [--foreground]

  start               Start the local gateway, daemonized by default
  start --foreground  Run in the foreground, suitable for testing
  stop                Stop the running gateway
  restart             Stop if running, then start with the last config path
  status              Print gateway state as JSON

Start and restart poll health before returning. When BASETEN_SWITCH_CONFIG_PATH is unset,
they reuse the config path recorded by the last gateway run.

Environment:
  BASETEN_SWITCH_CONFIG_PATH       Explicit gateway.yaml path
  BASETEN_SWITCH_GATEWAY_PORT      Router admin port, default 45273
  BASETEN_SWITCH_GATEWAY_PIDFILE   Router pidfile path
  BASETEN_SWITCH_GATEWAY_LOG       Router log path
  BASETEN_SWITCH_GATEWAY_TOKEN     Local gateway token
  BASETEN_BASE_URL      Baseten upstream base URL
  ANTHROPIC_API_BASE_URL Native Anthropic upstream base URL
  BASETEN_API_KEY       Baseten API key override
  ANTHROPIC_API_KEY     Anthropic API key override

Logs append to ~/.config/baseten-switch/logs/{router,door}.log by default. The
gateway binds 127.0.0.1 only.
`},
	{"config", "Initialize or reset gateway configuration", `Usage:
  baseten-switch config init [--force]
  baseten-switch config reset --yes [--preview-root PATH --router-addr HOST:PORT --door-addr HOST:PORT]
  baseten-switch config fallback status
  baseten-switch config fallback 429|5xx on|off [mutation options]
  baseten-switch config fallback model <client> [<native-model>] [mutation options]

init writes the canonical single-port gateway.yaml, refuses to overwrite by
default, and backs up the old file before --force replacement.

reset replaces the entire active config, saves a unique 0600 exact-byte
backup, validates, and hot-reloads a running router. Activation failure
restores and reactivates the prior bytes. The three Preview flags must be
supplied together.

fallback reads or changes the automatic native-provider fallback policy and
the per-client native target used for Baseten-specific model identities.
`},
	{"setup", "Check prerequisites, sign in, and initialize configuration", `Usage: baseten-switch setup

Check for baseten CLI v0.3.0 or newer, verify the current credential and run
"baseten auth login" interactively when needed, then create the default
gateway config if it does not exist. Existing config is never overwritten.

setup does not start daemons or change any coding harness configuration.
`},
	{"spend", "Summarize spend from local telemetry segments", `Usage: baseten-switch spend

Print the spend summary calculated from local telemetry segments.
`},
	{"traces", "Manage disabled-by-default local request and response capture", `Usage:
  baseten-switch traces status
  baseten-switch traces enable --client NAME [--client NAME] [--retention-days N]
  baseten-switch traces disable
  baseten-switch traces package --since TIME --client NAME [options]
  baseten-switch traces decode PACKAGE.zip --output DIR [--yes]
  baseten-switch traces purge --yes [--packages]
  baseten-switch traces cleanup IDENTIFIER

Trace capture is disabled by default. Captured bodies can contain prompts,
responses, reasoning summaries, tool input and output, source code, credentials,
personal data, and regulated data. Switch never uploads trace packages.

Decode validates a trace package, writes exact decoded request and response
bodies plus content-free indexes to a new private directory, and never
overwrites the destination. It does not require a config or running gateway.

Package options:
  --since TIME                 Required RFC 3339 timestamp or Go duration
  --until TIME                 Optional exclusive bound, defaults to command start
  --client NAME                Required and repeatable client selector
  --output PATH                Optional new ZIP destination; never overwritten
  --include-native             Include only high-confidence linked native turns
  --require-native             Fail if requested native enrichment drifts or is empty
  --native-session-selector P  Private mode-0600 explicit native selector file
  --include-codex-archived     Opt in to recognized archived Codex sessions
  --dry-run                    Print content-free counts without creating a ZIP
  --yes                        Acknowledge sensitive content noninteractively
  --allow-unscanned-content    Publish despite explicitly reported scan gaps
  --allow-detected-secrets     Publish despite detected credential categories
`},
	{"whoami", "Show the signed-in Baseten identity and token expiry", `Usage: baseten-switch whoami [--profile NAME] [--host URL] [--refresh]

Print the signed-in email, workspace, token expiry, and profile. Authentication
comes from the credential store written by "baseten auth login".

  --profile NAME  Select a credential profile
  --host URL      Override the Baseten API host
  --refresh       Force a token refresh before the identity lookup
`},
	{"auth", "Re-authenticate with the Baseten CLI", `Usage: baseten-switch auth login

Run "baseten auth login" interactively, SIGHUP the running router so it picks
up the fresh credential, then print the identity. baseten-switch does not write the
credential store itself.
`},
	{"doctor", "Diagnose the full request chain and suggest a concrete fix", `Usage: baseten-switch doctor [--json] [--probe] [--verbose] [--fix] [--yes] [--timeout SEC]

Walk the binary, config, auth, router, door, Claude wiring, Codex wiring,
supervision, telemetry, and optional end-to-end probe checks in request-path
order. Exit 0 when nothing fails, 1 on a failed check, and 2 on usage error.
The diagnosis names the first failure and its concrete fix.

  --json     Emit the check array as JSON
  --probe    Add live 1-token requests through healthy door ports
  --verbose  Expand passing sections
  --fix      Confirm and apply automatable fixes, re-running after each
  --yes      Auto-confirm probe cost and fix prompts
  --timeout  Per-probe HTTP timeout in seconds, default 5

Plain doctor is read-only. --fix cannot be combined with --json.
`},
	{"claude", "Manage Claude Code gateway wiring and model routing", `Usage:
  baseten-switch claude on|off|status
  baseten-switch claude picker status|enable [--dry-run --json] [--convert-replacement-mode]|list|add <slug> [--alias <alias>] [--dry-run --json]|remove <alias>|move <alias> --before <alias>|sync [--convert-replacement-mode]|disable
  baseten-switch claude subagents [<model>|on|inherit]
  baseten-switch claude route [<family> <target|default>]
  baseten-switch claude reasoning baseten <model> off|follow-harness|effort <value>|default

on sets ANTHROPIC_BASE_URL, CLAUDE_CODE_ATTRIBUTION_HEADER, and
ENABLE_TOOL_SEARCH in ~/.claude/settings.json, and backs up prior values. off
restores them exactly when possible, or strips only values proven to have been
written by Switch after drift. start and stop alias on and off.

picker manages the ordered Baseten rows appended to Claude Code's /model
picker. Claude Code 2.1.243 or later is required. Built-in rows remain owned
by Claude Code; remove leaves the routing alias intact.

subagents selects a configured alias, raw Baseten slug, or native model for
Task and sidechain requests. "on" re-enables the kept model. "inherit" leaves
Claude Code's requested model unchanged; "off" is an accepted alias.

route shows family pins or sets fable, opus, sonnet, or haiku to native, a
configured alias, a raw Baseten slug, or default to remove the pin. Config
edits use SIGHUP and live verification, not a restart.

reasoning configures a catalog-validated, Claude Code-only reasoning policy
for the final Baseten model.
`},
	{"codex", "Manage the opt-in Codex gateway profile", `Usage:
  baseten-switch codex on|off|status
  baseten-switch codex route <baseten-slug>
  baseten-switch codex reasoning baseten <model> off|follow-harness|effort <value>|default

on writes the managed $CODEX_HOME/baseten.config.toml overlay and points its
provider at the gateway door. It refuses to modify an existing overlay that is
not the exact managed shape. Opt in per session with "codex --profile baseten";
the user's config.toml is never written.
If the gateway Codex client is parked, on requests consent before enabling it.

off restores the pre-on overlay exactly, deletes one created by baseten-switch, or
strips only gateway-owned content after drift. status reports overlay,
gateway-client, door-port, model, token-stub, and backup state. start and stop
alias on and off.

route changes the Codex client's Baseten default_model with a journaled,
hot-reloaded config mutation. The managed profile stays byte-identical, so
active profiled sessions use the new target without a Codex restart.

reasoning configures a catalog-validated, Codex-only reasoning policy for the
final Baseten model.
`},
	{"pi", "Install the direct Baseten provider for Pi", `Usage:
  baseten-switch pi install
  baseten-switch pi status
  baseten-switch pi uninstall

install adds the direct Baseten provider to Pi using the account-visible model
catalog from "baseten model-api list --output json". Exact model IDs are
enriched with reasoning and text/image input capabilities from models.dev.
Unmatched models use conservative defaults and are reported. The command
requires BASETEN_API_KEY in Pi's environment, but writes only the literal
$BASETEN_API_KEY reference. It does not start the gateway, install services,
open the macOS app, or change any other harness configuration.

status reports whether the managed provider is installed and healthy.
uninstall removes only the managed Baseten provider. The default Pi agent
directory is ~/.pi/agent. A nonempty PI_CODING_AGENT_DIR overrides it and may
begin with ~.
`},
}

const healthzHelp = `Usage: baseten-switch healthz

Request the local gateway health endpoint and print its response.
BASETEN_SWITCH_GATEWAY_PORT selects the local port, default 45273.
`

func commandHelpRequested(args []string) bool {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if commandOptionConsumesValue(args, i) {
			i++
			continue
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

// commandOptionConsumesValue is deliberately command-aware. Several handlers
// ignore irrelevant extra arguments, so treating an option from some other
// command as value-taking here could let a trailing --help reach a mutating
// handler. Only options accepted on the current command path are listed.
func commandOptionConsumesValue(args []string, index int) bool {
	option := args[index]
	mutationValue := option == "--operation-id" ||
		option == "--if-active-token" ||
		option == "--if-config-hash"
	switch args[0] {
	case "on", "off":
		return mutationValue
	case "claude":
		return len(args) > 1 &&
			(args[1] == "route" || args[1] == "subagents" || args[1] == "reasoning") &&
			mutationValue
	case "codex":
		return len(args) > 1 &&
			(args[1] == "route" || args[1] == "reasoning") &&
			mutationValue
	case "gateway":
		return len(args) > 1 &&
			(args[1] == "start" || args[1] == "restart") &&
			option == "--port"
	case "config":
		if len(args) < 2 {
			return false
		}
		switch args[1] {
		case "reset":
			valueOption := option == "--preview-root" ||
				option == "--router-addr" ||
				option == "--door-addr"
			// cmdConfigReset rejects a following double-dash token as a
			// missing value, so it remains available to the help scanner.
			return valueOption &&
				index+1 < len(args) &&
				!strings.HasPrefix(args[index+1], "--")
		case "fallback":
			return mutationValue
		case "preview-snapshot":
			return option == "--source" ||
				option == "--output" ||
				option == "--root" ||
				option == "--router-addr" ||
				option == "--door-addr"
		case "preview-validate":
			return option == "--path" ||
				option == "--root" ||
				option == "--router-addr" ||
				option == "--door-addr"
		}
	case "door":
		return option == "--config" || option == "-config" ||
			option == "--port" || option == "-port" ||
			option == "--cooldown" || option == "-cooldown" ||
			option == "--probe-interval" || option == "-probe-interval" ||
			option == "--anthropic-url" || option == "-anthropic-url" ||
			option == "--openai-url" || option == "-openai-url"
	case "whoami":
		return option == "--profile" || option == "--host"
	case "doctor":
		return option == "--timeout"
	}
	return false
}

func usage() {
	fmt.Fprintln(os.Stderr, "baseten-switch - local rerouting gateway for AI coding harnesses")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Usage: baseten-switch <command> [options]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Commands:")
	for _, entry := range commandHelpEntries {
		fmt.Fprintf(os.Stderr, "  %-14s %s\n", entry.name, entry.summary)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Run 'baseten-switch <command> --help' for detailed help.")
}

func printCommandHelp(out io.Writer, command string) bool {
	if command == "healthz" {
		fmt.Fprint(out, healthzHelp)
		return true
	}
	for _, entry := range commandHelpEntries {
		if entry.name != command {
			continue
		}
		if command == "door" {
			_, _ = doorcli.ParseFlags([]string{"--help"}, out)
			return true
		}
		fmt.Fprint(out, entry.detail)
		return true
	}
	return false
}

func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// gatewayLogPath is the router daemon log. It moved from the old
// O_TRUNC ~/.baseten-gateway.log (which erased history on every restart,
// the lifecycle contract "Logs") to an append-mode file under
// ~/.config/baseten-switch/logs/. BASETEN_SWITCH_GATEWAY_LOG still overrides.
func gatewayLogPath() string {
	return envDefault("BASETEN_SWITCH_GATEWAY_LOG", homeJoin(".config", "baseten-switch", "logs", "router.log"))
}

// doorLogPath is the front-door daemon log (append mode).
func doorLogPath() string {
	return envDefault("BASETEN_SWITCH_DOOR_LOG", homeJoin(".config", "baseten-switch", "logs", "door.log"))
}

func homeJoin(remaining ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		parts := append([]string{os.TempDir()}, remaining...)
		return filepath.Join(parts...)
	}
	parts := append([]string{home}, remaining...)
	return filepath.Join(parts...)
}

func gatewayPortFlag(args []string) (int, bool, error) {
	port := envDefault("BASETEN_SWITCH_GATEWAY_PORT", strconv.Itoa(gateway.DefaultPort))
	foreground := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--foreground":
			foreground = true
		case "--port":
			if i+1 >= len(args) {
				return 0, false, fmt.Errorf("--port requires a value")
			}
			port = args[i+1]
			i++
		default:
			if strings.HasPrefix(args[i], "--port=") {
				port = strings.TrimPrefix(args[i], "--port=")
			}
		}
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return 0, false, fmt.Errorf("invalid port: %s", port)
	}
	return p, foreground, nil
}

func cmdGateway(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch gateway [start|stop|restart|status] [--port INT] [--foreground]")
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "start":
		return cmdGatewayStart(rest)
	case "stop":
		return cmdGatewayStop(rest)
	case "restart":
		return cmdGatewayRestart(rest)
	case "status":
		return cmdGatewayStatus(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown gateway subcommand: %s\n", sub)
		return 2
	}
}

// stickyConfigPath decides whether a gateway start should reuse the config
// path recorded by the last gateway run. explicitEnv is the raw
// BASETEN_SWITCH_CONFIG_PATH
// from the environment: when non-empty it always wins and no override
// happens. Returns the path to inject ("" = none) and a notice for
// stderr ("" = nothing to say).
func stickyConfigPath(explicitEnv, statePath string) (path, notice string) {
	if explicitEnv != "" {
		return "", ""
	}
	b, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ""
		}
		return "", fmt.Sprintf("warning: config-path state file %s unreadable (%v); using default config path", statePath, err)
	}
	saved := strings.TrimSpace(string(b))
	if saved == "" {
		return "", ""
	}
	if st, err := os.Stat(saved); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Sprintf("warning: last gateway run used config %s, which is missing or not a regular file; using default config path", saved)
	}
	f, err := os.Open(saved)
	if err != nil {
		return "", fmt.Sprintf("warning: last gateway run used config %s, which is not readable (%v); using default config path", saved, err)
	}
	f.Close()
	return saved, fmt.Sprintf("reusing config path %s recorded by the last gateway run (BASETEN_SWITCH_CONFIG_PATH not set; export it to override)", saved)
}

func cmdGatewayStart(args []string) int {
	port, foreground, err := gatewayPortFlag(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if p, notice := stickyConfigPath(os.Getenv("BASETEN_SWITCH_CONFIG_PATH"), pidfile.ConfigStatePath(pidfile.Path())); p != "" || notice != "" {
		if notice != "" {
			fmt.Fprintln(os.Stderr, notice)
		}
		if p != "" {
			// Set into the env so both the foreground run and the
			// daemonized child (which inherits our environment) load it.
			os.Setenv("BASETEN_SWITCH_CONFIG_PATH", p)
		}
	}
	cfg := gateway.LoadConfig()
	if isGatewayUp(cfg, port) {
		fmt.Fprintf(os.Stderr, "gateway already running on 127.0.0.1:%d\n", port)
		return 0
	}
	if err := migrateFallbackPolicyBeforeStart(cfg.ConfigPath, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "gateway start: fallback policy config migration failed: %v\n", err)
		return 1
	}
	cfg = gateway.LoadConfig()
	migrateClaudeAttributionBeforeStart(os.Stderr)
	if foreground {
		return runForeground(cfg)
	}
	return daemonize(cfg, port)
}

func runForeground(cfg gateway.Config) int {
	if err := gateway.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] fatal: %v\n", err)
		return 1
	}
	return 0
}

// spawnDetached starts this executable again with args as a detached
// daemon: own process group, stdout+stderr appended to logPath (dirs
// created 0755). extraEnv entries override the inherited environment.
// Shared by the router daemonize path and the up orchestrator (door,
// web). Package var, like launchdRunner and runCmd, so lifecycle tests
// can substitute a fake and never fork the test binary as a daemon.
var spawnDetached = func(logPath string, args []string, extraEnv map[string]string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return 0, fmt.Errorf("could not create log dir: %v", err)
	}
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("could not open log %s: %v", logPath, err)
	}
	defer logF.Close()
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("could not resolve self executable: %v", err)
	}
	cmd := exec.Command(self, args...)
	cmd.Stdin = nil
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.Env = envWith(extraEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("could not start daemon: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// envWith returns os.Environ with the given overrides applied (any
// existing entry for an overridden key is dropped so the override is
// unambiguous regardless of getenv duplicate-key behavior).
func envWith(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return os.Environ()
	}
	out := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if _, ok := overrides[key]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

func daemonize(cfg gateway.Config, port int) int {
	logPath := gatewayLogPath()
	pid, err := spawnDetached(logPath, []string{"gateway", "start", "--foreground", "--port", strconv.Itoa(port)}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := pollHealth(port, 5*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "gateway did not become healthy: %v (see %s)\n", err, logPath)
		return 1
	}
	if pf := pidfile.ReadFromSafe(cfg.PidFile); pf != 0 {
		pid = pf
	}
	fmt.Fprintf(os.Stderr, "gateway up (pid %d) on 127.0.0.1:%d (log: %s)\n", pid, port, logPath)
	return 0
}

func pollHealth(port int, dur time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("no healthy response from %s within %s", url, dur)
}

func isGatewayUp(cfg gateway.Config, port int) bool {
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return resp.StatusCode == 200
}

// pidfileState classifies the pidfile ahead of a stop or restart so the
// decision logic is testable without spawning processes.
type pidfileState int

const (
	pidfileMissing pidfileState = iota
	pidfileCorrupt
	pidfileDead
	pidfileAlive
)

func classifyPidfile(pf string) (pidfileState, int) {
	if _, err := os.Stat(pf); err != nil {
		return pidfileMissing, 0
	}
	pid, err := pidfile.ReadFrom(pf)
	if err != nil {
		return pidfileCorrupt, 0
	}
	if !pidfile.IsAlive(pid) {
		return pidfileDead, pid
	}
	return pidfileAlive, pid
}

// terminateGateway sends SIGTERM to pid, waits up to 3s, escalates to
// SIGKILL and waits up to 1s more. Returns an error only if the
// process is still alive afterwards.
func terminateGateway(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "sigterm error: %v\n", err)
	}
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if !pidfile.IsAlive(pid) {
			return nil
		}
	}
	fmt.Fprintf(os.Stderr, "gateway pid %d did not exit cleanly; sending SIGKILL.\n", pid)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for i := 0; i < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		if !pidfile.IsAlive(pid) {
			return nil
		}
	}
	return fmt.Errorf("gateway pid %d still alive after SIGKILL", pid)
}

func cmdGatewayStop(args []string) int {
	pf := gatewayPidfilePath()
	switch state, pid := classifyPidfile(pf); state {
	case pidfileMissing:
		fmt.Fprintln(os.Stderr, "gateway not running (no pidfile).")
		return 0
	case pidfileCorrupt:
		os.Remove(pf)
		fmt.Fprintln(os.Stderr, "pidfile corrupt; removed.")
		return 1
	case pidfileDead:
		os.Remove(pf)
		fmt.Fprintf(os.Stderr, "gateway pid %d not running; removing pidfile.\n", pid)
		return 0
	default:
		err := terminateGateway(pid)
		os.Remove(pf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stop failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "gateway stopped.")
		return 0
	}
}

// cmdGatewayRestart stops the running gateway (if any), then starts a
// new one via the same code path as `gateway start`, so the sticky
// config path (F2) applies and health is polled before returning.
func cmdGatewayRestart(args []string) int {
	pf := gatewayPidfilePath()
	switch state, pid := classifyPidfile(pf); state {
	case pidfileMissing:
		fmt.Fprintln(os.Stderr, "gateway not running; starting fresh.")
	case pidfileCorrupt:
		os.Remove(pf)
		fmt.Fprintln(os.Stderr, "pidfile corrupt; removed. starting fresh.")
	case pidfileDead:
		os.Remove(pf)
		fmt.Fprintf(os.Stderr, "gateway pid %d not running; removed stale pidfile. starting fresh.\n", pid)
	default:
		fmt.Fprintf(os.Stderr, "stopping gateway (pid %d)...\n", pid)
		err := terminateGateway(pid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "restart aborted: %v\n", err)
			return 1
		}
		os.Remove(pf)
		fmt.Fprintln(os.Stderr, "gateway stopped.")
	}
	return cmdGatewayStart(args)
}

func cmdGatewayStatus(args []string) int {
	pf := gatewayPidfilePath()
	port := envDefault("BASETEN_SWITCH_GATEWAY_PORT", strconv.Itoa(gateway.DefaultPort))
	portI, err := strconv.Atoi(port)
	if err != nil {
		portI = gateway.DefaultPort
	}
	healthy := false
	if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", portI)); err == nil {
		var h map[string]string
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		_ = json.Unmarshal(body, &h)
		if resp.StatusCode == 200 {
			healthy = true
		}
	}
	pid := 0
	alive := false
	if p, err := pidfile.ReadFrom(pf); err == nil {
		pid = p
		alive = pidfile.IsAlive(p)
	}
	running := alive && healthy
	out := map[string]interface{}{
		"running":                running,
		"host":                   fmt.Sprintf("127.0.0.1:%d", portI),
		"upstream_for_baseten":   envDefault("BASETEN_BASE_URL", gateway.DefaultBasetenURL),
		"upstream_for_anthropic": envDefault("ANTHROPIC_API_BASE_URL", gateway.DefaultAnthropicURL),
	}
	if running {
		out["pid"] = pid
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return 0
}

func gatewayPidfilePath() string {
	return pidfile.Path()
}

func cmdHealthz(args []string) int {
	port := envDefault("BASETEN_SWITCH_GATEWAY_PORT", strconv.Itoa(gateway.DefaultPort))
	url := "http://127.0.0.1:" + port + "/"
	// 1.5s timeout to keep the menu bar refresh snappy.
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthz error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	b, _ := ioutil.ReadAll(resp.Body)
	fmt.Println(string(b))
	return 0
}

type spendRow struct {
	Day             string  `json:"day"`
	Route           string  `json:"route"`
	Model           string  `json:"model"`
	Requests        int     `json:"requests"`
	Input           int64   `json:"input"`
	Output          int64   `json:"output"`
	CacheRead       int64   `json:"cache_read"`
	CacheWrite5m    int64   `json:"cache_write_5m"`
	CacheWrite1h    int64   `json:"cache_write_1h"`
	CacheWriteTotal int64   `json:"cache_write_total"`
	CostUSD         float64 `json:"cost_usd"`
	Errors          int     `json:"errors"`
}

func cmdSpend(args []string) int {
	_ = args
	telemetryDir := config.DefaultTelemetryDir()
	configPath, _ := resolveConfigPath()
	if file, err := config.Load(configPath); err == nil && file.Global.TelemetryDir != "" {
		telemetryDir = config.ExpandPath(file.Global.TelemetryDir)
	}
	events, err := telemetry.ReadEvents(telemetryDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: telemetry history is partial: %v\n", err)
		if len(events) == 0 {
			return 1
		}
	}
	type key struct{ day, route, model string }
	acc := map[key]*spendRow{}
	var order []key
	for _, event := range events {
		day := "0000-00-00"
		if !event.CompletedAt.IsZero() {
			day = event.CompletedAt.UTC().Format("2006-01-02")
		}
		model := event.ServedModel
		if model == "" {
			model = event.RequestedModel
		}
		if model == "" {
			model = "?"
		}
		// Fallback-served requests are attributed to the route that
		// actually served them, marked "(fb)", matching native analytics.
		eff := event.EffectiveProvider
		if eff == "" {
			eff = event.ConfiguredRoute
		}
		if event.Fallback.Attempted {
			eff += " (fb)"
		}
		k := key{day, eff, model}
		cell, ok := acc[k]
		if !ok {
			cell = &spendRow{Day: day, Route: eff, Model: model}
			acc[k] = cell
			order = append(order, k)
		}
		cell.Requests++
		cell.Input += tokenValue(event.Usage.InputTokens)
		cell.Output += tokenValue(event.Usage.OutputTokens)
		cell.CacheRead += tokenValue(event.Usage.CacheReadInputTokens)
		cell.CacheWrite5m += tokenValue(event.Usage.CacheWrite5mInputTokens)
		cell.CacheWrite1h += tokenValue(event.Usage.CacheWrite1hInputTokens)
		cell.CacheWriteTotal += tokenValue(
			event.Usage.CacheWriteTotalInputTokens,
		)
		if event.ActualCost.Priced && event.ActualCost.NanoUSD != nil {
			cell.CostUSD += float64(*event.ActualCost.NanoUSD) / 1_000_000_000
		}
		if event.IsHTTPError() {
			cell.Errors++
		}
	}
	if len(order) == 0 {
		fmt.Println("(no requests logged)")
		return 0
	}
	fmt.Printf("%-11s %-15s %-24s %5s %8s %8s %8s %8s %8s %8s %9s %4s\n",
		"day", "route", "model", "req", "in", "out", "cache_r",
		"cache_5m", "cache_1h", "cache_tot", "cost$", "err")
	fmt.Println(strings.Repeat("-", 126))
	for _, k := range order {
		c := acc[k]
		fmt.Printf("%-11s %-15s %-24s %5d %8d %8d %8d %8d %8d %8d %9.6f %4d\n",
			c.Day, c.Route, c.Model, c.Requests, c.Input, c.Output,
			c.CacheRead, c.CacheWrite5m, c.CacheWrite1h,
			c.CacheWriteTotal, c.CostUSD, c.Errors)
	}
	return 0
}

func tokenValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
