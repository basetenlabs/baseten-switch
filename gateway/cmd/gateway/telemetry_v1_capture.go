package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/reasoning"
	"github.com/basetenlabs/baseten-switch/gateway/internal/requestprofile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/usage"
)

const telemetryModelFamilyRevisionV1 = "claude-family-v1"

// telemetryRequestCaptureV1 is immutable request-admission state. In
// particular, nativeQuote must not be looked up again when the response ends:
// a catalog refresh during the request cannot change its counterfactual.
type telemetryRequestCaptureV1 struct {
	eventID                      string
	startedAt                    time.Time
	client                       string
	configuredRoute              string
	requestedModel               string
	canonicalModel               string
	requestedModelFamily         string
	requestedContextBudgetTokens *int64
	requestedSpeed               *string
	requestedReasoning           reasoning.RequestedReasoning
	requestedReasoningObserved   bool
	requestedOneHourCache        bool
	// primaryReasoning is the admitted primary attempt's reasoning decision.
	// It remains logical-request metadata when a later native attempt serves.
	primaryReasoning         reasoningTelemetryV1
	nativePricingUnsupported bool
	nativeQuote              pricing.Quote
	nativeQuoteCapturedAt    time.Time
}

// telemetryAttemptCaptureV1 is immutable attempt-start state. A fallback
// attempt gets its own capture, and the event uses the capture belonging to
// the attempt that actually served the request.
type telemetryAttemptCaptureV1 struct {
	effectiveProvider   string
	servedModel         string
	actualStandardQuote pricing.Quote
	actualFastQuote     pricing.Quote
	quoteCapturedAt     time.Time
	reasoning           reasoningTelemetryV1
}

// telemetryCompletionV1 contains facts known only when the logical request
// ends. Usage uses pointers so missing provider values remain distinct from
// reported zeroes.
type telemetryCompletionV1 struct {
	completedAt              time.Time
	providerReportedModel    *string
	status                   *int
	isStream                 bool
	firstOutputAt            *time.Time
	responseComplete         bool
	contextErr               error
	gatewayFailure           bool
	usageComplete            bool
	usage                    telemetry.UsageV1
	effectiveSpeed           *string
	actualPricingUnsupported bool
	providerStopReason       *string
	fallbackCount            int
	fallbackTrigger          *string
	subagent                 bool
	subagentModel            *string
	sanitized                bool
	translated               bool
	strippedToolTypes        []string
	responsesCompatibility   *telemetry.ResponsesCompatibilityV1
	toolCalls                int
	requestBytes             int
}

func (request telemetryRequestCaptureV1) requestedReasoningPresent() *bool {
	if !request.requestedReasoningObserved {
		return nil
	}
	present := request.requestedReasoning.Present
	return &present
}

func (request telemetryRequestCaptureV1) requestedReasoningEffort() *string {
	if !request.requestedReasoningObserved ||
		request.requestedReasoning.Effort == "" {
		return nil
	}
	effort := request.requestedReasoning.Effort
	return &effort
}

func captureTelemetryRequestV1(
	snapshot *pricing.Snapshot,
	startedAt time.Time,
	client string,
	configuredRoute string,
	protocolShape string,
	requestedModel string,
) (telemetryRequestCaptureV1, error) {
	return captureTelemetryRequestProfileV1(
		snapshot,
		startedAt,
		client,
		configuredRoute,
		protocolShape,
		requestedModel,
		requestprofile.Profile{},
	)
}

