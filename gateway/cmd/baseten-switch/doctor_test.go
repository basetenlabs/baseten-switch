package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

// All doctor tests are hermetic: every path and address goes through
// the documented env seams (BASETEN_SWITCH_CONFIG_PATH, BASETEN_SWITCH_ADMIN_ADDR,
// BASETEN_SWITCH_CLAUDE_SETTINGS, BASETEN_SWITCH_BACKUP_DIR, BASETEN_SWITCH_AUTH_FILE,
// telemetry_dir, BASETEN_SWITCH_LAUNCHD=off, ...) so the real
// ~/.claude/settings.json, ~/.config/baseten-switch, launchd domain, and
// credential store are never read or written, and no running gateway
// is ever contacted.

// doctorFixtureCfg selects how the fake chain misbehaves; the zero
// value is the all-green chain.
type doctorFixtureCfg struct {
	adminDown        bool              // nothing listens on the admin addr
	adminForeign     bool              // a non-router process owns the admin addr
	doorRouter       string            // /doorz-reported router addr ("" = the bound client listener)
	signedIn         bool              // set by newDoctorFixture default; false = empty auth store
	globalAuth       string            // extra global.auth yaml block ("" = none)
	settingsJSON     string            // claude settings content ("" = pointing at the door)
	subagentModel    string            // sets subagent_model on the claude-code client in gateway.yaml
	subagentRouting  string            // sets subagent_routing on the claude-code client ("" = omitted)
	subagentEnvModel string            // adds CLAUDE_CODE_SUBAGENT_MODEL to the settings env block
	modelAliases     map[string]string // model_aliases on the claude-code client (nil = none)
	clientRoute      string            // admin-reported route ("" = baseten)
	fallbackRoute    string            // admin-reported fallback_route ("" = none)
	// telRows writes synthetic telemetry rows: each is (ts, subagent).
	// ts is seconds offset from now (negative = past).
	telRows []telRowSpec
	// doorDown points the door spec at a closed port instead of the
	// fake door server, so the door checks see "down".
	doorDown bool
	// menubarVersion is the version the fake menubar binary prints
	// via --version. The default (empty) writes a script that prints
	// the test binary's own version.Version so the green chain matches.
	// Set to a different string to exercise the mismatch warn.
	menubarVersion string
	// noMenubarBinary suppresses the menubar binary entirely (no
	// BASETEN_SWITCH_GATEWAY_BIN, no brew paths) so the check skips.
	noMenubarBinary bool
	// modelRoutes sets model_routes on the claude-code client in
	// gateway.yaml (nil = none). Used by the doctor model_routes check.
	modelRoutes map[string]string
	// modelEnvVars adds harness env vars (ANTHROPIC_MODEL,
	// ANTHROPIC_DEFAULT_*_MODEL) to the settings env block. Used by the
	// doctor model_env check (the fireconnect survey hazard).
	modelEnvVars map[string]string
	// authHealth sets the health field the fake admin serves at
	// /v1/admin/auth/status ("-" omits the required field).
	authHealth string
	// apiKeyProfile makes the selected local profile a readable API-key
	// profile. routerSignedOut independently models a stale router that has
	// not loaded that credential.
	apiKeyProfile   bool
	routerSignedOut bool
	// authLastError sets last_refresh_error alongside authHealth.
	authLastError string
	// basetenVersions writes one fake baseten CLI per entry (each in
	// its own fake PATH dir printing the entry for --version). nil = a
	// single "baseten 0.2.0" so the green chain sees one healthy CLI.
	basetenVersions []string
	// noBasetenCLI empties the fake PATH so the two-CLI check skips.
	noBasetenCLI bool
	// doorProbe makes the fake door answer probe POSTs, stamping
	// X-Baseten-Switch-Door with this value ("router" or "fallback"; "none"
	// answers without the required header). "" leaves the door /doorz-only
	// (non-probe tests unaffected).
	doorProbe string
	// probeTelRoute/probeTelEffective: when doorProbe is set and
	// probeTelRoute is non-empty, the fake door appends a telemetry row
	// for the probe (route / route_effective, carrying the probe's
	// client, requested model, and status), emulating the router's
	// write. Empty probeTelRoute = no row (door-fallback case, or the
	// no-row skip case).
	probeTelRoute        string
	probeTelEffective    string
	probeFallbackTrigger string
	// probeStatus, when >= 400, makes the fake door answer the probe with
	// that status and probeError as the body instead of the 200 success
	// payload (for example the native provider's auth rejection for the
	// credential-less probe). 0 means 200.
	probeStatus int
	probeError  string
	// probeConcurrentModel, when non-empty, makes the fake door append a
	// second v1 event AFTER the probe's event: a concurrent harness
	// request completing just behind the probe, the misattribution
	// hazard for the served-route check.
	probeConcurrentModel string
	// routingOff writes routing_enabled: false so the configured route
	// for every model is the native one.
	routingOff bool
	// codexClient adds an enabled openai-shape codex client sharing the
	// claude listener (the real shared-listener topology) so the doctor
	// codex section resolves instead of skipping.
	codexClient bool
}

// telRowSpec is one synthetic telemetry row for the windowed traffic
// check: TS is a unix timestamp, Subagent is the subagent flag.
type telRowSpec struct {
	TS       int64
	Subagent bool
}

func doctorTestTelemetryEvent(
	completedAt time.Time,
	sequence int,
	client string,
	configuredRoute string,
	effectiveProvider string,
	requestedModel string,
	status int,
	subagent bool,
) telemetry.EventV1 {
	if effectiveProvider == "" {
		effectiveProvider = configuredRoute
	}
	statusValue := status
	termination := telemetry.TerminationCompleted
	if status >= http.StatusBadRequest {
		termination = telemetry.TerminationUpstreamHTTPError
	}
	return telemetry.EventV1{
		SchemaVersion:     telemetry.SchemaVersionV1,
		Event:             telemetry.EventRequest,
		EventID:           fmt.Sprintf("%032x", completedAt.UnixNano()+int64(sequence)),
		StartedAt:         completedAt.Add(-time.Second),
		CompletedAt:       completedAt,
		Client:            client,
		ConfiguredRoute:   configuredRoute,
		EffectiveProvider: effectiveProvider,
		RequestedModel:    requestedModel,
		ServedModel:       requestedModel,
		Status:            &statusValue,
		DurationMS:        1000,
		TerminationReason: termination,
		ActualCost:        telemetry.CostSnapshotV1{},
		Fallback:          telemetry.FallbackV1{},
		Subagent:          subagent,
		StrippedToolTypes: []string{},
	}
}

func writeDoctorTelemetryEvents(dir string, events ...telemetry.EventV1) error {
	writer, err := telemetry.NewWriter(telemetry.WriterOptions{Dir: dir})
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := writer.Write(event); err != nil {
			_ = writer.Close()
			return err
		}
	}
	return writer.Close()
}

type doctorFixture struct {
	clientAddr string
	doorAddr   string
	doorPort   string
	cfgPath    string
	settings   string
	codexHome  string
}

