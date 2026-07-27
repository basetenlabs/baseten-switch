package responsescompat

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeTextFormat(t *testing.T) {
	t.Run("adds default", func(t *testing.T) {
		body := []byte(`{"model":"m","text":{"verbosity":"low"}}`)
		got, changed := NormalizeTextFormat(body)
		if !changed {
			t.Fatal("changed = false, want true")
		}
		var data map[string]any
		if err := json.Unmarshal(got, &data); err != nil {
			t.Fatal(err)
		}
		text := data["text"].(map[string]any)
		if !reflect.DeepEqual(text["format"], map[string]any{"type": "text"}) {
			t.Fatalf("format = %#v", text["format"])
		}
		if text["verbosity"] != "low" {
			t.Fatalf("verbosity changed: %#v", text)
		}
	})

	for _, tc := range []struct {
		name string
		body string
	}{
		{"explicit future format", `{"text":{"format":{"type":"future"}}}`},
		{"explicit null format", `{"text":{"format":null}}`},
		{"text absent", `{"model":"m"}`},
		{"text wrong type", `{"text":"plain"}`},
		{"malformed", `{"text":`},
		{"non-object", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			got, changed := NormalizeTextFormat(body)
			if changed || !sameBytes(got, body) {
				t.Fatalf("no-op changed body: %q", got)
			}
		})
	}
}

func TestAdditionalToolsRuleApply(t *testing.T) {
	rule := NewAdditionalToolsRule()
	body := []byte(`{
		"input":[
			{"type":"message","role":"developer","content":[]},
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"custom","name":"exec"},
				{"type":"namespace","name":"tools"}
			]},
			{"type":"message","role":"user","content":[]}
		],
		"tool_choice":{"type":"additional_tools"}
	}`)
	got, changed, err := rule.Apply(body, RequestContext{})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var data map[string]any
	if err := json.Unmarshal(got, &data); err != nil {
		t.Fatal(err)
	}
	input := data["input"].([]any)
	if len(input) != 2 ||
		input[0].(map[string]any)["role"] != "developer" ||
		input[1].(map[string]any)["role"] != "user" {
		t.Fatalf("non-target input order changed: %#v", input)
	}
	tools := data["tools"].([]any)
	if len(tools) != 2 ||
		tools[0].(map[string]any)["name"] != "exec" ||
		tools[1].(map[string]any)["name"] != "tools" {
		t.Fatalf("tool order changed: %#v", tools)
	}
	if data["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %#v", data["tool_choice"])
	}

	t.Run("ordinary tool choice preserved", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"additional_tools","tools":[{"type":"function","name":"a"}]}],"tool_choice":{"type":"function","name":"a"}}`)
		got, changed, err := rule.Apply(body, RequestContext{})
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		var data map[string]any
		_ = json.Unmarshal(got, &data)
		if !reflect.DeepEqual(data["tool_choice"], map[string]any{"type": "function", "name": "a"}) {
			t.Fatalf("tool_choice changed: %#v", data["tool_choice"])
		}
	})

	for _, tc := range []struct {
		name string
		body string
		err  error
	}{
		{
			"duplicate envelopes",
			`{"input":[{"type":"additional_tools","tools":[]},{"type":"additional_tools","tools":[]}]}`,
			ErrAmbiguousAdditionalTools,
		},
		{
			"top-level definitions",
			`{"input":[{"type":"additional_tools","tools":[{"type":"function","name":"a"}]}],"tools":[{"type":"function","name":"b"}]}`,
			ErrAmbiguousAdditionalTools,
		},
		{
			"invalid envelope",
			`{"input":[{"type":"additional_tools"}]}`,
			ErrInvalidAdditionalTools,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			got, changed, err := rule.Apply(body, RequestContext{})
			if changed || !errors.Is(err, tc.err) || !sameBytes(got, body) {
				t.Fatalf("changed=%v err=%v body=%q", changed, err, got)
			}
		})
	}
}

func TestReasoningEffortRuleApply(t *testing.T) {
	rule, err := NewReasoningEffortRule(ReasoningEffortPolicy{
		Allowed: []string{"low", "high"},
		Map:     map[string]string{"medium": "low"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := RequestContext{}
	got, changed, err := rule.Apply([]byte(`{"reasoning":{"effort":"medium","summary":"auto"}}`), ctx)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var data map[string]any
	_ = json.Unmarshal(got, &data)
	reasoning := data["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", reasoning)
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"already accepted", `{"reasoning":{"effort":"high"}}`},
		{"unknown effort", `{"reasoning":{"effort":"future"}}`},
		{"no reasoning", `{"input":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			got, changed, err := rule.Apply(body, RequestContext{})
			if err != nil || changed || !sameBytes(got, body) {
				t.Fatalf("changed=%v err=%v body=%q", changed, err, got)
			}
		})
	}
}

