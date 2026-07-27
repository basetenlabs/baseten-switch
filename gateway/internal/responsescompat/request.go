package responsescompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

var (
	ErrAmbiguousAdditionalTools = errors.New("ambiguous additional_tools definitions")
	ErrInvalidAdditionalTools   = errors.New("additional_tools item has no tools array")
)

type RequestContext struct{}

type RequestRule interface {
	ID() string
	Apply(body []byte, ctx RequestContext) (next []byte, changed bool, err error)
}

// NormalizeTextFormat adds the OpenAI plain-text default only when a text
// object is present and its format member is absent. No-op and malformed
// inputs return the original byte slice.
func NormalizeTextFormat(body []byte) ([]byte, bool) {
	data, ok := decodeJSONObject(body)
	if !ok {
		return body, false
	}
	text, ok := data["text"].(map[string]any)
	if !ok {
		return body, false
	}
	if _, exists := text["format"]; exists {
		return body, false
	}
	text["format"] = map[string]any{"type": "text"}
	next, err := json.Marshal(data)
	if err != nil {
		return body, false
	}
	return next, true
}

type AdditionalToolsRule struct{}

func NewAdditionalToolsRule() AdditionalToolsRule {
	return AdditionalToolsRule{}
}

func (AdditionalToolsRule) ID() string { return RuleAdditionalToolsInput }

func (AdditionalToolsRule) Apply(body []byte, _ RequestContext) ([]byte, bool, error) {
	data, ok := decodeJSONObject(body)
	if !ok {
		return body, false, nil
	}
	input, ok := data["input"].([]any)
	if !ok {
		return body, false, nil
	}

	target := -1
	var hoisted []any
	for i, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "additional_tools" {
			continue
		}
		if target >= 0 {
			return body, false, ErrAmbiguousAdditionalTools
		}
		target = i
		tools, ok := item["tools"].([]any)
		if !ok {
			return body, false, ErrInvalidAdditionalTools
		}
		hoisted = tools
	}
	if target < 0 {
		return body, false, nil
	}
	if existing, exists := data["tools"]; exists {
		tools, ok := existing.([]any)
		if !ok || len(tools) != 0 {
			return body, false, ErrAmbiguousAdditionalTools
		}
	}

	nextInput := make([]any, 0, len(input)-1)
	nextInput = append(nextInput, input[:target]...)
	nextInput = append(nextInput, input[target+1:]...)
	data["input"] = nextInput
	data["tools"] = hoisted

	if choice, ok := data["tool_choice"].(map[string]any); ok &&
		choice["type"] == "additional_tools" {
		data["tool_choice"] = "auto"
	}
	next, err := json.Marshal(data)
	if err != nil {
		return body, false, err
	}
	return next, true, nil
}

type ReasoningEffortPolicy struct {
	// Allowed is the target model's explicit accepted set.
	Allowed []string
	// Map optionally overrides the deterministic nearest-value selection.
	// Every mapped target must also appear in Allowed.
	Map map[string]string
}

type ReasoningEffortRule struct {
	Policy ReasoningEffortPolicy
}

func NewReasoningEffortRule(policy ReasoningEffortPolicy) (*ReasoningEffortRule, error) {
	if len(policy.Allowed) == 0 {
		return nil, fmt.Errorf("reasoning effort policy requires allowed values")
	}
	for from, to := range policy.Map {
		if from == "" || !slices.Contains(policy.Allowed, to) {
			return nil, fmt.Errorf("reasoning effort policy maps %q to unsupported %q", from, to)
		}
	}
	return &ReasoningEffortRule{Policy: policy}, nil
}

func (*ReasoningEffortRule) ID() string { return RuleReasoningEffort }

func (r *ReasoningEffortRule) Apply(body []byte, _ RequestContext) ([]byte, bool, error) {
	if r == nil {
		return body, false, nil
	}
	policy := r.Policy
	data, ok := decodeJSONObject(body)
	if !ok {
		return body, false, nil
	}
	reasoning, ok := data["reasoning"].(map[string]any)
	if !ok {
		return body, false, nil
	}
	effort, ok := reasoning["effort"].(string)
	if !ok || slices.Contains(policy.Allowed, effort) {
		return body, false, nil
	}
	target, ok := policy.Map[effort]
	if !ok {
		target, ok = nearestEffort(effort, policy.Allowed)
	}
	if !ok {
		return body, false, nil
	}
	reasoning["effort"] = target
	next, err := json.Marshal(data)
	if err != nil {
		return body, false, err
	}
	return next, true, nil
}

var effortRank = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

func nearestEffort(requested string, allowed []string) (string, bool) {
	want, ok := effortRank[requested]
	if !ok {
		return "", false
	}
	best := ""
	bestDistance := int(^uint(0) >> 1)
	bestRank := -1
	for _, candidate := range allowed {
		rank, known := effortRank[candidate]
		if !known {
			continue
		}
		distance := rank - want
		if distance < 0 {
			distance = -distance
		}
		// Prefer the stronger effort on a tie. This is deterministic and
		// avoids mapping an active reasoning request to "none".
		if distance < bestDistance || distance == bestDistance && rank > bestRank {
			best = candidate
			bestDistance = distance
			bestRank = rank
		}
	}
	return best, best != ""
}

// StripToolTypes is the explicit emergency denylist helper. Unknown and
// unlisted tool types pass through. It resets tool_choice only when that
// choice references a type actually removed from tools.
func StripToolTypes(body []byte, denylist []string) ([]byte, []string) {
	if len(denylist) == 0 {
		return body, nil
	}
	data, ok := decodeJSONObject(body)
	if !ok {
		return body, nil
	}
	tools, ok := data["tools"].([]any)
	if !ok {
		return body, nil
	}
	deny := make(map[string]struct{}, len(denylist))
	for _, typ := range denylist {
		deny[typ] = struct{}{}
	}
	removed := make(map[string]struct{}, len(deny))
	stripped := make([]string, 0, len(deny))
	kept := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		typ, ok := tool["type"].(string)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		if _, denied := deny[typ]; !denied {
			kept = append(kept, raw)
			continue
		}
		if _, seen := removed[typ]; !seen {
			stripped = append(stripped, typ)
			removed[typ] = struct{}{}
		}
	}
	if len(stripped) == 0 {
		return body, nil
	}
	data["tools"] = kept
	if choice, ok := data["tool_choice"].(map[string]any); ok {
		if typ, ok := choice["type"].(string); ok {
			if _, wasRemoved := removed[typ]; wasRemoved {
				data["tool_choice"] = "auto"
			}
		}
	}
	next, err := json.Marshal(data)
	if err != nil {
		return body, nil
	}
	return next, stripped
}

func sameBytes(a, b []byte) bool {
	return len(a) == len(b) && bytes.Equal(a, b)
}

func decodeJSONObject(body []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var data map[string]any
	if err := decoder.Decode(&data); err != nil || data == nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return data, true
}
