package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

// All route-verb tests are hermetic and mirror claude_subagents_test.go:
// temp gateway.yaml via BASETEN_SWITCH_CONFIG_PATH, temp settings.json via
// BASETEN_SWITCH_CLAUDE_SETTINGS, a stub signalRouter recorder, and a fake admin
// server serving model_routes for the verify poll (or a router-down
// path). The real ~/.claude and ~/.config/baseten-switch are never touched.

// routeTestEnv bundles the temp paths and stubs for one route test. The
// fake admin server (when non-nil) serves model_routes from gateway.yaml
// so the SIGHUP verify poll succeeds; a nil server models router-down.
type routeTestEnv struct {
	cfgPath  string
	settings string
	sigPids  *[]int
	sigMu    *sync.Mutex
}

type fakeMutationOrdering struct {
	mu         sync.Mutex
	lastHash   string
	generation uint64
}

func (o *fakeMutationOrdering) observe(t *testing.T, path string) (string, uint64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := exactConfigHash(data)
	o.mu.Lock()
	defer o.mu.Unlock()
	if hash != o.lastHash {
		o.lastHash = hash
		o.generation++
	}
	return hash, o.generation
}

// newRouteTestEnv sets up a hermetic route test. Pass adminDown=true for
// the router-down path (no admin server, no pidfile). The gateway.yaml
// includes a claude-code client with model_aliases so alias targets
// validate.
func newRouteTestEnv(t *testing.T, adminDown bool) *routeTestEnv {
	t.Helper()
	dir := t.TempDir()

	cfgPath := filepath.Join(dir, "gateway.yaml")

	// Fake admin server: serves /v1/admin/status with the claude-code
	// client reporting model_routes read from the config file. The
	// handler re-reads gateway.yaml on each request so it reflects the
	// verb's config edit (mirrors the subagents fake admin).
	mux := http.NewServeMux()
	ordering := &fakeMutationOrdering{}
	mux.HandleFunc("/v1/admin/status", func(w http.ResponseWriter, r *http.Request) {
		routes := readModelRoutesFromConfig(t, cfgPath)
		hash, generation := ordering.observe(t, cfgPath)
		fmt.Fprintf(w, `{"uptime_seconds":42,"version":%q,
			"router_pid":%d,"router_boot_id":"route-test-router","active_generation":%d,
			"active_config_hash":%q,"desired_config_hash":%q,"config_path":%q,
			"capabilities":["global_routing"],"global_routing_enabled":true,
			"auth":{"signed_in":true,"profile":"doc","fallback_enabled":false,"fallback_in_use":false},
			"clients":[{"name":"claude-code","enabled":true,"bind_addr":"127.0.0.1:18081","protocol_shape":"anthropic",
				"fallback_route":"anthropic","auth_set":true,"currently_bound":true,
				"model_routes":%s}]}`,
			version.Version, os.Getpid(), generation, hash, hash, cfgPath,
			modelRoutesJSON(routes))
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

	// For the alive-router path, write a pidfile with the current pid so
	// classifyPidfile returns pidfileAlive. For router-down, leave none.
	gwPid := filepath.Join(dir, "gw.pid")
	if !adminDown {
		if err := os.WriteFile(gwPid, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", gwPid)

	cfg := `# route test config
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

	t.Setenv("BASETEN_SWITCH_BACKUP_DIR", filepath.Join(dir, "backups"))
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(dir, "door.pid"))
	t.Setenv("BASETEN_SWITCH_ENV_FILE", filepath.Join(dir, "env"))
	t.Setenv("BASETEN_SWITCH_LAUNCHD", "off")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("BASETEN_API_KEY", "")
	t.Setenv(claudeSubagentEnvKey, "")
	// Clear the harness env slots the route warnings check for.
	for _, k := range routeEnvSlotKeys() {
		t.Setenv(k, "")
	}

	return &routeTestEnv{
		cfgPath:  cfgPath,
		settings: settings,
		sigPids:  &pids,
		sigMu:    &mu,
	}
}

func (e *routeTestEnv) sigCount() int {
	e.sigMu.Lock()
	defer e.sigMu.Unlock()
	return len(*e.sigPids)
}

// readModelRoutesFromConfig extracts model_routes from gateway.yaml via
// the config package (the fake admin server uses it to mirror the real
// admin status).
func readModelRoutesFromConfig(t *testing.T, cfgPath string) map[string]string {
	t.Helper()
	f, _, err := loadGatewayConfigForAdapter()
	if err != nil {
		return nil
	}
	name, err := claudeTargetClientName(f)
	if err != nil {
		return nil
	}
	for i := range f.Clients {
		if f.Clients[i].Name == name {
			return f.Clients[i].ModelRoutes
		}
	}
	return nil
}

// modelRoutesJSON renders a model_routes map as a JSON object, or null
// when empty (so the admin status omits or nulls the field cleanly).
func modelRoutesJSON(m map[string]string) string {
	if len(m) == 0 {
		return "null"
	}
	var b strings.Builder
	b.WriteString("{")
	first := true
	for _, k := range sortedRouteKeys(m) {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, "%q:%q", k, m[k])
	}
	b.WriteString("}")
	return b.String()
}

// cfgModelRoutes reads model_routes from the temp gateway.yaml.
func cfgModelRoutes(t *testing.T, cfgPath string) map[string]string {
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
			return f.Clients[i].ModelRoutes
		}
	}
	t.Fatalf("client %s not found", name)
	return nil
}

// TestRouteSetFamilyPinWritesConfig runs `route opus native` and asserts
// the config is written, SIGHUP fires, and the verify poll succeeds.
func TestRouteSetFamilyPinWritesConfig(t *testing.T) {
	e := newRouteTestEnv(t, false)
	code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "native"})
	if code != 0 {
		t.Fatalf("route set = %d (%s)", code, stderr)
	}
	r := cfgModelRoutes(t, e.cfgPath)
	if r["opus"] != "native" {
		t.Errorf("model_routes[opus] = %q, want native", r["opus"])
	}
	if e.sigCount() == 0 {
		t.Error("no SIGHUP recorded")
	}
	if !strings.Contains(stderr, "verified live") {
		t.Errorf("verify poll message missing: %q", stderr)
	}
	if !strings.Contains(stderr, "opus -> native") {
		t.Errorf("affected-row message missing: %q", stderr)
	}
}

// TestRouteSetExactPinRejected asserts model_routes accepts family keys
// only and does not modify the config on an exact-id request.
func TestRouteSetExactPinRejected(t *testing.T) {
	e := newRouteTestEnv(t, false)
	before := fileBytes(t, e.cfgPath)
	code, _, stderr := runClaudeCaptured(t, []string{"route", "claude-opus-4-8[1m]", "zai-org/GLM-5.2"})
	if code != 1 {
		t.Fatalf("route set exact = %d, want 1 (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "not a supported family") {
		t.Errorf("error missing family-only rule: %q", stderr)
	}
	if !bytes.Equal(before, fileBytes(t, e.cfgPath)) {
		t.Error("rejected exact-id set modified the config")
	}
}

// TestRouteSetAliasPinWritesConfig runs `route sonnet
// claude-baseten-kimi-k2-7` and asserts the alias pin is written.
func TestRouteSetAliasPinWritesConfig(t *testing.T) {
	e := newRouteTestEnv(t, false)
	code, _, _ := runClaudeCaptured(t, []string{"route", "sonnet", "claude-baseten-kimi-k2-7"})
	if code != 0 {
		t.Fatal("route set alias failed")
	}
	r := cfgModelRoutes(t, e.cfgPath)
	if r["sonnet"] != "claude-baseten-kimi-k2-7" {
		t.Errorf("model_routes[sonnet] = %q, want claude-baseten-kimi-k2-7", r["sonnet"])
	}
}

// TestRouteUnknownAliasExits1 asserts an unknown alias target exits 1
// listing the configured aliases, and does not modify the config.
func TestRouteUnknownAliasExits1(t *testing.T) {
	e := newRouteTestEnv(t, false)
	before := fileBytes(t, e.cfgPath)
	code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "claude-baseten-nope"})
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

// TestRouteInvalidKeyExits1 asserts a key that is neither a family word
// nor a claude-/anthropic- prefixed id exits 1 listing the family set.
func TestRouteInvalidKeyExits1(t *testing.T) {
	e := newRouteTestEnv(t, false)
	before := fileBytes(t, e.cfgPath)
	code, _, stderr := runClaudeCaptured(t, []string{"route", "gpt-5", "native"})
	if code != 1 {
		t.Fatalf("invalid key = %d, want 1 (%s)", code, stderr)
	}
	for _, want := range []string{"gpt-5", "fable", "opus", "sonnet", "haiku"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("error output missing %q: %q", want, stderr)
		}
	}
	if !bytes.Equal(before, fileBytes(t, e.cfgPath)) {
		t.Error("failed set modified the config file")
	}
}

// TestRouteAliasNamespaceKeyExits1 asserts a non-family alias key exits
// 1 and does not modify the config.
func TestRouteAliasNamespaceKeyExits1(t *testing.T) {
	e := newRouteTestEnv(t, false)
	before := fileBytes(t, e.cfgPath)
	code, _, stderr := runClaudeCaptured(t, []string{"route", "claude-baseten-glm-5-2", "native"})
	if code != 1 {
		t.Fatalf("alias-namespace key = %d, want 1 (%s)", code, stderr)
	}
	for _, want := range []string{"claude-baseten-glm-5-2", "supported family"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("error output missing %q: %q", want, stderr)
		}
	}
	if !bytes.Equal(before, fileBytes(t, e.cfgPath)) {
		t.Error("failed set modified the config file")
	}
}

// TestRouteExactKeyAnyClaudePrefix asserts prefix shape does not make a
// non-family key valid.
func TestRouteExactKeyAnyClaudePrefix(t *testing.T) {
	_ = newRouteTestEnv(t, false)
	code, _, stderr := runClaudeCaptured(t, []string{"route", "anthropic.claude-v2", "native"})
	if code != 1 {
		t.Fatalf("unhyphenated prefix key = %d, want 1 (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "supported family") {
		t.Errorf("error missing family-only rule: %q", stderr)
	}
}

// TestRouteConfiguredAliasOutsideNamespaceAccepted asserts a configured
// alias whose id is NOT in the claude-baseten-/anthropic-baseten- namespace is
// accepted as a target (the configured-alias map is checked before the
// namespace gate, matching the router's load order).
func TestRouteConfiguredAliasOutsideNamespaceAccepted(t *testing.T) {
	e := newRouteTestEnv(t, false)
	cfg := `# route unusual alias test
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
      claudette-custom: zai-org/GLM-5.2
door:
  ports:
    - bind_addr: 127.0.0.1:8081
      router_addr: 127.0.0.1:18081
`
	if err := os.WriteFile(e.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "claudette-custom"})
	if code != 0 {
		t.Fatalf("configured unusual alias = %d, want 0 (%s)", code, stderr)
	}
	if r := cfgModelRoutes(t, e.cfgPath); r["opus"] != "claudette-custom" {
		t.Errorf("model_routes[opus] = %q, want claudette-custom", r["opus"])
	}
}

// TestRouteDefaultRemovesPin asserts `route opus default` removes an
// existing pin and verifies live.
func TestRouteDefaultRemovesPin(t *testing.T) {
	e := newRouteTestEnv(t, false)
	if code, _, _ := runClaudeCaptured(t, []string{"route", "opus", "native"}); code != 0 {
		t.Fatal("set failed")
	}
	if r := cfgModelRoutes(t, e.cfgPath); r["opus"] != "native" {
		t.Fatalf("pin not set: %v", r)
	}
	code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "default"})
	if code != 0 {
		t.Fatalf("route default = %d (%s)", code, stderr)
	}
	r := cfgModelRoutes(t, e.cfgPath)
	if _, has := r["opus"]; has {
		t.Errorf("opus pin still present: %v", r)
	}
	if !strings.Contains(stderr, "verified live") {
		t.Errorf("verify poll message missing on removal: %q", stderr)
	}
	if !strings.Contains(stderr, "pin removed") {
		t.Errorf("removal message missing: %q", stderr)
	}
}

