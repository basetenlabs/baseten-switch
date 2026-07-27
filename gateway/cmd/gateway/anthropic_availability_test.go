package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

const anthropicAvailabilityFixture = `{
  "data": [
    {
      "type": "model",
      "id": "claude-opus-5",
      "display_name": "Account Opus 5",
      "created_at": "2026-07-01T00:00:00Z",
      "max_input_tokens": 1000000,
      "max_tokens": 128000,
      "credential_echo": "raw-provider-secret"
    },
    {
      "type": "model",
      "id": "claude-account-only",
      "display_name": "Account Only",
      "max_input_tokens": 240000,
      "max_tokens": 32000
    }
  ],
  "has_more": false,
  "first_id": "claude-opus-5",
  "last_id": "claude-account-only"
}`

func TestAnthropicAvailabilityRequiresExplicitPaginationState(t *testing.T) {
	_, _, _, err := parseAnthropicModelsPage(
		[]byte(`{"data":[{"type":"model","id":"claude-opus-5"}]}`),
	)
	if err == nil || !strings.Contains(err.Error(), "has_more") {
		t.Fatalf("missing has_more error = %v", err)
	}
}

func TestAuthenticatedAnthropicAvailabilityPreservesPricingAndRestartsFromCache(t *testing.T) {
	const harnessKey = "test-sensitive-harness-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get("X-Api-Key"); key != harnessKey {
			t.Errorf("X-Api-Key = %q, want harness credential", key)
		}
		if version := r.Header.Get("Anthropic-Version"); version != "2023-06-01" {
			t.Errorf("Anthropic-Version = %q", version)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicAvailabilityFixture)
	}))
	defer server.Close()

	modelPricing := pricing.New()
	if err := modelPricing.ReplaceModelsDev(
		[]byte(publicCatalogGatewayFixture),
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		`"catalog-v1"`,
	); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	g := &Gateway{
		cfg: Config{
			ConfigPath:   configPath,
			AnthropicURL: server.URL,
		},
		pricing: modelPricing,
		client:  server.Client(),
	}
	restoreAnthropicAvailabilityNow(t)
	capturedAt := time.Date(2026, time.July, 26, 15, 0, 0, 0, time.UTC)
	anthropicAvailabilityNow = func() time.Time { return capturedAt }

	request := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.local/v1/models?limit=1000",
		nil,
	)
	request.Header.Set("X-Api-Key", harnessKey)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	entries := g.nativeModelEntries(request)
	if len(entries) != 2 {
		t.Fatalf("native entries = %d, want 2", len(entries))
	}

	quote := modelPricing.Capture().QuoteProfile(
		pricing.ProviderAnthropic,
		"claude-opus-5",
		pricing.ProfileStandard,
	)
	if !quote.Priced || quote.Price.Prompt != 5 || quote.Price.Completion != 25 ||
		quote.Source != "models_dev" {
		t.Fatalf("account availability replaced public pricing: %+v", quote)
	}
	opus, ok := modelPricing.Capture().Model(
		pricing.ProviderAnthropic,
		"claude-opus-5",
	)
	if !ok || opus.Availability.Public == nil ||
		opus.Availability.Account == nil {
		t.Fatalf("merged Opus availability = %+v, found=%t", opus, ok)
	}
	if evidence := opus.Availability.Account; evidence.State != pricing.AvailabilityAvailable ||
		evidence.Scope != pricing.AvailabilityScopeUnscopedLastObserved ||
		evidence.Provenance.Source != anthropicAvailabilitySource ||
		evidence.Provenance.LoadedFrom != pricing.LoadedFromLive ||
		evidence.Provenance.CapturedAt != capturedAt {
		t.Fatalf("account availability evidence = %+v", evidence)
	}
	accountOnly, ok := modelPricing.Capture().Model(
		pricing.ProviderAnthropic,
		"claude-account-only",
	)
	if !ok || accountOnly.DisplayName != "Account Only" ||
		accountOnly.ContextTokens != 240_000 ||
		accountOnly.MaxOutputTokens != 32_000 ||
		accountOnly.Availability.Account == nil {
		t.Fatalf("sanitized account-only model = %+v, found=%t", accountOnly, ok)
	}
	if _, priced := accountOnly.Prices[pricing.ProfileStandard]; priced {
		t.Fatalf("availability synthesized a price: %+v", accountOnly.Prices)
	}

	cachePath := providerCatalogCachePath(configPath, pricing.ProviderAnthropic)
	cacheBody, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read Anthropic cache: %v", err)
	}
	for _, forbidden := range []string{
		harnessKey,
		"raw-provider-secret",
		"credential_echo",
		"created_at",
	} {
		if bytes.Contains(cacheBody, []byte(forbidden)) {
			t.Fatalf("runtime cache retained forbidden raw data %q", forbidden)
		}
	}

	restarted := pricing.New()
	loadProviderCatalogCaches(restarted, configPath)
	restartedQuote := restarted.Capture().QuoteProfile(
		pricing.ProviderAnthropic,
		"claude-opus-5",
		pricing.ProfileStandard,
	)
	if !restartedQuote.Priced || restartedQuote.Price.Prompt != 5 ||
		restartedQuote.Source != "models_dev" {
		t.Fatalf("restarted price = %+v", restartedQuote)
	}
	restartedOpus, ok := restarted.Capture().Model(
		pricing.ProviderAnthropic,
		"claude-opus-5",
	)
	if !ok || restartedOpus.Availability.Account == nil ||
		restartedOpus.Availability.Account.Provenance.Source != anthropicAvailabilitySource ||
		restartedOpus.Availability.Account.Provenance.LoadedFrom != pricing.LoadedFromRuntimeCache {
		t.Fatalf("restarted account availability = %+v, found=%t", restartedOpus, ok)
	}
}

