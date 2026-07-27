package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestInitTemplateMatchesExampleConfig pins the go:embed copy
// (internal/config/gateway.example.yaml) to the repo-level
// config/gateway.example.yaml byte for byte, so 'baseten-switch config
// init' and the documented example can never drift apart. go:embed
// cannot reach outside the module, hence the copy plus this test.
func TestInitTemplateMatchesExampleConfig(t *testing.T) {
	example := filepath.Join("..", "..", "..", "config", "gateway.example.yaml")
	want, err := os.ReadFile(example)
	if err != nil {
		t.Fatalf("read repo example config: %v", err)
	}
	if !bytes.Equal(want, InitTemplate) {
		t.Fatalf("internal/config/gateway.example.yaml (embedded init template) differs from config/gateway.example.yaml; they must be byte-identical. Copy the repo example over the embedded file (cp config/gateway.example.yaml gateway/internal/config/gateway.example.yaml) or vice versa.")
	}
}

// TestInitTemplateLoads proves the embedded template is a loadable
// config with the intended single-port door topology.
func TestInitTemplateLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, InitTemplate, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("template does not load: %v", err)
	}
	if f.Global.RoutingEnabled == nil || !*f.Global.RoutingEnabled {
		t.Errorf("global.routing_enabled = %v, want explicit true", f.Global.RoutingEnabled)
	}
	if f.Global.Auth["baseten"] != "${BASETEN_API_KEY}" {
		t.Errorf("global.auth.baseten = %q, want ${BASETEN_API_KEY} reference", f.Global.Auth["baseten"])
	}

	// Only claude-code ships enabled; codex is a real client block
	// parked with enabled: false so 'baseten-switch codex on' can un-park it
	// via the scalar editor (the Codex integration contract item 2.2).
	if len(f.Clients) != 2 {
		t.Fatalf("want 2 clients (claude-code enabled, codex parked), got %d", len(f.Clients))
	}
	cc := f.Clients[0]
	if cc.Name != "claude-code" {
		t.Fatalf("client: got %q; want claude-code", cc.Name)
	}
	if cc.BindAddr != "127.0.0.1:45272" {
		t.Errorf("bind addr %q, want 127.0.0.1:45272", cc.BindAddr)
	}
	if cc.ProtocolShape != "anthropic" {
		t.Errorf("shape %q, want anthropic", cc.ProtocolShape)
	}
	if cc.FallbackRoute != "anthropic" {
		t.Errorf("fallback route %q, want anthropic", cc.FallbackRoute)
	}
	if !cc.Enabled {
		t.Errorf("enabled %t, want true", cc.Enabled)
	}
	if cc.DefaultModel != "zai-org/GLM-5.2" {
		t.Errorf("claude-code default_model = %q, want zai-org/GLM-5.2", cc.DefaultModel)
	}
	cx := f.Clients[1]
	if cx.Name != "codex" {
		t.Fatalf("second client: got %q; want codex", cx.Name)
	}
	if cx.Enabled {
		t.Errorf("codex enabled %t, want false (ships parked; un-parked by 'baseten-switch codex on')", cx.Enabled)
	}
	if cx.BindAddr != "127.0.0.1:45272" {
		t.Errorf("codex bind addr %q, want 127.0.0.1:45272 (shared listener, dispatch by path)", cx.BindAddr)
	}
	if cx.ProtocolShape != "openai" {
		t.Errorf("codex shape %q, want openai", cx.ProtocolShape)
	}
	if cx.FallbackRoute != "openai" {
		t.Errorf("codex fallback route %q, want openai", cx.FallbackRoute)
	}
	if cx.DefaultModel != "zai-org/GLM-5.2" {
		t.Errorf("codex default_model = %q, want zai-org/GLM-5.2", cx.DefaultModel)
	}
	if len(cx.ResponsesStripToolTypes) != 0 {
		t.Errorf("codex responses_strip_tool_types = %v, want empty (emergency override only)", cx.ResponsesStripToolTypes)
	}
	compatibility, err := ResolveResponsesCompatibility(cx.ResponsesCompatibility)
	if err != nil {
		t.Fatalf("resolve codex responses_compatibility: %v", err)
	}
	if compatibility.TextFormatDefault != ResponsesCompatibilityModeOn ||
		compatibility.AdditionalToolsInput != ResponsesCompatibilityModeOff ||
		compatibility.ReasoningEffort != ResponsesCompatibilityModeOn ||
		compatibility.FunctionArgumentsConsistency != ResponsesCompatibilityModeOn {
		t.Errorf("codex responses_compatibility = %+v, want supported defaults", compatibility)
	}
	// The parked codex client's ${CODEX_AUTH_TOKEN} must stay out of
	// the placeholder scan: a disabled client binds nothing, so a
	// fresh install must not warn about a variable that is unused
	// until 'codex on' (which writes the stub itself).
	for _, name := range f.CollectPlaceholders() {
		if name == "CODEX_AUTH_TOKEN" {
			t.Errorf("CollectPlaceholders includes CODEX_AUTH_TOKEN from the parked codex client; disabled clients must be exempt from the scan")
		}
	}
	// The canonical configuration keeps the supported gateway-discovery
	// aliases live.
	wantAliases := map[string]string{
		"claude-baseten-glm-5-2":   "zai-org/GLM-5.2",
		"claude-baseten-kimi-k2-7": "moonshotai/Kimi-K2.7-Code",
		"claude-baseten-nemotron":  "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B",
	}
	if len(cc.ModelAliases) != len(wantAliases) {
		t.Errorf("model_aliases = %v, want %v", cc.ModelAliases, wantAliases)
	}
	for alias, slug := range wantAliases {
		if cc.ModelAliases[alias] != slug {
			t.Errorf("model_aliases[%s] = %q, want %q", alias, cc.ModelAliases[alias], slug)
		}
	}
	// subagent_model/subagent_routing ship commented out next to
	// model_aliases (the subagent-routing contract); the parsed client must
	// not carry them.
	if !bytes.Contains(InitTemplate, []byte("# subagent_model:")) {
		t.Errorf("template lost the commented subagent_model example; keep it next to model_aliases")
	}
	if !bytes.Contains(InitTemplate, []byte("# subagent_routing:")) {
		t.Errorf("template lost the commented subagent_routing example; keep it next to model_aliases")
	}
	wantRoutes := map[string]string{
		"fable":  "zai-org/GLM-5.2",
		"opus":   "zai-org/GLM-5.2",
		"sonnet": "zai-org/GLM-5.2",
		"haiku":  "zai-org/GLM-5.2",
	}
	// responses_strip_tool_types belongs to the codex client only; an
	// anthropic-shape client carrying it refuses the config at gateway
	// resolve time.
	if len(cc.ResponsesStripToolTypes) != 0 {
		t.Errorf("claude-code must not carry responses_strip_tool_types, got %v", cc.ResponsesStripToolTypes)
	}
	for name, slug := range wantRoutes {
		if cc.ModelRoutes[name] != slug {
			t.Errorf("claude-code model_routes[%s] = %q, want %q", name, cc.ModelRoutes[name], slug)
		}
	}
	if err := ValidateRoutingPolicy(f); err != nil {
		t.Errorf("template routing policy is invalid: %v", err)
	}

	if f.Door == nil || len(f.Door.Ports) != 1 {
		t.Fatalf("want a door section with exactly one port, got %+v", f.Door)
	}
	dp := f.Door.Ports[0]
	if dp.BindAddr != "127.0.0.1:45271" || dp.RouterAddr != "127.0.0.1:45272" {
		t.Errorf("door port %s -> %s, want 127.0.0.1:45271 -> 127.0.0.1:45272", dp.BindAddr, dp.RouterAddr)
	}
}
