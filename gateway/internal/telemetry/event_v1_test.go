package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func int64Pointer(value int64) *int64 { return &value }

func TestNewEventID(t *testing.T) {
	first, err := NewEventID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewEventID()
	if err != nil {
		t.Fatal(err)
	}
	if !validEventID(first) {
		t.Fatalf("event id %q is invalid", first)
	}
	if first == second {
		t.Fatalf("event ids unexpectedly match: %q", first)
	}
}

func TestEventV1NullAndZeroSemantics(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	event := validEventV1(now)
	event.UsageComplete = false
	event.Usage = UsageV1{
		InputTokens:  int64Pointer(0),
		OutputTokens: nil,
	}
	event.ActualCost = CostSnapshotV1{
		Priced: false,
		Source: "baseten_embedded_fallback",
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	for _, want := range []string{
		`"input_tokens":0`,
		`"output_tokens":null`,
		`"nano_usd":null`,
		`"native_counterfactual_cost":null`,
		`"requested_context_budget_tokens":null`,
		`"requested_speed":null`,
		`"effective_speed":null`,
		`"stripped_tool_types":[]`,
	} {
		if !strings.Contains(value, want) {
			t.Fatalf("encoded event missing %s: %s", want, value)
		}
	}
	if strings.Contains(value, `"cache_write_input_tokens"`) ||
		strings.Contains(value, `"cache_write_input"`) {
		t.Fatalf("encoded event retained generic cache-write fields: %s", value)
	}
	if strings.Contains(value, `"primary"`) {
		t.Fatalf("event without fallback unexpectedly emitted primary: %s", value)
	}
}

func TestEventV1PrimarySummaryRoundTrip(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	event := validEventV1(now)
	event.EffectiveProvider = "anthropic"
	event.Fallback = FallbackV1{Attempted: true, Count: 1}
	status := 503
	event.Primary = &PrimaryV1{
		Provider:  "baseten",
		Model:     "zai-org/GLM-5.2",
		Attempted: true,
		Outcome:   PrimaryOutcomeHTTPError,
		Status:    &status,
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded EventV1
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Primary == nil ||
		decoded.Primary.Provider != "baseten" ||
		decoded.Primary.Model != "zai-org/GLM-5.2" ||
		!decoded.Primary.Attempted ||
		decoded.Primary.Outcome != PrimaryOutcomeHTTPError ||
		decoded.Primary.Status == nil || *decoded.Primary.Status != 503 {
		t.Fatalf("primary round trip = %+v", decoded.Primary)
	}
}

func TestEventV1RequestClassificationRoundTrip(t *testing.T) {
	now := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	event := validEventV1(now)
	event.EffectiveProvider = "anthropic"
	event.ServedModel = "claude-sonnet-5"
	event.RequestClassification = &RequestClassificationV1{
		Kind:          RequestClassificationKindClaudeAutoPermissionCheck,
		Detector:      RequestClassificationDetectorClaudeAutoV1,
		RoutingAction: RequestClassificationRoutingActionNativeAnthropic,
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "auto mode") ||
		strings.Contains(string(raw), "tool call") {
		t.Fatalf("classification telemetry contains prompt-derived data: %s", raw)
	}
	var decoded EventV1
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RequestClassification == nil ||
		decoded.RequestClassification.Kind != RequestClassificationKindClaudeAutoPermissionCheck ||
		decoded.RequestClassification.Detector != RequestClassificationDetectorClaudeAutoV1 ||
		decoded.RequestClassification.RoutingAction != RequestClassificationRoutingActionNativeAnthropic {
		t.Fatalf("request classification round trip = %+v", decoded.RequestClassification)
	}

	unclassified := validEventV1(now)
	raw, err = json.Marshal(unclassified)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"request_classification"`) {
		t.Fatalf("unclassified event emitted request_classification: %s", raw)
	}
}

func TestRequestClassificationV1Validation(t *testing.T) {
	now := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		value   RequestClassificationV1
		wantErr string
	}{
		{
			name: "unknown kind",
			value: RequestClassificationV1{
				Kind:          "other",
				Detector:      RequestClassificationDetectorClaudeAutoV1,
				RoutingAction: RequestClassificationRoutingActionNativeAnthropic,
			},
			wantErr: "kind",
		},
		{
			name: "unknown detector",
			value: RequestClassificationV1{
				Kind:          RequestClassificationKindClaudeAutoPermissionCheck,
				Detector:      "other",
				RoutingAction: RequestClassificationRoutingActionNativeAnthropic,
			},
			wantErr: "detector",
		},
		{
			name: "unknown routing action",
			value: RequestClassificationV1{
				Kind:          RequestClassificationKindClaudeAutoPermissionCheck,
				Detector:      RequestClassificationDetectorClaudeAutoV1,
				RoutingAction: "other",
			},
			wantErr: "routing_action",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := validEventV1(now)
			event.RequestClassification = &test.value
			err := event.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestRequestClassificationV1RequiresNativeTerminalRouting(t *testing.T) {
	now := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	classification := &RequestClassificationV1{
		Kind:          RequestClassificationKindClaudeAutoPermissionCheck,
		Detector:      RequestClassificationDetectorClaudeAutoV1,
		RoutingAction: RequestClassificationRoutingActionNativeAnthropic,
	}
	tests := []struct {
		name    string
		mutate  func(*EventV1)
		wantErr string
	}{
		{
			name: "non-Anthropic provider",
			mutate: func(event *EventV1) {
				event.EffectiveProvider = "baseten"
			},
			wantErr: "effective_provider=anthropic",
		},
		{
			name: "fallback",
			mutate: func(event *EventV1) {
				event.Fallback = FallbackV1{Attempted: true, Count: 1}
			},
			wantErr: "cannot record fallback",
		},
		{
			name: "native counterfactual",
			mutate: func(event *EventV1) {
				event.NativeCounterfactualCost = &CostSnapshotV1{}
			},
			wantErr: "cannot record native counterfactual",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := validEventV1(now)
			event.EffectiveProvider = "anthropic"
			event.RequestClassification = classification
			test.mutate(&event)
			err := event.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestPrimaryV1Validation(t *testing.T) {
	status := func(value int) *int { return &value }
	tests := []struct {
		name    string
		primary PrimaryV1
		wantErr bool
	}{
		{
			name: "HTTP fallback",
			primary: PrimaryV1{Provider: "baseten", Model: "model", Attempted: true,
				Outcome: PrimaryOutcomeHTTPError, Status: status(503)},
		},
		{
			name: "HTTP fallback accepts rate limit",
			primary: PrimaryV1{Provider: "baseten", Model: "model", Attempted: true,
				Outcome: PrimaryOutcomeHTTPError, Status: status(429)},
		},
		{
			name: "HTTP fallback rejects ineligible client error",
			primary: PrimaryV1{Provider: "baseten", Model: "model", Attempted: true,
				Outcome: PrimaryOutcomeHTTPError, Status: status(400)},
			wantErr: true,
		},
		{
			name: "HTTP fallback rejects success status",
			primary: PrimaryV1{Provider: "baseten", Model: "model", Attempted: true,
				Outcome: PrimaryOutcomeHTTPError, Status: status(200)},
			wantErr: true,
		},
		{
			name: "HTTP fallback requires status",
			primary: PrimaryV1{Provider: "baseten", Model: "model", Attempted: true,
				Outcome: PrimaryOutcomeHTTPError},
			wantErr: true,
		},
		{
			name: "reactive image requires exact rejection status",
			primary: PrimaryV1{Provider: "baseten", Model: "model", Attempted: true,
				Outcome: PrimaryOutcomeImageInputUnsupported, Status: status(400)},
		},
		{
			name: "reactive image rejects success status",
			primary: PrimaryV1{Provider: "baseten", Model: "model", Attempted: true,
				Outcome: PrimaryOutcomeImageInputUnsupported, Status: status(200)},
			wantErr: true,
		},
		{
			name: "attempted image requires status",
			primary: PrimaryV1{Provider: "baseten", Model: "model", Attempted: true,
				Outcome: PrimaryOutcomeImageInputUnsupported},
			wantErr: true,
		},
		{
			name: "preflight image has no status",
			primary: PrimaryV1{Provider: "baseten", Model: "model",
				Outcome: PrimaryOutcomeImageInputUnsupported},
		},
		{
			name: "unattempted status rejected",
			primary: PrimaryV1{Provider: "baseten", Model: "model",
				Outcome: PrimaryOutcomeCooldown, Status: status(503)},
			wantErr: true,
		},
		{
			name: "nonstandard three-digit server error accepted",
			primary: PrimaryV1{Provider: "baseten", Model: "model", Attempted: true,
				Outcome: PrimaryOutcomeHTTPError, Status: status(600)},
		},
		{
			name: "status above three-digit range rejected",
			primary: PrimaryV1{Provider: "baseten", Model: "model", Attempted: true,
				Outcome: PrimaryOutcomeHTTPError, Status: status(1000)},
			wantErr: true,
		},
		{
			name: "transport requires dispatch",
			primary: PrimaryV1{Provider: "baseten", Model: "model",
				Outcome: PrimaryOutcomeTransportError},
			wantErr: true,
		},
		{
			name: "request build is pre-dispatch",
			primary: PrimaryV1{Provider: "baseten", Model: "model",
				Outcome: PrimaryOutcomeRequestBuildError},
		},
		{
			name: "expired TTFT budget is pre-dispatch",
			primary: PrimaryV1{Provider: "baseten", Model: "model",
				Outcome: PrimaryOutcomeTTFTTimeout},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.primary.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestEventV1CacheWriteBucketsPreserveMissingVersusZero(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	missing := validEventV1(now)
	missing.Usage.CacheWrite5mInputTokens = nil
	missing.Usage.CacheWrite1hInputTokens = nil
	missing.Usage.CacheWriteTotalInputTokens = int64Pointer(7)
	missing.ActualCost = CostSnapshotV1{}
	if err := missing.Validate(); err != nil {
		t.Fatalf("unknown cache-write split rejected: %v", err)
	}
	raw, err := json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"cache_write_5m_input_tokens":null`) ||
		!strings.Contains(string(raw), `"cache_write_1h_input_tokens":null`) ||
		!strings.Contains(
			string(raw),
			`"cache_write_total_input_tokens":7`,
		) {
		t.Fatalf("missing cache-write buckets lost: %s", raw)
	}

	absent := validEventV1(now)
	absent.Usage.CacheWrite5mInputTokens = nil
	absent.Usage.CacheWrite1hInputTokens = nil
	if err := absent.Validate(); err == nil ||
		!strings.Contains(err.Error(), "exact or total-only") {
		t.Fatalf("absent cache-write usage validation = %v", err)
	}

	partial := validEventV1(now)
	partial.Usage.CacheWrite1hInputTokens = nil
	if err := partial.Validate(); err == nil ||
		!strings.Contains(err.Error(), "both be values or both be null") {
		t.Fatalf("partial cache-write split validation = %v", err)
	}

	exclusive := validEventV1(now)
	exclusive.Usage.CacheWriteTotalInputTokens = int64Pointer(0)
	if err := exclusive.Validate(); err == nil ||
		!strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("cache-write total/bucket exclusivity validation = %v", err)
	}
}

