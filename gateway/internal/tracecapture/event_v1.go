// Package tracecapture stores explicitly enabled, high-sensitivity request and
// response traces. It is intentionally separate from metadata telemetry.
package tracecapture

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

func DecodeTraceV1Strict(encoded []byte) (TraceV1, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var trace TraceV1
	if err := decoder.Decode(&trace); err != nil {
		return TraceV1{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return TraceV1{}, errors.New("trace row must contain one JSON object")
	}
	if err := trace.Validate(); err != nil {
		return TraceV1{}, err
	}
	return trace, nil
}

const (
	SchemaVersionV1 = 1
	EventTraceV1    = "request_response_trace"

	MaxBodyBytes       = 16 << 20
	MaxEncodedRowBytes = 48 << 20
)

type ProtocolShape string

const (
	ProtocolAnthropic ProtocolShape = "anthropic"
	ProtocolOpenAI    ProtocolShape = "openai"
)

type APIKind string

const (
	APIKindMessages        APIKind = "messages"
	APIKindChatCompletions APIKind = "chat_completions"
	APIKindResponses       APIKind = "responses"
)

type TerminationReason string

const (
	TerminationCompleted              TerminationReason = "completed"
	TerminationClientCancelled        TerminationReason = "client_cancelled"
	TerminationUpstreamHTTPError      TerminationReason = "upstream_http_error"
	TerminationUpstreamTransportError TerminationReason = "upstream_transport_error"
	TerminationIncompleteStream       TerminationReason = "incomplete_stream"
	TerminationGatewayError           TerminationReason = "gateway_error"
)

type OutcomeSource string

const (
	OutcomeSourceProvider OutcomeSource = "provider"
	OutcomeSourceGateway  OutcomeSource = "gateway"
)

type ProviderOutcome string

const (
	ProviderOutcomeCompleted  ProviderOutcome = "completed"
	ProviderOutcomeFailed     ProviderOutcome = "failed"
	ProviderOutcomeIncomplete ProviderOutcome = "incomplete"
	ProviderOutcomeUnknown    ProviderOutcome = "unknown"
)

type CaptureState string

const (
	CaptureStateCaptured           CaptureState = "captured"
	CaptureStateOmittedSizeLimit   CaptureState = "omitted_size_limit"
	CaptureStateOmittedMemoryLimit CaptureState = "omitted_memory_limit"
	CaptureStateUnavailable        CaptureState = "unavailable"
)

type NativeCorrelationV1 struct {
	Scheme  string  `json:"scheme"`
	KeyID   string  `json:"key_id"`
	Session *string `json:"session"`
	Turn    *string `json:"turn"`
	Agent   *string `json:"agent"`
}

type FallbackV1 struct {
	Attempted        bool    `json:"attempted"`
	Count            int     `json:"count"`
	PrimaryAttempted bool    `json:"primary_attempted"`
	Trigger          *string `json:"trigger"`
}

type BodyV1 struct {
	Boundary        string       `json:"boundary"`
	ContentType     string       `json:"content_type"`
	ContentEncoding string       `json:"content_encoding"`
	BodyEncoding    string       `json:"body_encoding"`
	BodyBase64      string       `json:"body_base64"`
	ObservedBytes   int64        `json:"observed_bytes"`
	CaptureState    CaptureState `json:"capture_state"`
	RawBody         []byte       `json:"-"`
}

func (t TraceV1) MarshalJSON() ([]byte, error) {
	type wireTrace TraceV1
	wire := wireTrace(t)
	if wire.Request.CaptureState == CaptureStateCaptured && wire.Request.RawBody != nil {
		wire.Request.BodyBase64 = base64.StdEncoding.EncodeToString(wire.Request.RawBody)
	}
	if wire.Response.CaptureState == CaptureStateCaptured && wire.Response.RawBody != nil {
		wire.Response.BodyBase64 = base64.StdEncoding.EncodeToString(wire.Response.RawBody)
	}
	return json.Marshal(wire)
}

type ResponseBodyV1 struct {
	BodyV1
	GatewayWriteComplete bool `json:"gateway_write_complete"`
	ProtocolComplete     bool `json:"protocol_complete"`
}

// MarshalJSON keeps the embedded response body fields at the same JSON level
// as the response completion fields.
//
// The default encoding/json behavior already flattens an anonymous exported
// struct, so this declaration is documentation rather than custom encoding.

type TraceV1 struct {
	SchemaVersion     int                  `json:"schema_version"`
	Event             string               `json:"event"`
	EventID           string               `json:"event_id"`
	NativeCorrelation *NativeCorrelationV1 `json:"native_correlation"`
	StartedAt         time.Time            `json:"started_at"`
	CompletedAt       time.Time            `json:"completed_at"`
	Client            string               `json:"client"`
	ProtocolShape     ProtocolShape        `json:"protocol_shape"`
	APIKind           APIKind              `json:"api_kind"`
	Endpoint          string               `json:"endpoint"`
	ConfiguredRoute   *string              `json:"configured_route"`
	EffectiveProvider *string              `json:"effective_provider"`
	RequestedModel    *string              `json:"requested_model"`
	ServedModel       *string              `json:"served_model"`
	Status            *int                 `json:"status"`
	TerminationReason TerminationReason    `json:"termination_reason"`
	OutcomeSource     OutcomeSource        `json:"outcome_source"`
	ProviderOutcome   ProviderOutcome      `json:"provider_outcome"`
	IsStream          bool                 `json:"is_stream"`
	ProviderStateful  bool                 `json:"provider_stateful"`
	Translated        bool                 `json:"translated"`
	Sanitized         bool                 `json:"sanitized"`
	Fallback          FallbackV1           `json:"fallback"`
	Request           BodyV1               `json:"request"`
	Response          ResponseBodyV1       `json:"response"`
}

func (t TraceV1) Validate() error {
	if t.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("schema_version must be %d", SchemaVersionV1)
	}
	if t.Event != EventTraceV1 {
		return fmt.Errorf("event must be %q", EventTraceV1)
	}
	if len(t.EventID) != 32 {
		return fmt.Errorf("event_id must be 32 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(t.EventID); err != nil || t.EventID != strings.ToLower(t.EventID) {
		return fmt.Errorf("event_id must be 32 lowercase hexadecimal characters")
	}
	if t.StartedAt.IsZero() || t.CompletedAt.IsZero() || t.CompletedAt.Before(t.StartedAt) {
		return fmt.Errorf("started_at and completed_at must define a nonnegative interval")
	}
	if strings.TrimSpace(t.Client) == "" {
		return fmt.Errorf("client must not be empty")
	}
	if err := validateProtocolAndEndpoint(t.ProtocolShape, t.APIKind, t.Endpoint); err != nil {
		return err
	}
	if t.Status != nil && (*t.Status < 100 || *t.Status > 599) {
		return fmt.Errorf("status must be a valid HTTP status or null")
	}
	if !validTermination(t.TerminationReason) {
		return fmt.Errorf("invalid termination_reason %q", t.TerminationReason)
	}
	if t.OutcomeSource != OutcomeSourceProvider && t.OutcomeSource != OutcomeSourceGateway {
		return fmt.Errorf("invalid outcome_source %q", t.OutcomeSource)
	}
	if !validProviderOutcome(t.ProviderOutcome) {
		return fmt.Errorf("invalid provider_outcome %q", t.ProviderOutcome)
	}
	if t.OutcomeSource == OutcomeSourceGateway && (t.EffectiveProvider != nil || t.ServedModel != nil) {
		return fmt.Errorf("gateway outcomes must not identify an effective provider or served model")
	}
	if t.Fallback.Count < 0 {
		return fmt.Errorf("fallback count must not be negative")
	}
	if t.Fallback.Attempted && t.Fallback.Count == 0 {
		return fmt.Errorf("attempted fallback must have a positive count")
	}
	if !t.Fallback.Attempted && t.Fallback.Count != 0 {
		return fmt.Errorf("fallback count requires attempted=true")
	}
	if err := t.Request.validate("client_ingress"); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := t.Response.BodyV1.validate("gateway_egress"); err != nil {
		return fmt.Errorf("response: %w", err)
	}
	if t.Response.ProtocolComplete && !t.Response.GatewayWriteComplete {
		return fmt.Errorf("protocol_complete requires gateway_write_complete")
	}
	if err := t.NativeCorrelation.validate(); err != nil {
		return fmt.Errorf("native_correlation: %w", err)
	}
	return nil
}

func (b BodyV1) validate(boundary string) error {
	if b.Boundary != boundary {
		return fmt.Errorf("boundary must be %q", boundary)
	}
	if b.ObservedBytes < 0 {
		return fmt.Errorf("observed_bytes must not be negative")
	}
	if !validCaptureState(b.CaptureState) {
		return fmt.Errorf("invalid capture_state %q", b.CaptureState)
	}
	if b.BodyEncoding != "base64" {
		return fmt.Errorf("body_encoding must be %q", "base64")
	}
	if b.CaptureState != CaptureStateCaptured {
		if b.BodyBase64 != "" || len(b.RawBody) != 0 {
			return fmt.Errorf("omitted or unavailable body must not contain body_base64")
		}
		return nil
	}
	if b.RawBody != nil {
		if b.BodyBase64 != "" {
			return fmt.Errorf("captured body must not contain both raw and base64 data")
		}
		if len(b.RawBody) > MaxBodyBytes {
			return fmt.Errorf("captured body exceeds %d bytes", MaxBodyBytes)
		}
		if int64(len(b.RawBody)) != b.ObservedBytes {
			return fmt.Errorf("observed_bytes does not match captured body length")
		}
		return nil
	}
	decodedLength, err := decodedBase64Length(b.BodyBase64)
	if err != nil {
		return fmt.Errorf("body_base64 is invalid: %w", err)
	}
	if decodedLength > MaxBodyBytes {
		return fmt.Errorf("captured body exceeds %d bytes", MaxBodyBytes)
	}
	if int64(decodedLength) != b.ObservedBytes {
		return fmt.Errorf("observed_bytes does not match captured body length")
	}
	return nil
}

func decodedBase64Length(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return 0, err
	}
	return len(decoded), nil
}

