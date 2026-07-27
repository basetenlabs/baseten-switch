package telemetry

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	SchemaVersionV1 = 1
	EventRequest    = "request"
)

type TerminationReason string

// JSONFieldStateV1 preserves the distinction between a missing optional JSON
// field, an explicit null, and a concrete value when telemetry rows are read.
type JSONFieldStateV1 uint8

const (
	JSONFieldMissingV1 JSONFieldStateV1 = iota
	JSONFieldNullV1
	JSONFieldValueV1
)

const (
	TerminationCompleted              TerminationReason = "completed"
	TerminationClientCancelled        TerminationReason = "client_cancelled"
	TerminationUpstreamHTTPError      TerminationReason = "upstream_http_error"
	TerminationUpstreamTransportError TerminationReason = "upstream_transport_error"
	TerminationIncompleteStream       TerminationReason = "incomplete_stream"
	TerminationGatewayError           TerminationReason = "gateway_error"
)

type EventV1 struct {
	SchemaVersion                 int                       `json:"schema_version"`
	Event                         string                    `json:"event"`
	EventID                       string                    `json:"event_id"`
	StartedAt                     time.Time                 `json:"started_at"`
	CompletedAt                   time.Time                 `json:"completed_at"`
	Client                        string                    `json:"client"`
	ConfiguredRoute               string                    `json:"configured_route"`
	EffectiveProvider             string                    `json:"effective_provider"`
	RequestedModel                string                    `json:"requested_model"`
	RequestedModelFamily          string                    `json:"requested_model_family"`
	RequestedContextBudgetTokens  *int64                    `json:"requested_context_budget_tokens"`
	RequestedSpeed                *string                   `json:"requested_speed"`
	RequestedReasoningPresent     *bool                     `json:"requested_reasoning_present,omitempty"`
	RequestedReasoningEffort      *string                   `json:"requested_reasoning_effort,omitempty"`
	ReasoningPolicyMode           *string                   `json:"reasoning_policy_mode,omitempty"`
	EffectiveReasoningEnabled     *bool                     `json:"effective_reasoning_enabled,omitempty"`
	EffectiveReasoningEffort      *string                   `json:"effective_reasoning_effort,omitempty"`
	ReasoningPolicySource         *string                   `json:"reasoning_policy_source,omitempty"`
	ReasoningCatalogRevision      *string                   `json:"reasoning_catalog_revision,omitempty"`
	ModelFamilyRevision           string                    `json:"model_family_revision"`
	ServedModel                   string                    `json:"served_model"`
	ProviderReportedModel         *string                   `json:"provider_reported_model"`
	EffectiveSpeed                *string                   `json:"effective_speed"`
	Status                        *int                      `json:"status"`
	IsStream                      bool                      `json:"is_stream"`
	DurationMS                    int64                     `json:"duration_ms"`
	TTFTMS                        *int64                    `json:"ttft_ms"`
	TerminationReason             TerminationReason         `json:"termination_reason"`
	ProviderStopReason            *string                   `json:"provider_stop_reason"`
	UsageComplete                 bool                      `json:"usage_complete"`
	Usage                         UsageV1                   `json:"usage"`
	ActualCost                    CostSnapshotV1            `json:"actual_cost"`
	NativeCounterfactualCost      *CostSnapshotV1           `json:"native_counterfactual_cost"`
	Fallback                      FallbackV1                `json:"fallback"`
	Subagent                      bool                      `json:"subagent"`
	SubagentModel                 *string                   `json:"subagent_model"`
	Sanitized                     bool                      `json:"sanitized"`
	Translated                    bool                      `json:"translated"`
	StrippedToolTypes             []string                  `json:"stripped_tool_types"`
	ResponsesCompatibility        *ResponsesCompatibilityV1 `json:"responses_compatibility,omitempty"`
	ToolCalls                     int                       `json:"tool_calls"`
	RequestBytes                  int                       `json:"request_bytes"`
	requestedReasoningPresentNull bool
	effectiveReasoningEnabledNull bool
}

// RequestedReasoningPresentState reports whether the serialized telemetry
// field was missing, explicitly null, or carried a boolean value.
func (e EventV1) RequestedReasoningPresentState() JSONFieldStateV1 {
	if e.RequestedReasoningPresent != nil {
		return JSONFieldValueV1
	}
	if e.requestedReasoningPresentNull {
		return JSONFieldNullV1
	}
	return JSONFieldMissingV1
}

