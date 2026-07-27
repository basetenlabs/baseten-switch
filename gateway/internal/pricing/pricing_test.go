package pricing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFreshPricingHasNoNativeProviderPrices(t *testing.T) {
	p := New()
	for _, provider := range []string{ProviderAnthropic, ProviderOpenAI} {
		if quote := p.Quote(provider, "any-model"); quote.Priced {
			t.Fatalf("fresh %s quote = %+v, want unpriced", provider, quote)
		}
		if models := p.Capture().Models(provider); len(models) != 0 {
			t.Fatalf("fresh %s models = %+v, want none", provider, models)
		}
	}
}

func TestCostUSDUnknownModel(t *testing.T) {
	p := New()
	if c := p.Quote("anthropic", "nope").CostUSD(100, 100, 0, 0, 0); c != 0 {
		t.Fatalf("expected 0 for unknown, got %v", c)
	}
}

func TestHydrateFromBasetenMock(t *testing.T) {
	resp := map[string]interface{}{
		"data": []map[string]interface{}{
			{
				"id": "zai-org/GLM-5.2",
				"pricing": map[string]interface{}{
					"prompt":            0.0000014,
					"completion":        0.0000014,
					"input_cache_read":  0.0000001,
					"input_cache_write": 0.0000014,
				},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer testkey" {
			t.Fatalf("bad auth header: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := New()
	if err := p.HydrateFromBaseten(srv.URL, "testkey", "zai-org/GLM-5.2"); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	got := p.BasetenPrice("zai-org/GLM-5.2")
	if !approx(got.Prompt, 1.4) {
		t.Fatalf("prompt = %v want 1.4", got.Prompt)
	}
	if !approx(got.Completion, 1.4) {
		t.Fatalf("completion = %v want 1.4", got.Completion)
	}
	if !approx(got.CacheRead, 0.1) {
		t.Fatalf("cache_read = %v want 0.1", got.CacheRead)
	}
	if !approx(got.CacheWrite5m, 1.4) {
		t.Fatalf("cache_write = %v want 1.4", got.CacheWrite5m)
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

func TestHydrateExpectedModelMissingErrors(t *testing.T) {
	resp := map[string]interface{}{
		"data": []map[string]interface{}{
			{"id": "other-model", "pricing": map[string]interface{}{"prompt": 0.000001}},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := New()
	if err := p.HydrateFromBaseten(srv.URL, "k", "zai-org/GLM-5.2"); err == nil {
		t.Fatal("expected error for missing expected model")
	}
}

func TestCostUSDKnownTuple(t *testing.T) {
	tbl := Price{Prompt: 1.4, Completion: 1.4, CacheRead: 0.1, CacheWrite5m: 1.4}
	p := NewWithPrices(map[string]Price{"zai-org/GLM-5.2": tbl})
	in, out, cr, cw := int64(1000), int64(500), int64(200), int64(100)
	want := (1000*1.4 + 500*1.4 + 200*0.1 + 100*1.4) / 1e6
	got := p.Quote("baseten", "zai-org/GLM-5.2").CostUSD(in, out, cr, cw, 0)
	if got != want {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestCostUSDBasetenUnknownModelZero(t *testing.T) {
	p := New()
	if c := p.Quote("baseten", "nope").CostUSD(100, 100, 0, 0, 0); c != 0 {
		t.Fatalf("expected 0, got %v", c)
	}
}
