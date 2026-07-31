package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderBaseten   = "baseten"
)

// ExecutionProfile names provider request behavior that can change pricing
// without changing the canonical model identity. It intentionally does not use
// "service tier": providers use that term for separate priority/batch APIs.
type ExecutionProfile string

const (
	ProfileStandard ExecutionProfile = "standard"
	ProfileFast     ExecutionProfile = "fast"
)

type LoadedFrom string

const (
	LoadedFromLive             LoadedFrom = "live"
	LoadedFromRuntimeCache     LoadedFrom = "runtime_cache"
	LoadedFromVendoredFallback LoadedFrom = "vendored_fallback"
)

// Provenance identifies the exact catalog material from which one record was
// built. Source describes the original authority even when LoadedFrom says the
// material was restored from the runtime cache.
type Provenance struct {
	Source         string     `json:"source"`
	LoadedFrom     LoadedFrom `json:"loaded_from"`
	Revision       string     `json:"revision"`
	CapturedAt     time.Time  `json:"captured_at"`
	EffectiveFrom  string     `json:"effective_from,omitempty"`
	EffectiveUntil string     `json:"effective_until,omitempty"`
	ETag           string     `json:"etag,omitempty"`
}

type RateDimension string

const (
	RateInput        RateDimension = "input"
	RateOutput       RateDimension = "output"
	RateCacheRead    RateDimension = "cache_read"
	RateCacheWrite5m RateDimension = "cache_write_5m"
	RateCacheWrite1h RateDimension = "cache_write_1h"
)

type RateProvenance struct {
	Input        Provenance `json:"input,omitempty"`
	Output       Provenance `json:"output,omitempty"`
	CacheRead    Provenance `json:"cache_read,omitempty"`
	CacheWrite5m Provenance `json:"cache_write_5m,omitempty"`
	CacheWrite1h Provenance `json:"cache_write_1h,omitempty"`
}

// ProfileDefinition describes how a provider execution profile is selected.
// Maps are copied at every publication and read boundary.
type ProfileDefinition struct {
	Profile        ExecutionProfile  `json:"profile"`
	Supported      bool              `json:"supported"`
	RequestBody    map[string]any    `json:"request_body,omitempty"`
	RequestHeaders map[string]string `json:"request_headers,omitempty"`
	Provenance     Provenance        `json:"provenance"`
}

// PriceProfile is one model's price for one execution profile.
type PriceProfile struct {
	Profile           ExecutionProfile `json:"profile"`
	Price             Price            `json:"price"`
	RatePresence      RatePresence     `json:"rate_presence"`
	RatePresenceKnown bool             `json:"rate_presence_known"`
	Provenance        Provenance       `json:"provenance"`
	RateProvenance    RateProvenance   `json:"rate_provenance"`
}

type AvailabilityState string

const (
	AvailabilityProviderListed            AvailabilityState = "provider_listed"
	AvailabilityAvailable                 AvailabilityState = "available"
	AvailabilityScopeUnscopedLastObserved                   = "unscoped_last_observed"
)

type AvailabilityEvidence struct {
	State      AvailabilityState `json:"state"`
	Scope      string            `json:"scope,omitempty"`
	Provenance Provenance        `json:"provenance"`
}

// ModelAvailability keeps public catalog evidence separate from evidence that
// a provider credential was observed using the model. Unscoped account
// evidence is historical metadata only, never a current authorization claim.
type ModelAvailability struct {
	Public  *AvailabilityEvidence `json:"public,omitempty"`
	Account *AvailabilityEvidence `json:"account,omitempty"`
}

type ReasoningOptionType string

const (
	ReasoningToggle       ReasoningOptionType = "toggle"
	ReasoningEffort       ReasoningOptionType = "effort"
	ReasoningBudgetTokens ReasoningOptionType = "budget_tokens"
)

// ReasoningOption is one provider-scoped semantic control advertised by the
// model catalog. It intentionally contains no provider request-field names.
type ReasoningOption struct {
	Type   ReasoningOptionType `json:"type"`
	Values []*string           `json:"values,omitempty"`
	Min    *int64              `json:"min,omitempty"`
	Max    *int64              `json:"max,omitempty"`
}

// ReasoningCapability distinguishes unsupported reasoning from supported
// reasoning with no verified configurable control. A nil pointer on
// ModelRecord means the active catalog layers have no reasoning metadata.
type ReasoningCapability struct {
	Supported  bool              `json:"supported"`
	Options    []ReasoningOption `json:"options"`
	Provenance Provenance        `json:"provenance"`
}

// ModelModalities records provider-declared input and output modalities.
// Harness projections select only the values their own schema can represent.
type ModelModalities struct {
	Input      []string   `json:"input"`
	Output     []string   `json:"output"`
	Provenance Provenance `json:"provenance"`
}

