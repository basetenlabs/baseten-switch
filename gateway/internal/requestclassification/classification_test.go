package requestclassification

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestClassifyClaudeMessagesPositive(t *testing.T) {
	tests := []struct {
		name   string
		system any
	}{
		{
			name: "single text block",
			system: []any{map[string]any{
				"type": "text",
				"text": "In Auto mode, classify whether the proposed tool call has permission to run safely.",
			}},
		},
		{
			name: "concepts span text blocks",
			system: []any{
				map[string]any{"type": "text", "text": "AUTO\u2003MODE permission"},
				map[string]any{"type": "metadata", "text": 42},
				map[string]any{"type": "text", "text": "classification of an action"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := messageBody(t, false, tc.system, "ordinary user text")
			got := ClassifyClaudeMessages(validContext(), body)
			if got == nil {
				t.Fatal("classification = nil, want positive")
			}
			want := Classification{
				Kind:          KindClaudeAutoPermissionCheck,
				Detector:      DetectorClaudeAutoV1,
				RoutingAction: RoutingActionNativeAnthropic,
			}
			if *got != want {
				t.Fatalf("classification = %+v, want %+v", *got, want)
			}
		})
	}

	withoutStream := messageBodyWithoutStream(
		t,
		textSystem("Auto mode permission classifier for a proposed tool use."),
		"Review the proposed operation.",
	)
	if got := ClassifyClaudeMessages(validContext(), withoutStream); got == nil {
		t.Fatal("classification with omitted stream = nil, want positive")
	}
}

func TestClassifyClaudeMessagesRequiresFullPredicate(t *testing.T) {
	matchingSystem := []any{map[string]any{
		"type": "text",
		"text": "Auto mode permission classifier for a proposed tool use.",
	}}
	streamTrue := true
	streamFalse := false
	tests := []struct {
		name string
		ctx  RequestContext
		body []byte
	}{
		{"non Claude listener", RequestContext{Method: http.MethodPost, Path: "/v1/messages"}, messageBody(t, false, matchingSystem, "hello")},
		{"wrong method", RequestContext{ClaudeListener: true, Method: http.MethodPut, Path: "/v1/messages"}, messageBody(t, false, matchingSystem, "hello")},
		{"wrong path", RequestContext{ClaudeListener: true, Method: http.MethodPost, Path: "/v1/messages/count_tokens"}, messageBody(t, false, matchingSystem, "hello")},
		{"invalid JSON", validContext(), []byte(`{"stream":false`)},
		{"stream true", validContext(), envelopeBody(t, &streamTrue, matchingSystem)},
		{"system string", validContext(), envelopeBody(t, &streamFalse, "Auto mode permission classifier for a tool call")},
		{"empty system", validContext(), envelopeBody(t, &streamFalse, []any{})},
		{"no text blocks", validContext(), envelopeBody(t, &streamFalse, []any{map[string]any{"type": "metadata", "text": "Auto mode permission classifier tool call"}})},
		{"malformed text block", validContext(), envelopeBody(t, &streamFalse, []any{map[string]any{"type": "text", "text": 42}, map[string]any{"type": "text", "text": "Auto mode permission classifier tool call"}})},
		{"missing auto mode", validContext(), messageBody(t, false, textSystem("Permission classifier for a tool call."), "hello")},
		{"missing safety", validContext(), messageBody(t, false, textSystem("Auto mode classifier for a tool call."), "hello")},
		{"missing classifier", validContext(), messageBody(t, false, textSystem("Auto mode checks permission for a tool call."), "hello")},
		{"missing action", validContext(), messageBody(t, false, textSystem("Auto mode permission classifier."), "hello")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyClaudeMessages(tc.ctx, tc.body); got != nil {
				t.Fatalf("classification = %+v, want nil", *got)
			}
		})
	}
}

func TestClassifyClaudeMessagesIgnoresUserContent(t *testing.T) {
	body := messageBody(
		t,
		false,
		textSystem("You are a general coding assistant."),
		"Auto mode permission classifier for a proposed tool call.",
	)
	if got := ClassifyClaudeMessages(validContext(), body); got != nil {
		t.Fatalf("classification = %+v, want nil", *got)
	}
}

