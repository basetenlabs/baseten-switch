package pricing

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedOfficialPricingSupplementIsStrictAndScoped(t *testing.T) {
	supplement, err := parseOfficialPricingSupplement(
		officialPricingSupplementJSON,
	)
	if err != nil {
		t.Fatal(err)
	}
	if supplement.source != officialPricingSupplementSource ||
		supplement.contentSHA !=
			"0016612b23adf833e5f9a993f69c86fc71c515436a98d9c5c662672a894facf5" ||
		len(supplement.entries) != 16 {
		t.Fatalf("embedded supplement = %+v", supplement)
	}
	for _, entry := range supplement.entries {
		if entry.Provider != ProviderAnthropic ||
			entry.Dimension != RateCacheWrite1h ||
			strings.Contains(entry.Model, "claude-opus-4-1") {
			t.Fatalf("unexpected embedded entry = %+v", entry)
		}
	}

	var envelope map[string]any
	if err := json.Unmarshal(
		officialPricingSupplementJSON,
		&envelope,
	); err != nil {
		t.Fatal(err)
	}
	entries := envelope["entries"].([]any)
	first := entries[0].(map[string]any)
	first["provider"] = ProviderBaseten
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseOfficialPricingSupplement(body); err == nil ||
		!strings.Contains(err.Error(), "Baseten supplement entries are forbidden") {
		t.Fatalf("Baseten supplement error = %v", err)
	}

	withUnknownField := bytes.Replace(
		officialPricingSupplementJSON,
		[]byte(`"schema_version": 1`),
		[]byte(`"schema_version": 1, "unknown": true`),
		1,
	)
	if _, err := parseOfficialPricingSupplement(
		withUnknownField,
	); err == nil ||
		!strings.Contains(err.Error(), `unknown field "unknown"`) {
		t.Fatalf("unknown-field error = %v", err)
	}

	tampered := bytes.Replace(
		officialPricingSupplementJSON,
		[]byte(`"usd_per_million": 20`),
		[]byte(`"usd_per_million": 21`),
		1,
	)
	if _, err := parseOfficialPricingSupplement(tampered); err == nil ||
		!strings.Contains(err.Error(), "content_sha256") {
		t.Fatalf("tampered supplement error = %v", err)
	}
}

func TestOfficialPricingSupplementFillsOnlyMissingExistingRates(t *testing.T) {
	supplement, err := parseOfficialPricingSupplement(
		officialPricingSupplementJSON,
	)
	if err != nil {
		t.Fatal(err)
	}
	modelsDevProvenance := Provenance{
		Source: modelsDevSource, LoadedFrom: LoadedFromLive,
		Revision: "models-dev-test",
		CapturedAt: time.Date(
			2026,
			time.July,
			26,
			0,
			0,
			0,
			0,
			time.UTC,
		),
	}
	standardPresence := RatePresence{
		Input: true, Output: true, CacheRead: true, CacheWrite5m: true,
	}
	record := ModelRecord{
		Provider: ProviderAnthropic, CanonicalModelID: "claude-fable-5",
		DisplayName: "Claude Fable 5",
		Profiles: map[ExecutionProfile]ProfileDefinition{
			ProfileStandard: {
				Profile: ProfileStandard, Supported: true,
				Provenance: modelsDevProvenance,
			},
		},
		Prices: map[ExecutionProfile]PriceProfile{
			ProfileStandard: {
				Profile: ProfileStandard,
				Price: Price{
					Prompt: 10, Completion: 50,
					CacheRead: 1, CacheWrite5m: 12.5,
				},
				RatePresence: standardPresence, RatePresenceKnown: true,
				Provenance: modelsDevProvenance,
				RateProvenance: rateProvenanceForPresence(
					standardPresence,
					modelsDevProvenance,
				),
			},
		},
		Provenance: modelsDevProvenance,
	}
	catalogs := map[string]providerCatalog{
		ProviderAnthropic: {
			metadata: ProviderMetadata{
				Provider:   ProviderAnthropic,
				Provenance: modelsDevProvenance,
			},
			models: map[string]ModelRecord{
				record.CanonicalModelID: record,
			},
		},
	}
	applyOfficialPricingSupplement(
		catalogs,
		supplement,
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
	)
	filled := catalogs[ProviderAnthropic].
		models["claude-fable-5"].
		Prices[ProfileStandard]
	filledRecord := catalogs[ProviderAnthropic].models["claude-fable-5"]
	if !filled.RatePresence.CacheWrite1h ||
		filled.Price.CacheWrite5m != 12.5 ||
		filled.Price.CacheWrite1h != 20 ||
		filled.RateProvenance.CacheWrite5m.Source != modelsDevSource ||
		filled.RateProvenance.CacheWrite1h.Source !=
			officialPricingSupplementSource {
		t.Fatalf("filled Fable price = %+v", filled)
	}
	if filledRecord.Availability != (ModelAvailability{}) {
		t.Fatalf(
			"supplement asserted availability: %+v",
			filledRecord.Availability,
		)
	}
	if len(catalogs[ProviderAnthropic].models) != 1 ||
		catalogs[ProviderAnthropic].models["claude-opus-5"].
			CanonicalModelID != "" {
		t.Fatalf("supplement created a model: %+v", catalogs)
	}

	explicitZero := filled
	explicitZero.Price.CacheWrite1h = 0
	explicitZero.RatePresence.CacheWrite1h = true
	explicitZero.RateProvenance.CacheWrite1h = modelsDevProvenance
	record.Prices[ProfileStandard] = explicitZero
	catalog := catalogs[ProviderAnthropic]
	catalog.models[record.CanonicalModelID] = record
	catalog.metadata.Diagnostics = nil
	catalogs[ProviderAnthropic] = catalog
	applyOfficialPricingSupplement(
		catalogs,
		supplement,
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
	)
	kept := catalogs[ProviderAnthropic].
		models["claude-fable-5"].
		Prices[ProfileStandard]
	if kept.Price.CacheWrite1h != 0 ||
		kept.RateProvenance.CacheWrite1h.Source != modelsDevSource {
		t.Fatalf("explicit zero was overridden: %+v", kept)
	}
	diagnostics := catalogs[ProviderAnthropic].metadata.Diagnostics
	if len(diagnostics) != 1 ||
		!strings.Contains(
			diagnostics[0],
			"kept upstream anthropic/claude-fable-5/standard/cache_write_1h rate 0",
		) {
		t.Fatalf("conflict diagnostics = %#v", diagnostics)
	}
}