// ModelRecord is the provider-scoped normalized catalog identity.
type ModelRecord struct {
	Provider         string                                 `json:"provider"`
	CanonicalModelID string                                 `json:"canonical_model_id"`
	DisplayName      string                                 `json:"display_name"`
	Family           string                                 `json:"family,omitempty"`
	ContextTokens    int64                                  `json:"context_tokens,omitempty"`
	MaxOutputTokens  int64                                  `json:"max_output_tokens,omitempty"`
	Availability     ModelAvailability                      `json:"availability"`
	Profiles         map[ExecutionProfile]ProfileDefinition `json:"profiles"`
	Prices           map[ExecutionProfile]PriceProfile      `json:"prices"`
	Reasoning        *ReasoningCapability                   `json:"reasoning,omitempty"`
	Modalities       *ModelModalities                       `json:"modalities,omitempty"`
	Provenance       Provenance                             `json:"provenance"`
}

type ProviderMetadata struct {
	Provider             string     `json:"provider"`
	Provenance           Provenance `json:"provenance"`
	ModelsDevValidatedAt time.Time  `json:"models_dev_validated_at,omitempty"`
	ModelCount           int        `json:"model_count"`
	PricedModelCount     int        `json:"priced_model_count"`
	Diagnostics          []string   `json:"diagnostics,omitempty"`
}

type providerCatalog struct {
	metadata                    ProviderMetadata
	models                      map[string]ModelRecord
	replacesAccountAvailability bool
	replacesPricing             bool
	basetenPricing              *basetenPricingCatalog
}

type basetenPricingCatalog struct {
	metadata CatalogMetadata
	models   []string
}

type providerLayerKey struct {
	provider   string
	loadedFrom LoadedFrom
	source     string
}

func supportedProvider(provider string) bool {
	switch provider {
	case ProviderAnthropic, ProviderOpenAI, ProviderBaseten:
		return true
	default:
		return false
	}
}

func validateProvenance(p Provenance) error {
	if strings.TrimSpace(p.Source) == "" {
		return fmt.Errorf("source is required")
	}
	switch p.LoadedFrom {
	case LoadedFromLive, LoadedFromRuntimeCache, LoadedFromVendoredFallback:
	default:
		return fmt.Errorf("loaded_from %q is invalid", p.LoadedFrom)
	}
	if strings.TrimSpace(p.Revision) == "" {
		return fmt.Errorf("revision is required")
	}
	if p.CapturedAt.IsZero() {
		return fmt.Errorf("captured_at is required")
	}
	if err := validateEffectiveWindow(
		p.EffectiveFrom,
		p.EffectiveUntil,
	); err != nil {
		return err
	}
	return nil
}

func validateModelRecord(record ModelRecord) error {
	if !supportedProvider(record.Provider) {
		return fmt.Errorf("provider %q is unsupported", record.Provider)
	}
	if strings.TrimSpace(record.CanonicalModelID) == "" {
		return fmt.Errorf("canonical_model_id is required")
	}
	if strings.TrimSpace(record.DisplayName) == "" {
		return fmt.Errorf("display_name is required")
	}
	if record.Family != "" &&
		sanitizeCatalogFamily(record.Family) != record.Family {
		return fmt.Errorf("family %q is invalid", record.Family)
	}
	if record.ContextTokens < 0 || record.MaxOutputTokens < 0 {
		return fmt.Errorf("model limits must be nonnegative")
	}
	if err := validateProvenance(record.Provenance); err != nil {
		return fmt.Errorf("model provenance: %w", err)
	}
	for dimension, evidence := range map[string]*AvailabilityEvidence{
		"public": record.Availability.Public, "account": record.Availability.Account,
	} {
		if evidence == nil {
			continue
		}
		switch evidence.State {
		case AvailabilityProviderListed, AvailabilityAvailable:
		default:
			return fmt.Errorf("%s availability state %q is invalid", dimension, evidence.State)
		}
		if err := validateProvenance(evidence.Provenance); err != nil {
			return fmt.Errorf("%s availability provenance: %w", dimension, err)
		}
		if dimension == "account" &&
			evidence.Scope != AvailabilityScopeUnscopedLastObserved {
			return fmt.Errorf(
				"account availability scope %q is invalid",
				evidence.Scope,
			)
		}
	}
	for profile, definition := range record.Profiles {
		if profile == "" || definition.Profile != profile {
			return fmt.Errorf("profile definition key %q does not match profile %q", profile, definition.Profile)
		}
		if err := validateProvenance(definition.Provenance); err != nil {
			return fmt.Errorf("profile %q provenance: %w", profile, err)
		}
	}
	for profile, price := range record.Prices {
		if profile == "" || price.Profile != profile {
			return fmt.Errorf("price profile key %q does not match profile %q", profile, price.Profile)
		}
		if err := validatePrice(price.Price); err != nil {
			return fmt.Errorf("price profile %q: %w", profile, err)
		}
		if err := validateRatePresence(
			price.Price,
			price.RatePresence,
			price.RatePresenceKnown,
		); err != nil {
			return fmt.Errorf("price profile %q: %w", profile, err)
		}
		if err := validateRateProvenance(
			price.RatePresence,
			price.RatePresenceKnown,
			price.RateProvenance,
		); err != nil {
			return fmt.Errorf("price profile %q: %w", profile, err)
		}
		if err := validateProvenance(price.Provenance); err != nil {
			return fmt.Errorf("price profile %q provenance: %w", profile, err)
		}
	}
	if record.Reasoning != nil {
		if err := validateReasoningCapability(*record.Reasoning); err != nil {
			return fmt.Errorf("reasoning capability: %w", err)
		}
	}
	if record.Modalities != nil {
		if err := validateModelModalities(*record.Modalities); err != nil {
			return fmt.Errorf("model modalities: %w", err)
		}
	}
	return nil
}

