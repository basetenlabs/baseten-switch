package pricing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const providerCacheSchemaVersion = 1

type providerCacheEnvelope struct {
	// Source, Revision, and CapturedAt identify the highest-authority
	// provider metadata in the merged cache. ETag and ValidatedAt belong only
	// to the models.dev root represented by field-level record provenance.
	SchemaVersion   int                          `json:"schema_version"`
	Provider        string                       `json:"provider"`
	Source          string                       `json:"source"`
	ETag            string                       `json:"etag,omitempty"`
	Revision        string                       `json:"revision"`
	CapturedAt      time.Time                    `json:"captured_at"`
	ValidatedAt     time.Time                    `json:"validated_at,omitempty"`
	ReplacesPricing *bool                        `json:"replaces_pricing"`
	PricingCatalog  *providerCachePricingCatalog `json:"pricing_catalog,omitempty"`
	ContentSHA256   string                       `json:"content_sha256"`
	Models          map[string]ModelRecord       `json:"models"`
}

type providerCachePricingCatalog struct {
	Source           string    `json:"source"`
	Provenance       string    `json:"provenance,omitempty"`
	Revision         string    `json:"revision"`
	FetchedAt        time.Time `json:"fetched_at"`
	PricedModelCount int       `json:"priced_model_count"`
	Models           []string  `json:"models"`
}

// ExportProviderCache serializes the active normalized provider catalog. The
// caller owns atomic private-file persistence.
func (s *Snapshot) ExportProviderCache(provider string) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("pricing snapshot is nil")
	}
	if !supportedProvider(provider) {
		return nil, fmt.Errorf("provider %q is unsupported", provider)
	}
	cacheableLayers := make(map[providerLayerKey]providerCatalog)
	for key, candidate := range s.providerLayers {
		if key.provider != provider ||
			key.loadedFrom == LoadedFromVendoredFallback {
			continue
		}
		cacheableLayers[key] = candidate
	}
	catalog := activeProviderCatalogs(cacheableLayers)[provider]
	if len(catalog.models) == 0 {
		return nil, nil
	}
	models := make(map[string]ModelRecord, len(catalog.models))
	for id, record := range catalog.models {
		models[id] = cloneModelRecord(record)
	}
	contentSHA, err := cachedModelsSHA256(models)
	if err != nil {
		return nil, err
	}
	replacesPricing := false
	for _, layer := range cacheableLayers {
		if layer.replacesPricing {
			replacesPricing = true
			break
		}
	}
	var pricingCatalog *providerCachePricingCatalog
	if provider == ProviderBaseten {
		authority := activeBasetenPricingCatalog(cacheableLayers)
		if authority.metadata.Source != "" {
			pricingCatalog = &providerCachePricingCatalog{
				Source:           authority.metadata.Source,
				Provenance:       authority.metadata.Provenance,
				Revision:         authority.metadata.Revision,
				FetchedAt:        authority.metadata.FetchedAt,
				PricedModelCount: authority.metadata.PricedModelCount,
				Models:           append([]string(nil), authority.models...),
			}
		}
	}
	envelope := providerCacheEnvelope{
		SchemaVersion: providerCacheSchemaVersion,
		Provider:      provider,
		Source:        catalog.metadata.Provenance.Source,
		// ETag is the models.dev root validator. The remaining provenance
		// fields describe the active merged provider metadata.
		ETag:            s.ModelsDevETag(provider),
		Revision:        catalog.metadata.Provenance.Revision,
		CapturedAt:      catalog.metadata.Provenance.CapturedAt,
		ValidatedAt:     catalog.metadata.ModelsDevValidatedAt,
		ReplacesPricing: &replacesPricing,
		PricingCatalog:  pricingCatalog,
		ContentSHA256:   contentSHA,
		Models:          models,
	}
	if cachedModelsContainModelsDev(models) &&
		envelope.ValidatedAt.IsZero() {
		return nil, fmt.Errorf(
			"models.dev provider cache validation time is unavailable",
		)
	}
	return json.Marshal(envelope)
}

func (p *Pricing) ExportProviderCache(provider string) ([]byte, error) {
	return p.Capture().ExportProviderCache(provider)
}

