package translate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func parse(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not json: %v\n%s", err, b)
	}
	return m
}

func TestRequestToOpenAIBasics(t *testing.T) {
	in := []byte(`{
		"model": "m1", "max_tokens": 100, "temperature": 0.5, "stream": true,
		"stop_sequences": ["END"],
		"system": [{"type":"text","text":"sys1"},{"type":"text","text":"sys2"}],
		"messages": [{"role":"user","content":"hi"}]
	}`)
	out, err := RequestToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, out)
	if m["model"] != "m1" || m["max_tokens"] != float64(100) || m["temperature"] != 0.5 {
		t.Fatalf("scalar params wrong: %v", m)
	}
	if m["stream"] != true {
		t.Fatal("stream lost")
	}
	if _, ok := m["stream_options"]; !ok {
		t.Fatal("stream_options.include_usage not set for streaming request")
	}
	if stop := m["stop"].([]any); stop[0] != "END" {
		t.Fatalf("stop_sequences not mapped: %v", m["stop"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want system+user, got %v", msgs)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "sys1\nsys2" {
		t.Fatalf("system not flattened: %v", sys)
	}
}

func TestRequestToOpenAIToolLoop(t *testing.T) {
	in := []byte(`{
		"model":"m","max_tokens":10,
		"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"any"},
		"messages":[
			{"role":"user","content":"list files"},
			{"role":"assistant","content":[
				{"type":"text","text":"ok"},
				{"type":"tool_use","id":"tu_1","name":"Bash","input":{"cmd":"ls"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"a.txt"}]},
				{"type":"text","text":"now what"}
			]}
		]
	}`)
	out, err := RequestToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, out)
	tools := m["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "Bash" || fn["parameters"] == nil {
		t.Fatalf("tool def wrong: %v", tools)
	}
	if m["tool_choice"] != "required" {
		t.Fatalf("tool_choice any -> required, got %v", m["tool_choice"])
	}
	msgs := m["messages"].([]any)
	// user, assistant(with tool_calls), tool, user
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d: %v", len(msgs), msgs)
	}
	asst := msgs[1].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "tu_1" {
		t.Fatalf("tool_call id wrong: %v", tc)
	}
	args := tc["function"].(map[string]any)["arguments"].(string)
	if !strings.Contains(args, `"cmd":"ls"`) {
		t.Fatalf("arguments not stringified: %q", args)
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "tu_1" || toolMsg["content"] != "a.txt" {
		t.Fatalf("tool_result not converted: %v", toolMsg)
	}
	if msgs[3].(map[string]any)["content"] != "now what" {
		t.Fatalf("trailing user text lost: %v", msgs[3])
	}
}

func TestRequestToOpenAIRejectsImages(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`)
	_, err := RequestToOpenAI(in)
	var uc ErrUnsupportedContent
	if !errors.As(err, &uc) || uc.BlockType != "image" {
		t.Fatalf("want ErrUnsupportedContent{image}, got %v", err)
	}
}

func TestRequestToOpenAIRejectsToolResultImages(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":[{
			"type":"tool_result",
			"tool_use_id":"tu_1",
			"content":[
				{"type":"text","text":"rendered output"},
				{"type":"image","source":{"type":"base64","data":"aW1hZ2U="}}
			]
		}]}]
	}`)
	_, err := RequestToOpenAI(in)
	var unsupported ErrUnsupportedContent
	if !errors.As(err, &unsupported) || unsupported.BlockType != "image" {
		t.Fatalf("want ErrUnsupportedContent{image}, got %v", err)
	}
}

func TestResponseToAnthropicToolCalls(t *testing.T) {
	in := []byte(`{"id":"chatcmpl-9","model":"gpt-x","choices":[{"finish_reason":"tool_calls","message":{"content":null,"tool_calls":[{"id":"call_1","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}}`)
	out, err := ResponseToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, out)
	if m["stop_reason"] != "tool_use" || m["id"] != "msg_chatcmpl-9" {
		t.Fatalf("envelope wrong: %v", m)
	}
	content := m["content"].([]any)
	tu := content[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["name"] != "Bash" {
		t.Fatalf("tool_use block wrong: %v", tu)
	}
	if tu["input"].(map[string]any)["cmd"] != "ls" {
		t.Fatalf("arguments not parsed: %v", tu)
	}
	u := m["usage"].(map[string]any)
	if u["input_tokens"] != float64(6) || u["cache_read_input_tokens"] != float64(4) || u["output_tokens"] != float64(5) {
		t.Fatalf("usage mapping wrong: %v", u)
	}
}

