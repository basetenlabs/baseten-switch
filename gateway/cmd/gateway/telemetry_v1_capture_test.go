package gateway

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/reasoning"
	"github.com/basetenlabs/baseten-switch/gateway/internal/requestprofile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/usage"
)

const telemetryVariantPricingFixture = `{
  "anthropic": {
    "id": "anthropic",
    "models": {
      "claude-opus-5": {
        "id": "claude-opus-5",
        "family": "claude-opus",
        "cost": {
          "input": 5,
          "output": 25,
          "cache_read": 0.5,
          "cache_write": 6.25
        },
        "experimental": {
          "modes": {
            "fast": {
              "cost": {
                "input": 10,
                "output": 50,
                "cache_read": 1,
                "cache_write": 12.5
              },
              "provider": {
                "body": {"speed": "fast"},
                "headers": {"anthropic-beta": "fast-mode-2026-02-01"}
              }
            }
          }
        }
      },
      "claude-codename-5": {
        "id": "claude-codename-5",
        "family": "claude-opus",
        "cost": {"input": 5, "output": 25}
      }
    }
  },
  "openai": {
    "id": "openai",
    "models": {
      "gpt-test": {
        "id": "gpt-test",
        "family": "gpt",
        "cost": {"input": 1, "output": 2, "cache_read": 0.1}
      }
    }
  },
  "baseten": {
    "id": "baseten",
    "models": {
      "zai-org/GLM-5.2": {
        "id": "zai-org/GLM-5.2",
        "family": "glm",
        "cost": {
          "input": 1.4,
          "output": 2.5,
          "cache_read": 0.1,
          "cache_write": 1.8
        }
      }
    }
  }
}`

func telemetryVariantPricing(t *testing.T, capturedAt time.Time) *pricing.Pricing {
	t.Helper()
	prices := pricing.New()
	if err := prices.ReplaceModelsDev(
		[]byte(telemetryVariantPricingFixture),
		capturedAt,
		`"telemetry-variant-fixture"`,
	); err != nil {
		t.Fatal(err)
	}
	replaceTelemetryBasetenPricing(t, prices, capturedAt, 1.4)
	return prices
}

