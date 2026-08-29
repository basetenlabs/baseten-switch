package tracepackage

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
)

type indexedTrace struct {
	EventID       string
	Client        string
	StartedAt     time.Time
	SchemaVersion int
	Packaged      PackagedTraceV1
	Bodies        []BodyForScan
}

type traceIndexFields struct {
	SchemaVersion int             `json:"schema_version"`
	Event         string          `json:"event"`
	EventID       string          `json:"event_id"`
	StartedAt     time.Time       `json:"started_at"`
	Client        string          `json:"client"`
	Request       json.RawMessage `json:"request"`
	Response      json.RawMessage `json:"response"`
}

type traceBodyFields struct {
	Boundary        string          `json:"boundary"`
	ContentType     string          `json:"content_type"`
	ContentEncoding string          `json:"content_encoding"`
	BodyEncoding    string          `json:"body_encoding"`
	BodyBase64      json.RawMessage `json:"body_base64"`
	CaptureState    string          `json:"capture_state"`
}

func projectTraceV1(line []byte) (indexedTrace, error) {
	if len(bytes.TrimSpace(line)) == 0 {
		return indexedTrace{}, errorsMalformed("empty trace row")
	}
	if _, err := tracecapture.DecodeTraceV1Strict(line); err != nil {
		return indexedTrace{}, errorsMalformed("decode trace row: %v", err)
	}
	var index traceIndexFields
	if err := json.Unmarshal(line, &index); err != nil {
		return indexedTrace{}, errorsMalformed("decode trace row: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return indexedTrace{}, errorsMalformed("decode trace fields: %v", err)
	}
	for _, reserved := range []string{
		"package_schema_version",
		"source_trace_schema_version",
		"native_turn_id",
	} {
		if _, exists := fields[reserved]; exists {
			return indexedTrace{}, errorsMalformed("trace contains reserved field %q", reserved)
		}
	}
	delete(fields, "native_correlation")

	bodies := make([]BodyForScan, 0, 2)
	for _, raw := range []json.RawMessage{index.Request, index.Response} {
		body, ok, err := bodyForScan(index.EventID, raw)
		if err != nil {
			return indexedTrace{}, err
		}
		if ok {
			bodies = append(bodies, body)
		}
	}
	return indexedTrace{
		EventID:       index.EventID,
		Client:        index.Client,
		StartedAt:     index.StartedAt.UTC(),
		SchemaVersion: index.SchemaVersion,
		Packaged: PackagedTraceV1{
			PackageSchemaVersion:     PackageSchemaVersionV1,
			SourceTraceSchemaVersion: index.SchemaVersion,
			Fields:                   fields,
		},
		Bodies: bodies,
	}, nil
}

func bodyForScan(eventID string, raw json.RawMessage) (BodyForScan, bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return BodyForScan{}, false, nil
	}
	var body traceBodyFields
	if err := json.Unmarshal(raw, &body); err != nil {
		return BodyForScan{}, false, errorsMalformed("decode trace body metadata: %v", err)
	}
	if body.CaptureState != "captured" {
		return BodyForScan{}, false, nil
	}
	if body.BodyEncoding != "base64" {
		return BodyForScan{}, false, errorsMalformed("captured body is not base64 encoded")
	}
	if len(body.BodyBase64) == 0 {
		return BodyForScan{}, false, errorsMalformed("captured body has no body_base64 field")
	}
	var encoded string
	if err := json.Unmarshal(body.BodyBase64, &encoded); err != nil {
		return BodyForScan{}, false, errorsMalformed("captured body_base64 is not a string")
	}
	return BodyForScan{
		EventID:         eventID,
		Boundary:        body.Boundary,
		ContentType:     body.ContentType,
		ContentEncoding: body.ContentEncoding,
		BodyBase64:      encoded,
	}, true, nil
}

func validEventID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16
}

type malformedRowError struct{ message string }

func (e *malformedRowError) Error() string { return e.message }

func errorsMalformed(format string, args ...any) error {
	return &malformedRowError{message: fmt.Sprintf(format, args...)}
}