func TestEventV1ReasoningBooleanJSONStatesRoundTrip(t *testing.T) {
	base, err := json.Marshal(validEventV1(
		time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC),
	))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		value     json.RawMessage
		wantState JSONFieldStateV1
		wantValue *bool
		wantJSON  string
	}{
		{
			name:      "missing",
			wantState: JSONFieldMissingV1,
		},
		{
			name:      "null",
			value:     json.RawMessage("null"),
			wantState: JSONFieldNullV1,
			wantJSON:  `"requested_reasoning_present":null`,
		},
		{
			name:      "false",
			value:     json.RawMessage("false"),
			wantState: JSONFieldValueV1,
			wantValue: func() *bool { value := false; return &value }(),
			wantJSON:  `"requested_reasoning_present":false`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := make(map[string]json.RawMessage, len(fields)+1)
			for key, value := range fields {
				row[key] = value
			}
			if tc.value == nil {
				delete(row, "requested_reasoning_present")
			} else {
				row["requested_reasoning_present"] = tc.value
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			var event EventV1
			if err := json.Unmarshal(encoded, &event); err != nil {
				t.Fatal(err)
			}
			if got := event.RequestedReasoningPresentState(); got != tc.wantState {
				t.Fatalf("state = %v, want %v", got, tc.wantState)
			}
			if tc.wantValue == nil {
				if event.RequestedReasoningPresent != nil {
					t.Fatalf(
						"value = %v, want nil",
						*event.RequestedReasoningPresent,
					)
				}
			} else if event.RequestedReasoningPresent == nil ||
				*event.RequestedReasoningPresent != *tc.wantValue {
				t.Fatalf(
					"value = %v, want %v",
					event.RequestedReasoningPresent,
					*tc.wantValue,
				)
			}
			roundTrip, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantJSON == "" {
				if strings.Contains(
					string(roundTrip),
					`"requested_reasoning_present"`,
				) {
					t.Fatalf("missing field was emitted: %s", roundTrip)
				}
			} else if !strings.Contains(string(roundTrip), tc.wantJSON) {
				t.Fatalf("round trip missing %s: %s", tc.wantJSON, roundTrip)
			}
		})
	}

	fields["effective_reasoning_enabled"] = json.RawMessage("null")
	withEffectiveNull, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var effective EventV1
	if err := json.Unmarshal(withEffectiveNull, &effective); err != nil {
		t.Fatal(err)
	}
	if got := effective.EffectiveReasoningEnabledState(); got != JSONFieldNullV1 {
		t.Fatalf("effective state = %v, want explicit null", got)
	}
	roundTrip, err := json.Marshal(effective)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		string(roundTrip),
		`"effective_reasoning_enabled":null`,
	) {
		t.Fatalf("effective null was not preserved: %s", roundTrip)
	}
}

