package translate

import (
	"encoding/json"
	"fmt"
)

// PrepareBasetenToolSchemas makes translated OpenAI tool schemas acceptable to
// Baseten's current JSON Schema validator. JSON Schema patterns use the
// ECMAScript regex dialect, where Unicode property escapes such as \p{Cc} are
// valid. Baseten's validator currently checks those patterns with Python's
// stdlib regex compiler, which rejects them before inference.
//
// Preserve the constraint as model-visible guidance, but remove the pattern
// keyword that triggers the false rejection. Scope this compatibility rewrite
// to tools[].function.parameters; prompts and unrelated request fields must not
// be searched or changed.
func PrepareBasetenToolSchemas(body []byte) ([]byte, bool, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, fmt.Errorf("parse translated OpenAI request: %w", err)
	}
	tools, ok := req["tools"].([]any)
	if !ok {
		return body, false, nil
	}

	changed := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		parameters, ok := function["parameters"].(map[string]any)
		if !ok {
			continue
		}
		if downgradeUnicodePropertyPatterns(parameters) {
			changed = true
		}
	}
	if !changed {
		return body, false, nil
	}
	out, err := json.Marshal(req)
	if err != nil {
		return nil, false, fmt.Errorf("encode Baseten-compatible OpenAI request: %w", err)
	}
	return out, true, nil
}

func downgradeUnicodePropertyPatterns(value any) bool {
	changed := false
	switch node := value.(type) {
	case map[string]any:
		if pattern, ok := node["pattern"].(string); ok && hasUnicodePropertyEscape(pattern) {
			delete(node, "pattern")
			guidance := "ECMAScript pattern constraint: " + pattern
			if description, ok := node["description"].(string); ok && description != "" {
				node["description"] = description + "\n" + guidance
			} else {
				node["description"] = guidance
			}
			changed = true
		}
		for _, child := range node {
			if downgradeUnicodePropertyPatterns(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range node {
			if downgradeUnicodePropertyPatterns(child) {
				changed = true
			}
		}
	}
	return changed
}

func hasUnicodePropertyEscape(pattern string) bool {
	for i := 1; i+1 < len(pattern); i++ {
		if (pattern[i] != 'p' && pattern[i] != 'P') || pattern[i+1] != '{' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && pattern[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			return true
		}
	}
	return false
}
