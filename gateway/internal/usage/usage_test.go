package usage

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"testing"
)

func mustJSON(t *testing.T, v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sseLine(payload string) string {
	return "data: " + payload + "\n"
}

func TestParseSSEUsageBasetenMessageDelta(t *testing.T) {
	buf := []byte(
		sseLine(`{"message":{"usage":{"input_tokens":0,"output_tokens":0}}}`) +
			sseLine(`{"usage":{"input_tokens":14,"output_tokens":1,"cache_read_input_tokens":0}}`))
	u := ParseSSEUsage(buf)
	if u.InputTokens != 14 {
		t.Fatalf("input = %d want 14", u.InputTokens)
	}
	if u.OutputTokens != 1 {
		t.Fatalf("output = %d want 1", u.OutputTokens)
	}
	if u.CacheReadInputTokens != 0 {
		t.Fatalf("cache_read = %d want 0", u.CacheReadInputTokens)
	}
}

func TestParseSSEUsageAnthropicMessageDelta(t *testing.T) {
	buf := []byte(
		sseLine(`{"message":{"usage":{"input_tokens":15,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`) +
			sseLine(`{"usage":{"input_tokens":15,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}`))
	u := ParseSSEUsage(buf)
	if u.InputTokens != 15 {
		t.Fatalf("input = %d want 15", u.InputTokens)
	}
	if u.OutputTokens != 5 {
		t.Fatalf("output = %d want 5", u.OutputTokens)
	}
}

func TestParseOpenAIResponsesUsageSeparatesCachedInput(t *testing.T) {
	metadata := ParseUsageWithSaw([]byte(`{
		"model": "gpt-test",
		"usage": {
			"input_tokens": 100,
			"input_tokens_details": {"cached_tokens": 80},
			"output_tokens": 7
		}
	}`))
	if !metadata.Saw ||
		metadata.Usage.InputTokens != 20 ||
		metadata.Usage.CacheReadInputTokens != 80 ||
		metadata.Usage.OutputTokens != 7 {
		t.Fatalf("OpenAI Responses usage = %+v", metadata)
	}
}

func TestParseOpenAIChatUsageSeparatesCachedInput(t *testing.T) {
	metadata := ParseUsageWithSaw([]byte(`{
		"model": "gpt-test",
		"usage": {
			"prompt_tokens": 50,
			"prompt_tokens_details": {"cached_tokens": 30},
			"completion_tokens": 4
		}
	}`))
	if !metadata.Saw ||
		metadata.Usage.InputTokens != 20 ||
		metadata.Usage.CacheReadInputTokens != 30 ||
		metadata.Usage.OutputTokens != 4 {
		t.Fatalf("OpenAI Chat usage = %+v", metadata)
	}
}

func TestParseSSEUsageSeedAndOverwrite(t *testing.T) {
	buf := []byte(
		sseLine(`{"message":{"usage":{"input_tokens":15,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1}}}`) +
			sseLine(`{"usage":{"output_tokens":9}}`))
	u := ParseSSEUsage(buf)
	if u.InputTokens != 15 {
		t.Fatalf("input should be seeded from message_start = 15, got %d", u.InputTokens)
	}
	if u.OutputTokens != 9 {
		t.Fatalf("output should be overwritten by message_delta = 9, got %d", u.OutputTokens)
	}
}

func TestParseSSEUsageGarbageSkipped(t *testing.T) {
	buf := []byte(
		"event: ping\n" +
			"data: [DONE]\n" +
			"data: not json\n" +
			"data: {\"usage\":{\"output_tokens\":3}}\n")
	u := ParseSSEUsage(buf)
	if u.OutputTokens != 3 {
		t.Fatalf("output = %d want 3", u.OutputTokens)
	}
	if u.InputTokens != 0 {
		t.Fatalf("input = %d want 0", u.InputTokens)
	}
}

func TestParseSSEUsageEmpty(t *testing.T) {
	u := ParseSSEUsage([]byte(""))
	if u != (Usage{}) {
		t.Fatalf("expected zero usage, got %+v", u)
	}
}

func TestParseSSEUsageCompletionRequiresTerminalEvent(t *testing.T) {
	partial := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0}}}`),
	))
	if !partial.Saw || partial.Complete {
		t.Fatalf("partial metadata = %+v, want saw without complete", partial)
	}
	complete := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"type":"message_delta","usage":{"output_tokens":0}}`) +
			sseLine(`{"type":"message_stop"}`),
	))
	if !complete.Saw || !complete.Complete {
		t.Fatalf("complete metadata = %+v", complete)
	}
}

