package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/responsescompat"
)

func newResponsesCompatibilityGateway(
	t *testing.T,
	cfg Config,
	rc resolvedClientConfig,
) (*Gateway, net.Listener, []net.Listener) {
	t.Helper()
	return newGateway(t, cfg, rc)
}

func responsesCompatibilityDefaults(t *testing.T) config.ResolvedResponsesCompatibility {
	t.Helper()
	resolved, err := config.ResolveResponsesCompatibility(
		&config.ResponsesCompatibility{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func responsesCompatibilityOnlyStream(t *testing.T) config.ResolvedResponsesCompatibility {
	t.Helper()
	resolved, err := config.ResolveResponsesCompatibility(
		&config.ResponsesCompatibility{
			TextFormatDefault:            config.ResponsesCompatibilityModeOff,
			AdditionalToolsInput:         config.ResponsesCompatibilityModeOff,
			ReasoningEffort:              config.ResponsesCompatibilityModeOff,
			FunctionArgumentsConsistency: config.ResponsesCompatibilityModeOn,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func sendResponsesRequest(
	t *testing.T,
	g *Gateway,
	body []byte,
) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, "codex", "/v1/responses"),
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Clone(), got
}

func writeCompatibilityError(w http.ResponseWriter, message string) {
	body, _ := json.Marshal(map[string]any{
		"message": message,
		"type":    "Bad Request",
		"code":    400,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(body)
}

func TestResponsesCompatibilityNormalizesRequestProactively(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	}))
	defer srv.Close()

	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedResponsesBaseten(t)
	rc.ResponsesCompatibility = responsesCompatibilityDefaults(t)
	rc.ResponsesCompatibility.AdditionalToolsInput =
		config.ResponsesCompatibilityModeOn
	g, adminL, _ := newResponsesCompatibilityGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	status, _, _ := sendResponsesRequest(t, g, []byte(`{
		"model":"public-model",
		"text":{"verbosity":"low"},
		"reasoning":{"effort":"medium"},
		"input":[{"type":"additional_tools","tools":[{"type":"function","name":"lookup"}]}]
	}`))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var got map[string]any
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatal(err)
	}
	if got["text"].(map[string]any)["format"].(map[string]any)["type"] != "text" {
		t.Fatalf("text was not normalized: %s", gotBody)
	}
	if got["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatalf("reasoning effort was not normalized: %s", gotBody)
	}
	if len(got["tools"].([]any)) != 1 || len(got["input"].([]any)) != 0 {
		t.Fatalf("additional tools were not hoisted: %s", gotBody)
	}
}

func TestResponsesCompatibilitySSEGuardFreshnessAndTelemetry(t *testing.T) {
	correctArgs := `{"x":1}`
	correct := append(
		gatewaySSEEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "call-1", "delta": correctArgs,
		}),
		gatewaySSEEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "call-1", "arguments": correctArgs,
		})...,
	)
	correct = append(correct, gatewaySSEEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done",
		"item": map[string]any{
			"type": "function_call", "id": "call-1", "arguments": correctArgs,
		},
	})...)
	correct = append(correct, gatewayCompletedEvent("call-1", correctArgs)...)

	lateArgs := `{"fresh":true}`
	premature := append(
		gatewaySSEEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "call-2", "delta": "",
		}),
		gatewaySSEEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "call-2", "arguments": "",
		})...,
	)
	premature = append(premature, gatewaySSEEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done",
		"item": map[string]any{
			"type": "function_call", "id": "call-2", "arguments": "",
		},
	})...)
	premature = append(premature, gatewaySSEEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "call-2", "delta": lateArgs,
	})...)
	premature = append(premature, gatewayCompletedEvent("call-2", lateArgs)...)

	var call atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream := correct
		if call.Add(1) == 2 {
			stream = premature
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("ETag", `"stale"`)
		w.Header().Set("Content-MD5", "stale")
		w.Header().Set("Digest", "sha-256=stale")
		w.Header().Set("Content-Digest", "sha-256=:stale:")
		w.Header().Set("Repr-Digest", "sha-256=:stale:")
		w.Header().Set("Content-Length", strconv.Itoa(len(stream)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(stream)
	}))
	defer srv.Close()

	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedResponsesBaseten(t)
	rc.ResponsesCompatibility = responsesCompatibilityOnlyStream(t)
	g, adminL, _ := newResponsesCompatibilityGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	request := []byte(`{"model":"gpt-5","stream":true,"input":"hi"}`)
	status, header, first := sendResponsesRequest(t, g, request)
	if status != http.StatusOK || !bytes.Equal(first, correct) {
		t.Fatalf("correct stream changed: status=%d\n got=%q\nwant=%q", status, first, correct)
	}
	for _, name := range []string{
		"Content-Encoding",
		"Content-Length",
		"Content-MD5",
		"Digest",
		"Content-Digest",
		"Repr-Digest",
		"ETag",
	} {
		if header.Get(name) != "" {
			t.Fatalf("guarded response retained stale %s=%q", name, header.Get(name))
		}
	}

	status, _, second := sendResponsesRequest(t, g, request)
	if status != http.StatusOK {
		t.Fatalf("repaired stream status = %d", status)
	}
	if strings.Count(string(second), `\"fresh\":true`) < 2 {
		t.Fatalf("repaired stream missing final arguments twice: %s", second)
	}
	lateAt := bytes.Index(second, []byte(`"delta":"{\"fresh\":true}"`))
	doneAt := bytes.Index(second, []byte("response.function_call_arguments.done"))
	completedAt := bytes.Index(second, []byte("response.completed"))
	if !(lateAt >= 0 && lateAt < doneAt && doneAt < completedAt) {
		t.Fatalf("repaired stream ordering invalid: late=%d done=%d completed=%d\n%s",
			lateAt, doneAt, completedAt, second)
	}

	rows := waitForRows(t, cfg.TelemetryDir, 2, 2*time.Second)
	firstSummary := rows[0].ResponsesCompatibility
	if firstSummary == nil || firstSummary.RepairedEvents != 0 ||
		firstSummary.ValidationErrors != 0 ||
		!containsRule(firstSummary.Considered, responsescompat.RuleFunctionArgumentsConsistency) {
		t.Fatalf("correct stream summary = %+v", firstSummary)
	}
	secondSummary := rows[1].ResponsesCompatibility
	if secondSummary == nil || secondSummary.RepairedEvents != 2 ||
		secondSummary.ValidationErrors != 0 ||
		!containsRule(secondSummary.Applied, responsescompat.RuleFunctionArgumentsConsistency) {
		t.Fatalf("repaired stream summary = %+v", secondSummary)
	}
	if rows[0].ToolCalls != 1 || rows[1].ToolCalls != 1 ||
		valueOrZero(rows[0].ProviderStopReason) != "completed" ||
		valueOrZero(rows[1].ProviderStopReason) != "completed" {
		t.Fatalf("stream usage metadata rows = %+v", rows)
	}
}