// EffectiveReasoningEnabledState reports whether the serialized telemetry
// field was missing, explicitly null, or carried a boolean value.
func (e EventV1) EffectiveReasoningEnabledState() JSONFieldStateV1 {
	if e.EffectiveReasoningEnabled != nil {
		return JSONFieldValueV1
	}
	if e.effectiveReasoningEnabledNull {
		return JSONFieldNullV1
	}
	return JSONFieldMissingV1
}

// MarshalJSON retains explicit nulls observed while reading telemetry. Events
// created by the gateway continue to omit unknown or inapplicable fields.
func (e EventV1) MarshalJSON() ([]byte, error) {
	type eventV1Alias EventV1
	encoded, err := json.Marshal(eventV1Alias(e))
	if err != nil {
		return nil, err
	}
	if !e.requestedReasoningPresentNull &&
		!e.effectiveReasoningEnabledNull {
		return encoded, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	if e.requestedReasoningPresentNull {
		fields["requested_reasoning_present"] = json.RawMessage("null")
	}
	if e.effectiveReasoningEnabledNull {
		fields["effective_reasoning_enabled"] = json.RawMessage("null")
	}
	return json.Marshal(fields)
}

// UnmarshalJSON records explicit nulls separately from absent optional
// reasoning fields while retaining the existing pointer API for values.
func (e *EventV1) UnmarshalJSON(data []byte) error {
	type eventV1Alias EventV1
	var decoded eventV1Alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*e = EventV1(decoded)
	e.requestedReasoningPresentNull = explicitJSONNull(
		fields["requested_reasoning_present"],
	)
	e.effectiveReasoningEnabledNull = explicitJSONNull(
		fields["effective_reasoning_enabled"],
	)
	return nil
}

func explicitJSONNull(value json.RawMessage) bool {
	return value != nil && bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

// ResponsesCompatibilityV1 summarizes bounded, content-free compatibility
// work performed inside one logical Responses request. Rule IDs are stable
// schema values. Bodies, prompts, tool arguments, and rejection text never
// belong in this object.
type ResponsesCompatibilityV1 struct {
	Considered       []string `json:"considered"`
	Applied          []string `json:"applied"`
	Forced           []string `json:"forced"`
	RepairedEvents   int      `json:"repaired_events"`
	ValidationErrors int      `json:"validation_errors"`
}

const (
	ResponsesCompatibilityRuleTextFormatDefault            = "responses.text_format_default"
	ResponsesCompatibilityRuleAdditionalToolsInput         = "responses.additional_tools_input"
	ResponsesCompatibilityRuleReasoningEffort              = "responses.reasoning_effort"
	ResponsesCompatibilityRuleFunctionArgumentsConsistency = "responses.function_arguments_consistency"
	ResponsesCompatibilityRuleToolTypeDenylist             = "responses.tool_type_denylist"
	ResponsesCompatibilityRuleCompletionUsage              = "responses.completion_usage"
)

type UsageV1 struct {
	InputTokens             *int64 `json:"input_tokens"`
	OutputTokens            *int64 `json:"output_tokens"`
	CacheReadInputTokens    *int64 `json:"cache_read_input_tokens"`
	CacheWrite5mInputTokens *int64 `json:"cache_write_5m_input_tokens"`
	CacheWrite1hInputTokens *int64 `json:"cache_write_1h_input_tokens"`
	// CacheWriteTotalInputTokens is present only when the provider reported an
	// aggregate cache-write count whose TTL split is unknown.
	CacheWriteTotalInputTokens *int64 `json:"cache_write_total_input_tokens"`
}

type TokenRatesV1 struct {
	Input             *int64 `json:"input"`
	Output            *int64 `json:"output"`
	CacheReadInput    *int64 `json:"cache_read_input"`
	CacheWrite5mInput *int64 `json:"cache_write_5m_input"`
	CacheWrite1hInput *int64 `json:"cache_write_1h_input"`
}

type RateProvenanceV1 struct {
	Source         string    `json:"source"`
	LoadedFrom     string    `json:"loaded_from"`
	Revision       string    `json:"revision"`
	CapturedAt     time.Time `json:"captured_at"`
	EffectiveFrom  string    `json:"effective_from,omitempty"`
	EffectiveUntil string    `json:"effective_until,omitempty"`
	ETag           string    `json:"etag,omitempty"`
}

type CostSnapshotV1 struct {
	Priced               bool                        `json:"priced"`
	NanoUSD              *int64                      `json:"nano_usd"`
	Source               string                      `json:"source"`
	Revision             *string                     `json:"revision"`
	CapturedAt           *time.Time                  `json:"captured_at"`
	RatesNanoUSDPerToken *TokenRatesV1               `json:"rates_nano_usd_per_token"`
	RateProvenance       map[string]RateProvenanceV1 `json:"rate_provenance"`
}

type FallbackV1 struct {
	Attempted bool    `json:"attempted"`
	Count     int     `json:"count"`
	Trigger   *string `json:"trigger"`
}

func NewEventID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate telemetry event id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// StatusCode returns zero when no upstream HTTP response was observed.
func (e EventV1) StatusCode() int {
	if e.Status == nil {
		return 0
	}
	return *e.Status
}

// IsHTTPError matches the admin stats definition: only an observed HTTP
// status of 400 or greater counts as an upstream HTTP error. Cancellations
// and transport failures remain visible through TerminationReason.
func (e EventV1) IsHTTPError() bool {
	return e.Status != nil && *e.Status >= 400
}

func (e EventV1) Validate() error {
	switch {
	case e.SchemaVersion != SchemaVersionV1:
		return fmt.Errorf("telemetry schema_version = %d, want %d", e.SchemaVersion, SchemaVersionV1)
	case e.Event != EventRequest:
		return fmt.Errorf("telemetry event = %q, want %q", e.Event, EventRequest)
	case !validEventID(e.EventID):
		return errors.New("telemetry event_id must be 32 lowercase hexadecimal characters")
	case e.StartedAt.IsZero():
		return errors.New("telemetry started_at is required")
	case e.CompletedAt.IsZero():
		return errors.New("telemetry completed_at is required")
	case e.CompletedAt.Before(e.StartedAt):
		return errors.New("telemetry completed_at precedes started_at")
	case e.DurationMS < 0:
		return errors.New("telemetry duration_ms must be nonnegative")
	case e.TTFTMS != nil && *e.TTFTMS < 0:
		return errors.New("telemetry ttft_ms must be nonnegative or null")
	case e.RequestedContextBudgetTokens != nil &&
		*e.RequestedContextBudgetTokens <= 0:
		return errors.New("telemetry requested_context_budget_tokens must be positive or null")
	case !validOptionalSpeed(e.RequestedSpeed):
		return errors.New("telemetry requested_speed must be standard, fast, or null")
	case !validOptionalSpeed(e.EffectiveSpeed):
		return errors.New("telemetry effective_speed must be standard, fast, or null")
	case !validTerminationReason(e.TerminationReason):
		return fmt.Errorf("telemetry termination_reason = %q is invalid", e.TerminationReason)
	case e.Fallback.Count < 0:
		return errors.New("telemetry fallback count must be nonnegative")
	case !e.Fallback.Attempted && e.Fallback.Count != 0:
		return errors.New("telemetry fallback count requires attempted=true")
	case e.ToolCalls < 0:
		return errors.New("telemetry tool_calls must be nonnegative")
	case e.RequestBytes < 0:
		return errors.New("telemetry request_bytes must be nonnegative")
	}
	if err := e.ResponsesCompatibility.validate(); err != nil {
		return err
	}
	if err := e.Usage.validate(e.UsageComplete); err != nil {
		return err
	}
	if err := e.ActualCost.validate("actual_cost", e.Usage); err != nil {
		return err
	}
	if e.NativeCounterfactualCost != nil {
		if err := e.NativeCounterfactualCost.validate(
			"native_counterfactual_cost",
			e.Usage,
		); err != nil {
			return err
		}
	}
	return nil
}

func (c *ResponsesCompatibilityV1) validate() error {
	if c == nil {
		return nil
	}
	switch {
	case c.RepairedEvents < 0:
		return errors.New("telemetry responses_compatibility repaired_events must be nonnegative")
	case c.ValidationErrors < 0:
		return errors.New("telemetry responses_compatibility validation_errors must be nonnegative")
	}

	sets := make(map[string]map[string]struct{}, 3)
	for _, list := range []struct {
		name string
		ids  []string
	}{
		{name: "considered", ids: c.Considered},
		{name: "applied", ids: c.Applied},
		{name: "forced", ids: c.Forced},
	} {
		name, ids := list.name, list.ids
		if len(ids) > 16 {
			return fmt.Errorf("telemetry responses_compatibility %s exceeds 16 rule IDs", name)
		}
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if id == "" {
				return fmt.Errorf("telemetry responses_compatibility %s contains an empty rule ID", name)
			}
			if !validResponsesCompatibilityRuleID(id) {
				return fmt.Errorf("telemetry responses_compatibility %s contains unknown rule ID %q", name, id)
			}
			if _, exists := seen[id]; exists {
				return fmt.Errorf("telemetry responses_compatibility %s contains duplicate rule ID %q", name, id)
			}
			seen[id] = struct{}{}
		}
		sets[name] = seen
	}

	for _, id := range c.Applied {
		if _, considered := sets["considered"][id]; !considered {
			return fmt.Errorf("telemetry responses_compatibility applied rule ID %q was not considered", id)
		}
	}
	for _, id := range c.Forced {
		if _, applied := sets["applied"][id]; !applied {
			return fmt.Errorf("telemetry responses_compatibility forced rule ID %q was not applied", id)
		}
	}
	return nil
}

func validResponsesCompatibilityRuleID(id string) bool {
	switch id {
	case ResponsesCompatibilityRuleTextFormatDefault,
		ResponsesCompatibilityRuleAdditionalToolsInput,
		ResponsesCompatibilityRuleReasoningEffort,
		ResponsesCompatibilityRuleFunctionArgumentsConsistency,
		ResponsesCompatibilityRuleToolTypeDenylist,
		ResponsesCompatibilityRuleCompletionUsage:
		return true
	default:
		return false
	}
}

func validOptionalSpeed(value *string) bool {
	return value == nil || *value == "standard" || *value == "fast"
}

func validEventID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func validTerminationReason(value TerminationReason) bool {
	switch value {
	case TerminationCompleted,
		TerminationClientCancelled,
		TerminationUpstreamHTTPError,
		TerminationUpstreamTransportError,
		TerminationIncompleteStream,
		TerminationGatewayError:
		return true
	default:
		return false
	}
}

func (u UsageV1) validate(complete bool) error {
	values := []*int64{
		u.InputTokens,
		u.OutputTokens,
		u.CacheReadInputTokens,
		u.CacheWrite5mInputTokens,
		u.CacheWrite1hInputTokens,
		u.CacheWriteTotalInputTokens,
	}
	for _, value := range values {
		if value != nil && *value < 0 {
			return errors.New("telemetry usage tokens must be nonnegative or null")
		}
	}
	if complete {
		for _, value := range values[:3] {
			if value == nil {
				return errors.New("telemetry complete usage requires input, output, and cache-read token fields")
			}
		}
		if (u.CacheWrite5mInputTokens == nil) !=
			(u.CacheWrite1hInputTokens == nil) {
			return errors.New("telemetry cache-write token buckets must both be values or both be null")
		}
		if u.CacheWrite5mInputTokens == nil &&
			u.CacheWriteTotalInputTokens == nil {
			return errors.New("telemetry complete usage requires exact or total-only cache-write tokens")
		}
	}
	if u.CacheWriteTotalInputTokens != nil &&
		(u.CacheWrite5mInputTokens != nil ||
			u.CacheWrite1hInputTokens != nil) {
		return errors.New("telemetry cache-write total-only tokens are mutually exclusive with TTL buckets")
	}
	return nil
}

func (c CostSnapshotV1) validate(name string, usage UsageV1) error {
	if c.Priced {
		switch {
		case c.NanoUSD == nil:
			return fmt.Errorf("telemetry %s priced cost requires nano_usd", name)
		case *c.NanoUSD < 0:
			return fmt.Errorf("telemetry %s nano_usd must be nonnegative", name)
		case c.Source == "":
			return fmt.Errorf("telemetry %s priced cost requires source", name)
		case c.Revision == nil || *c.Revision == "":
			return fmt.Errorf("telemetry %s priced cost requires revision", name)
		case c.CapturedAt == nil || c.CapturedAt.IsZero():
			return fmt.Errorf("telemetry %s priced cost requires captured_at", name)
		case c.RatesNanoUSDPerToken == nil:
			return fmt.Errorf("telemetry %s priced cost requires rates", name)
		}
		if err := c.RatesNanoUSDPerToken.validate(name, usage); err != nil {
			return err
		}
		if err := c.validateRateProvenance(name); err != nil {
			return err
		}
		return nil
	}
	if c.NanoUSD != nil {
		return fmt.Errorf("telemetry %s unpriced cost must use null nano_usd", name)
	}
	if len(c.RateProvenance) != 0 {
		return fmt.Errorf("telemetry %s unpriced cost must not include rate provenance", name)
	}
	return nil
}

func (c CostSnapshotV1) validateRateProvenance(name string) error {
	rates := c.RatesNanoUSDPerToken
	expected := map[string]*int64{
		"input":          rates.Input,
		"output":         rates.Output,
		"cache_read":     rates.CacheReadInput,
		"cache_write_5m": rates.CacheWrite5mInput,
		"cache_write_1h": rates.CacheWrite1hInput,
	}
	expectedCount := 0
	for _, rate := range expected {
		if rate != nil {
			expectedCount++
		}
	}
	for dimension := range c.RateProvenance {
		if _, known := expected[dimension]; !known {
			return fmt.Errorf(
				"telemetry %s rate provenance has unknown dimension %q",
				name,
				dimension,
			)
		}
	}
	if len(c.RateProvenance) != expectedCount {
		return fmt.Errorf(
			"telemetry %s rate provenance count = %d, want %d",
			name,
			len(c.RateProvenance),
			expectedCount,
		)
	}
	for dimension, rate := range expected {
		provenance, present := c.RateProvenance[dimension]
		if rate == nil {
			if present {
				return fmt.Errorf(
					"telemetry %s rate provenance has extra dimension %q",
					name,
					dimension,
				)
			}
			continue
		}
		if !present {
			return fmt.Errorf(
				"telemetry %s rate provenance missing dimension %q",
				name,
				dimension,
			)
		}
		if err := provenance.validate(name, dimension); err != nil {
			return err
		}
	}
	return nil
}

func (p RateProvenanceV1) validate(costName, dimension string) error {
	switch {
	case p.Source == "":
		return fmt.Errorf(
			"telemetry %s %s rate provenance requires source",
			costName,
			dimension,
		)
	case p.Revision == "":
		return fmt.Errorf(
			"telemetry %s %s rate provenance requires revision",
			costName,
			dimension,
		)
	case p.CapturedAt.IsZero():
		return fmt.Errorf(
			"telemetry %s %s rate provenance requires captured_at",
			costName,
			dimension,
		)
	}
	switch p.LoadedFrom {
	case "live", "runtime_cache", "vendored_fallback":
	default:
		return fmt.Errorf(
			"telemetry %s %s rate provenance loaded_from %q is invalid",
			costName,
			dimension,
			p.LoadedFrom,
		)
	}
	effectiveFrom, err := optionalEffectiveDate(p.EffectiveFrom)
	if err != nil {
		return fmt.Errorf(
			"telemetry %s %s rate provenance effective_from: %w",
			costName,
			dimension,
			err,
		)
	}
	effectiveUntil, err := optionalEffectiveDate(p.EffectiveUntil)
	if err != nil {
		return fmt.Errorf(
			"telemetry %s %s rate provenance effective_until: %w",
			costName,
			dimension,
			err,
		)
	}
	if !effectiveFrom.IsZero() &&
		!effectiveUntil.IsZero() &&
		effectiveFrom.After(effectiveUntil) {
		return fmt.Errorf(
			"telemetry %s %s rate provenance effective window is inverted",
			costName,
			dimension,
		)
	}
	return nil
}

func optionalEffectiveDate(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("must use YYYY-MM-DD")
	}
	return parsed, nil
}

func (r TokenRatesV1) validate(name string, usage UsageV1) error {
	dimensions := []struct {
		name   string
		tokens *int64
		rate   *int64
	}{
		{name: "input", tokens: usage.InputTokens, rate: r.Input},
		{name: "output", tokens: usage.OutputTokens, rate: r.Output},
		{name: "cache_read_input", tokens: usage.CacheReadInputTokens, rate: r.CacheReadInput},
		{name: "cache_write_5m_input", tokens: usage.CacheWrite5mInputTokens, rate: r.CacheWrite5mInput},
		{name: "cache_write_1h_input", tokens: usage.CacheWrite1hInputTokens, rate: r.CacheWrite1hInput},
	}
	for _, dimension := range dimensions {
		if dimension.rate != nil && *dimension.rate < 0 {
			return fmt.Errorf(
				"telemetry %s %s rate must be nonnegative",
				name,
				dimension.name,
			)
		}
		if dimension.tokens != nil &&
			*dimension.tokens != 0 &&
			dimension.rate == nil {
			return fmt.Errorf(
				"telemetry %s nonzero %s usage requires a rate",
				name,
				dimension.name,
			)
		}
	}
	return nil
}
