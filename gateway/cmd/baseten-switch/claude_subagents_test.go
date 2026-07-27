package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

// All tests operate exclusively on t.TempDir() paths through the same
// seams as claude_adapter_test.go; the real ~/.claude and
// ~/.config/baseten-switch are never read or written. The subagents verb is
// config-edit (gateway.yaml), not settings-write, so these tests build a
// temp gateway.yaml via BASETEN_SWITCH_CONFIG_PATH and exercise the verb through
// cmdClaude (the real entry point) with a stub signalRouter and a fake
// admin server for the verify poll (or a router-down path).

// subagentTestEnv bundles the temp paths and stubs for one subagents
// test. The fake admin server (when non-nil) lets the SIGHUP verify poll
// succeed; a nil server models the router-down path.
type subagentTestEnv struct {
	cfgPath   string
	settings  string
	backupDir string
	sigPids   *[]int
	sigMu     *sync.Mutex
}

// newSubagentTestEnv sets up a hermetic subagents test: temp gateway.yaml
// with a claude-code client (anthropic shape, model_aliases, a door
// port), temp settings.json pointing at the door, a stub signalRouter
// recorder, and optionally a fake admin server for the verify poll.
// Pass adminDown=true for the router-down path (no admin server, no
// pidfile so classifyPidfile returns not-alive).
func newSubagentTestEnv(t *testing.T, adminDown bool) *subagentTestEnv {
	t.Helper()
	dir := t.TempDir()

	cfgPath := filepath.Join(dir, "gateway.yaml")

	// Fake admin server: serves /v1/admin/status with the claude-code
	// client reporting the subagent_model and subagent_routing from the
	// config file. The handler re-reads gateway.yaml on each request so
	// it reflects the verb's config edit.
	mux := http.NewServeMux()
	ordering := &fakeMutationOrdering{}
	mux.HandleFunc("/v1/admin/status", func(w http.ResponseWriter, r *http.Request) {
		subModel := readSubagentModelFromConfig(t, cfgPath)
		subRouting := readSubagentRoutingFromConfig(t, cfgPath)
		hash, generation := ordering.observe(t, cfgPath)
		fmt.Fprintf(w, `{"uptime_seconds":42,"version":%q,
			"router_pid":%d,"router_boot_id":"subagent-test-router","active_generation":%d,
			"active_config_hash":%q,"desired_config_hash":%q,"config_path":%q,
			"capabilities":["global_routing"],"global_routing_enabled":true,
			"auth":{"signed_in":true,"profile":"doc","fallback_enabled":false,"fallback_in_use":false},
			"clients":[{"name":"claude-code","enabled":true,"bind_addr":"127.0.0.1:18081","protocol_shape":"anthropic",
				"effective_route":"baseten","fallback_route":"anthropic","auth_set":true,"currently_bound":true,
				"subagent_model":%q,"subagent_routing":%q}]}`,
			version.Version, os.Getpid(), generation, hash, hash, cfgPath,
			subModel, subRouting)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":true,"uptime_seconds":42,"version":%q}`, version.Version)
	})
	if !adminDown {
		adminSrv := httptest.NewServer(mux)
		t.Cleanup(adminSrv.Close)
		t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", hostPort(t, adminSrv.URL))
	} else {
		t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", closedPortAddr(t))
	}

	// Stub signalRouter to record SIGHUPs instead of signaling a real
	// process.
	var mu sync.Mutex
	var pids []int
	prevSig := signalRouter
	signalRouter = func(pid int) error {
		mu.Lock()
		pids = append(pids, pid)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { signalRouter = prevSig })

	// For the alive-router path, write a pidfile with the current pid
	// so classifyPidfile returns pidfileAlive. For router-down, leave no
	// pidfile.
	gwPid := filepath.Join(dir, "gw.pid")
	if !adminDown {
		if err := os.WriteFile(gwPid, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", gwPid)

	// gateway.yaml with a claude-code client.
	cfg := `# subagent test config
global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
    fallback_route: anthropic
    model_aliases:
      claude-baseten-glm-5-2: zai-org/GLM-5.2
      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2-7
door:
  ports:
    - bind_addr: 127.0.0.1:8081
      router_addr: 127.0.0.1:18081
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", cfgPath)

	// settings.json pointing at the door (wired state).
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CLAUDE_SETTINGS", settings)

	backupDir := filepath.Join(dir, "backups")
	t.Setenv("BASETEN_SWITCH_BACKUP_DIR", backupDir)

	// Remaining seams.
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(dir, "door.pid"))
	t.Setenv("BASETEN_SWITCH_ENV_FILE", filepath.Join(dir, "env"))
	t.Setenv("BASETEN_SWITCH_LAUNCHD", "off")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("BASETEN_API_KEY", "")
	t.Setenv(claudeSubagentEnvKey, "")

	return &subagentTestEnv{
		cfgPath:   cfgPath,
		settings:  settings,
		backupDir: backupDir,
		sigPids:   &pids,
		sigMu:     &mu,
	}
}

// readSubagentModelFromConfig extracts subagent_model from a gateway.yaml
// via the config package (the fake admin server uses it to mirror the
// real admin status).
func readSubagentModelFromConfig(t *testing.T, cfgPath string) string {
	t.Helper()
	f, _, err := loadGatewayConfigForAdapter()
	if err != nil {
		return ""
	}
	name, err := claudeTargetClientName(f)
	if err != nil {
		return ""
	}
	for i := range f.Clients {
		if f.Clients[i].Name == name {
			return f.Clients[i].SubagentModel
		}
	}
	return ""
}

// readSubagentRoutingFromConfig extracts subagent_routing from a
// gateway.yaml via the config package (the fake admin server uses it to
// mirror the real admin status).
func readSubagentRoutingFromConfig(t *testing.T, cfgPath string) string {
	t.Helper()
	f, _, err := loadGatewayConfigForAdapter()
	if err != nil {
		return ""
	}
	name, err := claudeTargetClientName(f)
	if err != nil {
		return ""
	}
	for i := range f.Clients {
		if f.Clients[i].Name == name {
			return f.Clients[i].SubagentRouting
		}
	}
	return ""
}

// sigCount returns the number of recorded SIGHUPs.
func (e *subagentTestEnv) sigCount() int {
	e.sigMu.Lock()
	defer e.sigMu.Unlock()
	return len(*e.sigPids)
}

// cfgSubagentModel reads subagent_model from the temp gateway.yaml.
func cfgSubagentModel(t *testing.T, cfgPath string) string {
	t.Helper()
	f, _, err := loadGatewayConfigForAdapter()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	name, err := claudeTargetClientName(f)
	if err != nil {
		t.Fatalf("resolve client: %v", err)
	}
	for i := range f.Clients {
		if f.Clients[i].Name == name {
			return f.Clients[i].SubagentModel
		}
	}
	t.Fatalf("client %s not found", name)
	return ""
}

// cfgSubagentRouting reads subagent_routing from the temp gateway.yaml.
func cfgSubagentRouting(t *testing.T, cfgPath string) string {
	t.Helper()
	f, _, err := loadGatewayConfigForAdapter()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	name, err := claudeTargetClientName(f)
	if err != nil {
		t.Fatalf("resolve client: %v", err)
	}
	for i := range f.Clients {
		if f.Clients[i].Name == name {
			return f.Clients[i].SubagentRouting
		}
	}
	t.Fatalf("client %s not found", name)
	return ""
}

// runClaudeCaptured runs cmdClaude and returns (code, stdout, stderr).
func runClaudeCaptured(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wOut
	os.Stderr = wErr
	code := cmdClaude(args)
	wOut.Close()
	wErr.Close()
	os.Stdout = oldOut
	os.Stderr = oldErr
	outBytes, _ := io.ReadAll(rOut)
	errBytes, _ := io.ReadAll(rErr)
	rOut.Close()
	rErr.Close()
	return code, string(outBytes), string(errBytes)
}

// TestSubagentsSetAliasWritesConfig runs `subagents <alias>` through
// cmdClaude and asserts the config is written, SIGHUP fires, and the
// verify poll succeeds.
func TestSubagentsSetAliasWritesConfig(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"})
	if code != 0 {
		t.Fatalf("subagents set = %d (%s)", code, stderr)
	}
	if m := cfgSubagentModel(t, e.cfgPath); m != "claude-baseten-glm-5-2" {
		t.Errorf("subagent_model = %q, want claude-baseten-glm-5-2", m)
	}
	if r := cfgSubagentRouting(t, e.cfgPath); r != "on" {
		t.Errorf("subagent_routing = %q, want on", r)
	}
	if e.sigCount() == 0 {
		t.Error("no SIGHUP recorded")
	}
	if !strings.Contains(stderr, "verified live") {
		t.Errorf("verify poll message missing: %q", stderr)
	}
}

// TestSubagentsSetSlugAcceptedWithNote runs `subagents <slug>` and
// asserts the slug note prints.
func TestSubagentsSetSlugAcceptedWithNote(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "zai-org/GLM-5.2"})
	if code != 0 {
		t.Fatalf("subagents set slug = %d (%s)", code, stderr)
	}
	if m := cfgSubagentModel(t, e.cfgPath); m != "zai-org/GLM-5.2" {
		t.Errorf("subagent_model = %q, want zai-org/GLM-5.2", m)
	}
	if !strings.Contains(stderr, "route explicitly to Baseten") {
		t.Errorf("slug note missing: %q", stderr)
	}
}

// TestSubagentsSetNativeIdAcceptedWithNote runs `subagents <native>` and
// asserts the native-id note prints.
func TestSubagentsSetNativeIdAcceptedWithNote(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "claude-haiku-4-5"})
	if code != 0 {
		t.Fatalf("subagents set native = %d (%s)", code, stderr)
	}
	if m := cfgSubagentModel(t, e.cfgPath); m != "claude-haiku-4-5" {
		t.Errorf("subagent_model = %q, want claude-haiku-4-5", m)
	}
	if !strings.Contains(stderr, "switch position") {
		t.Errorf("native-id note missing: %q", stderr)
	}
}

// TestSubagentsUnknownAliasExits1 asserts an unknown alias exits 1
// listing the configured aliases, and does not modify the config.
func TestSubagentsUnknownAliasExits1(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	before := fileBytes(t, e.cfgPath)
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "claude-baseten-nope"})
	if code != 1 {
		t.Fatalf("unknown alias = %d, want 1 (%s)", code, stderr)
	}
	for _, want := range []string{"unknown gateway model", "claude-baseten-glm-5-2", "claude-baseten-kimi-k2-7", "model_aliases"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("error output missing %q: %q", want, stderr)
		}
	}
	if !bytes.Equal(before, fileBytes(t, e.cfgPath)) {
		t.Error("failed set modified the config file")
	}
}

// TestSubagentsOnFlipsRouting keeps the model and flips routing on.
func TestSubagentsOnFlipsRouting(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	if code, _, _ := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"}); code != 0 {
		t.Fatal("set failed")
	}
	if code, _, _ := runClaudeCaptured(t, []string{"subagents", "off"}); code != 0 {
		t.Fatal("off failed")
	}
	if r := cfgSubagentRouting(t, e.cfgPath); r != "off" {
		t.Fatalf("routing = %q, want off", r)
	}
	code, _, _ := runClaudeCaptured(t, []string{"subagents", "on"})
	if code != 0 {
		t.Fatalf("on = %d", code)
	}
	if r := cfgSubagentRouting(t, e.cfgPath); r != "on" {
		t.Errorf("routing = %q, want on", r)
	}
	if m := cfgSubagentModel(t, e.cfgPath); m != "claude-baseten-glm-5-2" {
		t.Errorf("model changed on routing flip: %q", m)
	}
}

// TestSubagentsOnWithoutModelExits1 asserts "on" with no subagent_model
// exits 1 naming the fix.
func TestSubagentsOnWithoutModelExits1(t *testing.T) {
	_ = newSubagentTestEnv(t, false)
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "on"})
	if code != 1 {
		t.Fatalf("on without model = %d, want 1 (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "no subagent_model configured") {
		t.Errorf("error must name the fix: %q", stderr)
	}
}

// TestSubagentsOffWithoutModelNoop asserts "off" with no subagent_model
// is a noop: it prints an already-inherit message, exits 0, writes nothing
// to the config, and the resulting file still loads via config.Load and
// would pass gateway validation (no subagent_routing set while the
// model is empty).
func TestSubagentsOffWithoutModelNoop(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	before := fileBytes(t, e.cfgPath)
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "off"})
	if code != 0 {
		t.Fatalf("off without model = %d, want 0 (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "already inherit") || !strings.Contains(stderr, "no subagent_model configured") {
		t.Errorf("already-inherit message missing: %q", stderr)
	}
	if !bytes.Equal(before, fileBytes(t, e.cfgPath)) {
		t.Errorf("noop off modified the config file")
	}
	// The config must still load and not carry the rejected shape
	// (subagent_routing set while subagent_model empty).
	f, err := config.Load(e.cfgPath)
	if err != nil {
		t.Fatalf("config.Load after noop off: %v", err)
	}
	name, err := claudeTargetClientName(f)
	if err != nil {
		t.Fatalf("resolve client: %v", err)
	}
	for i := range f.Clients {
		if f.Clients[i].Name == name {
			if f.Clients[i].SubagentModel == "" && f.Clients[i].SubagentRouting != "" {
				t.Errorf("config has subagent_routing=%q with empty subagent_model; the router would refuse it", f.Clients[i].SubagentRouting)
			}
			break
		}
	}
}

// TestSubagentsOffFlipsRouting keeps the model.
func TestSubagentsOffFlipsRouting(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	if code, _, _ := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"}); code != 0 {
		t.Fatal("set failed")
	}
	code, _, _ := runClaudeCaptured(t, []string{"subagents", "off"})
	if code != 0 {
		t.Fatalf("off = %d", code)
	}
	if r := cfgSubagentRouting(t, e.cfgPath); r != "off" {
		t.Errorf("routing = %q, want off", r)
	}
	if m := cfgSubagentModel(t, e.cfgPath); m != "claude-baseten-glm-5-2" {
		t.Errorf("model lost on off: %q", m)
	}
}

// TestSubagentsInheritAlias asserts "inherit" is an accepted alias for
// "off": same config write (subagent_routing: off, model kept) and the
// confirmation names the inherit state.
func TestSubagentsInheritAlias(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	if code, _, _ := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"}); code != 0 {
		t.Fatal("set failed")
	}
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "inherit"})
	if code != 0 {
		t.Fatalf("inherit = %d (%s)", code, stderr)
	}
	if r := cfgSubagentRouting(t, e.cfgPath); r != "off" {
		t.Errorf("routing = %q, want off (the wire value for inherit)", r)
	}
	if m := cfgSubagentModel(t, e.cfgPath); m != "claude-baseten-glm-5-2" {
		t.Errorf("model lost on inherit: %q", m)
	}
	if !strings.Contains(stderr, "claude subagents: inherit") ||
		!strings.Contains(stderr, "no subagent-specific model rewrite") ||
		!strings.Contains(stderr, "family mappings or the unmatched-model default") {
		t.Errorf("confirmation must explain the inherit state: %q", stderr)
	}
}

// TestSubagentsRoutingFlipVerifiesLive asserts a routing flip (on->off)
// verifies live when the fake admin reports the new routing. The verify
// poll must require BOTH the model and the routing to match, not just
// the model (which is unchanged by a routing flip).
func TestSubagentsRoutingFlipVerifiesLive(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	if code, _, _ := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"}); code != 0 {
		t.Fatal("set failed")
	}
	// Flip off: the model is unchanged, so a poll that only checks the
	// model would falsely verify. The fake admin re-reads the config, so
	// it reports the new routing and the verify must succeed.
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "off"})
	if code != 0 {
		t.Fatalf("off = %d (%s)", code, stderr)
	}
	if r := cfgSubagentRouting(t, e.cfgPath); r != "off" {
		t.Errorf("routing = %q, want off", r)
	}
	if !strings.Contains(stderr, "verified live") {
		t.Errorf("verify poll message missing on routing flip: %q", stderr)
	}
}

// TestSubagentsRoutingFlipStaleAdminTimesOut asserts that when the fake
// admin keeps reporting the OLD routing (the SIGHUP never applied), the
// verify poll times out and prints a calm (non-scary) notice rather than
// a false "verified live". The notice must not say "warning".
func TestSubagentsRoutingFlipStaleAdminTimesOut(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.yaml")

	// Set a model first by writing the config directly, so the routing
	// flip has a model to keep.
	cfg := `# subagent test config
global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
    fallback_route: anthropic
    subagent_model: claude-baseten-glm-5-2
    model_aliases:
      claude-baseten-glm-5-2: zai-org/GLM-5.2
      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2-7
door:
  ports:
    - bind_addr: 127.0.0.1:8081
      router_addr: 127.0.0.1:18081
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	priorHash := exactConfigHash([]byte(cfg))
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", cfgPath)

	// Fake admin that ALWAYS reports routing "on" (stale: never reflects
	// the off flip the verb writes).
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/admin/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"uptime_seconds":42,"version":%q,
			"router_pid":%d,"router_boot_id":"stale-subagent-router","active_generation":1,
			"active_config_hash":%q,"desired_config_hash":%q,"config_path":%q,
			"capabilities":["global_routing"],"global_routing_enabled":true,
			"auth":{"signed_in":true,"profile":"doc","fallback_enabled":false,"fallback_in_use":false},
			"clients":[{"name":"claude-code","enabled":true,"bind_addr":"127.0.0.1:18081","protocol_shape":"anthropic",
				"effective_route":"baseten","fallback_route":"anthropic","auth_set":true,"currently_bound":true,
				"subagent_model":"claude-baseten-glm-5-2","subagent_routing":"on"}]}`,
			version.Version, os.Getpid(), priorHash, priorHash, cfgPath)
	})
	adminSrv := httptest.NewServer(mux)
	t.Cleanup(adminSrv.Close)
	t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", hostPort(t, adminSrv.URL))

	// Stub signalRouter and write a pidfile so the alive-router path runs.
	var mu sync.Mutex
	var pids []int
	prevSig := signalRouter
	signalRouter = func(pid int) error {
		mu.Lock()
		pids = append(pids, pid)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { signalRouter = prevSig })
	gwPid := filepath.Join(dir, "gw.pid")
	if err := os.WriteFile(gwPid, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", gwPid)

	// Shorten the timeout so the test does not hang.
	old := routeApplyTimeout
	routeApplyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { routeApplyTimeout = old })

	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CLAUDE_SETTINGS", settings)
	t.Setenv("BASETEN_SWITCH_BACKUP_DIR", filepath.Join(dir, "backups"))
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(dir, "door.pid"))
	t.Setenv("BASETEN_SWITCH_ENV_FILE", filepath.Join(dir, "env"))
	t.Setenv("BASETEN_SWITCH_LAUNCHD", "off")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("BASETEN_API_KEY", "")
	t.Setenv(claudeSubagentEnvKey, "")

	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "off"})
	if code != 1 {
		t.Fatalf("off = %d, want rollback failure 1 (%s)", code, stderr)
	}
	if r := cfgSubagentRouting(t, cfgPath); r != "" {
		t.Errorf("stale activation did not restore prior routing: %q", r)
	}
	if strings.Contains(stderr, "verified live") {
		t.Errorf("stale admin must not verify: %q", stderr)
	}
	if !strings.Contains(stderr, "restored and reactivated the prior exact config") {
		t.Errorf("rollback confirmation missing: %q", stderr)
	}
	// The notice must be calm, not scary: no "warning" prefix.
	if strings.Contains(stderr, "warning: SIGHUP sent") {
		t.Errorf("non-verified notice is scary (warning): %q", stderr)
	}
}

// TestSubagentsStatusPrintsConfigState asserts bare `subagents` reads
// state from gateway.yaml.
func TestSubagentsStatusPrintsConfigState(t *testing.T) {
	_ = newSubagentTestEnv(t, false)
	code, stdout, _ := runClaudeCaptured(t, []string{"subagents"})
	if code != 0 {
		t.Fatalf("bare subagents = %d", code)
	}
	if !strings.Contains(stdout, "unmanaged") {
		t.Errorf("unmanaged state missing: %q", stdout)
	}
	if code, _, _ := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"}); code != 0 {
		t.Fatal("set failed")
	}
	code, stdout, _ = runClaudeCaptured(t, []string{"subagents"})
	if code != 0 {
		t.Fatalf("bare subagents = %d", code)
	}
	if !strings.Contains(stdout, "claude-baseten-glm-5-2") || !strings.Contains(stdout, "configured in model_aliases") {
		t.Errorf("managed state missing class/config info: %q", stdout)
	}
}

// TestSubagentsStatusShowsRouting asserts the status line includes the
// routing state, with the off wire value displayed as inherit.
func TestSubagentsStatusShowsRouting(t *testing.T) {
	_ = newSubagentTestEnv(t, false)
	runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"})
	runClaudeCaptured(t, []string{"subagents", "off"})
	_, stdout, _ := runClaudeCaptured(t, []string{"subagents"})
	if !strings.Contains(stdout, "routing inherit") {
		t.Errorf("routing inherit missing: %q", stdout)
	}
}

// TestSubagentsStatusShowsWiringWarn asserts bare `subagents` prints the
// wiring-off warning (via subagentsWarnings) when claude wiring is off,
// alongside the status label. The warning goes to stderr (a.out).
func TestSubagentsStatusShowsWiringWarn(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	// Point settings away from the door so wiring reads as off.
	if err := os.WriteFile(e.settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runClaudeCaptured(t, []string{"subagents"})
	if code != 0 {
		t.Fatalf("bare subagents = %d", code)
	}
	if !strings.Contains(stdout, "unmanaged") {
		t.Errorf("status label missing: %q", stdout)
	}
	if !strings.Contains(stderr, "no effect until 'baseten-switch claude on'") {
		t.Errorf("wiring warn missing from bare status: %q", stderr)
	}
}

// TestSubagentsRouterDownNotice asserts that with the router down, the
// config is still written and a notice says it applies at next start.
func TestSubagentsRouterDownNotice(t *testing.T) {
	e := newSubagentTestEnv(t, true)
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"})
	if code != 0 {
		t.Fatalf("subagents set router-down = %d (%s)", code, stderr)
	}
	if m := cfgSubagentModel(t, e.cfgPath); m != "claude-baseten-glm-5-2" {
		t.Errorf("config not written: %q", m)
	}
	if !strings.Contains(stderr, "applies at next start") {
		t.Errorf("router-down notice missing: %q", stderr)
	}
	if e.sigCount() != 0 {
		t.Errorf("SIGHUP fired with router down: %d", e.sigCount())
	}
}

// TestSubagentsWiringWarn asserts that with claude wiring off (settings
// base URL not pointing at a gateway port), a warning prints but the
// config is still written (warnings, not refusals).
func TestSubagentsWiringWarn(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	if err := os.WriteFile(e.settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"})
	if code != 0 {
		t.Fatalf("subagents set with wiring off = %d (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "no effect until 'baseten-switch claude on'") {
		t.Errorf("wiring warn missing: %q", stderr)
	}
	if m := cfgSubagentModel(t, e.cfgPath); m != "claude-baseten-glm-5-2" {
		t.Errorf("config not written despite warning: %q", m)
	}
}

// TestSubagentsEnvVarDoubleManagementWarn asserts that with
// CLAUDE_CODE_SUBAGENT_MODEL in the settings env block, a warning prints
// about double management.
func TestSubagentsEnvVarDoubleManagementWarn(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	if err := os.WriteFile(e.settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","CLAUDE_CODE_SUBAGENT_MODEL":"claude-haiku-4-5"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"})
	if code != 0 {
		t.Fatalf("subagents set = %d (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "CLAUDE_CODE_SUBAGENT_MODEL is set in") {
		t.Errorf("env-var double-management warn missing: %q", stderr)
	}
}

// TestSubagentsProcessEnvVarWarn asserts that with
// CLAUDE_CODE_SUBAGENT_MODEL in the process env, a warning prints.
func TestSubagentsProcessEnvVarWarn(t *testing.T) {
	_ = newSubagentTestEnv(t, false)
	t.Setenv(claudeSubagentEnvKey, "claude-haiku-4-5")
	code, _, stderr := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"})
	if code != 0 {
		t.Fatalf("subagents set = %d (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "process environment") {
		t.Errorf("process env-var warn missing: %q", stderr)
	}
}

// TestSubagentsCommentsPreserved asserts that comments in gateway.yaml
// survive the config edit.
func TestSubagentsCommentsPreserved(t *testing.T) {
	e := newSubagentTestEnv(t, false)
	before := fileBytes(t, e.cfgPath)
	commentLine := "# subagent test config"
	if !bytes.Contains(before, []byte(commentLine)) {
		t.Fatal("test config must have a comment line")
	}
	if code, _, _ := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"}); code != 0 {
		t.Fatal("set failed")
	}
	after := fileBytes(t, e.cfgPath)
	if !bytes.Contains(after, []byte(commentLine)) {
		t.Errorf("comment line lost after config edit:\n%s", after)
	}
}

// TestSubagentsNoRestartMessage asserts the verb never prints a
// "restart Claude Code sessions" message (this is live, no restart).
func TestSubagentsNoRestartMessage(t *testing.T) {
	_ = newSubagentTestEnv(t, false)
	_, _, stderr := runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"})
	if strings.Contains(stderr, "restart Claude Code sessions") {
		t.Errorf("restart message must not appear: %q", stderr)
	}
}

// TestSubagentsUsageErrors asserts unrecognized models and too-many-args
// exit 2 with a usage line.
func TestSubagentsUsageErrors(t *testing.T) {
	_ = newSubagentTestEnv(t, false)
	for _, args := range [][]string{
		{"gpt-5"},
		{""},
		{"claude"},
		{"a", "b"},
	} {
		t.Run(strings.Join(args, ","), func(t *testing.T) {
			code, _, stderr := runClaudeCaptured(t, append([]string{"subagents"}, args...))
			if code != 2 {
				t.Errorf("subagents %v = %d, want 2", args, code)
			}
			if !strings.Contains(stderr, "usage:") {
				t.Errorf("usage line missing: %q", stderr)
			}
		})
	}
}

func TestSubagentsUsageExplainsInheritWithoutMainThreadMetaphor(t *testing.T) {
	for _, want := range []string{
		"no subagent-specific model rewrite",
		"requested model follows its family mapping or the unmatched-model default",
	} {
		if !strings.Contains(claudeSubagentsUsage, want) {
			t.Errorf("usage missing %q: %q", want, claudeSubagentsUsage)
		}
	}
	if strings.Contains(claudeSubagentsUsage, "main thread") {
		t.Errorf("usage retains misleading main-thread wording: %q", claudeSubagentsUsage)
	}
}

// TestSubagentsStatusCarriesSubagentLine asserts `claude status` reads
// the subagent line from gateway.yaml.
func TestSubagentsStatusCarriesSubagentLine(t *testing.T) {
	_ = newSubagentTestEnv(t, false)
	runClaudeCaptured(t, []string{"subagents", "claude-baseten-glm-5-2"})
	_, stdout, _ := runClaudeCaptured(t, []string{"status"})
	if !strings.Contains(stdout, "subagents:") || !strings.Contains(stdout, "claude-baseten-glm-5-2") {
		t.Errorf("status missing the subagents line: %q", stdout)
	}
}

// --- off-strips-owned coverage (adapted from the settings-era tests) ---
//
// "claude off" must still strip a gateway-owned
// CLAUDE_CODE_SUBAGENT_MODEL value (branch-era hygiene; the key stays in
// claudeManagedEnvKeys). These tests exercise the settings on/off path,
// not the subagents verb, and confirm the hygiene is intact.

// loadBackup reads the adapter's backup file (nil-safe).
func loadBackup(t *testing.T, a *claudeAdapter) *claudeBackup {
	t.Helper()
	bak, err := loadClaudeBackup(a.backupPath)
	if err != nil {
		t.Fatalf("load backup: %v", err)
	}
	return bak
}

// TestClaudeOffRestoresBaseURLStripsSubagent asserts `claude off`
// restores the base URL from backup AND strips a gateway-owned
// subagent value (the subagent key is in claudeManagedEnvKeys for
// branch-era hygiene, but the new config-edit verb never backs it up,
// so off goes strip-only-owned for the subagent key while restoring
// the base URL from the backup).
func TestClaudeOffRestoresBaseURLStripsSubagent(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com","CLAUDE_CODE_SUBAGENT_MODEL":"claude-haiku-4-5","OTHER":"keep"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	// Simulate a branch-era subagent env write by setting it directly,
	// then refresh the backup drift hash so off() sees a clean match
	// and restores the base URL (the subagent key has no backup
	// coverage, so it is stripped as gateway-owned).
	root := readTree(t, a.settingsPath)
	root["env"].(map[string]any)[claudeSubagentEnvKey] = "claude-baseten-glm-5-2"
	b, _ := json.MarshalIndent(root, "", "  ")
	newRaw := append(b, '\n')
	if err := os.WriteFile(a.settingsPath, newRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	bak := loadBackup(t, a)
	bak.WrittenHash = sha256Hex(newRaw)
	if err := saveClaudeBackup(a.backupPath, bak); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	// Base URL restored from backup.
	if v, _ := envValue(t, a.settingsPath, claudeManagedEnvKey); v != "https://corp-proxy.example.com" {
		t.Errorf("base url not restored: %q", v)
	}
	// Gateway-owned subagent value stripped (no backup coverage for it;
	// the new verb never backs up the subagent key).
	if _, ok := envValue(t, a.settingsPath, claudeSubagentEnvKey); ok {
		t.Errorf("gateway-owned subagent value not stripped")
	}
	if o, _ := envValue(t, a.settingsPath, "OTHER"); o != "keep" {
		t.Errorf("unmanaged env key lost: %q", o)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("backup not deleted after off")
	}
}

// TestClaudeOffLeavesUnmanagedSubagentValue asserts a user-set native
// subagent model that we never managed survives `claude off`.
func TestClaudeOffLeavesUnmanagedSubagentValue(t *testing.T) {
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com","CLAUDE_CODE_SUBAGENT_MODEL":"claude-opus-4-8"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	if code := a.off(); code != 0 {
		t.Fatal("off failed")
	}
	if v, _ := envValue(t, a.settingsPath, claudeSubagentEnvKey); v != "claude-opus-4-8" {
		t.Errorf("unmanaged subagent value lost on off: %q", v)
	}
}

// TestClaudeOffStripsGatewayOwnedSubagentValue asserts `claude off`
// strips a gateway-owned (alias/slug) subagent value with no backup
// coverage (strip-only-owned path).
func TestClaudeOffStripsGatewayOwnedSubagentValue(t *testing.T) {
	a, out := testAdapter(t)
	// No backup: write a settings file with a gateway-owned subagent
	// value and no prior `on` (so off goes strip-only-owned).
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","CLAUDE_CODE_SUBAGENT_MODEL":"claude-baseten-glm-5-2","OTHER":"keep"}}`)
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if _, ok := envValue(t, a.settingsPath, claudeSubagentEnvKey); ok {
		t.Errorf("gateway-owned subagent value not stripped")
	}
	if o, _ := envValue(t, a.settingsPath, "OTHER"); o != "keep" {
		t.Errorf("unmanaged env key touched: %q", o)
	}
}
