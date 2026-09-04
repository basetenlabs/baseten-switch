package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

func globalRoutingFile(enabled bool) *config.File {
	return &config.File{
		Global: config.Global{
			RoutingEnabled: &enabled,
		},
		Clients: []config.Client{{
			Name: "claude-code", Enabled: true, BindAddr: "127.0.0.1:0",
			ProtocolShape: "anthropic",
			DefaultModel:  "zai-org/GLM-5.2",
			ModelAliases: map[string]string{
				"claude-baseten-glm-5-2": "zai-org/GLM-5.2",
			},
			ModelOptions: config.ModelOptions{
				pricing.ProviderBaseten: {
					"zai-org/GLM-5.2": {
						Reasoning: &config.ReasoningPolicy{
							Mode: config.ReasoningFollowHarness,
						},
					},
				},
			},
			FallbackRoute: "anthropic",
		}},
	}
}

func newGlobalRoutingGateway(
	t *testing.T,
	f *config.File,
	basetenURL, anthropicURL string,
) (*Gateway, func()) {
	t.Helper()
	cfg := testConfig(t, basetenURL, anthropicURL)
	cfg.ConfigPath = t.TempDir() + "/gateway.yaml"
	if err := config.Save(cfg.ConfigPath, f); err != nil {
		t.Fatal(err)
	}
	resolved, err := loadResolvedClients(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	g, _, _ := newGateway(t, cfg, resolved...)
	stop := start(t, g)
	return g, stop
}

func TestGlobalRoutingAbsoluteOffBypassesEveryBasetenPolicy(t *testing.T) {
	var basetenRequests atomic.Int64
	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		basetenRequests.Add(1)
		http.Error(w, "must not be reached", http.StatusInternalServerError)
	}))
	defer baseten.Close()
	gotNativeModel := make(chan string, 1)
	anthropic := recordingStub(t, gotNativeModel, "NATIVE")
	defer anthropic.Close()

	f := globalRoutingFile(false)
	c := &f.Clients[0]
	c.ModelRoutes = map[string]string{
		"opus": "zai-org/GLM-5.2",
	}
	c.SubagentModel = "claude-baseten-glm-5-2"
	c.SubagentRouting = "on"

	g, stop := newGlobalRoutingGateway(t, f, baseten.URL, anthropic.URL)
	defer stop()
	// Prove request routing does not even need a Baseten credential.
	g.authMu.Lock()
	g.oauthClient = nil
	g.cfg.APIKeyFallback = false
	g.cfg.BasetenKey = ""
	g.authMu.Unlock()

	resp, body := postModelMessages(t, g, "claude-opus-4-8", "agent-1")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "NATIVE") {
		t.Fatalf("native request status=%d body=%s", resp.StatusCode, body)
	}
	if got := <-gotNativeModel; got != "claude-opus-4-8" {
		t.Fatalf("native upstream model = %q, want original id", got)
	}
	// The Codex sentinel has no special meaning on an Anthropic-shaped
	// listener. A coincidental Claude request using that ID retains the
	// existing global-Off native passthrough behavior.
	resp, body = postModelMessages(t, g, CodexCompatibilityModel, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "NATIVE") {
		t.Fatalf("anthropic sentinel request status=%d body=%s", resp.StatusCode, body)
	}
	if got := <-gotNativeModel; got != CodexCompatibilityModel {
		t.Fatalf("native upstream model = %q, want %q", got, CodexCompatibilityModel)
	}
	for _, explicit := range []string{"claude-baseten-glm-5-2", "zai-org/GLM-5.2"} {
		resp, body = postModelMessages(t, g, explicit, "")
		if resp.StatusCode != http.StatusBadRequest ||
			!strings.Contains(body, "global routing is Off") {
			t.Fatalf("explicit %q status=%d body=%s", explicit, resp.StatusCode, body)
		}
	}
	if got := basetenRequests.Load(); got != 0 {
		t.Fatalf("Baseten received %d requests while global routing was Off", got)
	}
}

func TestAnthropicShapeCodexSentinelNeverFallsBackNatively(t *testing.T) {
	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "retry natively", http.StatusInternalServerError)
	}))
	defer baseten.Close()
	gotNativeModel := make(chan string, 1)
	anthropic := recordingStub(t, gotNativeModel, "NATIVE")
	defer anthropic.Close()

	f := globalRoutingFile(true)
	g, stop := newGlobalRoutingGateway(t, f, baseten.URL, anthropic.URL)
	defer stop()

	resp, body := postModelMessages(t, g, CodexCompatibilityModel, "")
	if resp.StatusCode != http.StatusInternalServerError ||
		!strings.Contains(body, "retry natively") {
		t.Fatalf("anthropic sentinel status=%d body=%s", resp.StatusCode, body)
	}
	select {
	case got := <-gotNativeModel:
		t.Fatalf("compatibility sentinel reached native provider as %q", got)
	default:
	}
}