func TestEventV1PricedCostsRequireRatesForUsedDimensions(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	event := validEventV1(now)
	one := int64(1)
	event.Usage.InputTokens = &one
	if err := event.Validate(); err == nil ||
		!strings.Contains(err.Error(), "actual_cost") {
		t.Fatalf("actual cost missing used rate error = %v", err)
	}

	rate := int64(0)
	event.ActualCost.RatesNanoUSDPerToken.Input = &rate
	event.ActualCost.RateProvenance = map[string]RateProvenanceV1{
		"input": validRateProvenanceV1(now),
	}
	counterfactual := event.ActualCost
	counterfactual.RatesNanoUSDPerToken = &TokenRatesV1{}
	event.NativeCounterfactualCost = &counterfactual
	if err := event.Validate(); err == nil ||
		!strings.Contains(err.Error(), "native_counterfactual_cost") {
		t.Fatalf("counterfactual missing used rate error = %v", err)
	}
}

func TestCostSnapshotV1RequiresExactValidRateProvenance(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	valid := validEventV1(now)
	rate := int64(1)
	valid.ActualCost.RatesNanoUSDPerToken.Input = &rate
	valid.ActualCost.RateProvenance = map[string]RateProvenanceV1{
		"input": validRateProvenanceV1(now),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid rate provenance rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*EventV1)
		wantErr string
	}{
		{
			name: "missing",
			mutate: func(event *EventV1) {
				delete(event.ActualCost.RateProvenance, "input")
			},
			wantErr: "provenance count",
		},
		{
			name: "extra",
			mutate: func(event *EventV1) {
				event.ActualCost.RateProvenance["output"] =
					validRateProvenanceV1(now)
			},
			wantErr: "provenance count",
		},
		{
			name: "unknown dimension",
			mutate: func(event *EventV1) {
				delete(event.ActualCost.RateProvenance, "input")
				event.ActualCost.RateProvenance["unknown"] =
					validRateProvenanceV1(now)
			},
			wantErr: "unknown dimension",
		},
		{
			name: "missing source",
			mutate: func(event *EventV1) {
				value := event.ActualCost.RateProvenance["input"]
				value.Source = ""
				event.ActualCost.RateProvenance["input"] = value
			},
			wantErr: "requires source",
		},
		{
			name: "missing revision",
			mutate: func(event *EventV1) {
				value := event.ActualCost.RateProvenance["input"]
				value.Revision = ""
				event.ActualCost.RateProvenance["input"] = value
			},
			wantErr: "requires revision",
		},
		{
			name: "invalid loaded from",
			mutate: func(event *EventV1) {
				value := event.ActualCost.RateProvenance["input"]
				value.LoadedFrom = "other"
				event.ActualCost.RateProvenance["input"] = value
			},
			wantErr: "loaded_from",
		},
		{
			name: "missing captured at",
			mutate: func(event *EventV1) {
				value := event.ActualCost.RateProvenance["input"]
				value.CapturedAt = time.Time{}
				event.ActualCost.RateProvenance["input"] = value
			},
			wantErr: "requires captured_at",
		},
		{
			name: "malformed effective from",
			mutate: func(event *EventV1) {
				value := event.ActualCost.RateProvenance["input"]
				value.EffectiveFrom = "2026/01/01"
				event.ActualCost.RateProvenance["input"] = value
			},
			wantErr: "effective_from",
		},
		{
			name: "malformed effective until",
			mutate: func(event *EventV1) {
				value := event.ActualCost.RateProvenance["input"]
				value.EffectiveUntil = "tomorrow"
				event.ActualCost.RateProvenance["input"] = value
			},
			wantErr: "effective_until",
		},
		{
			name: "inverted effective window",
			mutate: func(event *EventV1) {
				value := event.ActualCost.RateProvenance["input"]
				value.EffectiveFrom = "2026-02-01"
				value.EffectiveUntil = "2026-01-01"
				event.ActualCost.RateProvenance["input"] = value
			},
			wantErr: "window is inverted",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := valid
			event.ActualCost.RateProvenance =
				cloneRateProvenanceV1(valid.ActualCost.RateProvenance)
			test.mutate(&event)
			err := event.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestEventV1Validation(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		mutate  func(*EventV1)
		wantErr string
	}{
		{
			name: "future schema",
			mutate: func(event *EventV1) {
				event.SchemaVersion = 2
			},
			wantErr: "schema_version",
		},
		{
			name: "bad id",
			mutate: func(event *EventV1) {
				event.EventID = "ABC"
			},
			wantErr: "event_id",
		},
		{
			name: "completion before start",
			mutate: func(event *EventV1) {
				event.CompletedAt = event.StartedAt.Add(-time.Second)
			},
			wantErr: "precedes",
		},
		{
			name: "complete usage missing token",
			mutate: func(event *EventV1) {
				event.Usage.OutputTokens = nil
			},
			wantErr: "input, output, and cache-read token fields",
		},
		{
			name: "priced cost missing revision",
			mutate: func(event *EventV1) {
				event.ActualCost.Revision = nil
			},
			wantErr: "requires revision",
		},
		{
			name: "unpriced cost has amount",
			mutate: func(event *EventV1) {
				event.ActualCost = CostSnapshotV1{
					Priced:  false,
					NanoUSD: int64Pointer(0),
				}
			},
			wantErr: "must use null",
		},
		{
			name: "fallback count without attempt",
			mutate: func(event *EventV1) {
				event.Fallback.Count = 1
			},
			wantErr: "requires attempted",
		},
		{
			name: "zero context budget",
			mutate: func(event *EventV1) {
				event.RequestedContextBudgetTokens = int64Pointer(0)
			},
			wantErr: "requested_context_budget_tokens",
		},
		{
			name: "negative context budget",
			mutate: func(event *EventV1) {
				event.RequestedContextBudgetTokens = int64Pointer(-1)
			},
			wantErr: "requested_context_budget_tokens",
		},
		{
			name: "invalid requested speed",
			mutate: func(event *EventV1) {
				value := "turbo"
				event.RequestedSpeed = &value
			},
			wantErr: "requested_speed",
		},
		{
			name: "invalid effective speed",
			mutate: func(event *EventV1) {
				value := ""
				event.EffectiveSpeed = &value
			},
			wantErr: "effective_speed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := validEventV1(now)
			test.mutate(&event)
			err := event.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestEventV1VariantFieldsAndMissingFieldCompatibility(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	event := validEventV1(now)
	contextBudget := int64(1_000_000)
	requestedSpeed := "fast"
	effectiveSpeed := "standard"
	event.RequestedContextBudgetTokens = &contextBudget
	event.RequestedSpeed = &requestedSpeed
	event.EffectiveSpeed = &effectiveSpeed
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"requested_context_budget_tokens",
		"requested_speed",
		"effective_speed",
	} {
		delete(object, key)
	}
	partialEncoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var partial EventV1
	if err := json.Unmarshal(partialEncoded, &partial); err != nil {
		t.Fatal(err)
	}
	if partial.RequestedContextBudgetTokens != nil ||
		partial.RequestedSpeed != nil ||
		partial.EffectiveSpeed != nil {
		t.Fatalf("missing variant fields did not decode as null: %+v", partial)
	}
	if err := partial.Validate(); err != nil {
		t.Fatalf("telemetry v1 row with omitted optional fields became invalid: %v", err)
	}
}

func TestEventV1ResponsesCompatibility(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	event := validEventV1(now)

	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"responses_compatibility"`) {
		t.Fatalf("nil compatibility summary should be omitted: %s", encoded)
	}

	event.ResponsesCompatibility = &ResponsesCompatibilityV1{
		Considered: []string{
			ResponsesCompatibilityRuleTextFormatDefault,
			ResponsesCompatibilityRuleReasoningEffort,
		},
		Applied:          []string{ResponsesCompatibilityRuleTextFormatDefault},
		Forced:           []string{},
		RepairedEvents:   2,
		ValidationErrors: 1,
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"responses_compatibility":`,
		`"considered":["responses.text_format_default","responses.reasoning_effort"]`,
		`"applied":["responses.text_format_default"]`,
		`"forced":[]`,
		`"repaired_events":2`,
		`"validation_errors":1`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("encoded event missing %s: %s", want, encoded)
		}
	}
}

func TestResponsesCompatibilityV1Validation(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		value   ResponsesCompatibilityV1
		wantErr string
	}{
		{
			name:    "negative repaired events",
			value:   ResponsesCompatibilityV1{RepairedEvents: -1},
			wantErr: "repaired_events",
		},
		{
			name:    "negative validation errors",
			value:   ResponsesCompatibilityV1{ValidationErrors: -1},
			wantErr: "validation_errors",
		},
		{
			name:    "empty rule id",
			value:   ResponsesCompatibilityV1{Applied: []string{""}},
			wantErr: "empty rule ID",
		},
		{
			name: "duplicate rule id",
			value: ResponsesCompatibilityV1{
				Forced: []string{
					ResponsesCompatibilityRuleTextFormatDefault,
					ResponsesCompatibilityRuleTextFormatDefault,
				},
			},
			wantErr: "duplicate rule ID",
		},
		{
			name: "unknown rule id",
			value: ResponsesCompatibilityV1{
				Considered: []string{"responses.renamed_rule"},
			},
			wantErr: "unknown rule ID",
		},
		{
			name: "applied rule was not considered",
			value: ResponsesCompatibilityV1{
				Applied: []string{ResponsesCompatibilityRuleTextFormatDefault},
			},
			wantErr: "was not considered",
		},
		{
			name: "forced rule was not applied",
			value: ResponsesCompatibilityV1{
				Considered: []string{ResponsesCompatibilityRuleToolTypeDenylist},
				Forced:     []string{ResponsesCompatibilityRuleToolTypeDenylist},
			},
			wantErr: "was not applied",
		},
		{
			name: "too many rule ids",
			value: ResponsesCompatibilityV1{
				Considered: []string{
					"01", "02", "03", "04", "05", "06", "07", "08", "09",
					"10", "11", "12", "13", "14", "15", "16", "17",
				},
			},
			wantErr: "exceeds 16",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := validEventV1(now)
			event.ResponsesCompatibility = &test.value
			err := event.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestResponsesCompatibilityRuleIDsStable(t *testing.T) {
	ids := []string{
		ResponsesCompatibilityRuleTextFormatDefault,
		ResponsesCompatibilityRuleAdditionalToolsInput,
		ResponsesCompatibilityRuleReasoningEffort,
		ResponsesCompatibilityRuleFunctionArgumentsConsistency,
		ResponsesCompatibilityRuleToolTypeDenylist,
		ResponsesCompatibilityRuleCompletionUsage,
	}
	want := []string{
		"responses.text_format_default",
		"responses.additional_tools_input",
		"responses.reasoning_effort",
		"responses.function_arguments_consistency",
		"responses.tool_type_denylist",
		"responses.completion_usage",
	}
	for index := range want {
		if ids[index] != want[index] {
			t.Errorf("rule ID %d = %q, want %q", index, ids[index], want[index])
		}
		if !validResponsesCompatibilityRuleID(ids[index]) {
			t.Errorf("stable rule ID %q is not accepted", ids[index])
		}
	}

	summary := &ResponsesCompatibilityV1{
		Considered: append([]string(nil), ids...),
		Applied: []string{
			ResponsesCompatibilityRuleTextFormatDefault,
			ResponsesCompatibilityRuleToolTypeDenylist,
		},
		Forced: []string{ResponsesCompatibilityRuleToolTypeDenylist},
	}
	if err := summary.validate(); err != nil {
		t.Fatalf("valid stable rule ID sets rejected: %v", err)
	}
}

func validEventV1(now time.Time) EventV1 {
	revision := "sha256:fixture"
	nanoUSD := int64(0)
	rates := &TokenRatesV1{}
	return EventV1{
		SchemaVersion:        SchemaVersionV1,
		Event:                EventRequest,
		EventID:              "00112233445566778899aabbccddeeff",
		StartedAt:            now,
		CompletedAt:          now.Add(time.Second),
		Client:               "claude-code",
		ConfiguredRoute:      "baseten",
		EffectiveProvider:    "baseten",
		RequestedModel:       "claude-fable-5",
		RequestedModelFamily: "fable",
		ModelFamilyRevision:  "claude-family-v1",
		ServedModel:          "zai-org/GLM-5.2",
		Status:               func() *int { value := 200; return &value }(),
		IsStream:             true,
		DurationMS:           1000,
		TTFTMS:               int64Pointer(100),
		TerminationReason:    TerminationCompleted,
		UsageComplete:        true,
		Usage: UsageV1{
			InputTokens:             int64Pointer(0),
			OutputTokens:            int64Pointer(0),
			CacheReadInputTokens:    int64Pointer(0),
			CacheWrite5mInputTokens: int64Pointer(0),
			CacheWrite1hInputTokens: int64Pointer(0),
		},
		ActualCost: CostSnapshotV1{
			Priced:               true,
			NanoUSD:              &nanoUSD,
			Source:               "baseten_embedded_fallback",
			Revision:             &revision,
			CapturedAt:           &now,
			RatesNanoUSDPerToken: rates,
		},
		Fallback:          FallbackV1{},
		StrippedToolTypes: []string{},
	}
}

func validRateProvenanceV1(capturedAt time.Time) RateProvenanceV1 {
	return RateProvenanceV1{
		Source:     "test",
		LoadedFrom: "live",
		Revision:   "test-revision",
		CapturedAt: capturedAt,
	}
}

func cloneRateProvenanceV1(
	source map[string]RateProvenanceV1,
) map[string]RateProvenanceV1 {
	out := make(map[string]RateProvenanceV1, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
