package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

func TestStartUpstreamSubAttemptDispatchSignal(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Run("expired TTFT budget", func(t *testing.T) {
		at := upstreamAttempt{url: server.URL, client: http.DefaultClient}
		resp, err, expired, dispatched, watch := startUpstreamSubAttempt(
			context.Background(), at, nil, time.Now().Add(-time.Second),
		)
		watch.cancel()
		if resp != nil || err != nil || !expired || dispatched {
			t.Fatalf("resp=%v err=%v expired=%v dispatched=%v", resp, err, expired, dispatched)
		}
		if hits.Load() != 0 {
			t.Fatalf("upstream hits = %d, want 0", hits.Load())
		}
	})

	t.Run("request build failure", func(t *testing.T) {
		at := upstreamAttempt{url: "://invalid", client: http.DefaultClient}
		resp, err, expired, dispatched, watch := startUpstreamSubAttempt(
			context.Background(), at, nil, time.Time{},
		)
		watch.cancel()
		if resp != nil || err == nil || expired || dispatched {
			t.Fatalf("resp=%v err=%v expired=%v dispatched=%v", resp, err, expired, dispatched)
		}
	})

	t.Run("transport starts", func(t *testing.T) {
		closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := closed.URL
		closed.Close()
		at := upstreamAttempt{url: url, client: http.DefaultClient}
		resp, err, expired, dispatched, watch := startUpstreamSubAttempt(
			context.Background(), at, nil, time.Time{},
		)
		watch.cancel()
		if resp != nil || err == nil || expired || !dispatched {
			t.Fatalf("resp=%v err=%v expired=%v dispatched=%v", resp, err, expired, dispatched)
		}
	})
}

func TestPrimaryTransportFallbackRecordsDispatch(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	primaryURL := primary.URL
	primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primaryURL, fallback.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	resp, body := ttftPost(t, g, `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"ping"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "FALLBACK") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.Primary == nil ||
		row.Primary.Provider != "baseten" ||
		row.Primary.Model != "zai-org/GLM-5.2" ||
		!row.Primary.Attempted ||
		row.Primary.Outcome != telemetry.PrimaryOutcomeTransportError ||
		row.Primary.Status != nil {
		t.Fatalf("primary = %+v, want dispatched Baseten transport error", row.Primary)
	}
}

func TestPrimaryRequestBuildFallbackRecordsNoDispatch(t *testing.T) {
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fallback.Close()

	cfg := testConfig(t, "://invalid", fallback.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	resp, body := ttftPost(t, g, `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"ping"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "FALLBACK") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.Primary == nil ||
		row.Primary.Provider != "baseten" ||
		row.Primary.Model != "zai-org/GLM-5.2" ||
		row.Primary.Attempted ||
		row.Primary.Outcome != telemetry.PrimaryOutcomeRequestBuildError ||
		row.Primary.Status != nil {
		t.Fatalf("primary = %+v, want unattempted Baseten request build error", row.Primary)
	}
	if valueOrZero(row.Fallback.Trigger) != fallbackTriggerRequestBuild {
		t.Fatalf("fallback trigger = %q, want %q", valueOrZero(row.Fallback.Trigger), fallbackTriggerRequestBuild)
	}
}

func TestOpenAIHTTPFallbackRecordsPrimary(t *testing.T) {
	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer baseten.Close()
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","model":"gpt-5","output":[]}`)
	}))
	defer openai.Close()

	cfg := testConfig(t, baseten.URL, baseten.URL)
	cfg.OpenAIURL = openai.URL
	rc := resolvedOpenAIBaseten(t, "codex", "baseten")
	rc.DefaultModel = "moonshotai/Kimi-K2.7-Code"
	rc.FallbackRoute = "openai"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	req, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, "codex", "/v1/responses"),
		bytes.NewBufferString(`{"model":"gpt-5","input":"ping"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.Primary == nil ||
		row.Primary.Provider != "baseten" ||
		row.Primary.Model != rc.DefaultModel ||
		!row.Primary.Attempted ||
		row.Primary.Outcome != telemetry.PrimaryOutcomeHTTPError ||
		row.Primary.Status == nil || *row.Primary.Status != http.StatusTooManyRequests {
		t.Fatalf("primary = %+v, want Baseten %s HTTP 429", row.Primary, rc.DefaultModel)
	}
}

func TestNonstandardServerErrorFallbackRetainsPrimaryTelemetry(t *testing.T) {
	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(600)
	}))
	defer baseten.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fallback.Close()

	cfg := testConfig(t, baseten.URL, fallback.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	resp, body := ttftPost(t, g, `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"ping"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "FALLBACK") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.Primary == nil ||
		row.Primary.Provider != "baseten" ||
		row.Primary.Model != "zai-org/GLM-5.2" ||
		!row.Primary.Attempted ||
		row.Primary.Outcome != telemetry.PrimaryOutcomeHTTPError ||
		row.Primary.Status == nil || *row.Primary.Status != 600 {
		t.Fatalf("primary = %+v, want Baseten zai-org/GLM-5.2 HTTP 600", row.Primary)
	}
}
