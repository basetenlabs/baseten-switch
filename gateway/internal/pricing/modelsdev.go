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

const modelsDevSource = "models_dev"

type modelsDevDiagnostics struct {
	unknownReasoningOptions int
	deprecatedModels        int
	unknownStatusModels     int
	unsupportedFastProfiles int
}

type modelsDevProvider struct {
	ID     string                     `json:"id"`
	Name   string                     `json:"name"`
	Models map[string]json.RawMessage `json:"models"`
}

type modelsDevModel struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Family           string          `json:"family"`
	Status           string          `json:"status"`
	Reasoning        json.RawMessage `json:"reasoning"`
	ReasoningOptions json.RawMessage `json:"reasoning_options"`
	Modalities       json.RawMessage `json:"modalities"`
	Cost             json.RawMessage `json:"cost"`
	Limit            struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	Experimental struct {
		Modes map[string]struct {
			Cost     json.RawMessage `json:"cost"`
			Provider struct {
				Body    map[string]any    `json:"body"`
				Headers map[string]string `json:"headers"`
			} `json:"provider"`
		} `json:"modes"`
	} `json:"experimental"`
}

// ReplaceModelsDev validates the provider-scoped anthropic, openai, and
// baseten records in one models.dev response, then atomically publishes all
// three live layers. An error leaves the current snapshot untouched.
func (p *Pricing) ReplaceModelsDev(body []byte, fetchedAt time.Time, etag string) error {
	catalogs, err := parseModelsDev(body, fetchedAt.UTC(), etag, LoadedFromLive)
	if err != nil {
		return err
	}
	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	current := p.current.Load()
	layers := cloneProviderLayers(current.providerLayers)
	for key := range layers {
		if key.loadedFrom == LoadedFromLive && key.source == modelsDevSource {
			delete(layers, key)
			continue
		}
		if key.loadedFrom != LoadedFromRuntimeCache {
			continue
		}
		catalog, keep := withoutCachedModelsDev(layers[key])
		if !keep {
			delete(layers, key)
			continue
		}
		layers[key] = catalog
	}
	for provider, catalog := range catalogs {
		layers[providerLayerKey{
			provider: provider, loadedFrom: LoadedFromLive, source: modelsDevSource,
		}] = catalog
	}
	snapshot := cloneSnapshotWithLayers(current, layers)
	p.publishLocked(snapshot)
	return nil
}

// withoutCachedModelsDev removes the superseded models.dev dimension from one
// merged runtime-cache layer. Independent authenticated availability and
// pricing evidence remain available until their own source refresh replaces
// them.
func withoutCachedModelsDev(catalog providerCatalog) (providerCatalog, bool) {
	catalog = cloneProviderCatalog(catalog)
	for id, record := range catalog.models {
		if record.Provenance.Source == modelsDevSource {
			delete(catalog.models, id)
			continue
		}
		if record.Availability.Public != nil &&
			record.Availability.Public.Provenance.Source == modelsDevSource {
			record.Availability.Public = nil
		}
		for profile, definition := range record.Profiles {
			if definition.Provenance.Source == modelsDevSource {
				delete(record.Profiles, profile)
			}
		}
		for profile, price := range record.Prices {
			if price.Provenance.Source == modelsDevSource {
				delete(record.Prices, profile)
			}
		}
		if record.Reasoning != nil &&
			record.Reasoning.Provenance.Source == modelsDevSource {
			record.Reasoning = nil
		}
		if record.Modalities != nil &&
			record.Modalities.Provenance.Source == modelsDevSource {
			record.Modalities = nil
		}
		// Family currently has record-level rather than field-level provenance.
		// models.dev is the only runtime-cache source that supplies it.
		record.Family = ""
		catalog.models[id] = record
	}
	if len(catalog.models) == 0 {
		return providerCatalog{}, false
	}
	catalog.metadata.ModelsDevValidatedAt = time.Time{}
	catalog.metadata.Diagnostics = nil
	catalog.metadata.ModelCount = len(catalog.models)
	catalog.metadata.PricedModelCount = 0
	for _, record := range catalog.models {
		if _, ok := record.Prices[ProfileStandard]; ok {
			catalog.metadata.PricedModelCount++
		}
	}
	return catalog, true
}

