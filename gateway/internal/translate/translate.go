// Package translate converts between the Anthropic Messages API and the
// OpenAI Chat Completions API so an anthropic-shape listener can serve a
// request from an openai-shape upstream (Claude Code on any Baseten model).
//
// Scope (see the translation contract): anthropic request in,
// openai chat.completions upstream, both streaming and non-streaming.
// Thinking blocks are dropped on the way out; image/document content is
// rejected with a clean error rather than mistranslated. The reverse
// direction (openai listener, anthropic upstream) is not implemented.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ErrUnsupportedContent is returned for content blocks that cannot be
// faithfully translated (images, documents). The gateway maps it to 400.
type ErrUnsupportedContent struct{ BlockType string }

func (e ErrUnsupportedContent) Error() string {
	return fmt.Sprintf("content block type %q not supported over cross-shape translation", e.BlockType)
}

// RequestToOpenAI converts an Anthropic Messages request body into an
// OpenAI Chat Completions request body. The model field is copied
// verbatim; callers apply model rewriting afterwards.
func RequestToOpenAI(body []byte) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	out := map[string]any{}
	if v, ok := req["model"]; ok {
		out["model"] = v
	}
	if v, ok := req["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p", "stream"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	if v, ok := req["stop_sequences"]; ok {
		out["stop"] = v
	}
	if stream, _ := req["stream"].(bool); stream {
		// Ask the upstream for a final usage chunk so telemetry and the
		// synthesized message_delta carry real token counts.
		out["stream_options"] = map[string]any{"include_usage": true}
	}

	msgs := []any{}
	if sys, ok := req["system"]; ok {
		if text := systemText(sys); text != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": text})
		}
	}
	inMsgs, _ := req["messages"].([]any)
	for _, m := range inMsgs {
		converted, err := convertMessage(m)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, converted...)
	}
	out["messages"] = msgs

	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		oaTools := make([]any, 0, len(tools))
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fn := map[string]any{"name": tm["name"]}
			if d, ok := tm["description"]; ok {
				fn["description"] = d
			}
			if s, ok := tm["input_schema"]; ok {
				fn["parameters"] = s
			}
			oaTools = append(oaTools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = oaTools
	}
	if tc, ok := req["tool_choice"].(map[string]any); ok {
		switch tc["type"] {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			out["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": tc["name"]},
			}
		}
	}
	return json.Marshal(out)
}

// systemText flattens an anthropic system prompt (string or list of text
// blocks) into one string.
func systemText(sys any) string {
	switch v := sys.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, b := range v {
			if bm, ok := b.(map[string]any); ok && bm["type"] == "text" {
				if t, ok := bm["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// convertMessage maps one anthropic message to one or more openai
// messages. tool_result blocks become role:"tool" messages; the rest of
// a user message's text follows as a plain user message. Assistant
// tool_use blocks become tool_calls.
func convertMessage(m any) ([]any, error) {
	mm, ok := m.(map[string]any)
	if !ok {
		return []any{m}, nil
	}
	role, _ := mm["role"].(string)
	switch content := mm["content"].(type) {
	case string:
		return []any{map[string]any{"role": role, "content": content}}, nil
	case []any:
		if role == "assistant" {
			return convertAssistant(content)
		}
		return convertUser(content)
	}
	return []any{m}, nil
}

func convertAssistant(content []any) ([]any, error) {
	var texts []string
	var toolCalls []any
	for _, b := range content {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			if t, ok := bm["text"].(string); ok {
				texts = append(texts, t)
			}
		case "thinking", "redacted_thinking":
			// Deferred: thinking blocks are dropped on translation.
		case "tool_use":
			args := "{}"
			if in, ok := bm["input"]; ok {
				if ab, err := json.Marshal(in); err == nil {
					args = string(ab)
				}
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   bm["id"],
				"type": "function",
				"function": map[string]any{
					"name":      bm["name"],
					"arguments": args,
				},
			})
		default:
			if bt, _ := bm["type"].(string); bt == "image" || bt == "document" {
				return nil, ErrUnsupportedContent{BlockType: bt}
			}
		}
	}
	out := map[string]any{"role": "assistant"}
	if len(texts) > 0 {
		out["content"] = strings.Join(texts, "")
	} else if len(toolCalls) == 0 {
		out["content"] = ""
	}
	if len(toolCalls) > 0 {
		out["tool_calls"] = toolCalls
		if _, ok := out["content"]; !ok {
			out["content"] = nil
		}
	}
	return []any{out}, nil
}

func convertUser(content []any) ([]any, error) {
	var out []any
	var texts []string
	for _, b := range content {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			if t, ok := bm["text"].(string); ok {
				texts = append(texts, t)
			}
		case "tool_result":
			content, err := toolResultText(bm["content"])
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": bm["tool_use_id"],
				"content":      content,
			})
		case "image", "document":
			bt, _ := bm["type"].(string)
			return nil, ErrUnsupportedContent{BlockType: bt}
		}
	}
	if len(texts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": strings.Join(texts, "\n")})
	}
	return out, nil
}

