package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/requestclassification"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

func TestAutoPermissionCheckRoutesOnceToAnthropicUnchanged(t *testing.T) {
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		basetenHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer baseten.Close()

	gotBody := make(chan []byte, 1)
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_auto","type":"message","role":"assistant","content":[{"type":"text","text":"allow"}],"model":"claude-example-model","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer anthropic.Close()

	cfg := testConfig(t, baseten.URL, anthropic.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.SanitizeHistory = true
	rc.FallbackRoute = "anthropic"
	rc.ModelAliases = map[string]string{
		"claude-baseten-example": "example/model",
		"claude-baseten-worker":  "example/worker",
	}
	rc.ModelRoutes = map[string]string{"sonnet": "claude-baseten-example"}
	rc.SubagentModel = "claude-baseten-worker"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	original := syntheticAutoPermissionBody("claude-baseten-example")
	req, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages?beta=true"),
		bytes.NewReader(original),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(subagentAgentIDHeader, "synthetic-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, responseBody)
	}
	if got := basetenHits.Load(); got != 0 {
		t.Fatalf("Baseten hits = %d, want 0", got)
	}
	select {
	case got := <-gotBody:
		if !bytes.Equal(got, original) {
			t.Fatalf("native body changed\ngot:  %s\nwant: %s", got, original)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Anthropic did not receive the request")
	}

	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].EffectiveProvider != "anthropic" {
		t.Fatalf("effective_provider = %q, want anthropic", rows[0].EffectiveProvider)
	}
	if rows[0].RequestedModel != "claude-baseten-example" ||
		rows[0].ServedModel != "claude-baseten-example" {
		t.Fatalf(
			"models = requested %q served %q, want preserved alias",
			rows[0].RequestedModel,
			rows[0].ServedModel,
		)
	}
	if rows[0].Sanitized {
		t.Fatal("sanitized = true, want false")
	}
	if rows[0].Fallback.Attempted || rows[0].Fallback.Count != 0 {
		t.Fatalf("fallback = %+v, want no fallback", rows[0].Fallback)
	}
	classification := rows[0].RequestClassification
	if classification == nil ||
		classification.Kind != requestclassification.KindClaudeAutoPermissionCheck ||
		classification.Detector != requestclassification.DetectorClaudeAutoV1 ||
		classification.RoutingAction != requestclassification.RoutingActionNativeAnthropic {
		t.Fatalf("request_classification = %+v", classification)
	}
}

func TestAutoPermissionCheckFailureIsTerminalAndDoesNotTripCooldown(t *testing.T) {
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		basetenHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_normal","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"example/model","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer baseten.Close()

	var anthropicHits atomic.Int32
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		anthropicHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"retry later"}}`))
	}))
	defer anthropic.Close()

	cfg := testConfig(t, baseten.URL, anthropic.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, body := postMessagesBody(t, g, syntheticAutoPermissionBody("claude-example-model"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Auto status = %d, body = %s", resp.StatusCode, body)
	}
	if got := anthropicHits.Load(); got != 1 {
		t.Fatalf("Anthropic hits = %d, want 1", got)
	}
	if got := basetenHits.Load(); got != 0 {
		t.Fatalf("Baseten hits after Auto failure = %d, want 0", got)
	}
	if _, active := g.fallbackDeadline("claude-code"); active {
		t.Fatal("Auto failure activated fallback cooldown")
	}

	normal := []byte(`{"model":"claude-example-model","stream":false,"system":[{"type":"text","text":"You are a coding assistant."}],"messages":[{"role":"user","content":"hello"}]}`)
	resp, body = postMessagesBody(t, g, normal)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ordinary status = %d, body = %s", resp.StatusCode, body)
	}
	if got := basetenHits.Load(); got != 1 {
		t.Fatalf("Baseten hits after ordinary request = %d, want 1", got)
	}
	if got := anthropicHits.Load(); got != 1 {
		t.Fatalf("Anthropic hits after ordinary request = %d, want 1", got)
	}
}

func TestAutoPermissionCheckHTTPFailuresNeverReachBaseten(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var basetenHits atomic.Int32
			baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				basetenHits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer baseten.Close()

			var anthropicHits atomic.Int32
			anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				anthropicHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"synthetic_error","message":"request failed"}}`))
			}))
			defer anthropic.Close()

			cfg := testConfig(t, baseten.URL, anthropic.URL)
			rc := resolvedAnthropicBaseten(t)
			rc.FallbackRoute = "anthropic"
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			resp, body := postMessagesBody(t, g, syntheticAutoPermissionBody("claude-example-model"))
			if resp.StatusCode != status {
				t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, status, body)
			}
			if got := anthropicHits.Load(); got != 1 {
				t.Fatalf("Anthropic hits = %d, want 1", got)
			}
			if got := basetenHits.Load(); got != 0 {
				t.Fatalf("Baseten hits = %d, want 0", got)
			}
			if _, active := g.fallbackDeadline("claude-code"); active {
				t.Fatal("Auto HTTP failure activated fallback cooldown")
			}
			rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
			row := rows[0]
			if row.StatusCode() != status {
				t.Fatalf("telemetry status = %d, want %d", row.StatusCode(), status)
			}
			if row.Fallback.Attempted || row.Fallback.Count != 0 || row.Fallback.Trigger != nil {
				t.Fatalf("fallback = %+v, want empty", row.Fallback)
			}
			assertAutoClassification(t, row.RequestClassification)
		})
	}
}