// RevalidateModelsDev records a successful root ETag revalidation without
// changing the source capture time, revision, or normalized model content.
// All three provider slices must still share a root validator.
func (p *Pricing) RevalidateModelsDev(validatedAt time.Time) error {
	if validatedAt.IsZero() {
		return fmt.Errorf("models.dev validated_at is required")
	}
	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	current := p.current.Load()
	if current.ModelsDevRootETag() == "" {
		return fmt.Errorf(
			"models.dev root ETag is unavailable or provider slices disagree",
		)
	}
	layers := cloneProviderLayers(current.providerLayers)
	validatedProviders := make(map[string]bool, 3)
	for key, catalog := range layers {
		hasModelsDevSlice := false
		for _, record := range catalog.models {
			if record.Availability.Public != nil &&
				record.Availability.Public.Provenance.Source ==
					modelsDevSource {
				hasModelsDevSlice = true
				break
			}
		}
		if !hasModelsDevSlice {
			continue
		}
		catalog.metadata.ModelsDevValidatedAt = validatedAt.UTC()
		layers[key] = catalog
		validatedProviders[key.provider] = true
	}
	for _, provider := range []string{
		ProviderAnthropic,
		ProviderOpenAI,
		ProviderBaseten,
	} {
		if !validatedProviders[provider] {
			return fmt.Errorf(
				"models.dev provider slice %q is unavailable",
				provider,
			)
		}
	}
	p.publishLocked(cloneSnapshotWithLayers(current, layers))
	return nil
}

func parseModelsDev(body []byte, fetchedAt time.Time, etag string, loadedFrom LoadedFrom) (map[string]providerCatalog, error) {
	if fetchedAt.IsZero() {
		return nil, fmt.Errorf("models.dev captured_at is required")
	}
	var root map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("models.dev catalog is not an object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("models.dev catalog contains multiple JSON values")
		}
		return nil, fmt.Errorf("decode models.dev trailing data: %w", err)
	}

	result := make(map[string]providerCatalog, 3)
	for _, provider := range []string{ProviderAnthropic, ProviderOpenAI, ProviderBaseten} {
		raw, ok := root[provider]
		if !ok {
			return nil, fmt.Errorf("models.dev catalog omitted provider %q", provider)
		}
		catalog, err := parseModelsDevProvider(provider, raw, fetchedAt, etag, loadedFrom)
		if err != nil {
			return nil, err
		}
		result[provider] = catalog
	}
	return result, nil
}

