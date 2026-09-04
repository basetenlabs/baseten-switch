package pricing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const modelsDevFixture = `{
  "anthropic": {
    "id": "anthropic",
    "name": "Anthropic",
    "models": {
      "claude-opus-5": {
        "id": "claude-opus-5",
        "name": "Claude Opus 5",
        "family": "claude-opus",
        "release_date": "2026-07-24",
        "limit": {"context": 1000000, "output": 128000},
        "cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25},
        "experimental": {
          "modes": {
            "fast": {
              "cost": {"input": 10, "output": 50, "cache_read": 1, "cache_write": 12.5},
              "provider": {
                "body": {"speed": "fast"},
                "headers": {"anthropic-beta": "fast-mode-2026-02-01"}
              }
            }
          }
        }
      },
      "claude-sonnet-5": {
        "id": "claude-sonnet-5",
        "name": "Claude Sonnet 5",
        "family": "claude-sonnet",
        "release_date": "2026-06-29",
        "limit": {"context": 1000000, "output": 128000},
        "cost": {"input": 2, "output": 10, "cache_read": 0.2, "cache_write": 2.5}
      }
    }
  },
  "openai": {
    "id": "openai",
    "models": {
      "gpt-test": {
        "id": "gpt-test",
        "name": "GPT Test",
        "family": "gpt",
        "limit": {"context": 400000, "output": 100000},
        "cost": {"input": 1.25, "output": 10}
      }
    }
  },
  "baseten": {
    "id": "baseten",
    "models": {
      "zai-org/GLM-Test": {
        "id": "zai-org/GLM-Test",
        "name": "GLM Test",
        "family": "glm",
        "reasoning": true,
        "reasoning_options": [{"type": "toggle"}],
        "modalities": {
          "input": ["text", "image", "audio"],
          "output": ["text"]
        },
        "limit": {"context": 200000, "output": 100000},
        "cost": {"input": 0.3, "output": 0.75, "cache_read": 0.06}
      },
      "deepseek-ai/DeepSeek-Test": {
        "id": "deepseek-ai/DeepSeek-Test",
        "name": "DeepSeek Test",
        "reasoning": true,
        "reasoning_options": [
          {"type": "effort", "values": ["low", null, "high", "turbo-next"]},
          {"type": "budget_tokens", "min": -1, "max": 32000}
        ]
      },
      "empty-options/Reasoning-Test": {
        "id": "empty-options/Reasoning-Test",
        "name": "Empty Options Reasoning Test",
        "reasoning": true,
        "reasoning_options": []
      },
      "future/Reasoning-Test": {
        "id": "future/Reasoning-Test",
        "name": "Future Reasoning Test",
        "reasoning": true,
        "reasoning_options": [
          {"type": "future_control", "value": "ignored"},
          {"type": "toggle"}
        ]
      },
      "future/Only-Unknown-Test": {
        "id": "future/Only-Unknown-Test",
        "name": "Only Unknown Reasoning Test",
        "reasoning": true,
        "reasoning_options": [
          {"type": "future_control", "value": "ignored"}
        ]
      },
      "plain/Model-Test": {
        "id": "plain/Model-Test",
        "name": "Plain Model Test",
        "reasoning": false
      }
    }
  }
}`

func TestReplaceModelsDevPublishesProviderScopedProfiles(t *testing.T) {
	capturedAt := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture), capturedAt, `"catalog-etag"`,
	); err != nil {
		t.Fatal(err)
	}

	standard := p.QuoteProfile(
		ProviderAnthropic, "claude-opus-5", ProfileStandard,
	)
	fast := p.QuoteProfile(
		ProviderAnthropic, "claude-opus-5", ProfileFast,
	)
	if !standard.Priced || standard.Price.Prompt != 5 ||
		standard.ExecutionProfile != ProfileStandard {
		t.Fatalf("standard quote = %+v", standard)
	}
	if !fast.Priced || fast.Price.Prompt != 10 ||
		fast.Price.CacheWrite5m != 12.5 ||
		fast.ExecutionProfile != ProfileFast {
		t.Fatalf("fast quote = %+v", fast)
	}
	if got := p.Quote(ProviderAnthropic, "claude-opus-5"); got != standard {
		t.Fatalf("compatibility Quote = %+v, want %+v", got, standard)
	}
	if quote := p.QuoteProfile(
		ProviderAnthropic, "claude-sonnet-5", ProfileFast,
	); quote.Priced {
		t.Fatalf("Sonnet fast quote unexpectedly priced: %+v", quote)
	}
	if quote := p.QuoteProfile(
		ProviderAnthropic, "claude-opus-5-future-variant", ProfileStandard,
	); quote.Priced {
		t.Fatalf("inexact model id inherited a price: %+v", quote)
	}
	openAI := p.QuoteProfile(
		ProviderOpenAI, "gpt-test", ProfileStandard,
	)
	if !openAI.Priced || openAI.Price.Prompt != 1.25 {
		t.Fatalf("OpenAI quote = %+v", openAI)
	}
	baseten := p.QuoteProfile(
		ProviderBaseten, "zai-org/GLM-Test", ProfileStandard,
	)
	if baseten.Priced {
		t.Fatalf("models.dev supplied a Baseten price: %+v", baseten)
	}

	record, ok := p.Capture().Model(ProviderAnthropic, "claude-opus-5")
	if !ok {
		t.Fatal("Opus record missing")
	}
	if record.Family != "opus" || record.ReleaseDate != "2026-07-24" ||
		record.ContextTokens != 1_000_000 ||
		record.MaxOutputTokens != 128_000 {
		t.Fatalf("Opus metadata = %+v", record)
	}
	fastDefinition := record.Profiles[ProfileFast]
	if fastDefinition.RequestBody["speed"] != "fast" ||
		fastDefinition.RequestHeaders["anthropic-beta"] !=
			"fast-mode-2026-02-01" {
		t.Fatalf("fast definition = %+v", fastDefinition)
	}
	metadata := p.Capture().ProviderMetadata(ProviderAnthropic)
	if metadata.ModelCount != 2 || metadata.PricedModelCount != 2 ||
		metadata.Provenance.Source != modelsDevSource ||
		metadata.Provenance.LoadedFrom != LoadedFromLive ||
		metadata.Provenance.ETag != `"catalog-etag"` {
		t.Fatalf("Anthropic metadata = %+v", metadata)
	}
	if quote := p.Quote(ProviderOpenAI, "openai/gpt-test"); quote.Priced {
		t.Fatalf("provider-prefixed alias inherited an exact-ID price: %+v", quote)
	}
}