func captureTelemetryRequestProfileV1(
	snapshot *pricing.Snapshot,
	startedAt time.Time,
	client string,
	configuredRoute string,
	protocolShape string,
	requestedModel string,
	profile requestprofile.Profile,
) (telemetryRequestCaptureV1, error) {
	eventID, err := telemetry.NewEventID()
	if err != nil {
		return telemetryRequestCaptureV1{}, err
	}
	canonicalModel := profile.CanonicalModel
	if canonicalModel == "" {
		canonicalModel = normalizeModelID(requestedModel)
	}
	nativeProvider := config.NativeRoute(protocolShape)
	family := ""
	if snapshot != nil && nativeProvider == pricing.ProviderAnthropic {
		if catalogFamily, ok := snapshot.ModelFamily(
			pricing.ProviderAnthropic,
			canonicalModel,
		); ok {
			family = catalogFamily
		}
	}
	if family == "" {
		family = familyOf(canonicalModel)
	}
	if family == "" {
		family = "other"
	}
	var requestedContextBudgetTokens *int64
	var requestedSpeed *string
	if nativeProvider == pricing.ProviderAnthropic {
		requestedContextBudgetTokens = cloneInt64Pointer(
			profile.RequestedContextBudgetTokens,
		)
		requestedSpeed = supportedSpeedPointer(profile.RequestedSpeed)
	}
	nativePricingUnsupported := nativeProvider == pricing.ProviderAnthropic &&
		((profile.RequestedSpeedPresent && requestedSpeed == nil) ||
			(profile.RequestedInferenceGeoPresent &&
				profile.RequestedInferenceGeo != "global"))
	nativeProfile := pricing.ProfileStandard
	if requestedSpeed != nil && *requestedSpeed == string(pricing.ProfileFast) {
		nativeProfile = pricing.ProfileFast
	}
	var nativeQuote pricing.Quote
	if snapshot != nil {
		nativeQuote = snapshot.QuoteProfile(
			nativeProvider,
			canonicalModel,
			nativeProfile,
		)
	}
	startedAt = startedAt.UTC()
	return telemetryRequestCaptureV1{
		eventID:                      eventID,
		startedAt:                    startedAt,
		client:                       client,
		configuredRoute:              configuredRoute,
		requestedModel:               requestedModel,
		canonicalModel:               canonicalModel,
		requestedModelFamily:         family,
		requestedContextBudgetTokens: requestedContextBudgetTokens,
		requestedSpeed:               requestedSpeed,
		requestedOneHourCache:        profile.RequestedOneHourCache,
		nativePricingUnsupported:     nativePricingUnsupported,
		nativeQuote:                  nativeQuote,
		nativeQuoteCapturedAt:        startedAt,
	}, nil
}

func captureTelemetryAttemptV1(
	snapshot *pricing.Snapshot,
	capturedAt time.Time,
	effectiveProvider string,
	servedModel string,
) telemetryAttemptCaptureV1 {
	var standardQuote pricing.Quote
	var fastQuote pricing.Quote
	if snapshot != nil {
		standardQuote = snapshot.QuoteProfile(
			effectiveProvider,
			servedModel,
			pricing.ProfileStandard,
		)
		if effectiveProvider == pricing.ProviderAnthropic {
			fastQuote = snapshot.QuoteProfile(
				effectiveProvider,
				servedModel,
				pricing.ProfileFast,
			)
		}
	}
	return telemetryAttemptCaptureV1{
		effectiveProvider:   effectiveProvider,
		servedModel:         servedModel,
		actualStandardQuote: standardQuote,
		actualFastQuote:     fastQuote,
		quoteCapturedAt:     capturedAt.UTC(),
	}
}

