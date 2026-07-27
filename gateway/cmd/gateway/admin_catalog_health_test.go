package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

func TestModelCatalogHealthJSONReportsEveryProvider(t *testing.T) {
	now := time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC)
	p := pricing.New()
	if err := p.ReplaceModelsDev(
		[]byte(publicCatalogGatewayFixture),
		now,
		`"catalog-v1"`,
	); err != nil {
		t.Fatal(err)
	}
	manager := newPublicCatalogRefreshManager()
	manager.startedAt = now.Add(-time.Hour)
	manager.lastAttempt = now.Add(-20 * time.Minute)
	manager.lastSuccess = now.Add(-19 * time.Minute)
	manager.nextAt = now.Add(8 * time.Hour)
	g := &Gateway{
		pricing:              p,
		publicCatalogRefresh: manager,
	}

	result := g.modelCatalogHealthJSON()
	for _, provider := range []string{
		pricing.ProviderAnthropic,
		pricing.ProviderOpenAI,
		pricing.ProviderBaseten,
	} {
		raw, ok := result[provider]
		if !ok {
			t.Fatalf("model catalog health missing %q", provider)
		}
		health, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("%s health type = %T", provider, raw)
		}
		for _, field := range []string{
			"source",
			"loaded_from",
			"revision",
			"model_count",
			"priced_model_count",
			"last_attempt_at",
			"last_success_at",
			"next_refresh_at",
			"stale",
			"last_error",
			"diagnostics",
		} {
			if _, ok := health[field]; !ok {
				t.Errorf("%s health missing %q", provider, field)
			}
		}
		if health["source"] != "models_dev" {
			t.Errorf("%s source = %v", provider, health["source"])
		}
		if health["loaded_from"] != string(pricing.LoadedFromLive) {
			t.Errorf("%s loaded_from = %v", provider, health["loaded_from"])
		}
		if health["last_attempt_at"] != rfc3339OrEmpty(manager.lastAttempt) {
			t.Errorf("%s last_attempt_at = %v", provider, health["last_attempt_at"])
		}
		if health["last_error"] != nil {
			t.Errorf("%s last_error = %v, want null", provider, health["last_error"])
		}
	}

	anthropic := result[pricing.ProviderAnthropic].(map[string]any)
	if anthropic["model_count"] != 3 ||
		anthropic["priced_model_count"] != 2 {
		t.Fatalf("Anthropic counts = models %v, priced %v",
			anthropic["model_count"],
			anthropic["priced_model_count"])
	}
}

func TestModelCatalogHealthReportsSanitizedReasoningDiagnostics(
	t *testing.T,
) {
	fixture := strings.Replace(
		publicCatalogGatewayFixture,
		`"reasoning_options": [{"type": "toggle"}]`,
		`"reasoning_options": [
			{"type": "future_secret_type", "secret": "must-not-escape"},
			{"type": "toggle"}
		]`,
		1,
	)
	p := pricing.New()
	if err := p.ReplaceModelsDev(
		[]byte(fixture),
		time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC),
		`"catalog-v1"`,
	); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{
		pricing:              p,
		publicCatalogRefresh: newPublicCatalogRefreshManager(),
	}
	health := g.modelCatalogHealthJSON()[pricing.ProviderBaseten].(map[string]any)
	diagnostics, ok := health["diagnostics"].([]string)
	if !ok || len(diagnostics) != 1 ||
		diagnostics[0] !=
			"ignored 1 unknown reasoning option type(s)" {
		t.Fatalf("diagnostics = %#v", health["diagnostics"])
	}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "future_secret_type") ||
		strings.Contains(string(encoded), "must-not-escape") {
		t.Fatalf("diagnostics leaked raw option data: %s", encoded)
	}
}

