package pricing

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestNewIncludesBundledBasetenFallback(t *testing.T) {
	p := New()
	quote := p.Quote("baseten", "zai-org/GLM-5.2")
	if !quote.Priced {
		t.Fatal("bundled GLM-5.2 fallback is unpriced")
	}
	if !approx(quote.Price.Prompt, 1.4) ||
		!approx(quote.Price.Completion, 4.4) ||
		!approx(quote.Price.CacheRead, 0.14) ||
		!approx(quote.Price.CacheWrite5m, 0) {
		t.Fatalf("fallback price = %+v", quote.Price)
	}
	metadata := p.Capture().BasetenMetadata()
	if metadata.Source != basetenFallbackSource {
		t.Fatalf("source = %q, want %q", metadata.Source, basetenFallbackSource)
	}
	if metadata.ModelCount != 3 || metadata.PricedModelCount != 3 {
		t.Fatalf("fallback metadata = %+v", metadata)
	}
	for model, want := range map[string]Price{
		"zai-org/GLM-5.2": {
			Prompt: 1.4, Completion: 4.4, CacheRead: 0.14,
		},
		"moonshotai/Kimi-K2.7-Code": {
			Prompt: 0.95, Completion: 4, CacheRead: 0.16,
		},
		"nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B": {
			Prompt: 0.6, Completion: 2.4, CacheRead: 0.12,
		},
	} {
		got := p.BasetenPrice(model)
		if !approx(got.Prompt, want.Prompt) ||
			!approx(got.Completion, want.Completion) ||
			!approx(got.CacheRead, want.CacheRead) ||
			!approx(got.CacheWrite5m, 0) {
			t.Fatalf("%s fallback price = %+v, want %+v", model, got, want)
		}
	}
	if metadata.Revision == "" {
		t.Fatal("fallback revision is empty")
	}
	if want := time.Date(2026, time.July, 25, 20, 46, 58, 0, time.UTC); !metadata.FetchedAt.Equal(want) {
		t.Fatalf("fallback fetched at = %s, want %s", metadata.FetchedAt, want)
	}
}

func TestEmbeddedFallbackProvenanceMatchesCompiledMetadata(t *testing.T) {
	var envelope struct {
		SchemaVersion int    `json:"schema_version"`
		Source        string `json:"source"`
		FetchedAt     string `json:"fetched_at"`
		SourceSHA256  string `json:"source_sha256"`
	}
	if err := json.Unmarshal(basetenFallbackJSON, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", envelope.SchemaVersion)
	}
	fetchedAt, err := time.Parse(time.RFC3339, envelope.FetchedAt)
	if err != nil {
		t.Fatalf("fetched_at = %q: %v", envelope.FetchedAt, err)
	}
	if len(envelope.SourceSHA256) != 64 ||
		strings.Trim(envelope.SourceSHA256, "0123456789abcdef") != "" {
		t.Fatalf("source_sha256 = %q, want lowercase SHA-256", envelope.SourceSHA256)
	}

	metadata := New().Capture().BasetenMetadata()
	if metadata.Source != basetenFallbackSource {
		t.Fatalf("metadata source = %q, want %q", metadata.Source, basetenFallbackSource)
	}
	if !metadata.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("metadata fetched_at = %s, envelope = %s", metadata.FetchedAt, fetchedAt)
	}
	for _, field := range []string{
		"source=" + envelope.Source,
		"fetched=" + envelope.FetchedAt,
		"source_sha256=" + envelope.SourceSHA256,
	} {
		if !strings.Contains(metadata.Provenance, field) {
			t.Fatalf("metadata provenance %q does not contain %q", metadata.Provenance, field)
		}
	}
}

func TestReplaceBasetenCatalogReplacesAndRemoves(t *testing.T) {
	p := New()
	first := catalogJSON(
		catalogModel{id: "model-a", prompt: 0.000001},
		catalogModel{id: "model-b", prompt: 0.000002},
	)
	if err := p.ReplaceBasetenCatalog(first, "live", time.Unix(10, 0), ""); err != nil {
		t.Fatal(err)
	}
	if p.BasetenModelCount() != 2 {
		t.Fatalf("first model count = %d, want 2", p.BasetenModelCount())
	}

	second := catalogJSON(catalogModel{id: "model-b", prompt: 0.000003})
	if err := p.ReplaceBasetenCatalog(second, "live", time.Unix(20, 0), ""); err != nil {
		t.Fatal(err)
	}
	if p.Quote("baseten", "model-a").Priced {
		t.Fatal("removed model-a survived replacement")
	}
	if got := p.BasetenPrice("model-b").Prompt; got != 3 {
		t.Fatalf("model-b prompt = %v, want 3", got)
	}
	if p.BasetenModelCount() != 1 {
		t.Fatalf("second model count = %d, want 1", p.BasetenModelCount())
	}
}