func validateModelModalities(modalities ModelModalities) error {
	if err := validateProvenance(modalities.Provenance); err != nil {
		return fmt.Errorf("provenance: %w", err)
	}
	if modalities.Provenance.Source != modelsDevSource {
		return fmt.Errorf(
			"source %q is not the modality authority",
			modalities.Provenance.Source,
		)
	}
	if modalities.Input == nil || modalities.Output == nil {
		return fmt.Errorf("input and output are required")
	}
	for label, values := range map[string][]string{
		"input": modalities.Input, "output": modalities.Output,
	} {
		seen := make(map[string]struct{}, len(values))
		for index, value := range values {
			if value == "" || strings.TrimSpace(value) != value {
				return fmt.Errorf("%s modality %d is invalid", label, index)
			}
			if _, duplicate := seen[value]; duplicate {
				return fmt.Errorf("%s modality %q is duplicated", label, value)
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}

func validateReasoningCapability(capability ReasoningCapability) error {
	if err := validateProvenance(capability.Provenance); err != nil {
		return fmt.Errorf("provenance: %w", err)
	}
	if capability.Provenance.Source != modelsDevSource {
		return fmt.Errorf(
			"source %q is not the reasoning authority",
			capability.Provenance.Source,
		)
	}
	if capability.Options == nil {
		return fmt.Errorf("options is required")
	}
	if !capability.Supported && len(capability.Options) != 0 {
		return fmt.Errorf("unsupported reasoning cannot advertise options")
	}
	for index, option := range capability.Options {
		if err := validateReasoningOption(option); err != nil {
			return fmt.Errorf("option %d: %w", index, err)
		}
	}
	return nil
}

func validateReasoningOption(option ReasoningOption) error {
	switch option.Type {
	case ReasoningToggle:
		if len(option.Values) != 0 || option.Min != nil || option.Max != nil {
			return fmt.Errorf("toggle contains fields for another option type")
		}
	case ReasoningEffort:
		if option.Min != nil || option.Max != nil {
			return fmt.Errorf("effort contains budget bounds")
		}
		for index, value := range option.Values {
			if value == nil {
				continue
			}
			if !validReasoningEffort(*value) {
				return fmt.Errorf("effort value %d is invalid", index)
			}
		}
	case ReasoningBudgetTokens:
		if len(option.Values) != 0 {
			return fmt.Errorf("budget_tokens contains effort values")
		}
		if option.Min != nil && *option.Min < -1 {
			return fmt.Errorf("minimum budget cannot be less than -1")
		}
		if option.Max != nil && *option.Max < 0 {
			return fmt.Errorf("maximum budget cannot be negative")
		}
		if option.Min != nil && option.Max != nil &&
			*option.Min > *option.Max {
			return fmt.Errorf("minimum budget exceeds maximum")
		}
	default:
		return fmt.Errorf("type %q is invalid", option.Type)
	}
	return nil
}

func validReasoningEffort(value string) bool {
	if value == "" || len(value) > 64 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func allRateDimensions() []RateDimension {
	return []RateDimension{
		RateInput,
		RateOutput,
		RateCacheRead,
		RateCacheWrite5m,
		RateCacheWrite1h,
	}
}

func validRateDimension(dimension RateDimension) bool {
	switch dimension {
	case RateInput,
		RateOutput,
		RateCacheRead,
		RateCacheWrite5m,
		RateCacheWrite1h:
		return true
	default:
		return false
	}
}

func rateDimensionPresent(
	presence RatePresence,
	dimension RateDimension,
) bool {
	switch dimension {
	case RateInput:
		return presence.Input
	case RateOutput:
		return presence.Output
	case RateCacheRead:
		return presence.CacheRead
	case RateCacheWrite5m:
		return presence.CacheWrite5m
	case RateCacheWrite1h:
		return presence.CacheWrite1h
	default:
		return false
	}
}

func setRatePresence(
	presence *RatePresence,
	dimension RateDimension,
	value bool,
) {
	switch dimension {
	case RateInput:
		presence.Input = value
	case RateOutput:
		presence.Output = value
	case RateCacheRead:
		presence.CacheRead = value
	case RateCacheWrite5m:
		presence.CacheWrite5m = value
	case RateCacheWrite1h:
		presence.CacheWrite1h = value
	}
}

func rateDimensionValue(
	price Price,
	dimension RateDimension,
) float64 {
	switch dimension {
	case RateInput:
		return price.Prompt
	case RateOutput:
		return price.Completion
	case RateCacheRead:
		return price.CacheRead
	case RateCacheWrite5m:
		return price.CacheWrite5m
	case RateCacheWrite1h:
		return price.CacheWrite1h
	default:
		return 0
	}
}

func setRateDimensionValue(
	price *Price,
	dimension RateDimension,
	value float64,
) {
	switch dimension {
	case RateInput:
		price.Prompt = value
	case RateOutput:
		price.Completion = value
	case RateCacheRead:
		price.CacheRead = value
	case RateCacheWrite5m:
		price.CacheWrite5m = value
	case RateCacheWrite1h:
		price.CacheWrite1h = value
	}
}

func rateProvenanceForPresence(
	presence RatePresence,
	provenance Provenance,
) RateProvenance {
	var result RateProvenance
	for _, dimension := range allRateDimensions() {
		if rateDimensionPresent(presence, dimension) {
			setRateDimensionProvenance(
				&result,
				dimension,
				provenance,
			)
		}
	}
	return result
}

func cloneRateProvenance(
	source RateProvenance,
) RateProvenance {
	return RateProvenance{
		Input:        cloneProvenance(source.Input),
		Output:       cloneProvenance(source.Output),
		CacheRead:    cloneProvenance(source.CacheRead),
		CacheWrite5m: cloneProvenance(source.CacheWrite5m),
		CacheWrite1h: cloneProvenance(source.CacheWrite1h),
	}
}

func rateDimensionProvenance(
	provenance RateProvenance,
	dimension RateDimension,
) Provenance {
	switch dimension {
	case RateInput:
		return provenance.Input
	case RateOutput:
		return provenance.Output
	case RateCacheRead:
		return provenance.CacheRead
	case RateCacheWrite5m:
		return provenance.CacheWrite5m
	case RateCacheWrite1h:
		return provenance.CacheWrite1h
	default:
		return Provenance{}
	}
}

func setRateDimensionProvenance(
	provenance *RateProvenance,
	dimension RateDimension,
	value Provenance,
) {
	value = cloneProvenance(value)
	switch dimension {
	case RateInput:
		provenance.Input = value
	case RateOutput:
		provenance.Output = value
	case RateCacheRead:
		provenance.CacheRead = value
	case RateCacheWrite5m:
		provenance.CacheWrite5m = value
	case RateCacheWrite1h:
		provenance.CacheWrite1h = value
	}
}

func validateEffectiveWindow(from, until string) error {
	var fromTime time.Time
	var untilTime time.Time
	var err error
	if from != "" {
		fromTime, err = time.Parse(time.DateOnly, from)
		if err != nil {
			return fmt.Errorf("effective_from must use YYYY-MM-DD")
		}
	}
	if until != "" {
		untilTime, err = time.Parse(time.DateOnly, until)
		if err != nil {
			return fmt.Errorf("effective_until must use YYYY-MM-DD")
		}
	}
	if !fromTime.IsZero() &&
		!untilTime.IsZero() &&
		fromTime.After(untilTime) {
		return fmt.Errorf("effective_from exceeds effective_until")
	}
	return nil
}

func validateRatePresence(
	price Price,
	presence RatePresence,
	known bool,
) error {
	if !known {
		if presence != (RatePresence{}) {
			return fmt.Errorf("rate presence is set without known presence")
		}
		return nil
	}
	if !presence.Input || !presence.Output {
		return fmt.Errorf("input and output rate presence is required")
	}
	values := []struct {
		name    string
		value   float64
		present bool
	}{
		{name: "input", value: price.Prompt, present: presence.Input},
		{name: "output", value: price.Completion, present: presence.Output},
		{name: "cache_read", value: price.CacheRead, present: presence.CacheRead},
		{name: "cache_write_5m", value: price.CacheWrite5m, present: presence.CacheWrite5m},
		{name: "cache_write_1h", value: price.CacheWrite1h, present: presence.CacheWrite1h},
	}
	for _, dimension := range values {
		if !dimension.present && dimension.value != 0 {
			return fmt.Errorf(
				"%s rate is nonzero but marked absent",
				dimension.name,
			)
		}
	}
	return nil
}

func validateRateProvenance(
	presence RatePresence,
	known bool,
	provenance RateProvenance,
) error {
	if !known {
		if provenance != (RateProvenance{}) {
			return fmt.Errorf(
				"rate provenance is set without known presence",
			)
		}
		return nil
	}
	for _, dimension := range allRateDimensions() {
		value := rateDimensionProvenance(provenance, dimension)
		present := value.Source != ""
		if !rateDimensionPresent(presence, dimension) {
			if present {
				return fmt.Errorf(
					"%s provenance is set for an absent rate",
					dimension,
				)
			}
			continue
		}
		if !present {
			return fmt.Errorf(
				"%s provenance is required",
				dimension,
			)
		}
		if err := validateProvenance(value); err != nil {
			return fmt.Errorf("%s provenance: %w", dimension, err)
		}
	}
	return nil
}

func validatePrice(price Price) error {
	for _, value := range []float64{
		price.Prompt,
		price.Completion,
		price.CacheRead,
		price.CacheWrite5m,
		price.CacheWrite1h,
	} {
		if _, err := nanoUSDPerToken(value); err != nil {
			return err
		}
	}
	return nil
}

func cloneProvenance(p Provenance) Provenance {
	p.Source = strings.Clone(p.Source)
	p.Revision = strings.Clone(p.Revision)
	p.EffectiveFrom = strings.Clone(p.EffectiveFrom)
	p.EffectiveUntil = strings.Clone(p.EffectiveUntil)
	p.ETag = strings.Clone(p.ETag)
	return p
}

func cloneStringAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		// Provider profile bodies contain JSON values. A JSON round-trip gives
		// callers a deep copy without adding reflection or external packages.
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		var cloned any
		if json.Unmarshal(raw, &cloned) == nil {
			out[key] = cloned
		}
	}
	return out
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[strings.Clone(key)] = strings.Clone(value)
	}
	return out
}

func cloneModelRecord(record ModelRecord) ModelRecord {
	out := record
	out.Provider = strings.Clone(record.Provider)
	out.CanonicalModelID = strings.Clone(record.CanonicalModelID)
	out.DisplayName = strings.Clone(record.DisplayName)
	out.Family = strings.Clone(record.Family)
	out.Provenance = cloneProvenance(record.Provenance)
	if record.Availability.Public != nil {
		evidence := *record.Availability.Public
		evidence.Provenance = cloneProvenance(evidence.Provenance)
		out.Availability.Public = &evidence
	}
	if record.Availability.Account != nil {
		evidence := *record.Availability.Account
		evidence.Provenance = cloneProvenance(evidence.Provenance)
		out.Availability.Account = &evidence
	}
	out.Profiles = make(map[ExecutionProfile]ProfileDefinition, len(record.Profiles))
	for profile, definition := range record.Profiles {
		definition.Provenance = cloneProvenance(definition.Provenance)
		definition.RequestBody = cloneStringAnyMap(definition.RequestBody)
		definition.RequestHeaders = cloneStringMap(definition.RequestHeaders)
		out.Profiles[profile] = definition
	}
	out.Prices = make(map[ExecutionProfile]PriceProfile, len(record.Prices))
	for profile, price := range record.Prices {
		price.Provenance = cloneProvenance(price.Provenance)
		price.RateProvenance = cloneRateProvenance(
			price.RateProvenance,
		)
		out.Prices[profile] = price
	}
	if record.Reasoning != nil {
		capability := cloneReasoningCapability(*record.Reasoning)
		out.Reasoning = &capability
	}
	if record.Modalities != nil {
		modalities := cloneModelModalities(*record.Modalities)
		out.Modalities = &modalities
	}
	return out
}

func cloneModelModalities(modalities ModelModalities) ModelModalities {
	out := modalities
	out.Provenance = cloneProvenance(modalities.Provenance)
	out.Input = cloneStrings(modalities.Input)
	out.Output = cloneStrings(modalities.Output)
	return out
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = strings.Clone(value)
	}
	return out
}

