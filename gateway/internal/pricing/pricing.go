package pricing

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxBasetenCatalogBytes = 32 << 20

// Price stores provider rates in USD per one million tokens.
type Price struct {
	Prompt       float64 `json:"input"`
	Completion   float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
}

// RatePresence distinguishes an explicitly priced zero from a rate dimension
// omitted by the source. Every persisted provider-cache quote includes this
// metadata.
type RatePresence struct {
	Input        bool `json:"input"`
	Output       bool `json:"output"`
	CacheRead    bool `json:"cache_read"`
	CacheWrite5m bool `json:"cache_write_5m"`
	CacheWrite1h bool `json:"cache_write_1h"`
}

// Quote is one immutable model-price lookup. Callers retain this value for the
// lifetime of a request, then apply final usage without consulting a newer
// catalog.
type Quote struct {
	Price             Price
	Priced            bool
	RatePresence      RatePresence
	RatePresenceKnown bool
	RateProvenance    RateProvenance
	Source            string
	Revision          string
	CapturedAt        time.Time
	ExecutionProfile  ExecutionProfile
}

var ErrUnpriced = errors.New("pricing quote is unpriced")

// NanoUSDRates stores integer nano-USD rates per token for telemetry v1.
type NanoUSDRates struct {
	Prompt       int64
	Completion   int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}

// NanoUSDRates converts the captured USD-per-million rates into checked
// integer nano-USD-per-token rates using the telemetry-v1 rounding rule.
func (q Quote) NanoUSDRates() (NanoUSDRates, error) {
	if !q.Priced {
		return NanoUSDRates{}, ErrUnpriced
	}
	values := []float64{
		q.Price.Prompt,
		q.Price.Completion,
		q.Price.CacheRead,
		q.Price.CacheWrite5m,
		q.Price.CacheWrite1h,
	}
	converted := make([]int64, len(values))
	for index, value := range values {
		rate, err := nanoUSDPerToken(value)
		if err != nil {
			return NanoUSDRates{}, err
		}
		converted[index] = rate
	}
	return NanoUSDRates{
		Prompt:       converted[0],
		Completion:   converted[1],
		CacheRead:    converted[2],
		CacheWrite5m: converted[3],
		CacheWrite1h: converted[4],
	}, nil
}

// HasRatesForUsage reports whether every nonzero token dimension has a known
// rate. In-memory fallback quotes without source presence metadata are treated
// as complete.
func (q Quote) HasRatesForUsage(
	in,
	out,
	cacheRead,
	cacheWrite5m,
	cacheWrite1h int64,
) bool {
	if !q.Priced {
		return false
	}
	if !q.RatePresenceKnown {
		return true
	}
	return (in == 0 || q.RatePresence.Input) &&
		(out == 0 || q.RatePresence.Output) &&
		(cacheRead == 0 || q.RatePresence.CacheRead) &&
		(cacheWrite5m == 0 || q.RatePresence.CacheWrite5m) &&
		(cacheWrite1h == 0 || q.RatePresence.CacheWrite1h)
}

// CostNanoUSD applies usage with checked integer multiplication and addition.
func (rates NanoUSDRates) CostNanoUSD(
	in,
	out,
	cacheRead,
	cacheWrite5m,
	cacheWrite1h int64,
) (int64, error) {
	tokens := []int64{in, out, cacheRead, cacheWrite5m, cacheWrite1h}
	values := []int64{
		rates.Prompt,
		rates.Completion,
		rates.CacheRead,
		rates.CacheWrite5m,
		rates.CacheWrite1h,
	}
	var total int64
	for index, tokenCount := range tokens {
		if tokenCount < 0 {
			return 0, fmt.Errorf("token count must be nonnegative")
		}
		rate := values[index]
		if rate < 0 {
			return 0, fmt.Errorf("nano-USD rate must be nonnegative")
		}
		if tokenCount != 0 && rate > math.MaxInt64/tokenCount {
			return 0, fmt.Errorf("nano-USD cost overflow")
		}
		part := tokenCount * rate
		if part > math.MaxInt64-total {
			return 0, fmt.Errorf("nano-USD cost overflow")
		}
		total += part
	}
	return total, nil
}

