package pricing

import (
	"fmt"
	"strings"
	"time"
)

// AvailabilityModel is one account-visible model returned by an authenticated
// provider catalog. Pricing is deliberately absent.
type AvailabilityModel struct {
	CanonicalModelID string
	DisplayName      string
	ContextTokens    int64
	MaxOutputTokens  int64
}

// ReplaceProviderAvailability atomically replaces one authenticated provider
// availability layer. Existing public metadata and price profiles remain
// intact and are merged with the new account evidence.
func (p *Pricing) ReplaceProviderAvailability(
	provider string,
	models []AvailabilityModel,
	source string,
	capturedAt time.Time,
	revision string,
) error {
	if !supportedProvider(provider) {
		return fmt.Errorf("provider %q is unsupported", provider)
	}
	source = strings.TrimSpace(source)
	if source == "" || strings.TrimSpace(revision) == "" || capturedAt.IsZero() {
		return fmt.Errorf("provider availability provenance is incomplete")
	}
	if len(models) == 0 {
		return fmt.Errorf("provider availability contains no models")
	}
	provenance := Provenance{
		Source: source, LoadedFrom: LoadedFromLive,
		Revision: revision, CapturedAt: capturedAt.UTC(),
	}
	records := make(map[string]ModelRecord, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.CanonicalModelID)
		if id == "" {
			return fmt.Errorf("provider availability contains an empty model id")
		}
		if _, duplicate := records[id]; duplicate {
			return fmt.Errorf("provider availability contains duplicate model %q", id)
		}
		if model.ContextTokens < 0 || model.MaxOutputTokens < 0 {
			return fmt.Errorf("provider availability model %q has negative limits", id)
		}
		name := strings.TrimSpace(model.DisplayName)
		if name == "" {
			name = id
		}
		record := ModelRecord{
			Provider: provider, CanonicalModelID: id, DisplayName: name,
			ContextTokens:   model.ContextTokens,
			MaxOutputTokens: model.MaxOutputTokens,
			Availability: ModelAvailability{
				Account: &AvailabilityEvidence{
					State:      AvailabilityAvailable,
					Scope:      AvailabilityScopeUnscopedLastObserved,
					Provenance: provenance,
				},
			},
			Profiles:   map[ExecutionProfile]ProfileDefinition{},
			Prices:     map[ExecutionProfile]PriceProfile{},
			Provenance: provenance,
		}
		if err := validateModelRecord(record); err != nil {
			return fmt.Errorf("provider availability model %q: %w", id, err)
		}
		records[id] = record
	}
	catalog := providerCatalog{
		metadata: ProviderMetadata{
			Provider: provider, Provenance: provenance, ModelCount: len(records),
		},
		models:                      records,
		replacesAccountAvailability: true,
	}

	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	current := p.current.Load()
	layers := cloneProviderLayers(current.providerLayers)
	layers[providerLayerKey{
		provider: provider, loadedFrom: LoadedFromLive, source: source,
	}] = catalog
	p.publishLocked(cloneSnapshotWithLayers(current, layers))
	return nil
}