func cloneReasoningCapability(
	capability ReasoningCapability,
) ReasoningCapability {
	out := capability
	out.Provenance = cloneProvenance(capability.Provenance)
	out.Options = make([]ReasoningOption, len(capability.Options))
	for index, option := range capability.Options {
		out.Options[index] = cloneReasoningOption(option)
	}
	return out
}

func cloneReasoningOption(option ReasoningOption) ReasoningOption {
	out := option
	if option.Values != nil {
		out.Values = make([]*string, len(option.Values))
		for index, value := range option.Values {
			if value == nil {
				continue
			}
			cloned := strings.Clone(*value)
			out.Values[index] = &cloned
		}
	}
	if option.Min != nil {
		value := *option.Min
		out.Min = &value
	}
	if option.Max != nil {
		value := *option.Max
		out.Max = &value
	}
	return out
}

func cloneProviderCatalog(catalog providerCatalog) providerCatalog {
	out := providerCatalog{
		metadata:                    catalog.metadata,
		models:                      make(map[string]ModelRecord, len(catalog.models)),
		replacesAccountAvailability: catalog.replacesAccountAvailability,
		replacesPricing:             catalog.replacesPricing,
	}
	if catalog.basetenPricing != nil {
		authority := *catalog.basetenPricing
		authority.models = append([]string(nil), catalog.basetenPricing.models...)
		out.basetenPricing = &authority
	}
	out.metadata.Provenance = cloneProvenance(catalog.metadata.Provenance)
	out.metadata.Diagnostics = append(
		[]string(nil),
		catalog.metadata.Diagnostics...,
	)
	for id, record := range catalog.models {
		out.models[id] = cloneModelRecord(record)
	}
	return out
}

