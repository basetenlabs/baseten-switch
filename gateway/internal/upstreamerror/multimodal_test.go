package upstreamerror

import (
	"fmt"
	"strings"
	"testing"
)

func TestIsBasetenMultimodalUnsupported400MatchesEndpointEnvelopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind EndpointKind
		body string
	}{
		{
			name: "chat completions",
			kind: EndpointChatCompletions,
			body: fmt.Sprintf(
				`{"message":%q,"type":"Bad Request","code":400,"request_id":"r1"}`,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			name: "messages",
			kind: EndpointMessages,
			body: fmt.Sprintf(
				`{"type":"error","request_id":"r1","error":{"type":"invalid_request_error","message":%q}}`,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			name: "responses",
			kind: EndpointResponses,
			body: fmt.Sprintf(
				"{\n  \"code\": 400,\n  \"unknown\": true,\n  \"message\": %q,\n  \"type\": \"Bad Request\"\n}",
				BasetenMultimodalUnsupportedMessage,
			),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if !IsBasetenMultimodalUnsupported400(test.kind, 400, []byte(test.body)) {
				t.Fatalf("expected match for %s body: %s", test.kind, test.body)
			}
		})
	}
}

func TestIsBasetenMultimodalUnsupported400RejectsNearMisses(t *testing.T) {
	t.Parallel()

	canonicalChat := fmt.Sprintf(
		`{"message":%q,"type":"Bad Request","code":400}`,
		BasetenMultimodalUnsupportedMessage,
	)
	canonicalMessages := fmt.Sprintf(
		`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`,
		BasetenMultimodalUnsupportedMessage,
	)
	canonicalResponses := fmt.Sprintf(
		`{"message":%q,"type":"Bad Request","code":400}`,
		BasetenMultimodalUnsupportedMessage,
	)

	tests := []struct {
		name   string
		kind   EndpointKind
		status int
		body   string
	}{
		{"wrong status", EndpointChatCompletions, 422, canonicalChat},
		{"unknown endpoint", EndpointKind("future"), 400, canonicalChat},
		{"empty", EndpointChatCompletions, 400, ""},
		{"malformed", EndpointChatCompletions, 400, `{"error":`},
		{"trailing JSON", EndpointChatCompletions, 400, canonicalChat + `{}`},
		{"top-level array", EndpointChatCompletions, 400, `[{"error":{"message":"x"}}]`},
		{"top-level scalar", EndpointChatCompletions, 400, `"error"`},
		{
			"chat recursively nested message",
			EndpointChatCompletions,
			400,
			fmt.Sprintf(`{"wrapper":{"error":{"message":%q}}}`, BasetenMultimodalUnsupportedMessage),
		},
		{
			"chat OpenAI error envelope",
			EndpointChatCompletions,
			400,
			fmt.Sprintf(
				`{"error":{"message":%q},"type":"Bad Request","code":400}`,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			"messages wrong envelope type",
			EndpointMessages,
			400,
			strings.Replace(canonicalMessages, `"type":"error"`, `"type":"Error"`, 1),
		},
		{
			"messages missing envelope type",
			EndpointMessages,
			400,
			fmt.Sprintf(
				`{"error":{"type":"invalid_request_error","message":%q}}`,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			"messages missing error type",
			EndpointMessages,
			400,
			fmt.Sprintf(
				`{"type":"error","error":{"message":%q}}`,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			"messages wrong error type",
			EndpointMessages,
			400,
			strings.Replace(
				canonicalMessages,
				`"invalid_request_error"`,
				`"bad_request"`,
				1,
			),
		},
		{
			"responses nested message",
			EndpointResponses,
			400,
			fmt.Sprintf(
				`{"error":{"message":%q},"type":"Bad Request","code":400}`,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			"responses wrong type",
			EndpointResponses,
			400,
			strings.Replace(canonicalResponses, `"Bad Request"`, `"bad request"`, 1),
		},
		{
			"responses string code",
			EndpointResponses,
			400,
			strings.Replace(canonicalResponses, `"code":400`, `"code":"400"`, 1),
		},
		{
			"responses wrong code",
			EndpointResponses,
			400,
			strings.Replace(canonicalResponses, `"code":400`, `"code":422`, 1),
		},
		{
			"message case",
			EndpointChatCompletions,
			400,
			strings.Replace(canonicalChat, "This model", "This Model", 1),
		},
		{
			"message punctuation",
			EndpointChatCompletions,
			400,
			strings.Replace(canonicalChat, "inputs.", "inputs!", 1),
		},
		{
			"message extra whitespace",
			EndpointChatCompletions,
			400,
			strings.Replace(canonicalChat, "This model", " This model", 1),
		},
		{
			"duplicate chat message",
			EndpointChatCompletions,
			400,
			fmt.Sprintf(
				`{"message":%q,"message":%q,"type":"Bad Request","code":400}`,
				BasetenMultimodalUnsupportedMessage,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			"duplicate messages direct message",
			EndpointMessages,
			400,
			fmt.Sprintf(
				`{"type":"error","error":{"type":"invalid_request_error","message":%q,"message":%q}}`,
				BasetenMultimodalUnsupportedMessage,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			"chat missing type and code",
			EndpointChatCompletions,
			400,
			fmt.Sprintf(
				`{"message":%q}`,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			"duplicate messages type",
			EndpointMessages,
			400,
			fmt.Sprintf(
				`{"type":"error","type":"error","error":{"type":"invalid_request_error","message":%q}}`,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			"duplicate messages error type",
			EndpointMessages,
			400,
			fmt.Sprintf(
				`{"type":"error","error":{"type":"invalid_request_error","type":"invalid_request_error","message":%q}}`,
				BasetenMultimodalUnsupportedMessage,
			),
		},
		{
			"duplicate responses code",
			EndpointResponses,
			400,
			fmt.Sprintf(
				`{"message":%q,"type":"Bad Request","code":400,"code":400}`,
				BasetenMultimodalUnsupportedMessage,
			),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if IsBasetenMultimodalUnsupported400(test.kind, test.status, []byte(test.body)) {
				t.Fatalf("unexpected match for %s body: %s", test.kind, test.body)
			}
		})
	}
}

func TestIsBasetenMultimodalUnsupported400RejectsOversizedBody(t *testing.T) {
	t.Parallel()

	base := fmt.Sprintf(
		`{"message":%q,"type":"Bad Request","code":400}`,
		BasetenMultimodalUnsupportedMessage,
	)
	atLimit := base + strings.Repeat(" ", MaxClassifierBodyBytes-len(base))
	if len(atLimit) != MaxClassifierBodyBytes {
		t.Fatalf("at-limit body length = %d, want %d", len(atLimit), MaxClassifierBodyBytes)
	}
	if !IsBasetenMultimodalUnsupported400(
		EndpointChatCompletions,
		400,
		[]byte(atLimit),
	) {
		t.Fatal("complete body at size limit did not match")
	}

	body := fmt.Sprintf(
		`{"message":%q,"type":"Bad Request","code":400,"padding":%q}`,
		BasetenMultimodalUnsupportedMessage,
		strings.Repeat("x", MaxClassifierBodyBytes),
	)
	if len(body) <= MaxClassifierBodyBytes {
		t.Fatalf("test body length = %d, want over %d", len(body), MaxClassifierBodyBytes)
	}
	if IsBasetenMultimodalUnsupported400(
		EndpointChatCompletions,
		400,
		[]byte(body),
	) {
		t.Fatal("oversized body unexpectedly matched")
	}
}