func (c *NativeCorrelationV1) validate() error {
	if c == nil {
		return nil
	}
	if c.Scheme != "hmac-sha256-v1" {
		return fmt.Errorf("unsupported scheme %q", c.Scheme)
	}
	if !isLowerHex(c.KeyID, 16) {
		return fmt.Errorf("key_id must be 16 lowercase hexadecimal characters")
	}
	values := []*string{c.Session, c.Turn, c.Agent}
	set := false
	for _, value := range values {
		if value == nil {
			continue
		}
		set = true
		if !isLowerHex(*value, 32) {
			return fmt.Errorf("correlation values must be 32 lowercase hexadecimal characters")
		}
	}
	if !set {
		return fmt.Errorf("at least one correlation value is required")
	}
	return nil
}

func isLowerHex(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateProtocolAndEndpoint(shape ProtocolShape, kind APIKind, endpoint string) error {
	switch kind {
	case APIKindMessages:
		if shape != ProtocolAnthropic || endpoint != "/v1/messages" {
			return fmt.Errorf("messages requires anthropic shape and /v1/messages endpoint")
		}
	case APIKindChatCompletions:
		if shape != ProtocolOpenAI || endpoint != "/v1/chat/completions" {
			return fmt.Errorf("chat_completions requires openai shape and /v1/chat/completions endpoint")
		}
	case APIKindResponses:
		if shape != ProtocolOpenAI || endpoint != "/v1/responses" {
			return fmt.Errorf("responses requires openai shape and /v1/responses endpoint")
		}
	default:
		return fmt.Errorf("invalid api_kind %q", kind)
	}
	return nil
}

func validTermination(value TerminationReason) bool {
	switch value {
	case TerminationCompleted, TerminationClientCancelled, TerminationUpstreamHTTPError,
		TerminationUpstreamTransportError, TerminationIncompleteStream, TerminationGatewayError:
		return true
	default:
		return false
	}
}

func validProviderOutcome(value ProviderOutcome) bool {
	switch value {
	case ProviderOutcomeCompleted, ProviderOutcomeFailed, ProviderOutcomeIncomplete, ProviderOutcomeUnknown:
		return true
	default:
		return false
	}
}

func validCaptureState(value CaptureState) bool {
	switch value {
	case CaptureStateCaptured, CaptureStateOmittedSizeLimit,
		CaptureStateOmittedMemoryLimit, CaptureStateUnavailable:
		return true
	default:
		return false
	}
}