func activeBasetenPricingCatalog(
	layers map[providerLayerKey]providerCatalog,
) basetenPricingCatalog {
	keys := make([]providerLayerKey, 0, len(layers))
	for key, catalog := range layers {
		if key.provider == ProviderBaseten && catalog.basetenPricing != nil {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := layerPriority(keys[i]), layerPriority(keys[j])
		if left != right {
			return left < right
		}
		return keys[i].source < keys[j].source
	})
	var active basetenPricingCatalog
	for _, key := range keys {
		active = *layers[key].basetenPricing
		active.models = append([]string(nil), active.models...)
	}
	return active
}

func cloneProviderLayers(source map[providerLayerKey]providerCatalog) map[providerLayerKey]providerCatalog {
	out := make(map[providerLayerKey]providerCatalog, len(source))
	for key, catalog := range source {
		out[key] = cloneProviderCatalog(catalog)
	}
	return out
}

func layerPriority(key providerLayerKey) int {
	base := 0
	switch key.loadedFrom {
	case LoadedFromVendoredFallback:
		base = 100
	case LoadedFromRuntimeCache:
		base = 200
	case LoadedFromLive:
		base = 300
	}
	return base + catalogSourcePriority(key.source)
}

func catalogSourcePriority(source string) int {
	switch source {
	case "baseten_model_apis", "baseten-model-apis":
		return 40
	case "anthropic_v1_models", "anthropic-v1-models",
		"openai_v1_models", "openai-v1-models",
		"baseten_v1_models", "baseten-v1-models":
		return 30
	case modelsDevSource:
		return 20
	case "static":
		return 10
	default:
		return 0
	}
}