func TestAutoPermissionCheckForwardsNativeAnthropicAuthentication(t *testing.T) {
	tests := []struct {
		name        string
		header      string
		value       string
		otherHeader string
	}{
		{
			name:        "OAuth-style bearer",
			header:      "Authorization",
			value:       "Bearer synthetic-claude-oauth-token",
			otherHeader: "X-Api-Key",
		},
		{
			name:        "native API key",
			header:      "X-Api-Key",
			value:       "synthetic-anthropic-api-key",
			otherHeader: "Authorization",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var basetenHits atomic.Int32
			baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				basetenHits.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer baseten.Close()

			anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get(tc.header); got != tc.value {
					t.Errorf("%s = %q, want %q", tc.header, got, tc.value)
				}
				if got := r.Header.Get(tc.otherHeader); got != "" {
					t.Errorf("unexpected %s = %q", tc.otherHeader, got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg_auto","type":"message","role":"assistant","content":[{"type":"text","text":"allow"}],"model":"claude-example-model","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer anthropic.Close()

			cfg := testConfig(t, baseten.URL, anthropic.URL)
			rc := resolvedAnthropicBaseten(t)
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			req, err := http.NewRequest(
				http.MethodPost,
				clientURL(g, "claude-code", "/v1/messages"),
				bytes.NewReader(syntheticAutoPermissionBody("claude-example-model")),
			)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(tc.header, tc.value)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			if got := basetenHits.Load(); got != 0 {
				t.Fatalf("Baseten hits = %d, want 0", got)
			}
		})
	}
}

// A gateway-only bearer is wire-indistinguishable from a native bearer, so
// Switch cannot identify it before Anthropic rejects it. The rejection must
// remain a classified terminal response with no Baseten fallback.
func TestAutoPermissionCheckGatewayOnlyBearerCannotBePreidentified(t *testing.T) {
	const bearer = "Bearer synthetic-gateway-only-token"
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		basetenHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer baseten.Close()

	var anthropicHits atomic.Int32
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicHits.Add(1)
		if got := r.Header.Get("Authorization"); got != bearer {
			t.Errorf("Authorization = %q, want exact gateway-only bearer", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid credential"}}`))
	}))
	defer anthropic.Close()

	cfg := testConfig(t, baseten.URL, anthropic.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	req, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(syntheticAutoPermissionBody("claude-example-model")),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if got := anthropicHits.Load(); got != 1 {
		t.Fatalf("Anthropic hits = %d, want 1", got)
	}
	if got := basetenHits.Load(); got != 0 {
		t.Fatalf("Baseten hits = %d, want 0", got)
	}
	if _, active := g.fallbackDeadline("claude-code"); active {
		t.Fatal("gateway-only bearer rejection activated fallback cooldown")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	row := rows[0]
	if row.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("telemetry status = %d, want 401", row.StatusCode())
	}
	if row.Fallback.Attempted || row.Fallback.Count != 0 || row.Fallback.Trigger != nil {
		t.Fatalf("fallback = %+v, want empty", row.Fallback)
	}
	assertAutoClassification(t, row.RequestClassification)
}

func TestAutoPermissionCheckMonitorStaysLocal(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, upstream.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.Route = "monitor"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, body := postMessagesBody(t, g, syntheticAutoPermissionBody("claude-example-model"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("monitor status = %d, body = %s", resp.StatusCode, body)
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream hits = %d, want 0", got)
	}
}

func TestAutoPermissionCheckTTFTIsTerminalWithoutFallbackMetadata(t *testing.T) {
	var anthropicHits atomic.Int32
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer anthropic.Close()

	cfg := testConfig(t, anthropic.URL, anthropic.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	rc.TTFTTimeout = 40 * time.Millisecond
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, body := postMessagesBody(t, g, syntheticAutoPermissionBody("claude-example-model"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if got := anthropicHits.Load(); got != 1 {
		t.Fatalf("Anthropic hits = %d, want 1", got)
	}
	if _, active := g.fallbackDeadline("claude-code"); active {
		t.Fatal("Auto TTFT activated fallback cooldown")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	row := rows[0]
	if row.StatusCode() != http.StatusGatewayTimeout || !row.IsHTTPError() {
		t.Fatalf("telemetry status/error = %d/%v", row.StatusCode(), row.IsHTTPError())
	}
	if row.Fallback.Attempted || row.Fallback.Count != 0 || row.Fallback.Trigger != nil {
		t.Fatalf("fallback = %+v, want empty", row.Fallback)
	}
	assertAutoClassification(t, row.RequestClassification)
}

func TestAutoPermissionCheckTransportFailureRetainsClassification(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("classified request reached Baseten")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer baseten.Close()

	cfg := testConfig(t, baseten.URL, closedURL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "baseten"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, body := postMessagesBody(t, g, syntheticAutoPermissionBody("claude-example-model"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if _, active := g.fallbackDeadline("claude-code"); active {
		t.Fatal("Auto transport failure activated fallback cooldown")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	row := rows[0]
	if row.Fallback.Attempted || row.Fallback.Count != 0 || row.Fallback.Trigger != nil {
		t.Fatalf("fallback = %+v, want empty", row.Fallback)
	}
	assertAutoClassification(t, row.RequestClassification)
}

func TestResolveAutoPermissionCheckBypassesRoutingLadder(t *testing.T) {
	g := &Gateway{
		cfg: Config{
			AnthropicURL: "https://anthropic.example.invalid",
			BasetenURL:   "https://baseten.example.invalid",
		},
		client:  http.DefaultClient,
		pricing: pricing.New(),
	}
	rc := resolvedClientConfig{
		Name:                 "claude-code",
		ProtocolShape:        "anthropic",
		Route:                "baseten",
		HasGlobalRoutingGate: true,
		GlobalRoutingEnabled: true,
		DefaultModel:         "example/default",
		ModelAliases:         map[string]string{"claude-baseten-example": "example/alias"},
		ModelRoutes:          map[string]string{"sonnet": "claude-baseten-example"},
		SubagentModel:        "example/worker",
		FallbackRoute:        "openai",
	}
	cl := &clientListener{cfg: rc}
	classification := &requestclassification.Classification{
		Kind:          requestclassification.KindClaudeAutoPermissionCheck,
		Detector:      requestclassification.DetectorClaudeAutoV1,
		RoutingAction: requestclassification.RoutingActionNativeAnthropic,
	}

	for _, model := range []string{
		"claude-sonnet-example",
		"claude-baseten-example",
		"example/raw-slug",
	} {
		t.Run(model, func(t *testing.T) {
			body := syntheticAutoPermissionBody(model)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			req.Header.Set(subagentAgentIDHeader, "synthetic-agent")
			requested, observed := inspectRequestedReasoning("messages", body)
			attempts, err := g.resolveAttemptsWithRequestedReasoning(
				cl,
				req,
				body,
				"messages",
				requested,
				observed,
				classification,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(attempts) != 1 {
				t.Fatalf("attempt count = %d, want 1", len(attempts))
			}
			at := attempts[0]
			if at.route != "anthropic" || at.res.RequestedModel != model || at.res.UpstreamModel != model {
				t.Fatalf("attempt route/models = %q/%q/%q", at.route, at.res.RequestedModel, at.res.UpstreamModel)
			}
			if !bytes.Equal(at.res.NewBody, body) {
				t.Fatalf("attempt body changed\ngot:  %s\nwant: %s", at.res.NewBody, body)
			}
			if at.fallbackCount != 0 || at.fallbackTrigger != "" || at.primary != nil {
				t.Fatalf("attempt has fallback state: %+v", at)
			}
			if at.requestClassification != classification {
				t.Fatal("classification was not carried on the native attempt")
			}
		})
	}

	t.Run("global routing off", func(t *testing.T) {
		cl.cfg.GlobalRoutingEnabled = false
		body := syntheticAutoPermissionBody("claude-example-model")
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		requested, observed := inspectRequestedReasoning("messages", body)
		attempts, err := g.resolveAttemptsWithRequestedReasoning(
			cl,
			req,
			body,
			"messages",
			requested,
			observed,
			classification,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(attempts) != 1 || attempts[0].route != "anthropic" {
			t.Fatalf("attempts = %+v, want one Anthropic attempt", attempts)
		}
	})
}

func syntheticAutoPermissionBody(model string) []byte {
	return []byte(`{"model":"` + model + `", "system":[{"type":"text","text":"In Auto mode, classify whether a proposed tool call has permission to run safely."}], "messages":[{"role":"assistant","content":[{"type":"text","text":""},{"type":"tool_use","id":"tools.Example:0","name":"Example","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tools.Example:0","content":"ok"}]}]}`)
}

func postMessagesBody(t *testing.T, g *Gateway, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, responseBody
}

func assertAutoClassification(
	t *testing.T,
	classification *telemetry.RequestClassificationV1,
) {
	t.Helper()
	if classification == nil ||
		classification.Kind != requestclassification.KindClaudeAutoPermissionCheck ||
		classification.Detector != requestclassification.DetectorClaudeAutoV1 ||
		classification.RoutingAction != requestclassification.RoutingActionNativeAnthropic {
		t.Fatalf("request_classification = %+v", classification)
	}
}