func (request telemetryRequestCaptureV1) event(
	attempt telemetryAttemptCaptureV1,
	completion telemetryCompletionV1,
) (telemetry.EventV1, error) {
	completedAt := completion.completedAt.UTC()
	if completedAt.Before(request.startedAt) {
		return telemetry.EventV1{}, fmt.Errorf("telemetry completion precedes request start")
	}
	actualQuote, effectiveSpeed := request.actualQuote(attempt, completion)
	actual := costSnapshotV1(
		actualQuote,
		attempt.quoteCapturedAt,
		completion.usage,
		completion.usageComplete,
		attempt.effectiveProvider == pricing.ProviderBaseten,
	)
	var nativeCounterfactual *telemetry.CostSnapshotV1
	if attempt.effectiveProvider == "baseten" {
		nativeQuote := request.nativeQuote
		if request.nativePricingUnsupported {
			nativeQuote = unpricedQuote(nativeQuote)
		}
		value := costSnapshotV1(
			nativeQuote,
			request.nativeQuoteCapturedAt,
			completion.usage,
			completion.usageComplete,
			false,
		)
		nativeCounterfactual = &value
	}
	reasoningCapture := attempt.reasoning
	if request.primaryReasoning.policyMode != nil ||
		request.primaryReasoning.effectiveEnabled != nil ||
		request.primaryReasoning.effectiveEffort != nil ||
		request.primaryReasoning.policySource != nil ||
		request.primaryReasoning.catalogRevision != nil {
		reasoningCapture = request.primaryReasoning
	}
	event := telemetry.EventV1{
		SchemaVersion:        telemetry.SchemaVersionV1,
		Event:                telemetry.EventRequest,
		EventID:              request.eventID,
		StartedAt:            request.startedAt,
		CompletedAt:          completedAt,
		Client:               request.client,
		ConfiguredRoute:      request.configuredRoute,
		EffectiveProvider:    attempt.effectiveProvider,
		RequestedModel:       request.requestedModel,
		RequestedModelFamily: request.requestedModelFamily,
		RequestedContextBudgetTokens: cloneInt64Pointer(
			request.requestedContextBudgetTokens,
		),
		RequestedSpeed:            cloneStringPointer(request.requestedSpeed),
		RequestedReasoningPresent: request.requestedReasoningPresent(),
		RequestedReasoningEffort:  request.requestedReasoningEffort(),
		ReasoningPolicyMode: cloneStringPointer(
			reasoningCapture.policyMode,
		),
		EffectiveReasoningEnabled: cloneBoolPointer(
			reasoningCapture.effectiveEnabled,
		),
		EffectiveReasoningEffort: cloneStringPointer(
			reasoningCapture.effectiveEffort,
		),
		ReasoningPolicySource: cloneStringPointer(
			reasoningCapture.policySource,
		),
		ReasoningCatalogRevision: cloneStringPointer(
			reasoningCapture.catalogRevision,
		),
		ModelFamilyRevision:      telemetryModelFamilyRevisionV1,
		ServedModel:              attempt.servedModel,
		ProviderReportedModel:    cloneStringPointer(completion.providerReportedModel),
		EffectiveSpeed:           effectiveSpeed,
		Status:                   cloneIntPointer(completion.status),
		IsStream:                 completion.isStream,
		DurationMS:               completedAt.Sub(request.startedAt).Milliseconds(),
		TTFTMS:                   outputLatencyMS(request.startedAt, completion.firstOutputAt),
		TerminationReason:        classifyTelemetryTermination(completion),
		ProviderStopReason:       cloneStringPointer(completion.providerStopReason),
		UsageComplete:            completion.usageComplete,
		Usage:                    cloneUsageV1(completion.usage),
		ActualCost:               actual,
		NativeCounterfactualCost: nativeCounterfactual,
		Fallback: telemetry.FallbackV1{
			Attempted: completion.fallbackCount > 0,
			Count:     completion.fallbackCount,
			Trigger:   cloneStringPointer(completion.fallbackTrigger),
		},
		Subagent:          completion.subagent,
		SubagentModel:     cloneStringPointer(completion.subagentModel),
		Sanitized:         completion.sanitized,
		Translated:        completion.translated,
		StrippedToolTypes: append([]string(nil), completion.strippedToolTypes...),
		ResponsesCompatibility: cloneResponsesCompatibilityV1(
			completion.responsesCompatibility,
		),
		ToolCalls:    completion.toolCalls,
		RequestBytes: completion.requestBytes,
	}
	if event.StrippedToolTypes == nil {
		event.StrippedToolTypes = []string{}
	}
	if err := event.Validate(); err != nil {
		return telemetry.EventV1{}, err
	}
	return event, nil
}

func cloneResponsesCompatibilityV1(
	in *telemetry.ResponsesCompatibilityV1,
) *telemetry.ResponsesCompatibilityV1 {
	if in == nil {
		return nil
	}
	out := *in
	out.Considered = append([]string(nil), in.Considered...)
	out.Applied = append([]string(nil), in.Applied...)
	out.Forced = append([]string(nil), in.Forced...)
	if out.Considered == nil {
		out.Considered = []string{}
	}
	if out.Applied == nil {
		out.Applied = []string{}
	}
	if out.Forced == nil {
		out.Forced = []string{}
	}
	return &out
}

func (request telemetryRequestCaptureV1) actualQuote(
	attempt telemetryAttemptCaptureV1,
	completion telemetryCompletionV1,
) (pricing.Quote, *string) {
	if attempt.effectiveProvider != pricing.ProviderAnthropic {
		return attempt.actualStandardQuote, nil
	}
	if request.nativePricingUnsupported ||
		completion.actualPricingUnsupported {
		return unpricedQuote(attempt.actualStandardQuote), nil
	}

	if completion.effectiveSpeed != nil {
		effectiveSpeed := supportedSpeedPointerValue(completion.effectiveSpeed)
		if effectiveSpeed == nil {
			return unpricedQuote(attempt.actualStandardQuote), nil
		}
		if *effectiveSpeed == string(pricing.ProfileFast) {
			return attempt.actualFastQuote, effectiveSpeed
		}
		return attempt.actualStandardQuote, effectiveSpeed
	}

	if request.requestedSpeed != nil &&
		*request.requestedSpeed == string(pricing.ProfileFast) {
		return unpricedQuote(attempt.actualFastQuote), nil
	}
	return attempt.actualStandardQuote, nil
}