func TestModelsDevTieredPricingRemainsUnpriced(t *testing.T) {
	fixture := strings.Replace(
		modelsDevFixture,
		`"cost": {"input": 1.25, "output": 10}`,
		`"cost": {
			"input": 1.25,
			"output": 10,
			"tiers": [{
				"input": 2.5,
				"output": 15,
				"tier": {"type": "context", "size": 272000}
			}],
			"context_over_200k": {"input": 2.5, "output": 15}
		}`,
		1,
	)
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(fixture),
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		`"tiered"`,
	); err != nil {
		t.Fatal(err)
	}
	if quote := p.Quote(ProviderOpenAI, "gpt-test"); quote.Priced {
		t.Fatalf("tiered model received incomplete base price: %+v", quote)
	}
	metadata := p.Capture().ProviderMetadata(ProviderOpenAI)
	if metadata.ModelCount != 1 || metadata.PricedModelCount != 0 {
		t.Fatalf("tiered OpenAI metadata = %+v", metadata)
	}
}

func TestModelsDevCannotOverrideBasetenPricingAuthority(t *testing.T) {
	p := New()
	before := p.Quote(ProviderBaseten, "zai-org/GLM-5.2")
	if !before.Priced || before.Source != basetenFallbackSource {
		t.Fatalf("cold-start Baseten quote = %+v", before)
	}
	fixture := strings.ReplaceAll(
		modelsDevFixture,
		"zai-org/GLM-Test",
		"zai-org/GLM-5.2",
	)
	if err := p.ReplaceModelsDev(
		[]byte(fixture),
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		`"models-dev-root"`,
	); err != nil {
		t.Fatal(err)
	}
	after := p.Quote(ProviderBaseten, "zai-org/GLM-5.2")
	if after != before {
		t.Fatalf("models.dev changed Baseten price: before=%+v after=%+v",
			before, after)
	}
}

func TestAuthenticatedBasetenOmissionSuppressesFallbackAcrossCache(t *testing.T) {
	p := New()
	if !p.Quote(ProviderBaseten, "zai-org/GLM-5.2").Priced {
		t.Fatal("test requires embedded Baseten fallback price")
	}
	body := []byte(`{
		"data": [{
			"id": "only/authenticated-model",
			"pricing": {"prompt": 0.000002, "completion": 0.000003}
		}]
	}`)
	if err := p.ReplaceBasetenCatalog(
		body,
		"baseten_v1_models",
		time.Date(2026, time.July, 26, 13, 0, 0, 0, time.UTC),
		"",
	); err != nil {
		t.Fatal(err)
	}
	if quote := p.Quote(
		ProviderBaseten,
		"zai-org/GLM-5.2",
	); quote.Priced {
		t.Fatalf("authenticated omission retained fallback price: %+v", quote)
	}
	if quote := p.Quote(
		ProviderBaseten,
		"only/authenticated-model",
	); !quote.Priced || quote.Source != "baseten_v1_models" {
		t.Fatalf("authenticated model quote = %+v", quote)
	}

	cache, err := p.ExportProviderCache(ProviderBaseten)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New()
	if err := restarted.ImportProviderCache(cache); err != nil {
		t.Fatal(err)
	}
	if quote := restarted.Quote(
		ProviderBaseten,
		"zai-org/GLM-5.2",
	); quote.Priced {
		t.Fatalf("cached authenticated omission restored fallback price: %+v",
			quote)
	}
	if metadata := restarted.Capture().ProviderMetadata(
		ProviderBaseten,
	); metadata.PricedModelCount != 1 {
		t.Fatalf("cached health disagrees with quotes: %+v", metadata)
	}
}

func TestProviderCacheRejectsPreReleaseShapeWithoutPricingMarker(t *testing.T) {
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		`"root"`,
	); err != nil {
		t.Fatal(err)
	}
	cache, err := p.ExportProviderCache(ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(cache, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "replaces_pricing")
	legacyCache, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}

	restarted := New()
	before := restarted.Capture()
	if err := restarted.ImportProviderCache(legacyCache); err == nil ||
		!strings.Contains(err.Error(), "replaces_pricing is required") {
		t.Fatalf("pre-release cache error = %v", err)
	}
	if restarted.Capture() != before {
		t.Fatal("rejected pre-release cache changed the active snapshot")
	}
}

