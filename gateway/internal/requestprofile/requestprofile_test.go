package requestprofile

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestInspect(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		raw           string
		canonical     string
		contextBudget *int64
		speed         string
		speedPresent  bool
		inferenceGeo  string
		geoPresent    bool
	}{
		{
			name:          "one million fast",
			body:          `{"model":"claude-opus-5[1m]","speed":"fast","inference_geo":"global","messages":[]}`,
			raw:           "claude-opus-5[1m]",
			canonical:     "claude-opus-5",
			contextBudget: int64Pointer(OneMillionContextTokens),
			speed:         "fast",
			speedPresent:  true,
			inferenceGeo:  "global",
			geoPresent:    true,
		},
		{
			name:         "standard canonical model",
			body:         `{"model":"claude-opus-5","speed":"standard"}`,
			raw:          "claude-opus-5",
			canonical:    "claude-opus-5",
			speed:        "standard",
			speedPresent: true,
		},
		{
			name:      "unknown decoration is model identity",
			body:      `{"model":"claude-opus-5[preview]"}`,
			raw:       "claude-opus-5[preview]",
			canonical: "claude-opus-5[preview]",
		},
		{
			name:      "only exact lowercase decoration is recognized",
			body:      `{"model":"claude-opus-5[1M]"}`,
			raw:       "claude-opus-5[1M]",
			canonical: "claude-opus-5[1M]",
		},
		{
			name:         "unexpected field types",
			body:         `{"model":123,"speed":true,"inference_geo":true}`,
			speedPresent: true,
			geoPresent:   true,
		},
		{
			name: "invalid JSON",
			body: `{`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Inspect([]byte(test.body))
			if got.RawModel != test.raw {
				t.Fatalf("RawModel = %q, want %q", got.RawModel, test.raw)
			}
			if got.CanonicalModel != test.canonical {
				t.Fatalf("CanonicalModel = %q, want %q", got.CanonicalModel, test.canonical)
			}
			if !reflect.DeepEqual(got.RequestedContextBudgetTokens, test.contextBudget) {
				t.Fatalf("RequestedContextBudgetTokens = %v, want %v", got.RequestedContextBudgetTokens, test.contextBudget)
			}
			if got.RequestedSpeed != test.speed {
				t.Fatalf("RequestedSpeed = %q, want %q", got.RequestedSpeed, test.speed)
			}
			if got.RequestedSpeedPresent != test.speedPresent {
				t.Fatalf(
					"RequestedSpeedPresent = %t, want %t",
					got.RequestedSpeedPresent,
					test.speedPresent,
				)
			}
			if got.RequestedInferenceGeo != test.inferenceGeo {
				t.Fatalf(
					"RequestedInferenceGeo = %q, want %q",
					got.RequestedInferenceGeo,
					test.inferenceGeo,
				)
			}
			if got.RequestedInferenceGeoPresent != test.geoPresent {
				t.Fatalf(
					"RequestedInferenceGeoPresent = %t, want %t",
					got.RequestedInferenceGeoPresent,
					test.geoPresent,
				)
			}
		})
	}
}

func TestInspectFindsOneHourCacheControl(t *testing.T) {
	profile := Inspect([]byte(`{
		"model":"claude-opus-5",
		"messages":[{
			"role":"user",
			"content":[{
				"type":"text",
				"text":"hello",
				"cache_control":{"type":"ephemeral","ttl":"1h"}
			}]
		}]
	}`))
	if !profile.RequestedOneHourCache {
		t.Fatal("one-hour cache control was not detected")
	}
	plain := Inspect([]byte(`{
		"model":"claude-opus-5",
		"messages":[{"content":[{"cache_control":{"ttl":"5m"}}]}]
	}`))
	if plain.RequestedOneHourCache {
		t.Fatal("five-minute cache control was classified as one-hour")
	}
}