func activeProviderCatalogs(layers map[providerLayerKey]providerCatalog) map[string]providerCatalog {
	keys := make([]providerLayerKey, 0, len(layers))
	for key := range layers {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := layerPriority(keys[i]), layerPriority(keys[j])
		if left != right {
			return left < right
		}
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].source < keys[j].source
	})
	active := make(map[string]providerCatalog)
	for _, key := range keys {
		layer := layers[key]
		current := active[key.provider]
		if current.models == nil {
			current.models = make(map[string]ModelRecord)
		}
		if layer.replacesAccountAvailability {
			for id, record := range current.models {
				record.Availability.Account = nil
				current.models[id] = record
			}
		}
		if layer.replacesPricing {
			for id, record := range current.models {
				record.Prices = map[ExecutionProfile]PriceProfile{}
				current.models[id] = record
			}
		}
		for id, higher := range layer.models {
			if lower, ok := current.models[id]; ok {
				higher = mergeModelRecord(lower, higher)
			}
			current.models[id] = cloneModelRecord(higher)
		}
		modelsDevValidatedAt := current.metadata.ModelsDevValidatedAt
		diagnostics := current.metadata.Diagnostics
		current.metadata = layer.metadata
		if current.metadata.ModelsDevValidatedAt.Before(
			modelsDevValidatedAt,
		) {
			current.metadata.ModelsDevValidatedAt =
				modelsDevValidatedAt
		}
		if layer.metadata.Provenance.Source != modelsDevSource &&
			len(current.metadata.Diagnostics) == 0 &&
			len(diagnostics) != 0 {
			current.metadata.Diagnostics = append(
				[]string(nil),
				diagnostics...,
			)
		}
		active[key.provider] = current
	}
	for provider, catalog := range active {
		catalog.metadata.Provider = provider
		catalog.metadata.ModelCount = len(catalog.models)
		catalog.metadata.PricedModelCount = 0
		for id, record := range catalog.models {
			if record.DisplayName == "" {
				record.DisplayName = id
				catalog.models[id] = record
			}
			if _, ok := record.Prices[ProfileStandard]; ok {
				catalog.metadata.PricedModelCount++
			}
		}
		active[provider] = catalog
	}
	return active
}

func activeProviderCatalogsForSnapshot(
	snapshot *Snapshot,
) map[string]providerCatalog {
	if snapshot == nil {
		return nil
	}
	active := activeProviderCatalogs(snapshot.providerLayers)
	applyOfficialPricingSupplement(
		active,
		snapshot.officialPricingSupplement,
		time.Now().UTC(),
	)
	for provider, catalog := range active {
		catalog.metadata.PricedModelCount = 0
		for _, record := range catalog.models {
			price, ok := record.Prices[ProfileStandard]
			if ok &&
				(!price.RatePresenceKnown ||
					(price.RatePresence.Input &&
						price.RatePresence.Output)) {
				catalog.metadata.PricedModelCount++
			}
		}
		active[provider] = catalog
	}
	return active
}

func mergeModelRecord(lower, higher ModelRecord) ModelRecord {
	out := cloneModelRecord(lower)
	providerMetadataWins := preferProviderMetadata(
		lower.Provenance,
		higher.Provenance,
	)
	if higher.Provider != "" {
		out.Provider = higher.Provider
	}
	if higher.CanonicalModelID != "" {
		out.CanonicalModelID = higher.CanonicalModelID
	}
	if higher.DisplayName != "" && providerMetadataWins {
		out.DisplayName = higher.DisplayName
	}
	if higher.Family != "" {
		if preferFamilyMetadata(lower.Provenance, higher.Provenance) {
			out.Family = higher.Family
		}
	}
	if higher.ContextTokens != 0 && providerMetadataWins {
		out.ContextTokens = higher.ContextTokens
	}
	if higher.MaxOutputTokens != 0 && providerMetadataWins {
		out.MaxOutputTokens = higher.MaxOutputTokens
	}
	if higher.Availability.Public != nil {
		evidence := *higher.Availability.Public
		out.Availability.Public = &evidence
	}
	if higher.Availability.Account != nil {
		evidence := *higher.Availability.Account
		out.Availability.Account = &evidence
	}
	for profile, definition := range higher.Profiles {
		out.Profiles[profile] = definition
	}
	for profile, price := range higher.Prices {
		out.Prices[profile] = price
	}
	if higher.Reasoning != nil {
		capability := cloneReasoningCapability(*higher.Reasoning)
		out.Reasoning = &capability
	}
	if higher.Modalities != nil {
		modalities := cloneModelModalities(*higher.Modalities)
		out.Modalities = &modalities
	}
	if providerMetadataWins {
		out.Provenance = higher.Provenance
	}
	return out
}