func parseModelsDevProvider(provider string, raw json.RawMessage, fetchedAt time.Time, etag string, loadedFrom LoadedFrom) (providerCatalog, error) {
	var source modelsDevProvider
	if err := json.Unmarshal(raw, &source); err != nil {
		return providerCatalog{}, fmt.Errorf("decode models.dev provider %q: %w", provider, err)
	}
	if source.ID != "" && source.ID != provider {
		return providerCatalog{}, fmt.Errorf("models.dev provider key %q has id %q", provider, source.ID)
	}
	if len(source.Models) == 0 {
		return providerCatalog{}, fmt.Errorf("models.dev provider %q contains no models", provider)
	}
	revision := revisionForRawJSON(raw)
	provenance := Provenance{
		Source: modelsDevSource, LoadedFrom: loadedFrom, Revision: revision,
		CapturedAt: fetchedAt, ETag: etag,
	}
	records := make(map[string]ModelRecord, len(source.Models))
	var diagnostics modelsDevDiagnostics
	for key, modelRaw := range source.Models {
		include, err := includeModelsDevModel(
			modelRaw,
			&diagnostics,
		)
		if err != nil {
			return providerCatalog{}, fmt.Errorf(
				"models.dev provider %q model %q: %w",
				provider,
				key,
				err,
			)
		}
		if !include {
			continue
		}
		record, err := parseModelsDevModel(
			provider,
			key,
			modelRaw,
			provenance,
			&diagnostics,
		)
		if err != nil {
			return providerCatalog{}, fmt.Errorf("models.dev provider %q model %q: %w", provider, key, err)
		}
		if _, duplicate := records[record.CanonicalModelID]; duplicate {
			return providerCatalog{}, fmt.Errorf("models.dev provider %q contains duplicate model %q", provider, record.CanonicalModelID)
		}
		records[record.CanonicalModelID] = record
	}
	if len(records) == 0 {
		return providerCatalog{}, fmt.Errorf(
			"models.dev provider %q contains no active models",
			provider,
		)
	}
	priced := 0
	for _, record := range records {
		if _, ok := record.Prices[ProfileStandard]; ok {
			priced++
		}
	}
	publicDiagnostics := []string(nil)
	if diagnostics.unknownReasoningOptions != 0 {
		publicDiagnostics = append(
			publicDiagnostics,
			fmt.Sprintf(
				"ignored %d unknown reasoning option type(s)",
				diagnostics.unknownReasoningOptions,
			),
		)
	}
	if diagnostics.deprecatedModels != 0 {
		publicDiagnostics = append(
			publicDiagnostics,
			fmt.Sprintf(
				"excluded %d deprecated model(s)",
				diagnostics.deprecatedModels,
			),
		)
	}
	if diagnostics.unknownStatusModels != 0 {
		publicDiagnostics = append(
			publicDiagnostics,
			fmt.Sprintf(
				"excluded %d model(s) with unknown status",
				diagnostics.unknownStatusModels,
			),
		)
	}
	if diagnostics.unsupportedFastProfiles != 0 {
		publicDiagnostics = append(
			publicDiagnostics,
			fmt.Sprintf(
				"ignored %d unsupported Anthropic fast profile(s)",
				diagnostics.unsupportedFastProfiles,
			),
		)
	}
	return providerCatalog{
		metadata: ProviderMetadata{
			Provider: provider, Provenance: provenance,
			ModelsDevValidatedAt: fetchedAt,
			ModelCount:           len(records), PricedModelCount: priced,
			Diagnostics: publicDiagnostics,
		},
		models: records,
	}, nil
}