func TestStripToolTypes(t *testing.T) {
	body := []byte(`{"tools":[{"type":"tool_search"},{"type":"function","name":"a"},{"type":"future"}],"tool_choice":{"type":"tool_search"}}`)
	got, stripped := StripToolTypes(body, []string{"tool_search"})
	if !reflect.DeepEqual(stripped, []string{"tool_search"}) {
		t.Fatalf("stripped = %v", stripped)
	}
	var data map[string]any
	_ = json.Unmarshal(got, &data)
	if len(data["tools"].([]any)) != 2 || data["tool_choice"] != "auto" {
		t.Fatalf("rewritten body = %s", got)
	}
	noop, stripped := StripToolTypes(body, nil)
	if stripped != nil || !sameBytes(noop, body) {
		t.Fatal("empty denylist must be byte-identical")
	}
	noop, stripped = StripToolTypes(body, []string{"unknown"})
	if stripped != nil || !sameBytes(noop, body) {
		t.Fatal("unknown denylist entry must fail open")
	}
}

func TestRequestRewritesPreserveJSONIntegers(t *testing.T) {
	const largeInteger = "9007199254740993"
	assertPreserved := func(t *testing.T, body []byte) {
		t.Helper()
		var data map[string]json.RawMessage
		if err := json.Unmarshal(body, &data); err != nil {
			t.Fatal(err)
		}
		if got := string(data["large_integer"]); got != largeInteger {
			t.Fatalf("large_integer = %s, want %s in %s", got, largeInteger, body)
		}
	}

	t.Run("text format", func(t *testing.T) {
		body := []byte(`{"large_integer":` + largeInteger + `,"text":{"verbosity":"low"}}`)
		got, changed := NormalizeTextFormat(body)
		if !changed {
			t.Fatal("changed = false")
		}
		assertPreserved(t, got)
	})

	t.Run("additional tools", func(t *testing.T) {
		body := []byte(`{"large_integer":` + largeInteger + `,"input":[{"type":"additional_tools","tools":[]}]}`)
		got, changed, err := NewAdditionalToolsRule().Apply(body, RequestContext{})
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		assertPreserved(t, got)
	})

	t.Run("reasoning effort", func(t *testing.T) {
		rule, err := NewReasoningEffortRule(ReasoningEffortPolicy{
			Allowed: []string{"high"},
		})
		if err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"large_integer":` + largeInteger + `,"reasoning":{"effort":"medium"}}`)
		got, changed, err := rule.Apply(body, RequestContext{})
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		assertPreserved(t, got)
	})

	t.Run("tool denylist", func(t *testing.T) {
		body := []byte(`{"large_integer":` + largeInteger + `,"tools":[{"type":"tool_search"},{"type":"function","name":"keep"}]}`)
		got, stripped := StripToolTypes(body, []string{"tool_search"})
		if !reflect.DeepEqual(stripped, []string{"tool_search"}) {
			t.Fatalf("stripped = %v", stripped)
		}
		assertPreserved(t, got)
	})
}