func unpricedQuote(quote pricing.Quote) pricing.Quote {
	quote.Priced = false
	quote.Price = pricing.Price{}
	return quote
}

func supportedSpeedPointer(value string) *string {
	if value != string(pricing.ProfileStandard) &&
		value != string(pricing.ProfileFast) {
		return nil
	}
	return stringPointer(value)
}

func supportedSpeedPointerValue(value *string) *string {
	if value == nil {
		return nil
	}
	return supportedSpeedPointer(*value)
}

func classifyTelemetryTermination(completion telemetryCompletionV1) telemetry.TerminationReason {
	switch {
	case completion.gatewayFailure:
		return telemetry.TerminationGatewayError
	case errors.Is(completion.contextErr, context.Canceled):
		return telemetry.TerminationClientCancelled
	case completion.status == nil:
		return telemetry.TerminationUpstreamTransportError
	case !completion.responseComplete:
		return telemetry.TerminationIncompleteStream
	case *completion.status >= 400:
		return telemetry.TerminationUpstreamHTTPError
	default:
		return telemetry.TerminationCompleted
	}
}

// outputLatencyMS intentionally measures the first content delta or output
// token observed by the relay. Callers must not pass header, SSE framing,
// ping, message_start, or content_block_start timestamps.
func outputLatencyMS(startedAt time.Time, firstOutputAt *time.Time) *int64 {
	if firstOutputAt == nil || firstOutputAt.Before(startedAt) {
		return nil
	}
	value := firstOutputAt.Sub(startedAt).Milliseconds()
	return &value
}

// containsOutputDelta recognizes output-bearing SSE events across the
// Anthropic, OpenAI chat-completions, and OpenAI Responses shapes. Callers
// pass the bytes collected so far so a JSON event split across network reads
// becomes visible once its terminating line arrives.
func containsOutputDelta(stream []byte) bool {
	for _, rawLine := range bytes.Split(stream, []byte{'\n'}) {
		line := bytes.TrimSpace(rawLine)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var event map[string]json.RawMessage
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		var eventType string
		_ = json.Unmarshal(event["type"], &eventType)
		if eventType == "content_block_delta" ||
			(strings.HasPrefix(eventType, "response.") &&
				strings.HasSuffix(eventType, ".delta")) {
			return true
		}

		var choices []struct {
			Delta map[string]json.RawMessage `json:"delta"`
		}
		if json.Unmarshal(event["choices"], &choices) != nil {
			continue
		}
		for _, choice := range choices {
			for _, key := range []string{
				"content",
				"reasoning_content",
				"tool_calls",
				"function_call",
			} {
				value, ok := choice.Delta[key]
				if ok && outputDeltaValuePresent(value) {
					return true
				}
			}
		}
	}
	return false
}

func outputDeltaValuePresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 ||
		bytes.Equal(trimmed, []byte("null")) ||
		bytes.Equal(trimmed, []byte(`""`)) ||
		bytes.Equal(trimmed, []byte("[]")) ||
		bytes.Equal(trimmed, []byte("{}")) {
		return false
	}
	return true
}