func TestErrorToAnthropic(t *testing.T) {
	b := ErrorToAnthropic(429, []byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
	m := parse(t, b)
	e := m["error"].(map[string]any)
	if m["type"] != "error" || e["type"] != "rate_limit_error" || e["message"] != "slow down" {
		t.Fatalf("error translation wrong: %s", b)
	}
	b2 := ErrorToAnthropic(500, []byte(`not json`))
	if !strings.Contains(string(b2), "not json") {
		t.Fatalf("raw body not preserved: %s", b2)
	}
}

// collectEvents parses an emitted anthropic SSE buffer into (event, data) pairs.
func collectEvents(t *testing.T, s string) []([2]string) {
	t.Helper()
	var out [][2]string
	for _, block := range strings.Split(strings.TrimSpace(s), "\n\n") {
		lines := strings.SplitN(block, "\n", 2)
		if len(lines) != 2 {
			t.Fatalf("bad SSE block: %q", block)
		}
		out = append(out, [2]string{
			strings.TrimPrefix(lines[0], "event: "),
			strings.TrimPrefix(lines[1], "data: "),
		})
	}
	return out
}

func TestStreamTranslatorTextAndTools(t *testing.T) {
	var buf strings.Builder
	tr := NewStreamTranslator(&buf)
	feed := []string{
		`{"id":"c1","model":"gpt-x","choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"id":"c1","model":"gpt-x","choices":[{"delta":{"content":"lo"}}]}`,
		`{"id":"c1","model":"gpt-x","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":""}}]}}]}`,
		`{"id":"c1","model":"gpt-x","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]}}]}`,
		`{"id":"c1","model":"gpt-x","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`{"id":"c1","model":"gpt-x","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","model":"gpt-x","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3}}`,
	}
	for _, d := range feed {
		if err := tr.HandleData([]byte(d)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Finish(); err != nil {
		t.Fatal(err)
	}
	events := collectEvents(t, buf.String())
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e[0])
	}
	want := []string{
		"message_start", "ping",
		"content_block_start", "content_block_delta", "content_block_delta", // text Hel, lo
		"content_block_stop",                                                // text closed when tool starts
		"content_block_start", "content_block_delta", "content_block_delta", // tool + 2 arg fragments
		"content_block_stop", "message_delta", "message_stop",
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("event order:\n got %v\nwant %v", kinds, want)
	}
	// tool block carries id/name; text deltas carry the text.
	var cbs map[string]any
	_ = json.Unmarshal([]byte(events[6][1]), &cbs)
	cb := cbs["content_block"].(map[string]any)
	if cb["type"] != "tool_use" || cb["id"] != "call_1" || cb["name"] != "Bash" {
		t.Fatalf("tool content_block_start wrong: %v", cb)
	}
	var md map[string]any
	_ = json.Unmarshal([]byte(events[10][1]), &md)
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason wrong: %v", md)
	}
	if md["usage"].(map[string]any)["output_tokens"] != float64(3) {
		t.Fatalf("usage missing from message_delta: %v", md)
	}
	// blocks got sequential indices 0 (text) and 1 (tool)
	var d1 map[string]any
	_ = json.Unmarshal([]byte(events[3][1]), &d1)
	if d1["index"] != float64(0) {
		t.Fatalf("text block index: %v", d1)
	}
	var d2 map[string]any
	_ = json.Unmarshal([]byte(events[7][1]), &d2)
	if d2["index"] != float64(1) {
		t.Fatalf("tool block index: %v", d2)
	}
}

func TestStreamTranslatorUpstreamError(t *testing.T) {
	var buf strings.Builder
	tr := NewStreamTranslator(&buf)
	if err := tr.HandleData([]byte(`{"error":{"message":"boom","type":"server_error"}}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "event: error") || !strings.Contains(buf.String(), "boom") {
		t.Fatalf("error event missing: %s", buf.String())
	}
}
