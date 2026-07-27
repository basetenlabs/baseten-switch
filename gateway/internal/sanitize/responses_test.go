package sanitize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResponsesStripToolTypes(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		types        []string
		want         string // "" means the original bytes must come back
		wantStripped []string
	}{
		{
			name:         "single type stripped",
			in:           `{"model":"m","tools":[{"type":"function","name":"a"},{"type":"tool_search"}],"tool_choice":"auto"}`,
			types:        []string{"tool_search"},
			want:         `{"model":"m","tools":[{"type":"function","name":"a"}],"tool_choice":"auto"}`,
			wantStripped: []string{"tool_search"},
		},
		{
			name:         "multiple types stripped, named in body order",
			in:           `{"tools":[{"type":"web_search"},{"type":"function","name":"a"},{"type":"tool_search"},{"type":"web_search"}]}`,
			types:        []string{"tool_search", "web_search", "image_generation"},
			want:         `{"tools":[{"type":"function","name":"a"}]}`,
			wantStripped: []string{"web_search", "tool_search"},
		},
		{
			name:  "empty denylist is a no-op",
			in:    `{"tools":[{"type":"tool_search"}]}`,
			types: nil,
		},
		{
			name:  "denylisted type absent",
			in:    `{"tools":[{"type":"function","name":"a"}],"tool_choice":"auto"}`,
			types: []string{"tool_search"},
		},
		{
			name:  "no tools field",
			in:    `{"model":"m","input":[]}`,
			types: []string{"tool_search"},
		},
		{
			name:  "tools not an array",
			in:    `{"tools":{"type":"tool_search"}}`,
			types: []string{"tool_search"},
		},
		{
			name:  "unparseable body passes through",
			in:    `{"tools": [`,
			types: []string{"tool_search"},
		},
		{
			name:  "non-object body passes through",
			in:    `[{"type":"tool_search"}]`,
			types: []string{"tool_search"},
		},
		{
			name:         "tool_choice object referencing stripped type reset to auto",
			in:           `{"tools":[{"type":"tool_search"},{"type":"function","name":"a"}],"tool_choice":{"type":"tool_search"}}`,
			types:        []string{"tool_search"},
			want:         `{"tools":[{"type":"function","name":"a"}],"tool_choice":"auto"}`,
			wantStripped: []string{"tool_search"},
		},
		{
			name:         "tool_choice object referencing surviving type untouched",
			in:           `{"tools":[{"type":"tool_search"},{"type":"function","name":"a"}],"tool_choice":{"type":"function","name":"a"}}`,
			types:        []string{"tool_search"},
			want:         `{"tools":[{"type":"function","name":"a"}],"tool_choice":{"type":"function","name":"a"}}`,
			wantStripped: []string{"tool_search"},
		},
		{
			name:  "tool_choice object referencing denylisted type absent from tools",
			in:    `{"tools":[{"type":"function","name":"a"}],"tool_choice":{"type":"tool_search"}}`,
			types: []string{"tool_search"},
		},
		{
			name:         "string tool_choice untouched",
			in:           `{"tools":[{"type":"tool_search"},{"type":"function","name":"a"}],"tool_choice":"none"}`,
			types:        []string{"tool_search"},
			want:         `{"tools":[{"type":"function","name":"a"}],"tool_choice":"none"}`,
			wantStripped: []string{"tool_search"},
		},
		{
			name:         "all entries stripped leaves empty tools array",
			in:           `{"tools":[{"type":"tool_search"}]}`,
			types:        []string{"tool_search"},
			want:         `{"tools":[]}`,
			wantStripped: []string{"tool_search"},
		},
		{
			name:         "non-object and untyped tools entries kept",
			in:           `{"tools":[5,{"type":"tool_search"},{"name":"untyped"}]}`,
			types:        []string{"tool_search"},
			want:         `{"tools":[5,{"name":"untyped"}]}`,
			wantStripped: []string{"tool_search"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.in)
			out, stripped := ResponsesStripToolTypes(body, tc.types)
			if !reflect.DeepEqual(stripped, tc.wantStripped) {
				t.Fatalf("stripped = %v, want %v", stripped, tc.wantStripped)
			}
			if tc.want == "" {
				if &out[0] != &body[0] {
					t.Fatal("no-op must return the original bytes")
				}
				return
			}
			var got, want map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("rewritten body invalid json: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatalf("parse want: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %s, want %s", out, tc.want)
			}
		})
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// The synthetic classic shape carries exactly one tool_search entry. The
// synthetic lite shape has no top-level tools[] at all.
func TestResponsesStripToolTypesFixtures(t *testing.T) {
	for _, name := range []string{
		"responses-classic.json",
	} {
		t.Run(name, func(t *testing.T) {
			body := readFixture(t, name)
			out, stripped := ResponsesStripToolTypes(body, []string{"tool_search"})
			if !reflect.DeepEqual(stripped, []string{"tool_search"}) {
				t.Fatalf("stripped = %v, want [tool_search]", stripped)
			}
			var orig, got map[string]any
			if err := json.Unmarshal(body, &orig); err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("rewritten body invalid json: %v", err)
			}
			origTools := orig["tools"].([]any)
			wantTools := make([]any, 0, len(origTools))
			removedCount := 0
			for _, tool := range origTools {
				if m, ok := tool.(map[string]any); ok && m["type"] == "tool_search" {
					removedCount++
					continue
				}
				wantTools = append(wantTools, tool)
			}
			if removedCount != 1 {
				t.Fatalf("fixture carries %d tool_search entries, want exactly 1", removedCount)
			}
			gotTools, ok := got["tools"].([]any)
			if !ok {
				t.Fatalf("rewritten body has no tools array: %v", got["tools"])
			}
			if !reflect.DeepEqual(gotTools, wantTools) {
				t.Fatalf("surviving tools not value-exact: got %d entries, want %d", len(gotTools), len(wantTools))
			}
			if len(got) != len(orig) {
				t.Fatalf("top-level key count changed: got %d, want %d", len(got), len(orig))
			}
			for k, v := range orig {
				if k == "tools" {
					continue
				}
				if !reflect.DeepEqual(got[k], v) {
					t.Errorf("top-level field %q not preserved value-exact", k)
				}
			}

			noop, s := ResponsesStripToolTypes(body, nil)
			if s != nil || &noop[0] != &body[0] {
				t.Fatal("empty denylist must return the original bytes with nil stripped")
			}
		})
	}

	t.Run("responses-lite.json", func(t *testing.T) {
		body := readFixture(t, "responses-lite.json")
		out, stripped := ResponsesStripToolTypes(body, []string{"tool_search"})
		if stripped != nil {
			t.Fatalf("stripped = %v, want nil", stripped)
		}
		if &out[0] != &body[0] {
			t.Fatal("lite-shape body without tools[] must pass through as the original bytes")
		}
	})
}

func TestResponsesStripToolTypesMap(t *testing.T) {
	body := readFixture(t, "responses-classic.json")
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	before := len(data["tools"].([]any))
	stripped := ResponsesStripToolTypesMap(data, []string{"tool_search", "image_generation"})
	if !reflect.DeepEqual(stripped, []string{"tool_search", "image_generation"}) {
		t.Fatalf("stripped = %v, want [tool_search image_generation]", stripped)
	}
	after := len(data["tools"].([]any))
	if after != before-2 {
		t.Fatalf("tools count = %d, want %d", after, before-2)
	}

	in := `{"tools":[{"type":"function","name":"a"}],"tool_choice":{"type":"tool_search"}}`
	var clean map[string]any
	if err := json.Unmarshal([]byte(in), &clean); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s := ResponsesStripToolTypesMap(clean, []string{"tool_search"}); s != nil {
		t.Fatalf("stripped = %v, want nil", s)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(in), &want); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(clean, want) {
		t.Fatalf("no-change call must leave the map unmodified: %v", clean)
	}
}
