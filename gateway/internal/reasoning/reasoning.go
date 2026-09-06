// Package reasoning resolves semantic model reasoning policy and applies the
// small set of provider request transforms that Baseten Switch has reviewed.
//
// Catalog data supplies capabilities, never provider field names. Wire
// mappings remain explicit, standard-library code in this package.
package reasoning

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Mode string

const (
	ModePassthrough   Mode = "passthrough"
	ModeOn            Mode = "on"
	ModeOff           Mode = "off"
	ModeFollowHarness Mode = "follow_harness"
	ModeFixed         Mode = "fixed"
)

type Source string

const (
	SourceCompatibilityDefault Source = "compatibility_default"
	SourceConfigured           Source = "user_config"
	SourceInternalPassthrough  Source = "internal_passthrough"
)

type WireShape string

const (
	WireAnthropicMessages WireShape = "anthropic_messages"
	WireOpenAIChat        WireShape = "openai_chat_completions"
	WireOpenAIResponses   WireShape = "openai_responses"
	WireTranslatedChat    WireShape = "anthropic_to_openai_chat"
)

// RequestedReasoning is the harness's semantic reasoning intent. It is
// captured before any cross-shape translation.
type RequestedReasoning struct {
	Present      bool
	Disabled     bool
	Effort       string
	BudgetTokens *int64
	// Recognized is false when the top-level control is present but does not
	// match a reviewed semantic shape.
	Recognized bool
}

type Capability struct {
	Known        bool
	Supported    bool
	Toggle       bool
	Efforts      []string
	BudgetTokens bool
}

type StoredPolicy struct {
	Present bool
	Mode    Mode
	Effort  string
}

type Input struct {
	Provider         string
	CanonicalModelID string
	WireShape        WireShape
	Capability       Capability
	Stored           StoredPolicy
	Requested        RequestedReasoning
}

type Decision struct {
	Mode   Mode
	Effort string
	Source Source
}

// AdapterAvailability is the reviewed policy surface for one target and wire
// shape after intersecting catalog capability with implemented protocol
// transforms. The slices are ordered for direct admin/UI use.
type AdapterAvailability struct {
	Modes   []Mode
	Efforts []string
}

type messagesCompatibility struct {
	Provider         string
	CanonicalModelID string
	ExplicitOnOff    bool
	DefaultOff       bool
}

var reviewedMessagesCompatibility = []messagesCompatibility{
	{
		Provider:         "baseten",
		CanonicalModelID: "zai-org/GLM-5.2-Fast",
		ExplicitOnOff:    true,
		DefaultOff:       true,
	},
}

// PolicyError is a local request preflight error. Gateway routing may advance
// mapped/default traffic to its already-resolved native fallback, while
// explicit Baseten selections remain loud local errors.
type PolicyError struct {
	Model     string
	Mode      Mode
	WireShape WireShape
	Reason    string
	// FallbackAllowed distinguishes a target-policy incompatibility from a
	// malformed harness request. Only the former may advance to native.
	FallbackAllowed bool
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf(
		"reasoning_policy_error: Baseten model %q cannot apply reasoning mode %q on %s: %s",
		e.Model,
		e.Mode,
		e.WireShape,
		e.Reason,
	)
}

func IsPolicyError(err error) bool {
	var policyErr *PolicyError
	return errors.As(err, &policyErr)
}

func AllowsFallback(err error) bool {
	var policyErr *PolicyError
	return errors.As(err, &policyErr) && policyErr.FallbackAllowed
}