// TestRouteDefaultOnAbsentPinNoop asserts `route opus default` on an
// absent pin reports already-default, exits 0, and writes nothing.
func TestRouteDefaultOnAbsentPinNoop(t *testing.T) {
	e := newRouteTestEnv(t, false)
	before := fileBytes(t, e.cfgPath)
	code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "default"})
	if code != 0 {
		t.Fatalf("default on absent = %d, want 0 (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "already default") {
		t.Errorf("already-default message missing: %q", stderr)
	}
	if !bytes.Equal(before, fileBytes(t, e.cfgPath)) {
		t.Errorf("noop default modified the config file")
	}
	// The config must still load.
	if _, err := config.Load(e.cfgPath); err != nil {
		t.Fatalf("config.Load after noop default: %v", err)
	}
}

// TestRouteRouterDownNotice asserts that with the router down, the
// config is still written and a notice says it applies at next start.
func TestRouteRouterDownNotice(t *testing.T) {
	e := newRouteTestEnv(t, true)
	code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "native"})
	if code != 0 {
		t.Fatalf("route set router-down = %d (%s)", code, stderr)
	}
	if r := cfgModelRoutes(t, e.cfgPath); r["opus"] != "native" {
		t.Errorf("config not written: %v", r)
	}
	if !strings.Contains(stderr, "applies at next start") {
		t.Errorf("router-down notice missing: %q", stderr)
	}
	if e.sigCount() != 0 {
		t.Errorf("SIGHUP fired with router down: %d", e.sigCount())
	}
}