func parseModelsDevModel(
	provider,
	key string,
	raw json.RawMessage,
	provenance Provenance,
	diagnostics *modelsDevDiagnostics,
) (ModelRecord, error) {
	var source modelsDevModel
	if err := json.Unmarshal(raw, &source); err != nil {
		return ModelRecord{}, err
	}
	key = strings.TrimSpace(key)
	id := strings.TrimSpace(source.ID)
	if id == "" {
		id = key
	}
	if key == "" || id != key {
		return ModelRecord{}, fmt.Errorf("model key %q does not match id %q", key, id)
	}
	name := strings.TrimSpace(source.Name)
	if name == "" {
		name = id
	}
	if source.Limit.Context < 0 || source.Limit.Output < 0 {
		return ModelRecord{}, fmt.Errorf("model limits must be nonnegative")
	}
	record := ModelRecord{
		Provider: provider, CanonicalModelID: id, DisplayName: name,
		Family:        normalizeModelsDevFamily(provider, source.Family, id),
		ContextTokens: source.Limit.Context, MaxOutputTokens: source.Limit.Output,
		Availability: ModelAvailability{
			Public: &AvailabilityEvidence{
				State: AvailabilityProviderListed, Provenance: provenance,
			},
		},
		Profiles: map[ExecutionProfile]ProfileDefinition{
			ProfileStandard: {
				Profile: ProfileStandard, Supported: true, Provenance: provenance,
			},
		},
		Prices:     map[ExecutionProfile]PriceProfile{},
		Provenance: provenance,
	}
	reasoning, err := parseModelsDevReasoning(
		source.Reasoning,
		source.ReasoningOptions,
		provenance,
		diagnostics,
	)
	if err != nil {
		return ModelRecord{}, fmt.Errorf("reasoning: %w", err)
	}
	record.Reasoning = reasoning
	modalities, err := parseModelsDevModalities(
		source.Modalities,
		provenance,
	)
	if err != nil {
		return ModelRecord{}, fmt.Errorf("modalities: %w", err)
	}
	record.Modalities = modalities
	if provider != ProviderBaseten {
		if price, ratePresence, present, err := parseModelsDevCost(source.Cost); err != nil {
			return ModelRecord{}, fmt.Errorf("standard cost: %w", err)
		} else if present {
			record.Prices[ProfileStandard] = PriceProfile{
				Profile: ProfileStandard, Price: price,
				RatePresence: ratePresence, RatePresenceKnown: true,
				Provenance: provenance,
				RateProvenance: rateProvenanceForPresence(
					ratePresence,
					provenance,
				),
			}
		}
	}
	modeNames := make([]string, 0, len(source.Experimental.Modes))
	for mode := range source.Experimental.Modes {
		modeNames = append(modeNames, mode)
	}
	sort.Strings(modeNames)
	for _, modeName := range modeNames {
		mode := source.Experimental.Modes[modeName]
		profile := ExecutionProfile(modeName)
		if profile == "" || profile == ProfileStandard {
			return ModelRecord{}, fmt.Errorf("experimental mode %q is invalid", modeName)
		}
		definition := safeModelsDevProfileDefinition(
			provider,
			profile,
			mode.Provider.Body,
			mode.Provider.Headers,
			provenance,
		)
		if provider == ProviderAnthropic && profile == ProfileFast &&
			(!anthropicFastProfileAllowed(id) || !definition.Supported) {
			if diagnostics != nil {
				diagnostics.unsupportedFastProfiles++
			}
			continue
		}
		record.Profiles[profile] = definition
		if provider != ProviderBaseten {
			if price, ratePresence, present, err := parseModelsDevCost(mode.Cost); err != nil {
				return ModelRecord{}, fmt.Errorf("profile %q cost: %w", profile, err)
			} else if present {
				record.Prices[profile] = PriceProfile{
					Profile: profile, Price: price,
					RatePresence: ratePresence, RatePresenceKnown: true,
					Provenance: provenance,
					RateProvenance: rateProvenanceForPresence(
						ratePresence,
						provenance,
					),
				}
			}
		}
	}
	if err := validateModelRecord(record); err != nil {
		return ModelRecord{}, err
	}
	return record, nil
}

func parseModelsDevModalities(
	raw json.RawMessage,
	provenance Provenance,
) (*ModelModalities, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("modalities is not an object")
	}
	var source struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	}
	if err := json.Unmarshal(trimmed, &source); err != nil {
		return nil, fmt.Errorf("modalities is not an object")
	}
	modalities := &ModelModalities{
		Input:      source.Input,
		Output:     source.Output,
		Provenance: provenance,
	}
	if err := validateModelModalities(*modalities); err != nil {
		return nil, err
	}
	return modalities, nil
}

func includeModelsDevModel(
	raw json.RawMessage,
	diagnostics *modelsDevDiagnostics,
) (bool, error) {
	var fields struct {
		Status json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(fields.Status)) == 0 {
		return true, nil
	}
	var status string
	if err := json.Unmarshal(fields.Status, &status); err != nil {
		return false, fmt.Errorf("status is not a string")
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "alpha", "beta":
		return true, nil
	case "deprecated":
		if diagnostics != nil {
			diagnostics.deprecatedModels++
		}
		return false, nil
	default:
		if diagnostics != nil {
			diagnostics.unknownStatusModels++
		}
		return false, nil
	}
}

func anthropicFastProfileAllowed(model string) bool {
	switch model {
	case "claude-opus-4-8", "claude-opus-5":
		return true
	default:
		return false
	}
}