func TestModelsDevReasoningPreservesProviderScopedOptions(t *testing.T) {
	capturedAt := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		capturedAt,
		`"reasoning-etag"`,
	); err != nil {
		t.Fatal(err)
	}

	glm, ok := p.Capture().ModelReasoning(
		ProviderBaseten,
		"zai-org/GLM-Test",
	)
	if !ok || !glm.Supported || len(glm.Options) != 1 ||
		glm.Options[0].Type != ReasoningToggle ||
		glm.Provenance.Source != modelsDevSource ||
		glm.Provenance.LoadedFrom != LoadedFromLive {
		t.Fatalf("GLM reasoning = %+v, found=%t", glm, ok)
	}

	deepseek, ok := p.Capture().ModelReasoning(
		ProviderBaseten,
		"deepseek-ai/DeepSeek-Test",
	)
	if !ok || len(deepseek.Options) != 2 ||
		deepseek.Options[0].Type != ReasoningEffort ||
		deepseek.Options[1].Type != ReasoningBudgetTokens {
		t.Fatalf("DeepSeek reasoning = %+v, found=%t", deepseek, ok)
	}
	values := deepseek.Options[0].Values
	if len(values) != 4 || values[0] == nil || *values[0] != "low" ||
		values[1] != nil || values[2] == nil || *values[2] != "high" ||
		values[3] == nil || *values[3] != "turbo-next" {
		t.Fatalf("ordered nullable efforts = %#v", values)
	}
	if deepseek.Options[1].Min == nil ||
		*deepseek.Options[1].Min != -1 ||
		deepseek.Options[1].Max == nil ||
		*deepseek.Options[1].Max != 32_000 {
		t.Fatalf("budget option = %+v", deepseek.Options[1])
	}

	emptyOptions, ok := p.Capture().ModelReasoning(
		ProviderBaseten,
		"empty-options/Reasoning-Test",
	)
	if !ok || !emptyOptions.Supported || emptyOptions.Options == nil ||
		len(emptyOptions.Options) != 0 {
		t.Fatalf("empty verified controls = %+v, found=%t", emptyOptions, ok)
	}
	future, ok := p.Capture().ModelReasoning(
		ProviderBaseten,
		"future/Reasoning-Test",
	)
	if !ok || len(future.Options) != 1 ||
		future.Options[0].Type != ReasoningToggle {
		t.Fatalf("known controls after future option = %+v, found=%t",
			future, ok)
	}
	onlyUnknown, ok := p.Capture().ModelReasoning(
		ProviderBaseten,
		"future/Only-Unknown-Test",
	)
	if !ok || !onlyUnknown.Supported || onlyUnknown.Options == nil ||
		len(onlyUnknown.Options) != 0 {
		t.Fatalf("all-unknown controls = %+v, found=%t",
			onlyUnknown, ok)
	}
	metadata := p.Capture().ProviderMetadata(ProviderBaseten)
	if len(metadata.Diagnostics) != 1 ||
		metadata.Diagnostics[0] !=
			"ignored 2 unknown reasoning option type(s)" {
		t.Fatalf("reasoning diagnostics = %#v", metadata.Diagnostics)
	}
	plain, ok := p.Capture().ModelReasoning(
		ProviderBaseten,
		"plain/Model-Test",
	)
	if !ok || plain.Supported || plain.Options == nil {
		t.Fatalf("unsupported reasoning = %+v, found=%t", plain, ok)
	}
}

func TestModelsDevModalitiesPreserveExactProviderScopedValues(t *testing.T) {
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		`"modalities-etag"`,
	); err != nil {
		t.Fatal(err)
	}

	input, ok := p.Capture().ModelInputModalities(
		ProviderBaseten,
		"zai-org/GLM-Test",
	)
	if !ok || len(input) != 3 ||
		input[0] != "text" || input[1] != "image" || input[2] != "audio" {
		t.Fatalf("GLM input modalities = %#v, found=%t", input, ok)
	}
	input[0] = "changed"
	again, ok := p.Capture().ModelInputModalities(
		ProviderBaseten,
		"zai-org/GLM-Test",
	)
	if !ok || again[0] != "text" {
		t.Fatalf("caller mutated input modalities: %#v, found=%t", again, ok)
	}
	if _, ok := p.Capture().ModelInputModalities(
		ProviderBaseten,
		"plain/Model-Test",
	); ok {
		t.Fatal("model without modality metadata reported a capability")
	}
	if _, ok := p.Capture().ModelInputModalities(
		ProviderBaseten,
		"zai-org/GLM-Test-Variant",
	); ok {
		t.Fatal("inexact model id inherited modalities")
	}
}

func TestReasoningEffortTokenValidationIsForwardCompatible(t *testing.T) {
	for _, value := range []string{
		"none",
		"xhigh",
		"turbo-next",
		"provider.next",
		"FUTURE_LEVEL",
	} {
		if !validReasoningEffort(value) {
			t.Errorf("valid effort %q was rejected", value)
		}
	}
	for _, value := range []string{
		"",
		" high",
		"high ",
		"high\nlow",
		"high/low",
		strings.Repeat("x", 65),
	} {
		if validReasoningEffort(value) {
			t.Errorf("invalid effort %q was accepted", value)
		}
	}
}

func TestModelsDevMalformedReasoningRetainsLastKnownGood(t *testing.T) {
	p := New()
	capturedAt := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		capturedAt,
		`"good"`,
	); err != nil {
		t.Fatal(err)
	}
	before := p.Capture()
	malformed := strings.Replace(
		modelsDevFixture,
		`"values": ["low", null, "high", "turbo-next"]`,
		`"values": ["low", 1, "high", "turbo-next"]`,
		1,
	)
	if err := p.ReplaceModelsDev(
		[]byte(malformed),
		capturedAt.Add(time.Hour),
		`"bad"`,
	); err == nil {
		t.Fatal("malformed known reasoning option was accepted")
	}
	if p.Capture() != before {
		t.Fatal("malformed reasoning candidate replaced last-known-good")
	}
}

func TestModelsDevRejectsOptionsWhenReasoningIsFalse(t *testing.T) {
	malformed := strings.Replace(
		modelsDevFixture,
		`"reasoning": false`,
		`"reasoning": false, "reasoning_options": []`,
		1,
	)
	if err := New().ReplaceModelsDev(
		[]byte(malformed),
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		"",
	); err == nil {
		t.Fatal("reasoning:false with options was accepted")
	}
}

func TestVendoredBasetenFallbackIsProjectedWithoutNativePrices(t *testing.T) {
	p := New()
	baseten, ok := p.Capture().Model(ProviderBaseten, "zai-org/GLM-5.2")
	if !ok || baseten.Provenance.LoadedFrom != LoadedFromVendoredFallback ||
		baseten.Prices[ProfileStandard].Price.Prompt != 1.4 {
		t.Fatalf("embedded Baseten record = %+v, found=%t", baseten, ok)
	}
	if baseten.Availability.Account != nil {
		t.Fatalf("vendored Baseten price asserted account availability: %+v", baseten)
	}
	if baseten.Reasoning == nil ||
		baseten.Reasoning.Provenance.Source != modelsDevSource ||
		baseten.Reasoning.Provenance.LoadedFrom !=
			LoadedFromVendoredFallback ||
		len(baseten.Reasoning.Options) != 1 ||
		baseten.Reasoning.Options[0].Type != ReasoningToggle {
		t.Fatalf("vendored Baseten reasoning = %+v", baseten.Reasoning)
	}
	for _, provider := range []string{ProviderAnthropic, ProviderOpenAI} {
		if models := p.Capture().Models(provider); len(models) != 0 {
			t.Fatalf("vendored %s records = %+v, want none", provider, models)
		}
	}
}