// startClientListener opens a listener that stays bound for the test:
// the doctor's per-client TCP check only needs the handshake.
func startClientListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func newDoctorFixture(t *testing.T, mut func(*doctorFixtureCfg)) *doctorFixture {
	t.Helper()
	cfg := doctorFixtureCfg{signedIn: true, authHealth: "ok"}
	if mut != nil {
		mut(&cfg)
	}
	dir := t.TempDir()
	fx := &doctorFixture{clientAddr: startClientListener(t)}
	// Resolved up front: the fake door's probe handler (below) appends
	// the probe's telemetry v1 event here, emulating the router's write.
	telDir := filepath.Join(dir, "telemetry")

	// Admin (router) endpoint.
	switch {
	case cfg.adminDown:
		t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", closedPortAddr(t))
	case cfg.adminForeign:
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)
		t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", hostPort(t, srv.URL))
	default:
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"ok":true,"uptime_seconds":42,"version":%q}`, version.Version)
		})
		clientRoute := cfg.clientRoute
		if clientRoute == "" {
			clientRoute = "baseten"
		}
		mux.HandleFunc("/v1/admin/status", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"uptime_seconds":42,"version":%q,
				"auth":{"signed_in":true,"profile":"doc","fallback_enabled":false,"fallback_in_use":false},
				"clients":[{"name":"claude-code","enabled":true,"bind_addr":%q,"protocol_shape":"anthropic",
					"effective_route":%q,"fallback_route":%q,"auth_set":true,"currently_bound":true}]}`,
				version.Version, fx.clientAddr, clientRoute, cfg.fallbackRoute)
		})
		mux.HandleFunc("/v1/admin/auth/status", func(w http.ResponseWriter, r *http.Request) {
			healthPart := ""
			if cfg.authHealth != "-" {
				// json.Marshal, not %q: the error text is server
				// controlled and may hold bytes Go escapes in ways JSON
				// does not accept (\x..), exactly like the real admin
				// endpoint's encoder handles them.
				errJSON, _ := json.Marshal(cfg.authLastError)
				healthPart = fmt.Sprintf(`"health":%q,"last_refresh_error":%s,"last_refresh_error_at":"2026-07-13T09:55:00Z",`,
					cfg.authHealth, errJSON)
			}
			signedIn := !cfg.routerSignedOut
			fmt.Fprintf(w, `{"signed_in":%t,%s"profile":"doc","fallback_enabled":false,"fallback_in_use":false,"email":"doc@example.com"}`, signedIn, healthPart)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", hostPort(t, srv.URL))
	}

	// Door endpoint.
	doorRouter := cfg.doorRouter
	if doorRouter == "" {
		doorRouter = fx.clientAddr
	}
	if cfg.doorDown {
		// Point the door spec at a closed port; no door server started.
		fx.doorAddr = closedPortAddr(t)
		fx.doorPort = portOf(fx.doorAddr)
	} else {
		doorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/doorz" {
				fmt.Fprintf(w, `{"port":0,"shape":"anthropic","router":%q,"tripped":false,"cooldown_remaining_ms":0,"version":%q}`,
					doorRouter, version.Version)
				return
			}
			if cfg.doorProbe != "" && r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
				// Emulate the router's telemetry write for the probe,
				// then answer with the door's serving marker header.
				probeStatus := cfg.probeStatus
				if probeStatus == 0 {
					probeStatus = http.StatusOK
				}
				if cfg.probeTelRoute != "" {
					completedAt := time.Now()
					probeEvent := doctorTestTelemetryEvent(
						completedAt,
						1,
						"claude-code",
						cfg.probeTelRoute,
						cfg.probeTelEffective,
						doctorProbeRequestModel("anthropic"),
						probeStatus,
						false,
					)
					if cfg.probeFallbackTrigger != "" {
						trigger := cfg.probeFallbackTrigger
						probeEvent.Fallback = telemetry.FallbackV1{Attempted: true, Count: 1, Trigger: &trigger}
					}
					events := []telemetry.EventV1{probeEvent}
					if cfg.probeConcurrentModel != "" {
						events = append(events, doctorTestTelemetryEvent(
							completedAt.Add(time.Nanosecond),
							2,
							"claude-code",
							"baseten",
							"anthropic",
							cfg.probeConcurrentModel,
							http.StatusOK,
							false,
						))
					}
					if err := writeDoctorTelemetryEvents(telDir, events...); err != nil {
						t.Errorf("write probe telemetry: %v", err)
					}
				}
				if cfg.doorProbe != "none" {
					w.Header().Set("X-Baseten-Switch-Door", cfg.doorProbe)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(probeStatus)
				if probeStatus >= http.StatusBadRequest {
					fmt.Fprint(w, cfg.probeError)
				} else {
					fmt.Fprint(w, `{"model":"probe-model"}`)
				}
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(doorSrv.Close)
		fx.doorAddr = hostPort(t, doorSrv.URL)
		fx.doorPort = portOf(fx.doorAddr)
	}

	// Config.
	comment := "# doctor test config\n"
	routingEnabled := "true"
	if cfg.routingOff {
		routingEnabled = "false"
	}
	globalBlock := fmt.Sprintf(
		"global:\n  routing_enabled: %s\n  telemetry_dir: %q\n",
		routingEnabled,
		telDir,
	)
	if cfg.globalAuth != "" {
		globalBlock += "  auth:\n    baseten: " + cfg.globalAuth + "\n"
	}
	aliasBlock := ""
	if len(cfg.modelAliases) > 0 {
		aliasBlock = "    model_aliases:\n"
		for _, id := range sortedAliasIDs(cfg.modelAliases) {
			aliasBlock += fmt.Sprintf("      %s: %s\n", id, cfg.modelAliases[id])
		}
	}
	subagentBlock := ""
	if cfg.subagentModel != "" {
		subagentBlock = fmt.Sprintf("    subagent_model: %s\n", cfg.subagentModel)
	}
	if cfg.subagentRouting != "" {
		subagentBlock += fmt.Sprintf("    subagent_routing: %s\n", cfg.subagentRouting)
	}
	routesBlock := ""
	if len(cfg.modelRoutes) > 0 {
		routesBlock = "    model_routes:\n"
		for _, k := range sortedRouteKeys(cfg.modelRoutes) {
			routesBlock += fmt.Sprintf("      %s: %s\n", k, cfg.modelRoutes[k])
		}
	}
	codexBlock := ""
	if cfg.codexClient {
		codexBlock = fmt.Sprintf(`  - name: codex
    enabled: true
    bind_addr: %s
    protocol_shape: openai
    default_model: zai-org/GLM-5.2
    responses_strip_tool_types: []
`, fx.clientAddr)
	}
	fx.cfgPath = filepath.Join(dir, "gateway.yaml")
	yaml := comment + globalBlock + fmt.Sprintf(`clients:
  - name: claude-code
    enabled: true
    bind_addr: %s
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
%s%s%s%sdoor:
  ports:
    - bind_addr: %s
      router_addr: %s
`, fx.clientAddr, aliasBlock, subagentBlock, routesBlock, codexBlock, fx.doorAddr, fx.clientAddr)
	if err := os.WriteFile(fx.cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", fx.cfgPath)

	// Claude settings pointing at the door, plus an intact backup so
	// the green run has no warns.
	fx.settings = filepath.Join(dir, "settings.json")
	settingsJSON := cfg.settingsJSON
	if settingsJSON == "" {
		subagentEnvPart := ""
		if cfg.subagentEnvModel != "" {
			subagentEnvPart = fmt.Sprintf(`,"CLAUDE_CODE_SUBAGENT_MODEL":%q`, cfg.subagentEnvModel)
		}
		modelEnvPart := ""
		for _, k := range routeEnvSlotKeys() {
			if v, ok := cfg.modelEnvVars[k]; ok && v != "" {
				modelEnvPart += fmt.Sprintf(`,%q:%q`, k, v)
			}
		}
		settingsJSON = fmt.Sprintf(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:%s","CLAUDE_CODE_ATTRIBUTION_HEADER":"0","ENABLE_TOOL_SEARCH":"true"%s%s}}`, fx.doorPort, subagentEnvPart, modelEnvPart)
	}
	if err := os.WriteFile(fx.settings, []byte(settingsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CLAUDE_SETTINGS", fx.settings)
	backupRoot := filepath.Join(dir, "backups")
	t.Setenv("BASETEN_SWITCH_BACKUP_DIR", backupRoot)
	if err := saveClaudeBackup(claudeBackupPath(backupRoot, fx.settings), &claudeBackup{
		ConfigPath:  fx.settings,
		Values:      map[string]string{claudeManagedEnvKey: "https://corp-proxy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex([]byte(settingsJSON)),
	}); err != nil {
		t.Fatal(err)
	}

	// Auth store.
	if cfg.apiKeyProfile {
		writeAuthJSON(t, `{
  "version": 1,
  "current": "example-profile",
  "profiles": {
    "example-profile": {
      "remote_url": "https://api.example.com",
      "auth_type": "api_key",
      "api_key": "synthetic-doctor-key"
    }
  }
}`)
	} else if cfg.signedIn {
		expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		writeAuthJSON(t, fmt.Sprintf(`{
  "version": 1,
  "current": "doc@example.com",
  "profiles": {
    "doc@example.com": {
      "remote_url": "https://app.example.com",
      "auth_type": "oauth",
      "oauth_credential": {"access_token": "at", "refresh_token": "rt", "expiry": %q}
    }
  }
}`, expiry))
	} else {
		writeAuthJSON(t, `{"version":1,"profiles":{}}`)
	}

	// Telemetry v1 store. Default: one fresh request event (no subagent).
	// When cfg.telRows is set, write those events instead.
	var events []telemetry.EventV1
	if len(cfg.telRows) > 0 {
		for index, row := range cfg.telRows {
			events = append(events, doctorTestTelemetryEvent(
				time.Unix(row.TS, 0),
				index+1,
				"claude-code",
				"baseten",
				"baseten",
				"",
				http.StatusOK,
				row.Subagent,
			))
		}
	} else {
		events = append(events, doctorTestTelemetryEvent(
			time.Now(),
			1,
			"claude-code",
			"baseten",
			"baseten",
			"",
			http.StatusOK,
			false,
		))
	}
	if err := writeDoctorTelemetryEvents(telDir, events...); err != nil {
		t.Fatal(err)
	}

	// Menubar binary: write a fake executable script to a temp dir and
	// point BASETEN_SWITCH_GATEWAY_BIN at it, and swap the brew paths to temp dirs
	// so no real host binary is ever probed or exec'd.
	if !cfg.noMenubarBinary {
		mbVer := cfg.menubarVersion
		if mbVer == "" {
			mbVer = version.Version
		}
		mbBin := filepath.Join(dir, "mb-bin", "baseten-switch")
		if err := os.MkdirAll(filepath.Dir(mbBin), 0o755); err != nil {
			t.Fatal(err)
		}
		// A shell script that prints "baseten-switch <version>" for --version.
		// chmod +x so isExecutable and os/exec accept it.
		script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"baseten-switch %s\"; fi\n", mbVer)
		if err := os.WriteFile(mbBin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BASETEN_SWITCH_GATEWAY_BIN", mbBin)
	} else {
		t.Setenv("BASETEN_SWITCH_GATEWAY_BIN", "")
		os.Unsetenv("BASETEN_SWITCH_GATEWAY_BIN")
	}
	// Swap brew paths to nonexistent temp dirs so the resolution
	// function never touches a real host brew install.
	oldBrew := menubarBrewPaths
	menubarBrewPaths = []string{
		filepath.Join(dir, "nope-brew-arm", "baseten-switch"),
		filepath.Join(dir, "nope-brew-intel", "baseten-switch"),
	}
	t.Cleanup(func() { menubarBrewPaths = oldBrew })
	// Same for the ~/.local/bin candidate: a real host install there
	// A development copy must never be probed or exec'd by tests.
	oldLocal := menubarLocalBin
	menubarLocalBin = func() string { return filepath.Join(dir, "nope-local", "baseten-switch") }
	t.Cleanup(func() { menubarLocalBin = oldLocal })

	// Baseten CLI scan: fake PATH dirs with scripted binaries, swapped
	// in via the basetenPATH seam, so the two-CLI check never scans the
	// host PATH or execs a real baseten install.
	cliVersions := cfg.basetenVersions
	if cliVersions == nil && !cfg.noBasetenCLI {
		cliVersions = []string{"baseten 0.2.0"}
	}
	var cliDirs []string
	for i, v := range cliVersions {
		d := filepath.Join(dir, fmt.Sprintf("baseten-cli-%d", i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		script := fmt.Sprintf("#!/bin/sh\necho %q\n", v)
		if err := os.WriteFile(filepath.Join(d, "baseten"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		cliDirs = append(cliDirs, d)
	}
	oldBasetenPATH := basetenPATH
	basetenPATH = func() string { return strings.Join(cliDirs, string(os.PathListSeparator)) }
	t.Cleanup(func() { basetenPATH = oldBasetenPATH })

	// The fake door writes the probe's telemetry row before responding,
	// so the row is always readable immediately; shrink the row wait so
	// the deliberate no-row skip cases do not stall the suite.
	oldRowWait := probeRowWait
	probeRowWait = 100 * time.Millisecond
	t.Cleanup(func() { probeRowWait = oldRowWait })

	// Remaining seams: nothing may leak to real state or a real shell.
	// BASETEN_SWITCH_CODEX_HOME always points into the temp dir so the codex
	// section can never peek at a real ~/.codex, even read-only.
	fx.codexHome = filepath.Join(dir, "codex-home")
	t.Setenv("BASETEN_SWITCH_CODEX_HOME", fx.codexHome)
	t.Setenv(codexManagedEnvKey, "")
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(dir, "gw.pid"))
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(dir, "door.pid"))
	t.Setenv("BASETEN_SWITCH_ENV_FILE", filepath.Join(dir, "env"))
	t.Setenv("BASETEN_SWITCH_LAUNCHD", "off")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("BASETEN_API_KEY", "")
	t.Setenv(claudeSubagentEnvKey, "")
	// Clear the harness env slots the model_env check scans for, so the
	// green run does not pick up a real shell value.
	for _, k := range routeEnvSlotKeys() {
		t.Setenv(k, "")
	}
	return fx
}

func findCheck(t *testing.T, rep doctorReport, section, name string) doctorCheck {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Section == section && c.Name == name {
			return c
		}
	}
	t.Fatalf("check %s/%s not in report: %+v", section, name, rep.Checks)
	return doctorCheck{}
}

func TestDoctorAllGreen(t *testing.T) {
	fx := newDoctorFixture(t, nil)
	rep := runDoctor(doctorOpts{})
	if rep.ExitCode != 0 || rep.FirstFailure != "" {
		t.Fatalf("exit = %d first_failure = %q\nchecks: %+v", rep.ExitCode, rep.FirstFailure, rep.Checks)
	}
	for _, c := range rep.Checks {
		if c.Status == docFail || c.Status == docWarn {
			t.Errorf("green run has %s: %s/%s: %s", c.Status, c.Section, c.Name, c.Finding)
		}
	}
	for _, want := range [][2]string{
		{"config", "load"}, {"auth", "signin"}, {"router", "client:claude-code"},
		{"door", "doorz:" + fx.doorPort}, {"door", "wiring:" + fx.doorPort},
		{"claude", "base_url"}, {"claude", "attribution_header"}, {"claude", "tool_search"},
		{"claude", "subagents"}, {"claude", "subagent_env"},
		{"claude", "model_routes"}, {"claude", "model_env"},
		{"claude", "backup"}, {"telemetry", "log"}, {"telemetry", "freshness"}, {"telemetry", "logs"},
	} {
		if c := findCheck(t, rep, want[0], want[1]); c.Status != docOK {
			t.Errorf("%s/%s = %s (%s), want ok", want[0], want[1], c.Status, c.Finding)
		}
	}
	// The telemetry/logs ok row names both daemon log paths.
	logsCheck := findCheck(t, rep, "telemetry", "logs")
	if !strings.Contains(logsCheck.Finding, "router") || !strings.Contains(logsCheck.Finding, "door") {
		t.Errorf("telemetry/logs finding does not name both daemon log paths: %s", logsCheck.Finding)
	}
	// subagent_traffic is skip when routing is not enabled (no subagent_model).
	if c := findCheck(t, rep, "claude", "subagent_traffic"); c.Status != docSkip {
		t.Errorf("subagent_traffic = %s, want skip (no subagent_model)", c.Status)
	}
	if c := findCheck(t, rep, "e2e", "probe"); c.Status != docSkip || !strings.Contains(c.Finding, "--probe") {
		t.Errorf("e2e without --probe = %+v, want skip pointing at --probe", c)
	}
	if c := findCheck(t, rep, "supervision", "launchd"); c.Status != docSkip {
		t.Errorf("supervision under BASETEN_SWITCH_LAUNCHD=off = %+v, want skip", c)
	}
	var buf bytes.Buffer
	printDoctorReport(&buf, rep, false)
	if !strings.Contains(buf.String(), "all checks passed") {
		t.Errorf("summary line missing:\n%s", buf.String())
	}
}

func TestDoctorClaudeMissingBaseURL(t *testing.T) {
	newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.settingsJSON = `{"permissions":{"allow":[]}}`
	})
	rep := runDoctor(doctorOpts{})
	if rep.ExitCode != 1 {
		t.Fatalf("exit = %d, want 1", rep.ExitCode)
	}
	if rep.FirstFailure != "claude/base_url" {
		t.Fatalf("first_failure = %q, want claude/base_url", rep.FirstFailure)
	}
	c := findCheck(t, rep, "claude", "base_url")
	if c.Status != docFail || !strings.Contains(c.Finding, "direct to Anthropic") {
		t.Errorf("base_url = %+v, want fail naming the direct-to-Anthropic state", c)
	}
	if c.Fix != "baseten-switch claude on" {
		t.Errorf("fix = %q, want 'baseten-switch claude on'", c.Fix)
	}
	var buf bytes.Buffer
	printDoctorReport(&buf, rep, false)
	if !strings.Contains(buf.String(), "DIAGNOSIS") || !strings.Contains(buf.String(), "baseten-switch claude on") {
		t.Errorf("DIAGNOSIS paragraph missing or without the fix:\n%s", buf.String())
	}
}

func TestDoctorClaudeWrongPort(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.settingsJSON = `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1"}}`
	})
	rep := runDoctor(doctorOpts{})
	c := findCheck(t, rep, "claude", "base_url")
	if c.Status != docFail {
		t.Fatalf("base_url = %+v, want fail", c)
	}
	if !strings.Contains(c.Finding, "points at :1") || !strings.Contains(c.Finding, "door listens on :"+fx.doorPort) {
		t.Errorf("finding %q must name both ports", c.Finding)
	}
	if rep.FirstFailure != "claude/base_url" {
		t.Errorf("first_failure = %q", rep.FirstFailure)
	}
}

func TestDoctorClaudeManagedEnvKnobsRequireExactValues(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		fx := newDoctorFixture(t, nil)
		root := readTree(t, fx.settings)
		env := root["env"].(map[string]any)
		delete(env, claudeAttributionEnvKey)
		delete(env, claudeToolSearchEnvKey)
		b, _ := json.Marshal(root)
		if err := os.WriteFile(fx.settings, b, 0o600); err != nil {
			t.Fatal(err)
		}
		rep := runDoctor(doctorOpts{})
		for _, name := range []string{"attribution_header", "tool_search"} {
			c := findCheck(t, rep, "claude", name)
			if c.Status != docFail || !strings.Contains(c.Finding, "missing") {
				t.Errorf("%s = %+v, want missing failure", name, c)
			}
			if len(c.fixArgv) != 2 || c.fixArgv[0] != "claude" || c.fixArgv[1] != "on" {
				t.Errorf("%s fixArgv = %v, want [claude on]", name, c.fixArgv)
			}
		}
	})
	t.Run("wrong values", func(t *testing.T) {
		fx := newDoctorFixture(t, nil)
		root := readTree(t, fx.settings)
		env := root["env"].(map[string]any)
		env[claudeAttributionEnvKey] = "1"
		env[claudeToolSearchEnvKey] = "auto"
		b, _ := json.Marshal(root)
		if err := os.WriteFile(fx.settings, b, 0o600); err != nil {
			t.Fatal(err)
		}
		rep := runDoctor(doctorOpts{})
		for name, want := range map[string]string{
			"attribution_header": `"0"`,
			"tool_search":        `"true"`,
		} {
			c := findCheck(t, rep, "claude", name)
			if c.Status != docFail || !strings.Contains(c.Finding, "want "+want) {
				t.Errorf("%s = %+v, want exact-value failure for %s", name, c, want)
			}
		}
	})
}

func TestDoctorShellEnvOverridesSettings(t *testing.T) {
	newDoctorFixture(t, nil)
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	rep := runDoctor(doctorOpts{})
	c := findCheck(t, rep, "claude", "shell_env")
	if c.Status != docFail || !strings.Contains(c.Finding, "overrides settings") {
		t.Errorf("shell_env = %+v, want fail naming the shell override", c)
	}
	if !strings.Contains(c.Fix, "unset ANTHROPIC_BASE_URL") {
		t.Errorf("fix = %q, want an unset instruction", c.Fix)
	}
}

func TestDoctorSubagentsChecks(t *testing.T) {
	aliases := map[string]string{"claude-baseten-glm-5-2": "zai-org/GLM-5.2"}

	t.Run("unset is ok", func(t *testing.T) {
		newDoctorFixture(t, nil)
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagents")
		if c.Status != docOK || !strings.Contains(c.Finding, "no subagent_model") {
			t.Errorf("subagents = %+v, want ok naming the unset state", c)
		}
	})

	t.Run("configured alias with wiring is ok", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.subagentModel = "claude-baseten-glm-5-2"
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagents")
		if c.Status != docOK || !strings.Contains(c.Finding, "subagent_model=claude-baseten-glm-5-2") {
			t.Errorf("subagents = %+v, want ok naming the configured model", c)
		}
	})

	t.Run("native id is ok", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.subagentModel = "claude-haiku-4-5"
		})
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "claude", "subagents"); c.Status != docOK {
			t.Errorf("subagents = %+v, want ok for a native id", c)
		}
	})

	t.Run("slug is ok", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.subagentModel = "zai-org/GLM-5.2"
		})
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "claude", "subagents"); c.Status != docOK {
			t.Errorf("subagents = %+v, want ok for a slug", c)
		}
	})

	t.Run("unconfigured baseten alias fails naming the configured set", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.subagentModel = "claude-baseten-removed"
		})
		rep := runDoctor(doctorOpts{})
		if rep.ExitCode != 1 {
			t.Fatalf("exit = %d, want 1", rep.ExitCode)
		}
		c := findCheck(t, rep, "claude", "subagents")
		if c.Status != docFail {
			t.Fatalf("subagents = %+v, want fail", c)
		}
		for _, want := range []string{"claude-baseten-removed", "claude-baseten-glm-5-2", "invalid"} {
			if !strings.Contains(c.Finding, want) {
				t.Errorf("finding %q missing %q", c.Finding, want)
			}
		}
		if !strings.Contains(c.Fix, "baseten-switch claude subagents") || !strings.Contains(c.Fix, "subagent_model") {
			t.Errorf("fix = %q, want the subagents command and the config edit", c.Fix)
		}
		if len(c.fixArgv) != 0 {
			t.Errorf("subagents fix must stay manual, got argv %v", c.fixArgv)
		}
	})

	t.Run("enabled but wiring off warns", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.subagentModel = "claude-baseten-glm-5-2"
			c.settingsJSON = `{"permissions":{}}`
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagents")
		if c.Status != docWarn || !strings.Contains(c.Finding, "no effect until") {
			t.Errorf("subagents = %+v, want warn naming the wiring-off state", c)
		}
		if c.Fix != "baseten-switch claude on" {
			t.Errorf("fix = %q, want 'baseten-switch claude on'", c.Fix)
		}
	})

	t.Run("routing off is ok and displays as inherit", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.subagentModel = "claude-baseten-glm-5-2"
			c.subagentRouting = "off"
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagents")
		if c.Status != docOK || !strings.Contains(c.Finding, "routing inherit (subagents follow the main model)") {
			t.Errorf("subagents = %+v, want ok naming routing inherit", c)
		}
	})

	t.Run("routing set with empty model fails naming the router refusal", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.subagentRouting = "on"
		})
		rep := runDoctor(doctorOpts{})
		if rep.ExitCode != 1 {
			t.Fatalf("exit = %d, want 1", rep.ExitCode)
		}
		c := findCheck(t, rep, "claude", "subagents")
		if c.Status != docFail {
			t.Fatalf("subagents = %+v, want fail", c)
		}
		for _, want := range []string{"subagent_routing", "empty", "router will refuse"} {
			if !strings.Contains(c.Finding, want) {
				t.Errorf("finding %q missing %q", c.Finding, want)
			}
		}
		if !strings.Contains(c.Fix, "baseten-switch claude subagents") && !strings.Contains(c.Fix, "remove subagent_routing") {
			t.Errorf("fix = %q, want the subagents command or the remove-routing instruction", c.Fix)
		}
	})
}

