// Package requestclassification identifies request purposes that require
// gateway-owned compatibility routing. It returns only content-free metadata.
package requestclassification

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"unicode"
)

const (
	KindClaudeAutoPermissionCheck = "claude_auto_permission_check"
	DetectorClaudeAutoV1          = "claude_auto_v1"
	RoutingActionNativeAnthropic  = "native_anthropic"

	maxSystemTextBytes = 128 << 10
)

// RequestContext contains the request facts that are safe and necessary for
// classification. Path excludes the query string.
type RequestContext struct {
	ClaudeListener bool
	Method         string
	Path           string
}

// Classification is the complete binary result retained by the gateway. It
// deliberately excludes matched text, excerpts, hashes, and confidence.
type Classification struct {
	Kind          string `json:"kind"`
	Detector      string `json:"detector"`
	RoutingAction string `json:"routing_action"`
}

type messagesEnvelope struct {
	Stream *bool           `json:"stream"`
	System json.RawMessage `json:"system"`
}

// ClassifyClaudeMessages returns a positive result only for the complete v1
// Auto permission-check signature. Unsupported or uncertain input is left
// unclassified so it follows the gateway's ordinary routing policy.
func ClassifyClaudeMessages(ctx RequestContext, body []byte) *Classification {
	if !ctx.ClaudeListener || ctx.Method != http.MethodPost || ctx.Path != "/v1/messages" {
		return nil
	}

	var envelope messagesEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil ||
		(envelope.Stream != nil && *envelope.Stream) || len(envelope.System) == 0 {
		return nil
	}

	systemText, ok := concatenateSystemText(envelope.System)
	if !ok {
		return nil
	}
	normalized := normalizeSystemText(systemText)
	if !containsAllConcepts(normalized) {
		return nil
	}

	return &Classification{
		Kind:          KindClaudeAutoPermissionCheck,
		Detector:      DetectorClaudeAutoV1,
		RoutingAction: RoutingActionNativeAnthropic,
	}
}

func concatenateSystemText(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return "", false
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(trimmed, &blocks); err != nil || len(blocks) == 0 {
		return "", false
	}

	var text strings.Builder
	textBlocks := 0
	for _, rawBlock := range blocks {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rawBlock, &header); err != nil || header.Type != "text" {
			continue
		}
		var block struct {
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(rawBlock, &block); err != nil || block.Text == nil {
			return "", false
		}
		additional := len(*block.Text)
		if textBlocks > 0 {
			additional++
		}
		if text.Len()+additional > maxSystemTextBytes {
			return "", false
		}
		if textBlocks > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(*block.Text)
		textBlocks++
	}
	if textBlocks == 0 {
		return "", false
	}
	return text.String(), true
}

func normalizeSystemText(text string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(text), unicode.IsSpace), " ")
}

func containsAllConcepts(text string) bool {
	return strings.Contains(text, "auto mode") &&
		containsAny(text, "permission", "safe", "safety") &&
		containsAny(text, "classify", "classifier", "classification") &&
		containsAny(text, "tool call", "tool use", "tool invocation", "action")
}

func containsAny(text string, terms ...string) bool {
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}