// TestRouteCommentsPreserved asserts that comments in gateway.yaml survive
// the config edit.
func TestRouteCommentsPreserved(t *testing.T) {
	e := newRouteTestEnv(t, false)
	before := fileBytes(t, e.cfgPath)
	commentLine := "# route test config"
	if !bytes.Contains(before, []byte(commentLine)) {
		t.Fatal("test config must have a comment line")
	}
	if code, _, _ := runClaudeCaptured(t, []string{"route", "opus", "native"}); code != 0 {
		t.Fatal("set failed")
	}
	after := fileBytes(t, e.cfgPath)
	if !bytes.Contains(after, []byte(commentLine)) {
		t.Errorf("comment line lost after config edit:\n%s", after)
	}
}

// TestRouteStatusPrintsTable asserts bare `route` prints one row per
// family.
func TestRouteStatusPrintsTable(t *testing.T) {
	_ = newRouteTestEnv(t, false)
	// Set a family pin.
	runClaudeCaptured(t, []string{"route", "opus", "native"})
	code, stdout, _ := runClaudeCaptured(t, []string{"route"})
	if code != 0 {
		t.Fatalf("bare route = %d", code)
	}
	// Every family appears, with the opus pin and the others default.
	for _, fam := range claudeFamilies {
		if !strings.Contains(stdout, fam) {
			t.Errorf("family %q missing from table: %q", fam, stdout)
		}
	}
	// The opus row shows the native pin. The key is %-22s padded, so
	// check the opus line carries the native value.
	if !regexp.MustCompile(`(?m)^  opus\s+native$`).MatchString(stdout) {
		t.Errorf("opus pin row missing or wrong: %q", stdout)
	}
	if !strings.Contains(stdout, "default (follow switch)") {
		t.Errorf("default row missing: %q", stdout)
	}
}

