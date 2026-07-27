package sanitize

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustParse(t *testing.T, s string) []any {
	t.Helper()
	var v []any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return v
}

func TestStripEmptyTextBlocks(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		want        string
		wantChanged bool
	}{
		{
			name:        "empty text alongside tool_use",
			in:          `[{"role":"assistant","content":[{"type":"text","text":""},{"type":"tool_use","id":"a","name":"Bash","input":{}}]}]`,
			want:        `[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Bash","input":{}}]}]`,
			wantChanged: true,
		},
		{
			name:        "whitespace-only text",
			in:          `[{"role":"assistant","content":[{"type":"text","text":"  \n\t"},{"type":"text","text":"keep"}]}]`,
			want:        `[{"role":"assistant","content":[{"type":"text","text":"keep"}]}]`,
			wantChanged: true,
		},
		{
			name:        "message dropped when content empties",
			in:          `[{"role":"assistant","content":[{"type":"text","text":""}]},{"role":"user","content":[{"type":"text","text":"hi"}]}]`,
			want:        `[{"role":"user","content":[{"type":"text","text":"hi"}]}]`,
			wantChanged: true,
		},
		{
			name:        "string content untouched",
			in:          `[{"role":"user","content":""}]`,
			want:        `[{"role":"user","content":""}]`,
			wantChanged: false,
		},
		{
			name:        "missing text field treated as empty",
			in:          `[{"role":"assistant","content":[{"type":"text"},{"type":"text","text":"ok"}]}]`,
			want:        `[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]`,
			wantChanged: true,
		},
		{
			name:        "clean messages unchanged",
			in:          `[{"role":"user","content":[{"type":"text","text":"hello"}]}]`,
			want:        `[{"role":"user","content":[{"type":"text","text":"hello"}]}]`,
			wantChanged: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := StripEmptyTextBlocks(mustParse(t, tc.in))
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if !reflect.DeepEqual(got, mustParse(t, tc.want)) {
				gb, _ := json.Marshal(got)
				t.Fatalf("got %s, want %s", gb, tc.want)
			}
		})
	}
}

func TestSanitizeToolUseIDs(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		want        string
		wantChanged bool
	}{
		{
			name:        "openai-style id normalized",
			in:          `[{"role":"assistant","content":[{"type":"tool_use","id":"functions.Bash:0","name":"Bash","input":{}}]}]`,
			want:        `[{"role":"assistant","content":[{"type":"tool_use","id":"functions_Bash_0","name":"Bash","input":{}}]}]`,
			wantChanged: true,
		},
		{
			name:        "tool_result id normalized",
			in:          `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"functions.Bash:0","content":"ok"}]}]`,
			want:        `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"functions_Bash_0","content":"ok"}]}]`,
			wantChanged: true,
		},
		{
			name:        "thought signature suffix stripped",
			in:          `[{"role":"assistant","content":[{"type":"tool_use","id":"call_1__thought__abc/def","name":"x","input":{}}]}]`,
			want:        `[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"x","input":{}}]}]`,
			wantChanged: true,
		},
		{
			name:        "valid ids unchanged",
			in:          `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01AbC","name":"x","input":{}}]}]`,
			want:        `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01AbC","name":"x","input":{}}]}]`,
			wantChanged: false,
		},
		{
			name:        "server_tool_use id normalized",
			in:          `[{"role":"assistant","content":[{"type":"server_tool_use","id":"srv:1","name":"x","input":{}}]}]`,
			want:        `[{"role":"assistant","content":[{"type":"server_tool_use","id":"srv_1","name":"x","input":{}}]}]`,
			wantChanged: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := SanitizeToolUseIDs(mustParse(t, tc.in))
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if !reflect.DeepEqual(got, mustParse(t, tc.want)) {
				gb, _ := json.Marshal(got)
				t.Fatalf("got %s, want %s", gb, tc.want)
			}
		})
	}
}

func TestNormalizeToolUseID(t *testing.T) {
	cases := map[string]string{
		"functions.Bash:0":      "functions_Bash_0",
		"toolu_01AbC":           "toolu_01AbC",
		"a__thought__x.y":       "a",
		"__thought__x":          "tool_use_id",
		"":                      "tool_use_id",
		"weird id/with spaces!": "weird_id_with_spaces_",
	}
	for in, want := range cases {
		if got := NormalizeToolUseID(in); got != want {
			t.Errorf("NormalizeToolUseID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAnthropicBody(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"text","text":""},{"type":"tool_use","id":"f.B:0","name":"Bash","input":{}}]}],"max_tokens":10}`)
	nb, changed := AnthropicBody(body)
	if !changed {
		t.Fatal("expected body change")
	}
	var data map[string]any
	if err := json.Unmarshal(nb, &data); err != nil {
		t.Fatalf("rewritten body invalid json: %v", err)
	}
	if data["model"] != "claude-x" || data["max_tokens"] != float64(10) {
		t.Fatalf("non-message fields not preserved: %v", data)
	}
	msgs := data["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("empty text block not stripped: %v", content)
	}
	if content[0].(map[string]any)["id"] != "f_B_0" {
		t.Fatalf("tool id not normalized: %v", content[0])
	}

	clean := []byte(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`)
	nb2, changed2 := AnthropicBody(clean)
	if changed2 {
		t.Fatal("clean body should not report change")
	}
	if &nb2[0] != &clean[0] {
		t.Fatal("clean body should be returned as-is")
	}

	notJSON := []byte(`{"messages": 5`)
	if _, c := AnthropicBody(notJSON); c {
		t.Fatal("unparseable body must pass through")
	}
}
