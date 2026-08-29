// Package tracepackage builds a bounded, local-only ZIP from captured Switch
// traces and matching metadata telemetry. It deliberately knows nothing about
// command-line parsing or the on-disk layouts owned by the source stores.
package tracepackage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

const (
	PackageSchemaVersionV1 = 1

	TraceMemberName     = "switch/traces.jsonl"
	TelemetryMemberName = "switch/telemetry.jsonl"
	ManifestMemberName  = "manifest.json"

	MaxSelectionInterval     = 30 * 24 * time.Hour
	MaxSelectedTraces        = 10_000
	MaxSelectedTelemetry     = 10_000
	MaxNormalizedNativeTurns = 10_000
	MaxTraceLineBytes        = 64 << 20
	MaxTelemetryLineBytes    = 4 << 20
	MaxNativeLineBytes       = 32 << 20
	MaxDecodedBodyScanBytes  = int64(64 << 20)
	MaxUncompressedBytes     = int64(4 << 30)
	MaxZIPBytes              = int64(2 << 30)
	MaxWorkingMemoryBytes    = int64(256 << 20)
	MaxRetainedNativeBytes   = MaxWorkingMemoryBytes * 3 / 16
	MaxSourceBytes           = int64(8 << 30)
)

var (
	ErrInvalidSelection  = errors.New("trace package: invalid selection")
	ErrLimitExceeded     = errors.New("trace package: limit exceeded")
	ErrSnapshotChanged   = errors.New("trace package: snapshot changed")
	ErrUnsafeMember      = errors.New("trace package: unsafe ZIP member")
	ErrDestinationExists = errors.New(
		"trace package: destination already exists",
	)
	ErrUnscannedContent = errors.New(
		"trace package: publication blocked by unscanned content",
	)
	ErrDetectedSecrets = errors.New(
		"trace package: publication blocked by detected credentials",
	)
)

// Selection is the normalized package interval and client allowlist. The
// interval is half-open: [Since, Until).
type Selection struct {
	Since   time.Time
	Until   time.Time
	Clients []string
}

func (s Selection) Validate() error {
	if s.Since.IsZero() || s.Until.IsZero() {
		return fmt.Errorf("%w: since and until are required", ErrInvalidSelection)
	}
	if !s.Since.Before(s.Until) {
		return fmt.Errorf("%w: since must precede until", ErrInvalidSelection)
	}
	if s.Until.Sub(s.Since) > MaxSelectionInterval {
		return fmt.Errorf(
			"%w: interval exceeds %s",
			ErrInvalidSelection,
			MaxSelectionInterval,
		)
	}
	if len(s.Clients) == 0 {
		return fmt.Errorf("%w: at least one client is required", ErrInvalidSelection)
	}
	seen := make(map[string]struct{}, len(s.Clients))
	for _, client := range s.Clients {
		if strings.TrimSpace(client) == "" || client != strings.TrimSpace(client) {
			return fmt.Errorf("%w: client values must be non-empty and trimmed", ErrInvalidSelection)
		}
		if _, exists := seen[client]; exists {
			return fmt.Errorf("%w: duplicate client %q", ErrInvalidSelection, client)
		}
		seen[client] = struct{}{}
	}
	return nil
}

func (s Selection) normalized() Selection {
	result := Selection{
		Since:   s.Since.UTC(),
		Until:   s.Until.UTC(),
		Clients: slices.Clone(s.Clients),
	}
	slices.Sort(result.Clients)
	return result
}

// Snapshot is a fixed-boundary reader obtained from a source store. Open must
// return a new reader limited to the captured boundary. Verify must detect
// replacement, truncation, or mutation within that boundary. Appends beyond
// the boundary are allowed.
type Snapshot struct {
	EstimatedBytes int64
	Open           func(context.Context) (io.ReadCloser, error)
	Verify         func(context.Context) error
}

func (s Snapshot) validate(kind string) error {
	if s.EstimatedBytes < 0 || s.EstimatedBytes > MaxSourceBytes {
		return fmt.Errorf("%w: %s snapshot bytes", ErrLimitExceeded, kind)
	}
	if s.Open == nil || s.Verify == nil {
		return fmt.Errorf("trace package: %s snapshot is incomplete", kind)
	}
	return nil
}

// TraceSnapshotFunc captures a stable trace-store view for a selection.
type TraceSnapshotFunc func(context.Context, Selection) (Snapshot, error)