func TestReplaceProviderAvailabilityPreservesPublicPricingAtomically(t *testing.T) {
	p := New()
	now := time.Date(2026, time.July, 26, 14, 0, 0, 0, time.UTC)
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		now,
		`"models-dev-root"`,
	); err != nil {
		t.Fatal(err)
	}
	if err := p.ReplaceProviderAvailability(
		ProviderAnthropic,
		[]AvailabilityModel{
			{
				CanonicalModelID: "claude-opus-5",
				DisplayName:      "Account Opus",
				ContextTokens:    900_000,
				MaxOutputTokens:  128_000,
			},
		},
		"anthropic_v1_models",
		now.Add(time.Minute),
		"sha256:account-one",
	); err != nil {
		t.Fatal(err)
	}
	record, ok := p.Capture().Model(ProviderAnthropic, "claude-opus-5")
	if !ok || record.Availability.Public == nil ||
		record.Availability.Account == nil {
		t.Fatalf("merged availability = %+v, found=%t", record, ok)
	}
	if record.Availability.Public.Provenance.Source != modelsDevSource ||
		record.Availability.Account.Provenance.Source != "anthropic_v1_models" {
		t.Fatalf("availability provenance = %+v", record.Availability)
	}
	if record.ContextTokens != 900_000 ||
		record.DisplayName != "Account Opus" ||
		record.Family != "opus" {
		t.Fatalf("dimension authority merge = %+v", record)
	}
	if quote := p.QuoteProfile(
		ProviderAnthropic, "claude-opus-5", ProfileFast,
	); !quote.Priced || quote.Price.Prompt != 10 ||
		quote.Source != modelsDevSource {
		t.Fatalf("availability replaced pricing: %+v", quote)
	}
	if err := p.ReplaceProviderAvailability(
		ProviderAnthropic,
		[]AvailabilityModel{
			{CanonicalModelID: "claude-sonnet-5", DisplayName: "Account Sonnet"},
		},
		"anthropic_v1_models",
		now.Add(90*time.Second),
		"sha256:account-two",
	); err != nil {
		t.Fatal(err)
	}
	opus, _ := p.Capture().Model(ProviderAnthropic, "claude-opus-5")
	if opus.Availability.Account != nil ||
		opus.Availability.Public == nil ||
		!p.Quote(ProviderAnthropic, "claude-opus-5").Priced {
		t.Fatalf("fresh account list did not clear stale Opus evidence: %+v", opus)
	}
	sonnet, _ := p.Capture().Model(ProviderAnthropic, "claude-sonnet-5")
	if sonnet.Availability.Account == nil {
		t.Fatalf("fresh account list did not set Sonnet evidence: %+v", sonnet)
	}

	before := p.Capture()
	if err := p.ReplaceProviderAvailability(
		ProviderAnthropic,
		[]AvailabilityModel{
			{CanonicalModelID: "duplicate"},
			{CanonicalModelID: "duplicate"},
		},
		"anthropic_v1_models",
		now.Add(2*time.Minute),
		"sha256:invalid",
	); err == nil {
		t.Fatal("duplicate availability candidate was accepted")
	}
	if p.Capture() != before {
		t.Fatal("invalid availability candidate changed the snapshot")
	}
}

func TestDisplayNameAndPresentationRevisionUseHighestPresentationAuthority(t *testing.T) {
	p := New()
	now := time.Date(2026, time.July, 26, 14, 0, 0, 0, time.UTC)
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		now,
		`"models-dev-root"`,
	); err != nil {
		t.Fatal(err)
	}
	before := p.Capture()
	initialRevision := before.PresentationRevision(ProviderBaseten)
	if name, ok := before.DisplayName(
		ProviderBaseten,
		"zai-org/GLM-Test",
	); !ok || name != "GLM Test" {
		t.Fatalf("models.dev display name = %q, found=%t", name, ok)
	}
	initialQuote := before.Quote(ProviderBaseten, "zai-org/GLM-Test")

	if err := p.ReplaceProviderAvailability(
		ProviderBaseten,
		[]AvailabilityModel{{
			CanonicalModelID: "zai-org/GLM-Test",
			DisplayName:      "Account GLM",
		}},
		"baseten_model_apis",
		now.Add(time.Minute),
		"sha256:model-apis",
	); err != nil {
		t.Fatal(err)
	}
	after := p.Capture()
	if name, ok := after.DisplayName(
		ProviderBaseten,
		"zai-org/GLM-Test",
	); !ok || name != "Account GLM" {
		t.Fatalf("Model APIs display name = %q, found=%t", name, ok)
	}
	if name, ok := after.DisplayName(
		ProviderBaseten,
		"baseten/zai-org/GLM-Test",
	); ok || name != "" {
		t.Fatalf("provider-prefixed alias resolved to %q, found=%t", name, ok)
	}
	if after.PresentationRevision(ProviderBaseten) == initialRevision {
		t.Fatal("display-name change did not change presentation revision")
	}
	if quote := after.Quote(
		ProviderBaseten,
		"zai-org/GLM-Test",
	); quote != initialQuote {
		t.Fatalf("presentation availability changed quote: before=%+v after=%+v", initialQuote, quote)
	}
	reasoning, ok := after.ModelReasoning(
		ProviderBaseten,
		"zai-org/GLM-Test",
	)
	if !ok || len(reasoning.Options) != 1 ||
		reasoning.Options[0].Type != ReasoningToggle ||
		reasoning.Provenance.Source != modelsDevSource {
		t.Fatalf("availability erased reasoning = %+v, found=%t",
			reasoning, ok)
	}
	if name, ok := after.DisplayName(
		ProviderBaseten,
		"missing/model",
	); ok || name != "" {
		t.Fatalf("missing display name = %q, found=%t", name, ok)
	}
	if revision := (*Snapshot)(nil).PresentationRevision(ProviderBaseten); revision != "" {
		t.Fatalf("nil snapshot presentation revision = %q", revision)
	}

	body, err := after.ExportProviderCache(ProviderBaseten)
	if err != nil {
		t.Fatal(err)
	}
	var envelope providerCacheEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Source != "baseten_model_apis" ||
		envelope.Revision != "sha256:model-apis" ||
		envelope.CapturedAt != now.Add(time.Minute) {
		t.Fatalf("mixed cache provenance = %+v", envelope)
	}
	if envelope.ETag != `"models-dev-root"` ||
		envelope.ValidatedAt != now {
		t.Fatalf("mixed cache models.dev validation = %+v", envelope)
	}
	restored := New()
	if err := restored.ImportProviderCache(body); err != nil {
		t.Fatal(err)
	}
	if name, ok := restored.Capture().DisplayName(
		ProviderBaseten,
		"zai-org/GLM-Test",
	); !ok || name != "Account GLM" {
		t.Fatalf("restored display name = %q, found=%t", name, ok)
	}
	restoredMetadata := restored.Capture().ProviderMetadata(
		ProviderBaseten,
	)
	if restoredMetadata.Provenance.Source != "baseten_model_apis" ||
		restoredMetadata.Provenance.Revision != "sha256:model-apis" ||
		restoredMetadata.Provenance.CapturedAt != now.Add(time.Minute) ||
		restoredMetadata.Provenance.ETag != "" {
		t.Fatalf("restored mixed provenance = %+v", restoredMetadata)
	}
	if restored.Capture().ModelsDevETag(ProviderBaseten) !=
		`"models-dev-root"` ||
		restored.Capture().ModelsDevValidatedAt(ProviderBaseten) != now {
		t.Fatalf("restored mixed models.dev metadata = %+v",
			restoredMetadata)
	}
}