// TestRouteStatusEmptyIsAllDefault asserts bare `route` with no pins
// shows every family as default.
func TestRouteStatusEmptyIsAllDefault(t *testing.T) {
	_ = newRouteTestEnv(t, false)
	code, stdout, _ := runClaudeCaptured(t, []string{"route"})
	if code != 0 {
		t.Fatalf("bare route = %d", code)
	}
	for _, fam := range claudeFamilies {
		if !strings.Contains(stdout, fam) {
			t.Errorf("family %q missing: %q", fam, stdout)
		}
	}
	// Four default rows.
	if got := strings.Count(stdout, "default (follow switch)"); got != len(claudeFamilies) {
		t.Errorf("default row count = %d, want %d: %q", got, len(claudeFamilies), stdout)
	}
}

// TestRouteVerifyPollAgainstFakeAdmin asserts the verify poll reads
// model_routes from admin status and confirms the written end state.
// A stale admin (never reflecting the pin) must time out with a calm
// notice, not a false "verified live".
func TestRouteVerifyPollAgainstFakeAdmin(t *testing.T) {
	t.Run("fresh admin verifies", func(t *testing.T) {
		_ = newRouteTestEnv(t, false)
		code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "native"})
		if code != 0 {
			t.Fatalf("route set = %d (%s)", code, stderr)
		}
		if !strings.Contains(stderr, "verified live") {
			t.Errorf("verify poll message missing: %q", stderr)
		}
	})
	t.Run("stale admin times out calmly", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "gateway.yaml")
		cfg := `# route stale admin test
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

		// Fake admin that ALWAYS reports an empty model_routes (stale:
		// never reflects the pin the verb writes).
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/admin/status", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"uptime_seconds":42,"version":%q,
				"router_pid":%d,"router_boot_id":"stale-route-router","active_generation":1,
				"active_config_hash":%q,"desired_config_hash":%q,"config_path":%q,
				"capabilities":["global_routing"],"global_routing_enabled":true,
				"auth":{"signed_in":true,"profile":"doc","fallback_enabled":false,"fallback_in_use":false},
				"clients":[{"name":"claude-code","enabled":true,"bind_addr":"127.0.0.1:18081","protocol_shape":"anthropic",
					"effective_route":"baseten","fallback_route":"anthropic","auth_set":true,"currently_bound":true,
					"model_routes":null}]}`,
				version.Version, os.Getpid(), priorHash, priorHash, cfgPath)
		})
		adminSrv := httptest.NewServer(mux)
		t.Cleanup(adminSrv.Close)
		t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", hostPort(t, adminSrv.URL))

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
		for _, k := range routeEnvSlotKeys() {
			t.Setenv(k, "")
		}

		code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "native"})
		if code != 1 {
			t.Fatalf("route set = %d, want rollback failure 1 (%s)", code, stderr)
		}
		if r := cfgModelRoutes(t, cfgPath); r["opus"] != "" {
			t.Errorf("stale activation did not restore prior config: %v", r)
		}
		if strings.Contains(stderr, "verified live") {
			t.Errorf("stale admin must not verify: %q", stderr)
		}
		if !strings.Contains(stderr, "prior routing state was restored") {
			t.Errorf("rollback confirmation missing: %q", stderr)
		}
		if strings.Contains(stderr, "warning: SIGHUP sent") {
			t.Errorf("non-verified notice is scary (warning): %q", stderr)
		}
	})
}