func preferProviderMetadata(lower, higher Provenance) bool {
	return providerMetadataAuthority(higher) >=
		providerMetadataAuthority(lower)
}

func providerMetadataAuthority(provenance Provenance) int {
	switch provenance.Source {
	case "baseten_model_apis", "baseten-model-apis":
		return 40
	case "anthropic_v1_models", "anthropic-v1-models",
		"openai_v1_models", "openai-v1-models",
		"baseten_v1_models", "baseten-v1-models":
		return 30
	case modelsDevSource:
		return 20
	default:
		if provenance.LoadedFrom == LoadedFromVendoredFallback {
			return 0
		}
		return 10
	}
}

func preferFamilyMetadata(lower, higher Provenance) bool {
	return familyMetadataAuthority(higher) >=
		familyMetadataAuthority(lower)
}

func familyMetadataAuthority(provenance Provenance) int {
	if provenance.Source == modelsDevSource {
		return 30
	}
	if provenance.LoadedFrom == LoadedFromVendoredFallback {
		return 0
	}
	return 10
}

// Model returns an owned copy of one normalized model record.
func (s *Snapshot) Model(provider, canonicalID string) (ModelRecord, bool) {
	if s == nil {
		return ModelRecord{}, false
	}
	catalog, ok := s.providerCatalogs[provider]
	if !ok {
		return ModelRecord{}, false
	}
	record, ok := catalog.models[canonicalID]
	if !ok {
		return ModelRecord{}, false
	}
	record = reevaluateOfficialPricingSupplement(
		record,
		s.officialPricingSupplement,
		time.Now().UTC(),
	)
	return cloneModelRecord(record), true
}

// ModelReasoning returns an owned copy of one exact provider-scoped
// capability. Callers must not substitute another provider's model record.
func (s *Snapshot) ModelReasoning(
	provider string,
	canonicalID string,
) (ReasoningCapability, bool) {
	if s == nil {
		return ReasoningCapability{}, false
	}
	catalog, ok := s.providerCatalogs[provider]
	if !ok {
		return ReasoningCapability{}, false
	}
	record, ok := lookupModelRecord(catalog.models, canonicalID)
	if !ok || record.Reasoning == nil {
		return ReasoningCapability{}, false
	}
	return cloneReasoningCapability(*record.Reasoning), true
}

// ModelInputModalities returns an owned copy of exact provider-scoped input
// modalities. Callers must not infer capabilities from a model family.
func (s *Snapshot) ModelInputModalities(
	provider string,
	canonicalID string,
) ([]string, bool) {
	if s == nil {
		return nil, false
	}
	catalog, ok := s.providerCatalogs[provider]
	if !ok {
		return nil, false
	}
	record, ok := lookupModelRecord(catalog.models, canonicalID)
	if !ok || record.Modalities == nil {
		return nil, false
	}
	return cloneStrings(record.Modalities.Input), true
}

// DisplayName returns presentation metadata for one provider-scoped canonical
// model identity. The returned string belongs to the immutable snapshot and
// must be treated as read-only.
func (s *Snapshot) DisplayName(
	provider string,
	canonicalID string,
) (string, bool) {
	if s == nil {
		return "", false
	}
	catalog, ok := s.providerCatalogs[provider]
	if !ok {
		return "", false
	}
	record, ok := lookupModelRecord(catalog.models, canonicalID)
	if !ok ||
		strings.TrimSpace(record.DisplayName) == "" ||
		record.DisplayName == record.CanonicalModelID {
		return "", false
	}
	return record.DisplayName, true
}