func parseModelsDevReasoning(
	rawReasoning json.RawMessage,
	rawOptions json.RawMessage,
	provenance Provenance,
	diagnostics *modelsDevDiagnostics,
) (*ReasoningCapability, error) {
	reasoningPresent := len(bytes.TrimSpace(rawReasoning)) != 0
	optionsPresent := len(bytes.TrimSpace(rawOptions)) != 0
	if !reasoningPresent {
		if optionsPresent {
			return nil, fmt.Errorf(
				"reasoning_options is present without reasoning",
			)
		}
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(rawReasoning), []byte("null")) {
		return nil, fmt.Errorf("reasoning is not a boolean")
	}

	var supported bool
	if err := json.Unmarshal(rawReasoning, &supported); err != nil {
		return nil, fmt.Errorf("reasoning is not a boolean")
	}
	if !supported {
		if optionsPresent {
			return nil, fmt.Errorf(
				"reasoning_options is present when reasoning is false",
			)
		}
		return &ReasoningCapability{
			Supported:  false,
			Options:    []ReasoningOption{},
			Provenance: provenance,
		}, nil
	}
	if !optionsPresent ||
		bytes.Equal(bytes.TrimSpace(rawOptions), []byte("null")) {
		return nil, fmt.Errorf(
			"reasoning_options is required when reasoning is true",
		)
	}

	var rawEntries []json.RawMessage
	if err := json.Unmarshal(rawOptions, &rawEntries); err != nil ||
		rawEntries == nil {
		return nil, fmt.Errorf("reasoning_options is not an array")
	}
	options := make([]ReasoningOption, 0, len(rawEntries))
	for index, raw := range rawEntries {
		option, known, err := parseModelsDevReasoningOption(raw)
		if err != nil {
			return nil, fmt.Errorf("reasoning_options[%d]: %w", index, err)
		}
		if known {
			options = append(options, option)
		} else if diagnostics != nil {
			diagnostics.unknownReasoningOptions++
		}
	}
	capability := &ReasoningCapability{
		Supported:  true,
		Options:    options,
		Provenance: provenance,
	}
	if err := validateReasoningCapability(*capability); err != nil {
		return nil, err
	}
	return capability, nil
}

