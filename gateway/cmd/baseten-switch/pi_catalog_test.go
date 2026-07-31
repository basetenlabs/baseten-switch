package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

const piModelsDevFixture = `{
  "anthropic": {
    "id": "anthropic",
    "models": {
      "claude-test": {
        "id": "claude-test",
        "name": "Claude Test",
        "reasoning": false,
        "modalities": {"input": ["text"], "output": ["text"]}
      }
    }
  },
  "openai": {
    "id": "openai",
    "models": {
      "gpt-test": {
        "id": "gpt-test",
        "name": "GPT Test",
        "reasoning": false,
        "modalities": {"input": ["text"], "output": ["text"]}
      }
    }
  },
  "baseten": {
    "id": "baseten",
    "models": {
      "example/vision": {
        "id": "example/vision",
        "name": "Vision",
        "reasoning": true,
        "reasoning_options": [{"type": "toggle"}],
        "modalities": {"input": ["text", "image", "audio"], "output": ["text"]}
      },
      "example/text": {
        "id": "example/text",
        "name": "Text",
        "reasoning": false,
        "modalities": {"input": ["text"], "output": ["text"]}
      }
    }
  }
}`

func TestModelsDevPiCapabilitySourceEnrichesExactModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet ||
				r.Header.Get("Accept") != "application/json" ||
				r.Header.Get("Authorization") != "" ||
				r.Header.Get("X-Api-Key") != "" {
				t.Fatalf("unexpected request: method=%s headers=%v", r.Method, r.Header)
			}
			w.Header().Set("ETag", `"fixture"`)
			_, _ = io.WriteString(w, piModelsDevFixture)
		},
	))
	defer server.Close()

	source := modelsDevPiCapabilitySource{
		client: server.Client(), endpoint: server.URL,
		timeout: time.Second,
		now: func() time.Time {
			return time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
		},
	}
	models := []piProviderModel{
		{ID: "example/vision"},
		{ID: "example/text"},
		{ID: "example/missing"},
	}
	result, err := source.Enrich(context.Background(), models)
	if err != nil {
		t.Fatal(err)
	}
	if result.Matched != 2 {
		t.Fatalf("matched = %d, want 2", result.Matched)
	}
	if !result.Models[0].Reasoning ||
		!result.Models[0].CapabilityKnown ||
		!reflect.DeepEqual(result.Models[0].Input, []string{"text", "image"}) {
		t.Fatalf("vision model = %#v", result.Models[0])
	}
	if result.Models[1].Reasoning ||
		!result.Models[1].CapabilityKnown ||
		!reflect.DeepEqual(result.Models[1].Input, []string{"text"}) {
		t.Fatalf("text model = %#v", result.Models[1])
	}
	if result.Models[2].Reasoning ||
		result.Models[2].CapabilityKnown ||
		!reflect.DeepEqual(result.Models[2].Input, []string{"text"}) {
		t.Fatalf("missing model = %#v", result.Models[2])
	}
	if models[0].Input != nil || models[0].Reasoning {
		t.Fatalf("source mutated input models: %#v", models)
	}
}

func TestModelsDevPiCapabilitySourceRejectsInvalidCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"baseten":`)
		},
	))
	defer server.Close()
	source := modelsDevPiCapabilitySource{
		client: server.Client(), endpoint: server.URL, timeout: time.Second,
	}
	if _, err := source.Enrich(
		context.Background(),
		[]piProviderModel{{ID: "example/model"}},
	); err == nil {
		t.Fatal("expected invalid catalog error")
	}
}

func TestPiInputModalities(t *testing.T) {
	for _, test := range []struct {
		name   string
		source []string
		want   []string
		ok     bool
	}{
		{"text", []string{"text"}, []string{"text"}, true},
		{"image", []string{"text", "image"}, []string{"text", "image"}, true},
		{"unsupported ignored", []string{"text", "audio"}, []string{"text"}, true},
		{"no text", []string{"image"}, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := piInputModalities(test.source)
			if ok != test.ok || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("piInputModalities(%v) = %v, %v; want %v, %v",
					test.source, got, ok, test.want, test.ok)
			}
		})
	}
}