// toolResultText flattens a tool_result content value (string or list of
// text blocks) into one string for the openai tool message.
func toolResultText(v any) (string, error) {
	switch c := v.(type) {
	case string:
		return c, nil
	case []any:
		var parts []string
		for _, b := range c {
			if bm, ok := b.(map[string]any); ok {
				switch bm["type"] {
				case "text":
					if t, ok := bm["text"].(string); ok {
						parts = append(parts, t)
					}
				case "image", "document":
					blockType, _ := bm["type"].(string)
					return "", ErrUnsupportedContent{BlockType: blockType}
				}
			}
		}
		return strings.Join(parts, "\n"), nil
	}
	return "", nil
}

// StopReason maps an openai finish_reason to an anthropic stop_reason.
func StopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop", "":
		return "end_turn"
	}
	return "end_turn"
}

// openAIUsage is the usage object of a chat.completions response.
type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	PromptDetails    struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u openAIUsage) anthropic() map[string]any {
	return map[string]any{
		"input_tokens":                u.PromptTokens - u.PromptDetails.CachedTokens,
		"output_tokens":               u.CompletionTokens,
		"cache_read_input_tokens":     u.PromptDetails.CachedTokens,
		"cache_creation_input_tokens": 0,
	}
}

// ResponseToAnthropic converts a non-streaming chat.completions response
// body into an Anthropic Messages response body.
func ResponseToAnthropic(body []byte) ([]byte, error) {
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openai response has no choices")
	}
	ch := resp.Choices[0]
	content := []any{}
	if ch.Message.Content != nil && *ch.Message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": *ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		var input any = map[string]any{}
		if tc.Function.Arguments != "" {
			var parsed any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err == nil {
				input = parsed
			}
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Function.Name,
			"input": input,
		})
	}
	out := map[string]any{
		"id":            messageID(resp.ID),
		"type":          "message",
		"role":          "assistant",
		"model":         resp.Model,
		"content":       content,
		"stop_reason":   StopReason(ch.FinishReason),
		"stop_sequence": nil,
		"usage":         resp.Usage.anthropic(),
	}
	return json.Marshal(out)
}

// ErrorToAnthropic converts an openai error body into the anthropic error
// envelope. Unparseable bodies produce a generic api_error with the raw
// text as the message.
func ErrorToAnthropic(status int, body []byte) []byte {
	msg := strings.TrimSpace(string(body))
	errType := "api_error"
	var oa struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &oa); err == nil && oa.Error.Message != "" {
		msg = oa.Error.Message
	}
	switch {
	case status == 429:
		errType = "rate_limit_error"
	case status == 401 || status == 403:
		errType = "authentication_error"
	case status >= 400 && status < 500:
		errType = "invalid_request_error"
	case status >= 500:
		errType = "api_error"
	}
	out, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": msg},
	})
	return out
}

func messageID(id string) string {
	if id == "" {
		return "msg_translated"
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}