func nanoUSDPerToken(usdPerMillion float64) (int64, error) {
	if math.IsNaN(usdPerMillion) || math.IsInf(usdPerMillion, 0) || usdPerMillion < 0 {
		return 0, fmt.Errorf("USD-per-million rate must be finite and nonnegative")
	}
	value := usdPerMillion * 1000
	rounded := math.Round(value)
	// float64 cannot distinguish the final 1,024 integer values below
	// MaxInt64. Reject that boundary conservatively rather than risk an
	// implementation-dependent out-of-range float-to-int conversion.
	if rounded >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("USD-per-million rate %g overflows nano-USD-per-token", usdPerMillion)
	}
	if usdPerMillion > 0 && rounded == 0 {
		return 0, fmt.Errorf("positive USD-per-million rate %g rounds to zero nano-USD per token", usdPerMillion)
	}
	return int64(rounded), nil
}

// CostUSD applies observed usage to this quote. An unpriced quote returns zero;
// callers must inspect Priced to distinguish unknown cost from a priced zero.
func (q Quote) CostUSD(
	in,
	out,
	cacheRead,
	cacheWrite5m,
	cacheWrite1h int64,
) float64 {
	return costUSD(
		q.Price,
		in,
		out,
		cacheRead,
		cacheWrite5m,
		cacheWrite1h,
	)
}

// CatalogMetadata describes the Baseten catalog held by a Snapshot.
type CatalogMetadata struct {
	Source           string
	Provenance       string
	Revision         string
	FetchedAt        time.Time
	ModelCount       int
	PricedModelCount int
}

// Snapshot is immutable after publication. Its maps and slices are private so
// a captured request cannot accidentally mutate the catalog shared by other
// requests.
type Snapshot struct {
	providerLayers            map[providerLayerKey]providerCatalog
	providerCatalogs          map[string]providerCatalog
	officialPricingSupplement officialPricingSupplement
}

// Quote returns a price from this exact snapshot.
func (s *Snapshot) Quote(route, model string) Quote {
	return s.QuoteProfile(route, model, ProfileStandard)
}

// BasetenMetadata returns metadata for this exact snapshot.
func (s *Snapshot) BasetenMetadata() CatalogMetadata {
	if s == nil {
		return CatalogMetadata{}
	}
	return activeBasetenPricingCatalog(s.providerLayers).metadata
}

// BasetenModels returns a copy of the catalog's sorted model IDs.
func (s *Snapshot) BasetenModels() []string {
	if s == nil {
		return nil
	}
	return activeBasetenPricingCatalog(s.providerLayers).models
}

// Pricing atomically publishes immutable pricing snapshots.
type Pricing struct {
	current atomic.Pointer[Snapshot]

	publishMu sync.Mutex
}

//go:embed baseten_fallback_prices.json
var basetenFallbackJSON []byte

//go:embed baseten_reasoning_fallback.json
var basetenReasoningFallbackJSON []byte

const basetenFallbackSource = "baseten_embedded_fallback"