func TestRemoveUnsupportedFastProfile(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"speed":"fast",
		"metadata":{"speed":"nested","trace":"keep"},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	headers := http.Header{}
	headers.Add("Anthropic-Beta", "context-1m-2025-08-07, "+AnthropicFastBeta)
	headers.Add("Anthropic-Beta", "other-preview")
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("X-Custom", "keep")
	originalHeaders := headers.Clone()
	originalBody := append([]byte(nil), body...)

	projectedBody, projectedHeaders, changed := RemoveUnsupportedFastProfile(body, headers)
	if !changed {
		t.Fatal("projection did not report a change")
	}
	if !bytes.Equal(body, originalBody) {
		t.Fatal("input body was mutated")
	}
	if !reflect.DeepEqual(headers, originalHeaders) {
		t.Fatal("input headers were mutated")
	}

	var projected map[string]any
	if err := json.Unmarshal(projectedBody, &projected); err != nil {
		t.Fatalf("projected body is invalid JSON: %v", err)
	}
	if _, ok := projected["speed"]; ok {
		t.Fatal("top-level speed was not removed")
	}
	metadata := projected["metadata"].(map[string]any)
	if metadata["speed"] != "nested" || metadata["trace"] != "keep" {
		t.Fatalf("nested metadata changed: %#v", metadata)
	}
	if projected["model"] != "claude-opus-5" {
		t.Fatalf("model changed: %#v", projected["model"])
	}

	if got := projectedHeaders.Values("Anthropic-Beta"); !reflect.DeepEqual(got, []string{"context-1m-2025-08-07", "other-preview"}) {
		t.Fatalf("Anthropic-Beta = %#v", got)
	}
	if projectedHeaders.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatal("Anthropic-Version changed")
	}
	if projectedHeaders.Get("X-Custom") != "keep" {
		t.Fatal("custom header changed")
	}
}

func TestRemoveUnsupportedFastProfileHeaderOnlyWithInvalidBody(t *testing.T) {
	body := []byte(`{`)
	headers := http.Header{"Anthropic-Beta": []string{AnthropicFastBeta}}

	projectedBody, projectedHeaders, changed := RemoveUnsupportedFastProfile(body, headers)
	if !changed {
		t.Fatal("header projection did not report a change")
	}
	if !bytes.Equal(projectedBody, body) {
		t.Fatal("invalid JSON body changed")
	}
	if projectedHeaders.Get("Anthropic-Beta") != "" {
		t.Fatalf("fast beta remained: %q", projectedHeaders.Get("Anthropic-Beta"))
	}
}

func TestRemoveUnsupportedFastProfileNoopIsByteIdentical(t *testing.T) {
	body := []byte("{ \"model\": \"claude-opus-5\", \"messages\": [] }\n")
	headers := http.Header{
		"Anthropic-Beta": []string{"context-1m-2025-08-07"},
		"X-Custom":       []string{"keep"},
	}

	projectedBody, projectedHeaders, changed := RemoveUnsupportedFastProfile(body, headers)
	if changed {
		t.Fatal("no-op projection reported a change")
	}
	if !bytes.Equal(projectedBody, body) {
		t.Fatalf("no-op changed body bytes:\nwant %q\ngot  %q", body, projectedBody)
	}
	if !reflect.DeepEqual(projectedHeaders, headers) {
		t.Fatalf("no-op changed headers:\nwant %#v\ngot  %#v", headers, projectedHeaders)
	}
	projectedHeaders.Set("X-Custom", "changed")
	if headers.Get("X-Custom") != "keep" {
		t.Fatal("projected headers share storage with the input")
	}
}

func TestRemoveUnsupportedFastProfileRemovesAllFastBetaOccurrences(t *testing.T) {
	body := []byte(`{"speed":"fast"}`)
	headers := http.Header{}
	headers.Add("Anthropic-Beta", AnthropicFastBeta+", keep-one")
	headers.Add("Anthropic-Beta", "KEEP-TWO, FAST-MODE-2026-02-01")

	_, projectedHeaders, changed := RemoveUnsupportedFastProfile(body, headers)
	if !changed {
		t.Fatal("projection did not report a change")
	}
	if got := projectedHeaders.Values("Anthropic-Beta"); !reflect.DeepEqual(got, []string{"keep-one", "KEEP-TWO"}) {
		t.Fatalf("Anthropic-Beta = %#v", got)
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}