func TestNormalizedCatalogReadResultsAreOwnedCopies(t *testing.T) {
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture), time.Now().UTC(), "",
	); err != nil {
		t.Fatal(err)
	}
	record, _ := p.Capture().Model(ProviderAnthropic, "claude-opus-5")
	definition := record.Profiles[ProfileFast]
	definition.RequestBody["speed"] = "changed"
	record.Profiles[ProfileFast] = definition
	record.Prices[ProfileFast] = PriceProfile{}
	deepseek, _ := p.Capture().Model(
		ProviderBaseten,
		"deepseek-ai/DeepSeek-Test",
	)
	*deepseek.Reasoning.Options[0].Values[0] = "changed"
	*deepseek.Reasoning.Options[1].Max = 1
	glm, _ := p.Capture().Model(
		ProviderBaseten,
		"zai-org/GLM-Test",
	)
	glm.Modalities.Input[0] = "changed"
	glm.Modalities.Output[0] = "changed"

	again, _ := p.Capture().Model(ProviderAnthropic, "claude-opus-5")
	if again.Profiles[ProfileFast].RequestBody["speed"] != "fast" ||
		again.Prices[ProfileFast].Price.Prompt != 10 {
		t.Fatalf("caller mutated snapshot: %+v", again)
	}
	models := p.Capture().Models(ProviderAnthropic)
	models[0].DisplayName = "changed"
	again, _ = p.Capture().Model(ProviderAnthropic, models[0].CanonicalModelID)
	if again.DisplayName == "changed" {
		t.Fatal("Models result retained snapshot memory")
	}
	deepseekAgain, _ := p.Capture().Model(
		ProviderBaseten,
		"deepseek-ai/DeepSeek-Test",
	)
	if *deepseekAgain.Reasoning.Options[0].Values[0] != "low" ||
		*deepseekAgain.Reasoning.Options[1].Max != 32_000 {
		t.Fatal("reasoning result retained snapshot memory")
	}
	glmAgain, _ := p.Capture().Model(
		ProviderBaseten,
		"zai-org/GLM-Test",
	)
	if glmAgain.Modalities.Input[0] != "text" ||
		glmAgain.Modalities.Output[0] != "text" {
		t.Fatal("modality result retained snapshot memory")
	}
}