// New constructs pricing with the bundled Baseten fallback. Live /v1/models
// hydration replaces the fallback atomically after a complete valid response.
func New() *Pricing {
	var envelope struct {
		Source       string `json:"source"`
		FetchedAt    string `json:"fetched_at"`
		SourceSHA256 string `json:"source_sha256"`
	}
	if err := json.Unmarshal(basetenFallbackJSON, &envelope); err != nil {
		panic("invalid embedded Baseten fallback envelope: " + err.Error())
	}
	fetchedAt, err := time.Parse(time.RFC3339, envelope.FetchedAt)
	if err != nil {
		panic("invalid embedded Baseten fallback fetched_at: " + err.Error())
	}
	if envelope.Source == "" || envelope.SourceSHA256 == "" {
		panic("invalid embedded Baseten fallback provenance")
	}
	candidate, err := parseBasetenCatalog(
		basetenFallbackJSON,
		basetenFallbackSource,
		fetchedAt,
	)
	if err != nil {
		panic("invalid embedded Baseten fallback pricing: " + err.Error())
	}
	provenance := fmt.Sprintf(
		"source=%s; fetched=%s; source_sha256=%s",
		envelope.Source,
		envelope.FetchedAt,
		envelope.SourceSHA256,
	)
	p := &Pricing{}
	supplement, err := parseOfficialPricingSupplement(
		officialPricingSupplementJSON,
	)
	if err != nil {
		panic("invalid embedded official pricing supplement: " + err.Error())
	}
	fallbackRevision := "baseten-embedded-" +
		fetchedAt.Format(time.DateOnly) + "+" + candidate.revision
	snapshot := &Snapshot{
		providerLayers:            map[providerLayerKey]providerCatalog{},
		officialPricingSupplement: supplement,
	}
	embeddedProvenance := Provenance{
		Source: basetenFallbackSource, LoadedFrom: LoadedFromVendoredFallback,
		Revision: fallbackRevision, CapturedAt: candidate.fetchedAt,
	}
	snapshot.providerLayers[providerLayerKey{
		provider: ProviderBaseten, loadedFrom: LoadedFromVendoredFallback,
		source: basetenFallbackSource,
	}] = basetenProviderCatalog(
		candidate,
		embeddedProvenance,
		provenance,
		false,
	)
	fallbackCatalog := snapshot.providerLayers[providerLayerKey{
		provider: ProviderBaseten, loadedFrom: LoadedFromVendoredFallback,
		source: basetenFallbackSource,
	}]
	if err := attachVendoredBasetenReasoning(&fallbackCatalog); err != nil {
		panic("invalid embedded Baseten reasoning fallback: " + err.Error())
	}
	snapshot.providerLayers[providerLayerKey{
		provider: ProviderBaseten, loadedFrom: LoadedFromVendoredFallback,
		source: basetenFallbackSource,
	}] = fallbackCatalog
	snapshot.providerCatalogs = activeProviderCatalogsForSnapshot(snapshot)
	p.current.Store(snapshot)
	return p
}

// NewWithPrices constructs a static Baseten snapshot for tests and embedders.
func NewWithPrices(baseten map[string]Price) *Pricing {
	p := New()
	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	basetenCopy := clonePrices(baseten)
	models := sortedModelIDs(basetenCopy)
	snapshot := &Snapshot{
		providerLayers: cloneProviderLayers(p.current.Load().providerLayers),
		officialPricingSupplement: cloneOfficialPricingSupplement(
			p.current.Load().officialPricingSupplement,
		),
	}
	capturedAt := time.Now().UTC()
	if len(basetenCopy) > 0 {
		snapshot.providerLayers[providerLayerKey{
			provider: ProviderBaseten, loadedFrom: LoadedFromLive, source: "static",
		}] = staticBasetenProviderCatalog(basetenCopy, models, capturedAt)
	}
	snapshot.providerCatalogs = activeProviderCatalogsForSnapshot(snapshot)
	p.publishLocked(snapshot)
	return p
}

// Capture returns the currently published immutable pricing snapshot.
func (p *Pricing) Capture() *Snapshot {
	if p == nil {
		return nil
	}
	return p.current.Load()
}

// Quote performs a one-shot lookup against one atomically loaded snapshot.
func (p *Pricing) Quote(route, model string) Quote {
	return p.Capture().Quote(route, model)
}

func (p *Pricing) HydrateFromBaseten(baseURL, apiKey, expectedModel string) error {
	url := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bearer "+apiKey)
	req.Header.Set("accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	return p.hydrateFromRequest(client, req, expectedModel)
}

func (p *Pricing) HydrateFromBasetenClient(client *http.Client, baseURL, expectedModel string) error {
	url := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	return p.hydrateFromRequest(client, req, expectedModel)
}

func (p *Pricing) hydrateFromRequest(client *http.Client, req *http.Request, expectedModel string) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("baseten /v1/models returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBasetenCatalogBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxBasetenCatalogBytes {
		return fmt.Errorf("baseten /v1/models response exceeds %d bytes", maxBasetenCatalogBytes)
	}
	return p.ReplaceBasetenCatalog(body, "baseten_v1_models", time.Now().UTC(), expectedModel)
}

