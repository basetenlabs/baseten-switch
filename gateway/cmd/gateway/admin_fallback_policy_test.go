package gateway

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

const nativeFallbackCatalogFixture = `{
  "anthropic": {
    "id": "anthropic",
    "models": {
      "claude-fable-5": {
        "id": "claude-fable-5",
        "name": "Claude Fable 5",
        "family": "claude-fable",
        "release_date": "2026-06-07",
        "cost": {"input": 5, "output": 25}
      },
      "claude-fable-5-1": {
        "id": "claude-fable-5-1",
        "name": "Claude Fable 5.1",
        "family": "claude-fable",
        "release_date": "2026-09",
        "cost": {"input": 5, "output": 25}
      },
      "claude-opus-5": {
        "id": "claude-opus-5",
        "name": "Claude Opus 5 (latest)",
        "family": "claude-opus",
        "release_date": "2026-07-24",
        "cost": {"input": 5, "output": 25}
      },
      "claude-opus-4-5": {
        "id": "claude-opus-4-5",
        "name": "Claude Opus 4.5 (latest)",
        "family": "claude-opus",
        "release_date": "2025-11-24",
        "cost": {"input": 5, "output": 25}
      },
      "claude-opus-4-5-20251101": {
        "id": "claude-opus-4-5-20251101",
        "name": "Claude Opus 4.5",
        "family": "claude-opus",
        "release_date": "2025-11-24",
        "cost": {"input": 5, "output": 25}
      },
      "claude-opus-4-8": {
        "id": "claude-opus-4-8",
        "name": "Claude Opus 4.8",
        "family": "claude-opus",
        "release_date": "2026-05-28",
        "cost": {"input": 5, "output": 25}
      },
      "claude-sonnet-5": {
        "id": "claude-sonnet-5",
        "name": "Claude Sonnet 5",
        "family": "claude-sonnet",
        "release_date": "2026-06-29",
        "cost": {"input": 2, "output": 10}
      },
      "claude-haiku-4-5": {
        "id": "claude-haiku-4-5",
        "name": "Claude Haiku 4.5 (latest)",
        "family": "claude-haiku",
        "release_date": "2025-10-15",
        "cost": {"input": 1, "output": 5}
      },
      "claude-haiku-4-5-20251001": {
        "id": "claude-haiku-4-5-20251001",
        "name": "Claude Haiku 4.5",
        "family": "claude-haiku",
        "release_date": "2025-10-15",
        "cost": {"input": 1, "output": 5}
      },
      "claude-haiku-5": {
        "id": "claude-haiku-5",
        "name": "Claude Haiku 5",
        "family": "claude-haiku",
        "release_date": "2026-08-01"
      },
      "claude-haiku-unpriced": {
        "id": "claude-haiku-unpriced",
        "name": "Claude Haiku Unpriced",
        "family": "claude-haiku"
      },
      "claude-haiku-retired": {
        "id": "claude-haiku-retired",
        "name": "Claude Haiku Retired",
        "family": "claude-haiku",
        "status": "deprecated",
        "cost": {"input": 1, "output": 5}
      },
      "claude-baseten-invalid": {
        "id": "claude-baseten-invalid",
        "name": "Invalid Gateway Alias",
        "cost": {"input": 1, "output": 5}
      }
    }
  },
  "openai": {
    "id": "openai",
    "models": {
      "gpt-test": {
        "id": "gpt-test",
        "name": "GPT Test",
        "cost": {"input": 1, "output": 4}
      }
    }
  },
  "baseten": {
    "id": "baseten",
    "models": {
      "example/Model": {
        "id": "example/Model",
        "name": "Example Model",
        "cost": {"input": 1, "output": 4}
      }
    }
  }
}`

func nativeFallbackCatalogSnapshot(t *testing.T) *pricing.Snapshot {
	t.Helper()
	catalog := pricing.New()
	if err := catalog.ReplaceModelsDev(
		[]byte(nativeFallbackCatalogFixture),
		time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC),
		`"fallback-catalog"`,
	); err != nil {
		t.Fatal(err)
	}
	return catalog.Capture()
}