func TestModelsDevMissingRatePresenceSurvivesProviderCache(t *testing.T) {
	capturedAt := time.Date(2026, time.July, 26, 12, 30, 0, 0, time.UTC)
	fixture := strings.Replace(
		modelsDevFixture,
		`"cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25}`,
		`"cost": {"input": 5, "output": 25, "cache_write": 6.25}`,
		1,
	)
	live := New()
	if err := live.ReplaceModelsDev(
		[]byte(fixture),
		capturedAt,
		`"missing-cache-read"`,
	); err != nil {
		t.Fatal(err)
	}
	assertMissingCacheRead := func(t *testing.T, quote Quote) {
		t.Helper()
		if !quote.Priced || !quote.RatePresenceKnown ||
			!quote.RatePresence.Input ||
			!quote.RatePresence.Output ||
			quote.RatePresence.CacheRead ||
			!quote.RatePresence.CacheWrite5m {
			t.Fatalf("quote rate presence = %+v", quote)
		}
		if !quote.HasRatesForUsage(10, 2, 0, 4, 0) {
			t.Fatal("zero cache-read usage required an absent cache-read rate")
		}
		if quote.HasRatesForUsage(10, 2, 1, 4, 0) {
			t.Fatal("nonzero cache-read usage accepted an absent cache-read rate")
		}
	}
	assertMissingCacheRead(t, live.Quote(
		ProviderAnthropic,
		"claude-opus-5",
	))

	cache, err := live.ExportProviderCache(ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	var envelope providerCacheEnvelope
	if err := json.Unmarshal(cache, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 {
		t.Fatalf("provider cache schema_version = %d, want 1",
			envelope.SchemaVersion)
	}
	if envelope.ValidatedAt != capturedAt {
		t.Fatalf("provider cache validated_at = %s, want %s",
			envelope.ValidatedAt, capturedAt)
	}
	restored := New()
	if err := restored.ImportProviderCache(cache); err != nil {
		t.Fatal(err)
	}
	assertMissingCacheRead(t, restored.Quote(
		ProviderAnthropic,
		"claude-opus-5",
	))
}

func TestProviderCacheSchema1RoundTripsReasoningAndRejectsOtherSchemas(
	t *testing.T,
) {
	capturedAt := time.Date(2026, time.July, 26, 15, 0, 0, 0, time.UTC)
	live := New()
	if err := live.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		capturedAt,
		`"reasoning-cache"`,
	); err != nil {
		t.Fatal(err)
	}
	body, err := live.ExportProviderCache(ProviderBaseten)
	if err != nil {
		t.Fatal(err)
	}
	restored := New()
	if err := restored.ImportProviderCache(body); err != nil {
		t.Fatal(err)
	}
	deepseek, ok := restored.Capture().ModelReasoning(
		ProviderBaseten,
		"deepseek-ai/DeepSeek-Test",
	)
	if !ok || deepseek.Provenance.LoadedFrom != LoadedFromRuntimeCache ||
		len(deepseek.Options) != 2 ||
		deepseek.Options[0].Values[1] != nil {
		t.Fatalf("restored reasoning = %+v, found=%t", deepseek, ok)
	}
	modalities, ok := restored.Capture().ModelInputModalities(
		ProviderBaseten,
		"zai-org/GLM-Test",
	)
	if !ok || len(modalities) != 3 || modalities[1] != "image" {
		t.Fatalf("restored modalities = %#v, found=%t", modalities, ok)
	}
	metadata := restored.Capture().ProviderMetadata(ProviderBaseten)
	if metadata.ModelsDevValidatedAt != capturedAt {
		t.Fatalf("restored validated_at = %s, want %s",
			metadata.ModelsDevValidatedAt, capturedAt)
	}

	var unsupported providerCacheEnvelope
	if err := json.Unmarshal(body, &unsupported); err != nil {
		t.Fatal(err)
	}
	unsupported.SchemaVersion = providerCacheSchemaVersion + 1
	unsupportedBody, err := json.Marshal(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	if err := New().ImportProviderCache(unsupportedBody); err == nil {
		t.Fatal("provider cache accepted an unsupported schema generation")
	}

	var invalid providerCacheEnvelope
	if err := json.Unmarshal(body, &invalid); err != nil {
		t.Fatal(err)
	}
	record := invalid.Models["deepseek-ai/DeepSeek-Test"]
	invalidMinimum := int64(-2)
	record.Reasoning.Options[1].Min = &invalidMinimum
	invalid.Models["deepseek-ai/DeepSeek-Test"] = record
	invalid.ContentSHA256, err = cachedModelsSHA256(invalid.Models)
	if err != nil {
		t.Fatal(err)
	}
	invalidBody, err := json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	target := New()
	if err := target.ImportProviderCache(invalidBody); err == nil {
		t.Fatal("schema 1 cache accepted invalid reasoning capability")
	}
}

func TestModelsDevRootETagRequiresConsistentCompleteSlices(t *testing.T) {
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		time.Date(2026, time.July, 26, 15, 0, 0, 0, time.UTC),
		`"shared-root"`,
	); err != nil {
		t.Fatal(err)
	}
	if got := p.Capture().ModelsDevRootETag(); got != `"shared-root"` {
		t.Fatalf("complete root ETag = %q", got)
	}

	p.publishMu.Lock()
	current := p.current.Load()
	layers := cloneProviderLayers(current.providerLayers)
	key := providerLayerKey{
		provider:   ProviderBaseten,
		loadedFrom: LoadedFromLive,
		source:     modelsDevSource,
	}
	catalog := layers[key]
	changed := false
	for id, record := range catalog.models {
		if !changed && record.Availability.Public != nil {
			record.Availability.Public.Provenance.ETag = `"different"`
			catalog.models[id] = record
			changed = true
		}
	}
	layers[key] = catalog
	p.publishLocked(cloneSnapshotWithLayers(current, layers))
	p.publishMu.Unlock()
	if !changed {
		t.Fatal("test fixture had no Baseten public evidence")
	}
	if got := p.Capture().ModelsDevRootETag(); got != "" {
		t.Fatalf("inconsistent provider root ETag = %q, want empty", got)
	}
}

func TestModelsDevInvalidCandidateRetainsLastKnownGood(t *testing.T) {
	p := New()
	capturedAt := time.Now().UTC()
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture), capturedAt, "",
	); err != nil {
		t.Fatal(err)
	}
	before := p.Capture()
	bad := strings.Replace(
		modelsDevFixture,
		`"cost": {"input": 10, "output": 50, "cache_read": 1, "cache_write": 12.5}`,
		`"cost": {"input": 10}`,
		1,
	)
	if err := p.ReplaceModelsDev([]byte(bad), capturedAt, ""); err == nil {
		t.Fatal("partial fast price was accepted")
	}
	if p.Capture() != before {
		t.Fatal("invalid candidate replaced the active snapshot")
	}
}

func TestModelsDevRejectsInvalidReleaseDate(t *testing.T) {
	bad := strings.Replace(
		modelsDevFixture,
		`"release_date": "2026-07-24"`,
		`"release_date": "2026-7-24"`,
		1,
	)
	if err := New().ReplaceModelsDev(
		[]byte(bad), time.Now().UTC(), "",
	); err == nil || !strings.Contains(err.Error(), "release_date") {
		t.Fatalf("invalid release date error = %v", err)
	}
}

func TestModelsDevAcceptsMonthReleaseDate(t *testing.T) {
	fixture := strings.Replace(
		modelsDevFixture,
		`"release_date": "2026-07-24"`,
		`"release_date": "2026-07"`,
		1,
	)
	catalog := New()
	if err := catalog.ReplaceModelsDev(
		[]byte(fixture), time.Now().UTC(), "",
	); err != nil {
		t.Fatal(err)
	}
	record, ok := catalog.Capture().Model(
		ProviderAnthropic,
		"claude-opus-5",
	)
	if !ok || record.ReleaseDate != "2026-07" {
		t.Fatalf("month-precision release date = %q, found=%t", record.ReleaseDate, ok)
	}
}

func TestModelsDevAnthropicFamilyNormalizationAdaptsToNewFamilies(t *testing.T) {
	tests := []struct {
		family string
		id     string
		want   string
	}{
		{family: "claude-opus", id: "claude-opus-5", want: "opus"},
		{family: "claude-newfamily", id: "claude-newfamily-1", want: "newfamily"},
		{id: "claude-newfamily-1", want: "newfamily"},
		{id: "claude-3-unknown-20250101", want: ""},
		{family: "<script>", id: "custom-model", want: ""},
	}
	for _, test := range tests {
		if got := normalizeModelsDevFamily(
			ProviderAnthropic,
			test.family,
			test.id,
		); got != test.want {
			t.Errorf(
				"normalizeModelsDevFamily(%q, %q) = %q, want %q",
				test.family,
				test.id,
				got,
				test.want,
			)
		}
	}
}