// ReplaceBasetenCatalog validates a complete /v1/models response and
// atomically replaces the live Baseten catalog. Any error leaves the
// last-known-good snapshot untouched.
func (p *Pricing) ReplaceBasetenCatalog(body []byte, source string, fetchedAt time.Time, expectedModel string) error {
	candidate, err := parseBasetenCatalog(body, source, fetchedAt)
	if err != nil {
		return err
	}
	if expectedModel != "" && !containsSorted(candidate.models, expectedModel) {
		return fmt.Errorf("expected model %q not present in Baseten /v1/models catalog", expectedModel)
	}

	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	current := p.current.Load()
	snapshot := &Snapshot{
		providerLayers: cloneProviderLayers(current.providerLayers),
		officialPricingSupplement: cloneOfficialPricingSupplement(
			current.officialPricingSupplement,
		),
	}
	provenance := Provenance{
		Source: candidate.source, LoadedFrom: LoadedFromLive,
		Revision: candidate.revision, CapturedAt: candidate.fetchedAt,
	}
	snapshot.providerLayers[providerLayerKey{
		provider: ProviderBaseten, loadedFrom: LoadedFromLive, source: candidate.source,
	}] = basetenProviderCatalog(candidate, provenance, "", true)
	snapshot.providerCatalogs = activeProviderCatalogsForSnapshot(snapshot)
	p.publishLocked(snapshot)
	return nil
}

func (p *Pricing) publishLocked(snapshot *Snapshot) {
	if snapshot.providerLayers == nil {
		snapshot.providerLayers = map[providerLayerKey]providerCatalog{}
	}
	if snapshot.providerCatalogs == nil {
		snapshot.providerCatalogs = activeProviderCatalogsForSnapshot(snapshot)
	}
	p.current.Store(snapshot)
}

type basetenCandidate struct {
	prices       map[string]Price
	ratePresence map[string]RatePresence
	models       []string
	source       string
	revision     string
	fetchedAt    time.Time
}

func basetenProviderCatalog(
	candidate basetenCandidate,
	provenance Provenance,
	provenanceDescription string,
	replacesPricing bool,
) providerCatalog {
	records := make(map[string]ModelRecord, len(candidate.models))
	authenticatedAvailability :=
		provenance.Source == "baseten_v1_models" ||
			provenance.Source == "baseten-v1-models"
	for _, id := range candidate.models {
		availability := ModelAvailability{}
		if authenticatedAvailability {
			availability.Account = &AvailabilityEvidence{
				State:      AvailabilityAvailable,
				Scope:      AvailabilityScopeUnscopedLastObserved,
				Provenance: provenance,
			}
		}
		record := ModelRecord{
			Provider: ProviderBaseten, CanonicalModelID: id,
			Availability: availability,
			Profiles: map[ExecutionProfile]ProfileDefinition{
				ProfileStandard: {
					Profile: ProfileStandard, Supported: true, Provenance: provenance,
				},
			},
			Prices:     map[ExecutionProfile]PriceProfile{},
			Provenance: provenance,
		}
		if price, ok := candidate.prices[id]; ok {
			record.Prices[ProfileStandard] = PriceProfile{
				Profile: ProfileStandard, Price: price,
				RatePresence: candidate.ratePresence[id], RatePresenceKnown: true,
				Provenance: provenance,
				RateProvenance: rateProvenanceForPresence(
					candidate.ratePresence[id],
					provenance,
				),
			}
		}
		records[id] = record
	}
	return providerCatalog{
		metadata: ProviderMetadata{
			Provider: ProviderBaseten, Provenance: provenance,
			ModelCount: len(records), PricedModelCount: len(candidate.prices),
		},
		models:                      records,
		replacesAccountAvailability: authenticatedAvailability,
		replacesPricing:             replacesPricing,
		basetenPricing: &basetenPricingCatalog{
			metadata: CatalogMetadata{
				Source:           candidate.source,
				Provenance:       provenanceDescription,
				Revision:         provenance.Revision,
				FetchedAt:        candidate.fetchedAt,
				ModelCount:       len(candidate.models),
				PricedModelCount: len(candidate.prices),
			},
			models: append([]string(nil), candidate.models...),
		},
	}
}