// TelemetrySnapshotFunc captures a stable telemetry-store view. eventIDs is a
// sorted copy of the selected Switch event IDs. The packager still performs
// the authoritative exact-ID join after reading the returned snapshot.
type TelemetrySnapshotFunc func(
	context.Context,
	Selection,
	[]string,
) (Snapshot, error)

type Sources struct {
	Traces    TraceSnapshotFunc
	Telemetry TelemetrySnapshotFunc
}

// BodyForScan contains one exact captured body and bounded metadata. Scanners
// must not log BodyBase64 or return it in errors.
type BodyForScan struct {
	EventID         string
	Boundary        string
	ContentType     string
	ContentEncoding string
	BodyBase64      string
}

// BodyScanResult reports only content-free scanner outcomes.
type BodyScanResult struct {
	Scanned            bool
	DetectedCategories []string
}

type Scanner struct {
	Version string
	Scan    func(context.Context, BodyForScan) (BodyScanResult, error)
}

// QuarantineFunc is invoked only when immediate staging cleanup fails. A
// successful implementation moves the complete staging tree to a validated,
// private quarantine location without replacement and returns an opaque ID.
type QuarantineFunc func(context.Context, string) (string, error)

type Options struct {
	Destination   string
	Selection     Selection
	SwitchVersion string
	Sources       Sources
	Scanner       Scanner
	NativeMembers []NativeMember
	// TraceNativeLinks maps selected Switch event IDs to bundle-local native
	// turn IDs. Create rejects links that do not name a selected trace.
	TraceNativeLinks            map[string]string
	OperatorSelectedNativeTurns []string

	AllowUnscannedContent bool
	AllowDetectedSecrets  bool
	Quarantine            QuarantineFunc

	// Now is a test seam. Production callers should leave it nil.
	Now func() time.Time
}

type Result struct {
	Destination     string
	ArchiveID       string
	ArchiveSHA256   string
	TraceCount      int
	TelemetryCount  int
	NativeTurnCount int
	ArchiveBytes    int64
	Published       bool
	CleanupID       string
}

// NativeMember contains normalized native rows only. Raw session files and
// source paths must never be supplied here.
type NativeMember struct {
	Name               string
	Client             string
	SourceKind         string
	Rows               []json.RawMessage
	CollectorVersion   string
	ClientVersions     []string
	CorrelationMethods []string
	Exclusions         map[string]int
	CollectionStatus   string
	SchemaDrift        NativeSchemaDriftV1
}

// CleanupError reports that sensitive staging data could not be removed
// immediately. Published says whether the final ZIP already exists.
type CleanupError struct {
	Published    bool
	CleanupID    string
	RecoveryRoot string
	Err          error
}

type ContentScanError struct {
	Err            error
	UnscannedCount int
	CategoryCounts map[string]int
}

func (e *ContentScanError) Error() string { return e.Err.Error() }
func (e *ContentScanError) Unwrap() error { return e.Err }

func (e *CleanupError) Error() string {
	if e.CleanupID != "" {
		return fmt.Sprintf("trace package staging quarantined as %s: %v", e.CleanupID, e.Err)
	}
	return fmt.Sprintf("trace package staging cleanup failed: %v", e.Err)
}

func (e *CleanupError) Unwrap() error { return e.Err }

// PublicationError reports an error after or during no-replace publication.
// Applied is true when the destination link was created and must be treated as
// a published archive even if the durability check or staged-link cleanup
// subsequently failed.
type PublicationError struct {
	Applied bool
	Err     error
}

func (e *PublicationError) Error() string { return e.Err.Error() }
func (e *PublicationError) Unwrap() error { return e.Err }