func TestBasetenModelFallbackStatusUsesNativeProviderReadiness(t *testing.T) {
	rc := resolvedClientConfig{
		ProtocolShape: "anthropic", FallbackRoute: "anthropic",
		NativeFallbackModel: config.DefaultClaudeNativeFallbackModel,
	}
	// Native fallback preserves harness authentication on each request, so
	// process-level provider keys are not the readiness authority.
	t.Setenv("ANTHROPIC_API_KEY", "")
	catalog := nativeFallbackCatalogSnapshot(t)
	status := basetenModelFallbackStatus(rc, false, catalog)
	if status["provider_ready"] != true || status["ready"] != true || status["reason"] != nil {
		t.Fatalf("ready status=%#v", status)
	}
	if status["display_name"] != "Claude Opus 5" {
		t.Fatalf("display_name=%q", status["display_name"])
	}
	if got := status["available_models"]; !reflect.DeepEqual(got, []map[string]any{
		{"model": "claude-fable-5-1", "display_name": "Claude Fable 5.1"},
		{"model": "claude-opus-5", "display_name": "Claude Opus 5"},
		{"model": "claude-sonnet-5", "display_name": "Claude Sonnet 5"},
		{"model": "claude-haiku-5", "display_name": "Claude Haiku 5"},
	}) {
		t.Fatalf("available_models=%#v", got)
	}
	rc.FallbackRoute = ""
	status = basetenModelFallbackStatus(rc, false, catalog)
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
	catalog := nativeFallbackCatalogSnapshot(t)
	models := nativeFallbackAvailableModels(rc, catalog)
	if !reflect.DeepEqual(models, []map[string]any{
		{"model": "claude-fable-5-1", "display_name": "Claude Fable 5.1"},
		{"model": "claude-opus-5", "display_name": "Claude Opus 5"},
		{"model": "claude-sonnet-5", "display_name": "Claude Sonnet 5"},
		{"model": "claude-haiku-5", "display_name": "Claude Haiku 5"},
	}) {
		t.Fatalf("available_models=%#v", models)
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
	for _, entry := range nativeFallbackAvailableModels(rc, catalog) {
		if entry["model"] == rc.NativeFallbackModel {
			t.Fatalf("invalid current target leaked into available models")
		}
	}

	rc.NativeFallbackModel = "claude-opus-5"
	models = nativeFallbackAvailableModels(rc, catalog)
	if len(models) != 4 || models[1]["model"] != "claude-opus-5" {
		t.Fatalf("catalog current target was duplicated: %#v", models)
	}
}

func TestNativeFallbackOptionsDoNotIncludeOlderCurrentModel(t *testing.T) {
	rc := resolvedClientConfig{
		ProtocolShape:       "anthropic",
		NativeFallbackModel: "claude-opus-4-5-20251101",
	}
	models := nativeFallbackAvailableModels(rc, nativeFallbackCatalogSnapshot(t))
	if !reflect.DeepEqual(models, []map[string]any{
		{"model": "claude-fable-5-1", "display_name": "Claude Fable 5.1"},
		{"model": "claude-opus-5", "display_name": "Claude Opus 5"},
		{"model": "claude-sonnet-5", "display_name": "Claude Sonnet 5"},
		{"model": "claude-haiku-5", "display_name": "Claude Haiku 5"},
	}) {
		t.Fatalf("available_models=%#v", models)
	}
}

func TestNativeFallbackOptionsUseVersionForLegacyCacheWithoutReleaseDates(t *testing.T) {
	fixture := nativeFallbackCatalogFixture
	for _, releaseDate := range []string{
		"2025-10-15",
		"2025-11-24",
		"2026-05-28",
		"2026-06-07",
		"2026-06-29",
		"2026-07-24",
		"2026-08-01",
		"2026-09",
	} {
		fixture = strings.ReplaceAll(fixture, releaseDate, "")
	}
	catalog := pricing.New()
	if err := catalog.ReplaceModelsDev(
		[]byte(fixture), time.Now().UTC(), `"legacy-cache"`,
	); err != nil {
		t.Fatal(err)
	}
	models := nativeFallbackAvailableModels(
		resolvedClientConfig{ProtocolShape: "anthropic"},
		catalog.Capture(),
	)
	if !reflect.DeepEqual(models, []map[string]any{
		{"model": "claude-fable-5-1", "display_name": "Claude Fable 5.1"},
		{"model": "claude-opus-5", "display_name": "Claude Opus 5"},
		{"model": "claude-sonnet-5", "display_name": "Claude Sonnet 5"},
		{"model": "claude-haiku-5", "display_name": "Claude Haiku 5"},
	}) {
		t.Fatalf("available_models=%#v", models)
	}
}

func TestNativeFallbackVersionSupportsCurrentAndLegacyIDOrders(t *testing.T) {
	for _, test := range []struct {
		model  string
		family string
		want   []uint64
	}{
		{"claude-fable-5-1", "fable", []uint64{5, 1}},
		{"claude-haiku-4-5-20251001", "haiku", []uint64{4, 5}},
		{"claude-3-7-sonnet-20250219", "sonnet", []uint64{3, 7}},
		{"claude-opus-latest", "opus", nil},
	} {
		if got := nativeFallbackVersion(test.model, test.family); !reflect.DeepEqual(got, test.want) {
			t.Errorf("nativeFallbackVersion(%q, %q)=%v, want %v", test.model, test.family, got, test.want)
		}
	}
}

func TestNativeFallbackReleaseDateComparisonSupportsMixedPrecision(t *testing.T) {
	for _, test := range []struct {
		left  string
		right string
		want  int
	}{
		{"2026-09", "2026-08-31", 1},
		{"2026-09-01", "2026-09", 1},
		{"2026-08-31", "2026-09", -1},
		{"2026-09", "2026-09", 0},
		{"", "2026-09", -1},
		{"2026-09", "", 1},
	} {
		if got := compareNativeFallbackReleaseDates(test.left, test.right); got != test.want {
			t.Errorf("compareNativeFallbackReleaseDates(%q, %q)=%d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestNormalizeNativeFallbackDisplayNameRemovesOnlyExactTrailingSuffix(t *testing.T) {
	for _, test := range []struct {
		name string
		want string
	}{
		{"Claude Opus 5 (latest)", "Claude Opus 5"},
		{"Claude (latest) Opus 5", "Claude (latest) Opus 5"},
		{"Claude Opus 5 (Latest)", "Claude Opus 5 (Latest)"},
		{"Claude Opus 5", "Claude Opus 5"},
	} {
		if got := normalizeNativeFallbackDisplayName(test.name); got != test.want {
			t.Errorf("normalizeNativeFallbackDisplayName(%q)=%q, want %q", test.name, got, test.want)
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
	if err := g.pricing.ReplaceModelsDev(
		[]byte(nativeFallbackCatalogFixture),
		time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC),
		`"fallback-catalog"`,
	); err != nil {
		t.Fatal(err)
	}
	status := adminStatusGet(t, g)
	policy := status["fallback_policy"].(map[string]any)
	if policy["on_baseten_429"] != false || policy["on_baseten_5xx"] != true {
		t.Fatalf("fallback_policy=%#v", policy)
	}
	client := status["clients"].([]any)[0].(map[string]any)
	projection := client["baseten_model_fallback"].(map[string]any)
	if projection["configured_model"] != config.DefaultClaudeNativeFallbackModel ||
		projection["resolved_model"] != config.DefaultClaudeNativeFallbackModel ||
		projection["display_name"] != "Claude Opus 5" || projection["provider_ready"] != true ||
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
