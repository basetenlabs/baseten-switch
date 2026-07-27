// Package requestprofile extracts model-selection metadata from request bodies
// and projects provider-specific execution options for upstreams that do not
// support them.
package requestprofile

import (
	"encoding/json"
	"net/http"
	"strings"
)

const (
	// OneMillionContextTokens is the Claude Code context budget selected by
	// the documented [1m] model decoration.
	OneMillionContextTokens int64 = 1_000_000

	// AnthropicFastBeta is the beta token required by Anthropic fast mode.
	AnthropicFastBeta = "fast-mode-2026-02-01"
)

// Profile is best-effort metadata extracted from an Anthropic-style request.
// A nil RequestedContextBudgetTokens means the request did not expose a known
// context selection. Callers must not infer a default budget from its absence.
type Profile struct {
	RawModel                     string
	CanonicalModel               string
	RequestedContextBudgetTokens *int64
	RequestedSpeed               string
	RequestedSpeedPresent        bool
	RequestedInferenceGeo        string
	RequestedInferenceGeoPresent bool
	RequestedOneHourCache        bool
}

// Inspect extracts the raw and canonical model ids, the exact [1m] Claude Code
// decoration when present, and a string-valued top-level speed. Invalid JSON
// and fields with unexpected types produce a partial or empty profile.
func Inspect(body []byte) Profile {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return Profile{}
	}

	var profile Profile
	if raw, ok := request["model"]; ok {
		_ = json.Unmarshal(raw, &profile.RawModel)
	}
	profile.CanonicalModel = profile.RawModel
	if strings.HasSuffix(profile.RawModel, "[1m]") {
		profile.CanonicalModel = strings.TrimSuffix(profile.RawModel, "[1m]")
		budget := OneMillionContextTokens
		profile.RequestedContextBudgetTokens = &budget
	}
	if raw, ok := request["speed"]; ok {
		profile.RequestedSpeedPresent = true
		_ = json.Unmarshal(raw, &profile.RequestedSpeed)
	}
	if raw, ok := request["inference_geo"]; ok {
		profile.RequestedInferenceGeoPresent = true
		_ = json.Unmarshal(raw, &profile.RequestedInferenceGeo)
	}
	profile.RequestedOneHourCache = containsOneHourCacheControl(request)
	return profile
}

func containsOneHourCacheControl(value any) bool {
	switch typed := value.(type) {
	case map[string]json.RawMessage:
		for key, raw := range typed {
			if key == "cache_control" {
				var control map[string]json.RawMessage
				if json.Unmarshal(raw, &control) == nil {
					var ttl string
					if json.Unmarshal(control["ttl"], &ttl) == nil &&
						ttl == "1h" {
						return true
					}
				}
			}
			var nested any
			if json.Unmarshal(raw, &nested) == nil &&
				containsOneHourCacheControl(nested) {
				return true
			}
		}
	case map[string]any:
		for key, nested := range typed {
			if key == "cache_control" {
				if control, ok := nested.(map[string]any); ok &&
					control["ttl"] == "1h" {
					return true
				}
			}
			if containsOneHourCacheControl(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsOneHourCacheControl(nested) {
				return true
			}
		}
	}
	return false
}

// RemoveUnsupportedFastProfile removes the top-level speed request option and
// Anthropic's fast-mode beta token before forwarding to an upstream adapter
// that has not declared an equivalent fast profile. Other JSON fields, beta
// tokens, and headers are retained. Inputs are never mutated.
//
// Invalid JSON is left untouched. Header projection still runs because it is
// independent of body parsing.
func RemoveUnsupportedFastProfile(body []byte, headers http.Header) ([]byte, http.Header, bool) {
	projectedBody, bodyChanged := removeTopLevelSpeed(body)
	projectedHeaders, headersChanged := removeBetaToken(headers, AnthropicFastBeta)
	return projectedBody, projectedHeaders, bodyChanged || headersChanged
}

func removeTopLevelSpeed(body []byte) ([]byte, bool) {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return body, false
	}
	if _, ok := request["speed"]; !ok {
		return body, false
	}
	delete(request, "speed")
	projected, err := json.Marshal(request)
	if err != nil {
		return body, false
	}
	return projected, true
}

func removeBetaToken(headers http.Header, token string) (http.Header, bool) {
	projected := headers.Clone()
	values := headers.Values("Anthropic-Beta")
	if len(values) == 0 {
		return projected, false
	}

	changed := false
	keptValues := make([]string, 0, len(values))
	for _, value := range values {
		parts := strings.Split(value, ",")
		keptParts := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.EqualFold(part, token) {
				changed = true
				continue
			}
			if part != "" {
				keptParts = append(keptParts, part)
			}
		}
		if len(keptParts) > 0 {
			keptValues = append(keptValues, strings.Join(keptParts, ", "))
		}
	}
	if !changed {
		return projected, false
	}
	projected.Del("Anthropic-Beta")
	for _, value := range keptValues {
		projected.Add("Anthropic-Beta", value)
	}
	return projected, true
}