func TestBasetenCatalogTracksMissingCacheRates(t *testing.T) {
	p := New()
	body := []byte(`{
		"data": [{
			"id": "model-a",
			"pricing": {"prompt": 0.000001, "completion": 0.000002}
		}]
	}`)
	if err := p.ReplaceBasetenCatalog(
		body,
		"baseten_v1_models",
		time.Unix(10, 0),
		"",
	); err != nil {
		t.Fatal(err)
	}
	quote := p.Quote(ProviderBaseten, "model-a")
	if !quote.Priced || !quote.RatePresenceKnown ||
		!quote.RatePresence.Input ||
		!quote.RatePresence.Output ||
		quote.RatePresence.CacheRead ||
		quote.RatePresence.CacheWrite5m {
		t.Fatalf("Baseten rate presence = %+v", quote)
	}
	if !quote.HasRatesForUsage(1, 1, 0, 0, 0) {
		t.Fatal("zero cache usage required missing Baseten cache rates")
	}
	if quote.HasRatesForUsage(1, 1, 1, 0, 0) {
		t.Fatal("nonzero cache usage accepted missing Baseten cache-read rate")
	}
}

func TestInvalidBasetenRefreshRetainsLastKnownGood(t *testing.T) {
	p := New()
	good := catalogJSON(catalogModel{id: "model-a", prompt: 0.000002})
	fetchedAt := time.Unix(100, 0).UTC()
	if err := p.ReplaceBasetenCatalog(good, "live", fetchedAt, ""); err != nil {
		t.Fatal(err)
	}
	before := p.Capture()
	beforeMetadata := before.BasetenMetadata()

	bad := []byte(`{"data":[{"id":"model-a","pricing":{"prompt":-1}}]}`)
	if err := p.ReplaceBasetenCatalog(bad, "broken", time.Unix(200, 0), ""); err == nil {
		t.Fatal("negative pricing refresh succeeded")
	}
	after := p.Capture()
	if after != before {
		t.Fatal("invalid refresh published a new snapshot")
	}
	if after.BasetenMetadata() != beforeMetadata {
		t.Fatalf("metadata changed after failure: before=%+v after=%+v",
			beforeMetadata, after.BasetenMetadata())
	}
	if got := after.Quote("baseten", "model-a").Price.Prompt; got != 2 {
		t.Fatalf("last-known-good prompt = %v, want 2", got)
	}
}

func TestBasetenRevisionIndependentOfResponseOrder(t *testing.T) {
	modelA := catalogModel{id: "model-a", prompt: 0.000001, completion: 0.000002}
	modelB := catalogModel{id: "model-b", prompt: 0.000003, completion: 0.000004}
	p1 := New()
	p2 := New()
	if err := p1.ReplaceBasetenCatalog(
		catalogJSON(modelA, modelB), "one", time.Unix(10, 0), "",
	); err != nil {
		t.Fatal(err)
	}
	if err := p2.ReplaceBasetenCatalog(
		catalogJSON(modelB, modelA), "two", time.Unix(20, 0), "",
	); err != nil {
		t.Fatal(err)
	}
	revision1 := p1.Capture().BasetenMetadata().Revision
	revision2 := p2.Capture().BasetenMetadata().Revision
	if revision1 != revision2 {
		t.Fatalf("revisions differ by response order: %q != %q", revision1, revision2)
	}
}

func TestBasetenRevisionDistinguishesUnpricedFromPricedZero(t *testing.T) {
	unpriced := New()
	if err := unpriced.ReplaceBasetenCatalog(
		[]byte(`{"data":[{"id":"model-a"}]}`),
		"live",
		time.Unix(10, 0),
		"",
	); err != nil {
		t.Fatal(err)
	}
	pricedZero := New()
	if err := pricedZero.ReplaceBasetenCatalog(
		[]byte(`{"data":[{"id":"model-a","pricing":{"prompt":0,"completion":0}}]}`),
		"live",
		time.Unix(10, 0),
		"",
	); err != nil {
		t.Fatal(err)
	}
	if unpriced.Capture().BasetenMetadata().Revision ==
		pricedZero.Capture().BasetenMetadata().Revision {
		t.Fatal("unpriced and explicit zero-price catalogs have the same revision")
	}
}