// Resolve selects the effective semantic policy after provider, capability,
// and active wire-adapter resolution.
func Resolve(in Input) (Decision, error) {
	decision := EffectivePolicy(in)

	if decision.Mode == ModePassthrough {
		return decision, nil
	}
	if decision.Mode == ModeFixed {
		return Decision{}, policyError(
			in,
			decision.Mode,
			"fixed effort is not supported by the Messages-first adapter",
		)
	}
	if in.WireShape != WireAnthropicMessages {
		return Decision{}, policyError(
			in,
			decision.Mode,
			"this phase supports same-shape Anthropic Messages only",
		)
	}
	switch decision.Mode {
	case ModeFollowHarness:
		if !in.Capability.Known || !in.Capability.Supported {
			return Decision{}, policyError(
				in,
				decision.Mode,
				"the exact target has no validated reasoning capability",
			)
		}
		if !supportsMessagesFollowHarness(in) {
			return Decision{}, policyError(
				in,
				decision.Mode,
				"Messages Follow Harness requires a toggle or budget_tokens capability",
			)
		}
		if in.Requested.Present && !in.Requested.Recognized {
			return Decision{}, requestPolicyError(
				in,
				decision.Mode,
				"the incoming top-level thinking control is not a reviewed shape",
			)
		}
		if in.Requested.BudgetTokens != nil &&
			!in.Capability.BudgetTokens &&
			!in.Capability.Toggle {
			return Decision{}, policyError(
				in,
				decision.Mode,
				"the incoming numeric thinking budget has no reviewed target control",
			)
		}
		if in.Requested.Present &&
			in.Requested.BudgetTokens == nil &&
			!in.Capability.Toggle {
			return Decision{}, policyError(
				in,
				decision.Mode,
				"the incoming thinking toggle is not supported by the exact target",
			)
		}
		return decision, nil
	case ModeOn, ModeOff:
		if !in.Capability.Known || !in.Capability.Supported {
			return Decision{}, policyError(
				in,
				decision.Mode,
				"the exact target has no validated reasoning capability",
			)
		}
		if !supportsMessagesExplicitOnOff(in) {
			return Decision{}, policyError(
				in,
				decision.Mode,
				"Messages On and Off require a reviewed binary control",
			)
		}
		return decision, nil
	default:
		return Decision{}, policyError(in, decision.Mode, "unknown reasoning mode")
	}
}

// EffectivePolicy resolves stored policy and compatibility defaults without
// asserting that the selected adapter can represent the result. Admin status
// uses this to preserve and explain unavailable saved values. Resolve performs
// the request-time capability and adapter validation afterward.
func EffectivePolicy(in Input) Decision {
	if in.Provider != "baseten" {
		return Decision{
			Mode:   ModePassthrough,
			Source: SourceInternalPassthrough,
		}
	}
	if in.Stored.Present {
		return Decision{
			Mode:   in.Stored.Mode,
			Effort: in.Stored.Effort,
			Source: SourceConfigured,
		}
	}
	if supportsMessagesDefaultOff(in) {
		return Decision{
			Mode:   ModeOff,
			Source: SourceCompatibilityDefault,
		}
	}
	return Decision{
		Mode:   ModePassthrough,
		Source: SourceInternalPassthrough,
	}
}

// ReviewedAdapterAvailability returns only policies implemented and reviewed
// for the provider, model capability, and wire shape. Catalog metadata
// supplies semantic controls but never enables an unreviewed wire transform.
func ReviewedAdapterAvailability(in Input) AdapterAvailability {
	availability := AdapterAvailability{
		Modes:   []Mode{},
		Efforts: []string{},
	}
	if in.Provider != "baseten" ||
		in.WireShape != WireAnthropicMessages ||
		!in.Capability.Known ||
		!in.Capability.Supported {
		return availability
	}
	if supportsMessagesExplicitOnOff(in) {
		availability.Modes = append(
			availability.Modes,
			ModeOff,
			ModeOn,
		)
	}
	if supportsMessagesFollowHarness(in) {
		availability.Modes = append(
			availability.Modes,
			ModeFollowHarness,
		)
	}
	return availability
}

func supportsMessagesDefaultOff(in Input) bool {
	if in.Provider != "baseten" ||
		in.WireShape != WireAnthropicMessages ||
		!in.Capability.Known ||
		!in.Capability.Supported {
		return false
	}
	if in.Capability.Toggle {
		return true
	}
	compatibility, ok := reviewedMessagesCompatibilityFor(in)
	return ok && compatibility.ExplicitOnOff && compatibility.DefaultOff
}

func supportsMessagesExplicitOnOff(in Input) bool {
	if in.Provider != "baseten" ||
		in.WireShape != WireAnthropicMessages ||
		!in.Capability.Known ||
		!in.Capability.Supported {
		return false
	}
	if in.Capability.Toggle {
		return true
	}
	compatibility, ok := reviewedMessagesCompatibilityFor(in)
	return ok && compatibility.ExplicitOnOff
}