func TestClassifyClaudeMessagesNegativeRequestFamilies(t *testing.T) {
	matchingSystem := textSystem("In Auto mode, classify whether a proposed tool call has permission to run safely.")
	tests := []struct {
		name       string
		ctx        RequestContext
		stream     bool
		omitStream bool
		system     any
		user       string
	}{
		{
			name:   "ordinary streaming turn",
			ctx:    validContext(),
			stream: true,
			system: textSystem("You are a coding assistant. Help with the requested task."),
			user:   "Explain this function.",
		},
		{
			name:       "ordinary nonstreaming turn",
			ctx:        validContext(),
			omitStream: true,
			system:     textSystem("You are a coding assistant. Answer concisely."),
			user:       "Name two testing strategies.",
		},
		{
			name:       "title generation",
			ctx:        validContext(),
			omitStream: true,
			system:     textSystem("Generate a short title for the conversation."),
			user:       "We discussed improving a command-line interface.",
		},
		{
			name:       "summarization and compaction",
			ctx:        validContext(),
			omitStream: true,
			system:     textSystem("Summarize the conversation into a compact continuation note."),
			user:       "Keep the decisions and unresolved questions.",
		},
		{
			name:       "tool search",
			ctx:        validContext(),
			omitStream: true,
			system:     textSystem("Find tools relevant to the requested operation."),
			user:       "Look for a source-code search tool.",
		},
		{
			name:       "subagent turn",
			ctx:        validContext(),
			omitStream: true,
			system:     textSystem("You are a focused subagent investigating one bounded task."),
			user:       "Review the parser tests.",
		},
		{
			name: "token counting endpoint",
			ctx: RequestContext{
				ClaudeListener: true,
				Method:         http.MethodPost,
				Path:           "/v1/messages/count_tokens",
			},
			system: matchingSystem,
			user:   "Count these tokens.",
		},
		{
			name:   "classifier-like non-Auto system text",
			ctx:    validContext(),
			system: textSystem("Classify whether a tool call has permission to run safely."),
			user:   "Review the proposed operation.",
		},
		{
			name:   "classifier language only in user content",
			ctx:    validContext(),
			system: textSystem("You are a general coding assistant."),
			user:   "In Auto mode, classify whether a proposed tool call has permission to run safely.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := messageBody(t, tc.stream, tc.system, tc.user)
			if tc.omitStream {
				body = messageBodyWithoutStream(t, tc.system, tc.user)
			}
			if got := ClassifyClaudeMessages(tc.ctx, body); got != nil {
				t.Fatalf("classification = %+v, want nil", *got)
			}
		})
	}
}

func TestClassifyClaudeMessagesSystemTextLimit(t *testing.T) {
	matching := "Auto mode permission classifier for a tool call."
	atLimit := matching + strings.Repeat("x", maxSystemTextBytes-len(matching))
	if got := ClassifyClaudeMessages(
		validContext(),
		messageBody(t, false, textSystem(atLimit), "hello"),
	); got == nil {
		t.Fatal("classification at limit = nil, want positive")
	}
	overLimit := atLimit + "x"
	if got := ClassifyClaudeMessages(
		validContext(),
		messageBody(t, false, textSystem(overLimit), "hello"),
	); got != nil {
		t.Fatalf("classification over limit = %+v, want nil", *got)
	}
}

func validContext() RequestContext {
	return RequestContext{
		ClaudeListener: true,
		Method:         http.MethodPost,
		Path:           "/v1/messages",
	}
}

func textSystem(text string) []any {
	return []any{map[string]any{"type": "text", "text": text}}
}

func messageBody(t *testing.T, stream bool, system any, user string) []byte {
	t.Helper()
	body := map[string]any{
		"model":  "claude-example-model",
		"stream": stream,
		"system": system,
		"messages": []any{map[string]any{
			"role": "user", "content": user,
		}},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func messageBodyWithoutStream(t *testing.T, system any, user string) []byte {
	t.Helper()
	body := map[string]any{
		"model":  "claude-example-model",
		"system": system,
		"messages": []any{map[string]any{
			"role": "user", "content": user,
		}},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func envelopeBody(t *testing.T, stream *bool, system any) []byte {
	t.Helper()
	body := map[string]any{"system": system}
	if stream != nil {
		body["stream"] = *stream
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
