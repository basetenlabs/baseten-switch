package tracepackage

import (
	"encoding/json"
	"sync"
	"time"
)

const DecodeSchemaVersionV1 = 1

const (
	DecodedMemberSourceManifest = "source_manifest"
	DecodedMemberIndex          = "index"
	DecodedMemberTelemetry      = "copied_telemetry"
	DecodedMemberNative         = "copied_native"
	DecodedMemberRequestBody    = "decoded_request_body"
	DecodedMemberResponseBody   = "decoded_response_body"
)

type DecodeManifestV1 struct {
	DecodeSchemaVersion        int                       `json:"decode_schema_version"`
	CreatedAt                  time.Time                 `json:"created_at"`
	DecoderSwitchVersion       string                    `json:"decoder_switch_version"`
	SourceArchiveID            string                    `json:"source_archive_id"`
	SourcePackageSchemaVersion int                       `json:"source_package_schema_version"`
	SourcePackageSHA256        string                    `json:"source_package_sha256"`
	Members                    []DecodedMemberManifestV1 `json:"members"`
	NoUploadPerformed          bool                      `json:"no_upload_performed"`
	RedactionPerformed         bool                      `json:"redaction_performed"`
	SensitiveDataNotice        string                    `json:"sensitive_data_notice"`
}

type DecodedMemberManifestV1 struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	SourceMember string `json:"source_member,omitempty"`
	EventID      string `json:"event_id,omitempty"`
	Boundary     string `json:"boundary,omitempty"`
	RecordCount  *int   `json:"record_count,omitempty"`
	Bytes        int64  `json:"bytes"`
	SHA256       string `json:"sha256"`
}

type DecodedTraceIndexV1 struct {
	DecodeSchemaVersion int                        `json:"decode_schema_version"`
	ArchiveID           string                     `json:"archive_id"`
	EventID             string                     `json:"event_id"`
	TraceMetadata       map[string]json.RawMessage `json:"trace_metadata"`
	Request             map[string]json.RawMessage `json:"request"`
	Response            map[string]json.RawMessage `json:"response"`
}

type DecodePreflight struct {
	ArchiveID         string
	PackageSHA256     string
	TraceCount        int
	CapturedBodyCount int
	OmittedBodyCount  int
	DecodedBytes      int64
	MemberNames       []string
	TelemetryRows     int
	NativeRows        int
	Scanner           ScannerManifestV1
}

type DecodePlan struct {
	Preflight DecodePreflight
	state     *decodePlanState
}

type decodePlanState struct {
	mu          sync.Mutex
	used        bool
	packagePath string
	packageInfo fileIdentity
}

type DecodeResult struct {
	OutputDir          string
	ArchiveID          string
	PackageSHA256      string
	TraceCount         int
	MaterializedBodies int
	DecodedBytes       int64
}
