package pricing

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const officialPricingSupplementSource = "anthropic_official_pricing"

//go:embed official_pricing_supplement.json
var officialPricingSupplementJSON []byte

type officialPricingSupplement struct {
	source     string
	sourceURLs []string
	capturedAt time.Time
	reviewedAt time.Time
	revision   string
	contentSHA string
	entries    []officialPricingSupplementEntry
}

type officialPricingSupplementEntry struct {
	Provider       string           `json:"provider"`
	Model          string           `json:"model"`
	Profile        ExecutionProfile `json:"profile"`
	Dimension      RateDimension    `json:"dimension"`
	USDPerMillion  float64          `json:"usd_per_million"`
	EffectiveFrom  string           `json:"effective_from,omitempty"`
	EffectiveUntil string           `json:"effective_until,omitempty"`
}

type officialPricingSupplementEnvelope struct {
	SchemaVersion int                              `json:"schema_version"`
	Source        string                           `json:"source"`
	SourceURLs    []string                         `json:"source_urls"`
	CapturedAt    time.Time                        `json:"captured_at"`
	ReviewedAt    time.Time                        `json:"reviewed_at"`
	ContentSHA256 string                           `json:"content_sha256"`
	Entries       []officialPricingSupplementEntry `json:"entries"`
}

func parseOfficialPricingSupplement(
	body []byte,
) (officialPricingSupplement, error) {
	var envelope officialPricingSupplementEnvelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return officialPricingSupplement{}, fmt.Errorf(
			"decode official pricing supplement: %w",
			err,
		)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return officialPricingSupplement{}, fmt.Errorf(
				"official pricing supplement contains multiple JSON values",
			)
		}
		return officialPricingSupplement{}, fmt.Errorf(
			"decode official pricing supplement trailing data: %w",
			err,
		)
	}
	if envelope.SchemaVersion != 1 {
		return officialPricingSupplement{}, fmt.Errorf(
			"official pricing supplement schema_version = %d, want 1",
			envelope.SchemaVersion,
		)
	}
	if envelope.Source != officialPricingSupplementSource {
		return officialPricingSupplement{}, fmt.Errorf(
			"official pricing supplement source %q is unsupported",
			envelope.Source,
		)
	}
	if envelope.CapturedAt.IsZero() || envelope.ReviewedAt.IsZero() {
		return officialPricingSupplement{}, fmt.Errorf(
			"official pricing supplement dates are required",
		)
	}
	if envelope.ReviewedAt.Before(envelope.CapturedAt) {
		return officialPricingSupplement{}, fmt.Errorf(
			"official pricing supplement reviewed_at precedes captured_at",
		)
	}
	if err := validateSupplementSourceURLs(envelope.SourceURLs); err != nil {
		return officialPricingSupplement{}, err
	}
	if len(envelope.Entries) == 0 {
		return officialPricingSupplement{}, fmt.Errorf(
			"official pricing supplement contains no entries",
		)
	}
	for index, entry := range envelope.Entries {
		if err := validateSupplementEntry(entry); err != nil {
			return officialPricingSupplement{}, fmt.Errorf(
				"official pricing supplement entry %d: %w",
				index,
				err,
			)
		}
		if index > 0 &&
			!supplementEntryLess(envelope.Entries[index-1], entry) {
			return officialPricingSupplement{}, fmt.Errorf(
				"official pricing supplement entries must be unique and sorted",
			)
		}
	}
	if err := validateSupplementWindows(envelope.Entries); err != nil {
		return officialPricingSupplement{}, err
	}
	encodedEntries, err := json.Marshal(envelope.Entries)
	if err != nil {
		return officialPricingSupplement{}, fmt.Errorf(
			"encode official pricing supplement entries: %w",
			err,
		)
	}
	contentHash := sha256.Sum256(encodedEntries)
	contentSHA := hex.EncodeToString(contentHash[:])
	if envelope.ContentSHA256 != contentSHA {
		return officialPricingSupplement{}, fmt.Errorf(
			"official pricing supplement content_sha256 %q does not match entries %q",
			envelope.ContentSHA256,
			contentSHA,
		)
	}
	return officialPricingSupplement{
		source:     envelope.Source,
		sourceURLs: append([]string(nil), envelope.SourceURLs...),
		capturedAt: envelope.CapturedAt.UTC(),
		reviewedAt: envelope.ReviewedAt.UTC(),
		revision:   "sha256:" + contentSHA,
		contentSHA: contentSHA,
		entries:    append([]officialPricingSupplementEntry(nil), envelope.Entries...),
	}, nil
}

func validateSupplementSourceURLs(values []string) error {
	if len(values) == 0 {
		return fmt.Errorf(
			"official pricing supplement source_urls is required",
		)
	}
	previous := ""
	for _, value := range values {
		parsed, err := url.Parse(value)
		if err != nil ||
			parsed.Scheme != "https" ||
			parsed.Host == "" ||
			parsed.User != nil ||
			parsed.Fragment != "" {
			return fmt.Errorf(
				"official pricing supplement source URL %q is invalid",
				value,
			)
		}
		if previous != "" && value <= previous {
			return fmt.Errorf(
				"official pricing supplement source_urls must be unique and sorted",
			)
		}
		previous = value
	}
	return nil
}

