package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

const anthropicAvailabilitySource = "anthropic_v1_models"

var anthropicAvailabilityNow = time.Now

type anthropicModelsPage struct {
	Data    []json.RawMessage `json:"data"`
	HasMore *bool             `json:"has_more"`
}

type anthropicModelAvailabilityEntry struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	MaxInputTokens int64  `json:"max_input_tokens"`
	MaxTokens      int64  `json:"max_tokens"`
}

// parseAnthropicModelsPage keeps the response used for immediate discovery
// separate from the normalized availability candidate that can be persisted.
// Provider capabilities and any unknown response fields are never retained in
// the runtime cache.
func parseAnthropicModelsPage(
	body []byte,
) (
	entries []map[string]any,
	models []pricing.AvailabilityModel,
	complete bool,
	err error,
) {
	var page anthropicModelsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, nil, false, fmt.Errorf("decode Anthropic model list: %w", err)
	}
	if len(page.Data) == 0 {
		return nil, nil, false, fmt.Errorf("Anthropic model list contains no models")
	}
	if page.HasMore == nil {
		return nil, nil, false, fmt.Errorf(
			"Anthropic model list omitted has_more",
		)
	}

	entries = make([]map[string]any, 0, len(page.Data))
	models = make([]pricing.AvailabilityModel, 0, len(page.Data))
	seen := make(map[string]struct{}, len(page.Data))
	for _, raw := range page.Data {
		var entry anthropicModelAvailabilityEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, nil, false, fmt.Errorf("decode Anthropic model entry: %w", err)
		}
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			return nil, nil, false, fmt.Errorf("Anthropic model entry has an empty id")
		}
		if entry.Type != "" && entry.Type != "model" {
			return nil, nil, false, fmt.Errorf(
				"Anthropic model %q has type %q",
				id,
				entry.Type,
			)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, nil, false, fmt.Errorf(
				"Anthropic model list contains duplicate id %q",
				id,
			)
		}
		if entry.MaxInputTokens < 0 || entry.MaxTokens < 0 {
			return nil, nil, false, fmt.Errorf(
				"Anthropic model %q has negative token limits",
				id,
			)
		}
		seen[id] = struct{}{}
		displayName := strings.TrimSpace(entry.DisplayName)
		if displayName == "" {
			displayName = id
		}
		models = append(models, pricing.AvailabilityModel{
			CanonicalModelID: id,
			DisplayName:      displayName,
			ContextTokens:    entry.MaxInputTokens,
			MaxOutputTokens:  entry.MaxTokens,
		})

		var discoveryEntry map[string]any
		if err := json.Unmarshal(raw, &discoveryEntry); err != nil {
			return nil, nil, false, fmt.Errorf(
				"decode Anthropic discovery entry: %w",
				err,
			)
		}
		entries = append(entries, discoveryEntry)
	}
	return entries, models, !*page.HasMore, nil
}

func availabilityRevision(models []pricing.AvailabilityModel) string {
	sorted := append([]pricing.AvailabilityModel(nil), models...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CanonicalModelID < sorted[j].CanonicalModelID
	})
	encoded, _ := json.Marshal(sorted)
	hash := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func (g *Gateway) publishAnthropicAvailability(
	models []pricing.AvailabilityModel,
) error {
	capturedAt := anthropicAvailabilityNow().UTC()
	if err := g.pricing.ReplaceProviderAvailability(
		pricing.ProviderAnthropic,
		models,
		anthropicAvailabilitySource,
		capturedAt,
		availabilityRevision(models),
	); err != nil {
		return err
	}
	configPath := strings.TrimSpace(g.runtimeConfig().ConfigPath)
	if configPath == "" {
		// In-process embedders and hermetic tests may not have a backing config
		// file. The live snapshot remains useful, but there is no cache scope.
		return nil
	}
	return g.persistProviderCatalogCaches()
}