func costSnapshotV1(
	quote pricing.Quote,
	capturedAt time.Time,
	usage telemetry.UsageV1,
	usageComplete bool,
	combineCacheWrites bool,
) telemetry.CostSnapshotV1 {
	out := telemetry.CostSnapshotV1{
		Source: quote.Source,
	}
	if quote.Revision != "" {
		out.Revision = stringPointer(quote.Revision)
	}
	if !capturedAt.IsZero() {
		capturedAt = capturedAt.UTC()
		out.CapturedAt = &capturedAt
	}
	if !quote.Priced || !usageComplete {
		return out
	}
	if usage.InputTokens == nil ||
		usage.OutputTokens == nil ||
		usage.CacheReadInputTokens == nil {
		return out
	}
	var cacheWrite5m, cacheWrite1h int64
	if combineCacheWrites {
		if usage.CacheWriteTotalInputTokens != nil {
			cacheWrite5m = *usage.CacheWriteTotalInputTokens
		} else if usage.CacheWrite5mInputTokens != nil &&
			usage.CacheWrite1hInputTokens != nil {
			cacheWrite5m = *usage.CacheWrite5mInputTokens +
				*usage.CacheWrite1hInputTokens
		} else {
			return out
		}
	} else {
		if usage.CacheWrite5mInputTokens == nil ||
			usage.CacheWrite1hInputTokens == nil {
			return out
		}
		cacheWrite5m = *usage.CacheWrite5mInputTokens
		cacheWrite1h = *usage.CacheWrite1hInputTokens
	}
	if !quote.HasRatesForUsage(
		*usage.InputTokens,
		*usage.OutputTokens,
		*usage.CacheReadInputTokens,
		cacheWrite5m,
		cacheWrite1h,
	) {
		return out
	}
	rates, err := quote.NanoUSDRates()
	if err != nil {
		return out
	}
	cost, err := rates.CostNanoUSD(
		*usage.InputTokens,
		*usage.OutputTokens,
		*usage.CacheReadInputTokens,
		cacheWrite5m,
		cacheWrite1h,
	)
	if err != nil {
		return out
	}
	eventRates := telemetry.TokenRatesV1{
		Input: ratePointer(
			rates.Prompt,
			!quote.RatePresenceKnown || quote.RatePresence.Input,
		),
		Output: ratePointer(
			rates.Completion,
			!quote.RatePresenceKnown || quote.RatePresence.Output,
		),
		CacheReadInput: ratePointer(
			rates.CacheRead,
			!quote.RatePresenceKnown || quote.RatePresence.CacheRead,
		),
		CacheWrite5mInput: ratePointer(
			rates.CacheWrite5m,
			!quote.RatePresenceKnown || quote.RatePresence.CacheWrite5m,
		),
		CacheWrite1hInput: ratePointer(
			rates.CacheWrite1h,
			!quote.RatePresenceKnown || quote.RatePresence.CacheWrite1h,
		),
	}
	if combineCacheWrites && eventRates.CacheWrite5mInput != nil {
		eventRates.CacheWrite1hInput = cloneInt64Pointer(
			eventRates.CacheWrite5mInput,
		)
	}
	out.Priced = true
	out.NanoUSD = &cost
	out.RatesNanoUSDPerToken = &eventRates
	out.RateProvenance = telemetryRateProvenanceV1(
		quote,
		combineCacheWrites,
	)
	return out
}

