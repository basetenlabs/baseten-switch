package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/reasoning"
)

func TestFallbackStatusPolicyBoundaries(t *testing.T) {
	policy := config.ResolvedFallbackPolicy{OnBaseten429: true, OnBaseten5xx: true}
	for _, tc := range []struct {
		status int
		mask   fallbackTriggerMask
		on     bool
	}{
		{http.StatusTooManyRequests, fallbackAllowHTTP429, true},
		{499, 0, false},
		{500, fallbackAllowHTTP5xx, true},
		{599, fallbackAllowHTTP5xx, true},
		{600, 0, false},
	} {
		if got := fallbackStatusMask(tc.status); got != tc.mask {
			t.Errorf("status %d mask = %d, want %d", tc.status, got, tc.mask)
		}
		if got := statusFallbackEnabled(policy, tc.status); got != tc.on {
			t.Errorf("status %d enabled = %t, want %t", tc.status, got, tc.on)
		}
	}
	policy.OnBaseten429 = false
	if statusFallbackEnabled(policy, http.StatusTooManyRequests) {
		t.Fatal("429 remained enabled after its independent policy was disabled")
	}
	if !statusFallbackEnabled(policy, http.StatusInternalServerError) {
		t.Fatal("disabling 429 changed the independent 5xx policy")
	}
	deadline := time.Now().Add(time.Minute)
	if cooldownEligible(policy, fallbackAllowStatus, fallbackCooldownState{
		Until: deadline, Trigger: fallbackCooldownHTTP429,
	}) {
		t.Fatal("disabled 429 policy still honored a 429-origin cooldown")
	}
	if !cooldownEligible(policy, fallbackAllowStatus, fallbackCooldownState{
		Until: deadline, Trigger: fallbackCooldownHTTP5xx,
	}) {
		t.Fatal("disabling 429 invalidated an unrelated 5xx-origin cooldown")
	}
	if cooldownEligible(policy, fallbackAllowStatus, fallbackCooldownState{
		Until: deadline, Trigger: "transport_error",
	}) {
		t.Fatal("status-only candidate honored a transport-origin cooldown")
	}
}

func TestNativeOriginStatusPolicyMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		policy     config.ResolvedFallbackPolicy
		fallback   bool
		suppressed string
	}{
		{
			name:     "429_on",
			status:   http.StatusTooManyRequests,
			policy:   config.ResolvedFallbackPolicy{OnBaseten429: true, OnBaseten5xx: false},
			fallback: true,
		},
		{
			name:       "429_off",
			status:     http.StatusTooManyRequests,
			policy:     config.ResolvedFallbackPolicy{OnBaseten429: false, OnBaseten5xx: true},
			suppressed: "policy_disabled_http_429",
		},
		{
			name:     "503_on",
			status:   http.StatusServiceUnavailable,
			policy:   config.ResolvedFallbackPolicy{OnBaseten429: false, OnBaseten5xx: true},
			fallback: true,
		},
		{
			name:       "503_off",
			status:     http.StatusServiceUnavailable,
			policy:     config.ResolvedFallbackPolicy{OnBaseten429: true, OnBaseten5xx: false},
			suppressed: "policy_disabled_http_5xx",
		},
		{
			name:   "499_is_not_429",
			status: 499,
			policy: config.ResolvedFallbackPolicy{OnBaseten429: true, OnBaseten5xx: true},
		},
		{
			name:   "600_is_not_5xx",
			status: 600,
			policy: config.ResolvedFallbackPolicy{OnBaseten429: true, OnBaseten5xx: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"message":"primary"}}`)
			}))
			defer primary.Close()
			var fallbackHits atomic.Int32
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_native","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer fallback.Close()

			cfg := testConfig(t, primary.URL, fallback.URL)
			rc := resolvedAnthropicBaseten(t)
			rc.FallbackRoute = "anthropic"
			rc.HasFallbackPolicy = true
			rc.FallbackPolicy = tc.policy
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			resp, body := postMessages(t, g, "claude-sonnet-5")
			if tc.fallback {
				if resp.StatusCode != http.StatusOK || fallbackHits.Load() != 1 {
					t.Fatalf("status/hits = %d/%d, body=%s, want 200/1", resp.StatusCode, fallbackHits.Load(), body)
				}
				if !g.fallbackActive("claude-code") {
					t.Fatal("eligible status did not create a cooldown")
				}
			} else {
				if resp.StatusCode != tc.status || fallbackHits.Load() != 0 {
					t.Fatalf("status/hits = %d/%d, body=%s, want %d/0", resp.StatusCode, fallbackHits.Load(), body, tc.status)
				}
				if g.fallbackActive("claude-code") {
					t.Fatal("terminal status created a cooldown")
				}
				if tc.status == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "7" {
					t.Fatalf("Retry-After = %q, want 7", resp.Header.Get("Retry-After"))
				}
			}
			row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
			if got := valueOrZero(row.Fallback.SuppressedReason); got != tc.suppressed {
				t.Fatalf("suppressed_reason = %q, want %q", got, tc.suppressed)
			}
			if row.Fallback.Attempted != tc.fallback {
				t.Fatalf("fallback attempted = %t, want %t", row.Fallback.Attempted, tc.fallback)
			}
		})
	}
}

func TestNativeOrigin429ReplaysExactIngressRequest(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer primary.Close()
	nativeBody := make(chan []byte, 1)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		nativeBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_native","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, fallback.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	rc.NativeFallbackModel = config.DefaultClaudeNativeFallbackModel
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	ingress := []byte(`{ "model": "claude-sonnet-5", "stream": false, "messages": [{"role":"user","content":"ping"}] }`)
	resp, body := postMessagesRaw(t, g, ingress, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	select {
	case got := <-nativeBody:
		if !bytes.Equal(got, ingress) {
			t.Fatalf("native fallback changed ingress bytes:\n got: %s\nwant: %s", got, ingress)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("native fallback request was not observed")
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if !row.Fallback.Attempted || valueOrZero(row.Fallback.Trigger) != "http_429" ||
		valueOrZero(row.Fallback.ModelSource) != fallbackModelSourceOriginal {
		t.Fatalf("fallback telemetry = %+v", row.Fallback)
	}
	if row.RequestedModel != "claude-sonnet-5" || row.ServedModel != "claude-sonnet-5" {
		t.Fatalf("requested/served = %q/%q, want original Sonnet model", row.RequestedModel, row.ServedModel)
	}
}

func TestNativeOrigin429ReplaysDecoratedIngressRequestExactly(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer primary.Close()
	nativeBody := make(chan []byte, 1)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		nativeBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_native","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5[1m]","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, fallback.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	rc.NativeFallbackModel = config.DefaultClaudeNativeFallbackModel
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	ingress := []byte(`{ "model": "claude-sonnet-5[1m]", "stream": false, "messages": [{"role":"user","content":"ping"}] }`)
	resp, body := postMessagesRaw(t, g, ingress, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	select {
	case got := <-nativeBody:
		if !bytes.Equal(got, ingress) {
			t.Fatalf("decorated native fallback changed ingress bytes:\n got: %s\nwant: %s", got, ingress)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decorated native fallback request was not observed")
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if !row.Fallback.Attempted ||
		valueOrZero(row.Fallback.Trigger) != "http_429" ||
		valueOrZero(row.Fallback.ModelSource) != fallbackModelSourceOriginal {
		t.Fatalf("fallback telemetry = %+v", row.Fallback)
	}
	if row.RequestedModel != "claude-sonnet-5" ||
		row.ServedModel != "claude-sonnet-5[1m]" {
		t.Fatalf("requested/served = %q/%q, want canonical ingress attribution and exact decorated fallback model", row.RequestedModel, row.ServedModel)
	}
}

func TestClaudeAlias599UsesConfiguredNativeTarget(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(599)
	}))
	defer primary.Close()
	nativeBody := make(chan []byte, 1)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		nativeBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_native","type":"message","role":"assistant","content":[],"model":"claude-opus-5","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, fallback.URL)
	rc := aliasedClient(t, "baseten")
	rc.FallbackRoute = "anthropic"
	rc.NativeFallbackModel = config.DefaultClaudeNativeFallbackModel
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	ingress := []byte(`{"model":"claude-baseten-glm-5-2","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	resp, body := postMessagesRaw(t, g, ingress, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	got := <-nativeBody
	var before, after map[string]any
	if err := json.Unmarshal(ingress, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &after); err != nil {
		t.Fatal(err)
	}
	if after["model"] != config.DefaultClaudeNativeFallbackModel {
		t.Fatalf("native model = %#v, want %q", after["model"], config.DefaultClaudeNativeFallbackModel)
	}
	delete(before, "model")
	delete(after, "model")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("fallback changed fields other than model: got %#v want %#v", after, before)
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if valueOrZero(row.Fallback.Trigger) != "http_599" ||
		valueOrZero(row.Fallback.ModelSource) != fallbackModelSourceConfigured ||
		row.RequestedModel != "claude-baseten-glm-5-2" ||
		row.ServedModel != config.DefaultClaudeNativeFallbackModel {
		t.Fatalf("alias fallback telemetry = %+v", row)
	}
}

func TestClaudeAliasEnabledPolicyReportsMissingNativeTarget(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer primary.Close()
	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, fallback.URL)
	rc := aliasedClient(t, "baseten")
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, _ := postMessages(t, g, "claude-baseten-glm-5-2")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want terminal 429", resp.StatusCode)
	}
	if got := fallbackHits.Load(); got != 0 {
		t.Fatalf("native fallback hits = %d, want 0", got)
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.Fallback.Attempted || row.Fallback.Trigger != nil ||
		valueOrZero(row.Fallback.SuppressedReason) != "native_target_unconfigured" {
		t.Fatalf("fallback telemetry = %+v", row.Fallback)
	}
}