func TestGlobalRoutingOnPrecedenceAndDefaultModel(t *testing.T) {
	gotModel := make(chan string, 8)
	baseten := recordingStub(t, gotModel, "BASETEN")
	defer baseten.Close()
	anthropic := recordingStub(t, nil, "NATIVE")
	defer anthropic.Close()

	f := globalRoutingFile(true)
	c := &f.Clients[0]
	c.ModelRoutes = map[string]string{
		"opus": "zai-org/FAMILY",
	}

	g, stop := newGlobalRoutingGateway(t, f, baseten.URL, anthropic.URL)
	defer stop()
	for _, tc := range []struct {
		model, want string
	}{
		{"claude-opus-4-8", "zai-org/FAMILY"},
		{"claude-opus-4-7", "zai-org/FAMILY"},
		{"claude-instant-1.2", "zai-org/GLM-5.2"},
		{"claude-mythos-5", "zai-org/GLM-5.2"},
	} {
		resp, body := postModelMessages(t, g, tc.model, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", tc.model, resp.StatusCode, body)
		}
		if got := <-gotModel; got != tc.want {
			t.Fatalf("%s upstream model=%q want %q", tc.model, got, tc.want)
		}
	}
}

func TestGlobalRoutingStatusUsesActiveResolverAndExactByteHashes(t *testing.T) {
	baseten := recordingStub(t, nil, "BASETEN")
	defer baseten.Close()
	anthropic := recordingStub(t, nil, "NATIVE")
	defer anthropic.Close()
	f := globalRoutingFile(true)
	f.Clients[0].ModelRoutes = map[string]string{"opus": "zai-org/GLM-5.2"}

	g, stop := newGlobalRoutingGateway(t, f, baseten.URL, anthropic.URL)
	defer stop()
	status := adminStatusGet(t, g)
	if status["global_routing_enabled"] != true {
		t.Fatalf("global_routing_enabled = %v", status["global_routing_enabled"])
	}
	if status["router_boot_id"] == "" || status["active_generation"] != float64(1) {
		t.Fatalf("ordering fields = boot:%v generation:%v", status["router_boot_id"], status["active_generation"])
	}
	if status["active_config_hash"] == "" ||
		status["active_config_hash"] != status["desired_config_hash"] {
		t.Fatalf("hashes active=%v desired=%v", status["active_config_hash"], status["desired_config_hash"])
	}
	caps := status["capabilities"].([]any)
	if len(caps) != 2 || caps[0] != "global_routing" || caps[1] != "fallback_policy" {
		t.Fatalf("capabilities = %v", caps)
	}
	client := status["clients"].([]any)[0].(map[string]any)
	if client["effective_route"] != "baseten" {
		t.Fatalf("effective_route = %v, want baseten", client["effective_route"])
	}
	if client["effective_summary"] != "Baseten · GLM 5.2" {
		t.Fatalf("effective_summary = %v", client["effective_summary"])
	}
	families := client["families"].([]any)
	var opus map[string]any
	for _, raw := range families {
		row := raw.(map[string]any)
		if row["family"] == "opus" {
			opus = row
			break
		}
	}
	if opus == nil || opus["effective_source"] != "family_mapping" {
		t.Fatalf("opus family = %v", opus)
	}
	activeHash := status["active_config_hash"]
	// Write syntactically valid but incomplete clean-schema bytes, then
	// attempt reload.
	raw := []byte("global:\n  auth: {}\nclients: []\n")
	if err := os.WriteFile(g.activeConfigPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	g.reloadConfig()
	status = adminStatusGet(t, g)
	if status["active_config_hash"] != activeHash ||
		status["desired_config_hash"] != exactConfigHash(raw) {
		t.Fatalf("failed reload hashes active=%v desired=%v", status["active_config_hash"], status["desired_config_hash"])
	}
	reload := status["reload"].(map[string]any)
	if reload["state"] != "error" || reload["error"] == "" {
		t.Fatalf("reload = %v", reload)
	}
	client = status["clients"].([]any)[0].(map[string]any)
	if client["effective_summary"] != "Baseten · GLM 5.2" {
		t.Fatalf("failed reload changed active resolver: %v", client)
	}
}

func TestEffectiveSummaryProviderDisplayIsSafe(t *testing.T) {
	for _, tc := range []struct {
		name string
		rc   resolvedClientConfig
		want string
	}{
		{
			name: "empty shape off defaults Anthropic",
			rc: resolvedClientConfig{
				HasGlobalRoutingGate: true,
				GlobalRoutingEnabled: false,
			},
			want: "Native · Anthropic",
		},
		{
			name: "openai capitalization",
			rc: resolvedClientConfig{
				ProtocolShape: "openai", Route: "openai",
			},
			want: "Native · OpenAI",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveSummary(tc.rc, nil); got != tc.want {
				t.Fatalf("effectiveSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestActiveConfigSnapshotIsImmutable(t *testing.T) {
	enabled := true
	raw := []byte("global:\n  routing_enabled: true\nclients:\n  - name: claude-code\n    enabled: false\n    default_model: zai-org/GLM-5.2\n")
	f := &config.File{Global: config.Global{
		RoutingEnabled: &enabled,
	}, Clients: []config.Client{{Name: "claude-code", DefaultModel: "zai-org/GLM-5.2"}}}
	g := &Gateway{}
	g.activateConfigSnapshot(f, raw)
	*f.Global.RoutingEnabled = false
	f.Clients[0].DefaultModel = "mutated/model"
	state := g.activeRoutingState()
	if state.file.Global.RoutingEnabled == nil || !*state.file.Global.RoutingEnabled ||
		state.file.Clients[0].DefaultModel != "zai-org/GLM-5.2" {
		b, _ := json.Marshal(state.file)
		t.Fatalf("active snapshot retained mutable input: %s", b)
	}
}

func TestGlobalRoutingReloadKeepsListenerAndConnection(t *testing.T) {
	baseten := recordingStub(t, nil, "BASETEN")
	defer baseten.Close()
	anthropic := recordingStub(t, nil, "NATIVE")
	defer anthropic.Close()

	initial := globalRoutingFile(true)
	g, stop := newGlobalRoutingGateway(t, initial, baseten.URL, anthropic.URL)
	defer stop()
	spec := groupResolved(mustResolveFile(t, initial))[0]
	beforeGroup := g.snapshotGroup(spec.key)
	beforeListener := beforeGroup.listener
	beforeState := g.activeRoutingState()

	conn, err := net.Dial("tcp", g.ClientAddr("claude-code").String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	readHealth := func() {
		t.Helper()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: keep-alive\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("read health response on persistent connection: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("health status = %d", resp.StatusCode)
		}
	}
	readHealth()

	updated := globalRoutingFile(false)
	if err := config.Save(g.activeConfigPath(), updated); err != nil {
		t.Fatal(err)
	}
	g.reloadConfig()

	afterGroup := g.snapshotGroup(spec.key)
	if afterGroup != beforeGroup || afterGroup.listener != beforeListener {
		t.Fatal("policy-only reload replaced the listener group or socket")
	}
	afterState := g.activeRoutingState()
	if afterState.generation != beforeState.generation+1 ||
		afterState.activeHash == beforeState.activeHash {
		t.Fatalf("active state did not advance exactly once: before=%+v after=%+v", beforeState, afterState)
	}
	if got := afterGroup.clientConfigs()[0]; !got.globalRoutingOff() || got.Route != "anthropic" {
		t.Fatalf("hot-swapped client = %+v, want global routing Off/native", got)
	}
	readHealth()
}

func TestGlobalRoutingReloadDrainsInFlightRequestOnOldResolver(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_baseten","type":"message","content":[]}`))
	}))
	defer baseten.Close()
	nativeModels := make(chan string, 1)
	anthropic := recordingStub(t, nativeModels, "NATIVE")
	defer anthropic.Close()

	initial := globalRoutingFile(true)
	g, stop := newGlobalRoutingGateway(t, initial, baseten.URL, anthropic.URL)
	defer stop()
	spec := groupResolved(mustResolveFile(t, initial))[0]
	beforeGroup := g.snapshotGroup(spec.key)
	beforeListener := beforeGroup.listener

	inflight := make(chan error, 1)
	go func() {
		resp, body := postModelMessages(t, g, "claude-opus-4-8", "")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_baseten") {
			inflight <- fmt.Errorf("in-flight response status=%d body=%s", resp.StatusCode, body)
			return
		}
		inflight <- nil
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not reach the old Baseten resolver")
	}

	updated := globalRoutingFile(false)
	if err := config.Save(g.activeConfigPath(), updated); err != nil {
		t.Fatal(err)
	}
	g.reloadConfig()
	if afterGroup := g.snapshotGroup(spec.key); afterGroup != beforeGroup || afterGroup.listener != beforeListener {
		t.Fatal("policy-only reload replaced the listener group or socket")
	}
	close(release)
	select {
	case err := <-inflight:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not drain after policy reload")
	}

	resp, body := postModelMessages(t, g, "claude-opus-4-8", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "NATIVE") {
		t.Fatalf("post-reload request status=%d body=%s", resp.StatusCode, body)
	}
	if got := <-nativeModels; got != "claude-opus-4-8" {
		t.Fatalf("post-reload native model = %q", got)
	}
}

func TestGlobalRoutingFailedTopologyBindRetainsListenersAndActiveSnapshot(t *testing.T) {
	baseten := recordingStub(t, nil, "BASETEN")
	defer baseten.Close()
	anthropic := recordingStub(t, nil, "NATIVE")
	defer anthropic.Close()

	initial := globalRoutingFile(true)
	g, stop := newGlobalRoutingGateway(t, initial, baseten.URL, anthropic.URL)
	defer stop()
	initialSpec := groupResolved(mustResolveFile(t, initial))[0]
	beforeGroup := g.snapshotGroup(initialSpec.key)
	beforeListener := beforeGroup.listener
	beforeState := g.activeRoutingState()

	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	updated := globalRoutingFile(false)
	updated.Clients = append(updated.Clients, config.Client{
		Name: "codex", Enabled: true, BindAddr: blocker.Addr().String(),
		ProtocolShape: "openai",
		DefaultModel:  "zai-org/GLM-5.2",
		FallbackRoute: "openai",
	})
	if err := config.Save(g.activeConfigPath(), updated); err != nil {
		t.Fatal(err)
	}
	desiredRaw, err := os.ReadFile(g.activeConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	g.reloadConfig()

	afterState := g.activeRoutingState()
	if afterState.generation != beforeState.generation ||
		afterState.activeHash != beforeState.activeHash {
		t.Fatalf("failed topology reload advanced active state: before=%+v after=%+v", beforeState, afterState)
	}
	if afterState.reloadErr == "" {
		t.Fatal("failed topology reload did not record an error")
	}
	afterGroup := g.snapshotGroup(initialSpec.key)
	if afterGroup != beforeGroup || afterGroup.listener != beforeListener {
		t.Fatal("failed topology reload replaced the existing listener")
	}
	if g.ClientAddr("codex") != nil {
		t.Fatal("failed topology reload published the unbound client")
	}
	status := adminStatusGet(t, g)
	if status["active_config_hash"] != beforeState.activeHash ||
		status["desired_config_hash"] != exactConfigHash(desiredRaw) {
		t.Fatalf("failed reload hashes = active:%v desired:%v", status["active_config_hash"], status["desired_config_hash"])
	}
	resp, body := postModelMessages(t, g, "claude-opus-4-8", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "BASETEN") {
		t.Fatalf("old listener stopped serving after failed topology reload: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestStartupUsesExactResolvedSnapshotWhenFileChanges(t *testing.T) {
	cfg := testConfig(t, "http://baseten.invalid", "http://anthropic.invalid")
	cfg.ConfigPath = t.TempDir() + "/gateway.yaml"
	initial := globalRoutingFile(true)
	if err := config.Save(cfg.ConfigPath, initial); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loadResolvedConfigSnapshot(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	updated := globalRoutingFile(false)
	if err := config.Save(cfg.ConfigPath, updated); err != nil {
		t.Fatal(err)
	}
	desiredRaw, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g, err := newGatewayWithSnapshot(cfg, pricing.New(), adminListener, snapshot.clients, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer g.shutdownAllClients()
	defer adminListener.Close()

	state := g.activeRoutingState()
	if state.activeHash != exactConfigHash(snapshot.raw) || state.generation != 1 {
		t.Fatalf("startup active state = %+v, want exact resolved snapshot", state)
	}
	if state.file.Global.RoutingEnabled == nil || !*state.file.Global.RoutingEnabled {
		t.Fatal("startup snapshot was relabeled with later Off bytes")
	}
	client := g.snapshotClients()[0].cfg
	if client.Route != "baseten" || !client.GlobalRoutingEnabled {
		t.Fatalf("startup listener does not match active snapshot: %+v", client)
	}
	if exactConfigHash(desiredRaw) == state.activeHash {
		t.Fatal("test did not create a desired-versus-active mismatch")
	}
}

func mustResolveFile(t *testing.T, file *config.File) []resolvedClientConfig {
	t.Helper()
	resolved, err := resolveFromFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
