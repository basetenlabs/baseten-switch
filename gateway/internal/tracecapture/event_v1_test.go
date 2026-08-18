package tracecapture

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTraceV1RoundTripAndValidation(t *testing.T) {
	trace := validTrace(t, time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "01")
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"event":"request_response_trace"`, `"native_correlation":null`,
		`"boundary":"client_ingress"`, `"gateway_write_complete":true`,
	} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("encoded row missing %s: %s", field, encoded)
		}
	}
	var decoded TraceV1
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("round-tripped trace is invalid: %v", err)
	}
}

func TestTraceV1RejectsBodyLengthMismatchAndInvalidCorrelation(t *testing.T) {
	trace := validTrace(t, time.Now().UTC(), "02")
	trace.Request.ObservedBytes++
	if err := trace.Validate(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected body length error, got %v", err)
	}

	trace = validTrace(t, time.Now().UTC(), "03")
	bad := "raw-session-id"
	trace.NativeCorrelation = &NativeCorrelationV1{
		Scheme: "hmac-sha256-v1", KeyID: "0123456789abcdef", Session: &bad,
	}
	if err := trace.Validate(); err == nil || !strings.Contains(err.Error(), "correlation values") {
		t.Fatalf("expected correlation error, got %v", err)
	}
}

func TestTraceV1DefersRawBodyEncodingUntilMarshal(t *testing.T) {
	trace := validTrace(t, time.Now().UTC(), "04")
	request := []byte(`{"model":"deferred"}`)
	trace.Request.BodyBase64 = ""
	trace.Request.RawBody = request
	trace.Request.ObservedBytes = int64(len(request))
	if err := trace.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"body_base64":"`+base64.StdEncoding.EncodeToString(request)+`"`) || strings.Contains(string(encoded), "RawBody") {
		t.Fatalf("raw body was not encoded safely: %s", encoded)
	}
}

func validTrace(t *testing.T, completed time.Time, suffix string) TraceV1 {
	t.Helper()
	request := []byte(`{"model":"example"}`)
	response := []byte(`{"id":"message_example"}`)
	status := 200
	route := "baseten"
	provider := "baseten"
	requestedModel := "claude-example"
	servedModel := "provider/model"
	eventID := strings.Repeat("0", 32-len(suffix)) + suffix
	trace := TraceV1{
		SchemaVersion:     SchemaVersionV1,
		Event:             EventTraceV1,
		EventID:           eventID,
		StartedAt:         completed.Add(-time.Second),
		CompletedAt:       completed,
		Client:            "claude-code",
		ProtocolShape:     ProtocolAnthropic,
		APIKind:           APIKindMessages,
		Endpoint:          "/v1/messages",
		ConfiguredRoute:   &route,
		EffectiveProvider: &provider,
		RequestedModel:    &requestedModel,
		ServedModel:       &servedModel,
		Status:            &status,
		TerminationReason: TerminationCompleted,
		OutcomeSource:     OutcomeSourceProvider,
		ProviderOutcome:   ProviderOutcomeCompleted,
		Fallback:          FallbackV1{PrimaryAttempted: true},
		Request: BodyV1{
			Boundary: "client_ingress", ContentType: "application/json",
			BodyEncoding: "base64", BodyBase64: base64.StdEncoding.EncodeToString(request),
			ObservedBytes: int64(len(request)), CaptureState: CaptureStateCaptured,
		},
		Response: ResponseBodyV1{
			BodyV1: BodyV1{
				Boundary: "gateway_egress", ContentType: "application/json",
				BodyEncoding: "base64", BodyBase64: base64.StdEncoding.EncodeToString(response),
				ObservedBytes: int64(len(response)), CaptureState: CaptureStateCaptured,
			},
			GatewayWriteComplete: true, ProtocolComplete: true,
		},
	}
	if err := trace.Validate(); err != nil {
		t.Fatalf("test trace invalid: %v", err)
	}
	return trace
}