func TestDoctorSubagentEnvVarWarns(t *testing.T) {
	t.Run("settings env var warns", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.subagentEnvModel = "claude-haiku-4-5"
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagent_env")
		if c.Status != docWarn || !strings.Contains(c.Finding, "CLAUDE_CODE_SUBAGENT_MODEL") {
			t.Errorf("subagent_env = %+v, want warn naming the env var", c)
		}
	})

	t.Run("process env var warns", func(t *testing.T) {
		newDoctorFixture(t, nil)
		t.Setenv(claudeSubagentEnvKey, "claude-haiku-4-5")
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagent_env")
		if c.Status != docWarn || !strings.Contains(c.Finding, "process env") {
			t.Errorf("subagent_env = %+v, want warn naming the process env", c)
		}
	})

	t.Run("no env var is ok", func(t *testing.T) {
		newDoctorFixture(t, nil)
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagent_env")
		if c.Status != docOK {
			t.Errorf("subagent_env = %+v, want ok", c)
		}
	})
}

func TestDoctorSubagentTrafficCheck(t *testing.T) {
	aliases := map[string]string{"claude-baseten-glm-5-2": "zai-org/GLM-5.2"}
	now := time.Now().Unix()

	t.Run("enabled with subagent traffic is ok", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.subagentModel = "claude-baseten-glm-5-2"
			c.telRows = []telRowSpec{
				{TS: now - 60, Subagent: false},
				{TS: now - 30, Subagent: true},
			}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagent_traffic")
		if c.Status != docOK || !strings.Contains(c.Finding, "subagent") {
			t.Errorf("subagent_traffic = %+v, want ok naming subagent rows", c)
		}
	})

	t.Run("enabled with recent rows but no subagent warns", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.subagentModel = "claude-baseten-glm-5-2"
			c.telRows = []telRowSpec{
				{TS: now - 60, Subagent: false},
				{TS: now - 30, Subagent: false},
			}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagent_traffic")
		if c.Status != docWarn {
			t.Fatalf("subagent_traffic = %+v, want warn", c)
		}
		for _, want := range []string{"none carry", "x-claude-code-agent-id", "no agentic runs", "dropped the header"} {
			if !strings.Contains(c.Finding, want) {
				t.Errorf("finding %q missing %q", c.Finding, want)
			}
		}
	})

	t.Run("enabled with only old rows is ok", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.subagentModel = "claude-baseten-glm-5-2"
			c.telRows = []telRowSpec{
				{TS: now - 48*3600, Subagent: false},
			}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagent_traffic")
		if c.Status != docOK || !strings.Contains(c.Finding, "no request rows in the last 24h") {
			t.Errorf("subagent_traffic = %+v, want ok naming no recent rows", c)
		}
	})

	t.Run("disabled skips traffic check", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.telRows = []telRowSpec{
				{TS: now - 30, Subagent: false},
			}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "subagent_traffic")
		if c.Status != docSkip {
			t.Errorf("subagent_traffic = %+v, want skip (not enabled)", c)
		}
	})
}

// TestDoctorModelRoutesCheck covers the config-side family route pin
// check (config/schema.md): empty is ok, a valid pin is ok, an
// invalid key or target fails naming the router refusal.
func TestDoctorModelRoutesCheck(t *testing.T) {
	aliases := map[string]string{"claude-baseten-glm-5-2": "zai-org/GLM-5.2"}

	t.Run("empty is ok", func(t *testing.T) {
		newDoctorFixture(t, nil)
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_routes")
		if c.Status != docOK || !strings.Contains(c.Finding, "no model_routes") {
			t.Errorf("model_routes = %+v, want ok naming the empty state", c)
		}
	})

	t.Run("valid family pin is ok", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelRoutes = map[string]string{"opus": "native"}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_routes")
		if c.Status != docOK || !strings.Contains(c.Finding, "1 pin") {
			t.Errorf("model_routes = %+v, want ok naming the pin count", c)
		}
	})

	t.Run("valid alias pin is ok", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.modelRoutes = map[string]string{"sonnet": "claude-baseten-glm-5-2"}
		})
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "claude", "model_routes"); c.Status != docOK {
			t.Errorf("model_routes = %+v, want ok for a configured alias pin", c)
		}
	})

	t.Run("valid slug pin is ok", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelRoutes = map[string]string{"haiku": "nvidia/NVIDIA-Nemotron-3-Ultra"}
		})
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "claude", "model_routes"); c.Status != docOK {
			t.Errorf("model_routes = %+v, want ok for a slug pin", c)
		}
	})

	t.Run("exact-id pin is rejected", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.modelRoutes = map[string]string{"claude-opus-4-8": "zai-org/GLM-5.2"}
		})
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "claude", "model_routes"); c.Status != docFail {
			t.Errorf("model_routes = %+v, want fail for an exact-id pin", c)
		}
	})

	t.Run("invalid key fails naming the router refusal", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelRoutes = map[string]string{"gpt-5": "native"}
		})
		rep := runDoctor(doctorOpts{})
		if rep.ExitCode != 1 {
			t.Fatalf("exit = %d, want 1", rep.ExitCode)
		}
		c := findCheck(t, rep, "claude", "model_routes")
		if c.Status != docFail {
			t.Fatalf("model_routes = %+v, want fail", c)
		}
		for _, want := range []string{"gpt-5", "supported family", "router will refuse"} {
			if !strings.Contains(c.Finding, want) {
				t.Errorf("finding %q missing %q", c.Finding, want)
			}
		}
		if !strings.Contains(c.Fix, "baseten-switch claude route") {
			t.Errorf("fix = %q, want the route command", c.Fix)
		}
		if len(c.fixArgv) != 0 {
			t.Errorf("model_routes fix must stay manual, got argv %v", c.fixArgv)
		}
	})

	t.Run("bracketed key fails naming the router refusal", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelRoutes = map[string]string{"claude-opus-4-8[1m]": "native"}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_routes")
		if c.Status != docFail || !strings.Contains(c.Finding, "claude-opus-4-8[1m]") {
			t.Errorf("model_routes = %+v, want fail naming the bracketed key", c)
		}
	})

	t.Run("non-family key with spaces fails", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelRoutes = map[string]string{"claude opus": "native"}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_routes")
		if c.Status != docFail {
			t.Fatalf("model_routes = %+v, want fail", c)
		}
		for _, want := range []string{"claude opus", "supported family", "router will refuse"} {
			if !strings.Contains(c.Finding, want) {
				t.Errorf("finding %q missing %q", c.Finding, want)
			}
		}
	})

	t.Run("alias-namespace key fails as non-family", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.modelRoutes = map[string]string{"claude-baseten-glm-5-2": "native"}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_routes")
		if c.Status != docFail {
			t.Fatalf("model_routes = %+v, want fail", c)
		}
		for _, want := range []string{"claude-baseten-glm-5-2", "supported family"} {
			if !strings.Contains(c.Finding, want) {
				t.Errorf("finding %q missing %q", c.Finding, want)
			}
		}
	})

	t.Run("unhyphenated prefix exact key is rejected", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelRoutes = map[string]string{"anthropic.claude-v2": "native"}
		})
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "claude", "model_routes"); c.Status != docFail {
			t.Errorf("model_routes = %+v, want fail for a non-family key", c)
		}
	})

	t.Run("configured alias outside the namespace is ok as a target", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = map[string]string{"claudette-custom": "zai-org/GLM-5.2"}
			c.modelRoutes = map[string]string{"opus": "claudette-custom"}
		})
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "claude", "model_routes"); c.Status != docOK {
			t.Errorf("model_routes = %+v, want ok (configured alias checked before the namespace gate)", c)
		}
	})

	t.Run("unconfigured alias target fails naming the configured set", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelAliases = aliases
			c.modelRoutes = map[string]string{"opus": "claude-baseten-removed"}
		})
		rep := runDoctor(doctorOpts{})
		if rep.ExitCode != 1 {
			t.Fatalf("exit = %d, want 1", rep.ExitCode)
		}
		c := findCheck(t, rep, "claude", "model_routes")
		if c.Status != docFail {
			t.Fatalf("model_routes = %+v, want fail", c)
		}
		for _, want := range []string{"claude-baseten-removed", "claude-baseten-glm-5-2", "router will refuse"} {
			if !strings.Contains(c.Finding, want) {
				t.Errorf("finding %q missing %q", c.Finding, want)
			}
		}
	})

	t.Run("every family pinned is valid", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelRoutes = map[string]string{
				"fable":  "native",
				"opus":   "native",
				"sonnet": "native",
				"haiku":  "native",
			}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_routes")
		if c.Status != docOK {
			t.Fatalf("model_routes = %+v, want ok", c)
		}
		if rep.ExitCode != 0 {
			t.Errorf("exit = %d, warn alone must not fail", rep.ExitCode)
		}
	})

	t.Run("three families pinned is ok (switch still affects one)", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelRoutes = map[string]string{
				"opus":   "native",
				"sonnet": "native",
				"haiku":  "native",
			}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_routes")
		if c.Status != docOK {
			t.Errorf("model_routes = %+v, want ok (fable still follows the switch)", c)
		}
	})
}

// TestDoctorModelEnvCheck covers the harness env-slot warn
// (ANTHROPIC_MODEL / ANTHROPIC_DEFAULT_*_MODEL in settings env or process
// env; the fireconnect survey hazard).
func TestDoctorModelEnvCheck(t *testing.T) {
	t.Run("settings ANTHROPIC_MODEL warns", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelEnvVars = map[string]string{"ANTHROPIC_MODEL": "claude-haiku-4-5"}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_env")
		if c.Status != docWarn || !strings.Contains(c.Finding, "ANTHROPIC_MODEL") || !strings.Contains(c.Finding, "upstream of family pins") {
			t.Errorf("model_env = %+v, want warn naming ANTHROPIC_MODEL and the upstream hazard", c)
		}
		// Present-only list: vars that are not set must not be named.
		if strings.Contains(c.Finding, "ANTHROPIC_DEFAULT_") {
			t.Errorf("model_env finding names absent env vars: %q", c.Finding)
		}
	})

	t.Run("settings ANTHROPIC_DEFAULT_SONNET_MODEL warns", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelEnvVars = map[string]string{"ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-4-6"}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_env")
		if c.Status != docWarn || !strings.Contains(c.Finding, "ANTHROPIC_DEFAULT_SONNET_MODEL") {
			t.Errorf("model_env = %+v, want warn naming the default-slot var", c)
		}
		for _, absent := range []string{"ANTHROPIC_MODEL,", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
			if strings.Contains(c.Finding, absent) {
				t.Errorf("model_env finding names absent env var %s: %q", absent, c.Finding)
			}
		}
	})

	t.Run("process env ANTHROPIC_DEFAULT_OPUS_MODEL warns", func(t *testing.T) {
		newDoctorFixture(t, nil)
		t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "claude-opus-4-6")
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_env")
		if c.Status != docWarn || !strings.Contains(c.Finding, "process env") || !strings.Contains(c.Finding, "ANTHROPIC_DEFAULT_OPUS_MODEL") {
			t.Errorf("model_env = %+v, want warn naming the process env var", c)
		}
		for _, absent := range []string{"ANTHROPIC_MODEL,", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
			if strings.Contains(c.Finding, absent) {
				t.Errorf("model_env finding names absent env var %s: %q", absent, c.Finding)
			}
		}
	})

	t.Run("settings and process vars are each named once", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.modelEnvVars = map[string]string{"ANTHROPIC_MODEL": "claude-haiku-4-5"}
		})
		t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "claude-opus-4-6")
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_env")
		if c.Status != docWarn {
			t.Fatalf("model_env = %+v, want warn", c)
		}
		// The present list is "ANTHROPIC_MODEL, ANTHROPIC_DEFAULT_OPUS_MODEL
		// set in ..."; both sources follow with their values.
		for _, want := range []string{"ANTHROPIC_MODEL, ANTHROPIC_DEFAULT_OPUS_MODEL set in", "settings env", "process env"} {
			if !strings.Contains(c.Finding, want) {
				t.Errorf("finding %q missing %q", c.Finding, want)
			}
		}
	})

	t.Run("no env vars is ok", func(t *testing.T) {
		newDoctorFixture(t, nil)
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "claude", "model_env")
		if c.Status != docOK {
			t.Errorf("model_env = %+v, want ok", c)
		}
	})
}

// TestDoctorClaudeEarlyExitShapes asserts the model_routes and model_env
// checks are present in every report shape: skipped when the adapter
// cannot resolve (nothing to validate against), but still RUN on the two
// settings-failure paths (the config-validity half needs only
// gateway.yaml, and the env-slot scan degrades to the process env).
func TestDoctorClaudeEarlyExitShapes(t *testing.T) {
	t.Run("adapter error skips model checks", func(t *testing.T) {
		newDoctorFixture(t, nil)
		// Point the config at a missing file so newClaudeAdapterFromEnv
		// fails inside doctorClaudeChecks.
		t.Setenv("BASETEN_SWITCH_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.yaml"))
		rep := runDoctor(doctorOpts{})
		for _, name := range []string{"settings", "base_url", "attribution_header", "tool_search", "shell_env", "subagents", "model_routes", "model_env", "backup"} {
			if c := findCheck(t, rep, "claude", name); c.Status != docSkip {
				t.Errorf("claude/%s = %+v, want skip on the adapter-error path", name, c)
			}
		}
	})

	t.Run("unreadable settings still runs model checks", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.settingsJSON = `{not json`
			c.modelRoutes = map[string]string{"gpt-5": "native"}
		})
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "claude", "settings"); c.Status != docFail {
			t.Fatalf("settings = %+v, want fail", c)
		}
		// The config-validity half needs only gateway.yaml, so the invalid
		// key still fails loudly.
		c := findCheck(t, rep, "claude", "model_routes")
		if c.Status != docFail || !strings.Contains(c.Finding, "gpt-5") {
			t.Errorf("model_routes = %+v, want fail naming the invalid key", c)
		}
		if c := findCheck(t, rep, "claude", "model_env"); c.Status != docOK {
			t.Errorf("model_env = %+v, want ok (present in every report shape)", c)
		}
	})

	t.Run("unreadable env block still runs model checks", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.settingsJSON = `{"env":"nope"}`
		})
		// The settings env scan degrades away, but the process env scan
		// still applies.
		t.Setenv("ANTHROPIC_MODEL", "claude-haiku-4-5")
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "claude", "settings"); c.Status != docFail {
			t.Fatalf("settings = %+v, want fail", c)
		}
		if c := findCheck(t, rep, "claude", "model_routes"); c.Status != docOK {
			t.Errorf("model_routes = %+v, want ok (no routes configured)", c)
		}
		c := findCheck(t, rep, "claude", "model_env")
		if c.Status != docWarn || !strings.Contains(c.Finding, "ANTHROPIC_MODEL") || !strings.Contains(c.Finding, "process env") {
			t.Errorf("model_env = %+v, want warn from the process env scan", c)
		}
	})
}