func TestModelsDevProfileProjectionRejectsUntrustedRequestFields(t *testing.T) {
	provenance := Provenance{
		Source: modelsDevSource, LoadedFrom: LoadedFromLive,
		Revision: "sha256:test", CapturedAt: time.Now().UTC(),
	}
	definition := safeModelsDevProfileDefinition(
		ProviderAnthropic,
		ProfileFast,
		map[string]any{
			"speed":    "fast",
			"messages": []any{"untrusted"},
		},
		map[string]string{
			"anthropic-beta": "fast-mode-2026-02-01",
			"Authorization":  "secret",
		},
		provenance,
	)
	if definition.Supported ||
		len(definition.RequestBody) != 0 ||
		len(definition.RequestHeaders) != 0 {
		t.Fatalf("unsafe profile projection = %+v", definition)
	}
}

func TestProviderCacheRoundTripAndLivePrecedence(t *testing.T) {
	capturedAt := time.Date(2026, time.July, 26, 13, 0, 0, 0, time.UTC)
	live := New()
	if err := live.ReplaceModelsDev(
		[]byte(modelsDevFixture), capturedAt, `"one"`,
	); err != nil {
		t.Fatal(err)
	}
	if err := live.ReplaceProviderAvailability(
		ProviderAnthropic,
		[]AvailabilityModel{{
			CanonicalModelID: "claude-opus-5",
			DisplayName:      "Account Opus",
			ContextTokens:    900_000,
		}},
		"anthropic_v1_models",
		capturedAt.Add(time.Minute),
		"sha256:account",
	); err != nil {
		t.Fatal(err)
	}
	cache, err := live.ExportProviderCache(ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}

	restored := New()
	if err := restored.ImportProviderCache(cache); err != nil {
		t.Fatal(err)
	}
	quote := restored.QuoteProfile(
		ProviderAnthropic, "claude-opus-5", ProfileFast,
	)
	if !quote.Priced || quote.Price.Prompt != 10 ||
		quote.Source != modelsDevSource {
		t.Fatalf("restored quote = %+v", quote)
	}
	metadata := restored.Capture().ProviderMetadata(ProviderAnthropic)
	if metadata.Provenance.LoadedFrom != LoadedFromRuntimeCache ||
		metadata.Provenance.Source != "anthropic_v1_models" ||
		metadata.Provenance.CapturedAt != capturedAt.Add(time.Minute) {
		t.Fatalf("restored metadata = %+v", metadata)
	}
	if etag := restored.Capture().ModelsDevETag(
		ProviderAnthropic,
	); etag != `"one"` {
		t.Fatalf("restored models.dev ETag = %q", etag)
	}
	record, ok := restored.Capture().Model(
		ProviderAnthropic, "claude-opus-5",
	)
	if !ok || record.Availability.Account == nil ||
		record.Availability.Account.Provenance.LoadedFrom != LoadedFromRuntimeCache ||
		record.Availability.Account.Provenance.Source != "anthropic_v1_models" {
		t.Fatalf("restored account availability = %+v, found=%t", record, ok)
	}
	if record.ContextTokens != 900_000 ||
		record.DisplayName != "Account Opus" ||
		record.ReleaseDate != "2026-07-24" {
		t.Fatalf("restored provider metadata authority = %+v", record)
	}
	if err := restored.ReplaceProviderAvailability(
		ProviderAnthropic,
		[]AvailabilityModel{{CanonicalModelID: "claude-sonnet-5"}},
		"anthropic_v1_models",
		capturedAt.Add(30*time.Minute),
		"sha256:account-two",
	); err != nil {
		t.Fatal(err)
	}
	refreshedCache, err := restored.ExportProviderCache(ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	afterAvailabilityRefresh := New()
	if err := afterAvailabilityRefresh.ImportProviderCache(
		refreshedCache,
	); err != nil {
		t.Fatal(err)
	}
	if quote := afterAvailabilityRefresh.Quote(
		ProviderAnthropic,
		"claude-opus-5",
	); !quote.Priced {
		t.Fatalf("availability-only refresh erased cached pricing: %+v", quote)
	}
	opusAfterRefresh, _ := afterAvailabilityRefresh.Capture().Model(
		ProviderAnthropic,
		"claude-opus-5",
	)
	sonnetAfterRefresh, _ := afterAvailabilityRefresh.Capture().Model(
		ProviderAnthropic,
		"claude-sonnet-5",
	)
	if opusAfterRefresh.Availability.Account != nil ||
		sonnetAfterRefresh.Availability.Account == nil {
		t.Fatalf(
			"availability refresh account evidence: opus=%+v sonnet=%+v",
			opusAfterRefresh.Availability,
			sonnetAfterRefresh.Availability,
		)
	}

	updated := strings.Replace(
		modelsDevFixture,
		`"cost": {"input": 10, "output": 50, "cache_read": 1, "cache_write": 12.5}`,
		`"cost": {"input": 11, "output": 51, "cache_read": 1, "cache_write": 12.5}`,
		1,
	)
	if err := restored.ReplaceModelsDev(
		[]byte(updated), capturedAt.Add(time.Hour), `"two"`,
	); err != nil {
		t.Fatal(err)
	}
	quote = restored.QuoteProfile(
		ProviderAnthropic, "claude-opus-5", ProfileFast,
	)
	if quote.Price.Prompt != 11 {
		t.Fatalf("runtime cache overrode live quote: %+v", quote)
	}
	afterLivePublic, _ := restored.Capture().Model(
		ProviderAnthropic,
		"claude-opus-5",
	)
	if afterLivePublic.ContextTokens != 900_000 ||
		afterLivePublic.DisplayName != "Account Opus" {
		t.Fatalf(
			"live public metadata overrode provider cache: %+v",
			afterLivePublic,
		)
	}
}

func TestReplaceModelsDevRetiresRemovedRuntimeCacheRecords(t *testing.T) {
	oldCapturedAt := time.Date(
		2026,
		time.July,
		26,
		13,
		0,
		0,
		0,
		time.UTC,
	)
	seed := New()
	if err := seed.ReplaceModelsDev(
		[]byte(modelsDevFixture),
		oldCapturedAt,
		`"old-root"`,
	); err != nil {
		t.Fatal(err)
	}
	if err := seed.ReplaceProviderAvailability(
		ProviderBaseten,
		[]AvailabilityModel{{
			CanonicalModelID: "deepseek-ai/DeepSeek-Test",
			DisplayName:      "Account DeepSeek",
			ContextTokens:    900_000,
		}},
		"baseten_model_apis",
		oldCapturedAt.Add(time.Minute),
		"sha256:account",
	); err != nil {
		t.Fatal(err)
	}

	restored := New()
	for _, provider := range []string{
		ProviderAnthropic,
		ProviderOpenAI,
		ProviderBaseten,
	} {
		cache, err := seed.ExportProviderCache(provider)
		if err != nil {
			t.Fatal(err)
		}
		if err := restored.ImportProviderCache(cache); err != nil {
			t.Fatal(err)
		}
	}

	var updated map[string]any
	if err := json.Unmarshal([]byte(modelsDevFixture), &updated); err != nil {
		t.Fatal(err)
	}
	baseten := updated[ProviderBaseten].(map[string]any)
	models := baseten["models"].(map[string]any)
	delete(models, "deepseek-ai/DeepSeek-Test")
	delete(models, "empty-options/Reasoning-Test")
	updatedBody, err := json.Marshal(updated)
	if err != nil {
		t.Fatal(err)
	}
	newCapturedAt := oldCapturedAt.Add(time.Hour)
	if err := restored.ReplaceModelsDev(
		updatedBody,
		newCapturedAt,
		`"new-root"`,
	); err != nil {
		t.Fatal(err)
	}

	if _, ok := restored.Capture().Model(
		ProviderBaseten,
		"empty-options/Reasoning-Test",
	); ok {
		t.Fatal("removed pure models.dev cache record remained active")
	}
	deepseek, ok := restored.Capture().Model(
		ProviderBaseten,
		"deepseek-ai/DeepSeek-Test",
	)
	if !ok {
		t.Fatal("independent account availability was removed")
	}
	if deepseek.DisplayName != "Account DeepSeek" ||
		deepseek.ContextTokens != 900_000 ||
		deepseek.Availability.Account == nil {
		t.Fatalf("preserved account metadata = %+v", deepseek)
	}
	if deepseek.Availability.Public != nil ||
		deepseek.Reasoning != nil ||
		deepseek.Family != "" ||
		len(deepseek.Profiles) != 0 ||
		len(deepseek.Prices) != 0 {
		t.Fatalf("superseded models.dev fields remained active: %+v",
			deepseek)
	}
	if got := restored.Capture().ModelsDevRootETag(); got != `"new-root"` {
		t.Fatalf("root ETag after complete replacement = %q", got)
	}
	if got := restored.Capture().ModelsDevValidatedAt(
		ProviderBaseten,
	); got != newCapturedAt {
		t.Fatalf("Baseten validated_at = %s, want %s",
			got, newCapturedAt)
	}

	afterRestart := New()
	for _, provider := range []string{
		ProviderAnthropic,
		ProviderOpenAI,
		ProviderBaseten,
	} {
		cache, err := restored.ExportProviderCache(provider)
		if err != nil {
			t.Fatal(err)
		}
		if err := afterRestart.ImportProviderCache(cache); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := afterRestart.Capture().ModelReasoning(
		ProviderBaseten,
		"deepseek-ai/DeepSeek-Test",
	); ok {
		t.Fatal("removed reasoning capability survived cache export")
	}
	if _, ok := afterRestart.Capture().Model(
		ProviderBaseten,
		"empty-options/Reasoning-Test",
	); ok {
		t.Fatal("removed model survived cache export")
	}
	if got := afterRestart.Capture().ModelsDevRootETag(); got !=
		`"new-root"` {
		t.Fatalf("restored root ETag = %q", got)
	}
	if got := afterRestart.Capture().ModelsDevValidatedAt(
		ProviderBaseten,
	); got != newCapturedAt {
		t.Fatalf("restored Baseten validated_at = %s, want %s",
			got, newCapturedAt)
	}
}

func TestProviderCacheExportSkipsProviderWithoutLiveData(t *testing.T) {
	body, err := New().ExportProviderCache(ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		t.Fatalf("cache body = %q, want nil", body)
	}
}

func TestProviderCacheWithoutModelsDevDoesNotInventValidation(t *testing.T) {
	p := NewWithPrices(map[string]Price{
		"static/model": {Prompt: 1, Completion: 2},
	})
	body, err := p.ExportProviderCache(ProviderBaseten)
	if err != nil {
		t.Fatal(err)
	}
	var envelope providerCacheEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.ValidatedAt.IsZero() || envelope.ETag != "" {
		t.Fatalf("non-models.dev validation metadata = %+v", envelope)
	}
	restored := New()
	if err := restored.ImportProviderCache(body); err != nil {
		t.Fatal(err)
	}
	if got := restored.Capture().ModelsDevValidatedAt(
		ProviderBaseten,
	); !got.IsZero() {
		t.Fatalf("invented models.dev validation time = %s", got)
	}
}

func TestProviderCacheRejectsTamperingWithoutPublication(t *testing.T) {
	p := New()
	if err := p.ReplaceModelsDev(
		[]byte(modelsDevFixture), time.Now().UTC(), "",
	); err != nil {
		t.Fatal(err)
	}
	cache, err := p.ExportProviderCache(ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(cache, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["content_sha256"] = strings.Repeat("0", 64)
	tampered, _ := json.Marshal(envelope)

	target := New()
	before := target.Capture()
	if err := target.ImportProviderCache(tampered); err == nil {
		t.Fatal("tampered cache was accepted")
	}
	if target.Capture() != before {
		t.Fatal("tampered cache changed the active snapshot")
	}
}
