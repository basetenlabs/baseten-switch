package analytics

import (
	"sort"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/internal/modelmeta"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

type groupKey struct {
	provider string
	model    string
}

type accumulator struct {
	requests int
	tokens   int64
	cost     float64
	priced   int
	unpriced int
	ttft     []int64
	tps      []float64
}

type savingsAccumulator struct {
	actual    float64
	estimated float64
}

var claudeFamilyOrder = map[string]int{
	"fable": 0, "opus": 1, "sonnet": 2, "haiku": 3, "other": 4,
}

// Build projects presentation metadata from one immutable catalog snapshot
// for the entire response. A nil catalog uses deterministic local metadata.
func Build(
	events []telemetry.EventV1,
	window Window,
	generatedAt int64,
	retained Snapshot,
	collectionEnabled bool,
	catalog *pricing.Snapshot,
) Response {
	response := Response{
		GeneratedAt: generatedAt,
		Window:      window,
		Coverage: Coverage{
			Complete:          retained.Complete,
			Reason:            retained.Reason,
			CollectionEnabled: collectionEnabled,
		},
		Cost: Cost{
			Providers: []CostGroup{},
			Models:    []CostGroup{},
			Savings: Savings{
				ByBasetenModel: []SavingsModel{},
				Mappings:       []SavingsMapping{},
			},
		},
		Performance: Performance{
			Providers: []PerformanceGroup{},
			Models:    []PerformanceGroup{},
		},
	}
	if !retained.Earliest.IsZero() {
		value := retained.Earliest.Unix()
		response.Coverage.EarliestCompletedAt = &value
	}
	if !retained.Latest.IsZero() {
		value := retained.Latest.Unix()
		response.Coverage.LatestCompletedAt = &value
	}

	costGroups := map[groupKey]*accumulator{}
	perfGroups := map[groupKey]*accumulator{}
	savingsModels := map[string]*savingsAccumulator{}
	savingsMappings := map[string]*savingsAccumulator{}
	var savingsEligibleActual float64

	for _, event := range events {
		ts := event.CompletedAt.Unix()
		if ts < window.Since || ts >= window.Until {
			continue
		}
		response.Coverage.RequestRows++
		tokens, usageComplete := tokenTotal(event)
		if !usageComplete {
			response.Coverage.IncompleteUsageRows++
		}

		provider, ok := providerFor(event)
		if !ok {
			response.Coverage.UnpricedActualCostRows++
			continue
		}
		modelID := modelIDFor(event)
		requestedModel := event.RequestedModel
		if requestedModel == "" {
			requestedModel = modelID
		}
		model := modelMetadata(
			catalog,
			provider,
			modelID,
			event.RequestedModelFamily,
			requestedModel,
		)

		perf := getAccumulator(perfGroups, groupKey{provider: provider, model: model.ID})
		perf.requests++
		if usageComplete {
			perf.tokens += tokens
			if event.TTFTMS != nil && *event.TTFTMS > 0 {
				perf.ttft = append(perf.ttft, *event.TTFTMS)
			}
			if event.Usage.OutputTokens != nil && *event.Usage.OutputTokens > 0 &&
				event.TTFTMS != nil && event.DurationMS > *event.TTFTMS {
				seconds := float64(event.DurationMS-*event.TTFTMS) / 1000
				perf.tps = append(
					perf.tps,
					float64(*event.Usage.OutputTokens)/seconds,
				)
			}
		}

		actualUSD, actualPriced := persistedUSD(event.ActualCost)
		cost := getAccumulator(costGroups, groupKey{provider: provider, model: model.ID})
		cost.requests++
		if usageComplete {
			cost.tokens += tokens
		}
		if actualPriced {
			response.Coverage.PricedActualCostRows++
			cost.priced++
			cost.cost += actualUSD
			if provider == "Claude" {
				response.Cost.Summary.ActualClaudeCostUSD += actualUSD
			} else {
				response.Cost.Summary.ActualBasetenCostUSD += actualUSD
			}
		} else {
			response.Coverage.UnpricedActualCostRows++
			cost.unpriced++
		}

		if provider != "Baseten" {
			continue
		}
		estimated, counterfactualPriced := persistedCounterfactualUSD(
			event.NativeCounterfactualCost,
		)
		if !actualPriced || !counterfactualPriced {
			response.Coverage.SavingsUnpricedRows++
			if !counterfactualPriced {
				response.Coverage.UnpricedCounterfactualRows++
			}
			continue
		}

		response.Coverage.SavingsEligibleRows++
		response.Cost.Summary.EstimatedNativeCostForBasetenUSD += estimated
		savingsEligibleActual += actualUSD

		sm := savingsModels[model.ID]
		if sm == nil {
			sm = &savingsAccumulator{}
			savingsModels[model.ID] = sm
		}
		sm.actual += actualUSD
		sm.estimated += estimated

		family := modelmeta.ResolveClaudeFamily(
			event.RequestedModelFamily,
			requestedModel,
		)
		mappingKey := model.ID + "\x00" + family.DisplayName
		mapping := savingsMappings[mappingKey]
		if mapping == nil {
			mapping = &savingsAccumulator{}
			savingsMappings[mappingKey] = mapping
		}
		mapping.actual += actualUSD
		mapping.estimated += estimated
	}

	response.Cost.Summary.SavedUSD =
		response.Cost.Summary.EstimatedNativeCostForBasetenUSD -
			savingsEligibleActual
	response.Cost.Summary.SavedPercent = savedPercent(
		response.Cost.Summary.SavedUSD,
		response.Cost.Summary.EstimatedNativeCostForBasetenUSD,
	)
	response.Cost.Providers, response.Cost.Models = materializeCost(catalog, costGroups)
	response.Performance.Providers, response.Performance.Models =
		materializePerformance(catalog, perfGroups)
	response.Cost.Savings.ByBasetenModel =
		materializeSavingsModels(catalog, savingsModels)
	response.Cost.Savings.Mappings =
		materializeSavingsMappings(catalog, savingsMappings)
	return response
}

func getAccumulator(groups map[groupKey]*accumulator, key groupKey) *accumulator {
	value := groups[key]
	if value == nil {
		value = &accumulator{}
		groups[key] = value
	}
	return value
}

func providerFor(event telemetry.EventV1) (string, bool) {
	switch strings.ToLower(event.EffectiveProvider) {
	case "anthropic":
		return "Claude", true
	case "baseten":
		return "Baseten", true
	default:
		return "", false
	}
}

func modelIDFor(event telemetry.EventV1) string {
	if event.ServedModel != "" {
		return event.ServedModel
	}
	return event.RequestedModel
}

func tokenTotal(event telemetry.EventV1) (int64, bool) {
	if !event.UsageComplete ||
		event.Usage.InputTokens == nil ||
		event.Usage.OutputTokens == nil ||
		event.Usage.CacheReadInputTokens == nil {
		return 0, false
	}
	hasExactCacheWrites := event.Usage.CacheWrite5mInputTokens != nil &&
		event.Usage.CacheWrite1hInputTokens != nil
	if !hasExactCacheWrites &&
		event.Usage.CacheWriteTotalInputTokens == nil {
		return 0, false
	}
	return *event.Usage.InputTokens + *event.Usage.OutputTokens, true
}

func persistedUSD(cost telemetry.CostSnapshotV1) (float64, bool) {
	if !cost.Priced || cost.NanoUSD == nil {
		return 0, false
	}
	return float64(*cost.NanoUSD) / 1_000_000_000, true
}

func persistedCounterfactualUSD(cost *telemetry.CostSnapshotV1) (float64, bool) {
	if cost == nil {
		return 0, false
	}
	return persistedUSD(*cost)
}

func modelMetadata(
	catalog *pricing.Snapshot,
	provider string,
	modelID string,
	requestedFamily string,
	requestedModel string,
) modelmeta.Model {
	if provider == "Claude" {
		return modelmeta.ResolveClaudeFamily(requestedFamily, requestedModel)
	}
	metadata := modelmeta.ResolveBaseten(modelID)
	if catalog != nil {
		if displayName, ok := catalog.DisplayName(
			pricing.ProviderBaseten,
			modelID,
		); ok {
			metadata.DisplayName = displayName
		}
	}
	return metadata
}

func materializeCost(
	catalog *pricing.Snapshot,
	groups map[groupKey]*accumulator,
) ([]CostGroup, []CostGroup) {
	providerGroups := map[string]*accumulator{}
	models := make([]CostGroup, 0, len(groups))
	for key, value := range groups {
		provider := providerGroups[key.provider]
		if provider == nil {
			provider = &accumulator{}
			providerGroups[key.provider] = provider
		}
		provider.requests += value.requests
		provider.tokens += value.tokens
		provider.cost += value.cost
		provider.priced += value.priced
		provider.unpriced += value.unpriced
		model := modelMetadata(catalog, key.provider, key.model, "", key.model)
		models = append(models, CostGroup{
			Provider: key.provider, ModelID: model.ID, DisplayName: model.DisplayName,
			Requests: value.requests,
			Tokens:   value.tokens, ActualCostUSD: knownCost(value),
			PricedRows: value.priced, UnpricedRows: value.unpriced,
		})
	}
	providers := make([]CostGroup, 0, len(providerGroups))
	for key, value := range providerGroups {
		providers = append(providers, CostGroup{
			Provider: key, Requests: value.requests, Tokens: value.tokens,
			ActualCostUSD: knownCost(value),
			PricedRows:    value.priced, UnpricedRows: value.unpriced,
		})
	}
	sortCostGroups(providers)
	sortCostGroups(models)
	return providers, models
}

func knownCost(value *accumulator) *float64 {
	if value.priced == 0 {
		return nil
	}
	cost := value.cost
	return &cost
}

func materializePerformance(
	catalog *pricing.Snapshot,
	groups map[groupKey]*accumulator,
) ([]PerformanceGroup, []PerformanceGroup) {
	providerGroups := map[string]*accumulator{}
	models := make([]PerformanceGroup, 0, len(groups))
	for key, value := range groups {
		provider := providerGroups[key.provider]
		if provider == nil {
			provider = &accumulator{}
			providerGroups[key.provider] = provider
		}
		provider.requests += value.requests
		provider.tokens += value.tokens
		provider.ttft = append(provider.ttft, value.ttft...)
		provider.tps = append(provider.tps, value.tps...)
		models = append(
			models,
			performanceGroup(catalog, key.provider, key.model, value),
		)
	}
	providers := make([]PerformanceGroup, 0, len(providerGroups))
	for key, value := range providerGroups {
		providers = append(
			providers,
			performanceGroup(catalog, key, "", value),
		)
	}
	sortPerformanceGroups(providers)
	sortPerformanceGroups(models)
	return providers, models
}

func performanceGroup(
	catalog *pricing.Snapshot,
	provider,
	model string,
	value *accumulator,
) PerformanceGroup {
	metadata := modelmeta.Model{}
	if model != "" {
		metadata = modelMetadata(catalog, provider, model, "", model)
	}
	return PerformanceGroup{
		Provider: provider, ModelID: metadata.ID, DisplayName: metadata.DisplayName,
		Requests: value.requests, Tokens: value.tokens,
		TTFTSamples: len(value.ttft), MedianTTFTMs: medianInt64(value.ttft),
		OutputTPSSamples: len(value.tps), MedianOutputTokensPerSecond: medianFloat64(value.tps),
	}
}

func materializeSavingsModels(
	catalog *pricing.Snapshot,
	groups map[string]*savingsAccumulator,
) []SavingsModel {
	result := make([]SavingsModel, 0, len(groups))
	for model, value := range groups {
		saved := value.estimated - value.actual
		metadata := modelMetadata(catalog, "Baseten", model, "", model)
		result = append(result, SavingsModel{
			ModelID: metadata.ID, DisplayName: metadata.DisplayName,
			ActualBasetenCostUSD:   value.actual,
			EstimatedNativeCostUSD: value.estimated, SavedUSD: saved,
			SavedPercent: savedPercent(saved, value.estimated),
		})
	}
	sort.Slice(result, func(a, b int) bool { return result[a].ModelID < result[b].ModelID })
	return result
}

func materializeSavingsMappings(
	catalog *pricing.Snapshot,
	groups map[string]*savingsAccumulator,
) []SavingsMapping {
	result := make([]SavingsMapping, 0, len(groups))
	for key, value := range groups {
		parts := strings.SplitN(key, "\x00", 2)
		metadata := modelMetadata(catalog, "Baseten", parts[0], "", parts[0])
		result = append(result, SavingsMapping{
			BasetenModelID: metadata.ID, BasetenDisplayName: metadata.DisplayName,
			RequestedClaudeFamily: parts[1],
			ActualBasetenCostUSD:  value.actual, EstimatedNativeCostUSD: value.estimated,
		})
	}
	sort.Slice(result, func(a, b int) bool {
		if result[a].BasetenModelID != result[b].BasetenModelID {
			return result[a].BasetenModelID < result[b].BasetenModelID
		}
		return claudeFamilyOrder[strings.ToLower(result[a].RequestedClaudeFamily)] <
			claudeFamilyOrder[strings.ToLower(result[b].RequestedClaudeFamily)]
	})
	return result
}

func sortCostGroups(groups []CostGroup) {
	sort.Slice(groups, func(a, b int) bool {
		return groupLess(
			groups[a].Provider,
			groups[a].ModelID,
			groups[b].Provider,
			groups[b].ModelID,
		)
	})
}

func sortPerformanceGroups(groups []PerformanceGroup) {
	sort.Slice(groups, func(a, b int) bool {
		return groupLess(
			groups[a].Provider,
			groups[a].ModelID,
			groups[b].Provider,
			groups[b].ModelID,
		)
	})
}

func groupLess(ap, am, bp, bm string) bool {
	if ap != bp {
		return ap == "Claude"
	}
	if ap == "Claude" {
		return claudeFamilyOrder[am] < claudeFamilyOrder[bm]
	}
	return am < bm
}

func savedPercent(saved, estimated float64) float64 {
	if estimated == 0 {
		return 0
	}
	return saved / estimated * 100
}

func medianInt64(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func medianFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