func TestDoctorDoorMiswired(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorRouter = "127.0.0.1:1" // live door forwards to an addr no client is bound on
	})
	rep := runDoctor(doctorOpts{})
	c := findCheck(t, rep, "door", "wiring:"+fx.doorPort)
	if c.Status != docFail || !strings.Contains(c.Finding, "no bound client listens there") {
		t.Fatalf("wiring = %+v, want mis-wiring fail", c)
	}
	if !strings.Contains(c.Fix, "router_addr") {
		t.Errorf("fix = %q, want a router_addr correction", c.Fix)
	}
	if rep.FirstFailure != "door/wiring:"+fx.doorPort {
		t.Errorf("first_failure = %q", rep.FirstFailure)
	}
}

func TestDoctorRouterDownDependentsSkip(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) { c.adminDown = true })
	rep := runDoctor(doctorOpts{})
	if rep.FirstFailure != "router/reachable" {
		t.Fatalf("first_failure = %q, want router/reachable\nchecks: %+v", rep.FirstFailure, rep.Checks)
	}
	c := findCheck(t, rep, "router", "reachable")
	if c.Status != docFail || !strings.Contains(c.Finding, "not running") || c.Fix != "baseten-switch up" {
		t.Errorf("reachable = %+v, want down fail with 'baseten-switch up'", c)
	}
	for _, name := range []string{"status", "clients"} {
		if c := findCheck(t, rep, "router", name); c.Status != docSkip {
			t.Errorf("router/%s = %s, want skip", name, c.Status)
		}
	}
	if c := findCheck(t, rep, "door", "wiring:"+fx.doorPort); c.Status != docSkip {
		t.Errorf("door wiring with router down = %s, want skip", c.Status)
	}
}

func TestDoctorMutationRecoveryClassificationPolicy(t *testing.T) {
	tests := []struct {
		name        string
		class       string
		wantStatus  string
		wantFix     bool
		wantFinding string
	}{
		{name: "none", class: mutationStatusNone, wantStatus: docOK},
		{name: "desired active", class: mutationStatusDesiredActive, wantStatus: docFail, wantFix: true},
		{name: "prior active", class: mutationStatusPriorActive, wantStatus: docFail, wantFix: true, wantFinding: "was not applied; the prior routing state is active"},
		{name: "cleanup pending", class: mutationStatusCleanupPending, wantStatus: docFail, wantFix: true},
		{name: "desired pending", class: mutationStatusDesiredPending, wantStatus: docFail},
		{name: "prior pending", class: mutationStatusPriorPending, wantStatus: docFail},
		{name: "router unavailable", class: mutationStatusRouterUnavailable, wantStatus: docFail},
		{name: "router unsupported", class: mutationStatusRouterUnsupported, wantStatus: docFail},
		{name: "external change", class: mutationStatusExternalChange, wantStatus: docFail},
		{name: "commit recovery required", class: mutationStatusCommitRecoveryRequired, wantStatus: docFail},
		{name: "journal invalid", class: mutationStatusJournalInvalid, wantStatus: docFail},
		{name: "journal conflict", class: mutationStatusJournalConflict, wantStatus: docFail},
		{name: "unknown", class: "future_state", wantStatus: docFail},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldInspect := doctorInspectRoutingMutationStatus
			doctorInspectRoutingMutationStatus = func(string) (routingMutationStatus, error) {
				return routingMutationStatus{Classification: tt.class}, nil
			}
			t.Cleanup(func() { doctorInspectRoutingMutationStatus = oldInspect })

			var checks []doctorCheck
			doctorMutationRecoveryCheck(func(section, name, status, finding, fix string, fixArgv ...string) {
				checks = append(checks, doctorCheck{
					Section: section,
					Name:    name,
					Status:  status,
					Finding: finding,
					Fix:     fix,
					fixArgv: fixArgv,
				})
			}, "/private/config/path")

			if len(checks) != 1 {
				t.Fatalf("checks = %+v, want one", checks)
			}
			check := checks[0]
			if check.Section != "router" || check.Name != "mutation_recovery" || check.Status != tt.wantStatus {
				t.Errorf("check = %+v, want router/mutation_recovery %s", check, tt.wantStatus)
			}
			if tt.wantFinding != "" && !strings.Contains(check.Finding, tt.wantFinding) {
				t.Errorf("finding = %q, want text %q", check.Finding, tt.wantFinding)
			}
			if tt.wantFix {
				if got := strings.Join(check.fixArgv, " "); got != "mutation recover" {
					t.Errorf("fixArgv = %q, want mutation recover", got)
				}
			} else if len(check.fixArgv) != 0 {
				t.Errorf("manual-only classification got fixArgv %v", check.fixArgv)
			}
			if strings.Contains(check.Finding, "/private/config/path") || strings.Contains(check.Fix, "/private/config/path") {
				t.Errorf("check leaked config path: %+v", check)
			}
		})
	}
}

func TestDoctorMutationRecoveryInspectionErrorIsSanitized(t *testing.T) {
	oldInspect := doctorInspectRoutingMutationStatus
	doctorInspectRoutingMutationStatus = func(string) (routingMutationStatus, error) {
		return routingMutationStatus{}, fmt.Errorf("open /private/config/path.mutation-journal: permission denied")
	}
	t.Cleanup(func() { doctorInspectRoutingMutationStatus = oldInspect })

	var check doctorCheck
	doctorMutationRecoveryCheck(func(section, name, status, finding, fix string, fixArgv ...string) {
		check = doctorCheck{Section: section, Name: name, Status: status, Finding: finding, Fix: fix, fixArgv: fixArgv}
	}, "/private/config/path")
	if check.Status != docFail || len(check.fixArgv) != 0 {
		t.Fatalf("check = %+v, want manual-only failure", check)
	}
	if strings.Contains(check.Finding, "/private/") || strings.Contains(check.Fix, "/private/") || strings.Contains(check.Finding, "permission denied") {
		t.Errorf("inspection failure leaked raw error or path: %+v", check)
	}
}

func TestDoctorMutationRecoveryCheckOrdering(t *testing.T) {
	newDoctorFixture(t, nil)
	oldInspect := doctorInspectRoutingMutationStatus
	doctorInspectRoutingMutationStatus = func(string) (routingMutationStatus, error) {
		return routingMutationStatus{Classification: mutationStatusNone}, nil
	}
	t.Cleanup(func() { doctorInspectRoutingMutationStatus = oldInspect })

	rep := runDoctor(doctorOpts{})
	indices := map[string]int{}
	for i, check := range rep.Checks {
		indices[check.Section+"/"+check.Name] = i
	}
	reachable := indices["router/reachable"]
	status := indices["router/status"]
	recovery := indices["router/mutation_recovery"]
	client := indices["router/client:claude-code"]
	if !(reachable < status && status < recovery && recovery < client) {
		t.Fatalf("router check order = reachable:%d status:%d recovery:%d client:%d", reachable, status, recovery, client)
	}
}

func TestDoctorForeignAdminPort(t *testing.T) {
	newDoctorFixture(t, func(c *doctorFixtureCfg) { c.adminForeign = true })
	rep := runDoctor(doctorOpts{})
	c := findCheck(t, rep, "router", "reachable")
	if c.Status != docFail {
		t.Fatalf("reachable = %+v, want fail", c)
	}
	if !strings.Contains(c.Finding, "foreign process") {
		t.Errorf("finding %q must call the owner foreign, not down", c.Finding)
	}
	if strings.Contains(c.Finding, "not running") {
		t.Errorf("finding %q conflates foreign with down", c.Finding)
	}
}

func TestDoctorUnresolvedAPIKeyNoOAuth(t *testing.T) {
	newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.signedIn = false
		c.globalAuth = "${BASETEN_API_KEY}"
	})
	rep := runDoctor(doctorOpts{})
	c := findCheck(t, rep, "auth", "signin")
	if c.Status != docFail || !strings.Contains(c.Finding, "${BASETEN_API_KEY}") {
		t.Fatalf("signin = %+v, want fail naming ${BASETEN_API_KEY}", c)
	}
	if !strings.Contains(c.Fix, "baseten auth login") || !strings.Contains(c.Fix, "BASETEN_API_KEY=") {
		t.Errorf("fix = %q, want both fix paths (login and env file)", c.Fix)
	}
	if rep.FirstFailure != "auth/signin" {
		t.Errorf("first_failure = %q", rep.FirstFailure)
	}
	if p := findCheck(t, rep, "config", "placeholders"); p.Status != docWarn {
		t.Errorf("config/placeholders = %s, want warn", p.Status)
	}
}

func TestDoctorReadableAPIKeyProfileRouterSignedOutFails(t *testing.T) {
	newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.apiKeyProfile = true
		c.routerSignedOut = true
		c.authHealth = "signed_out"
	})
	rep := runDoctor(doctorOpts{})
	if c := findCheck(t, rep, "auth", "signin"); c.Status != docOK || !strings.Contains(c.Finding, "API key") {
		t.Fatalf("signin = %+v, want readable API-key profile", c)
	}
	c := findCheck(t, rep, "auth", "health")
	if c.Status != docFail || !strings.Contains(c.Finding, "example-profile") || !strings.Contains(c.Finding, "router reports signed out") {
		t.Fatalf("health = %+v, want profile/router inconsistency failure", c)
	}
	if rep.FirstFailure != "auth/health" {
		t.Fatalf("first failure = %q, want auth/health", rep.FirstFailure)
	}
}

func TestDoctorJSONShape(t *testing.T) {
	newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.settingsJSON = `{"permissions":{}}`
	})
	out, code := captureStdout(t, func() int {
		return cmdDoctor([]string{"--json"})
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json output does not parse: %v\n%s", err, out)
	}
	if rep.FirstFailure != "claude/base_url" || rep.ExitCode != 1 {
		t.Fatalf("first_failure = %q exit_code = %d", rep.FirstFailure, rep.ExitCode)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("empty check array")
	}
	for _, c := range rep.Checks {
		if c.Section == "" || c.Name == "" || c.Status == "" || c.Finding == "" {
			t.Errorf("incomplete check in JSON: %+v", c)
		}
	}
}

func TestDoctorUsageError(t *testing.T) {
	newDoctorFixture(t, nil)
	if code := cmdDoctor([]string{"--bogus"}); code != 2 {
		t.Errorf("unknown flag exit = %d, want 2", code)
	}
}

// --- doctor --fix ------------------------------------------------------
//
// The fix tests reuse the hermetic fixture and replace the three fix
// seams, so no real verb is ever exec'd and no real terminal is read.

// setDoctorFixSeams installs fakes for the --fix seams and returns the
// recorded fix argvs. apply (optional) mutates the fixture to emulate
// the verb's effect.
func setDoctorFixSeams(t *testing.T, tty bool, input string, apply func(argv []string) error) *[][]string {
	t.Helper()
	prevExec, prevIn, prevTTY := doctorFixExec, doctorFixIn, doctorStdinIsTTY
	t.Cleanup(func() {
		doctorFixExec, doctorFixIn, doctorStdinIsTTY = prevExec, prevIn, prevTTY
	})
	calls := &[][]string{}
	doctorFixExec = func(argv []string) error {
		*calls = append(*calls, argv)
		if apply != nil {
			return apply(argv)
		}
		return nil
	}
	doctorFixIn = strings.NewReader(input)
	doctorStdinIsTTY = func() bool { return tty }
	return calls
}

// takeRouterDown points the doctor at a closed admin port while the
// fixture's admin server keeps running; the returned restore emulates
// a successful `up` by pointing back at the live server.
func takeRouterDown(t *testing.T) (restore func()) {
	t.Helper()
	live := os.Getenv("BASETEN_SWITCH_ADMIN_ADDR")
	os.Setenv("BASETEN_SWITCH_ADMIN_ADDR", closedPortAddr(t))
	return func() { os.Setenv("BASETEN_SWITCH_ADMIN_ADDR", live) }
}