func TestOfficialPricingSupplementEffectiveWindowsFailClosed(t *testing.T) {
	supplement, err := parseOfficialPricingSupplement(
		officialPricingSupplementJSON,
	)
	if err != nil {
		t.Fatal(err)
	}
	priceAt := func(date time.Time) float64 {
		provenance := Provenance{
			Source: modelsDevSource, LoadedFrom: LoadedFromLive,
			Revision: "models-dev", CapturedAt: date,
		}
		presence := RatePresence{Input: true, Output: true}
		catalogs := map[string]providerCatalog{
			ProviderAnthropic: {
				models: map[string]ModelRecord{
					"claude-sonnet-5": {
						Provider:         ProviderAnthropic,
						CanonicalModelID: "claude-sonnet-5",
						DisplayName:      "Claude Sonnet 5",
						Profiles: map[ExecutionProfile]ProfileDefinition{
							ProfileStandard: {
								Profile: ProfileStandard, Supported: true,
								Provenance: provenance,
							},
						},
						Prices: map[ExecutionProfile]PriceProfile{
							ProfileStandard: {
								Profile:      ProfileStandard,
								Price:        Price{Prompt: 1, Completion: 2},
								RatePresence: presence, RatePresenceKnown: true,
								Provenance: provenance,
								RateProvenance: rateProvenanceForPresence(
									presence,
									provenance,
								),
							},
						},
						Provenance: provenance,
					},
				},
			},
		}
		applyOfficialPricingSupplement(catalogs, supplement, date)
		return catalogs[ProviderAnthropic].
			models["claude-sonnet-5"].
			Prices[ProfileStandard].
			Price.CacheWrite1h
	}
	if got := priceAt(time.Date(
		2026, time.August, 31, 23, 59, 0, 0, time.UTC,
	)); got != 4 {
		t.Fatalf("August Sonnet 5 price = %v, want 4", got)
	}
	if got := priceAt(time.Date(
		2026, time.September, 1, 0, 0, 0, 0, time.UTC,
	)); got != 6 {
		t.Fatalf("September Sonnet 5 price = %v, want 6", got)
	}
}

func TestCapturedSnapshotReevaluatesOfficialPricingAtWindowRollover(
	t *testing.T,
) {
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		`"rollover"`,
	); err != nil {
		t.Fatal(err)
	}
	captured := p.Capture()
	august := captured.quoteProfileAt(
		ProviderAnthropic,
		"claude-sonnet-5",
		ProfileStandard,
		time.Date(
			2026,
			time.August,
			31,
			23,
			59,
			59,
			0,
			time.UTC,
		),
	)
	september := captured.quoteProfileAt(
		ProviderAnthropic,
		"claude-sonnet-5",
		ProfileStandard,
		time.Date(
			2026,
			time.September,
			1,
			0,
			0,
			0,
			0,
			time.UTC,
		),
	)
	if august.Price.CacheWrite1h != 4 ||
		august.RateProvenance.CacheWrite1h.EffectiveUntil !=
			"2026-08-31" {
		t.Fatalf("August captured quote = %+v", august)
	}
	if september.Price.CacheWrite1h != 6 ||
		september.RateProvenance.CacheWrite1h.EffectiveFrom !=
			"2026-09-01" {
		t.Fatalf("September captured quote = %+v", september)
	}
	if captured != p.Capture() {
		t.Fatal("quote-time rollover published a new snapshot")
	}
}