type MemberManifestV1 struct {
	Name        string `json:"name"`
	RecordCount int    `json:"record_count"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
}

type ScannerManifestV1 struct {
	Version                    string         `json:"version"`
	ScannedBodyCount           int            `json:"scanned_body_count"`
	UnscannedBodyCount         int            `json:"unscanned_body_count"`
	DetectedCategoryCounts     map[string]int `json:"detected_category_counts"`
	ScannedNativeRecordCount   int            `json:"scanned_native_record_count"`
	UnscannedNativeRecordCount int            `json:"unscanned_native_record_count"`
	AllowUnscannedContentUsed  bool           `json:"allow_unscanned_content_used"`
	AllowDetectedSecretsUsed   bool           `json:"allow_detected_secrets_used"`
}

type NativeCollectorManifestV1 struct {
	Member             string              `json:"member"`
	Client             string              `json:"client"`
	SourceKind         string              `json:"source_kind"`
	CollectorVersion   string              `json:"collector_version"`
	ClientVersions     []string            `json:"client_versions"`
	CorrelationMethods []string            `json:"correlation_methods"`
	CollectionStatus   string              `json:"collection_status,omitempty"`
	SchemaDrift        NativeSchemaDriftV1 `json:"schema_drift,omitempty"`
}

const (
	NativeCollectionComplete    = "complete"
	NativeCollectionPartial     = "partial"
	NativeCollectionUnavailable = "unavailable"
)

// NativeSchemaDriftV1 reports only bounded counts. Native record types, field
// names, values, identifiers, paths, and parser errors must never enter it.
type NativeSchemaDriftV1 struct {
	IgnoredMetadataRecords int `json:"ignored_metadata_records"`
	ExcludedSemanticTurns  int `json:"excluded_semantic_turns"`
	ExcludedSources        int `json:"excluded_sources"`
}

type SnapshotManifestV1 struct {
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

type SelectionManifestV1 struct {
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	Clients []string  `json:"clients"`
}

type ManifestV1 struct {
	PackageSchemaVersion        int                         `json:"package_schema_version"`
	ArchiveID                   string                      `json:"archive_id"`
	CreatedAt                   time.Time                   `json:"created_at"`
	SwitchVersion               string                      `json:"switch_version"`
	TraceSchemaVersions         []int                       `json:"trace_schema_versions"`
	Selection                   SelectionManifestV1         `json:"selection"`
	Snapshot                    SnapshotManifestV1          `json:"snapshot"`
	Members                     []MemberManifestV1          `json:"members"`
	CorrelationMethods          []string                    `json:"correlation_methods"`
	NativeCollectors            []NativeCollectorManifestV1 `json:"native_collectors"`
	OperatorSelectedNativeTurns []string                    `json:"operator_selected_native_turns"`
	Scanner                     ScannerManifestV1           `json:"scanner"`
	Exclusions                  map[string]int              `json:"exclusions"`
	NoUploadPerformed           bool                        `json:"no_upload_performed"`
	SensitiveDataNotice         string                      `json:"sensitive_data_notice"`
}

// PackagedTraceV1 preserves all source trace fields except
// native_correlation, then adds package-only provenance and linkage fields.
// Fields must not contain package-only keys.
type PackagedTraceV1 struct {
	PackageSchemaVersion     int
	SourceTraceSchemaVersion int
	NativeTurnID             *string
	Fields                   map[string]json.RawMessage
}

func (p PackagedTraceV1) MarshalJSON() ([]byte, error) {
	fields := maps.Clone(p.Fields)
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	delete(fields, "native_correlation")
	for _, reserved := range []string{
		"package_schema_version",
		"source_trace_schema_version",
		"native_turn_id",
	} {
		delete(fields, reserved)
	}
	packageVersion, err := json.Marshal(p.PackageSchemaVersion)
	if err != nil {
		return nil, err
	}
	sourceVersion, err := json.Marshal(p.SourceTraceSchemaVersion)
	if err != nil {
		return nil, err
	}
	nativeTurn, err := json.Marshal(p.NativeTurnID)
	if err != nil {
		return nil, err
	}
	fields["package_schema_version"] = packageVersion
	fields["source_trace_schema_version"] = sourceVersion
	fields["native_turn_id"] = nativeTurn
	return json.Marshal(fields)
}

func (p *PackagedTraceV1) UnmarshalJSON(data []byte) error {
	if p == nil {
		return errors.New("trace package: unmarshal into nil PackagedTraceV1")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["package_schema_version"], &p.PackageSchemaVersion); err != nil {
		return fmt.Errorf("package_schema_version: %w", err)
	}
	if err := json.Unmarshal(fields["source_trace_schema_version"], &p.SourceTraceSchemaVersion); err != nil {
		return fmt.Errorf("source_trace_schema_version: %w", err)
	}
	if value, ok := fields["native_turn_id"]; ok {
		if err := json.Unmarshal(value, &p.NativeTurnID); err != nil {
			return fmt.Errorf("native_turn_id: %w", err)
		}
	}
	delete(fields, "package_schema_version")
	delete(fields, "source_trace_schema_version")
	delete(fields, "native_turn_id")
	delete(fields, "native_correlation")
	p.Fields = fields
	return nil
}