func TestDoctorFixGreenChainNoPrompts(t *testing.T) {
	newDoctorFixture(t, nil)
	calls := setDoctorFixSeams(t, true, "", nil)
	out, code := captureStdout(t, func() int { return cmdDoctor([]string{"--fix"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	if len(*calls) != 0 {
		t.Errorf("green chain executed fixes: %v", *calls)
	}
	if strings.Contains(out, "apply?") {
		t.Errorf("green chain prompted:\n%s", out)
	}
	if !strings.Contains(out, "all checks passed") {
		t.Errorf("summary line missing:\n%s", out)
	}
}

func TestDoctorFixRouterDownAppliedAndResolved(t *testing.T) {
	newDoctorFixture(t, nil)
	restore := takeRouterDown(t)
	calls := setDoctorFixSeams(t, true, "y\n", func(argv []string) error {
		restore()
		return nil
	})
	out, code := captureStdout(t, func() int { return cmdDoctor([]string{"--fix"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0 after the fix resolves\n%s", code, out)
	}
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != "up" {
		t.Fatalf("fix argvs = %v, want exactly ['up']", *calls)
	}
	if n := strings.Count(out, "apply? [y/N]"); n != 1 {
		t.Errorf("prompt count = %d, want 1\n%s", n, out)
	}
	if !strings.Contains(out, "will run:") {
		t.Errorf("exact command line missing:\n%s", out)
	}
	if !strings.Contains(out, "all checks passed") {
		t.Errorf("final green report missing:\n%s", out)
	}
}

func TestDoctorFixRouterDownWithActiveMutationDoesNotStartRouter(t *testing.T) {
	fx := newDoctorFixture(t, nil)
	journal := legacyJournalForTest(t, fx.cfgPath, "doctor-blocks-up")
	if err := writeMutationJournal(journal); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(fx.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	takeRouterDown(t)
	calls := setDoctorFixSeams(t, false, "", nil)
	out, code := captureStdout(t, func() int { return cmdDoctor([]string{"--fix", "--yes"}) })
	if code != 1 {
		t.Fatalf("exit = %d, want 1 with recovery still pending\n%s", code, out)
	}
	if len(*calls) != 0 {
		t.Fatalf("router-down recovery state executed fixes: %v", *calls)
	}
	if strings.Contains(out, "will run:") || !strings.Contains(out, "resolve the routing mutation recovery state") {
		t.Fatalf("doctor offered startup before recovery:\n%s", out)
	}
	after, err := os.ReadFile(fx.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("doctor changed the routing config while recovery was pending")
	}
	if _, err := os.Stat(mutationJournalPath(fx.cfgPath, journal.OperationID)); err != nil {
		t.Fatalf("doctor changed the active recovery journal: %v", err)
	}
}

func TestDoctorFixMutationRecoveryRunsOnlyRecover(t *testing.T) {
	newDoctorFixture(t, nil)
	classification := mutationStatusDesiredActive
	oldInspect := doctorInspectRoutingMutationStatus
	doctorInspectRoutingMutationStatus = func(string) (routingMutationStatus, error) {
		return routingMutationStatus{Classification: classification}, nil
	}
	t.Cleanup(func() { doctorInspectRoutingMutationStatus = oldInspect })

	calls := setDoctorFixSeams(t, false, "", func(argv []string) error {
		classification = mutationStatusNone
		return nil
	})
	out, code := captureStdout(t, func() int { return cmdDoctor([]string{"--fix", "--yes"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0 after recovery cleanup\n%s", code, out)
	}
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != "mutation recover" {
		t.Fatalf("fix argvs = %v, want exactly ['mutation', 'recover']", *calls)
	}
	if strings.Contains(out, "mutation reconcile") {
		t.Errorf("doctor --fix proposed full reconciliation:\n%s", out)
	}
}

func TestDoctorFixYesCannotReconcileUnsafeMutation(t *testing.T) {
	newDoctorFixture(t, nil)
	oldInspect := doctorInspectRoutingMutationStatus
	doctorInspectRoutingMutationStatus = func(string) (routingMutationStatus, error) {
		return routingMutationStatus{Classification: mutationStatusDesiredPending}, nil
	}
	t.Cleanup(func() { doctorInspectRoutingMutationStatus = oldInspect })

	calls := setDoctorFixSeams(t, false, "", nil)
	out, code := captureStdout(t, func() int { return cmdDoctor([]string{"--fix", "--yes"}) })
	if code != 1 {
		t.Fatalf("exit = %d, want 1 with manual reconciliation pending\n%s", code, out)
	}
	if len(*calls) != 0 {
		t.Fatalf("unsafe status executed fixes: %v", *calls)
	}
	if !strings.Contains(out, "no automatable fix for router/mutation_recovery") {
		t.Errorf("manual-only guidance missing:\n%s", out)
	}
}

func TestDoctorFixDeclinedExecutesNothing(t *testing.T) {
	newDoctorFixture(t, nil)
	takeRouterDown(t)
	calls := setDoctorFixSeams(t, true, "n\n", nil)
	out, code := captureStdout(t, func() int { return cmdDoctor([]string{"--fix"}) })
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (failure remains after decline)\n%s", code, out)
	}
	if len(*calls) != 0 {
		t.Errorf("decline executed fixes: %v", *calls)
	}
	if !strings.Contains(out, "not applied") || !strings.Contains(out, "manual fix: baseten-switch up") {
		t.Errorf("decline must leave the manual guidance:\n%s", out)
	}
}

func TestDoctorFixDidNotResolveStops(t *testing.T) {
	newDoctorFixture(t, nil)
	takeRouterDown(t)
	calls := setDoctorFixSeams(t, true, "y\ny\ny\n", nil) // exec is a noop: the check stays failed
	out, code := captureStdout(t, func() int { return cmdDoctor([]string{"--fix"}) })
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if len(*calls) != 1 {
		t.Errorf("fix applied %d times, want 1 (stop on non-resolution)", len(*calls))
	}
	if !strings.Contains(out, "fix did not resolve router/reachable") {
		t.Errorf("did-not-resolve message missing:\n%s", out)
	}
}

// TestDoctorFixLoopCap ping-pongs two automatable fixes (each clears
// its own check but re-breaks the other link) and pins the round cap.
func TestDoctorFixLoopCap(t *testing.T) {
	fx := newDoctorFixture(t, nil)
	liveAdmin := os.Getenv("BASETEN_SWITCH_ADMIN_ADDR")
	closedAdmin := closedPortAddr(t)
	goodSettings := fmt.Sprintf(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:%s","CLAUDE_CODE_ATTRIBUTION_HEADER":"0","ENABLE_TOOL_SEARCH":"true"}}`, fx.doorPort)
	badSettings := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1","CLAUDE_CODE_ATTRIBUTION_HEADER":"0","ENABLE_TOOL_SEARCH":"true"}}`
	if err := os.WriteFile(fx.settings, []byte(badSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := setDoctorFixSeams(t, true, strings.Repeat("y\n", 10), func(argv []string) error {
		switch argv[0] {
		case "claude": // fixes the settings, "crashes" the router
			os.Setenv("BASETEN_SWITCH_ADMIN_ADDR", closedAdmin)
			return os.WriteFile(fx.settings, []byte(goodSettings), 0o600)
		case "up": // brings the router back, re-breaks the settings
			os.Setenv("BASETEN_SWITCH_ADMIN_ADDR", liveAdmin)
			return os.WriteFile(fx.settings, []byte(badSettings), 0o600)
		}
		return nil
	})
	out, code := captureStdout(t, func() int { return cmdDoctor([]string{"--fix"}) })
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if len(*calls) != doctorFixMaxRounds {
		t.Errorf("fixes applied = %d, want the cap %d\ncalls: %v", len(*calls), doctorFixMaxRounds, *calls)
	}
	if !strings.Contains(out, fmt.Sprintf("repair round cap (%d) reached", doctorFixMaxRounds)) {
		t.Errorf("cap message missing:\n%s", out)
	}
}

func TestDoctorFixWithJSONIsUsageError(t *testing.T) {
	newDoctorFixture(t, nil)
	calls := setDoctorFixSeams(t, true, "", nil)
	if code := cmdDoctor([]string{"--fix", "--json"}); code != 2 {
		t.Errorf("--fix --json exit = %d, want 2", code)
	}
	if len(*calls) != 0 {
		t.Errorf("usage error executed fixes: %v", *calls)
	}
}

func TestDoctorFixNonTTYRefusedWithoutYes(t *testing.T) {
	newDoctorFixture(t, nil)
	calls := setDoctorFixSeams(t, false, "", nil)
	if code := cmdDoctor([]string{"--fix"}); code != 2 {
		t.Errorf("non-TTY --fix exit = %d, want 2", code)
	}
	if len(*calls) != 0 {
		t.Errorf("refusal executed fixes: %v", *calls)
	}
}

func TestDoctorFixYesSkipsPrompts(t *testing.T) {
	newDoctorFixture(t, nil)
	restore := takeRouterDown(t)
	// Non-TTY stdin and no prompt input: --yes must bypass both the
	// TTY refusal and every confirmation.
	calls := setDoctorFixSeams(t, false, "", func(argv []string) error {
		restore()
		return nil
	})
	out, code := captureStdout(t, func() int { return cmdDoctor([]string{"--fix", "--yes"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	if len(*calls) != 1 {
		t.Errorf("fixes applied = %d, want 1", len(*calls))
	}
	if strings.Contains(out, "apply? [y/N]") {
		t.Errorf("--yes still prompted:\n%s", out)
	}
	if !strings.Contains(out, "applying (--yes)") {
		t.Errorf("--yes application line missing:\n%s", out)
	}
}

func TestDoctorFixRecursionGuard(t *testing.T) {
	newDoctorFixture(t, nil)
	calls := setDoctorFixSeams(t, true, "", nil)
	t.Setenv("BASETEN_SWITCH_DOCTOR_FIX", "1")
	if code := cmdDoctor([]string{"--fix", "--yes"}); code != 2 {
		t.Errorf("nested --fix exit = %d, want 2", code)
	}
	if len(*calls) != 0 {
		t.Errorf("nested --fix executed fixes: %v", *calls)
	}
}

// TestIsTerminalRejectsNullAndPipe pins the isatty probe: /dev/null is
// a character device, so a mode-bit check would wrongly let
// `doctor --fix </dev/null` run interactively.
func TestIsTerminalRejectsNullAndPipe(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Error("isTerminal(/dev/null) = true, want false")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Error("isTerminal(pipe) = true, want false")
	}
}

// TestDoctorJSONOmitsFixArgv pins the exported doctorCheck JSON shape:
// fixArgv is internal and must never serialize, byte for byte.
func TestDoctorJSONOmitsFixArgv(t *testing.T) {
	b, err := json.Marshal(doctorCheck{Section: "s", Name: "n", Status: "fail", Finding: "f", Fix: "x", fixArgv: []string{"up"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"section":"s","name":"n","status":"fail","finding":"f","fix":"x"}`
	if string(b) != want {
		t.Fatalf("doctorCheck JSON = %s, want %s", b, want)
	}

	// A --json run whose first failure carries a fixArgv internally
	// (router down) must expose only the pinned keys.
	newDoctorFixture(t, func(c *doctorFixtureCfg) { c.adminDown = true })
	out, code := captureStdout(t, func() int { return cmdDoctor([]string{"--json"}) })
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	var rep struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json output does not parse: %v\n%s", err, out)
	}
	allowed := map[string]bool{"section": true, "name": true, "status": true, "finding": true, "fix": true}
	for _, c := range rep.Checks {
		for k := range c {
			if !allowed[k] {
				t.Errorf("unexpected key %q serialized in check %v", k, c)
			}
		}
	}
}

// TestDoctorFallbackDormantWhileNative pins the switch-OFF shape as
// healthy: flipping off sets route to the native route while the
// template's fallback_route stays, so route == fallback_route is the
// designed dormant state on every switched-off machine, not a
// misconfiguration (the gateway ignores the dormant fallback; it
// reactivates on the next flip on). A FAIL here was a live false
// positive on 2026-07-08.
func TestDoctorFallbackDormantWhileNative(t *testing.T) {
	newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.clientRoute = "anthropic"
		c.fallbackRoute = "anthropic"
	})
	rep := runDoctor(doctorOpts{})
	if rep.ExitCode != 0 || rep.FirstFailure != "" {
		t.Fatalf("switched-off machine must be healthy: exit = %d first_failure = %q", rep.ExitCode, rep.FirstFailure)
	}
	c := findCheck(t, rep, "router", "client:claude-code")
	if c.Status != docOK || !strings.Contains(c.Finding, "dormant") {
		t.Fatalf("client check = %s (%q), want ok mentioning the dormant fallback", c.Status, c.Finding)
	}

	// A genuinely invalid fallback_route still fails.
	newDoctorFixture(t, func(c *doctorFixtureCfg) { c.fallbackRoute = "monitor" })
	rep = runDoctor(doctorOpts{})
	if c := findCheck(t, rep, "router", "client:claude-code"); c.Status != docFail {
		t.Fatalf("monitor fallback_route = %s, want fail", c.Status)
	}
}

// --- menubar binary resolution check --------------------------------------

// TestDoctorMenubarBinaryCheck covers the four cells of the menubar
// binary resolution check: match, mismatch, nothing resolved, and
// resolved-but---version-fails. All hermetic: the fake binary is a
// shell script in a temp dir, and the brew paths are swapped to temp
// dirs so no real host binary is ever probed or exec'd.
func TestDoctorMenubarBinaryCheck(t *testing.T) {
	t.Run("match is ok", func(t *testing.T) {
		newDoctorFixture(t, nil) // menubarVersion defaults to version.Version
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "binary", "menubar")
		if c.Status != docOK {
			t.Fatalf("menubar = %s (%s), want ok", c.Status, c.Finding)
		}
		if !strings.Contains(c.Finding, "matching the running components") {
			t.Errorf("finding %q must name the match", c.Finding)
		}
	})

	t.Run("mismatch warns naming the stale-click risk", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.menubarVersion = "v0.2.0" // differs from the test binary's "dev"
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "binary", "menubar")
		if c.Status != docWarn {
			t.Fatalf("menubar = %s (%s), want warn", c.Status, c.Finding)
		}
		for _, want := range []string{"v0.2.0", "stale component", "Open Dashboard or Start click"} {
			if !strings.Contains(c.Finding, want) {
				t.Errorf("finding %q missing %q", c.Finding, want)
			}
		}
		if !strings.Contains(c.Fix, "brew reinstall basetenlabs/baseten/baseten-switch") {
			t.Errorf("fix = %q, want canonical Homebrew reinstall", c.Fix)
		}
		if len(c.fixArgv) != 0 {
			t.Errorf("menubar fix must stay manual, got argv %v", c.fixArgv)
		}
	})

	t.Run("nothing resolved skips", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.noMenubarBinary = true
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "binary", "menubar")
		if c.Status != docSkip {
			t.Fatalf("menubar = %s (%s), want skip", c.Status, c.Finding)
		}
		if !strings.Contains(c.Finding, "no baseten-switch binary found") {
			t.Errorf("finding %q must name the missing binary", c.Finding)
		}
	})

	t.Run("resolved but --version fails warns", func(t *testing.T) {
		dir := t.TempDir()
		// A script that exits 1 on --version (no output).
		mbBin := filepath.Join(dir, "mb-bin", "baseten-switch")
		if err := os.MkdirAll(filepath.Dir(mbBin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mbBin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			// noMenubarBinary=false but we override BASETEN_SWITCH_GATEWAY_BIN after
			// the fixture writes its own. The fixture cleanup restores
			// the brew paths; we just point the env at our broken script.
		})
		t.Setenv("BASETEN_SWITCH_GATEWAY_BIN", mbBin)
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "binary", "menubar")
		if c.Status != docWarn {
			t.Fatalf("menubar = %s (%s), want warn", c.Status, c.Finding)
		}
		if !strings.Contains(c.Finding, "did not print a version") {
			t.Errorf("finding %q must name the --version failure", c.Finding)
		}
		if !strings.Contains(c.Fix, "reinstall") {
			t.Errorf("fix = %q, want a reinstall instruction", c.Fix)
		}
	})

	t.Run("nothing running is ok", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.adminDown = true // router down
			c.doorDown = true  // door down
			// web defaults to down
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "binary", "menubar")
		if c.Status != docOK {
			t.Fatalf("menubar = %s (%s), want ok (nothing running)", c.Status, c.Finding)
		}
		if !strings.Contains(c.Finding, "nothing running to compare against") {
			t.Errorf("finding %q must name the nothing-running state", c.Finding)
		}
	})
}

// --- auth health (running-router credential health) ------------------------

// TestDoctorAuthHealthCheck covers the running-router credential health
// check (the credential-refresh contract): refresh_failed is a FAIL naming
// the reauth verb, error warns transient, ok passes, an admin payload
// without the required field fails, and a down router skips. The store-based
// signin check stays ok throughout: the
// dead-credential state is exactly the one it cannot see.
func TestDoctorAuthHealthCheck(t *testing.T) {
	t.Run("refresh_failed is a broken link naming reauth", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.authHealth = "refresh_failed"
			c.authLastError = `oauth2: "invalid_grant"`
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "auth", "health")
		if c.Status != docFail {
			t.Fatalf("auth/health = %s (%s), want fail", c.Status, c.Finding)
		}
		if !strings.Contains(c.Finding, "invalid_grant") {
			t.Errorf("finding %q must name the refresh error", c.Finding)
		}
		if !strings.Contains(c.Fix, "baseten-switch auth login") || !strings.Contains(c.Fix, "baseten auth login") {
			t.Errorf("fix %q must name both reauth paths", c.Fix)
		}
		if rep.FirstFailure != "auth/health" {
			t.Errorf("first_failure = %q, want auth/health", rep.FirstFailure)
		}
		if s := findCheck(t, rep, "auth", "signin"); s.Status != docOK {
			t.Errorf("auth/signin = %s, want ok (the store looks healthy; only the router knows)", s.Status)
		}
	})

	t.Run("refresh error is sanitized before terminal output", func(t *testing.T) {
		// last_refresh_error carries token-endpoint response bytes
		// verbatim; a hostile endpoint could inject ANSI sequences and
		// newlines that forge check lines, or flood the terminal.
		hostile := "oauth2: cannot fetch token\x1b[2K\n[ok]   auth: all healthy" + strings.Repeat("A", 5000)
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.authHealth = "refresh_failed"
			c.authLastError = hostile
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "auth", "health")
		if c.Status != docFail {
			t.Fatalf("auth/health = %s, want fail", c.Status)
		}
		if strings.ContainsAny(c.Finding, "\n\x1b\r") {
			t.Errorf("finding still contains control characters: %q", c.Finding)
		}
		if len(c.Finding) > 500 {
			t.Errorf("finding length = %d, want the server-controlled text capped", len(c.Finding))
		}
	})

	t.Run("transient error warns", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.authHealth = "error"
			c.authLastError = "dial tcp: i/o timeout"
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "auth", "health")
		if c.Status != docWarn || !strings.Contains(c.Finding, "transient") {
			t.Fatalf("auth/health = %s (%s), want transient warn", c.Status, c.Finding)
		}
		if rep.ExitCode != 0 {
			t.Errorf("exit = %d, a transient warn must not fail the chain", rep.ExitCode)
		}
	})

	t.Run("ok passes", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) { c.authHealth = "ok" })
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "auth", "health"); c.Status != docOK {
			t.Fatalf("auth/health = %s (%s), want ok", c.Status, c.Finding)
		}
	})

	t.Run("absent required field fails", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) { c.authHealth = "-" })
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "auth", "health")
		if c.Status != docFail ||
			!strings.Contains(c.Finding, "required health field") ||
			!strings.Contains(c.Fix, "baseten-switch up") {
			t.Fatalf("auth/health = %s (%s), fix %q; want current-contract failure", c.Status, c.Finding, c.Fix)
		}
	})

	t.Run("router down skips", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) { c.adminDown = true })
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "auth", "health"); c.Status != docSkip {
			t.Fatalf("auth/health = %s (%s), want skip with the router down", c.Status, c.Finding)
		}
	})
}

// --- two-CLI hazard ---------------------------------------------------------

// TestDoctorBasetenCLICheck covers the PATH scan for baseten installs:
// one CLI is ok, two warn naming both paths and versions because credential
// ownership becomes ambiguous,
// same-version duplicates still warn, and no CLI skips with the install
// hint. All hermetic via the basetenPATH seam; only fixture-written
// scripts are ever exec'd.
func TestDoctorBasetenCLICheck(t *testing.T) {
	t.Run("one CLI ok", func(t *testing.T) {
		newDoctorFixture(t, nil)
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "auth", "cli")
		if c.Status != docOK || !strings.Contains(c.Finding, "baseten 0.2.0") {
			t.Fatalf("auth/cli = %s (%s), want ok naming the version", c.Status, c.Finding)
		}
	})

	t.Run("two CLIs with different versions warn naming both", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.basetenVersions = []string{"baseten 0.1.0", "baseten 0.2.0"}
		})
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "auth", "cli")
		if c.Status != docWarn {
			t.Fatalf("auth/cli = %s (%s), want warn", c.Status, c.Finding)
		}
		for _, want := range []string{"baseten-cli-0", "baseten-cli-1", "baseten 0.1.0", "baseten 0.2.0", "incompatible credential-store formats"} {
			if !strings.Contains(c.Finding, want) {
				t.Errorf("finding %q missing %q", c.Finding, want)
			}
		}
		if !strings.Contains(c.Fix, "remove the stale copies") {
			t.Errorf("fix %q must say to remove the stale copies", c.Fix)
		}
		if rep.ExitCode != 0 {
			t.Errorf("exit = %d, the two-CLI warn must not fail the chain", rep.ExitCode)
		}
	})

	t.Run("two CLIs with the same version still warn", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) {
			c.basetenVersions = []string{"baseten 0.2.0", "baseten 0.2.0"}
		})
		rep := runDoctor(doctorOpts{})
		if c := findCheck(t, rep, "auth", "cli"); c.Status != docWarn {
			t.Fatalf("auth/cli = %s (%s), want warn on duplicate installs", c.Status, c.Finding)
		}
	})

	t.Run("no CLI skips with the install hint", func(t *testing.T) {
		newDoctorFixture(t, func(c *doctorFixtureCfg) { c.noBasetenCLI = true })
		rep := runDoctor(doctorOpts{})
		c := findCheck(t, rep, "auth", "cli")
		if c.Status != docSkip || !strings.Contains(c.Finding, basetenBrewHint) {
			t.Fatalf("auth/cli = %s (%s), want skip with the brew hint", c.Status, c.Finding)
		}
	})
}

// TestScanBasetenCLIsSkipsRelativePATHEntries pins the LookPath-parity
// policy: scan results are exec'd for --version, so a relative PATH
// entry (including ".") would resolve against doctor's cwd and run an
// untrusted checkout's bin/baseten with the user's privileges. Relative
// entries must never be scanned, reported, or exec'd.
func TestScanBasetenCLIsSkipsRelativePATHEntries(t *testing.T) {
	dir := t.TempDir()
	writeCLI := func(d string) string {
		t.Helper()
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, "baseten")
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho baseten 9.9.9\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	absCLI := writeCLI(filepath.Join(dir, "abs"))
	writeCLI(filepath.Join(dir, "rel")) // reachable only via the relative entry
	writeCLI(dir)                       // reachable only via "."
	t.Chdir(dir)

	oldPATH := basetenPATH
	basetenPATH = func() string {
		return strings.Join([]string{"rel", ".", filepath.Join(dir, "abs")}, string(os.PathListSeparator))
	}
	t.Cleanup(func() { basetenPATH = oldPATH })

	got := scanBasetenCLIs()
	if len(got) != 1 || got[0] != absCLI {
		t.Fatalf("scanBasetenCLIs = %v, want only the absolute entry %q", got, absCLI)
	}
}

// --- doctor --probe served-route assertion ----------------------------------

// The served-route tests pin the fix for the impact-map blind spot:
// doctor --probe used to validate "the harness gets answers", so door
// failover made it pass while the Baseten path was dead. The fake door
// answers the probe with the X-Baseten-Switch-Door marker and (optionally) appends
// the probe's telemetry row, emulating the router's write.

func TestDoctorProbeServedRouteMatches(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorProbe = "router"
		c.probeTelRoute = "baseten"
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	if c := findCheck(t, rep, "e2e", "probe:"+fx.doorPort); c.Status != docOK {
		t.Fatalf("probe = %s (%s), want ok", c.Status, c.Finding)
	}
	c := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if c.Status != docOK || !strings.Contains(c.Finding, "baseten") {
		t.Fatalf("route = %s (%s), want ok naming baseten", c.Status, c.Finding)
	}
	if rep.ExitCode != 0 {
		t.Errorf("exit = %d, want 0\nchecks: %+v", rep.ExitCode, rep.Checks)
	}
}

func TestDoctorProbeDoorFallbackFails(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorProbe = "fallback" // door failover served; the router never saw it
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	if c := findCheck(t, rep, "e2e", "probe:"+fx.doorPort); c.Status != docOK {
		t.Fatalf("probe = %s (%s), want ok (the answer itself succeeded)", c.Status, c.Finding)
	}
	c := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if c.Status != docFail {
		t.Fatalf("route = %s (%s), want fail", c.Status, c.Finding)
	}
	for _, want := range []string{"X-Baseten-Switch-Door: fallback", `"baseten"`} {
		if !strings.Contains(c.Finding, want) {
			t.Errorf("finding %q missing %q", c.Finding, want)
		}
	}
	if rep.ExitCode != 1 {
		t.Errorf("exit = %d, want 1 (a fallback-served probe must fail the chain)", rep.ExitCode)
	}
}

func TestDoctorFailedProbeNamesDoorFallback(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorProbe = "fallback"
		c.probeStatus = http.StatusUnauthorized
		c.probeError = `{"type":"error","error":{"message":"x-api-key header is required"}}`
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	probe := findCheck(t, rep, "e2e", "probe:"+fx.doorPort)
	if probe.Status != docFail || !strings.Contains(probe.Finding, "status 401, served via fallback") {
		t.Fatalf("probe = %+v, want failed fallback attribution", probe)
	}
	routeCheck := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if routeCheck.Status != docFail || !strings.Contains(routeCheck.Finding, "native failover served it") || !strings.Contains(routeCheck.Finding, `"baseten"`) {
		t.Fatalf("route = %+v, want configured-versus-fallback attribution", routeCheck)
	}
}

func TestDoctorFailedProbeUsesRouterTelemetry(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorProbe = "router"
		c.probeStatus = http.StatusServiceUnavailable
		c.probeError = `{"error":"synthetic upstream failure"}`
		c.probeTelRoute = "baseten"
		c.probeTelEffective = "anthropic"
		c.probeFallbackTrigger = "http_503\nunsafe-detail"
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	probe := findCheck(t, rep, "e2e", "probe:"+fx.doorPort)
	if probe.Status != docFail || !strings.Contains(probe.Finding, "status 503, served via router") {
		t.Fatalf("probe = %+v, want failed router attribution", probe)
	}
	routeCheck := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	for _, want := range []string{`route "anthropic"`, `configured route is "baseten"`, `fallback trigger "http_503 unsafe-detail"`} {
		if routeCheck.Status != docFail || !strings.Contains(routeCheck.Finding, want) {
			t.Fatalf("route = %+v, want safe telemetry detail %q", routeCheck, want)
		}
	}
	if strings.Contains(routeCheck.Finding, "\n") {
		t.Fatalf("route finding contains unsafe newline: %q", routeCheck.Finding)
	}
}

func TestDoctorFailedProbeReachedConfiguredRoute(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorProbe = "router"
		c.probeStatus = http.StatusUnauthorized
		c.probeError = `{"error":"credential rejected"}`
		c.probeTelRoute = "baseten"
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	routeCheck := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if routeCheck.Status != docOK || !strings.Contains(routeCheck.Finding, "failed probe reached the configured route (baseten), status 401") {
		t.Fatalf("route = %+v, want successful attribution of failed probe", routeCheck)
	}
}

func TestDoctorProbeRouterFallbackServedFails(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorProbe = "router"
		c.probeTelRoute = "baseten"
		c.probeTelEffective = "anthropic" // the router's fallback_route served it
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	c := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if c.Status != docFail {
		t.Fatalf("route = %s (%s), want fail", c.Status, c.Finding)
	}
	for _, want := range []string{`"anthropic"`, `"baseten"`} {
		if !strings.Contains(c.Finding, want) {
			t.Errorf("finding %q must name served vs configured (%q)", c.Finding, want)
		}
	}
}

func TestDoctorProbeNoTelemetryRowSkips(t *testing.T) {
	// The fixture's default telemetry row is FRESH (written at fixture
	// time, inside the probe window) but carries no requested_model: it
	// must not be mistaken for the probe's own row, so the check skips
	// instead of asserting against a concurrent request's route.
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorProbe = "router" // no probeTelRoute: the door appends no row
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	c := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if c.Status != docSkip || !strings.Contains(c.Finding, "no telemetry row") {
		t.Fatalf("route = %s (%s), want skip on a missing row", c.Status, c.Finding)
	}
}

// TestDoctorProbePinnedModelRouteOK pins the model_routes disambiguation:
// a route_effective value on the probe's row is NOT fallback evidence
// when a pin designates that route for the probe's model. Config: route
// baseten with opus pinned native; the probe's model is an opus id, so
// the row carries route_effective=anthropic on a fully healthy system,
// and the check must pass.
func TestDoctorProbePinnedModelRouteOK(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.modelRoutes = map[string]string{"opus": "native"}
		c.doorProbe = "router"
		c.probeTelRoute = "baseten"
		c.probeTelEffective = "anthropic" // the pin served it natively
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	c := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if c.Status != docOK {
		t.Fatalf("route = %s (%s), want ok on a pin-served probe", c.Status, c.Finding)
	}
	if !strings.Contains(c.Finding, "model_routes") {
		t.Errorf("finding %q must say the pin (not the configured route) was exercised", c.Finding)
	}
	if rep.ExitCode != 0 {
		t.Errorf("exit = %d, want 0 (pinned routing is a designed, healthy state)\nchecks: %+v", rep.ExitCode, rep.Checks)
	}
}

// TestDoctorProbeConcurrentRowNotAttributed pins the row-attribution fix:
// a concurrent harness request completing just after the probe (newer
// row, same client, fallback-served) must not be read as the probe's own
// row, which used to produce a false FAIL on a healthy configured path.
func TestDoctorProbeConcurrentRowNotAttributed(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorProbe = "router"
		c.probeTelRoute = "baseten" // the probe itself went over the primary
		c.probeConcurrentModel = "claude-haiku-4-5"
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	c := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if c.Status != docOK {
		t.Fatalf("route = %s (%s), want ok (the concurrent fallback-served row is not the probe's)", c.Status, c.Finding)
	}
	if rep.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", rep.ExitCode)
	}
}

// TestDoctorProbeUnstampedDoorFails enforces the current door contract. Without
// X-Baseten-Switch-Door, telemetry cannot be attributed safely to the probe.
func TestDoctorProbeUnstampedDoorFails(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorProbe = "none"        // door answers without the header
		c.probeTelRoute = "baseten" // a healthy-looking row exists anyway
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	c := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if c.Status != docFail ||
		!strings.Contains(c.Finding, "required X-Baseten-Switch-Door") ||
		!strings.Contains(c.Fix, "baseten-switch up") {
		t.Fatalf("route = %s (%s), fix %q; want current-contract failure", c.Status, c.Finding, c.Fix)
	}
}

// TestDoctorProbeNativeRouteAuthRejectionOK pins the native-passthrough
// contract: with routing off the probe goes to the native provider
// carrying no credential, so the provider's 401 is the expected answer
// on a healthy chain and must not fail doctor (README: doctor exits 0
// when nothing fails).
func TestDoctorProbeNativeRouteAuthRejectionOK(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.routingOff = true
		c.clientRoute = "anthropic" // keep the fake admin view consistent
		c.doorProbe = "router"
		c.probeStatus = http.StatusUnauthorized
		c.probeError = `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`
		c.probeTelRoute = "anthropic"
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	if c := findCheck(t, rep, "e2e", "probe:"+fx.doorPort); c.Status != docOK {
		t.Fatalf("probe = %s (%s), want ok: the provider's 401 proves the native path", c.Status, c.Finding)
	}
	// The served route is still attributed from the router's telemetry, so
	// the probe allowance cannot mask a router that answered elsewhere.
	c := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if c.Status != docOK || !strings.Contains(c.Finding, "reached the configured route (anthropic)") {
		t.Fatalf("route = %s (%s), want ok naming the native route", c.Status, c.Finding)
	}
	if rep.ExitCode != 0 {
		t.Errorf("exit = %d, want 0 on a healthy native-passthrough chain\nchecks: %+v", rep.ExitCode, rep.Checks)
	}
}

// TestDoctorProbePinnedNativeRouteAuthRejectionOK covers the same
// credential-less rejection with routing ON but the probe's model
// pinned native: the pin designates the native route, so a 401 from
// the provider is the healthy answer here too.
func TestDoctorProbePinnedNativeRouteAuthRejectionOK(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.modelRoutes = map[string]string{"opus": "native"}
		c.doorProbe = "router"
		c.probeStatus = http.StatusUnauthorized
		c.probeError = `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`
		c.probeTelRoute = "anthropic"
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	if c := findCheck(t, rep, "e2e", "probe:"+fx.doorPort); c.Status != docOK {
		t.Fatalf("probe = %s (%s), want ok on the pin-designated native route", c.Status, c.Finding)
	}
	c := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if c.Status != docOK || !strings.Contains(c.Finding, "reached the configured route (anthropic)") {
		t.Fatalf("route = %s (%s), want ok naming the native route", c.Status, c.Finding)
	}
}

// TestDoctorProbeNativeAuthRejectionRouterFallbackServedFails pins the
// boundary between the two halves of the probe verdict: the credential-less
// rejection may pass the probe check, but the served route is still
// attributed from the router's telemetry. A router that answered the probe
// from its fallback_route instead of the native route it was configured for
// must still fail the chain.
func TestDoctorProbeNativeAuthRejectionRouterFallbackServedFails(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.routingOff = true
		c.clientRoute = "anthropic"
		c.doorProbe = "router"
		c.probeStatus = http.StatusUnauthorized
		c.probeError = `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`
		c.probeTelRoute = "anthropic"
		c.probeTelEffective = "baseten" // the router's fallback_route served it
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	if c := findCheck(t, rep, "e2e", "probe:"+fx.doorPort); c.Status != docOK {
		t.Fatalf("probe = %s (%s), want ok: the rejection still came back through the door", c.Status, c.Finding)
	}
	c := findCheck(t, rep, "e2e", "route:"+fx.doorPort)
	if c.Status != docFail {
		t.Fatalf("route = %s (%s), want fail: the configured native route was not exercised", c.Status, c.Finding)
	}
	for _, want := range []string{`"baseten"`, `"anthropic"`} {
		if !strings.Contains(c.Finding, want) {
			t.Errorf("finding %q missing %q", c.Finding, want)
		}
	}
	if rep.ExitCode != 1 {
		t.Errorf("exit = %d, want 1 when the configured route never served the probe", rep.ExitCode)
	}
}

// TestDoctorProbeNativeRouteAuthRejectionDoorFallbackFails ensures a
// provider rejection cannot prove the router path when the door's native
// failover served the request instead.
func TestDoctorProbeNativeRouteAuthRejectionDoorFallbackFails(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.routingOff = true
		c.clientRoute = "anthropic"
		c.doorProbe = "fallback"
		c.probeStatus = http.StatusUnauthorized
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	if c := findCheck(t, rep, "e2e", "probe:"+fx.doorPort); c.Status != docFail {
		t.Fatalf("probe = %s (%s), want fail when native failover bypasses the router", c.Status, c.Finding)
	}
	if rep.ExitCode != 1 {
		t.Errorf("exit = %d, want 1", rep.ExitCode)
	}
}

// TestDoctorProbeNativeRouteAuthRejectionUnstampedFails ensures an
// unstamped provider rejection cannot be attributed to the router path.
func TestDoctorProbeNativeRouteAuthRejectionUnstampedFails(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.routingOff = true
		c.clientRoute = "anthropic"
		c.doorProbe = "none"
		c.probeStatus = http.StatusForbidden
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	if c := findCheck(t, rep, "e2e", "probe:"+fx.doorPort); c.Status != docFail {
		t.Fatalf("probe = %s (%s), want fail without router provenance", c.Status, c.Finding)
	}
	if rep.ExitCode != 1 {
		t.Errorf("exit = %d, want 1", rep.ExitCode)
	}
}

// TestDoctorProbeBasetenRouteAuthRejectionFails guards the boundary of
// the native allowance: on the baseten route a 401 means the Baseten
// credential itself is broken, which must keep failing doctor.
func TestDoctorProbeBasetenRouteAuthRejectionFails(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.doorProbe = "router"
		c.probeStatus = http.StatusUnauthorized
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	if c := findCheck(t, rep, "e2e", "probe:"+fx.doorPort); c.Status != docFail {
		t.Fatalf("probe = %s (%s), want fail: a baseten-route 401 is a broken credential", c.Status, c.Finding)
	}
	if rep.ExitCode != 1 {
		t.Errorf("exit = %d, want 1", rep.ExitCode)
	}
}

// TestDoctorProbeNativeRouteTransportErrorFails guards the other
// boundary: only an HTTP auth rejection from the far end counts as
// path-proving; a connection failure on a native route still fails.
func TestDoctorProbeNativeRouteTransportErrorFails(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.routingOff = true
		c.clientRoute = "anthropic"
		c.doorProbe = "router"
		c.probeStatus = http.StatusBadGateway // not an auth rejection
	})
	rep := runDoctor(doctorOpts{probe: true, yes: true, timeoutSec: 5})
	if c := findCheck(t, rep, "e2e", "probe:"+fx.doorPort); c.Status != docFail {
		t.Fatalf("probe = %s (%s), want fail on a non-auth error", c.Status, c.Finding)
	}
}

// TestDoctorClientForMatchesShape pins the shared-bind_addr resolution: a
// door port can front a bind_addr serving an openai-shape and an
// anthropic-shape client as one listener group, and config order must not
// decide which client the probe's route check compares against.
func TestDoctorClientForMatchesShape(t *testing.T) {
	f := &config.File{Clients: []config.Client{
		{Name: "codex", Enabled: true, BindAddr: "127.0.0.1:9081", ProtocolShape: "openai"},
		{Name: "claude-code", Enabled: true, BindAddr: "127.0.0.1:9081"}, // shape defaults to anthropic
	}}
	if c, ok := doctorClientFor(f, "127.0.0.1:9081", "anthropic"); !ok || c.Name != "claude-code" {
		t.Fatalf("anthropic probe resolved client %q (ok=%t), want claude-code", c.Name, ok)
	}
	if c, ok := doctorClientFor(f, "127.0.0.1:9081", "openai"); !ok || c.Name != "codex" {
		t.Fatalf("openai probe resolved client %q (ok=%t), want codex", c.Name, ok)
	}
	if _, ok := doctorClientFor(f, "127.0.0.1:9081", "monitor"); ok {
		t.Fatal("monitor shape must not resolve a client")
	}
}

// --- codex section -----------------------------------------------------------

// codexDoctorManagedOverlay is the adapter-written overlay pointing at
// the door port the codexDoctorConfig topology resolves (8081).
const codexDoctorManagedOverlay = `# Managed by baseten-switch ('baseten-switch codex on'); remove with 'baseten-switch codex off'.
model_provider = "baseten"
model = "` + gateway.CodexCompatibilityModel + `"

[model_providers.baseten]
name = "Baseten Switch (local gateway)"
base_url = "http://127.0.0.1:8081/v1"
wire_api = "responses"
`

// codexDoctorConfig builds the minimal gateway config the codex
// section resolves against: a codex client behind door port 8081 on
// the shared listener, matching testCodexAdapter's topology.
func codexDoctorConfig(mut func(*config.File)) *config.File {
	routingEnabled := true
	f := &config.File{
		Global: config.Global{
			RoutingEnabled: &routingEnabled,
		},
		Clients: []config.Client{{
			Name:          "codex",
			Enabled:       true,
			BindAddr:      "127.0.0.1:18081",
			ProtocolShape: "openai",
			DefaultModel:  "zai-org/GLM-5.2",
			ResponsesCompatibility: &config.ResponsesCompatibility{
				TextFormatDefault:            config.ResponsesCompatibilityModeOn,
				AdditionalToolsInput:         config.ResponsesCompatibilityModeOff,
				ReasoningEffort:              config.ResponsesCompatibilityModeOn,
				FunctionArgumentsConsistency: config.ResponsesCompatibilityModeOn,
			},
			ResponsesStripToolTypes: []string{},
		}},
		Door: &config.Door{Ports: []config.DoorPort{{BindAddr: "127.0.0.1:8081", RouterAddr: "127.0.0.1:18081"}}},
	}
	if mut != nil {
		mut(f)
	}
	return f
}

// collectCodexChecks runs doctorCodexChecks in isolation with every
// path seam pointed into codexHome (never the real ~/.codex or backup
// dir) and shellToken as the process-env CODEX_AUTH_TOKEN ("" = unset).
// The shell value is controllable so tests can prove it does not
// satisfy the Switch-managed gateway placeholder check.
func collectCodexChecks(t *testing.T, f *config.File, codexHome, shellToken string, envFile map[string]string) doctorReport {
	t.Helper()
	t.Setenv("BASETEN_SWITCH_CODEX_HOME", codexHome)
	t.Setenv("BASETEN_SWITCH_BACKUP_DIR", filepath.Join(codexHome, "backups"))
	t.Setenv(codexManagedEnvKey, shellToken)
	var rep doctorReport
	var add addCheck = func(section, name, status, finding, fix string, fixArgv ...string) {
		rep.Checks = append(rep.Checks, doctorCheck{Section: section, Name: name, Status: status, Finding: finding, Fix: fix, fixArgv: fixArgv})
	}
	doctorCodexChecks(add, f, envFile, filepath.Join(codexHome, "env"))
	return rep
}

func writeCodexHomeFile(t *testing.T, codexHome, name, content string) {
	t.Helper()
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorCodexSectionSkips: every path that cannot resolve the
// codex wiring skips ALL checks with one reason; none may fail or
// warn (codex wiring is additive, absence is a legitimate default).
func TestDoctorCodexSectionSkips(t *testing.T) {
	cases := []struct {
		name       string
		f          *config.File
		wantReason string
	}{
		{"config did not load", nil, "config did not load"},
		{"no openai-shape client", &config.File{Clients: []config.Client{{Name: "claude-code", Enabled: true, ProtocolShape: "anthropic"}}}, "no openai-shape client"},
		{"no door section", codexDoctorConfig(func(f *config.File) { f.Door = nil }), "cannot resolve the codex door port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := collectCodexChecks(t, tc.f, t.TempDir(), "", nil)
			if len(rep.Checks) != len(codexDoctorCheckNames) {
				t.Fatalf("got %d checks, want %d: %+v", len(rep.Checks), len(codexDoctorCheckNames), rep.Checks)
			}
			for _, name := range codexDoctorCheckNames {
				c := findCheck(t, rep, "codex", name)
				if c.Status != docSkip || !strings.Contains(c.Finding, tc.wantReason) {
					t.Errorf("codex/%s = %s (%s), want skip naming %q", name, c.Status, c.Finding, tc.wantReason)
				}
			}
		})
	}
}

// TestDoctorCodexOverlayStates covers the overlay check per file
// state: absent is OK (additive default), managed is OK, ours-shaped
// on a dead port is the FAIL with the codex-on self-fix, foreign
// content is a WARN with no automated fix ('codex on' would refuse).
func TestDoctorCodexOverlayStates(t *testing.T) {
	t.Run("absent is ok and dependents skip", func(t *testing.T) {
		rep := collectCodexChecks(t, codexDoctorConfig(nil), t.TempDir(), "", nil)
		if c := findCheck(t, rep, "codex", "overlay"); c.Status != docOK || !strings.Contains(c.Finding, "additive default") {
			t.Errorf("overlay = %s (%s), want ok naming the additive default", c.Status, c.Finding)
		}
		if c := findCheck(t, rep, "codex", "auth_token"); c.Status != docSkip {
			t.Errorf("auth_token = %s, want skip with no managed overlay", c.Status)
		}
		if c := findCheck(t, rep, "codex", "backup"); c.Status != docSkip {
			t.Errorf("backup = %s, want skip when not gateway-managed", c.Status)
		}
		if c := findCheck(t, rep, "codex", "client"); c.Status != docOK {
			t.Errorf("client = %s (%s), want ok", c.Status, c.Finding)
		}
	})
	t.Run("managed overlay is ok", func(t *testing.T) {
		home := t.TempDir()
		writeCodexHomeFile(t, home, codexOverlayName, codexDoctorManagedOverlay)
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home, "baseten-switch-local", nil)
		if c := findCheck(t, rep, "codex", "overlay"); c.Status != docOK || !strings.Contains(c.Finding, "points at the door") {
			t.Errorf("overlay = %s (%s), want ok pointing at the door", c.Status, c.Finding)
		}
	})
	t.Run("noncurrent model shape is unrecognized", func(t *testing.T) {
		home := t.TempDir()
		unrecognized := strings.Replace(
			codexDoctorManagedOverlay,
			gateway.CodexCompatibilityModel,
			"zai-org/GLM-5.2",
			1,
		)
		writeCodexHomeFile(t, home, codexOverlayName, unrecognized)
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home, "baseten-switch-local", nil)
		c := findCheck(t, rep, "codex", "overlay")
		if c.Status != docWarn ||
			!strings.Contains(c.Finding, "not baseten-switch-managed") {
			t.Errorf("overlay = %s (%s), fix %q; want generic unrecognized-file warning", c.Status, c.Finding, c.Fix)
		}
	})
	t.Run("stale ours-shaped overlay fails with self-fix", func(t *testing.T) {
		home := t.TempDir()
		writeCodexHomeFile(t, home, codexOverlayName, codexTestStaleOverlay) // base_url on dead port 9081
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home, "baseten-switch-local", nil)
		c := findCheck(t, rep, "codex", "overlay")
		if c.Status != docFail || !strings.Contains(c.Finding, ":9081") || !strings.Contains(c.Finding, ":8081") {
			t.Errorf("overlay = %s (%s), want fail naming both ports", c.Status, c.Finding)
		}
		if c.Fix != "baseten-switch codex on" || len(c.fixArgv) != 2 || c.fixArgv[0] != "codex" || c.fixArgv[1] != "on" {
			t.Errorf("fix = %q argv %v, want 'baseten-switch codex on' with [codex on]", c.Fix, c.fixArgv)
		}
		// Ours-shaped but not on a live gateway port: no backup expected.
		if c := findCheck(t, rep, "codex", "backup"); c.Status != docSkip {
			t.Errorf("backup = %s, want skip for a stale overlay", c.Status)
		}
	})
	t.Run("foreign overlay warns without self-fix", func(t *testing.T) {
		home := t.TempDir()
		writeCodexHomeFile(t, home, codexOverlayName, codexTestForeignOverlay)
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home, "", nil)
		c := findCheck(t, rep, "codex", "overlay")
		if c.Status != docWarn || !strings.Contains(c.Finding, "not baseten-switch-managed") {
			t.Errorf("overlay = %s (%s), want warn naming the foreign file", c.Status, c.Finding)
		}
		if len(c.fixArgv) != 0 {
			t.Errorf("foreign overlay carries fixArgv %v; 'codex on' would refuse", c.fixArgv)
		}
		if c := findCheck(t, rep, "codex", "auth_token"); c.Status != docSkip {
			t.Errorf("auth_token = %s, want skip for a foreign overlay", c.Status)
		}
	})
}

// TestDoctorCodexClientParked: parked with the overlay installed is
// the dead-route FAIL (manual fix only: the un-park flip needs
// interactive consent); parked without an overlay is a legitimate
// default and stays ok.
func TestDoctorCodexClientParked(t *testing.T) {
	parked := func(f *config.File) { f.Clients[0].Enabled = false }
	t.Run("parked with overlay is the dead-route fail", func(t *testing.T) {
		home := t.TempDir()
		writeCodexHomeFile(t, home, codexOverlayName, codexDoctorManagedOverlay)
		rep := collectCodexChecks(t, codexDoctorConfig(parked), home, "baseten-switch-local", nil)
		c := findCheck(t, rep, "codex", "client")
		if c.Status != docFail || !strings.Contains(c.Finding, "dead route") {
			t.Errorf("client = %s (%s), want fail naming the dead route", c.Status, c.Finding)
		}
		if len(c.fixArgv) != 0 {
			t.Errorf("parked fail carries fixArgv %v; the un-park flip needs interactive consent", c.fixArgv)
		}
		if !strings.Contains(c.Fix, "baseten-switch codex on") {
			t.Errorf("fix = %q, want it to name 'baseten-switch codex on'", c.Fix)
		}
	})
	t.Run("parked without overlay is ok", func(t *testing.T) {
		rep := collectCodexChecks(t, codexDoctorConfig(parked), t.TempDir(), "", nil)
		if c := findCheck(t, rep, "codex", "client"); c.Status != docOK || !strings.Contains(c.Finding, "parked") {
			t.Errorf("client = %s (%s), want ok naming the parked state", c.Status, c.Finding)
		}
	})
}

// TestDoctorCodexAuthToken: with a managed overlay the gateway
// placeholder must be in the Switch env file. A shell value does not
// satisfy the check because the profile does not read it. Missing
// placeholder state fails with the codex-on self-fix.
func TestDoctorCodexAuthToken(t *testing.T) {
	home := func(t *testing.T) string {
		d := t.TempDir()
		writeCodexHomeFile(t, d, codexOverlayName, codexDoctorManagedOverlay)
		return d
	}
	t.Run("env file", func(t *testing.T) {
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home(t), "", map[string]string{codexManagedEnvKey: "baseten-switch-local"})
		if c := findCheck(t, rep, "codex", "auth_token"); c.Status != docOK ||
			!strings.Contains(c.Finding, "Switch-managed gateway placeholder") {
			t.Errorf("auth_token = %s (%s), want ok from the env file", c.Status, c.Finding)
		}
	})
	t.Run("shell env does not satisfy gateway placeholder", func(t *testing.T) {
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home(t), "user-chosen", nil)
		c := findCheck(t, rep, "codex", "auth_token")
		if c.Status != docFail || !strings.Contains(c.Finding, "Switch-managed gateway placeholder") {
			t.Errorf("auth_token = %s (%s), want fail for missing env-file placeholder", c.Status, c.Finding)
		}
		if strings.Contains(c.Finding, "hard-errors") || strings.Contains(c.Fix, "export") {
			t.Errorf("doctor still describes a Codex shell requirement: finding=%q fix=%q", c.Finding, c.Fix)
		}
	})
	t.Run("missing placeholder fails with self-fix", func(t *testing.T) {
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home(t), "", nil)
		c := findCheck(t, rep, "codex", "auth_token")
		if c.Status != docFail || !strings.Contains(c.Finding, "gateway.yaml interpolates it") {
			t.Errorf("auth_token = %s (%s), want fail naming gateway interpolation", c.Status, c.Finding)
		}
		if strings.Contains(c.Finding, "hard-errors") || strings.Contains(c.Fix, "export") {
			t.Errorf("doctor still describes a Codex shell requirement: finding=%q fix=%q", c.Finding, c.Fix)
		}
		if len(c.fixArgv) != 2 || c.fixArgv[0] != "codex" || c.fixArgv[1] != "on" {
			t.Errorf("fixArgv = %v, want [codex on]", c.fixArgv)
		}
	})
}

// TestDoctorCodexConfigToml pins the additive-invariant peek: only a
// ROOT-scope model_provider = "baseten" in the user's config.toml warns;
// table-scoped values, other providers, and an absent file hold the
// invariant. The file is never written, only read.
func TestDoctorCodexConfigToml(t *testing.T) {
	cases := []struct {
		name       string
		content    string // "" = no config.toml
		wantStatus string
	}{
		{"absent", "", docOK},
		{"root flipped to baseten", "model = \"gpt-5\"\nmodel_provider = \"baseten\"\n", docWarn},
		{"root flipped with inline comment", "model_provider = \"baseten\" # oops\n", docWarn},
		{"other root provider", "model_provider = \"other\"\n", docOK},
		{"baseten only inside a table", "[profiles.x]\nmodel_provider = \"baseten\"\n", docOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.content != "" {
				writeCodexHomeFile(t, home, "config.toml", tc.content)
			}
			rep := collectCodexChecks(t, codexDoctorConfig(nil), home, "", nil)
			c := findCheck(t, rep, "codex", "config_toml")
			if c.Status != tc.wantStatus {
				t.Fatalf("config_toml = %s (%s), want %s", c.Status, c.Finding, tc.wantStatus)
			}
			if tc.wantStatus == docWarn && !strings.Contains(c.Finding, "EVERY codex session") {
				t.Errorf("warn does not name the flipped invariant: %s", c.Finding)
			}
			if tc.content != "" {
				if got := string(fileBytes(t, filepath.Join(home, "config.toml"))); got != tc.content {
					t.Errorf("doctor modified config.toml:\n%s", got)
				}
			}
		})
	}
}

// TestDoctorCodexBackupStates mirrors the claude backup check against
// a managed overlay: missing, poisoned, and unreadable backups warn;
// an intact one is ok.
func TestDoctorCodexBackupStates(t *testing.T) {
	setup := func(t *testing.T) (string, string) {
		home := t.TempDir()
		writeCodexHomeFile(t, home, codexOverlayName, codexDoctorManagedOverlay)
		overlayPath := filepath.Join(home, codexOverlayName)
		return home, codexBackupPath(filepath.Join(home, "backups"), overlayPath)
	}
	t.Run("intact backup ok", func(t *testing.T) {
		home, bakPath := setup(t)
		if err := saveCodexBackup(bakPath, &codexBackup{
			ConfigPath:  filepath.Join(home, codexOverlayName),
			Existed:     false,
			WrittenHash: sha256Hex([]byte(codexDoctorManagedOverlay)),
		}); err != nil {
			t.Fatal(err)
		}
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home, "baseten-switch-local", nil)
		if c := findCheck(t, rep, "codex", "backup"); c.Status != docOK {
			t.Errorf("backup = %s (%s), want ok", c.Status, c.Finding)
		}
	})
	t.Run("no backup warns", func(t *testing.T) {
		home, _ := setup(t)
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home, "baseten-switch-local", nil)
		if c := findCheck(t, rep, "codex", "backup"); c.Status != docWarn || !strings.Contains(c.Finding, "no backup recorded") {
			t.Errorf("backup = %s (%s), want warn for the missing backup", c.Status, c.Finding)
		}
	})
	t.Run("poisoned backup warns", func(t *testing.T) {
		home, bakPath := setup(t)
		poison := strings.ReplaceAll(codexDoctorManagedOverlay, "127.0.0.1:8081", "127.0.0.1:18081")
		if err := saveCodexBackup(bakPath, &codexBackup{
			ConfigPath:  filepath.Join(home, codexOverlayName),
			Content:     []byte(poison),
			Existed:     true,
			WrittenHash: sha256Hex([]byte(codexDoctorManagedOverlay)),
		}); err != nil {
			t.Fatal(err)
		}
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home, "baseten-switch-local", nil)
		if c := findCheck(t, rep, "codex", "backup"); c.Status != docWarn || !strings.Contains(c.Finding, "poisoned") {
			t.Errorf("backup = %s (%s), want warn naming the poison", c.Status, c.Finding)
		}
	})
	t.Run("unreadable backup warns", func(t *testing.T) {
		home, bakPath := setup(t)
		if err := os.MkdirAll(filepath.Dir(bakPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bakPath, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		rep := collectCodexChecks(t, codexDoctorConfig(nil), home, "baseten-switch-local", nil)
		if c := findCheck(t, rep, "codex", "backup"); c.Status != docWarn || !strings.Contains(c.Finding, "unreadable") {
			t.Errorf("backup = %s (%s), want warn for the unreadable backup", c.Status, c.Finding)
		}
	})
}

// TestDoctorCodexStripKnob: the stable check name reports capability-degrading
// tool denylist overrides and invalid compatibility modes.
func TestDoctorCodexStripKnob(t *testing.T) {
	t.Run("Preview-safe config ok", func(t *testing.T) {
		rep := collectCodexChecks(t, codexDoctorConfig(nil), t.TempDir(), "", nil)
		if c := findCheck(t, rep, "codex", "strip_knob"); c.Status != docOK || !strings.Contains(c.Finding, "safe Responses compatibility defaults") {
			t.Errorf("strip_knob = %s (%s), want ok with no lossy overrides", c.Status, c.Finding)
		}
	})
	t.Run("absent compatibility block ok", func(t *testing.T) {
		f := codexDoctorConfig(func(f *config.File) { f.Clients[0].ResponsesCompatibility = nil })
		rep := collectCodexChecks(t, f, t.TempDir(), "", nil)
		if c := findCheck(t, rep, "codex", "strip_knob"); c.Status != docOK {
			t.Errorf("strip_knob = %s (%s), want ok for absent-block all-off config", c.Status, c.Finding)
		}
	})
	t.Run("non-empty denylist warns even while routing off", func(t *testing.T) {
		f := codexDoctorConfig(func(f *config.File) {
			f.Clients[0].ResponsesStripToolTypes = []string{"tool_search"}
			*f.Global.RoutingEnabled = false
		})
		rep := collectCodexChecks(t, f, t.TempDir(), "", nil)
		c := findCheck(t, rep, "codex", "strip_knob")
		if c.Status != docWarn || !strings.Contains(c.Finding, "tool_search") {
			t.Errorf("strip_knob = %s (%s), want warn naming explicit denylist", c.Status, c.Finding)
		}
		if !strings.Contains(c.Fix, "responses_strip_tool_types: []") {
			t.Errorf("fix = %q, want empty denylist guidance", c.Fix)
		}
	})
	t.Run("experimental additional tools mode warns", func(t *testing.T) {
		f := codexDoctorConfig(func(f *config.File) {
			f.Clients[0].ResponsesCompatibility.AdditionalToolsInput = config.ResponsesCompatibilityModeOn
		})
		rep := collectCodexChecks(t, f, t.TempDir(), "", nil)
		c := findCheck(t, rep, "codex", "strip_knob")
		if c.Status != docWarn ||
			!strings.Contains(c.Finding, "experimental responses_compatibility.additional_tools_input") ||
			!strings.Contains(c.Fix, "additional_tools_input: off") {
			t.Errorf("strip_knob = %s (%s), fix %q, want experimental-mode warning", c.Status, c.Finding, c.Fix)
		}
	})
	t.Run("invalid additional tools mode warns", func(t *testing.T) {
		f := codexDoctorConfig(func(f *config.File) {
			f.Clients[0].ResponsesCompatibility.AdditionalToolsInput = config.ResponsesCompatibilityMode("invalid")
		})
		rep := collectCodexChecks(t, f, t.TempDir(), "", nil)
		c := findCheck(t, rep, "codex", "strip_knob")
		if c.Status != docWarn ||
			!strings.Contains(c.Finding, "invalid responses_compatibility") ||
			!strings.Contains(c.Fix, "correct the responses_compatibility block") {
			t.Errorf("strip_knob = %s (%s), fix %q, want invalid-mode warning", c.Status, c.Finding, c.Fix)
		}
	})
}

func TestCodexRootModelProvider(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"root value", "model_provider = \"baseten\"\n", "baseten"},
		{"inline comment stripped", "model_provider = \"baseten\" # note\n", "baseten"},
		{"comment line ignored", "# model_provider = \"baseten\"\n", ""},
		{"table-scoped ignored", "[profiles.x]\nmodel_provider = \"baseten\"\n", ""},
		{"root before table wins", "model_provider = \"other\"\n[profiles.x]\nmodel_provider = \"baseten\"\n", "other"},
		{"absent", "model = \"gpt-5\"\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexRootModelProvider([]byte(tc.raw)); got != tc.want {
				t.Errorf("codexRootModelProvider(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestDoctorCodexSkipsWithoutCodexConfig: the full chain on a machine
// with no codex wiring at all (the default fixture has only the
// anthropic-shape claude client) produces SKIPs for every codex
// check, never failures.
func TestDoctorCodexSkipsWithoutCodexConfig(t *testing.T) {
	newDoctorFixture(t, nil)
	rep := runDoctor(doctorOpts{})
	if rep.ExitCode != 0 {
		t.Fatalf("exit = %d first_failure = %q", rep.ExitCode, rep.FirstFailure)
	}
	for _, name := range codexDoctorCheckNames {
		c := findCheck(t, rep, "codex", name)
		if c.Status != docSkip || !strings.Contains(c.Finding, "no openai-shape client") {
			t.Errorf("codex/%s = %s (%s), want skip naming the missing client", name, c.Status, c.Finding)
		}
	}
}

// TestDoctorCodexGreenInChain: the full chain with the codex client
// wired and a healthy managed overlay reports every codex check ok.
func TestDoctorCodexGreenInChain(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) { c.codexClient = true })
	overlay := strings.ReplaceAll(codexDoctorManagedOverlay, ":8081", ":"+fx.doorPort)
	writeCodexHomeFile(t, fx.codexHome, codexOverlayName, overlay)
	overlayPath := filepath.Join(fx.codexHome, codexOverlayName)
	if err := saveCodexBackup(codexBackupPath(os.Getenv("BASETEN_SWITCH_BACKUP_DIR"), overlayPath), &codexBackup{
		ConfigPath:  overlayPath,
		Existed:     false,
		WrittenHash: sha256Hex([]byte(overlay)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendEnvLine(os.Getenv("BASETEN_SWITCH_ENV_FILE"), codexManagedEnvKey, codexAuthTokenStub); err != nil {
		t.Fatal(err)
	}
	rep := runDoctor(doctorOpts{})
	if rep.ExitCode != 0 {
		t.Fatalf("exit = %d first_failure = %q\nchecks: %+v", rep.ExitCode, rep.FirstFailure, rep.Checks)
	}
	for _, name := range codexDoctorCheckNames {
		if c := findCheck(t, rep, "codex", name); c.Status != docOK {
			t.Errorf("codex/%s = %s (%s), want ok", name, c.Status, c.Finding)
		}
	}
}

// TestDoctorCodexParkedDeadRouteInChain: through the full chain, an
// installed overlay with the gateway client parked is the codex
// section's first failure.
func TestDoctorCodexParkedDeadRouteInChain(t *testing.T) {
	fx := newDoctorFixture(t, func(c *doctorFixtureCfg) { c.codexClient = true })
	// Re-park the codex client in the fixture config (the fixture writes
	// it enabled) without disturbing the rest of the chain.
	if err := config.SetClientScalars(fx.cfgPath, "codex", map[string]string{"enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	overlay := strings.ReplaceAll(codexDoctorManagedOverlay, ":8081", ":"+fx.doorPort)
	writeCodexHomeFile(t, fx.codexHome, codexOverlayName, overlay)
	rep := runDoctor(doctorOpts{})
	if rep.ExitCode != 1 || rep.FirstFailure != "codex/client" {
		t.Fatalf("exit = %d first_failure = %q, want 1 and codex/client", rep.ExitCode, rep.FirstFailure)
	}
	c := findCheck(t, rep, "codex", "client")
	if c.Status != docFail || !strings.Contains(c.Finding, "dead route") {
		t.Errorf("client = %s (%s), want fail naming the dead route", c.Status, c.Finding)
	}
}