func validateSupplementEntry(
	entry officialPricingSupplementEntry,
) error {
	if entry.Provider == ProviderBaseten {
		return fmt.Errorf("Baseten supplement entries are forbidden")
	}
	if entry.Provider != ProviderAnthropic &&
		entry.Provider != ProviderOpenAI {
		return fmt.Errorf("provider %q is unsupported", entry.Provider)
	}
	if entry.Model == "" ||
		strings.TrimSpace(entry.Model) != entry.Model ||
		strings.ContainsAny(entry.Model, "[] \t\r\n") {
		return fmt.Errorf("model %q is not an exact canonical ID", entry.Model)
	}
	switch entry.Profile {
	case ProfileStandard, ProfileFast:
	default:
		return fmt.Errorf("profile %q is unsupported", entry.Profile)
	}
	if !validRateDimension(entry.Dimension) {
		return fmt.Errorf("dimension %q is unsupported", entry.Dimension)
	}
	if math.IsNaN(entry.USDPerMillion) ||
		math.IsInf(entry.USDPerMillion, 0) ||
		entry.USDPerMillion < 0 {
		return fmt.Errorf("usd_per_million must be finite and nonnegative")
	}
	if _, err := nanoUSDPerToken(entry.USDPerMillion); err != nil {
		return fmt.Errorf("usd_per_million: %w", err)
	}
	return validateEffectiveWindow(
		entry.EffectiveFrom,
		entry.EffectiveUntil,
	)
}

func supplementEntryLess(
	left,
	right officialPricingSupplementEntry,
) bool {
	leftKey := supplementEntrySortKey(left)
	rightKey := supplementEntrySortKey(right)
	return leftKey < rightKey
}

func supplementEntrySortKey(
	entry officialPricingSupplementEntry,
) string {
	return strings.Join([]string{
		entry.Provider,
		entry.Model,
		string(entry.Profile),
		string(entry.Dimension),
		entry.EffectiveFrom,
		entry.EffectiveUntil,
		strconv.FormatFloat(entry.USDPerMillion, 'g', -1, 64),
	}, "\x00")
}

func validateSupplementWindows(
	entries []officialPricingSupplementEntry,
) error {
	type key struct {
		provider  string
		model     string
		profile   ExecutionProfile
		dimension RateDimension
	}
	grouped := make(map[key][]officialPricingSupplementEntry)
	for _, entry := range entries {
		entryKey := key{
			provider: entry.Provider, model: entry.Model,
			profile: entry.Profile, dimension: entry.Dimension,
		}
		grouped[entryKey] = append(grouped[entryKey], entry)
	}
	for entryKey, values := range grouped {
		for left := 0; left < len(values); left++ {
			for right := left + 1; right < len(values); right++ {
				if supplementWindowsOverlap(values[left], values[right]) {
					return fmt.Errorf(
						"official pricing supplement has overlapping windows for %s/%s/%s/%s",
						entryKey.provider,
						entryKey.model,
						entryKey.profile,
						entryKey.dimension,
					)
				}
			}
		}
	}
	return nil
}

func supplementWindowsOverlap(
	left,
	right officialPricingSupplementEntry,
) bool {
	leftStart := left.EffectiveFrom
	rightStart := right.EffectiveFrom
	leftEnd := left.EffectiveUntil
	rightEnd := right.EffectiveUntil
	return (leftEnd == "" || rightStart == "" || rightStart <= leftEnd) &&
		(rightEnd == "" || leftStart == "" || leftStart <= rightEnd)
}

func supplementEntryActive(
	entry officialPricingSupplementEntry,
	date string,
) bool {
	return (entry.EffectiveFrom == "" || date >= entry.EffectiveFrom) &&
		(entry.EffectiveUntil == "" || date <= entry.EffectiveUntil)
}

func activeOfficialSupplementEntry(
	supplement officialPricingSupplement,
	provider,
	model string,
	profile ExecutionProfile,
	dimension RateDimension,
	effectiveAt time.Time,
) (officialPricingSupplementEntry, bool) {
	if effectiveAt.IsZero() {
		return officialPricingSupplementEntry{}, false
	}
	date := effectiveAt.UTC().Format(time.DateOnly)
	for _, entry := range supplement.entries {
		if entry.Provider == provider &&
			entry.Model == model &&
			entry.Profile == profile &&
			entry.Dimension == dimension &&
			supplementEntryActive(entry, date) {
			return entry, true
		}
	}
	return officialPricingSupplementEntry{}, false
}