func TestCapturedSnapshotStableAcrossRefresh(t *testing.T) {
	p := New()
	if err := p.ReplaceBasetenCatalog(
		catalogJSON(catalogModel{id: "model-a", prompt: 0.000001}),
		"revision-a",
		time.Unix(10, 0),
		"",
	); err != nil {
		t.Fatal(err)
	}
	captured := p.Capture()
	oldQuote := captured.Quote("baseten", "model-a")

	if err := p.ReplaceBasetenCatalog(
		catalogJSON(catalogModel{id: "model-a", prompt: 0.000009}),
		"revision-b",
		time.Unix(20, 0),
		"",
	); err != nil {
		t.Fatal(err)
	}
	if got := captured.Quote("baseten", "model-a"); got != oldQuote {
		t.Fatalf("captured quote changed: before=%+v after=%+v", oldQuote, got)
	}
	if got := p.Quote("baseten", "model-a").Price.Prompt; got != 9 {
		t.Fatalf("current prompt = %v, want 9", got)
	}
	if oldQuote.CostUSD(1_000_000, 0, 0, 0, 0) != 1 {
		t.Fatalf("captured cost = %v, want 1", oldQuote.CostUSD(1_000_000, 0, 0, 0, 0))
	}
}

func TestQuoteNanoUSDRatesAndCheckedCost(t *testing.T) {
	p := New()
	quote := p.Quote("baseten", "zai-org/GLM-5.2")
	rates, err := quote.NanoUSDRates()
	if err != nil {
		t.Fatal(err)
	}
	if rates != (NanoUSDRates{
		Prompt:       1400,
		Completion:   4400,
		CacheRead:    140,
		CacheWrite5m: 0,
	}) {
		t.Fatalf("nano-USD rates = %+v", rates)
	}
	cost, err := rates.CostNanoUSD(100, 10, 20, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cost != 186_800 {
		t.Fatalf("cost = %d nano-USD, want 186800", cost)
	}
	if _, err := rates.CostNanoUSD(math.MaxInt64, 1, 0, 0, 0); err == nil {
		t.Fatal("overflowing cost succeeded")
	}
	if _, err := (Quote{}).NanoUSDRates(); err != ErrUnpriced {
		t.Fatalf("unpriced error = %v, want ErrUnpriced", err)
	}
	rounded, err := (Quote{
		Priced: true,
		Price:  Price{Prompt: 0.0006},
	}).NanoUSDRates()
	if err != nil || rounded.Prompt != 1 {
		t.Fatalf("rounded rates = %+v, err=%v, want prompt=1", rounded, err)
	}
	if _, err := (Quote{
		Priced: true,
		Price:  Price{Prompt: 0.0004},
	}).NanoUSDRates(); err == nil {
		t.Fatal("positive rate that rounds to zero succeeded")
	}
}

func TestExplicitZeroBasetenRatesRemainPriced(t *testing.T) {
	p := New()
	body := []byte(`{
	  "data": [
	    {
	      "id": "free/model",
	      "pricing": {"prompt": 0, "completion": 0}
	    }
	  ]
	}`)
	if err := p.ReplaceBasetenCatalog(body, "live", time.Unix(10, 0), ""); err != nil {
		t.Fatal(err)
	}
	quote := p.Quote("baseten", "free/model")
	if !quote.Priced {
		t.Fatal("explicit zero rates became unpriced")
	}
	rates, err := quote.NanoUSDRates()
	if err != nil {
		t.Fatal(err)
	}
	if rates != (NanoUSDRates{}) {
		t.Fatalf("zero rates = %+v", rates)
	}
	cost, err := rates.CostNanoUSD(100, 200, 300, 400, 0)
	if err != nil || cost != 0 {
		t.Fatalf("priced-zero cost = %d, err=%v", cost, err)
	}
}

type catalogModel struct {
	id         string
	prompt     float64
	completion float64
}

func catalogJSON(models ...catalogModel) []byte {
	out := `{"data":[`
	for i, model := range models {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf(
			`{"id":%q,"pricing":{"prompt":%g,"completion":%g}}`,
			model.id,
			model.prompt,
			model.completion,
		)
	}
	out += "]}"
	return []byte(out)
}
