package piconfig

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const testProvider = `{"name":"Example","baseUrl":"https://example.invalid/v1","models":[{"id":"example/model"}]}`

func TestProviderReturnsExactValueCopy(t *testing.T) {
	input := []byte(`{
  "providers": {
    "exam\u0070le": {
      // Formatting inside the value is significant to ownership.
      "name": "Example",
    } /* outside value */
  }
}`)
	got, found, err := Provider(input, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("Provider did not find escaped provider key")
	}
	want := `{
      // Formatting inside the value is significant to ownership.
      "name": "Example",
    }`
	if string(got) != want {
		t.Fatalf("Provider returned %q, want %q", got, want)
	}
	got[0] = '['
	if input[bytes.Index(input, []byte(want))] != '{' {
		t.Fatal("Provider result aliases its input")
	}
}

func TestProviderAbsent(t *testing.T) {
	for _, input := range []string{
		`{}`,
		`{"providers":{}}`,
		`{"providers":{"other":{}}}`,
	} {
		got, found, err := Provider([]byte(input), "example")
		if err != nil {
			t.Fatal(err)
		}
		if found || got != nil {
			t.Fatalf("Provider(%q) returned %q, found=%v", input, got, found)
		}
	}
}

func TestProviderRejectsInvalidOrAmbiguousInput(t *testing.T) {
	inputs := []string{
		`[]`,
		`{"providers":`,
		`{"providers":[]}`,
		`{"providers":{},"providers":{}}`,
		`{"providers":{"example":{},"example":{}}}`,
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			if _, _, err := Provider([]byte(input), "example"); err == nil {
				t.Fatal("Provider accepted invalid or ambiguous input")
			}
		})
	}
}