// reevaluateOfficialPricingSupplement updates only supplement-owned dimensions
// against the supplied instant, then fills any newly active missing dimensions.
// The caller owns the record copy. Upstream-owned values always remain intact.
func reevaluateOfficialPricingSupplement(
	record ModelRecord,
	supplement officialPricingSupplement,
	effectiveAt time.Time,
) ModelRecord {
	if len(supplement.entries) == 0 || effectiveAt.IsZero() {
		return record
	}
	for profile, price := range record.Prices {
		if !price.RatePresenceKnown {
			continue
		}
		for _, dimension := range allRateDimensions() {
			currentProvenance := rateDimensionProvenance(
				price.RateProvenance,
				dimension,
			)
			active, found := activeOfficialSupplementEntry(
				supplement,
				record.Provider,
				record.CanonicalModelID,
				profile,
				dimension,
				effectiveAt,
			)
			if currentProvenance.Source ==
				officialPricingSupplementSource {
				setRateDimensionValue(&price.Price, dimension, 0)
				setRatePresence(
					&price.RatePresence,
					dimension,
					false,
				)
				setRateDimensionProvenance(
					&price.RateProvenance,
					dimension,
					Provenance{},
				)
			} else if rateDimensionPresent(
				price.RatePresence,
				dimension,
			) {
				continue
			}
			if !found {
				continue
			}
			provenance := Provenance{
				Source:         supplement.source,
				LoadedFrom:     LoadedFromVendoredFallback,
				Revision:       supplement.revision,
				CapturedAt:     supplement.capturedAt,
				EffectiveFrom:  active.EffectiveFrom,
				EffectiveUntil: active.EffectiveUntil,
			}
			setRateDimensionValue(
				&price.Price,
				dimension,
				active.USDPerMillion,
			)
			setRatePresence(
				&price.RatePresence,
				dimension,
				true,
			)
			setRateDimensionProvenance(
				&price.RateProvenance,
				dimension,
				provenance,
			)
		}
		record.Prices[profile] = price
	}
	return record
}

func applyOfficialPricingSupplement(
	catalogs map[string]providerCatalog,
	supplement officialPricingSupplement,
	effectiveAt time.Time,
) {
	if len(supplement.entries) == 0 || effectiveAt.IsZero() {
		return
	}
	date := effectiveAt.UTC().Format(time.DateOnly)
	for _, entry := range supplement.entries {
		if !supplementEntryActive(entry, date) {
			continue
		}
		catalog, ok := catalogs[entry.Provider]
		if !ok {
			continue
		}
		record, ok := catalog.models[entry.Model]
		if !ok {
			continue
		}
		definition, ok := record.Profiles[entry.Profile]
		if !ok || !definition.Supported {
			continue
		}
		price, priced := record.Prices[entry.Profile]
		if !priced {
			price = PriceProfile{
				Profile:           entry.Profile,
				RatePresenceKnown: true,
			}
		}
		if price.RatePresenceKnown &&
			rateDimensionPresent(
				price.RatePresence,
				entry.Dimension,
			) {
			upstream := rateDimensionValue(price.Price, entry.Dimension)
			if upstream != entry.USDPerMillion {
				catalog.metadata.Diagnostics = append(
					catalog.metadata.Diagnostics,
					supplementConflictDiagnostic(entry, upstream),
				)
			}
			catalogs[entry.Provider] = catalog
			continue
		}
		if !price.RatePresenceKnown {
			continue
		}
		provenance := Provenance{
			Source:         supplement.source,
			LoadedFrom:     LoadedFromVendoredFallback,
			Revision:       supplement.revision,
			CapturedAt:     supplement.capturedAt,
			EffectiveFrom:  entry.EffectiveFrom,
			EffectiveUntil: entry.EffectiveUntil,
		}
		setRateDimensionValue(
			&price.Price,
			entry.Dimension,
			entry.USDPerMillion,
		)
		setRatePresence(
			&price.RatePresence,
			entry.Dimension,
			true,
		)
		setRateDimensionProvenance(
			&price.RateProvenance,
			entry.Dimension,
			provenance,
		)
		if !priced {
			if !price.RatePresence.Input ||
				!price.RatePresence.Output {
				continue
			}
			price.Provenance = provenance
		}
		record.Prices[entry.Profile] = price
		catalog.models[entry.Model] = record
		catalogs[entry.Provider] = catalog
	}
}

func supplementConflictDiagnostic(
	entry officialPricingSupplementEntry,
	upstream float64,
) string {
	return fmt.Sprintf(
		"kept upstream %s/%s/%s/%s rate %s over official supplement rate %s",
		entry.Provider,
		entry.Model,
		entry.Profile,
		entry.Dimension,
		strconv.FormatFloat(upstream, 'g', -1, 64),
		strconv.FormatFloat(entry.USDPerMillion, 'g', -1, 64),
	)
}

func cloneOfficialPricingSupplement(
	source officialPricingSupplement,
) officialPricingSupplement {
	return officialPricingSupplement{
		source:     strings.Clone(source.source),
		sourceURLs: append([]string(nil), source.sourceURLs...),
		capturedAt: source.capturedAt,
		reviewedAt: source.reviewedAt,
		revision:   strings.Clone(source.revision),
		contentSHA: strings.Clone(source.contentSHA),
		entries:    append([]officialPricingSupplementEntry(nil), source.entries...),
	}
}