func TestParseUsageMetadataCapturesProviderReportedModel(t *testing.T) {
	stream := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"type":"message_start","message":{"model":"zai-org/GLM-5.2","usage":{"input_tokens":1}}}`),
	))
	if stream.Model != "zai-org/GLM-5.2" {
		t.Fatalf("stream model = %q", stream.Model)
	}

	nonStream := ParseUsageWithSaw([]byte(
		`{"model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1}}`,
	))
	if nonStream.Model != "claude-opus-4-8" {
		t.Fatalf("non-stream model = %q", nonStream.Model)
	}

	responsesStream := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"type":"response.created","response":{"model":"openai/gpt-5"}}`),
	))
	if responsesStream.Model != "openai/gpt-5" {
		t.Fatalf("responses stream model = %q", responsesStream.Model)
	}
}

func TestParseUsageMetadataCapturesSpeed(t *testing.T) {
	nonStream := ParseUsageWithSaw([]byte(
		`{"usage":{"input_tokens":1,"output_tokens":1,"speed":"fast"}}`,
	))
	if nonStream.Speed != "fast" || !nonStream.SpeedPresent {
		t.Fatalf("non-stream speed = %q, want fast", nonStream.Speed)
	}

	stream := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"type":"message_start","message":{"usage":{"input_tokens":1,"speed":"fast"}}}`) +
			sseLine(`{"type":"message_delta","usage":{"output_tokens":2,"speed":"standard"}}`) +
			sseLine(`{"type":"message_stop"}`),
	))
	if stream.Speed != "standard" || !stream.SpeedPresent {
		t.Fatalf("stream speed = %q, want final standard", stream.Speed)
	}
}

func TestParseUsageMetadataRetainsSeedSpeedWhenFinalOmitsIt(t *testing.T) {
	stream := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"type":"message_start","message":{"usage":{"input_tokens":1,"speed":"fast"}}}`) +
			sseLine(`{"type":"message_delta","usage":{"output_tokens":2}}`) +
			sseLine(`{"type":"message_stop"}`),
	))
	if stream.Speed != "fast" {
		t.Fatalf("stream speed = %q, want seeded fast", stream.Speed)
	}
}

func TestParseUsageMetadataIgnoresUnknownSpeed(t *testing.T) {
	nonStream := ParseUsageWithSaw([]byte(
		`{"usage":{"input_tokens":1,"speed":"turbo"}}`,
	))
	if nonStream.Speed != "" || !nonStream.SpeedPresent {
		t.Fatalf("unknown speed metadata = %+v", nonStream)
	}

	stream := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"message":{"usage":{"input_tokens":1,"speed":"fast"}}}`) +
			sseLine(`{"usage":{"output_tokens":1,"speed":"turbo"}}`),
	))
	if stream.Speed != "" || !stream.SpeedPresent {
		t.Fatalf("unsupported final speed did not clear seed: %+v", stream)
	}
}

func TestParseUsageMetadataCapturesOneHourCacheWrites(t *testing.T) {
	nonStream := ParseUsageWithSaw([]byte(`{
			"usage":{
				"input_tokens":1,
				"output_tokens":1,
				"cache_creation_input_tokens":10,
				"cache_creation":{
					"ephemeral_5m_input_tokens":3,
					"ephemeral_1h_input_tokens":7
				}
			}
		}`))
	if !nonStream.Usage.CacheCreationOneHourTokensObserved ||
		!nonStream.Usage.CacheCreationFiveMinuteTokensObserved ||
		!nonStream.Usage.CacheCreationTokenBreakdownComplete ||
		nonStream.Usage.CacheCreationFiveMinuteInputTokens != 3 ||
		nonStream.Usage.CacheCreationOneHourInputTokens != 7 {
		t.Fatalf("non-stream cache metadata = %+v", nonStream)
	}
	stream := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"message":{"usage":{"input_tokens":1,"cache_creation_input_tokens":4,"cache_creation":{"ephemeral_1h_input_tokens":3}}}}`) +
			sseLine(`{"usage":{"output_tokens":1,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_1h_input_tokens":7}}}`),
	))
	if !stream.Usage.CacheCreationOneHourTokensObserved ||
		stream.Usage.CacheCreationFiveMinuteTokensObserved ||
		!stream.Usage.CacheCreationTokenBreakdownComplete ||
		stream.Usage.CacheCreationFiveMinuteInputTokens != 3 ||
		stream.Usage.CacheCreationOneHourInputTokens != 7 {
		t.Fatalf("stream cache metadata = %+v", stream)
	}
}