func reviewedMessagesCompatibilityFor(in Input) (messagesCompatibility, bool) {
	for _, compatibility := range reviewedMessagesCompatibility {
		if compatibility.Provider == in.Provider &&
			compatibility.CanonicalModelID == in.CanonicalModelID {
			return compatibility, true
		}
	}
	return messagesCompatibility{}, false
}

func supportsMessagesFollowHarness(in Input) bool {
	return in.Provider == "baseten" &&
		in.WireShape == WireAnthropicMessages &&
		in.Capability.Known &&
		in.Capability.Supported &&
		(in.Capability.Toggle || in.Capability.BudgetTokens)
}

func policyError(in Input, mode Mode, reason string) error {
	return &PolicyError{
		Model:           in.CanonicalModelID,
		Mode:            mode,
		WireShape:       in.WireShape,
		Reason:          reason,
		FallbackAllowed: true,
	}
}

func requestPolicyError(in Input, mode Mode, reason string) error {
	return &PolicyError{
		Model:           in.CanonicalModelID,
		Mode:            mode,
		WireShape:       in.WireShape,
		Reason:          reason,
		FallbackAllowed: false,
	}
}

// InspectAnthropicMessages captures top-level Messages reasoning intent.
// Malformed or unfamiliar values remain Present without inventing semantics.
func InspectAnthropicMessages(body []byte) RequestedReasoning {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return RequestedReasoning{}
	}
	raw, ok := envelope["thinking"]
	if !ok {
		return RequestedReasoning{}
	}
	out := RequestedReasoning{Present: true}
	var thinking struct {
		Type         string `json:"type"`
		BudgetTokens *int64 `json:"budget_tokens"`
	}
	if err := json.Unmarshal(raw, &thinking); err != nil {
		return out
	}
	switch strings.ToLower(thinking.Type) {
	case "enabled", "disabled", "adaptive":
		out.Recognized = true
	default:
		return out
	}
	out.Disabled = strings.EqualFold(thinking.Type, "disabled")
	out.BudgetTokens = thinking.BudgetTokens
	return out
}

// ApplyAnthropicMessages applies a reviewed same-shape Messages policy.
// Passthrough preserves the original bytes. Follow Harness preserves reviewed
// enabled and disabled controls, and normalizes Claude's adaptive control to
// the Baseten Messages toggle shape. On and Off replace only the top-level
// reasoning control. Every transform retains all other JSON fields.
func ApplyAnthropicMessages(body []byte, decision Decision) ([]byte, error) {
	switch decision.Mode {
	case ModePassthrough:
		return body, nil
	case ModeFollowHarness:
		return normalizeAnthropicAdaptiveThinking(body)
	case ModeOn, ModeOff:
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(body, &envelope); err != nil ||
			envelope == nil {
			reason := "request body is not a JSON object"
			if err != nil {
				reason = "decode request body: " + err.Error()
			}
			return nil, &PolicyError{
				Mode:      decision.Mode,
				WireShape: WireAnthropicMessages,
				Reason:    reason,
			}
		}
		thinking := json.RawMessage(`{"type":"disabled"}`)
		if decision.Mode == ModeOn {
			thinking = json.RawMessage(`{"type":"enabled"}`)
		}
		envelope["thinking"] = thinking
		transformed, err := json.Marshal(envelope)
		if err != nil {
			return nil, &PolicyError{
				Mode:      decision.Mode,
				WireShape: WireAnthropicMessages,
				Reason:    "encode request body: " + err.Error(),
			}
		}
		return transformed, nil
	default:
		return nil, &PolicyError{
			Mode:      decision.Mode,
			WireShape: WireAnthropicMessages,
			Reason:    "mode is unsupported by the Messages adapter",
		}
	}
}

func normalizeAnthropicAdaptiveThinking(body []byte) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return body, nil
	}
	raw, ok := envelope["thinking"]
	if !ok {
		return body, nil
	}
	var thinking struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &thinking); err != nil ||
		!strings.EqualFold(thinking.Type, "adaptive") {
		return body, nil
	}
	envelope["thinking"] = json.RawMessage(`{"type":"enabled"}`)
	transformed, err := json.Marshal(envelope)
	if err != nil {
		return nil, &PolicyError{
			Mode:      ModeFollowHarness,
			WireShape: WireAnthropicMessages,
			Reason:    "encode request body: " + err.Error(),
		}
	}
	return transformed, nil
}
