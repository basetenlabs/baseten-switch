package requestcapability

import (
	"runtime"
	"strings"
	"testing"
)

var allocationInspectionSink Inspection

func TestInspectDetectsSupportedImageLocations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint Endpoint
		body     string
	}{
		{
			name:     "anthropic user image",
			endpoint: AnthropicMessages,
			body:     `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"not-decoded"}}]}]}`,
		},
		{
			name:     "anthropic direct tool result child",
			endpoint: AnthropicMessages,
			body:     `{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}]}]}]}`,
		},
		{
			name:     "openai chat user image URL",
			endpoint: OpenAIChat,
			body:     `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,not-decoded"}}]}]}`,
		},
		{
			name:     "responses easy input message",
			endpoint: OpenAIResponses,
			body:     `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/image.png"}]}]}`,
		},
		{
			name:     "responses explicit input message",
			endpoint: OpenAIResponses,
			body:     `{"input":[{"type":"message","role":"developer","content":[{"type":"input_image","file_id":"file_123"}]}]}`,
		},
		{
			name:     "responses function call output",
			endpoint: OpenAIResponses,
			body:     `{"input":[{"type":"function_call_output","call_id":"call_123","output":[{"type":"input_image","image_url":"https://example.invalid/image.png"}]}]}`,
		},
		{
			name:     "responses custom tool call output",
			endpoint: OpenAIResponses,
			body:     `{"input":[{"type":"custom_tool_call_output","call_id":"call_123","output":[{"type":"input_image","file_id":"file_123"}]}]}`,
		},
		{
			name:     "responses computer screenshot",
			endpoint: OpenAIResponses,
			body:     `{"input":[{"type":"computer_call_output","call_id":"call_123","output":{"type":"computer_screenshot","image_url":"https://example.invalid/image.png"}}]}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Inspect(tt.endpoint, []byte(tt.body))
			if got.Malformed || !got.HasImage {
				t.Fatalf("Inspect() = %+v, want HasImage true and Malformed false", got)
			}
		})
	}
}

func TestInspectIgnoresUnsupportedAndNestedImageMarkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint Endpoint
		body     string
	}{
		{
			name:     "anthropic assistant image",
			endpoint: AnthropicMessages,
			body:     `{"messages":[{"role":"assistant","content":[{"type":"image"}]}]}`,
		},
		{
			name:     "anthropic nested tool result image",
			endpoint: AnthropicMessages,
			body:     `{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"tool_result","content":[{"type":"image"}]}]}]}]}`,
		},
		{
			name:     "anthropic tool schema marker",
			endpoint: AnthropicMessages,
			body:     `{"messages":[{"role":"user","content":"text"}],"tools":[{"input_schema":{"properties":{"format":{"const":"image"}}}}]}`,
		},
		{
			name:     "openai chat assistant image",
			endpoint: OpenAIChat,
			body:     `{"messages":[{"role":"assistant","content":[{"type":"image_url"}]}]}`,
		},
		{
			name:     "openai chat tool schema marker",
			endpoint: OpenAIChat,
			body:     `{"messages":[{"role":"user","content":"text"}],"tools":[{"function":{"parameters":{"properties":{"part":{"const":"image_url"}}}}}]}`,
		},
		{
			name:     "responses message missing role",
			endpoint: OpenAIResponses,
			body:     `{"input":[{"content":[{"type":"input_image"}]}]}`,
		},
		{
			name:     "responses arbitrary nested marker",
			endpoint: OpenAIResponses,
			body:     `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","metadata":{"type":"input_image"}}]}]}`,
		},
		{
			name:     "responses nested function output marker",
			endpoint: OpenAIResponses,
			body:     `{"input":[{"type":"function_call_output","output":[{"type":"output_text","value":{"type":"input_image"}}]}]}`,
		},
		{
			name:     "responses nested computer screenshot marker",
			endpoint: OpenAIResponses,
			body:     `{"input":[{"type":"computer_call_output","output":{"type":"wrapper","value":{"type":"computer_screenshot"}}}]}`,
		},
		{
			name:     "responses image generation item",
			endpoint: OpenAIResponses,
			body:     `{"input":[{"type":"image_generation_call","result":{"type":"input_image"}}],"include":["message.input_image.image_url"]}`,
		},
		{
			name:     "unknown endpoint",
			endpoint: Endpoint("unknown"),
			body:     `{"messages":[{"role":"user","content":[{"type":"image"}]}]}`,
		},
		{
			name:     "valid non-object",
			endpoint: AnthropicMessages,
			body:     `[{"type":"image"}]`,
		},
		{
			name:     "valid null",
			endpoint: OpenAIResponses,
			body:     `null`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Inspect(tt.endpoint, []byte(tt.body))
			if got.Malformed || got.HasImage {
				t.Fatalf("Inspect() = %+v, want HasImage false and Malformed false", got)
			}
		})
	}
}

func TestInspectReportsMalformedJSONWithoutImage(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []Endpoint{AnthropicMessages, OpenAIChat, OpenAIResponses} {
		endpoint := endpoint
		t.Run(string(endpoint), func(t *testing.T) {
			t.Parallel()
			got := Inspect(endpoint, []byte(`{"type":"image"`))
			if !got.Malformed || got.HasImage {
				t.Fatalf("Inspect() = %+v, want HasImage false and Malformed true", got)
			}
		})
	}
}

func TestInspectResponsesReportsProviderOwnedState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		stateful bool
	}{
		{"previous response", `{"previous_response_id":"resp_123"}`, true},
		{"conversation id", `{"conversation":"conv_123"}`, true},
		{"conversation object", `{"conversation":{"id":"conv_123"}}`, true},
		{"empty previous response", `{"previous_response_id":""}`, false},
		{"null conversation", `{"conversation":null}`, false},
		{"stateless", `{"input":"hello"}`, false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := Inspect(OpenAIResponses, []byte(test.body))
			if got.Malformed || got.Stateful != test.stateful {
				t.Fatalf(
					"Inspect() = %+v, want Stateful %t and Malformed false",
					got,
					test.stateful,
				)
			}
		})
	}
}

func TestInspectAllocationDoesNotScaleWithInlineBase64Payload(t *testing.T) {
	smallBody := anthropicBase64Body(strings.Repeat("a", 16))
	largeBody := anthropicBase64Body(strings.Repeat("a", 4<<20))

	for name, body := range map[string][]byte{"small": smallBody, "large": largeBody} {
		got := Inspect(AnthropicMessages, body)
		if got.Malformed || !got.HasImage {
			t.Fatalf("%s request Inspect() = %+v, want image detection", name, got)
		}
	}

	smallBytes := allocatedBytesPerInspect(AnthropicMessages, smallBody)
	largeBytes := allocatedBytesPerInspect(AnthropicMessages, largeBody)
	t.Logf("allocated bytes per inspection: small=%d large=%d", smallBytes, largeBytes)

	const allocationSlack = int64(4 << 10)
	if largeBytes > smallBytes+allocationSlack {
		t.Fatalf(
			"large inline payload allocated %d bytes/op, small payload allocated %d bytes/op; want growth <= %d",
			largeBytes,
			smallBytes,
			allocationSlack,
		)
	}
}

func anthropicBase64Body(payload string) []byte {
	return []byte(
		`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
			payload +
			`"}}]}]}`,
	)
}

func allocatedBytesPerInspect(endpoint Endpoint, body []byte) int64 {
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			allocationInspectionSink = Inspect(endpoint, body)
		}
		runtime.KeepAlive(body)
	})
	return result.AllocedBytesPerOp()
}
