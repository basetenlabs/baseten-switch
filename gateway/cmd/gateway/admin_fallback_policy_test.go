package gateway

import (
	"reflect"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func TestBasetenModelFallbackStatusUsesNativeProviderReadiness(t *testing.T) {
	rc := resolvedClientConfig{
		ProtocolShape: "anthropic", FallbackRoute: "anthropic",
		NativeFallbackModel: config.DefaultClaudeNativeFallbackModel,
	}
	// Native fallback preserves harness authentication on each request, so
	// process-level provider keys are not the readiness authority.
	t.Setenv("ANTHROPIC_API_KEY", "")
	status := basetenModelFallbackStatus(rc, false)
	if status["provider_ready"] != true || status["ready"] != true || status["reason"] != nil {
		t.Fatalf("ready status=%#v", status)
	}
	if got := status["available_models"]; !reflect.DeepEqual(got, []map[string]any{
		{"model": "claude-opus-5", "display_name": "Opus"},
		{"model": "claude-fable-5", "display_name": "Fable"},
		{"model": "claude-opus-4-8", "display_name": "Opus"},
		{"model": "claude-sonnet-4-6", "display_name": "Sonnet"},
		{"model": "claude-haiku-4-5", "display_name": "Haiku"},
	}) {
		t.Fatalf("available_models=%#v", got)
	}
	rc.FallbackRoute = ""
	status = basetenModelFallbackStatus(rc, false)
	if status["provider_ready"] != false || status["ready"] != false || status["reason"] != "provider_auth_unavailable" {
		t.Fatalf("unavailable status=%#v", status)
	}
}

func TestNativeFallbackAvailableModelsIncludeValidCurrentTargetOnce(t *testing.T) {
	rc := resolvedClientConfig{
		ProtocolShape:       "anthropic",
		FallbackRoute:       "anthropic",
		NativeFallbackModel: "claude-sonnet-5",
	}
	models := nativeFallbackAvailableModels(rc)
	if len(models) < 2 || models[0]["model"] != "claude-sonnet-5" {
		t.Fatalf("available_models=%#v, want custom current target first and multiple choices", models)
	}
	seen := map[string]bool{}
	for _, entry := range models {
		model := entry["model"].(string)
		if seen[model] {
			t.Fatalf("available_models contains duplicate %q: %#v", model, models)
		}
		seen[model] = true
		if !config.IsProtocolNativeModel("anthropic", model) {
			t.Fatalf("available model %q is not Anthropic-native", model)
		}
	}

	rc.NativeFallbackModel = "claude-baseten-invalid"
	for _, entry := range nativeFallbackAvailableModels(rc) {
		if entry["model"] == rc.NativeFallbackModel {
			t.Fatalf("invalid current target leaked into available models")
		}
	}
}

func TestFallbackAdminCooldownHonorsActivePolicy(t *testing.T) {
	g := &Gateway{fallbackUntil: map[string]fallbackCooldownState{
		"claude-code": {Until: time.Now().Add(time.Minute), Trigger: fallbackCooldownHTTP429},
	}}
	rc := resolvedClientConfig{
		Name: "claude-code", FallbackRoute: "anthropic", HasFallbackPolicy: true,
		FallbackPolicy: config.ResolvedFallbackPolicy{OnBaseten429: false, OnBaseten5xx: true},
	}
	if status := fallbackStatus(g, rc); status["active"] != false {
		t.Fatalf("disabled 429 cooldown status=%#v", status)
	}
	g.fallbackUntil["claude-code"] = fallbackCooldownState{Until: time.Now().Add(time.Minute), Trigger: "transport_error"}
	if status := fallbackStatus(g, rc); status["active"] != true {
		t.Fatalf("unrelated cooldown status=%#v", status)
	}
}

func TestAdminStatusProjectsResolvedFallbackPolicyAndClaudeTarget(t *testing.T) {
	baseten := recordingStub(t, nil, "BASETEN")
	defer baseten.Close()
	anthropic := recordingStub(t, nil, "NATIVE")
	defer anthropic.Close()
	f := globalRoutingFile(true)
	f.Clients[0].NativeFallbackModel = config.DefaultClaudeNativeFallbackModel
	policyOff := false
	f.Global.FallbackPolicy = &config.FallbackPolicy{OnBaseten429: &policyOff}
	g, stop := newGlobalRoutingGateway(t, f, baseten.URL, anthropic.URL)
	defer stop()
	status := adminStatusGet(t, g)
	policy := status["fallback_policy"].(map[string]any)
	if policy["on_baseten_429"] != false || policy["on_baseten_5xx"] != true {
		t.Fatalf("fallback_policy=%#v", policy)
	}
	client := status["clients"].([]any)[0].(map[string]any)
	projection := client["baseten_model_fallback"].(map[string]any)
	if projection["configured_model"] != config.DefaultClaudeNativeFallbackModel ||
		projection["resolved_model"] != config.DefaultClaudeNativeFallbackModel ||
		projection["display_name"] != "Opus" || projection["provider_ready"] != true ||
		projection["ready"] != true || projection["reason"] != nil {
		t.Fatalf("baseten_model_fallback=%#v", projection)
	}
	available := projection["available_models"].([]any)
	if len(available) < 2 {
		t.Fatalf("available_models=%#v, want multiple native choices", available)
	}
	for _, raw := range available {
		entry := raw.(map[string]any)
		model := entry["model"].(string)
		if !config.IsProtocolNativeModel("anthropic", model) {
			t.Fatalf("available model %q is not Anthropic-native", model)
		}
		if entry["display_name"] == "" {
			t.Fatalf("available model %q has empty display name", model)
		}
	}
}