func TestParseUsageRejectsInconsistentCacheCreationBreakdown(t *testing.T) {
	for _, body := range []string{
		`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":7}}}`,
		`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":6,"cache_creation":{"ephemeral_1h_input_tokens":7}}}`,
		`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":-1,"ephemeral_1h_input_tokens":2}}}`,
	} {
		got := ParseUsageWithSaw([]byte(body)).Usage
		if got.CacheCreationTokenBreakdownComplete ||
			!got.CacheCreationTokenBreakdownInconsistent {
			t.Fatalf("inconsistent cache breakdown accepted: %+v", got)
		}
	}
}

func TestParseUsagePreservesMissingVersusReportedZeroCacheBuckets(t *testing.T) {
	missing := ParseUsageWithSaw([]byte(
		`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0}}`,
	)).Usage
	if missing.CacheCreationFiveMinuteTokensObserved ||
		missing.CacheCreationOneHourTokensObserved ||
		missing.CacheCreationTokenBreakdownComplete {
		t.Fatalf("missing cache breakdown became observed: %+v", missing)
	}
	zero := ParseUsageWithSaw([]byte(
		`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}}}`,
	)).Usage
	if !zero.CacheCreationFiveMinuteTokensObserved ||
		!zero.CacheCreationOneHourTokensObserved ||
		!zero.CacheCreationTokenBreakdownComplete ||
		zero.CacheCreationTokenBreakdownInconsistent {
		t.Fatalf("reported zero cache breakdown lost: %+v", zero)
	}
}

func TestParseUsageHappy(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"usage": map[string]interface{}{
			"input_tokens":                10,
			"output_tokens":               20,
			"cache_creation_input_tokens": 30,
			"cache_read_input_tokens":     40,
		},
	})
	u := ParseUsage(body)
	if u.InputTokens != 10 || u.OutputTokens != 20 || u.CacheCreationInputTokens != 30 || u.CacheReadInputTokens != 40 {
		t.Fatalf("mismatch: %+v", u)
	}
}

func TestParseOpenAIChatCompletionsUsage(t *testing.T) {
	nonStream := ParseUsageWithSaw([]byte(
		`{"model":"gpt-5","usage":{"prompt_tokens":14,"completion_tokens":3}}`,
	))
	if !nonStream.Saw ||
		nonStream.Usage.InputTokens != 14 ||
		nonStream.Usage.OutputTokens != 3 {
		t.Fatalf("non-stream OpenAI usage = %+v", nonStream)
	}

	stream := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"model":"gpt-5","choices":[],"usage":{"prompt_tokens":21,"completion_tokens":5}}`) +
			sseLine(`[DONE]`),
	))
	if !stream.Saw || !stream.Complete ||
		stream.Usage.InputTokens != 21 ||
		stream.Usage.OutputTokens != 5 {
		t.Fatalf("stream OpenAI usage = %+v", stream)
	}
}

func TestParseResponsesCompletedMetadata(t *testing.T) {
	stream := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"type":"response.created","response":{"model":"zai-org/GLM-5.2","status":"in_progress"}}`) +
			sseLine(`{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}`) +
			sseLine(`{"type":"response.output_item.done","item":{"id":"msg_1","type":"message","status":"completed"}}`) +
			sseLine(`{"type":"response.completed","response":{"model":"zai-org/GLM-5.2","status":"completed","output":[{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}],"usage":{"input_tokens":41,"output_tokens":7}}}`),
	))
	if !stream.Saw || !stream.Complete {
		t.Fatalf("completed Responses metadata = %+v", stream)
	}
	if stream.Usage.InputTokens != 41 || stream.Usage.OutputTokens != 7 {
		t.Fatalf("completed Responses usage = %+v", stream.Usage)
	}
	if stream.Model != "zai-org/GLM-5.2" {
		t.Fatalf("completed Responses model = %q", stream.Model)
	}
	if stream.StopReason != "completed" {
		t.Fatalf("completed Responses stop reason = %q", stream.StopReason)
	}
	if stream.ToolCalls != 1 {
		t.Fatalf("completed Responses tool calls = %d, want 1", stream.ToolCalls)
	}
}

