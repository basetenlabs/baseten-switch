package gateway

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/proxy"
	"github.com/basetenlabs/baseten-switch/gateway/internal/requestprofile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
)

func traceTestRuntime(t *testing.T, enabled bool) (*Gateway, Config) {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "gateway.yaml")
	if err := os.WriteFile(configPath, []byte("global: {}\nclients: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := resolveTraceDirectory(configPath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCfg := Config{
		ConfigPath: configPath,
		TraceDir:   dir,
		TraceCapture: config.ResolvedTraceCapture{
			Enabled:       enabled,
			Clients:       []string{"claude-code"},
			RetentionDays: 7,
		},
	}
	g := &Gateway{}
	if err := g.initializeTraceCapture(runtimeCfg); err != nil {
		t.Fatal(err)
	}
	return g, runtimeCfg
}

func TestTraceCaptureRecordsExactClientBoundaryAndRoutingOutcome(t *testing.T) {
	g, runtimeCfg := traceTestRuntime(t, true)
	cl := &clientListener{cfg: resolvedClientConfig{
		Name:          "claude-code",
		ProtocolShape: "anthropic",
		Route:         "baseten",
	}}
	requestBody := []byte("{\n  \"model\": \"claude-example\",\n  \"stream\": true\n}\n")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Add("Content-Encoding", "identity")
	req.Header.Add("Content-Encoding", "gzip")
	req.Header.Set("x-claude-code-session-id", "private-session-id")
	started := time.Now().UTC()
	capture := g.beginTraceCapture(
		cl,
		req,
		started,
		requestBody,
		tracecapture.APIKindMessages,
	)
	if capture == nil {
		t.Fatal("trace capture was not admitted")
	}
	metadataCapture, err := captureTelemetryRequestProfileWithEventIDV1(
		nil,
		started,
		cl.cfg.Name,
		cl.cfg.Route,
		cl.cfg.ProtocolShape,
		"claude-example",
		requestprofile.Profile{},
		capture.eventID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if metadataCapture.eventID != capture.eventID {
		t.Fatalf("telemetry event_id = %q, trace event_id = %q", metadataCapture.eventID, capture.eventID)
	}

	clientWriter := httptest.NewRecorder()
	w := newTraceResponseWriter(clientWriter, capture)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	responseBody := []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if _, err := w.Write(responseBody[:23]); err != nil {
		t.Fatal(err)
	}
	w.(http.Flusher).Flush()
	if _, err := w.Write(responseBody[23:]); err != nil {
		t.Fatal(err)
	}

	status := http.StatusOK
	trigger := "http_500"
	at := upstreamAttempt{
		route:            "anthropic",
		modelForCost:     "claude-served-model",
		traceCapture:     capture,
		fallbackCount:    1,
		fallbackTrigger:  trigger,
		primaryAttempted: true,
		res: proxy.RewriteResult{
			RequestedModel: "claude-example",
			Sanitized:      true,
		},
		primary: &telemetry.PrimaryV1{
			Provider:  "baseten",
			Model:     "provider/primary",
			Attempted: true,
			Outcome:   telemetry.PrimaryOutcomeHTTPError,
		},
	}
	g.recordTelemetryV1(cl, at, telemetryCompletionV1{
		completedAt:      started.Add(time.Second),
		status:           &status,
		isStream:         true,
		responseComplete: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.closeTraceCapture(ctx); err != nil {
		t.Fatal(err)
	}
	traces, err := tracecapture.ReadTraces(runtimeCfg.TraceDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 {
		t.Fatalf("trace count = %d, want 1", len(traces))
	}
	trace := traces[0]
	if got, err := base64.StdEncoding.DecodeString(trace.Request.BodyBase64); err != nil || string(got) != string(requestBody) {
		t.Fatalf("request bytes = %q, %v", got, err)
	}
	if got, err := base64.StdEncoding.DecodeString(trace.Response.BodyBase64); err != nil || string(got) != string(responseBody) {
		t.Fatalf("response bytes = %q, %v", got, err)
	}
	if trace.Request.ContentEncoding != "identity,gzip" {
		t.Fatalf("request content encoding = %q, want %q", trace.Request.ContentEncoding, "identity,gzip")
	}
	if trace.EventID != capture.eventID {
		t.Fatalf("event_id = %q, want %q", trace.EventID, capture.eventID)
	}
	if trace.EffectiveProvider == nil || *trace.EffectiveProvider != "anthropic" ||
		trace.ServedModel == nil || *trace.ServedModel != "claude-served-model" {
		t.Fatalf("served route = provider %v model %v", trace.EffectiveProvider, trace.ServedModel)
	}
	if !trace.Fallback.Attempted || trace.Fallback.Count != 1 ||
		!trace.Fallback.PrimaryAttempted || trace.Fallback.Trigger == nil ||
		*trace.Fallback.Trigger != trigger {
		t.Fatalf("fallback = %+v", trace.Fallback)
	}
	if !trace.Response.GatewayWriteComplete || !trace.Response.ProtocolComplete {
		t.Fatalf("response completion = gateway %t protocol %t", trace.Response.GatewayWriteComplete, trace.Response.ProtocolComplete)
	}
	if trace.NativeCorrelation == nil || trace.NativeCorrelation.Session == nil {
		t.Fatal("expected pseudonymous Claude session correlation")
	}
	if *trace.NativeCorrelation.Session == "private-session-id" {
		t.Fatal("raw native session ID entered trace")
	}
}

func TestTraceCaptureDisabledCreatesNoStoreAndAdmitsNothing(t *testing.T) {
	g, runtimeCfg := traceTestRuntime(t, false)
	cl := &clientListener{cfg: resolvedClientConfig{Name: "claude-code", ProtocolShape: "anthropic"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if capture := g.beginTraceCapture(cl, req, time.Now(), []byte(`{}`), tracecapture.APIKindMessages); capture != nil {
		t.Fatal("disabled capture admitted a request")
	}
	if _, err := os.Stat(runtimeCfg.TraceDir); !os.IsNotExist(err) {
		t.Fatalf("disabled trace store stat error = %v, want not-exist", err)
	}
}

func TestTraceCaptureFinalizesPostAdmissionGatewayError(t *testing.T) {
	g, runtimeCfg := traceTestRuntime(t, true)
	cl := &clientListener{cfg: resolvedClientConfig{
		Name: "claude-code", ProtocolShape: "anthropic", Route: "baseten",
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	capture := g.beginTraceCapture(
		cl,
		req,
		time.Now(),
		[]byte(`{"model":"claude-example"}`),
		tracecapture.APIKindMessages,
	)
	if capture == nil {
		t.Fatal("trace capture was not admitted")
	}
	w := newTraceResponseWriter(httptest.NewRecorder(), capture)
	g.reject(w, http.StatusBadRequest, "synthetic local rejection")
	capture.finalizeDefault()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.closeTraceCapture(ctx); err != nil {
		t.Fatal(err)
	}
	traces, err := tracecapture.ReadTraces(runtimeCfg.TraceDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 {
		t.Fatalf("trace count = %d, want 1", len(traces))
	}
	trace := traces[0]
	if trace.OutcomeSource != tracecapture.OutcomeSourceGateway ||
		trace.TerminationReason != tracecapture.TerminationGatewayError ||
		trace.EffectiveProvider != nil || trace.ServedModel != nil {
		t.Fatalf("gateway outcome = source %q termination %q provider %v model %v",
			trace.OutcomeSource,
			trace.TerminationReason,
			trace.EffectiveProvider,
			trace.ServedModel,
		)
	}
	if trace.Status == nil || *trace.Status != http.StatusBadRequest ||
		!trace.Response.GatewayWriteComplete || trace.Response.ProtocolComplete {
		t.Fatalf("response = status %v gateway_complete %t protocol_complete %t",
			trace.Status,
			trace.Response.GatewayWriteComplete,
			trace.Response.ProtocolComplete,
		)
	}
}

func TestTraceCapturePolicyGenerationInvalidatesInFlightRequest(t *testing.T) {
	g, runtimeCfg := traceTestRuntime(t, true)
	cl := &clientListener{cfg: resolvedClientConfig{Name: "claude-code", ProtocolShape: "anthropic"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	capture := g.beginTraceCapture(cl, req, time.Now(), []byte(`{"model":"x"}`), tracecapture.APIKindMessages)
	if capture == nil {
		t.Fatal("trace capture was not admitted")
	}
	disabled := runtimeCfg
	disabled.TraceCapture.Enabled = false
	g.reconcileTraceCapture(disabled)
	capture.finalizeDefault()

	traces, err := tracecapture.ReadTraces(runtimeCfg.TraceDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 0 {
		t.Fatalf("trace count = %d, want 0 after consent generation changed", len(traces))
	}
}