func staticBasetenProviderCatalog(
	prices map[string]Price,
	models []string,
	capturedAt time.Time,
) providerCatalog {
	revision := priceTableRevision(prices)
	provenance := Provenance{
		Source: "static", LoadedFrom: LoadedFromLive,
		Revision: revision, CapturedAt: capturedAt,
	}
	records := make(map[string]ModelRecord, len(prices))
	for id, price := range prices {
		records[id] = ModelRecord{
			Provider: ProviderBaseten, CanonicalModelID: id, DisplayName: id,
			Profiles: map[ExecutionProfile]ProfileDefinition{
				ProfileStandard: {
					Profile: ProfileStandard, Supported: true, Provenance: provenance,
				},
			},
			Prices: map[ExecutionProfile]PriceProfile{
				ProfileStandard: {
					Profile: ProfileStandard, Price: price,
					RatePresence: RatePresence{
						Input:        true,
						Output:       true,
						CacheRead:    true,
						CacheWrite5m: true,
						CacheWrite1h: true,
					},
					RatePresenceKnown: true,
					Provenance:        provenance,
					RateProvenance: rateProvenanceForPresence(
						RatePresence{
							Input:        true,
							Output:       true,
							CacheRead:    true,
							CacheWrite5m: true,
							CacheWrite1h: true,
						},
						provenance,
					),
				},
			},
			Provenance: provenance,
		}
	}
	return providerCatalog{
		metadata: ProviderMetadata{
			Provider: ProviderBaseten, Provenance: provenance,
			ModelCount: len(records), PricedModelCount: len(records),
		},
		models:          records,
		replacesPricing: true,
		basetenPricing: &basetenPricingCatalog{
			metadata: CatalogMetadata{
				Source: "static", Provenance: "static",
				Revision: revision, FetchedAt: capturedAt,
				ModelCount: len(models), PricedModelCount: len(prices),
			},
			models: append([]string(nil), models...),
		},
	}
}

func parseBasetenCatalog(body []byte, source string, fetchedAt time.Time) (basetenCandidate, error) {
	var data struct {
		Data []struct {
			ID      string          `json:"id"`
			Pricing json.RawMessage `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return basetenCandidate{}, fmt.Errorf("decode Baseten catalog: %w", err)
	}
	if len(data.Data) == 0 {
		return basetenCandidate{}, fmt.Errorf("Baseten catalog contains no models")
	}

	prices := make(map[string]Price, len(data.Data))
	ratePresence := make(map[string]RatePresence, len(data.Data))
	models := make([]string, 0, len(data.Data))
	seen := make(map[string]bool, len(data.Data))
	for _, model := range data.Data {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return basetenCandidate{}, fmt.Errorf("Baseten catalog contains an empty model id")
		}
		if seen[id] {
			return basetenCandidate{}, fmt.Errorf("Baseten catalog contains duplicate model %q", id)
		}
		seen[id] = true
		models = append(models, id)

		price, presence, priced, err := parseBasetenPrice(model.Pricing)
		if err != nil {
			return basetenCandidate{}, fmt.Errorf("Baseten model %q pricing: %w", id, err)
		}
		if priced {
			prices[id] = price
			ratePresence[id] = presence
		}
	}
	sort.Strings(models)
	if source == "" {
		source = "baseten-v1-models"
	}
	return basetenCandidate{
		prices:       prices,
		ratePresence: ratePresence,
		models:       models,
		source:       source,
		revision:     catalogRevisionWithRatePresence(models, prices, ratePresence),
		fetchedAt:    fetchedAt,
	}, nil
}

func parseBasetenPrice(raw json.RawMessage) (Price, RatePresence, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return Price{}, RatePresence{}, false, nil
	}
	var fields struct {
		Prompt          json.RawMessage `json:"prompt"`
		Completion      json.RawMessage `json:"completion"`
		InputCacheRead  json.RawMessage `json:"input_cache_read"`
		InputCacheWrite json.RawMessage `json:"input_cache_write"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Price{}, RatePresence{}, false, err
	}
	presence := RatePresence{
		Input:        ratePresent(fields.Prompt),
		Output:       ratePresent(fields.Completion),
		CacheRead:    ratePresent(fields.InputCacheRead),
		CacheWrite5m: ratePresent(fields.InputCacheWrite),
	}
	values := []struct {
		name string
		raw  json.RawMessage
		out  *float64
	}{
		{name: "prompt", raw: fields.Prompt},
		{name: "completion", raw: fields.Completion},
		{name: "input_cache_read", raw: fields.InputCacheRead},
		{name: "input_cache_write", raw: fields.InputCacheWrite},
	}
	var price Price
	values[0].out = &price.Prompt
	values[1].out = &price.Completion
	values[2].out = &price.CacheRead
	values[3].out = &price.CacheWrite5m
	for _, value := range values {
		perToken, err := strictOptionalFloat(value.raw)
		if err != nil {
			return Price{}, RatePresence{}, false, fmt.Errorf("%s: %w", value.name, err)
		}
		*value.out = perToken * 1e6
	}
	return price, presence, presence.Input && presence.Output, nil
}

func strictOptionalFloat(raw json.RawMessage) (float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		value, err := number.Float64()
		if err != nil {
			return 0, fmt.Errorf("invalid number")
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return 0, fmt.Errorf("must be finite and nonnegative")
		}
		return value, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, fmt.Errorf("must be a number or numeric string")
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, fmt.Errorf("must be finite and nonnegative")
	}
	return value, nil
}