func TestAuthenticatedAnthropicAvailabilityFailuresPreserveSnapshotAndCache(t *testing.T) {
	const (
		availabilitySuccess int32 = iota
		availabilityMalformed
		availabilityUnavailable
	)
	var mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case availabilitySuccess:
			_, _ = io.WriteString(w, anthropicAvailabilityFixture)
		case availabilityMalformed:
			_, _ = io.WriteString(w, `{"data":[{"type":"model","id":""}]}`)
		case availabilityUnavailable:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"unavailable"}`)
		}
	}))
	defer server.Close()

	modelPricing := pricing.New()
	if err := modelPricing.ReplaceModelsDev(
		[]byte(publicCatalogGatewayFixture),
		time.Now().UTC(),
		"",
	); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	g := &Gateway{
		cfg: Config{
			ConfigPath:   configPath,
			AnthropicURL: server.URL,
		},
		pricing: modelPricing,
		client:  server.Client(),
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.local/v1/models?limit=1000",
		nil,
	)
	if entries := g.nativeModelEntries(request); len(entries) != 2 {
		t.Fatalf("initial native entries = %d, want 2", len(entries))
	}
	lastKnownGood := modelPricing.Capture()
	cachePath := providerCatalogCachePath(configPath, pricing.ProviderAnthropic)
	cacheBefore, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}

	for _, failure := range []struct {
		name string
		mode int32
	}{
		{name: "malformed", mode: availabilityMalformed},
		{name: "non-2xx", mode: availabilityUnavailable},
	} {
		t.Run(failure.name, func(t *testing.T) {
			mode.Store(failure.mode)
			if entries := g.nativeModelEntries(request); entries != nil {
				t.Fatalf("%s response produced discovery entries: %+v", failure.name, entries)
			}
			if modelPricing.Capture() != lastKnownGood {
				t.Fatalf("%s response replaced the active snapshot", failure.name)
			}
			cacheAfter, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(cacheAfter, cacheBefore) {
				t.Fatalf("%s response replaced the runtime cache", failure.name)
			}
		})
	}
}

func TestClaudeDiscoveryKeepsCachedPricedModelsWhenAnthropicFetchFails(t *testing.T) {
	live := pricing.New()
	if err := live.ReplaceModelsDev(
		[]byte(publicCatalogGatewayFixture),
		time.Now().UTC(),
		"",
	); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := persistProviderCatalogCaches(live, configPath); err != nil {
		t.Fatal(err)
	}
	cached := pricing.New()
	loadProviderCatalogCaches(cached, configPath)

	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unavailable.Close()
	g := &Gateway{
		cfg: Config{
			ConfigPath:   configPath,
			AnthropicURL: unavailable.URL,
		},
		pricing: cached,
		client:  unavailable.Client(),
	}
	client := &clientListener{cfg: aliasedClient(t, "anthropic")}
	request := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.local/v1/models?limit=1000",
		nil,
	)
	recorder := httptest.NewRecorder()

	g.forwardModelsGet(client, recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var list modelsList
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, entry := range list.Data {
		ids = append(ids, entry.ID)
	}
	joined := "," + strings.Join(ids, ",") + ","
	if !strings.Contains(joined, ",claude-opus-5,") {
		t.Fatalf("cached priced model disappeared after native failure: %v", ids)
	}
	if strings.Contains(joined, ",claude-haiku-unpriced,") {
		t.Fatalf("cached unpriced model was published: %v", ids)
	}
}

func restoreAnthropicAvailabilityNow(t *testing.T) {
	t.Helper()
	original := anthropicAvailabilityNow
	t.Cleanup(func() {
		anthropicAvailabilityNow = original
	})
}