func TestModelCatalogHealthJSONUsesAuthenticatedBasetenRefreshState(t *testing.T) {
	publicAt := time.Date(2026, time.July, 27, 8, 0, 0, 0, time.UTC)
	authAt := publicAt.Add(time.Hour)
	p := pricing.New()
	if err := p.ReplaceModelsDev(
		[]byte(publicCatalogGatewayFixture),
		publicAt,
		`"catalog-v1"`,
	); err != nil {
		t.Fatal(err)
	}
	if err := p.ReplaceBasetenCatalog(
		[]byte(liveCatalogFixture),
		"baseten_v1_models",
		authAt,
		"",
	); err != nil {
		t.Fatal(err)
	}

	publicManager := newPublicCatalogRefreshManager()
	publicManager.lastAttempt = publicAt.Add(10 * time.Minute)
	publicManager.lastSuccess = publicAt.Add(11 * time.Minute)
	publicManager.nextAt = publicAt.Add(12 * time.Hour)
	authManager := newCatalogRefreshManager()
	authManager.startedAt = authAt.Add(-time.Minute)
	authManager.lastAttempt = authAt.Add(time.Minute)
	authManager.lastSuccess = authAt.Add(2 * time.Minute)
	authManager.nextAt = authAt.Add(time.Hour)
	authManager.lastError = `Get "https://user:secret@api.example.test/v1/models?token=secret": open /Users/test/private/catalog.json: permission denied`

	restoreCatalogClock(t)
	catalogNow = func() time.Time { return authAt.Add(30 * time.Minute) }
	g := &Gateway{
		pricing:              p,
		publicCatalogRefresh: publicManager,
		catalogRefresh:       authManager,
	}

	result := g.modelCatalogHealthJSON()
	baseten := result[pricing.ProviderBaseten].(map[string]any)
	if baseten["source"] != "baseten_v1_models" {
		t.Fatalf("Baseten source = %v", baseten["source"])
	}
	if baseten["last_attempt_at"] != rfc3339OrEmpty(authManager.lastAttempt) ||
		baseten["last_success_at"] != rfc3339OrEmpty(authManager.lastSuccess) ||
		baseten["next_refresh_at"] != rfc3339OrEmpty(authManager.nextAt) {
		t.Fatalf("Baseten refresh timing = %+v", baseten)
	}
	if baseten["last_error"] != "catalog cache operation failed" {
		t.Fatalf("Baseten last_error = %v", baseten["last_error"])
	}
	if anthropic := result[pricing.ProviderAnthropic].(map[string]any); anthropic["last_attempt_at"] != rfc3339OrEmpty(publicManager.lastAttempt) {
		t.Fatalf("Anthropic refresh timing = %+v", anthropic)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{
		"secret",
		"/Users/test",
		"api.example.test",
		"catalog.json",
	} {
		if strings.Contains(string(encoded), sensitive) {
			t.Errorf("diagnostics exposed %q: %s", sensitive, encoded)
		}
	}
}

func TestSanitizeCatalogDiagnosticError(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "empty", message: " \n", want: ""},
		{name: "timeout", message: "context deadline exceeded", want: "refresh timed out"},
		{name: "canceled", message: "refresh canceled", want: "refresh canceled"},
		{name: "authorization", message: "models returned 401", want: "catalog source rejected authorization"},
		{name: "status", message: "models.dev returned 502", want: "catalog source returned a non-success status"},
		{name: "size", message: "response exceeds 1024 bytes", want: "catalog response exceeded size limit"},
		{name: "validation", message: "decode catalog: invalid JSON", want: "catalog response failed validation"},
		{name: "cache", message: "persist cache: permission denied", want: "catalog cache operation failed"},
		{
			name:    "hostile",
			message: "Get https://user:token@example.test/private?q=raw\n{\"key\":\"value\"}",
			want:    "catalog refresh failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeCatalogDiagnosticError(test.message); got != test.want {
				t.Fatalf("sanitizeCatalogDiagnosticError(%q) = %q, want %q",
					test.message, got, test.want)
			}
		})
	}
}

func restoreCatalogClock(t *testing.T) {
	t.Helper()
	original := catalogNow
	t.Cleanup(func() {
		catalogNow = original
	})
}