func (p *Pricing) CheckBasetenModel(expectedModel string) error {
	if expectedModel == "" {
		return nil
	}
	if !containsSorted(p.Capture().BasetenModels(), expectedModel) {
		return fmt.Errorf("expected model %q not present in Baseten /v1/models catalog", expectedModel)
	}
	return nil
}

func (p *Pricing) BasetenPrice(model string) Price {
	return p.Quote("baseten", model).Price
}

func (p *Pricing) BasetenCount() int {
	snapshot := p.Capture()
	if snapshot == nil {
		return 0
	}
	return snapshot.BasetenMetadata().PricedModelCount
}

func (p *Pricing) BasetenModelCount() int {
	return p.Capture().BasetenMetadata().ModelCount
}

func costUSD(
	price Price,
	in,
	out,
	cacheRead,
	cacheWrite5m,
	cacheWrite1h int64,
) float64 {
	return (float64(in)*price.Prompt +
		float64(out)*price.Completion +
		float64(cacheRead)*price.CacheRead +
		float64(cacheWrite5m)*price.CacheWrite5m +
		float64(cacheWrite1h)*price.CacheWrite1h) / 1e6
}

func clonePrices(source map[string]Price) map[string]Price {
	out := make(map[string]Price, len(source))
	for model, price := range source {
		out[model] = price
	}
	return out
}

func sortedModelIDs(prices map[string]Price) []string {
	models := make([]string, 0, len(prices))
	for model := range prices {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func containsSorted(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func catalogRevision(models []string, prices map[string]Price) string {
	hash := sha256.New()
	for _, model := range models {
		price, priced := prices[model]
		_, _ = io.WriteString(hash, model)
		_, _ = hash.Write([]byte{0})
		if priced {
			_, _ = hash.Write([]byte{1})
		} else {
			_, _ = hash.Write([]byte{0})
		}
		for _, value := range []float64{
			price.Prompt,
			price.Completion,
			price.CacheRead,
			price.CacheWrite5m,
			price.CacheWrite1h,
		} {
			_, _ = io.WriteString(hash, strconv.FormatFloat(value, 'g', -1, 64))
			_, _ = hash.Write([]byte{0})
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func catalogRevisionWithRatePresence(
	models []string,
	prices map[string]Price,
	presence map[string]RatePresence,
) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, catalogRevision(models, prices))
	for _, model := range models {
		value := presence[model]
		for _, present := range []bool{
			value.Input,
			value.Output,
			value.CacheRead,
			value.CacheWrite5m,
			value.CacheWrite1h,
		} {
			if present {
				_, _ = hash.Write([]byte{1})
			} else {
				_, _ = hash.Write([]byte{0})
			}
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func priceTableRevision(prices map[string]Price) string {
	return catalogRevision(sortedModelIDs(prices), prices)
}