func replaceTelemetryBasetenPricing(
	t *testing.T,
	prices *pricing.Pricing,
	capturedAt time.Time,
	input float64,
) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"data": []map[string]any{{
			"id": "zai-org/GLM-5.2",
			"pricing": map[string]any{
				"prompt":            input / 1e6,
				"completion":        2.5 / 1e6,
				"input_cache_read":  0.1 / 1e6,
				"input_cache_write": 1.8 / 1e6,
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prices.ReplaceBasetenCatalog(
		body,
		"baseten_v1_models",
		capturedAt,
		"zai-org/GLM-5.2",
	); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryV1CaptureRetainsRequestAndAttemptPrices(t *testing.T) {
	startedAt := time.Date(2026, time.July, 25, 20, 0, 0, 0, time.UTC)
	prices := telemetryVariantPricing(t, startedAt)
	profile := requestprofile.Inspect(
		[]byte(`{"model":"claude-opus-5[1m]","speed":"fast"}`),
	)
	request, err := captureTelemetryRequestProfileV1(
		prices.Capture(),
		startedAt,
		"claude-code",
		"baseten",
		"anthropic",
		"claude-opus-5[1m]",
		profile,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.eventID = "00112233445566778899aabbccddeeff"
	attempt := captureTelemetryAttemptV1(
		prices.Capture(),
		startedAt.Add(time.Millisecond),
		"baseten",
		"zai-org/GLM-5.2",
	)

	// A live replacement after both captures must not affect this event.
	replaceTelemetryBasetenPricing(
		t,
		prices,
		startedAt.Add(time.Second),
		9,
	)

	firstOutputAt := startedAt.Add(240 * time.Millisecond)
	status := 200
	providerModel := "zai-org/GLM-5.2"
	stopReason := "end_turn"
	trigger := "http_500"
	subagentModel := "claude-baseten-glm-5-2"
	reportedSpeed := "fast"
	compatibility := &telemetry.ResponsesCompatibilityV1{
		Considered:       []string{"responses.additional_tools_input"},
		Applied:          []string{"responses.additional_tools_input"},
		Forced:           []string{},
		RepairedEvents:   2,
		ValidationErrors: 0,
	}
	event, err := request.event(attempt, telemetryCompletionV1{
		completedAt:            startedAt.Add(2 * time.Second),
		providerReportedModel:  &providerModel,
		status:                 &status,
		isStream:               true,
		firstOutputAt:          &firstOutputAt,
		responseComplete:       true,
		usageComplete:          true,
		usage:                  completeTelemetryUsage(10, 2, 3, 4),
		effectiveSpeed:         &reportedSpeed,
		providerStopReason:     &stopReason,
		fallbackCount:          1,
		fallbackTrigger:        &trigger,
		subagent:               true,
		subagentModel:          &subagentModel,
		strippedToolTypes:      []string{"web_search"},
		responsesCompatibility: compatibility,
		toolCalls:              2,
		requestBytes:           500,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !event.ActualCost.Priced || event.ActualCost.NanoUSD == nil ||
		*event.ActualCost.NanoUSD != 26_500 {
		t.Fatalf("actual cost = %+v, want 26500 nano-USD", event.ActualCost)
	}
	if event.NativeCounterfactualCost == nil ||
		!event.NativeCounterfactualCost.Priced ||
		event.NativeCounterfactualCost.NanoUSD == nil ||
		*event.NativeCounterfactualCost.NanoUSD != 253_000 {
		t.Fatalf("native counterfactual = %+v, want 253000 nano-USD",
			event.NativeCounterfactualCost)
	}
	if got := prices.Quote("baseten", "zai-org/GLM-5.2").Price.Prompt; got != 9 {
		t.Fatalf("test did not replace live price: prompt = %v", got)
	}
	if event.TTFTMS == nil || *event.TTFTMS != 240 {
		t.Fatalf("ttft_ms = %v, want 240", event.TTFTMS)
	}
	if event.RequestedModel != "claude-opus-5[1m]" ||
		request.canonicalModel != "claude-opus-5" ||
		event.RequestedModelFamily != "opus" ||
		event.EffectiveProvider != "baseten" ||
		event.ServedModel != "zai-org/GLM-5.2" {
		t.Fatalf("model attribution = %+v", event)
	}
	if event.RequestedContextBudgetTokens == nil ||
		*event.RequestedContextBudgetTokens != requestprofile.OneMillionContextTokens ||
		event.RequestedSpeed == nil ||
		*event.RequestedSpeed != "fast" ||
		event.EffectiveSpeed != nil {
		t.Fatalf("variant attribution = %+v", event)
	}
	if !event.Fallback.Attempted || event.Fallback.Count != 1 ||
		event.Fallback.Trigger == nil || *event.Fallback.Trigger != trigger {
		t.Fatalf("fallback = %+v", event.Fallback)
	}

	// The event owns its slices and pointers rather than retaining completion
	// memory that a caller could mutate after submission.
	providerModel = "changed"
	trigger = "changed"
	subagentModel = "changed"
	compatibility.Considered[0] = "changed"
	compatibility.Applied[0] = "changed"
	if *event.ProviderReportedModel != "zai-org/GLM-5.2" ||
		*event.Fallback.Trigger != "http_500" ||
		*event.SubagentModel != "claude-baseten-glm-5-2" ||
		event.ResponsesCompatibility == nil ||
		event.ResponsesCompatibility.Considered[0] != "responses.additional_tools_input" ||
		event.ResponsesCompatibility.Applied[0] != "responses.additional_tools_input" {
		t.Fatalf("event retained mutable completion pointers: %+v", event)
	}
}

func TestTelemetryV1AnthropicActualPriceRequiresConfirmedFastSpeed(t *testing.T) {
	startedAt := time.Date(2026, time.July, 25, 20, 0, 0, 0, time.UTC)
	prices := telemetryVariantPricing(t, startedAt)
	profile := requestprofile.Inspect(
		[]byte(`{"model":"claude-opus-5","speed":"fast"}`),
	)
	request, err := captureTelemetryRequestProfileV1(
		prices.Capture(),
		startedAt,
		"claude-code",
		"anthropic",
		"anthropic",
		"claude-opus-5",
		profile,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.eventID = "00112233445566778899aabbccddeeff"
	attempt := captureTelemetryAttemptV1(
		prices.Capture(),
		startedAt,
		"anthropic",
		"claude-opus-5",
	)
	fast := "fast"
	standard := "standard"
	tests := []struct {
		name           string
		effectiveSpeed *string
		wantPriced     bool
		wantNanoUSD    int64
		wantSpeed      *string
	}{
		{
			name:           "confirmed fast uses fast price",
			effectiveSpeed: &fast,
			wantPriced:     true,
			wantNanoUSD:    253_000,
			wantSpeed:      &fast,
		},
		{
			name:       "missing effective speed is unpriced",
			wantPriced: false,
		},
		{
			name:           "provider downgrade uses standard price",
			effectiveSpeed: &standard,
			wantPriced:     true,
			wantNanoUSD:    126_500,
			wantSpeed:      &standard,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, eventErr := request.event(attempt, telemetryCompletionV1{
				completedAt:      startedAt.Add(time.Second),
				status:           intPointer(200),
				responseComplete: true,
				usageComplete:    true,
				usage:            completeTelemetryUsage(10, 2, 3, 4),
				effectiveSpeed:   test.effectiveSpeed,
			})
			if eventErr != nil {
				t.Fatal(eventErr)
			}
			if event.ActualCost.Priced != test.wantPriced {
				t.Fatalf("actual cost = %+v, want priced=%t",
					event.ActualCost, test.wantPriced)
			}
			if test.wantPriced {
				if event.ActualCost.NanoUSD == nil ||
					*event.ActualCost.NanoUSD != test.wantNanoUSD {
					t.Fatalf("actual cost = %+v, want %d nano-USD",
						event.ActualCost, test.wantNanoUSD)
				}
			} else if event.ActualCost.NanoUSD != nil {
				t.Fatalf("unpriced actual cost has amount: %+v", event.ActualCost)
			}
			if !equalOptionalString(event.EffectiveSpeed, test.wantSpeed) {
				t.Fatalf("effective speed = %v, want %v",
					event.EffectiveSpeed, test.wantSpeed)
			}
		})
	}
}

func TestTelemetryV1UnsupportedPricingModifiersRemainUnpriced(t *testing.T) {
	startedAt := time.Date(2026, time.July, 25, 20, 0, 0, 0, time.UTC)
	prices := telemetryVariantPricing(t, startedAt)
	tests := []struct {
		name                     string
		body                     string
		effectiveProvider        string
		servedModel              string
		effectiveSpeed           *string
		actualPricingUnsupported bool
		wantActualPriced         bool
		wantNativePriced         bool
	}{
		{
			name:              "unknown requested speed",
			body:              `{"model":"claude-opus-5","speed":"turbo"}`,
			effectiveProvider: pricing.ProviderAnthropic,
			servedModel:       "claude-opus-5",
		},
		{
			name:              "non-string requested speed",
			body:              `{"model":"claude-opus-5","speed":true}`,
			effectiveProvider: pricing.ProviderAnthropic,
			servedModel:       "claude-opus-5",
		},
		{
			name:              "unknown requested speed keeps Baseten actual priced",
			body:              `{"model":"claude-opus-5","speed":"turbo"}`,
			effectiveProvider: pricing.ProviderBaseten,
			servedModel:       "zai-org/GLM-5.2",
			wantActualPriced:  true,
		},
		{
			name:              "unknown effective speed",
			body:              `{"model":"claude-opus-5"}`,
			effectiveProvider: pricing.ProviderAnthropic,
			servedModel:       "claude-opus-5",
			effectiveSpeed:    stringPointer("turbo"),
		},
		{
			name: "one-hour native counterfactual",
			body: `{
				"model":"claude-opus-5",
				"messages":[{"content":[{
					"cache_control":{"type":"ephemeral","ttl":"1h"}
				}]}]
			}`,
			effectiveProvider: pricing.ProviderBaseten,
			servedModel:       "zai-org/GLM-5.2",
			wantActualPriced:  true,
			wantNativePriced:  true,
		},
		{
			name: "one-hour native actual without usage breakdown",
			body: `{
				"model":"claude-opus-5",
				"messages":[{"content":[{
					"cache_control":{"type":"ephemeral","ttl":"1h"}
				}]}]
			}`,
			effectiveProvider: pricing.ProviderAnthropic,
			servedModel:       "claude-opus-5",
			wantActualPriced:  true,
		},
		{
			name:                     "observed one-hour actual usage",
			body:                     `{"model":"claude-opus-5"}`,
			effectiveProvider:        pricing.ProviderAnthropic,
			servedModel:              "claude-opus-5",
			actualPricingUnsupported: true,
		},
		{
			name:              "US inference geo unprices native actual",
			body:              `{"model":"claude-opus-5","inference_geo":"us"}`,
			effectiveProvider: pricing.ProviderAnthropic,
			servedModel:       "claude-opus-5",
		},
		{
			name:              "US inference geo keeps Baseten actual priced",
			body:              `{"model":"claude-opus-5","inference_geo":"us"}`,
			effectiveProvider: pricing.ProviderBaseten,
			servedModel:       "zai-org/GLM-5.2",
			wantActualPriced:  true,
		},
		{
			name:              "global inference geo uses global rate",
			body:              `{"model":"claude-opus-5","inference_geo":"global"}`,
			effectiveProvider: pricing.ProviderAnthropic,
			servedModel:       "claude-opus-5",
			wantActualPriced:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := requestprofile.Inspect([]byte(test.body))
			request, err := captureTelemetryRequestProfileV1(
				prices.Capture(),
				startedAt,
				"claude-code",
				test.effectiveProvider,
				pricing.ProviderAnthropic,
				profile.RawModel,
				profile,
			)
			if err != nil {
				t.Fatal(err)
			}
			request.eventID = "00112233445566778899aabbccddeeff"
			attempt := captureTelemetryAttemptV1(
				prices.Capture(),
				startedAt,
				test.effectiveProvider,
				test.servedModel,
			)
			status := http.StatusOK
			event, err := request.event(attempt, telemetryCompletionV1{
				completedAt:              startedAt.Add(time.Second),
				status:                   &status,
				responseComplete:         true,
				usageComplete:            true,
				usage:                    completeTelemetryUsage(10, 2, 3, 4),
				effectiveSpeed:           test.effectiveSpeed,
				actualPricingUnsupported: test.actualPricingUnsupported,
			})
			if err != nil {
				t.Fatal(err)
			}
			if event.ActualCost.Priced != test.wantActualPriced {
				t.Fatalf("actual cost = %+v", event.ActualCost)
			}
			if !event.ActualCost.Priced &&
				(event.ActualCost.NanoUSD != nil ||
					event.ActualCost.RatesNanoUSDPerToken != nil) {
				t.Fatalf("unpriced actual cost retained money: %+v", event.ActualCost)
			}
			if event.RequestedSpeed != nil || event.EffectiveSpeed != nil {
				t.Fatalf(
					"unsupported speeds must serialize as null: requested=%v effective=%v",
					event.RequestedSpeed,
					event.EffectiveSpeed,
				)
			}
			if test.effectiveProvider == pricing.ProviderBaseten {
				if event.NativeCounterfactualCost == nil ||
					event.NativeCounterfactualCost.Priced !=
						test.wantNativePriced {
					t.Fatalf(
						"native counterfactual = %+v",
						event.NativeCounterfactualCost,
					)
				}
				if !event.NativeCounterfactualCost.Priced &&
					(event.NativeCounterfactualCost.NanoUSD != nil ||
						event.NativeCounterfactualCost.RatesNanoUSDPerToken != nil) {
					t.Fatalf(
						"unpriced native counterfactual retained money: %+v",
						event.NativeCounterfactualCost,
					)
				}
			}
		})
	}
}

func TestTelemetryV1FamilyUsesCapturedCatalogBeforeParserFallback(t *testing.T) {
	startedAt := time.Date(2026, time.July, 25, 20, 0, 0, 0, time.UTC)
	prices := telemetryVariantPricing(t, startedAt)
	request, err := captureTelemetryRequestProfileV1(
		prices.Capture(),
		startedAt,
		"claude-code",
		"anthropic",
		"anthropic",
		"claude-codename-5",
		requestprofile.Inspect([]byte(`{"model":"claude-codename-5"}`)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.requestedModel != "claude-codename-5" ||
		request.canonicalModel != "claude-codename-5" ||
		request.requestedModelFamily != "opus" {
		t.Fatalf("request capture = %+v", request)
	}
}

func TestTelemetryV1RequestedReasoningNullability(t *testing.T) {
	unobserved := telemetryRequestCaptureV1{}
	if unobserved.requestedReasoningPresent() != nil {
		t.Fatal("unobserved reasoning presence must remain nil")
	}

	observedAbsent := telemetryRequestCaptureV1{
		requestedReasoningObserved: true,
	}
	if got := observedAbsent.requestedReasoningPresent(); got == nil || *got {
		t.Fatalf("observed absent reasoning = %v, want false pointer", got)
	}

	observedPresent := telemetryRequestCaptureV1{
		requestedReasoningObserved: true,
		requestedReasoning: reasoning.RequestedReasoning{
			Present: true,
		},
	}
	if got := observedPresent.requestedReasoningPresent(); got == nil || !*got {
		t.Fatalf("observed present reasoning = %v, want true pointer", got)
	}
}

func TestTelemetryV1CaptureTerminationSemantics(t *testing.T) {
	statusOK := 200
	statusError := 429
	tests := []struct {
		name       string
		completion telemetryCompletionV1
		want       telemetry.TerminationReason
	}{
		{
			name: "completed",
			completion: telemetryCompletionV1{
				status: &statusOK, responseComplete: true,
			},
			want: telemetry.TerminationCompleted,
		},
		{
			name: "client cancelled",
			completion: telemetryCompletionV1{
				status: &statusOK, contextErr: context.Canceled,
			},
			want: telemetry.TerminationClientCancelled,
		},
		{
			name: "upstream http error",
			completion: telemetryCompletionV1{
				status: &statusError, responseComplete: true,
			},
			want: telemetry.TerminationUpstreamHTTPError,
		},
		{
			name: "upstream transport error",
			completion: telemetryCompletionV1{
				contextErr: context.DeadlineExceeded,
			},
			want: telemetry.TerminationUpstreamTransportError,
		},
		{
			name: "incomplete stream",
			completion: telemetryCompletionV1{
				status: &statusOK, isStream: true, responseComplete: false,
			},
			want: telemetry.TerminationIncompleteStream,
		},
		{
			name: "gateway error",
			completion: telemetryCompletionV1{
				gatewayFailure: true,
			},
			want: telemetry.TerminationGatewayError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyTelemetryTermination(test.completion); got != test.want {
				t.Fatalf("termination = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTelemetryV1CapturePreservesUnknownAndReportedZeroUsage(t *testing.T) {
	startedAt := time.Date(2026, time.July, 25, 20, 0, 0, 0, time.UTC)
	prices := pricing.NewWithPrices(map[string]pricing.Price{
		"model": {Prompt: 1},
	})
	request, err := captureTelemetryRequestV1(
		prices.Capture(), startedAt, "claude-code", "baseten", "anthropic", "unknown",
	)
	if err != nil {
		t.Fatal(err)
	}
	request.eventID = "00112233445566778899aabbccddeeff"
	attempt := captureTelemetryAttemptV1(
		prices.Capture(), startedAt, "baseten", "model",
	)
	status := 200
	zero := int64(0)
	partial, err := request.event(attempt, telemetryCompletionV1{
		completedAt:      startedAt.Add(time.Second),
		status:           &status,
		isStream:         true,
		responseComplete: false,
		usageComplete:    false,
		usage: telemetry.UsageV1{
			InputTokens: &zero,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if partial.Usage.InputTokens == nil || *partial.Usage.InputTokens != 0 {
		t.Fatalf("reported zero input was lost: %+v", partial.Usage)
	}
	if partial.Usage.OutputTokens != nil ||
		partial.Usage.CacheReadInputTokens != nil ||
		partial.Usage.CacheWrite5mInputTokens != nil ||
		partial.Usage.CacheWrite1hInputTokens != nil {
		t.Fatalf("unknown usage became zero: %+v", partial.Usage)
	}
	if partial.ActualCost.Priced || partial.ActualCost.NanoUSD != nil {
		t.Fatalf("incomplete usage was priced: %+v", partial.ActualCost)
	}

	complete, err := request.event(attempt, telemetryCompletionV1{
		completedAt:      startedAt.Add(time.Second),
		status:           &status,
		responseComplete: true,
		usageComplete:    true,
		usage:            completeTelemetryUsage(0, 0, 0, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !complete.ActualCost.Priced || complete.ActualCost.NanoUSD == nil ||
		*complete.ActualCost.NanoUSD != 0 {
		t.Fatalf("priced zero is not explicit: %+v", complete.ActualCost)
	}
}

func TestTelemetryV1MissingRateOnlyUnpricesUsedDimension(t *testing.T) {
	capturedAt := time.Date(2026, time.July, 25, 20, 30, 0, 0, time.UTC)
	prices := pricing.New()
	if err := prices.ReplaceModelsDev(
		[]byte(telemetryVariantPricingFixture),
		capturedAt,
		"",
	); err != nil {
		t.Fatal(err)
	}
	quote := prices.Quote(
		pricing.ProviderAnthropic,
		"claude-codename-5",
	)
	if !quote.Priced || !quote.RatePresenceKnown ||
		quote.RatePresence.CacheRead ||
		quote.RatePresence.CacheWrite5m ||
		quote.RatePresence.CacheWrite1h {
		t.Fatalf("models.dev quote = %+v", quote)
	}

	zeroCache := costSnapshotV1(
		quote,
		capturedAt,
		completeTelemetryUsage(10, 2, 0, 0),
		true,
		false,
	)
	if !zeroCache.Priced ||
		zeroCache.NanoUSD == nil ||
		*zeroCache.NanoUSD != 100_000 {
		t.Fatalf("zero-cache cost = %+v, want priced 100000 nano-USD", zeroCache)
	}
	rates := zeroCache.RatesNanoUSDPerToken
	if rates == nil ||
		rates.Input == nil ||
		rates.Output == nil ||
		rates.CacheReadInput != nil ||
		rates.CacheWrite5mInput != nil ||
		rates.CacheWrite1hInput != nil {
		t.Fatalf("zero-cache cost rate presence = %+v", zeroCache)
	}

	nonzeroCache := costSnapshotV1(
		quote,
		capturedAt,
		completeTelemetryUsage(10, 2, 1, 0),
		true,
		false,
	)
	if nonzeroCache.Priced || nonzeroCache.NanoUSD != nil {
		t.Fatalf("missing used cache-read rate became priced: %+v", nonzeroCache)
	}
}

func TestTelemetryV1MixedTTLCacheWriteCosts(t *testing.T) {
	capturedAt := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	prices := telemetryVariantPricing(t, capturedAt)
	observed := completeTelemetryUsageBuckets(10, 2, 3, 4, 5)

	anthropic := costSnapshotV1(
		prices.Quote(pricing.ProviderAnthropic, "claude-opus-5"),
		capturedAt,
		observed,
		true,
		false,
	)
	if !anthropic.Priced || anthropic.NanoUSD == nil ||
		*anthropic.NanoUSD != 176_500 {
		t.Fatalf("mixed Anthropic TTL cost = %+v, want 176500 nano-USD", anthropic)
	}
	if anthropic.RatesNanoUSDPerToken == nil ||
		anthropic.RatesNanoUSDPerToken.CacheWrite5mInput == nil ||
		*anthropic.RatesNanoUSDPerToken.CacheWrite5mInput != 6_250 ||
		anthropic.RatesNanoUSDPerToken.CacheWrite1hInput == nil ||
		*anthropic.RatesNanoUSDPerToken.CacheWrite1hInput != 10_000 {
		t.Fatalf("mixed Anthropic TTL rates = %+v", anthropic)
	}
	if provenance, ok := anthropic.RateProvenance[string(
		pricing.RateCacheWrite1h,
	)]; !ok || provenance.Source == "" || provenance.Revision == "" {
		t.Fatalf("one-hour rate provenance = %+v", anthropic.RateProvenance)
	}
	missingOneHour := prices.Quote(
		pricing.ProviderAnthropic,
		"claude-opus-5",
	)
	missingOneHour.Price.CacheWrite1h = 0
	missingOneHour.RatePresence.CacheWrite1h = false
	missingOneHour.RateProvenance.CacheWrite1h = pricing.Provenance{}
	if got := costSnapshotV1(
		missingOneHour,
		capturedAt,
		observed,
		true,
		false,
	); got.Priced || got.NanoUSD != nil {
		t.Fatalf("missing used one-hour rate became priced: %+v", got)
	}

	baseten := costSnapshotV1(
		prices.Quote(pricing.ProviderBaseten, "zai-org/GLM-5.2"),
		capturedAt,
		observed,
		true,
		true,
	)
	if !baseten.Priced || baseten.NanoUSD == nil ||
		*baseten.NanoUSD != 35_500 {
		t.Fatalf("combined Baseten cache-write cost = %+v, want 35500 nano-USD", baseten)
	}
	if baseten.RatesNanoUSDPerToken == nil ||
		baseten.RatesNanoUSDPerToken.CacheWrite5mInput == nil ||
		baseten.RatesNanoUSDPerToken.CacheWrite1hInput == nil ||
		*baseten.RatesNanoUSDPerToken.CacheWrite5mInput !=
			*baseten.RatesNanoUSDPerToken.CacheWrite1hInput {
		t.Fatalf("Baseten combined cache-write rates = %+v", baseten)
	}
}

func TestObservedGatewayUsageV1CacheBucketInference(t *testing.T) {
	exact := observedGatewayUsageV1(usage.Usage{
		InputTokens:                           1,
		OutputTokens:                          2,
		CacheReadInputTokens:                  3,
		CacheCreationInputTokens:              9,
		CacheCreationFiveMinuteInputTokens:    4,
		CacheCreationOneHourInputTokens:       5,
		CacheCreationTokenBreakdownComplete:   true,
		CacheCreationFiveMinuteTokensObserved: true,
		CacheCreationOneHourTokensObserved:    true,
	}, true, true)
	if exact.CacheWrite5mInputTokens == nil ||
		*exact.CacheWrite5mInputTokens != 4 ||
		exact.CacheWrite1hInputTokens == nil ||
		*exact.CacheWrite1hInputTokens != 5 {
		t.Fatalf("exact cache buckets = %+v", exact)
	}

	inferred5m := observedGatewayUsageV1(usage.Usage{
		CacheCreationInputTokens: 7,
	}, true, false)
	if inferred5m.CacheWrite5mInputTokens == nil ||
		*inferred5m.CacheWrite5mInputTokens != 7 ||
		inferred5m.CacheWrite1hInputTokens == nil ||
		*inferred5m.CacheWrite1hInputTokens != 0 {
		t.Fatalf("five-minute-only inference = %+v", inferred5m)
	}

	unknown := observedGatewayUsageV1(usage.Usage{
		CacheCreationInputTokens: 7,
	}, true, true)
	if unknown.CacheWrite5mInputTokens != nil ||
		unknown.CacheWrite1hInputTokens != nil ||
		unknown.CacheWriteTotalInputTokens == nil ||
		*unknown.CacheWriteTotalInputTokens != 7 {
		t.Fatalf("unknown one-hour split = %+v", unknown)
	}

	reportedZero := observedGatewayUsageV1(usage.Usage{}, true, true)
	if reportedZero.CacheWrite5mInputTokens == nil ||
		reportedZero.CacheWrite1hInputTokens == nil ||
		*reportedZero.CacheWrite5mInputTokens != 0 ||
		*reportedZero.CacheWrite1hInputTokens != 0 {
		t.Fatalf("zero cache buckets = %+v", reportedZero)
	}
}

func TestUnknownOneHourSplitKeepsBasetenActualButUnpricesNativeCounterfactual(
	t *testing.T,
) {
	startedAt := time.Date(2026, time.July, 26, 13, 0, 0, 0, time.UTC)
	prices := telemetryVariantPricing(t, startedAt)
	profile := requestprofile.Inspect([]byte(`{
		"model":"claude-opus-5",
		"messages":[{"content":[{
			"cache_control":{"type":"ephemeral","ttl":"1h"}
		}]}]
	}`))
	request, err := captureTelemetryRequestProfileV1(
		prices.Capture(),
		startedAt,
		"claude-code",
		pricing.ProviderBaseten,
		pricing.ProviderAnthropic,
		profile.RawModel,
		profile,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.eventID = "00112233445566778899aabbccddeeff"
	attempt := captureTelemetryAttemptV1(
		prices.Capture(),
		startedAt,
		pricing.ProviderBaseten,
		"zai-org/GLM-5.2",
	)
	status := http.StatusOK
	event, err := request.event(attempt, telemetryCompletionV1{
		completedAt:      startedAt.Add(time.Second),
		status:           &status,
		responseComplete: true,
		usageComplete:    true,
		usage: observedGatewayUsageV1(usage.Usage{
			InputTokens:              10,
			OutputTokens:             2,
			CacheReadInputTokens:     3,
			CacheCreationInputTokens: 9,
		}, true, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !event.ActualCost.Priced || event.ActualCost.NanoUSD == nil {
		t.Fatalf("Baseten actual cost = %+v", event.ActualCost)
	}
	if event.NativeCounterfactualCost == nil ||
		event.NativeCounterfactualCost.Priced ||
		event.NativeCounterfactualCost.NanoUSD != nil {
		t.Fatalf(
			"unknown native TTL split counterfactual = %+v",
			event.NativeCounterfactualCost,
		)
	}
	if event.Usage.CacheWrite5mInputTokens != nil ||
		event.Usage.CacheWrite1hInputTokens != nil {
		t.Fatalf("unknown cache split became zero: %+v", event.Usage)
	}
}

func TestTelemetryV1TTFTIsFirstOutputNotResponseHeaders(t *testing.T) {
	startedAt := time.Date(2026, time.July, 25, 20, 0, 0, 0, time.UTC)
	headerAt := startedAt.Add(20 * time.Millisecond)
	firstOutputAt := startedAt.Add(320 * time.Millisecond)

	// The header timestamp is deliberately not part of the capture API.
	_ = headerAt
	got := outputLatencyMS(startedAt, &firstOutputAt)
	if got == nil || *got != 320 {
		t.Fatalf("ttft_ms = %v, want first output latency 320", got)
	}
	if got := outputLatencyMS(startedAt, nil); got != nil {
		t.Fatalf("missing output produced ttft_ms %v", *got)
	}
	beforeStart := startedAt.Add(-time.Millisecond)
	if got := outputLatencyMS(startedAt, &beforeStart); got != nil {
		t.Fatalf("pre-request output produced ttft_ms %v", *got)
	}
}

func TestTelemetryV1CostOverflowBecomesUnpriced(t *testing.T) {
	usage := completeTelemetryUsage(math.MaxInt64, 0, 0, 0)
	revision := "fixture"
	quote := pricing.Quote{
		Price: pricing.Price{
			Prompt: float64(math.MaxInt64),
		},
		Priced:   true,
		Source:   "fixture",
		Revision: revision,
	}
	cost := costSnapshotV1(quote, time.Now(), usage, true, false)
	if cost.Priced || cost.NanoUSD != nil {
		t.Fatalf("overflow became priced: %+v", cost)
	}
}

func TestTelemetryV1CaptureJSONNulls(t *testing.T) {
	startedAt := time.Date(2026, time.July, 25, 20, 0, 0, 0, time.UTC)
	request, err := captureTelemetryRequestV1(
		nil, startedAt, "claude-code", "baseten", "anthropic", "unknown",
	)
	if err != nil {
		t.Fatal(err)
	}
	request.eventID = "00112233445566778899aabbccddeeff"
	attempt := captureTelemetryAttemptV1(nil, startedAt, "baseten", "unknown")
	event, err := request.event(attempt, telemetryCompletionV1{
		completedAt: startedAt.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["status"] != nil || decoded["ttft_ms"] != nil {
		t.Fatalf("status/ttft null semantics lost: %s", raw)
	}
	usageObject := decoded["usage"].(map[string]any)
	for _, key := range []string{
		"input_tokens",
		"output_tokens",
		"cache_read_input_tokens",
		"cache_write_5m_input_tokens",
		"cache_write_1h_input_tokens",
	} {
		if usageObject[key] != nil {
			t.Fatalf("usage.%s = %v, want null: %s", key, usageObject[key], raw)
		}
	}
}

func completeTelemetryUsage(input, output, cacheRead, cacheWrite int64) telemetry.UsageV1 {
	return completeTelemetryUsageBuckets(input, output, cacheRead, cacheWrite, 0)
}

func completeTelemetryUsageBuckets(
	input, output, cacheRead, cacheWrite5m, cacheWrite1h int64,
) telemetry.UsageV1 {
	return telemetry.UsageV1{
		InputTokens:             &input,
		OutputTokens:            &output,
		CacheReadInputTokens:    &cacheRead,
		CacheWrite5mInputTokens: &cacheWrite5m,
		CacheWrite1hInputTokens: &cacheWrite1h,
	}
}

func intPointer(value int) *int {
	return &value
}

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