func TestClaudeAliasTransportFailureRemainsTerminal(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	primaryURL := closed.URL
	closed.Close()
	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primaryURL, fallback.URL)
	rc := aliasedClient(t, "baseten")
	rc.FallbackRoute = "anthropic"
	rc.NativeFallbackModel = config.DefaultClaudeNativeFallbackModel
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, _ := postMessages(t, g, "claude-baseten-glm-5-2")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want terminal 502", resp.StatusCode)
	}
	if got := fallbackHits.Load(); got != 0 {
		t.Fatalf("native fallback hits = %d, want 0", got)
	}
	if g.fallbackActive("claude-code") {
		t.Fatal("status-only alias candidate created transport cooldown")
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.Fallback.Attempted || row.Fallback.Trigger != nil ||
		row.Fallback.SuppressedReason != nil || row.Fallback.ModelSource != nil {
		t.Fatalf("fallback telemetry = %+v, want terminal transport failure", row.Fallback)
	}
}

func TestClaudeAliasAndRawSlugAuthUnavailableRemainTerminal(t *testing.T) {
	for _, model := range []string{
		"claude-baseten-glm-5-2",
		"zai-org/GLM-5.2",
	} {
		t.Run(model, func(t *testing.T) {
			cfg := testConfig(t, "http://baseten.invalid", "http://anthropic.invalid")
			cfg.BasetenKey = ""
			cfg.APIKeyFallback = false
			g := &Gateway{
				cfg:     cfg,
				pricing: pricing.New(),
				client:  &http.Client{Transport: defaultTransport()},
			}
			rc := aliasedClient(t, "baseten")
			rc.FallbackRoute = "anthropic"
			rc.NativeFallbackModel = config.DefaultClaudeNativeFallbackModel
			attempts, err := g.resolveAttempts(
				&clientListener{cfg: rc},
				httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
				[]byte(`{"model":"`+model+`","messages":[{"role":"user","content":"ping"}]}`),
				"messages",
			)
			if !errors.Is(err, errNeedsLogin) {
				t.Fatalf("error = %v, want errNeedsLogin", err)
			}
			if len(attempts) != 0 {
				t.Fatalf("attempts = %d, want terminal preflight failure", len(attempts))
			}
		})
	}
}

func TestClaudeAliasAndRawSlugImageTranslationFailureRemainsTerminal(t *testing.T) {
	for _, model := range []string{
		"claude-baseten-glm-5-2",
		"zai-org/GLM-5.2",
	} {
		t.Run(model, func(t *testing.T) {
			cfg := testConfig(t, "http://baseten.invalid", "http://anthropic.invalid")
			g := &Gateway{
				cfg:     cfg,
				pricing: pricing.New(),
				client:  &http.Client{Transport: defaultTransport()},
			}
			rc := aliasedClient(t, "baseten")
			rc.UpstreamShape = "openai"
			rc.FallbackRoute = "anthropic"
			rc.NativeFallbackModel = config.DefaultClaudeNativeFallbackModel
			// Leave reasoning unconfigured so the unknown catalog capability is
			// internal passthrough and request translation reaches the image gate.
			rc.ModelOptions = nil
			attempts, err := g.resolveAttempts(
				&clientListener{cfg: rc},
				httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
				[]byte(`{"model":"`+model+`","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`),
				"messages",
			)
			if !allowsImageTranslationFallback(err) {
				t.Fatalf("error = %v, want image translation failure", err)
			}
			if len(attempts) != 0 {
				t.Fatalf("attempts = %d, want terminal preflight failure", len(attempts))
			}
		})
	}
}

