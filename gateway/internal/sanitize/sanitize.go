// Package sanitize repairs Anthropic Messages request bodies that carry
// conversation history a native Anthropic upstream would reject with a 400.
// Coding harnesses (Claude Code in particular) replay prior assistant turns
// verbatim, including empty text blocks and, after a mid-session route flip
// through an OpenAI-shape provider, tool ids that violate Anthropic's
// ^[a-zA-Z0-9_-]+$ pattern.
package sanitize

import (
	"encoding/json"
	"regexp"
	"strings"
)

// thoughtSignatureSeparator marks Gemini thought-signature suffixes that
// some providers append to tool ids; everything from the separator on is
// dropped before normalization.
const thoughtSignatureSeparator = "__thought__"

var invalidIDChar = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// AnthropicBody applies both sanitizers to a raw Anthropic Messages request
// body. It returns the (possibly rewritten) body and whether anything
// changed. Bodies that fail to parse, or whose "messages" field is not an
// array, are returned untouched.
func AnthropicBody(body []byte) ([]byte, bool) {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return body, false
	}
	msgs, ok := data["messages"].([]any)
	if !ok {
		return body, false
	}
	out, changed := StripEmptyTextBlocks(msgs)
	out, changedIDs := SanitizeToolUseIDs(out)
	if !changed && !changedIDs {
		return body, false
	}
	data["messages"] = out
	nb, err := json.Marshal(data)
	if err != nil {
		return body, false
	}
	return nb, true
}

// StripEmptyTextBlocks returns a message list with empty or whitespace-only
// {"type":"text"} content blocks removed. Anthropic rejects such blocks
// ("text content blocks must be non-empty"), but assistant turns routinely
// contain them alongside tool_use blocks and get looped back as history.
// A message whose content list empties out is dropped entirely; messages
// whose content is not an array pass through untouched.
func StripEmptyTextBlocks(messages []any) ([]any, bool) {
	out := make([]any, 0, len(messages))
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			out = append(out, m)
			continue
		}
		filtered := make([]any, 0, len(content))
		for _, b := range content {
			if !isEmptyTextBlock(b) {
				filtered = append(filtered, b)
			}
		}
		switch {
		case len(filtered) == len(content):
			out = append(out, m)
		case len(filtered) > 0:
			nm := shallowCopy(msg)
			nm["content"] = filtered
			out = append(out, nm)
			changed = true
		default:
			changed = true // message dropped
		}
	}
	return out, changed
}

func isEmptyTextBlock(block any) bool {
	b, ok := block.(map[string]any)
	if !ok || b["type"] != "text" {
		return false
	}
	text, ok := b["text"].(string)
	return !ok || strings.TrimSpace(text) == ""
}

// SanitizeToolUseIDs rewrites tool_use / server_tool_use "id" and
// tool_result "tool_use_id" values to satisfy Anthropic's
// ^[a-zA-Z0-9_-]+$ requirement. History replayed from an OpenAI-shape
// provider can carry ids like "functions.Bash:0" that the upstream
// accepted but Anthropic rejects.
func SanitizeToolUseIDs(messages []any) ([]any, bool) {
	out := make([]any, 0, len(messages))
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			out = append(out, m)
			continue
		}
		newContent := make([]any, len(content))
		blockChanged := false
		for i, b := range content {
			nb, c := sanitizeToolIDBlock(b)
			newContent[i] = nb
			blockChanged = blockChanged || c
		}
		if blockChanged {
			nm := shallowCopy(msg)
			nm["content"] = newContent
			out = append(out, nm)
			changed = true
		} else {
			out = append(out, m)
		}
	}
	return out, changed
}

func sanitizeToolIDBlock(block any) (any, bool) {
	b, ok := block.(map[string]any)
	if !ok {
		return block, false
	}
	var key string
	switch b["type"] {
	case "tool_use", "server_tool_use":
		key = "id"
	case "tool_result":
		key = "tool_use_id"
	default:
		return block, false
	}
	raw, ok := b[key].(string)
	if !ok {
		return block, false
	}
	normalized := NormalizeToolUseID(raw)
	if normalized == raw {
		return block, false
	}
	nb := shallowCopy(b)
	nb[key] = normalized
	return nb, true
}

// NormalizeToolUseID strips a Gemini thought-signature suffix, then replaces
// any character outside [a-zA-Z0-9_-] with "_". An id that normalizes to
// empty becomes "tool_use_id".
func NormalizeToolUseID(raw string) string {
	base := raw
	if i := strings.Index(raw, thoughtSignatureSeparator); i >= 0 {
		base = raw[:i]
	}
	s := invalidIDChar.ReplaceAllString(base, "_")
	if s == "" {
		return "tool_use_id"
	}
	return s
}

func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