// PresentationRevision returns a deterministic revision of the provider's
// canonical identity-to-display-name mapping. Price and availability changes
// do not affect it.
func (s *Snapshot) PresentationRevision(provider string) string {
	if s == nil {
		return ""
	}
	catalog, ok := s.providerCatalogs[provider]
	if !ok || len(catalog.models) == 0 {
		return ""
	}
	ids := make([]string, 0, len(catalog.models))
	for id := range catalog.models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	hash := sha256.New()
	for _, id := range ids {
		_, _ = hash.Write([]byte(id))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(catalog.models[id].DisplayName))
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// ModelFamily returns the immutable normalized family without cloning the
// record's capability, profile, and pricing maps. It is the request-path
// lookup used by routing and telemetry classification.
func (s *Snapshot) ModelFamily(
	provider string,
	canonicalID string,
) (string, bool) {
	if s == nil {
		return "", false
	}
	catalog, ok := s.providerCatalogs[provider]
	if !ok {
		return "", false
	}
	record, ok := lookupModelRecord(catalog.models, canonicalID)
	if !ok || record.Family == "" {
		return "", false
	}
	return record.Family, true
}

// Models returns sorted owned copies of one provider's normalized records.
func (s *Snapshot) Models(provider string) []ModelRecord {
	if s == nil {
		return nil
	}
	catalog, ok := s.providerCatalogs[provider]
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(catalog.models))
	for id := range catalog.models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ModelRecord, 0, len(ids))
	effectiveAt := time.Now().UTC()
	for _, id := range ids {
		record := reevaluateOfficialPricingSupplement(
			catalog.models[id],
			s.officialPricingSupplement,
			effectiveAt,
		)
		out = append(out, cloneModelRecord(record))
	}
	return out
}

func (s *Snapshot) ProviderMetadata(provider string) ProviderMetadata {
	if s == nil {
		return ProviderMetadata{}
	}
	metadata := s.providerCatalogs[provider].metadata
	metadata.Provenance = cloneProvenance(metadata.Provenance)
	metadata.Diagnostics = append(
		[]string(nil),
		metadata.Diagnostics...,
	)
	return metadata
}

// ModelsDevValidatedAt returns the latest successful models.dev validation
// for this provider slice. Authenticated provider refreshes cannot advance it.
func (s *Snapshot) ModelsDevValidatedAt(provider string) time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.providerCatalogs[provider].metadata.ModelsDevValidatedAt
}

// ModelsDevETag returns the public-catalog validator retained on any active
// provider record. It remains available when authenticated provider metadata
// has higher field authority than the public source.
func (s *Snapshot) ModelsDevETag(provider string) string {
	if s == nil {
		return ""
	}
	catalog, ok := s.providerCatalogs[provider]
	if !ok {
		return ""
	}
	etag := ""
	found := false
	for _, record := range catalog.models {
		if evidence := record.Availability.Public; evidence != nil &&
			evidence.Provenance.Source == modelsDevSource {
			if evidence.Provenance.ETag == "" {
				return ""
			}
			if !found {
				etag = evidence.Provenance.ETag
				found = true
				continue
			}
			if evidence.Provenance.ETag != etag {
				return ""
			}
		}
	}
	if !found {
		return ""
	}
	return etag
}

// ModelsDevRootETag returns a conditional-request validator only when all
// provider slices parsed from the root models.dev response are present and
// carry the same validator. An empty result forces an unconditional repair.
func (s *Snapshot) ModelsDevRootETag() string {
	if s == nil {
		return ""
	}
	common := ""
	for _, provider := range []string{
		ProviderAnthropic,
		ProviderOpenAI,
		ProviderBaseten,
	} {
		etag := s.ModelsDevETag(provider)
		if etag == "" {
			return ""
		}
		if common == "" {
			common = etag
			continue
		}
		if etag != common {
			return ""
		}
	}
	return common
}

// QuoteProfile resolves one canonical model and execution profile from this
// exact immutable snapshot.
func (s *Snapshot) QuoteProfile(provider, model string, profile ExecutionProfile) Quote {
	return s.quoteProfileAt(
		provider,
		model,
		profile,
		time.Now().UTC(),
	)
}

func (s *Snapshot) quoteProfileAt(
	provider,
	model string,
	profile ExecutionProfile,
	effectiveAt time.Time,
) Quote {
	if s == nil {
		return Quote{}
	}
	if profile == "" {
		profile = ProfileStandard
	}
	if catalog, ok := s.providerCatalogs[provider]; ok {
		if record, ok := lookupModelRecord(catalog.models, model); ok {
			record = reevaluateOfficialPricingSupplement(
				record,
				s.officialPricingSupplement,
				effectiveAt,
			)
			if price, ok := record.Prices[profile]; ok {
				if price.RatePresenceKnown &&
					(!price.RatePresence.Input ||
						!price.RatePresence.Output) {
					return Quote{ExecutionProfile: profile}
				}
				return Quote{
					Price:             price.Price,
					Priced:            true,
					RatePresence:      price.RatePresence,
					RatePresenceKnown: price.RatePresenceKnown,
					RateProvenance: cloneRateProvenance(
						price.RateProvenance,
					),
					Source:           price.Provenance.Source,
					Revision:         price.Provenance.Revision,
					CapturedAt:       price.Provenance.CapturedAt,
					ExecutionProfile: profile,
				}
			}
		}
		return Quote{ExecutionProfile: profile}
	}
	return Quote{ExecutionProfile: profile}
}

func lookupModelRecord(records map[string]ModelRecord, model string) (ModelRecord, bool) {
	record, ok := records[model]
	return record, ok
}

func (p *Pricing) QuoteProfile(provider, model string, profile ExecutionProfile) Quote {
	return p.Capture().QuoteProfile(provider, model, profile)
}