// TestRouteWiringWarn asserts that with claude wiring off, a warning
// prints but the config is still written (warnings, not refusals).
func TestRouteWiringWarn(t *testing.T) {
	e := newRouteTestEnv(t, false)
	if err := os.WriteFile(e.settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "native"})
	if code != 0 {
		t.Fatalf("route set with wiring off = %d (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "no effect until 'baseten-switch claude on'") {
		t.Errorf("wiring warn missing: %q", stderr)
	}
	if r := cfgModelRoutes(t, e.cfgPath); r["opus"] != "native" {
		t.Errorf("config not written despite warning: %v", r)
	}
}

// TestRouteEnvVarDoubleManagementWarn asserts that with ANTHROPIC_MODEL
// in the settings env block, a warning prints about double management.
func TestRouteEnvVarDoubleManagementWarn(t *testing.T) {
	e := newRouteTestEnv(t, false)
	if err := os.WriteFile(e.settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","ANTHROPIC_MODEL":"claude-haiku-4-5"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "native"})
	if code != 0 {
		t.Fatalf("route set = %d (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "ANTHROPIC_MODEL") || !strings.Contains(stderr, "upstream of family pins") {
		t.Errorf("env-var double-management warn missing: %q", stderr)
	}
	// The warn names only the vars actually present, not the whole
	// checked set.
	if strings.Contains(stderr, "ANTHROPIC_DEFAULT_") {
		t.Errorf("warn names absent env vars: %q", stderr)
	}
}

// TestRouteProcessEnvVarWarn asserts that with an
// ANTHROPIC_DEFAULT_OPUS_MODEL in the process env, a warning prints.
func TestRouteProcessEnvVarWarn(t *testing.T) {
	_ = newRouteTestEnv(t, false)
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "claude-opus-4-6")
	code, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "native"})
	if code != 0 {
		t.Fatalf("route set = %d (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "ANTHROPIC_DEFAULT_OPUS_MODEL") || !strings.Contains(stderr, "process env") {
		t.Errorf("process env-var warn missing: %q", stderr)
	}
	// Present-only list: the absent slots must not be named.
	for _, absent := range []string{"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
		if strings.Contains(stderr, absent) {
			t.Errorf("warn names absent env var %s: %q", absent, stderr)
		}
	}
}

// TestRouteNoRestartMessage asserts the verb never prints a "restart
// Claude Code sessions" message (this is live, no restart).
func TestRouteNoRestartMessage(t *testing.T) {
	_ = newRouteTestEnv(t, false)
	_, _, stderr := runClaudeCaptured(t, []string{"route", "opus", "native"})
	if strings.Contains(stderr, "restart Claude Code sessions") {
		t.Errorf("restart message must not appear: %q", stderr)
	}
}

// TestRouteUsageErrors asserts too-many-args exits 2 with a usage line.
func TestRouteUsageErrors(t *testing.T) {
	_ = newRouteTestEnv(t, false)
	for _, args := range [][]string{
		{"opus"},
		{"opus", "native", "extra"},
	} {
		t.Run(strings.Join(args, ","), func(t *testing.T) {
			code, _, stderr := runClaudeCaptured(t, append([]string{"route"}, args...))
			if code != 2 {
				t.Errorf("route %v = %d, want 2", args, code)
			}
			if !strings.Contains(stderr, "usage:") {
				t.Errorf("usage line missing: %q", stderr)
			}
		})
	}
}

// TestRouteFamilyWordWithBracketSuffixExits asserts a suffixed family
// word is rejected because model_routes keys are exact family words.
func TestRouteFamilyWordWithBracketSuffixExits(t *testing.T) {
	_ = newRouteTestEnv(t, false)
	code, _, stderr := runClaudeCaptured(t, []string{"route", "opus[1m]", "native"})
	if code != 1 {
		t.Fatalf("family+suffix = %d, want 1 (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "not a supported family") {
		t.Errorf("family-suffix error message missing: %q", stderr)
	}
}