func TestResponsesCompatibilityGuardCancellationFlushesState(t *testing.T) {
	delta := gatewaySSEEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "call-1", "delta": "",
	})
	done := gatewaySSEEvent("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": "call-1", "arguments": "",
	})
	body := newCancellationReadCloser(append(append([]byte(nil), delta...), done...))
	writer := newCancellationResponseWriter()
	guard := responsescompat.NewSSEGuard(responsescompat.StreamLimits{})
	compatibility := &responsesCompatibilityRequest{}
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan responsesGuardedRelayResult, 1)
	go func() {
		resultCh <- relayResponsesSSE(
			ctx,
			body,
			writer,
			writer,
			guard,
			compatibility,
		)
	}()

	select {
	case <-writer.wrote:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("guarded relay did not emit the unheld delta")
	}
	result := <-resultCh
	if result.relayErr != context.Canceled || result.responseComplete {
		t.Fatalf("relay result = %+v", result)
	}
	if got := writer.bytes(); !bytes.Equal(got, delta) {
		t.Fatalf("cancellation relayed held terminal\n got: %q\nwant: %q", got, delta)
	}
	if compatibility.summary.ValidationErrors != 1 ||
		compatibility.summary.RepairedEvents != 0 {
		t.Fatalf("compatibility summary = %+v", compatibility.summary)
	}
	if flushed, flushResult := guard.Flush(); len(flushed) != 0 ||
		flushResult != (responsescompat.StreamResult{}) {
		t.Fatalf("cancellation leaked state: flushed=%q result=%#v", flushed, flushResult)
	}

	complete := gatewaySSEEvent("response.completed", map[string]any{
		"type": "response.completed", "response": map[string]any{"id": "resp-2"},
	})
	got, pushResult := guard.Push(complete)
	if !bytes.Equal(got, complete) || pushResult != (responsescompat.StreamResult{}) {
		t.Fatalf("state leaked into next stream: got=%q result=%#v", got, pushResult)
	}
}