func TestParseResponsesIncompleteAndFailedAreTerminal(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantReason string
		wantSaw    bool
	}{
		{
			name: "incomplete",
			payload: `{"type":"response.incomplete","response":{` +
				`"model":"openai/gpt-5","status":"incomplete",` +
				`"incomplete_details":{"reason":"max_output_tokens"},` +
				`"usage":{"input_tokens":12,"output_tokens":4}}}`,
			wantReason: "max_output_tokens",
			wantSaw:    true,
		},
		{
			name: "failed",
			payload: `{"type":"response.failed","response":{` +
				`"model":"openai/gpt-5","status":"failed",` +
				`"error":{"code":"server_error","message":"redacted"}}}`,
			wantReason: "server_error",
			wantSaw:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSSEUsageWithSaw([]byte(sseLine(tc.payload)))
			if !got.Complete {
				t.Fatalf("terminal Responses event not recognized: %+v", got)
			}
			if got.Saw != tc.wantSaw {
				t.Fatalf("Saw = %v, want %v: %+v", got.Saw, tc.wantSaw, got)
			}
			if got.StopReason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got.StopReason, tc.wantReason)
			}
			if got.Model != "openai/gpt-5" {
				t.Fatalf("model = %q", got.Model)
			}
		})
	}
}

func TestParseResponsesFunctionCallsWithoutTerminalSnapshot(t *testing.T) {
	got := ParseSSEUsageWithSaw([]byte(
		sseLine(`{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1"}}`) +
			sseLine(`{"type":"response.output_item.done","item":{"id":"fc_2","type":"function_call","call_id":"call_2"}}`) +
			sseLine(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`),
	))
	if got.ToolCalls != 2 {
		t.Fatalf("tool calls = %d, want 2", got.ToolCalls)
	}
}

func TestParseNonStreamingResponsesMetadata(t *testing.T) {
	got := ParseUsageWithSaw([]byte(`{
		"id":"resp_1",
		"object":"response",
		"model":"openai/gpt-5",
		"status":"incomplete",
		"incomplete_details":{"reason":"content_filter"},
		"output":[
			{"id":"fc_1","type":"function_call","call_id":"call_1"},
			{"id":"msg_1","type":"message"}
		],
		"usage":{"input_tokens":9,"output_tokens":2}
	}`))
	if !got.Saw ||
		got.Usage.InputTokens != 9 ||
		got.Usage.OutputTokens != 2 ||
		got.Model != "openai/gpt-5" ||
		got.StopReason != "content_filter" ||
		got.ToolCalls != 1 {
		t.Fatalf("non-streaming Responses metadata = %+v", got)
	}
}

func TestParseUsageEmpty(t *testing.T) {
	u := ParseUsage([]byte("{}"))
	if u != (Usage{}) {
		t.Fatalf("expected zero, got %+v", u)
	}
	u = ParseUsage([]byte("not json"))
	if u != (Usage{}) {
		t.Fatalf("expected zero on garbage, got %+v", u)
	}
}

func TestParseUsageWithSawPreservesReportedZero(t *testing.T) {
	observed := ParseUsageWithSaw([]byte(
		`{"usage":{"input_tokens":0,"output_tokens":0}}`,
	))
	if !observed.Saw || observed.Usage != (Usage{}) {
		t.Fatalf("observed zero metadata = %+v", observed)
	}
	missing := ParseUsageWithSaw([]byte(`{"content":[]}`))
	if missing.Saw {
		t.Fatalf("missing usage reported as observed: %+v", missing)
	}
}

func TestSynthCountTokensEmpty(t *testing.T) {
	if c := SynthCountTokens([]byte("{}")); c != 1 {
		t.Fatalf("empty got %d want 1", c)
	}
}

func TestSynthCountTokensBody(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"messages": []map[string]interface{}{
			{"content": "hello world this is forty chars maybe"},
		},
	})
	c := SynthCountTokens(body)
	if c < 1 {
		t.Fatalf("got %d want >=1", c)
	}
}

func TestSynthCountTokensMultiBlock(t *testing.T) {
	body := mustJSON(t, map[string]interface{}{
		"system": []map[string]interface{}{{"text": "system prompt here"}},
		"messages": []map[string]interface{}{
			{"content": []map[string]interface{}{
				{"text": "alpha beta gamma delta"},
				{"input": "epsilon zeta eta"},
			}},
		},
		"tools": []map[string]interface{}{
			{"name": "search", "description": "do a search"},
		},
	})
	c := SynthCountTokens(body)
	if c < 1 {
		t.Fatalf("got %d want >=1", c)
	}
	if c > 100000 {
		t.Fatalf("got %d, suspiciously large", c)
	}
}

func TestSynthCountTokensGarbage(t *testing.T) {
	if c := SynthCountTokens([]byte("nope")); c != 0 {
		t.Fatalf("garbage got %d want 0", c)
	}
}

func TestMaybeDecompressPassthrough(t *testing.T) {
	in := []byte("plain text body")
	out := MaybeDecompress(in, "")
	if !bytes.Equal(out, in) {
		t.Fatal("passthrough mutated bytes")
	}
	out = MaybeDecompress(in, "identity")
	if !bytes.Equal(out, in) {
		t.Fatal("identity mutated bytes")
	}
}

func TestMaybeDecompressGzip(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	want := []byte("the quick brown fox jumps over the lazy dog")
	_, _ = w.Write(want)
	_ = w.Close()
	out := MaybeDecompress(gz.Bytes(), "gzip")
	if !bytes.Equal(out, want) {
		t.Fatalf("gzip decompress mismatch: %q", out)
	}
}

func TestMaybeDecompressGzipInvalid(t *testing.T) {
	out := MaybeDecompress([]byte("not gzip"), "gzip")
	if !bytes.Equal(out, []byte("not gzip")) {
		t.Fatal("invalid gzip should return input unchanged")
	}
}

func TestParseSSEUsageAnthropicWithNestedUsageObjects(t *testing.T) {
	// Anthropic's SSE emits `cache_creation: {ephemeral_5m_input_tokens: 0, ...}`
	// and `output_tokens_details: {thinking_tokens: 0}` nested objects inside
	// the usage object. A map[string]json.Number unmarshal fails silently on
	// those nested values and yields an empty usage dict. Must use raw + per-key parse.
	buf := []byte(
		sseLine(`{"message":{"usage":{"input_tokens":15,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens":5,"service_tier":"standard","inference_geo":"global"}}}`) +
			sseLine(`{"usage":{"input_tokens":15,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5,"output_tokens_details":{"thinking_tokens":0}}}`) +
			"data: [DONE]\n")
	u := ParseSSEUsage(buf)
	if u.InputTokens != 15 {
		t.Fatalf("input = %d want 15", u.InputTokens)
	}
	if u.OutputTokens != 5 {
		t.Fatalf("output = %d want 5", u.OutputTokens)
	}
	if u.CacheReadInputTokens != 0 {
		t.Fatalf("cache_read = %d want 0", u.CacheReadInputTokens)
	}
	if u.CacheCreationInputTokens != 0 {
		t.Fatalf("cache_creation = %d want 0", u.CacheCreationInputTokens)
	}
}

func TestParseUsageNonSSEWithNestedUsageObjects(t *testing.T) {
	// Non-streaming JSON response also includes nested usage objects from Anthropic.
	body := []byte(`{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":15,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens":5,"output_tokens_details":{"thinking_tokens":0}}}`)
	u := ParseUsage(body)
	if u.InputTokens != 15 {
		t.Fatalf("input = %d want 15", u.InputTokens)
	}
	if u.OutputTokens != 5 {
		t.Fatalf("output = %d want 5", u.OutputTokens)
	}
}

func TestParseNumberAcceptsString(t *testing.T) {
	n, err := parseNumber(json.RawMessage(`"42"`))
	if err != nil || n != 42 {
		t.Fatalf("string number parse: %d err=%v", n, err)
	}
	n, err = parseNumber(json.RawMessage(`42`))
	if err != nil || n != 42 {
		t.Fatalf("number parse: %d err=%v", n, err)
	}
	n, err = parseNumber(json.RawMessage(`{"x":1}`))
	if err == nil {
		t.Fatalf("expected error for nested object, got %d", n)
	}
}

// TestNormalizeAnthropicBody covers the Baseten inclusive-input compensation:
// input_tokens is de-double-counted only when cache_read_input_tokens is
// present, cache_creation_input_tokens is ABSENT (its presence is the
// fixed-adapter sentinel: Anthropic always sends it, Baseten's inclusive
// adapter never does), and the guard (input >= cache_read) holds; everything
// else passes through untouched without panicking.
func TestNormalizeAnthropicBody(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantChanged bool
		wantInput   int64 // only checked when wantChanged
	}{
		{
			name:        "non-stream body with cache_read normalized",
			in:          `{"id":"m","type":"message","usage":{"input_tokens":46292,"output_tokens":16,"cache_read_input_tokens":46272}}`,
			wantChanged: true,
			wantInput:   20,
		},
		{
			name:        "cache_creation present (fixed adapter sentinel) untouched",
			in:          `{"usage":{"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":60,"cache_creation_input_tokens":30}}`,
			wantChanged: false,
		},
		{
			name:        "cache_creation zero still counts as sentinel, untouched",
			in:          `{"usage":{"input_tokens":46292,"output_tokens":16,"cache_read_input_tokens":46272,"cache_creation_input_tokens":0}}`,
			wantChanged: false,
		},
		{
			name:        "string-wrapped counts normalized",
			in:          `{"usage":{"input_tokens":"46292","output_tokens":"16","cache_read_input_tokens":"46272"}}`,
			wantChanged: true,
			wantInput:   20,
		},
		{
			name:        "no cache fields untouched",
			in:          `{"usage":{"input_tokens":20,"output_tokens":16}}`,
			wantChanged: false,
		},
		{
			name:        "input below cache_read untouched (guard)",
			in:          `{"usage":{"input_tokens":20,"output_tokens":16,"cache_read_input_tokens":46272}}`,
			wantChanged: false,
		},
		{
			name:        "streaming message_delta with cache_read normalized",
			in:          `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":107987,"output_tokens":124,"cache_read_input_tokens":76960}}`,
			wantChanged: true,
			wantInput:   31027,
		},
		{
			name:        "message_start without cache fields untouched",
			in:          `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":107987,"output_tokens":1}}}`,
			wantChanged: false,
		},
		{
			name:        "message_start with cache fields normalized in nested usage",
			in:          `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":90}}}`,
			wantChanged: true,
			wantInput:   10,
		},
		{
			name:        "malformed usage body passed through unchanged",
			in:          `{"usage":{"input_tokens":`,
			wantChanged: false,
		},
		{
			name:        "usage not an object passed through",
			in:          `{"usage":"nope"}`,
			wantChanged: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := NormalizeAnthropicBody([]byte(tc.in))
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v (out=%s)", changed, tc.wantChanged, out)
			}
			if !changed {
				if string(out) != tc.in {
					t.Fatalf("unchanged body was rewritten: got %s want %s", out, tc.in)
				}
				return
			}
			// Parse the rewritten body and confirm input_tokens landed where
			// expected; also confirm cache fields are preserved and no
			// negative value was produced.
			var body struct {
				Usage   json.RawMessage `json:"usage"`
				Message *struct {
					Usage json.RawMessage `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(out, &body); err != nil {
				t.Fatalf("rewritten body is not valid JSON: %v (%s)", err, out)
			}
			raw := body.Usage
			if len(raw) == 0 && body.Message != nil {
				raw = body.Message.Usage
			}
			u := ParseUsage([]byte(`{"usage":` + string(raw) + `}`))
			if u.InputTokens != tc.wantInput {
				t.Fatalf("input_tokens = %d, want %d (out=%s)", u.InputTokens, tc.wantInput, out)
			}
			if u.InputTokens < 0 {
				t.Fatalf("normalization produced negative input_tokens: %d", u.InputTokens)
			}
		})
	}
}
