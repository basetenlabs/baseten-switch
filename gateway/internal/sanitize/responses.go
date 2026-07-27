package sanitize

import "encoding/json"

// ResponsesStripToolTypes drops tools[] entries whose "type" is in types
// from an OpenAI Responses API request body, and resets an object-form
// tool_choice to "auto" when it references a stripped type (string forms
// like "auto" pass untouched). It returns the rewritten body and the
// stripped type names, deduplicated in body order, so telemetry can name
// them. When nothing changes, or the body does not parse as a JSON object,
// the original bytes are returned with a nil list.
func ResponsesStripToolTypes(body []byte, types []string) ([]byte, []string) {
	if len(types) == 0 {
		return body, nil
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return body, nil
	}
	stripped := ResponsesStripToolTypesMap(data, types)
	if stripped == nil {
		return body, nil
	}
	nb, err := json.Marshal(data)
	if err != nil {
		return body, nil
	}
	return nb, stripped
}

// ResponsesStripToolTypesMap is the map-level form of
// ResponsesStripToolTypes for callers that already hold a decoded body and
// share one decode across several rewrites. It mutates data in place and
// returns the stripped type names, or nil when nothing changed (data is
// then left unmodified).
func ResponsesStripToolTypesMap(data map[string]any, types []string) []string {
	if len(types) == 0 {
		return nil
	}
	tools, ok := data["tools"].([]any)
	if !ok {
		return nil
	}
	deny := make(map[string]bool, len(types))
	for _, t := range types {
		deny[t] = true
	}
	kept := make([]any, 0, len(tools))
	removed := make(map[string]bool, len(types))
	var stripped []string
	for _, tool := range tools {
		if m, ok := tool.(map[string]any); ok {
			if typ, ok := m["type"].(string); ok && deny[typ] {
				if !removed[typ] {
					removed[typ] = true
					stripped = append(stripped, typ)
				}
				continue
			}
		}
		kept = append(kept, tool)
	}
	if len(stripped) == 0 {
		return nil
	}
	data["tools"] = kept
	if tc, ok := data["tool_choice"].(map[string]any); ok {
		if typ, ok := tc["type"].(string); ok && removed[typ] {
			data["tool_choice"] = "auto"
		}
	}
	return stripped
}
