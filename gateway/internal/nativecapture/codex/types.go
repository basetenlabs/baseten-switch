// Package codex collects a narrow, normalized subset of Codex rollout records
// for Switch traces that can be linked with high confidence. It never returns
// source paths or original structural identifiers.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
)

const (
	ClientName            = "codex"
	CollectorVersion      = "codex-native-v1"
	NativeSchemaVersionV1 = 1

	MaxSourceFiles           = 10_000
	MaxSourceBytes     int64 = 8 << 30
	MaxRecordBytes           = 32 << 20
	MaxNativeTurns           = 10_000
	MaxNormalizedBytes       = 24 << 20
)

var (
	ErrInvalidSelection = errors.New("codex native capture: invalid selection")
	ErrLimitExceeded    = errors.New("codex native capture: limit exceeded")
	ErrExplicitSource   = errors.New("codex native capture: explicit source unavailable")
)

// Selection describes the native data that may be inspected. The interval is
// half-open: [Since, Until). Archived rollouts are never inspected unless
// IncludeArchived is true.
type Selection struct {
	Since            time.Time
	Until            time.Time
	Traces           []TraceReference
	ExplicitSessions []string
	IncludeArchived  bool
}

// TraceReference contains only the Switch fields required to correlate one
// exact Responses request with a native turn.
type TraceReference struct {
	EventID           string
	StartedAt         time.Time
	CompletedAt       time.Time
	RequestBody       []byte
	RequestedModel    string
	NativeCorrelation *tracecapture.NativeCorrelationV1
}

// Plan is a fixed-offset, read-only discovery result. Source paths and native
// identifiers remain private so callers cannot accidentally expose them.
type Plan struct {
	CandidateFileCount int
	CandidateBytes     int64

	selection Selection
	files     []sourceFile
}

type Result struct {
	CollectorVersion   string
	ClientVersions     []string
	Turns              []NativeTurnV1
	TraceLinks         map[string]string
	Exclusions         map[string]int
	CorrelationMethods []string
	SchemaDrift        SchemaDriftSummary
}

// SchemaDriftSummary reports only counts and a derived status. It never
// includes native record types, field names, source paths, or content.
type SchemaDriftSummary struct {
	Status                 string
	IgnoredMetadataRecords int
	ExcludedSemanticTurns  int
	ExcludedSources        int
}

func (s *SchemaDriftSummary) finalize(collectedTurns int) {
	switch {
	case s.IgnoredMetadataRecords == 0 && s.ExcludedSemanticTurns == 0 && s.ExcludedSources == 0:
		s.Status = "complete"
	case collectedTurns > 0:
		s.Status = "partial"
	default:
		s.Status = "unavailable"
	}
}

type NativeTurnV1 struct {
	NativeSchemaVersion int             `json:"native_schema_version"`
	Client              string          `json:"client"`
	BundleSessionID     string          `json:"bundle_session_id"`
	BundleTurnID        string          `json:"bundle_turn_id"`
	SwitchEventIDs      []string        `json:"switch_event_ids"`
	MatchMode           string          `json:"match_mode"`
	StartedAt           time.Time       `json:"started_at"`
	CompletedAt         time.Time       `json:"completed_at"`
	Status              string          `json:"status"`
	Events              []NativeEventV1 `json:"events"`
}

// NativeEventV1 is intentionally narrower than Codex's unstable rollout
// schema. Data contains only an event-specific allowlist after structural IDs,
// local paths, repository metadata, and opaque reasoning have been removed.
type NativeEventV1 struct {
	Kind             string          `json:"kind"`
	At               time.Time       `json:"at"`
	Role             string          `json:"role,omitempty"`
	Name             string          `json:"name,omitempty"`
	Status           string          `json:"status,omitempty"`
	BundleItemID     *string         `json:"bundle_item_id,omitempty"`
	BundleToolCallID *string         `json:"bundle_tool_call_id,omitempty"`
	BundleRelatedIDs []string        `json:"bundle_related_ids,omitempty"`
	Data             json.RawMessage `json:"data,omitempty"`
}

type Collector struct {
	// CodexHome overrides discovery for tests and embedding. When empty,
	// CODEX_HOME is used, followed by ~/.codex.
	CodexHome      string
	CorrelationKey *tracecapture.CorrelationKey

	// afterRead is a test seam invoked after reading a fixed boundary and
	// before verifying the file identity. Production callers leave it nil.
	afterRead func(path string)
}

type NativeCollector interface {
	Discover(context.Context, Selection) (Plan, error)
	Collect(context.Context, Plan) (Result, error)
}

var _ NativeCollector = Collector{}