func TestClaudeAliasAndRawSlugReasoningFailureRemainsTerminal(t *testing.T) {
	for _, model := range []string{
		"claude-baseten-glm-5-2",
		"zai-org/GLM-5.2",
	} {
		t.Run(model, func(t *testing.T) {
			cfg := testConfig(t, "http://baseten.invalid", "http://anthropic.invalid")
			g := &Gateway{
				cfg:     cfg,
				pricing: pricing.New(),
				client:  &http.Client{Transport: defaultTransport()},
			}
			rc := aliasedClient(t, "baseten")
			rc.FallbackRoute = "anthropic"
			rc.NativeFallbackModel = config.DefaultClaudeNativeFallbackModel
			rc.ModelOptions = config.ModelOptions{
				pricing.ProviderBaseten: {
					"zai-org/GLM-5.2": {
						Reasoning: &config.ReasoningPolicy{
							Mode:   config.ReasoningFixed,
							Effort: "high",
						},
					},
				},
			}
			attempts, err := g.resolveAttempts(
				&clientListener{cfg: rc},
				httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
				[]byte(`{"model":"`+model+`","messages":[{"role":"user","content":"ping"}]}`),
				"messages",
			)
			if !reasoning.AllowsFallback(err) {
				t.Fatalf("error = %v, want fallback-eligible reasoning policy error", err)
			}
			if len(attempts) != 0 {
				t.Fatalf("attempts = %d, want terminal preflight failure", len(attempts))
			}
		})
	}
}

func TestStatusCooldownResolvesEachIngressIdentity(t *testing.T) {
	var primaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	models := make(chan string, 3)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		models <- proxyModel(t, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_native","type":"message","role":"assistant","content":[],"model":"claude-opus-5","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, fallback.URL)
	rc := aliasedClient(t, "baseten")
	rc.FallbackRoute = "anthropic"
	rc.NativeFallbackModel = config.DefaultClaudeNativeFallbackModel
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	for _, model := range []string{
		"claude-baseten-glm-5-2",
		"claude-sonnet-5",
		"anthropic-baseten-kimi",
	} {
		resp, body := postMessages(t, g, model)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("model %q status = %d, body = %s", model, resp.StatusCode, body)
		}
	}
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary hits = %d, want 1 before status cooldown", got)
	}
	want := []string{config.DefaultClaudeNativeFallbackModel, "claude-sonnet-5", config.DefaultClaudeNativeFallbackModel}
	for i, expected := range want {
		select {
		case got := <-models:
			if got != expected {
				t.Fatalf("native model %d = %q, want %q", i, got, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("native request %d not observed", i)
		}
	}
	rows := waitForRows(t, cfg.TelemetryDir, 3, 2*time.Second)
	wantSources := []string{fallbackModelSourceConfigured, fallbackModelSourceOriginal, fallbackModelSourceConfigured}
	for i, source := range wantSources {
		if got := valueOrZero(rows[i].Fallback.ModelSource); got != source {
			t.Fatalf("row %d model_source = %q, want %q", i, got, source)
		}
	}
}

func TestSubagentBasetenIngressUsesConfiguredTarget(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	nativeModels := make(chan string, 1)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		nativeModels <- proxyModel(t, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_native","type":"message","role":"assistant","content":[],"model":"claude-opus-5","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, fallback.URL)
	rc := subagentClient(t, "baseten", "claude-baseten-glm-5-2", "on")
	rc.FallbackRoute = "anthropic"
	rc.NativeFallbackModel = config.DefaultClaudeNativeFallbackModel
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, body := postMessagesWithAgent(t, g, "anthropic-baseten-kimi", "agent-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	select {
	case got := <-nativeModels:
		if got != config.DefaultClaudeNativeFallbackModel {
			t.Fatalf("native model = %q, want configured target", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("native fallback request not observed")
	}
	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.RequestedModel != "anthropic-baseten-kimi" ||
		valueOrZero(row.SubagentModel) != "claude-baseten-glm-5-2" ||
		valueOrZero(row.Fallback.ModelSource) != fallbackModelSourceConfigured {
		t.Fatalf("subagent fallback telemetry = %+v", row)
	}
}

func proxyModel(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode upstream body: %v: %s", err, body)
	}
	return envelope.Model
}