func TestOfficialPricingSupplementIsExcludedFromProviderCache(t *testing.T) {
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		`"supplement-cache"`,
	); err != nil {
		t.Fatal(err)
	}
	live := p.QuoteProfile(
		ProviderAnthropic,
		"claude-opus-5",
		ProfileStandard,
	)
	if !live.RatePresence.CacheWrite1h ||
		live.Price.CacheWrite1h != 10 {
		t.Fatalf("live supplemented quote = %+v", live)
	}
	body, err := p.ExportProviderCache(ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	var envelope providerCacheEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	cached := envelope.Models["claude-opus-5"].
		Prices[ProfileStandard]
	if cached.RatePresence.CacheWrite1h ||
		cached.Price.CacheWrite1h != 0 ||
		cached.RateProvenance.CacheWrite1h.Source != "" {
		t.Fatalf("supplement leaked into provider cache: %+v", cached)
	}

	restarted := New()
	if err := restarted.ImportProviderCache(body); err != nil {
		t.Fatal(err)
	}
	restored := restarted.QuoteProfile(
		ProviderAnthropic,
		"claude-opus-5",
		ProfileStandard,
	)
	if !restored.RatePresence.CacheWrite1h ||
		restored.Price.CacheWrite1h != 10 ||
		restored.RateProvenance.CacheWrite1h.LoadedFrom !=
			LoadedFromVendoredFallback {
		t.Fatalf("restart did not reapply supplement: %+v", restored)
	}
}

func TestModelsDevExcludesDeprecatedAndUnsupportedFastProfiles(t *testing.T) {
	if !anthropicFastProfileAllowed("claude-opus-4-8") ||
		!anthropicFastProfileAllowed("claude-opus-5") ||
		anthropicFastProfileAllowed("claude-opus-4-7") {
		t.Fatal("Anthropic fast profile allowlist is incorrect")
	}
	deprecatedAndStale := `
	      "claude-opus-4-1": {
	        "id": "claude-opus-4-1",
	        "name": "Claude Opus 4.1",
	        "status": "deprecated",
	        "cost": {"input": 1, "output": 2}
	      },
	      "claude-opus-unknown-status": {
	        "id": "claude-opus-unknown-status",
	        "name": "Unknown Status",
	        "status": "retired",
	        "cost": {"input": 1, "output": 2}
	      },
	      "claude-opus-4-6": {
	        "id": "claude-opus-4-6",
	        "name": "Claude Opus 4.6",
	        "cost": {"input": 5, "output": 25},
	        "experimental": {
	          "modes": {
	            "fast": {
	              "cost": {"input": 10, "output": 50},
	              "provider": {
	                "body": {"speed": "fast"},
	                "headers": {"anthropic-beta": "fast-mode-2026-02-01"}
	              }
	            }
	          }
	        }
	      },
`
	fixture := strings.Replace(
		modelsDevFixture,
		`"models": {`,
		`"models": {`+deprecatedAndStale,
		1,
	)
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(fixture),
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		`"status-fast"`,
	); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		"claude-opus-4-1",
		"claude-opus-unknown-status",
	} {
		if _, ok := p.Capture().Model(ProviderAnthropic, id); ok {
			t.Fatalf("excluded model %q remains", id)
		}
	}
	stale, ok := p.Capture().Model(
		ProviderAnthropic,
		"claude-opus-4-6",
	)
	if !ok {
		t.Fatal("active Opus 4.6 model is missing")
	}
	if _, ok := stale.Profiles[ProfileFast]; ok {
		t.Fatalf("stale fast profile remains: %+v", stale)
	}
	if quote := p.QuoteProfile(
		ProviderAnthropic,
		"claude-opus-4-6",
		ProfileFast,
	); quote.Priced {
		t.Fatalf("stale fast price remains: %+v", quote)
	}
	diagnostics := p.Capture().
		ProviderMetadata(ProviderAnthropic).
		Diagnostics
	want := []string{
		"excluded 1 deprecated model(s)",
		"excluded 1 model(s) with unknown status",
		"ignored 1 unsupported Anthropic fast profile(s)",
	}
	if len(diagnostics) != len(want) {
		t.Fatalf("diagnostics = %#v, want %#v", diagnostics, want)
	}
	for index := range want {
		if diagnostics[index] != want[index] {
			t.Fatalf("diagnostics = %#v, want %#v", diagnostics, want)
		}
	}
}