func TestUpsertProviderCreatesProvidersObject(t *testing.T) {
	input := []byte("{\n  // Existing settings stay byte-for-byte.\n  \"setting\": true,\n}\n")
	got, changed, err := UpsertProvider(input, "example", []byte(testProvider), false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("UpsertProvider reported no change")
	}
	want := "{\n  // Existing settings stay byte-for-byte.\n  \"setting\": true,\n  \"providers\": {\"example\": " + testProvider + "}\n}\n"
	if string(got) != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestUpsertProviderPreservesJSONCAndTrailingComma(t *testing.T) {
	input := []byte(`{
  "before": [1, 2,],
  "providers": {
    // This provider is unrelated.
    "other": {"enabled": true,},
  },
  "after": "unchanged",
}
`)
	got, changed, err := UpsertProvider(input, "example", []byte(testProvider), false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("UpsertProvider reported no change")
	}
	for _, preserved := range []string{
		`"before": [1, 2,],`,
		"// This provider is unrelated.",
		`"other": {"enabled": true,},`,
		`"after": "unchanged",`,
	} {
		if !strings.Contains(string(got), preserved) {
			t.Errorf("output did not preserve %q:\n%s", preserved, got)
		}
	}
	if !strings.Contains(string(got), `"example": `+testProvider) {
		t.Fatalf("output omitted provider:\n%s", got)
	}
}

func TestUpsertProviderIntoOneLineObjectWithoutTrailingComma(t *testing.T) {
	input := []byte(`{"providers":{"other":{}}}`)
	got, changed, err := UpsertProvider(input, "example", []byte(testProvider), false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("UpsertProvider reported no change")
	}
	want := `{"providers":{"other":{}, "example": ` + testProvider + `}}`
	if string(got) != want {
		t.Fatalf("output %q, want %q", got, want)
	}
}

func TestUpsertProviderRefusesCollision(t *testing.T) {
	input := []byte(`{"providers":{"example":{"name":"User value"}}}`)
	got, changed, err := UpsertProvider(input, "example", []byte(testProvider), false)
	if !errors.Is(err, ErrProviderCollision) {
		t.Fatalf("error = %v, want ErrProviderCollision", err)
	}
	if got != nil || changed {
		t.Fatalf("collision returned output %q, changed=%v", got, changed)
	}
}

func TestUpsertProviderReplacesOnlyValue(t *testing.T) {
	input := []byte(`{
  "providers": {
    "example" /* key note */ : /* value note */ {"name":"Old"} /* after note */,
    "other": {"keep":true}
  }
}`)
	got, changed, err := UpsertProvider(input, "example", []byte(testProvider), true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("UpsertProvider reported no change")
	}
	want := `{
  "providers": {
    "example" /* key note */ : /* value note */ ` + testProvider + ` /* after note */,
    "other": {"keep":true}
  }
}`
	if string(got) != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestUpsertProviderIdenticalValueIsNoop(t *testing.T) {
	input := []byte(`{"providers":{"example":` + testProvider + `}}`)
	got, changed, err := UpsertProvider(input, "example", []byte(" \n"+testProvider+"\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("identical provider reported a change")
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("no-op changed bytes: %q", got)
	}
}

func TestRemoveProviderPreservesNeighborsAndComments(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "first",
			input: `{"providers":{"example":{} /* keep comment */, "other":{}}}`,
			want:  `{"providers":{ /* keep comment */ "other":{}}}`,
		},
		{
			name:  "middle",
			input: `{"providers":{"one":{}, "example":{} /* keep comment */, "two":{}}}`,
			want:  `{"providers":{"one":{},  /* keep comment */ "two":{}}}`,
		},
		{
			name:  "last",
			input: `{"providers":{"other":{}, /* keep comment */ "example":{}}}`,
			want:  `{"providers":{"other":{} /* keep comment */ }}`,
		},
		{
			name:  "only with trailing comma",
			input: `{"providers":{"example":{} /* keep comment */,}}`,
			want:  `{"providers":{ /* keep comment */}}`,
		},
		{
			name:  "only without comma",
			input: `{"providers":{/* before */"example":{}/* after */}}`,
			want:  `{"providers":{/* before *//* after */}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, changed, err := RemoveProvider([]byte(test.input), "example")
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("RemoveProvider reported no change")
			}
			if string(got) != test.want {
				t.Fatalf("output %q, want %q", got, test.want)
			}
		})
	}
}

func TestRemoveProviderAbsentIsByteExactNoop(t *testing.T) {
	for _, input := range []string{
		"{/* no providers */}\n",
		`{"providers":{"other":{}}}`,
	} {
		got, changed, err := RemoveProvider([]byte(input), "example")
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Fatalf("RemoveProvider(%q) reported a change", input)
		}
		if string(got) != input {
			t.Fatalf("no-op output %q, want %q", got, input)
		}
	}
}

func TestRejectsMalformedJSONC(t *testing.T) {
	inputs := []string{
		"",
		`[]`,
		`{"providers":`,
		`{"providers":{/* unterminated`,
		`{"providers":{"example": tru}}`,
		`{"providers":{"example":{}}, trailing}`,
		`{"providers":{"example":{}},} extra`,
		`{"providers":{"example":"\x"}}`,
		`{"providers":[1,2]}`,
		string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}),
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			if _, _, err := UpsertProvider([]byte(input), "example", []byte(testProvider), true); err == nil {
				t.Fatal("UpsertProvider accepted malformed or unsupported input")
			}
			if _, _, err := RemoveProvider([]byte(input), "example"); err == nil {
				t.Fatal("RemoveProvider accepted malformed or unsupported input")
			}
		})
	}
}

func TestRejectsDuplicateRelevantKeys(t *testing.T) {
	inputs := []string{
		`{"providers":{},"providers":{}}`,
		`{"providers":{},"pro\u0076iders":{}}`,
		`{"providers":{"example":{},"example":{}}}`,
		`{"providers":{"example":{},"exam\u0070le":{}}}`,
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			if _, _, err := UpsertProvider([]byte(input), "example", []byte(testProvider), true); err == nil {
				t.Fatal("UpsertProvider accepted duplicate relevant key")
			}
			if _, _, err := RemoveProvider([]byte(input), "example"); err == nil {
				t.Fatal("RemoveProvider accepted duplicate relevant key")
			}
		})
	}
}

func TestUpsertProviderWithNestedMultilineValue(t *testing.T) {
	input := []byte("{\"providers\":{\"other\":{\"list\":[\n1,\n2\n]}}}")
	got, changed, err := UpsertProvider(input, "example", []byte(testProvider), false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("UpsertProvider reported no change")
	}
	raw, found, err := Provider(got, "example")
	if err != nil || !found || string(raw) != testProvider {
		t.Fatalf("inserted provider = %q, found=%v, err=%v", raw, found, err)
	}
	if !bytes.Contains(got, []byte("\"other\":{\"list\":[\n1,\n2\n]}")) {
		t.Fatalf("nested multiline value changed:\n%s", got)
	}
}

func TestRemoveProviderPreservesCRLFLineComment(t *testing.T) {
	input := []byte("{\r\n  \"providers\": {\r\n    \"example\": {} // keep comment\r\n    , \"other\": {}\r\n  }\r\n}\r\n")
	got, changed, err := RemoveProvider(input, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("RemoveProvider reported no change")
	}
	want := "{\r\n  \"providers\": {\r\n     // keep comment\r\n     \"other\": {}\r\n  }\r\n}\r\n"
	if string(got) != want {
		t.Fatalf("output %q, want %q", got, want)
	}
}

func TestAllowsDuplicateUnrelatedKeys(t *testing.T) {
	input := []byte(`{"note":1,"note":2,"providers":{"other":{},"other":{}}}`)
	if _, _, err := UpsertProvider(input, "example", []byte(testProvider), false); err != nil {
		t.Fatalf("unrelated duplicate prevented update: %v", err)
	}
}

func TestRejectsInvalidProviderJSON(t *testing.T) {
	input := []byte(`{"providers":{}}`)
	providers := []string{
		"",
		"null",
		"[]",
		`{"name":"bad",}`,
		`{/* comment */"name":"bad"}`,
		`{"name":"one","name":"two"}`,
		`{"nested":{"key":1,"key":2}}`,
	}
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			if _, _, err := UpsertProvider(input, "example", []byte(provider), false); err == nil {
				t.Fatal("UpsertProvider accepted invalid provider JSON")
			}
		})
	}
}

func TestEscapedKeysAreDecodedForOwnership(t *testing.T) {
	input := []byte(`{"pro\u0076iders":{"exam\u0070le":{"name":"old"}}}`)
	if _, _, err := UpsertProvider(input, "example", []byte(testProvider), false); !errors.Is(err, ErrProviderCollision) {
		t.Fatalf("error = %v, want escaped-key collision", err)
	}
	got, changed, err := RemoveProvider(input, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || string(got) != `{"pro\u0076iders":{}}` {
		t.Fatalf("RemoveProvider output = %q, changed=%v", got, changed)
	}
}

func TestRejectsInvalidProviderID(t *testing.T) {
	for _, providerID := range []string{"", string([]byte{0xff})} {
		if _, _, err := UpsertProvider([]byte(`{}`), providerID, []byte(testProvider), false); err == nil {
			t.Fatalf("UpsertProvider accepted provider ID %q", providerID)
		}
		if _, _, err := RemoveProvider([]byte(`{}`), providerID); err == nil {
			t.Fatalf("RemoveProvider accepted provider ID %q", providerID)
		}
	}
}

func TestEmptyObjectFormatting(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`{}`, `{"providers": {"example": ` + testProvider + `}}`},
		{`{ }`, `{ "providers": {"example": ` + testProvider + `}}`},
		{"{\n}\n", "{\n  \"providers\": {\"example\": " + testProvider + "}\n}\n"},
		{"{\n  /* note */\n}\n", "{\n  /* note */\n  \"providers\": {\"example\": " + testProvider + "}\n}\n"},
	}
	for _, test := range tests {
		got, _, err := UpsertProvider([]byte(test.input), "example", []byte(testProvider), false)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != test.want {
			t.Errorf("input %q produced %q, want %q", test.input, got, test.want)
		}
	}
}

func TestUpsertProviderPreservesCRLFStyle(t *testing.T) {
	input := []byte("{\r\n  \"setting\": true,\r\n}\r\n")
	got, changed, err := UpsertProvider(input, "example", []byte(testProvider), false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("UpsertProvider reported no change")
	}
	want := "{\r\n  \"setting\": true,\r\n  \"providers\": {\"example\": " + testProvider + "}\r\n}\r\n"
	if string(got) != want {
		t.Fatalf("output %q, want %q", got, want)
	}
	if bytes.Contains(bytes.ReplaceAll(got, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatalf("output introduced a bare line feed: %q", got)
	}
}

func FuzzProviderEditsRemainValidJSONC(f *testing.F) {
	for _, input := range []string{
		`{}`,
		`{"providers":{}}`,
		`{"providers":{"other":{}}}`,
		"{\n// note\n\"providers\":{\"other\":{\"list\":[1,2,],},},\n}\n",
		`{"providers":{"example":{"old":true}}}`,
		`not JSON`,
	} {
		f.Add([]byte(input))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		got, _, err := UpsertProvider(input, "example", []byte(testProvider), true)
		if err != nil {
			return
		}
		if _, err := parseJSONCDocument(got); err != nil {
			t.Fatalf("successful upsert returned invalid JSONC: %v", err)
		}
		raw, found, err := Provider(got, "example")
		if err != nil {
			t.Fatalf("read after successful upsert failed: %v", err)
		}
		if !found || string(raw) != testProvider {
			t.Fatalf("read after successful upsert returned %q, found=%v", raw, found)
		}
		removed, _, err := RemoveProvider(got, "example")
		if err != nil {
			t.Fatalf("remove after successful upsert failed: %v", err)
		}
		if _, err := parseJSONCDocument(removed); err != nil {
			t.Fatalf("successful removal returned invalid JSONC: %v", err)
		}
	})
}