type cancellationReadCloser struct {
	first     []byte
	firstRead bool
	closed    chan struct{}
	closeOnce sync.Once
}

func newCancellationReadCloser(first []byte) *cancellationReadCloser {
	return &cancellationReadCloser{
		first:  first,
		closed: make(chan struct{}),
	}
}

func (body *cancellationReadCloser) Read(p []byte) (int, error) {
	if !body.firstRead {
		body.firstRead = true
		return copy(p, body.first), nil
	}
	<-body.closed
	return 0, context.Canceled
}

func (body *cancellationReadCloser) Close() error {
	body.closeOnce.Do(func() {
		close(body.closed)
	})
	return nil
}

type cancellationResponseWriter struct {
	header    http.Header
	body      bytes.Buffer
	mu        sync.Mutex
	wrote     chan struct{}
	wroteOnce sync.Once
}

func newCancellationResponseWriter() *cancellationResponseWriter {
	return &cancellationResponseWriter{
		header: make(http.Header),
		wrote:  make(chan struct{}),
	}
}

func (writer *cancellationResponseWriter) Header() http.Header {
	return writer.header
}

func (writer *cancellationResponseWriter) WriteHeader(int) {}

func (writer *cancellationResponseWriter) Write(p []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	n, err := writer.body.Write(p)
	writer.wroteOnce.Do(func() {
		close(writer.wrote)
	})
	return n, err
}

func (writer *cancellationResponseWriter) Flush() {}

func (writer *cancellationResponseWriter) bytes() []byte {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]byte(nil), writer.body.Bytes()...)
}

func containsRule(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func gatewaySSEEvent(eventType string, data map[string]any) []byte {
	payload, _ := json.Marshal(data)
	return []byte("event: " + eventType + "\ndata: " + string(payload) + "\n\n")
}

func gatewayCompletedEvent(callID, arguments string) []byte {
	return gatewaySSEEvent("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"model":  "public-model",
			"status": "completed",
			"output": []any{map[string]any{
				"type": "function_call", "id": callID, "arguments": arguments,
			}},
			"usage": map[string]any{"input_tokens": 9, "output_tokens": 2},
		},
	})
}

func TestResponsesCompatibilityUnexpectedEncodingSkipsGuard(t *testing.T) {
	const encoded = "opaque-encoded-sse"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "future-codec")
		w.Header().Set("ETag", `"encoded"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(encoded)))
		_, _ = w.Write([]byte(encoded))
	}))
	defer srv.Close()

	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedResponsesBaseten(t)
	rc.ResponsesCompatibility = responsesCompatibilityOnlyStream(t)
	g, adminL, _ := newResponsesCompatibilityGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	status, header, body := sendResponsesRequest(
		t,
		g,
		[]byte(`{"model":"gpt-5","stream":true,"input":"hi"}`),
	)
	if status != http.StatusOK || string(body) != encoded ||
		header.Get("Content-Encoding") != "future-codec" ||
		header.Get("ETag") != `"encoded"` {
		t.Fatalf("encoded stream changed: status=%d headers=%v body=%q", status, header, body)
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.ResponsesCompatibility == nil ||
		row.ResponsesCompatibility.ValidationErrors != 1 {
		t.Fatalf("encoded stream summary = %+v", row.ResponsesCompatibility)
	}
}

func TestResponsesGuardedTelemetryCaptureIsBoundedAndIncremental(t *testing.T) {
	tail := newResponsesCappedTail(8)
	tail.Write([]byte("abcdef"))
	tail.Write([]byte("ghijkl"))
	if got := string(tail.Bytes()); got != "efghijkl" {
		t.Fatalf("rolling tail = %q, want efghijkl", got)
	}
	tail.Write([]byte("0123456789"))
	if got := string(tail.Bytes()); got != "23456789" {
		t.Fatalf("oversized rolling tail = %q, want 23456789", got)
	}

	var detector responsesOutputDeltaDetector
	line := gatewaySSEEvent("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "delta": "hello",
	})
	split := bytes.Index(line, []byte(`"delta"`))
	if split <= 0 {
		t.Fatalf("test event has no split point: %s", line)
	}
	if detector.Feed(line[:split]) {
		t.Fatal("partial event reported output")
	}
	if !detector.Feed(line[split:]) {
		t.Fatal("completed split event did not report output")
	}
}