func telemetryRateProvenanceV1(
	quote pricing.Quote,
	combineCacheWrites bool,
) map[string]telemetry.RateProvenanceV1 {
	out := make(map[string]telemetry.RateProvenanceV1, 5)
	values := []struct {
		dimension  pricing.RateDimension
		provenance pricing.Provenance
	}{
		{pricing.RateInput, quote.RateProvenance.Input},
		{pricing.RateOutput, quote.RateProvenance.Output},
		{pricing.RateCacheRead, quote.RateProvenance.CacheRead},
		{pricing.RateCacheWrite5m, quote.RateProvenance.CacheWrite5m},
		{pricing.RateCacheWrite1h, quote.RateProvenance.CacheWrite1h},
	}
	for _, value := range values {
		if value.provenance.Source == "" {
			continue
		}
		out[string(value.dimension)] = telemetry.RateProvenanceV1{
			Source:         value.provenance.Source,
			LoadedFrom:     string(value.provenance.LoadedFrom),
			Revision:       value.provenance.Revision,
			CapturedAt:     value.provenance.CapturedAt,
			EffectiveFrom:  value.provenance.EffectiveFrom,
			EffectiveUntil: value.provenance.EffectiveUntil,
			ETag:           value.provenance.ETag,
		}
	}
	if combineCacheWrites {
		if provenance, ok := out[string(pricing.RateCacheWrite5m)]; ok {
			out[string(pricing.RateCacheWrite1h)] = provenance
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func ratePointer(value int64, present bool) *int64 {
	if !present {
		return nil
	}
	return &value
}

func cloneUsageV1(value telemetry.UsageV1) telemetry.UsageV1 {
	return telemetry.UsageV1{
		InputTokens:                cloneInt64Pointer(value.InputTokens),
		OutputTokens:               cloneInt64Pointer(value.OutputTokens),
		CacheReadInputTokens:       cloneInt64Pointer(value.CacheReadInputTokens),
		CacheWrite5mInputTokens:    cloneInt64Pointer(value.CacheWrite5mInputTokens),
		CacheWrite1hInputTokens:    cloneInt64Pointer(value.CacheWrite1hInputTokens),
		CacheWriteTotalInputTokens: cloneInt64Pointer(value.CacheWriteTotalInputTokens),
	}
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func stringPointer(value string) *string {
	value = strings.Clone(value)
	return &value
}

func optionalStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return stringPointer(value)
}

func observedGatewayUsageV1(
	observed usage.Usage,
	complete bool,
	requestedOneHourCache bool,
) telemetry.UsageV1 {
	if !complete {
		return telemetry.UsageV1{}
	}
	out := telemetry.UsageV1{
		InputTokens:          cloneInt64Pointer(&observed.InputTokens),
		OutputTokens:         cloneInt64Pointer(&observed.OutputTokens),
		CacheReadInputTokens: cloneInt64Pointer(&observed.CacheReadInputTokens),
	}
	switch {
	case observed.CacheCreationTokenBreakdownComplete &&
		!observed.CacheCreationTokenBreakdownInconsistent:
		out.CacheWrite5mInputTokens = cloneInt64Pointer(
			&observed.CacheCreationFiveMinuteInputTokens,
		)
		out.CacheWrite1hInputTokens = cloneInt64Pointer(
			&observed.CacheCreationOneHourInputTokens,
		)
	case observed.CacheCreationInputTokens == 0:
		zero := int64(0)
		out.CacheWrite5mInputTokens = &zero
		out.CacheWrite1hInputTokens = cloneInt64Pointer(&zero)
	case !requestedOneHourCache &&
		!observed.CacheCreationTokenBreakdownInconsistent:
		zero := int64(0)
		out.CacheWrite5mInputTokens = cloneInt64Pointer(
			&observed.CacheCreationInputTokens,
		)
		out.CacheWrite1hInputTokens = &zero
	default:
		out.CacheWriteTotalInputTokens = cloneInt64Pointer(
			&observed.CacheCreationInputTokens,
		)
	}
	return out
}

func telemetryRequestedOneHourCache(at upstreamAttempt) bool {
	return at.telemetryRequest != nil &&
		at.telemetryRequest.requestedOneHourCache
}

func (g *Gateway) captureLocalTelemetryAttemptV1(
	cl *clientListener,
	startedAt time.Time,
	requestedModel string,
) (upstreamAttempt, bool) {
	snapshot := g.pricing.Capture()
	request, err := captureTelemetryRequestV1(
		snapshot,
		startedAt,
		cl.cfg.Name,
		cl.cfg.Route,
		cl.cfg.ProtocolShape,
		requestedModel,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] telemetry capture failed: %v\n", err)
		return upstreamAttempt{}, false
	}
	attempt := upstreamAttempt{
		route:            cl.cfg.Route,
		modelForCost:     requestedModel,
		telemetryRequest: &request,
		telemetryAttempt: captureTelemetryAttemptV1(
			snapshot,
			startedAt,
			cl.cfg.Route,
			requestedModel,
		),
	}
	return attempt, true
}

// recordTelemetryV1 is the single request-capture exit. Storage lifecycle is
// owned by writeTelemetryV1.
func (g *Gateway) recordTelemetryV1(
	cl *clientListener,
	at upstreamAttempt,
	completion telemetryCompletionV1,
) {
	if at.telemetryRequest == nil {
		fmt.Fprintln(os.Stderr, "[gateway] telemetry event skipped: missing request capture")
		return
	}
	completion.fallbackCount = at.fallbackCount
	if completion.fallbackTrigger == nil && at.fallbackTrigger != "" {
		completion.fallbackTrigger = stringPointer(at.fallbackTrigger)
	}
	completion.subagent = at.subagent
	if at.subagentModel != "" {
		completion.subagentModel = stringPointer(at.subagentModel)
	}
	completion.sanitized = at.res.Sanitized
	completion.translated = at.translate
	completion.strippedToolTypes = append(
		[]string(nil),
		at.strippedToolTypes...,
	)
	if completion.responsesCompatibility == nil &&
		at.responsesCompatibility != nil {
		completion.responsesCompatibility =
			at.responsesCompatibility.telemetry()
	}
	event, err := at.telemetryRequest.event(at.telemetryAttempt, completion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] telemetry event invalid: %v\n", err)
		return
	}
	if len(event.StrippedToolTypes) > 0 {
		fmt.Fprintf(
			os.Stderr,
			"[gateway] responses_strip client=%s types=%s\n",
			event.Client,
			strings.Join(event.StrippedToolTypes, ","),
		)
	}
	g.writeTelemetryV1(event)
}