// ImportProviderCache validates one provider-scoped runtime cache and
// atomically publishes it below live data and above vendored fallback data.
func (p *Pricing) ImportProviderCache(body []byte) error {
	var envelope providerCacheEnvelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("decode provider cache: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("provider cache contains multiple JSON values")
		}
		return fmt.Errorf("decode provider cache trailing data: %w", err)
	}
	if envelope.SchemaVersion != providerCacheSchemaVersion {
		return fmt.Errorf(
			"provider cache schema_version = %d, want %d",
			envelope.SchemaVersion,
			providerCacheSchemaVersion,
		)
	}
	if envelope.ReplacesPricing == nil {
		return fmt.Errorf("provider cache replaces_pricing is required")
	}
	if !supportedProvider(envelope.Provider) {
		return fmt.Errorf("provider cache provider %q is unsupported", envelope.Provider)
	}
	if strings.TrimSpace(envelope.Source) == "" ||
		strings.TrimSpace(envelope.Revision) == "" ||
		envelope.CapturedAt.IsZero() {
		return fmt.Errorf("provider cache provenance is incomplete")
	}
	hasModelsDev := cachedModelsContainModelsDev(envelope.Models)
	if hasModelsDev && envelope.ValidatedAt.IsZero() {
		return fmt.Errorf("provider cache validated_at is required")
	}
	if len(envelope.Models) == 0 {
		return fmt.Errorf("provider cache contains no models")
	}
	if err := validateProviderCachePricingCatalog(envelope); err != nil {
		return err
	}
	contentSHA, err := cachedModelsSHA256(envelope.Models)
	if err != nil {
		return err
	}
	if envelope.ContentSHA256 != contentSHA {
		return fmt.Errorf("provider cache content_sha256 does not match its models")
	}
	provenanceETag := ""
	if envelope.Source == modelsDevSource {
		provenanceETag = envelope.ETag
	}
	cacheProvenance := Provenance{
		Source: envelope.Source, LoadedFrom: LoadedFromRuntimeCache,
		Revision: envelope.Revision, CapturedAt: envelope.CapturedAt.UTC(),
		ETag: provenanceETag,
	}
	validatedAt := envelope.ValidatedAt.UTC()
	if !hasModelsDev {
		validatedAt = time.Time{}
	}
	records := make(map[string]ModelRecord, len(envelope.Models))
	priced := 0
	hasAccountAvailability := false
	for id, record := range envelope.Models {
		if id == "" || record.CanonicalModelID != id {
			return fmt.Errorf("provider cache model key %q does not match canonical id %q", id, record.CanonicalModelID)
		}
		if record.Provider != envelope.Provider {
			return fmt.Errorf("provider cache model %q has provider %q", id, record.Provider)
		}
		record = markModelLoadedFrom(record, LoadedFromRuntimeCache)
		for profile, price := range record.Prices {
			if !price.RatePresenceKnown {
				return fmt.Errorf(
					"provider cache model %q profile %q omitted rate presence",
					id,
					profile,
				)
			}
		}
		if err := validateModelRecord(record); err != nil {
			return fmt.Errorf("provider cache model %q: %w", id, err)
		}
		if _, ok := record.Prices[ProfileStandard]; ok {
			priced++
		}
		if record.Availability.Account != nil {
			hasAccountAvailability = true
		}
		records[id] = cloneModelRecord(record)
	}
	catalog := providerCatalog{
		metadata: ProviderMetadata{
			Provider: envelope.Provider, Provenance: cacheProvenance,
			ModelsDevValidatedAt: validatedAt,
			ModelCount:           len(records), PricedModelCount: priced,
		},
		models:                      records,
		replacesAccountAvailability: hasAccountAvailability,
		replacesPricing:             *envelope.ReplacesPricing,
	}
	if envelope.PricingCatalog != nil {
		catalog.basetenPricing = &basetenPricingCatalog{
			metadata: CatalogMetadata{
				Source:           envelope.PricingCatalog.Source,
				Provenance:       envelope.PricingCatalog.Provenance,
				Revision:         envelope.PricingCatalog.Revision,
				FetchedAt:        envelope.PricingCatalog.FetchedAt.UTC(),
				ModelCount:       len(envelope.PricingCatalog.Models),
				PricedModelCount: envelope.PricingCatalog.PricedModelCount,
			},
			models: append([]string(nil), envelope.PricingCatalog.Models...),
		}
	}

	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	current := p.current.Load()
	layers := cloneProviderLayers(current.providerLayers)
	for key := range layers {
		if key.provider == envelope.Provider && key.loadedFrom == LoadedFromRuntimeCache {
			delete(layers, key)
		}
	}
	layers[providerLayerKey{
		provider: envelope.Provider, loadedFrom: LoadedFromRuntimeCache,
		source: envelope.Source,
	}] = catalog
	p.publishLocked(cloneSnapshotWithLayers(current, layers))
	return nil
}

