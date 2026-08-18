// Package claude collects a narrow, normalized subset of Claude Code session
// records for traces that can be linked with high confidence. It never returns
// source paths or original structural identifiers.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
)

const (
	ClientName            = "claude-code"
	CollectorVersion      = "claude-code-native-v1"
	NativeSchemaVersionV1 = 1

	MaxSourceFiles           = 10_000
	MaxSourceBytes     int64 = 8 << 30
	MaxRecordBytes           = 32 << 20
	MaxNativeTurns           = 10_000
	MaxNormalizedBytes       = 24 << 20
)

var (
	ErrInvalidSelection = errors.New("claude native capture: invalid selection")
	ErrLimitExceeded    = errors.New("claude native capture: limit exceeded")
	ErrExplicitSource   = errors.New("claude native capture: explicit source unavailable")
)

// Selection describes the native data that may be inspected. The interval is
// half-open: [Since, Until). ResponseBody contains the exact decoded response
// bytes from a selected Switch trace, not a reconstructed native response.
type Selection struct {
	Since            time.Time
	Until            time.Time
	Traces           []TraceReference
	ExplicitSessions []string
}

type TraceReference struct {
	EventID           string
	StartedAt         time.Time
	CompletedAt       time.Time
	ResponseBody      []byte
	NativeCorrelation *tracecapture.NativeCorrelationV1
}

// Plan is a fixed-offset, read-only discovery result. Its source file details
// are intentionally private so callers cannot accidentally expose paths.
type Plan struct {
	CandidateFileCount int
	CandidateBytes     int64

	selection Selection
	files     []sourceFile
}

type Result struct {
	CollectorVersion string
	ClientVersions   []string
	Turns            []NativeTurnV1
	// TraceLinks maps a Switch event ID to exactly one bundle-local turn ID.
	TraceLinks         map[string]string
	Exclusions         map[string]int
	CorrelationMethods []string
}

type NativeTurnV1 struct {
	NativeSchemaVersion int             `json:"native_schema_version"`
	Client              string          `json:"client"`
	BundleSessionID     string          `json:"bundle_session_id"`
	BundleTurnID        string          `json:"bundle_turn_id"`
	BundleAgentID       *string         `json:"bundle_agent_id,omitempty"`
	BundleParentToolID  *string         `json:"bundle_parent_tool_call_id,omitempty"`
	SwitchEventIDs      []string        `json:"switch_event_ids"`
	MatchMode           string          `json:"match_mode"`
	StartedAt           time.Time       `json:"started_at"`
	CompletedAt         time.Time       `json:"completed_at"`
	Status              string          `json:"status"`
	Events              []NativeEventV1 `json:"events"`
}

type NativeEventV1 struct {
	Kind                string            `json:"kind"`
	BundleEventID       string            `json:"bundle_event_id"`
	BundleParentEventID *string           `json:"bundle_parent_event_id,omitempty"`
	At                  time.Time         `json:"at"`
	Role                string            `json:"role,omitempty"`
	Subtype             string            `json:"subtype,omitempty"`
	Content             []NativeContentV1 `json:"content,omitempty"`
}

type NativeContentV1 struct {
	Type             string          `json:"type"`
	Text             string          `json:"text,omitempty"`
	Thinking         string          `json:"thinking,omitempty"`
	Name             string          `json:"name,omitempty"`
	BundleToolCallID *string         `json:"bundle_tool_call_id,omitempty"`
	Input            json.RawMessage `json:"input,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	IsError          bool            `json:"is_error,omitempty"`
}

// Collector is safe for a single Discover/Collect operation. CorrelationKey is
// optional. When absent or when its ID differs from a trace, keyed correlation
// is unavailable and the response-ID fallback remains available.
type Collector struct {
	ConfigRoot     string
	CorrelationKey *tracecapture.CorrelationKey

	// afterRead is a test seam invoked after a source boundary is read and
	// before it is verified. Production callers leave it nil.
	afterRead func(path string)
}

// NativeCollector is the versioned surface used by the package command.
type NativeCollector interface {
	Discover(context.Context, Selection) (Plan, error)
	Collect(context.Context, Plan) (Result, error)
}

var _ NativeCollector = Collector{}