func attachVendoredBasetenReasoning(catalog *providerCatalog) error {
	var envelope struct {
		SchemaVersion int                        `json:"schema_version"`
		Source        string                     `json:"source"`
		SourceCommit  string                     `json:"source_commit"`
		CapturedAt    time.Time                  `json:"captured_at"`
		Models        map[string]json.RawMessage `json:"models"`
	}
	decoder := json.NewDecoder(bytes.NewReader(basetenReasoningFallbackJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("fallback contains trailing JSON")
	}
	if envelope.SchemaVersion != 1 ||
		envelope.Source != modelsDevSource ||
		strings.TrimSpace(envelope.SourceCommit) == "" ||
		envelope.CapturedAt.IsZero() ||
		len(envelope.Models) == 0 {
		return fmt.Errorf("fallback envelope is incomplete")
	}
	encodedModels, err := json.Marshal(envelope.Models)
	if err != nil {
		return err
	}
	provenance := Provenance{
		Source:     modelsDevSource,
		LoadedFrom: LoadedFromVendoredFallback,
		Revision:   revisionForRawJSON(encodedModels),
		CapturedAt: envelope.CapturedAt.UTC(),
	}
	for id, raw := range envelope.Models {
		record, ok := catalog.models[id]
		if !ok {
			return fmt.Errorf(
				"reasoning model %q is absent from pricing fallback",
				id,
			)
		}
		var source struct {
			Reasoning        json.RawMessage `json:"reasoning"`
			ReasoningOptions json.RawMessage `json:"reasoning_options"`
		}
		if err := json.Unmarshal(raw, &source); err != nil {
			return fmt.Errorf("model %q: %w", id, err)
		}
		capability, err := parseModelsDevReasoning(
			source.Reasoning,
			source.ReasoningOptions,
			provenance,
			nil,
		)
		if err != nil {
			return fmt.Errorf("model %q: %w", id, err)
		}
		if capability == nil {
			return fmt.Errorf("model %q omitted reasoning metadata", id)
		}
		record.Reasoning = capability
		catalog.models[id] = record
	}
	return nil
}

func parseModelsDevReasoningOption(
	raw json.RawMessage,
) (ReasoningOption, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ReasoningOption{}, false, fmt.Errorf("option is not an object")
	}
	var optionType string
	if err := json.Unmarshal(fields["type"], &optionType); err != nil ||
		strings.TrimSpace(optionType) == "" {
		return ReasoningOption{}, false, fmt.Errorf(
			"option type is not a non-empty string",
		)
	}
	switch ReasoningOptionType(optionType) {
	case ReasoningToggle:
		if err := rejectUnexpectedReasoningOptionFields(
			fields,
			"type",
		); err != nil {
			return ReasoningOption{}, false, err
		}
		option := ReasoningOption{Type: ReasoningToggle}
		if err := validateReasoningOption(option); err != nil {
			return ReasoningOption{}, false, err
		}
		return option, true, nil
	case ReasoningEffort:
		if err := rejectUnexpectedReasoningOptionFields(
			fields,
			"type",
			"values",
		); err != nil {
			return ReasoningOption{}, false, err
		}
		rawValues, ok := fields["values"]
		if !ok || bytes.Equal(bytes.TrimSpace(rawValues), []byte("null")) {
			return ReasoningOption{}, false, fmt.Errorf(
				"effort values is required",
			)
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(rawValues, &entries); err != nil ||
			entries == nil {
			return ReasoningOption{}, false, fmt.Errorf(
				"effort values is not an array",
			)
		}
		values := make([]*string, len(entries))
		for index, entry := range entries {
			if bytes.Equal(bytes.TrimSpace(entry), []byte("null")) {
				continue
			}
			var value string
			if err := json.Unmarshal(entry, &value); err != nil ||
				!validReasoningEffort(value) {
				return ReasoningOption{}, false, fmt.Errorf(
					"effort value %d is invalid",
					index,
				)
			}
			cloned := strings.Clone(value)
			values[index] = &cloned
		}
		option := ReasoningOption{
			Type: ReasoningEffort, Values: values,
		}
		if err := validateReasoningOption(option); err != nil {
			return ReasoningOption{}, false, err
		}
		return option, true, nil
	case ReasoningBudgetTokens:
		if err := rejectUnexpectedReasoningOptionFields(
			fields,
			"type",
			"min",
			"max",
		); err != nil {
			return ReasoningOption{}, false, err
		}
		minimum, err := parseOptionalReasoningBound(fields, "min")
		if err != nil {
			return ReasoningOption{}, false, err
		}
		maximum, err := parseOptionalReasoningBound(fields, "max")
		if err != nil {
			return ReasoningOption{}, false, err
		}
		option := ReasoningOption{
			Type: ReasoningBudgetTokens,
			Min:  minimum,
			Max:  maximum,
		}
		if err := validateReasoningOption(option); err != nil {
			return ReasoningOption{}, false, err
		}
		return option, true, nil
	default:
		// Future option types are not controls until the gateway understands
		// their complete semantics. Preserve the known options in this model.
		return ReasoningOption{}, false, nil
	}
}

func rejectUnexpectedReasoningOptionFields(
	fields map[string]json.RawMessage,
	allowed ...string,
) error {
	allowedFields := make(map[string]bool, len(allowed))
	for _, field := range allowed {
		allowedFields[field] = true
	}
	for field := range fields {
		if !allowedFields[field] {
			return fmt.Errorf("field %q is not valid for this option type",
				field)
		}
	}
	return nil
}

func parseOptionalReasoningBound(
	fields map[string]json.RawMessage,
	name string,
) (*int64, error) {
	raw, ok := fields[name]
	if !ok {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("%s is not an integer", name)
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s is not an integer", name)
	}
	return &value, nil
}

func safeModelsDevProfileDefinition(
	provider string,
	profile ExecutionProfile,
	body map[string]any,
	headers map[string]string,
	provenance Provenance,
) ProfileDefinition {
	definition := ProfileDefinition{
		Profile: profile, Provenance: provenance,
	}
	if provider != ProviderAnthropic || profile != ProfileFast ||
		len(body) != 1 || len(headers) != 1 {
		return definition
	}
	speed, speedOK := body["speed"].(string)
	beta := ""
	for key, value := range headers {
		if strings.EqualFold(key, "anthropic-beta") {
			beta = value
		}
	}
	if !speedOK || speed != "fast" ||
		beta != "fast-mode-2026-02-01" {
		return definition
	}
	definition.Supported = true
	definition.RequestBody = map[string]any{"speed": "fast"}
	definition.RequestHeaders = map[string]string{
		"anthropic-beta": "fast-mode-2026-02-01",
	}
	return definition
}

func parseModelsDevCost(raw json.RawMessage) (Price, RatePresence, bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Price{}, RatePresence{}, false, nil
	}
	var fields struct {
		Input           json.RawMessage `json:"input"`
		Output          json.RawMessage `json:"output"`
		CacheRead       json.RawMessage `json:"cache_read"`
		CacheWrite      json.RawMessage `json:"cache_write"`
		Tiers           json.RawMessage `json:"tiers"`
		ContextOver200K json.RawMessage `json:"context_over_200k"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Price{}, RatePresence{}, false, err
	}
	// models.dev publishes context-dependent prices for some models. Until
	// request accounting selects the correct tier from captured token usage,
	// treating the base rate as complete would understate cost.
	if ratePresent(fields.Tiers) || ratePresent(fields.ContextOver200K) {
		return Price{}, RatePresence{}, false, nil
	}
	ratePresence := RatePresence{
		Input:        ratePresent(fields.Input),
		Output:       ratePresent(fields.Output),
		CacheRead:    ratePresent(fields.CacheRead),
		CacheWrite5m: ratePresent(fields.CacheWrite),
	}
	if !ratePresence.Input || !ratePresence.Output {
		return Price{}, RatePresence{}, false, fmt.Errorf("input and output rates are required")
	}
	values := []struct {
		name string
		raw  json.RawMessage
		out  *float64
	}{
		{name: "input", raw: fields.Input},
		{name: "output", raw: fields.Output},
		{name: "cache_read", raw: fields.CacheRead},
		{name: "cache_write", raw: fields.CacheWrite},
	}
	var price Price
	values[0].out = &price.Prompt
	values[1].out = &price.Completion
	values[2].out = &price.CacheRead
	values[3].out = &price.CacheWrite5m
	for _, field := range values {
		value, err := strictOptionalFloat(field.raw)
		if err != nil {
			return Price{}, RatePresence{}, false, fmt.Errorf("%s: %w", field.name, err)
		}
		*field.out = value
	}
	if err := validatePrice(price); err != nil {
		return Price{}, RatePresence{}, false, err
	}
	return price, ratePresence, true, nil
}

func ratePresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}

func normalizeModelsDevFamily(provider, family, id string) string {
	if provider != ProviderAnthropic {
		return sanitizeCatalogFamily(family)
	}
	lower := strings.ToLower(family + " " + id)
	for _, candidate := range []string{"fable", "opus", "sonnet", "haiku"} {
		if strings.Contains(lower, candidate) {
			return candidate
		}
	}
	if normalized := sanitizeCatalogFamily(
		strings.TrimPrefix(strings.ToLower(strings.TrimSpace(family)), "claude-"),
	); normalized != "" {
		return normalized
	}
	parts := strings.Split(strings.ToLower(strings.TrimSpace(id)), "-")
	if len(parts) >= 3 && parts[0] == "claude" &&
		(parts[1] == "" || parts[1][0] < '0' || parts[1][0] > '9') {
		return sanitizeCatalogFamily(parts[1])
	}
	return ""
}

func sanitizeCatalogFamily(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') ||
			char == '-' || char == '_' {
			continue
		}
		return ""
	}
	return value
}

func revisionForRawJSON(raw json.RawMessage) string {
	var value any
	_ = json.Unmarshal(raw, &value)
	canonical, _ := json.Marshal(value)
	hash := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func cloneSnapshotWithLayers(
	current *Snapshot,
	layers map[providerLayerKey]providerCatalog,
) *Snapshot {
	snapshot := &Snapshot{
		providerLayers: layers,
	}
	if current != nil {
		snapshot.officialPricingSupplement = cloneOfficialPricingSupplement(
			current.officialPricingSupplement,
		)
	}
	snapshot.providerCatalogs = activeProviderCatalogsForSnapshot(snapshot)
	return snapshot
}