func validateProviderCachePricingCatalog(
	envelope providerCacheEnvelope,
) error {
	replacesPricing := *envelope.ReplacesPricing
	if !replacesPricing {
		if envelope.PricingCatalog != nil {
			return fmt.Errorf(
				"provider cache pricing_catalog requires replaces_pricing",
			)
		}
		return nil
	}
	if envelope.Provider != ProviderBaseten {
		return fmt.Errorf(
			"provider cache replaces_pricing is only valid for Baseten",
		)
	}
	catalog := envelope.PricingCatalog
	if catalog == nil {
		return fmt.Errorf(
			"Baseten provider cache pricing_catalog is required",
		)
	}
	if strings.TrimSpace(catalog.Source) == "" ||
		strings.TrimSpace(catalog.Revision) == "" ||
		catalog.FetchedAt.IsZero() ||
		len(catalog.Models) == 0 {
		return fmt.Errorf(
			"Baseten provider cache pricing_catalog is incomplete",
		)
	}
	if catalog.PricedModelCount < 0 ||
		catalog.PricedModelCount > len(catalog.Models) {
		return fmt.Errorf(
			"Baseten provider cache priced_model_count is invalid",
		)
	}
	sorted := append([]string(nil), catalog.Models...)
	sort.Strings(sorted)
	for index, id := range catalog.Models {
		if strings.TrimSpace(id) == "" ||
			id != sorted[index] ||
			(index > 0 && id == catalog.Models[index-1]) {
			return fmt.Errorf(
				"Baseten provider cache pricing models must be unique and sorted",
			)
		}
		if _, ok := envelope.Models[id]; !ok {
			return fmt.Errorf(
				"Baseten pricing model %q is absent from provider cache",
				id,
			)
		}
	}
	priced := 0
	for _, id := range catalog.Models {
		if _, ok := envelope.Models[id].Prices[ProfileStandard]; ok {
			priced++
		}
	}
	if priced != catalog.PricedModelCount {
		return fmt.Errorf(
			"Baseten provider cache priced_model_count = %d, want %d",
			catalog.PricedModelCount,
			priced,
		)
	}
	return nil
}

func cachedModelsContainModelsDev(
	models map[string]ModelRecord,
) bool {
	for _, record := range models {
		if record.Provenance.Source == modelsDevSource {
			return true
		}
		if record.Availability.Public != nil &&
			record.Availability.Public.Provenance.Source ==
				modelsDevSource {
			return true
		}
		if record.Reasoning != nil &&
			record.Reasoning.Provenance.Source == modelsDevSource {
			return true
		}
		for _, definition := range record.Profiles {
			if definition.Provenance.Source == modelsDevSource {
				return true
			}
		}
		for _, price := range record.Prices {
			if price.Provenance.Source == modelsDevSource {
				return true
			}
		}
	}
	return false
}

func markModelLoadedFrom(record ModelRecord, loadedFrom LoadedFrom) ModelRecord {
	record.Provenance.LoadedFrom = loadedFrom
	if record.Availability.Public != nil {
		evidence := *record.Availability.Public
		evidence.Provenance.LoadedFrom = loadedFrom
		record.Availability.Public = &evidence
	}
	if record.Availability.Account != nil {
		evidence := *record.Availability.Account
		evidence.Provenance.LoadedFrom = loadedFrom
		record.Availability.Account = &evidence
	}
	for profile, definition := range record.Profiles {
		definition.Provenance.LoadedFrom = loadedFrom
		record.Profiles[profile] = definition
	}
	for profile, price := range record.Prices {
		price.Provenance.LoadedFrom = loadedFrom
		for _, dimension := range allRateDimensions() {
			provenance := rateDimensionProvenance(
				price.RateProvenance,
				dimension,
			)
			if provenance.Source == "" {
				continue
			}
			provenance.LoadedFrom = loadedFrom
			setRateDimensionProvenance(
				&price.RateProvenance,
				dimension,
				provenance,
			)
		}
		record.Prices[profile] = price
	}
	if record.Reasoning != nil {
		record.Reasoning.Provenance.LoadedFrom = loadedFrom
	}
	return record
}

func cachedModelsSHA256(models map[string]ModelRecord) (string, error) {
	encoded, err := json.Marshal(models)
	if err != nil {
		return "", fmt.Errorf("encode provider cache models: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}
