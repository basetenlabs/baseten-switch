package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/reasoning"
)

func reasoningTestGateway(t *testing.T) *Gateway {
	t.Helper()
	cfg := testConfig(
		t,
		"http://baseten.invalid",
		"http://anthropic.invalid",
	)
	return &Gateway{
		cfg:     cfg,
		pricing: pricing.New(),
		client:  &http.Client{Transport: defaultTransport()},
	}
}

func resolvedAnthropicBasetenDefaultReasoning(
	t *testing.T,
) resolvedClientConfig {
	t.Helper()
	rc := resolvedAnthropicBaseten(t)
	rc.ModelOptions = nil
	return rc
}

func assertGatewayThinkingDisabled(t *testing.T, body []byte) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode transformed body: %v\nbody: %s", err, body)
	}
	var thinking map[string]json.RawMessage
	if err := json.Unmarshal(envelope["thinking"], &thinking); err != nil {
		t.Fatalf("decode transformed thinking: %v\nbody: %s", err, body)
	}
	if len(thinking) != 1 ||
		string(thinking["type"]) != `"disabled"` {
		t.Fatalf(
			"transformed thinking = %s, want exactly {\"type\":\"disabled\"}",
			envelope["thinking"],
		)
	}
}

func TestReasoningPolicyMappedToggleDefaultsOff(t *testing.T) {
	g := reasoningTestGateway(t)
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	cl := &clientListener{cfg: rc}
	body := []byte(`{
		"model":"claude-opus-4-8",
		"thinking":{"type":"enabled","budget_tokens":32000},
		"messages":[{"role":"user","content":"hello"}]
	}`)

	attempts, err := g.resolveAttempts(
		cl,
		httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		body,
		"messages",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 ||
		attempts[0].route != "baseten" ||
		attempts[0].fallbackTrigger != "" {
		t.Fatalf("attempts = %+v, want one Baseten primary", attempts)
	}
	assertGatewayThinkingDisabled(t, attempts[0].res.NewBody)
	if !bytes.Contains(
		attempts[0].res.NewBody,
		[]byte(`"model":"zai-org/GLM-5.2"`),
	) {
		t.Fatalf("mapped body = %s, want rewritten model", attempts[0].res.NewBody)
	}
}

func TestReasoningPolicyToggleOffAfterEveryMessagesTargetResolver(
	t *testing.T,
) {
	cases := []struct {
		name      string
		configure func(*resolvedClientConfig, *http.Request)
		model     string
	}{
		{
			name: "GLM default mapping",
			configure: func(_ *resolvedClientConfig, _ *http.Request) {
			},
			model: "claude-opus-4-8",
		},
		{
			name: "Kimi family route",
			configure: func(rc *resolvedClientConfig, _ *http.Request) {
				rc.ModelRoutes = map[string]string{
					"opus": "moonshotai/Kimi-K2.7-Code",
				}
			},
			model: "claude-opus-4-8",
		},
		{
			name: "Nemotron alias",
			configure: func(rc *resolvedClientConfig, _ *http.Request) {
				rc.ModelAliases = map[string]string{
					"claude-baseten-nemotron": "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B",
				}
			},
			model: "claude-baseten-nemotron",
		},
		{
			name: "GLM raw slug",
			configure: func(_ *resolvedClientConfig, _ *http.Request) {
			},
			model: "zai-org/GLM-5.2",
		},
		{
			name: "GLM native subagent uses default mapping",
			configure: func(rc *resolvedClientConfig, req *http.Request) {
				rc.SubagentModel = "claude-sonnet-4-6"
				req.Header.Set(subagentAgentIDHeader, "agent-1")
			},
			model: "claude-sonnet-4-6",
		},
		{
			name: "Kimi raw subagent override",
			configure: func(rc *resolvedClientConfig, req *http.Request) {
				rc.SubagentModel = "moonshotai/Kimi-K2.7-Code"
				req.Header.Set(subagentAgentIDHeader, "agent-1")
			},
			model: "claude-sonnet-4-6",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := reasoningTestGateway(t)
			rc := resolvedAnthropicBasetenDefaultReasoning(t)
			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/messages",
				nil,
			)
			tc.configure(&rc, req)
			body := []byte(`{
				"model":"` + tc.model + `",
				"thinking":{"type":"enabled","budget_tokens":32000},
				"messages":[{"role":"user","content":"hello"}]
			}`)
			attempts, err := g.resolveAttempts(
				&clientListener{cfg: rc},
				req,
				body,
				"messages",
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(attempts) != 1 ||
				attempts[0].route != "baseten" ||
				attempts[0].fallbackTrigger != "" {
				t.Fatalf("attempts = %+v", attempts)
			}
			assertGatewayThinkingDisabled(t, attempts[0].res.NewBody)
		})
	}
}

func TestReasoningPolicyFollowHarnessPreservesToggle(t *testing.T) {
	g := reasoningTestGateway(t)
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	rc.ModelOptions = config.ModelOptions{
		"baseten": {
			"zai-org/GLM-5.2": {
				Reasoning: &config.ReasoningPolicy{
					Mode: config.ReasoningFollowHarness,
				},
			},
		},
	}
	body := []byte(`{
		"model":"claude-opus-4-8",
		"thinking":{"type":"enabled","budget_tokens":32000},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	attempts, err := g.resolveAttempts(
		&clientListener{cfg: rc},
		httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		body,
		"messages",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 ||
		!bytes.Contains(attempts[0].res.NewBody, []byte(`"thinking"`)) ||
		!bytes.Contains(attempts[0].res.NewBody, []byte(`32000`)) {
		t.Fatalf("follow_harness body = %s", attempts[0].res.NewBody)
	}
}

func TestReasoningPolicyKimiFollowHarnessNormalizesClaudeAdaptiveThinking(
	t *testing.T,
) {
	var basetenHits atomic.Int32
	upstreamBody := make(chan []byte, 1)
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			basetenHits.Add(1)
			body, _ := io.ReadAll(r.Body)
			upstreamBody <- body
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				"event: message_stop\n" +
					"data: {\"type\":\"message_stop\"}\n\n",
			))
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			nativeHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, native.URL)
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	rc.ModelRoutes = map[string]string{
		"sonnet": "moonshotai/Kimi-K2.7-Code",
	}
	rc.ModelOptions = config.ModelOptions{
		pricing.ProviderBaseten: {
			"moonshotai/Kimi-K2.7-Code": {
				Reasoning: &config.ReasoningPolicy{
					Mode: config.ReasoningFollowHarness,
				},
			},
		},
	}
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	if err := g.pricing.ReplaceModelsDev(
		[]byte(adminReasoningCatalogFixture),
		time.Now().UTC(),
		`"kimi-adaptive-thinking"`,
	); err != nil {
		t.Fatal(err)
	}
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":32000,
		"stream":true,
		"thinking":{"type":"adaptive","display":"omitted"},
		"output_config":{"effort":"high"},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	req, _ := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages?beta=true"),
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf(
			"status=%d body=%s, want successful Kimi Baseten attempt",
			response.StatusCode,
			responseBody,
		)
	}
	if basetenHits.Load() != 1 || nativeHits.Load() != 0 {
		t.Fatalf(
			"upstream hits = Baseten %d native %d, want 1/0",
			basetenHits.Load(),
			nativeHits.Load(),
		)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(<-upstreamBody, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["model"]) != `"moonshotai/Kimi-K2.7-Code"` {
		t.Fatalf("upstream model = %s, want Kimi K2.7 Code", got["model"])
	}
	if string(got["thinking"]) != `{"type":"enabled"}` {
		t.Fatalf(
			"upstream thinking = %s, want exactly {\"type\":\"enabled\"}",
			got["thinking"],
		)
	}
	var outputConfig map[string]json.RawMessage
	if err := json.Unmarshal(got["output_config"], &outputConfig); err != nil {
		t.Fatal(err)
	}
	if string(outputConfig["effort"]) != `"high"` {
		t.Fatalf(
			"upstream output_config = %s, want preserved effort high",
			got["output_config"],
		)
	}
}

func TestReasoningPolicyUnsupportedAdapterDefaultsPassthrough(t *testing.T) {
	g := reasoningTestGateway(t)
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	rc.UpstreamShape = "openai"
	body := []byte(`{
		"model":"claude-opus-4-8",
		"thinking":{"type":"enabled","budget_tokens":32000},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	attempts, err := g.resolveAttempts(
		&clientListener{cfg: rc},
		httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		body,
		"messages",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 ||
		attempts[0].route != "baseten" ||
		attempts[0].fallbackTrigger != "" {
		t.Fatalf("attempt = %+v, want Baseten passthrough", attempts[0])
	}
}

func TestReasoningPolicyModelsWithoutOffDefaultPassthrough(t *testing.T) {
	for _, model := range []string{
		"deepseek-ai/DeepSeek-V4-Pro",
		"example/No-Control",
	} {
		t.Run(model, func(t *testing.T) {
			g := reasoningTestGateway(t)
			if err := g.pricing.ReplaceModelsDev(
				[]byte(adminReasoningCatalogFixture),
				time.Now().UTC(),
				`"default-passthrough"`,
			); err != nil {
				t.Fatal(err)
			}
			rc := resolvedAnthropicBasetenDefaultReasoning(t)
			rc.DefaultModel = model
			body := []byte(`{
				"model":"claude-opus-4-8",
				"thinking":{"type":"enabled","budget_tokens":32000},
				"messages":[{"role":"user","content":"hello"}]
			}`)
			attempts, err := g.resolveAttempts(
				&clientListener{cfg: rc},
				httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
				body,
				"messages",
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(attempts) != 1 ||
				attempts[0].route != "baseten" ||
				attempts[0].fallbackTrigger != "" {
				t.Fatalf("attempts = %+v", attempts)
			}
			requested := reasoning.InspectAnthropicMessages(
				attempts[0].res.NewBody,
			)
			if !requested.Present ||
				!requested.Recognized ||
				requested.Disabled ||
				requested.BudgetTokens == nil ||
				*requested.BudgetTokens != 32000 {
				t.Fatalf(
					"%s passthrough reasoning = %+v",
					model,
					requested,
				)
			}
		})
	}
}

func TestReasoningPolicyExplicitBasetenErrorHasNoFallback(t *testing.T) {
	g := reasoningTestGateway(t)
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	rc.DefaultModel = "deepseek-ai/DeepSeek-V4-Pro"
	rc.FallbackRoute = "anthropic"
	rc.ModelOptions = config.ModelOptions{
		pricing.ProviderBaseten: {
			"deepseek-ai/DeepSeek-V4-Pro": {
				Reasoning: &config.ReasoningPolicy{
					Mode: config.ReasoningOff,
				},
			},
		},
	}
	if err := g.pricing.ReplaceModelsDev(
		[]byte(adminReasoningCatalogFixture),
		time.Now().UTC(),
		`"explicit-unsupported-off"`,
	); err != nil {
		t.Fatal(err)
	}
	_, err := g.resolveAttempts(
		&clientListener{cfg: rc},
		httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		[]byte(`{
			"model":"deepseek-ai/DeepSeek-V4-Pro",
			"messages":[{"role":"user","content":"hello"}]
		}`),
		"messages",
	)
	var policyErr *reasoning.PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("error = %v, want explicit local PolicyError", err)
	}
}

func TestReasoningPolicyExplicitConfiguredUnsupportedOffStaysLocal(
	t *testing.T,
) {
	for _, tc := range []struct {
		name       string
		path       string
		body       string
		clientName string
		client     func(*testing.T) resolvedClientConfig
	}{
		{
			name:       "Messages effort-only model",
			path:       "/v1/messages",
			body:       `{"model":"deepseek-ai/DeepSeek-V4-Pro","messages":[{"role":"user","content":"hello"}]}`,
			clientName: "claude-code",
			client: func(t *testing.T) resolvedClientConfig {
				rc := resolvedAnthropicBasetenDefaultReasoning(t)
				rc.DefaultModel = "deepseek-ai/DeepSeek-V4-Pro"
				rc.FallbackRoute = "anthropic"
				rc.ModelOptions = config.ModelOptions{
					pricing.ProviderBaseten: {
						"deepseek-ai/DeepSeek-V4-Pro": {
							Reasoning: &config.ReasoningPolicy{
								Mode: config.ReasoningOff,
							},
						},
					},
				}
				return rc
			},
		},
		{
			name:       "OpenAI Chat",
			path:       "/v1/chat/completions",
			body:       `{"model":"zai-org/GLM-5.2","messages":[{"role":"user","content":"hello"}]}`,
			clientName: "opencode",
			client: func(t *testing.T) resolvedClientConfig {
				rc := resolvedOpenAIBaseten(t, "opencode", "baseten")
				rc.FallbackRoute = "openai"
				rc.ModelOptions = config.ModelOptions{
					pricing.ProviderBaseten: {
						"zai-org/GLM-5.2": {
							Reasoning: &config.ReasoningPolicy{
								Mode: config.ReasoningOff,
							},
						},
					},
				}
				return rc
			},
		},
		{
			name:       "OpenAI Responses",
			path:       "/v1/responses",
			body:       `{"model":"zai-org/GLM-5.2","input":"hello"}`,
			clientName: "codex",
			client: func(t *testing.T) resolvedClientConfig {
				rc := resolvedOpenAIBaseten(t, "codex", "baseten")
				rc.FallbackRoute = "openai"
				rc.ModelOptions = config.ModelOptions{
					pricing.ProviderBaseten: {
						"zai-org/GLM-5.2": {
							Reasoning: &config.ReasoningPolicy{
								Mode: config.ReasoningOff,
							},
						},
					},
				}
				return rc
			},
		},
		{
			name:       "translated Messages",
			path:       "/v1/messages",
			body:       `{"model":"zai-org/GLM-5.2","thinking":{"type":"enabled","budget_tokens":32000},"messages":[{"role":"user","content":"hello"}]}`,
			clientName: "claude-code",
			client: func(t *testing.T) resolvedClientConfig {
				rc := resolvedAnthropicBasetenDefaultReasoning(t)
				rc.UpstreamShape = "openai"
				rc.FallbackRoute = "anthropic"
				rc.ModelOptions = config.ModelOptions{
					pricing.ProviderBaseten: {
						"zai-org/GLM-5.2": {
							Reasoning: &config.ReasoningPolicy{
								Mode: config.ReasoningOff,
							},
						},
					},
				}
				return rc
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var basetenHits atomic.Int32
			baseten := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					basetenHits.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				},
			))
			defer baseten.Close()

			var nativeHits atomic.Int32
			native := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					nativeHits.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				},
			))
			defer native.Close()

			cfg := testConfig(t, baseten.URL, native.URL)
			cfg.OpenAIURL = native.URL
			g, adminListener, _ := newGateway(t, cfg, tc.client(t))
			defer adminListener.Close()
			if err := g.pricing.ReplaceModelsDev(
				[]byte(adminReasoningCatalogFixture),
				time.Now().UTC(),
				`"explicit-unsupported-off"`,
			); err != nil {
				t.Fatal(err)
			}
			stop := start(t, g)
			defer stop()

			req, _ := http.NewRequest(
				http.MethodPost,
				clientURL(g, tc.clientName, tc.path),
				bytes.NewBufferString(tc.body),
			)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer tok")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			responseBody, _ := io.ReadAll(response.Body)
			response.Body.Close()

			if response.StatusCode != http.StatusBadRequest ||
				!bytes.Contains(
					responseBody,
					[]byte("reasoning_policy_error"),
				) {
				t.Fatalf(
					"status=%d body=%s, want local reasoning_policy_error",
					response.StatusCode,
					responseBody,
				)
			}
			if basetenHits.Load() != 0 || nativeHits.Load() != 0 {
				t.Fatalf(
					"upstream hits = Baseten %d native %d, want 0/0",
					basetenHits.Load(),
					nativeHits.Load(),
				)
			}
		})
	}
}

func TestReasoningPolicyUnrecognizedHarnessControlContactsNoUpstream(t *testing.T) {
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			basetenHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			nativeHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, native.URL)
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	rc.FallbackRoute = "anthropic"
	rc.ModelOptions = config.ModelOptions{
		"baseten": {
			"zai-org/GLM-5.2": {
				Reasoning: &config.ReasoningPolicy{
					Mode: config.ReasoningFollowHarness,
				},
			},
		},
	}
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	req, _ := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewBufferString(`{
			"model":"claude-opus-4-8",
			"thinking":{"type":"future_mode"},
			"messages":[{"role":"user","content":"hello"}]
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	response.Body.Close()

	if response.StatusCode != http.StatusBadRequest ||
		!bytes.Contains(responseBody, []byte("reasoning_policy_error")) {
		t.Fatalf(
			"status=%d body=%s, want local reasoning_policy_error",
			response.StatusCode,
			responseBody,
		)
	}
	if basetenHits.Load() != 0 || nativeHits.Load() != 0 {
		t.Fatalf(
			"upstream hits = Baseten %d native %d, want 0/0",
			basetenHits.Load(),
			nativeHits.Load(),
		)
	}
}

func TestReasoningPolicyChangesResolvedClientHash(t *testing.T) {
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	before := rc.hash()
	rc.ModelOptions = config.ModelOptions{
		"baseten": {
			"zai-org/GLM-5.2": {
				Reasoning: &config.ReasoningPolicy{
					Mode: config.ReasoningFollowHarness,
				},
			},
		},
	}
	if after := rc.hash(); after == before {
		t.Fatal("reasoning policy did not change resolved client hash")
	}
}

func TestReasoningPolicyMappedOffRuntimeFallbackPreservesNativeBody(
	t *testing.T,
) {
	var basetenHits atomic.Int32
	var basetenBody []byte
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			basetenHits.Add(1)
			basetenBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer baseten.Close()

	var nativeBody []byte
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			nativeBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id":"msg_native",
				"type":"message",
				"role":"assistant",
				"content":[{"type":"text","text":"native"}],
				"model":"claude-opus-4-8",
				"usage":{"input_tokens":1,"output_tokens":1}
			}`))
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, native.URL)
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	rc.FallbackRoute = "anthropic"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"claude-opus-4-8",
		"thinking":{"type":"enabled","budget_tokens":32000},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	req, _ := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d body = %s", response.StatusCode, responseBody)
	}
	if basetenHits.Load() != 1 {
		t.Fatalf("Baseten hits = %d, want 1", basetenHits.Load())
	}
	assertGatewayThinkingDisabled(t, basetenBody)
	if !bytes.Equal(nativeBody, body) {
		t.Fatalf(
			"native fallback body changed\ngot:  %s\nwant: %s",
			nativeBody,
			body,
		)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	row := rows[0]
	if row.RequestedReasoningPresent == nil ||
		!*row.RequestedReasoningPresent {
		t.Fatalf(
			"fallback requested_reasoning_present = %v, want true",
			row.RequestedReasoningPresent,
		)
	}
	if row.ReasoningPolicyMode == nil ||
		*row.ReasoningPolicyMode != "off" ||
		row.EffectiveReasoningEnabled == nil ||
		*row.EffectiveReasoningEnabled ||
		row.ReasoningPolicySource == nil ||
		*row.ReasoningPolicySource != "compatibility_default" {
		t.Fatalf(
			"runtime fallback policy: mode=%v enabled=%v source=%v",
			row.ReasoningPolicyMode,
			row.EffectiveReasoningEnabled,
			row.ReasoningPolicySource,
		)
	}
	if row.Fallback.Trigger == nil ||
		*row.Fallback.Trigger != "http_500" {
		t.Fatalf("fallback trigger = %v, want http_500", row.Fallback.Trigger)
	}
}

func TestReasoningPolicyConfiguredOffOpenAIShapesPreflightFallbackHTTP(
	t *testing.T,
) {
	for _, tc := range []struct {
		name       string
		clientName string
		path       string
		body       []byte
	}{
		{
			name:       "Chat Completions",
			clientName: "opencode",
			path:       "/v1/chat/completions",
			body: []byte(
				`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"high"}`,
			),
		},
		{
			name:       "Responses",
			clientName: "codex",
			path:       "/v1/responses",
			body: []byte(
				`{"model":"gpt-5","input":"hello","reasoning":{"effort":"high"}}`,
			),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var basetenHits atomic.Int32
			baseten := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					basetenHits.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				},
			))
			defer baseten.Close()

			nativeBody := make(chan []byte, 1)
			native := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					nativeBody <- body
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"native","object":"response"}`))
				},
			))
			defer native.Close()

			cfg := testConfig(t, baseten.URL, baseten.URL)
			cfg.OpenAIURL = native.URL
			rc := resolvedOpenAIBaseten(t, tc.clientName, "baseten")
			rc.FallbackRoute = "openai"
			rc.ModelOptions = config.ModelOptions{
				pricing.ProviderBaseten: {
					"zai-org/GLM-5.2": {
						Reasoning: &config.ReasoningPolicy{
							Mode: config.ReasoningOff,
						},
					},
				},
			}
			g, adminListener, _ := newGateway(t, cfg, rc)
			defer adminListener.Close()
			stop := start(t, g)
			defer stop()

			req, _ := http.NewRequest(
				http.MethodPost,
				clientURL(g, tc.clientName, tc.path),
				bytes.NewReader(tc.body),
			)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer tok")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			responseBody, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf(
					"status = %d body = %s, want native fallback 200",
					response.StatusCode,
					responseBody,
				)
			}
			if basetenHits.Load() != 0 {
				t.Fatalf("Baseten hits = %d, want preflight 0", basetenHits.Load())
			}
			if got := <-nativeBody; !bytes.Equal(got, tc.body) {
				t.Fatalf(
					"native fallback did not receive exact original body\ngot:  %s\nwant: %s",
					got,
					tc.body,
				)
			}

			rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
			if rows[0].Fallback.Trigger == nil ||
				*rows[0].Fallback.Trigger != "reasoning_policy_error" {
				t.Fatalf(
					"fallback trigger = %v, want reasoning_policy_error",
					rows[0].Fallback.Trigger,
				)
			}
		})
	}
}

func TestReasoningPolicyCatalogSnapshotStableAcrossRuntimeFallback(t *testing.T) {
	const initialCatalog = `{
		"anthropic":{"id":"anthropic","models":{"claude-opus-4-8":{"id":"claude-opus-4-8"}}},
		"openai":{"id":"openai","models":{"gpt-5":{"id":"gpt-5"}}},
		"baseten":{"id":"baseten","models":{"zai-org/GLM-5.2":{
			"id":"zai-org/GLM-5.2",
			"name":"GLM 5.2 initial",
			"family":"glm",
			"reasoning":true,
			"reasoning_options":[{"type":"toggle"}]
		}}}
	}`
	const changedCatalog = `{
		"anthropic":{"id":"anthropic","models":{"claude-opus-4-8":{"id":"claude-opus-4-8"}}},
		"openai":{"id":"openai","models":{"gpt-5":{"id":"gpt-5"}}},
		"baseten":{"id":"baseten","models":{"zai-org/GLM-5.2":{
			"id":"zai-org/GLM-5.2",
			"name":"GLM 5.2 refreshed",
			"family":"glm",
			"reasoning":true,
			"reasoning_options":[{"type":"toggle"}]
		}}}
	}`

	restorePublicCatalogTestGlobals(t)
	publicCatalogFailure := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	))
	defer publicCatalogFailure.Close()
	publicCatalogURL = publicCatalogFailure.URL

	var catalog *pricing.Pricing
	refreshResult := make(chan error, 1)
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			refreshResult <- catalog.ReplaceModelsDev(
				[]byte(changedCatalog),
				time.Unix(200, 0).UTC(),
				`"changed"`,
			)
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer baseten.Close()

	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id":"msg_native",
				"type":"message",
				"role":"assistant",
				"content":[{"type":"text","text":"native"}],
				"model":"claude-opus-4-8",
				"usage":{"input_tokens":1,"output_tokens":1}
			}`))
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, native.URL)
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	rc.FallbackRoute = "anthropic"
	rc.ModelOptions = config.ModelOptions{
		"baseten": {
			"zai-org/GLM-5.2": {
				Reasoning: &config.ReasoningPolicy{
					Mode: config.ReasoningFollowHarness,
				},
			},
		},
	}
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	catalog = g.pricing
	if err := catalog.ReplaceModelsDev(
		[]byte(initialCatalog),
		time.Unix(100, 0).UTC(),
		`"initial"`,
	); err != nil {
		t.Fatal(err)
	}
	initialCapability, ok := catalog.Capture().ModelReasoning(
		pricing.ProviderBaseten,
		"zai-org/GLM-5.2",
	)
	if !ok || initialCapability.Provenance.Revision == "" {
		t.Fatalf("initial GLM reasoning capability = %+v, found = %t", initialCapability, ok)
	}
	initialRevision := initialCapability.Provenance.Revision

	stop := start(t, g)
	defer stop()
	body := []byte(`{
		"model":"claude-opus-4-8",
		"thinking":{"type":"enabled","budget_tokens":32000},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	req, _ := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s, want fallback 200", response.StatusCode, responseBody)
	}
	if err := <-refreshResult; err != nil {
		t.Fatalf("publish changed catalog: %v", err)
	}
	changedCapability, ok := catalog.Capture().ModelReasoning(
		pricing.ProviderBaseten,
		"zai-org/GLM-5.2",
	)
	if !ok || changedCapability.Provenance.Revision == initialRevision {
		t.Fatalf(
			"changed GLM reasoning revision = %q, want different from %q",
			changedCapability.Provenance.Revision,
			initialRevision,
		)
	}

	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.ReasoningPolicyMode == nil ||
		*row.ReasoningPolicyMode != "follow_harness" {
		t.Fatalf(
			"logical request reasoning policy = %v, want follow_harness",
			row.ReasoningPolicyMode,
		)
	}
	if row.ReasoningCatalogRevision == nil ||
		*row.ReasoningCatalogRevision != initialRevision {
		t.Fatalf(
			"logical request reasoning catalog revision = %v, want initial %q",
			row.ReasoningCatalogRevision,
			initialRevision,
		)
	}
	if row.Fallback.Trigger == nil || *row.Fallback.Trigger != "http_500" {
		t.Fatalf("fallback trigger = %v, want http_500", row.Fallback.Trigger)
	}
}

func TestReasoningPolicyMessagesSSEStreamsBeforeCompletion(t *testing.T) {
	firstEventWritten := make(chan struct{})
	releaseCompletion := make(chan struct{})
	upstreamComplete := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseCompletion) })

	requestBody := make(chan []byte, 1)
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			body, _ := io.ReadAll(r.Body)
			requestBody <- body
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte(
				"event: content_block_delta\n" +
					"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"first\"}}\n\n",
			))
			flusher.Flush()
			close(firstEventWritten)
			<-releaseCompletion
			_, _ = w.Write([]byte(
				"event: message_stop\n" +
					"data: {\"type\":\"message_stop\"}\n\n",
			))
			flusher.Flush()
			close(upstreamComplete)
		},
	))
	defer baseten.Close()

	cfg := testConfig(t, baseten.URL, "http://anthropic.invalid")
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	rc.ModelOptions = config.ModelOptions{
		"baseten": {
			"zai-org/GLM-5.2": {
				Reasoning: &config.ReasoningPolicy{
					Mode: config.ReasoningFollowHarness,
				},
			},
		},
	}
	g, adminListener, _ := newGateway(
		t,
		cfg,
		rc,
	)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"claude-opus-4-8",
		"stream":true,
		"thinking":{"type":"enabled","budget_tokens":32000},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	req, _ := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d body = %s, want 200", response.StatusCode, responseBody)
	}

	select {
	case <-firstEventWritten:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not write first SSE event")
	}
	reader := bufio.NewReader(response.Body)
	firstEvent := make(chan string, 1)
	firstEventError := make(chan error, 1)
	go func() {
		var event bytes.Buffer
		for {
			line, readErr := reader.ReadString('\n')
			event.WriteString(line)
			if line == "\n" || readErr != nil {
				if readErr != nil {
					firstEventError <- readErr
					return
				}
				firstEvent <- event.String()
				return
			}
		}
	}()
	select {
	case got := <-firstEvent:
		if !strings.Contains(got, `"text":"first"`) {
			t.Fatalf("first SSE event = %q, want first text delta", got)
		}
	case err := <-firstEventError:
		t.Fatalf("read first SSE event before completion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE event was buffered until stream completion")
	}
	select {
	case <-upstreamComplete:
		t.Fatal("upstream completed before the test released it")
	default:
	}

	gotRequestBody := <-requestBody
	releaseOnce.Do(func() { close(releaseCompletion) })
	inspectedRequest := reasoning.InspectAnthropicMessages(gotRequestBody)
	if !inspectedRequest.Present ||
		!inspectedRequest.Recognized ||
		inspectedRequest.Disabled ||
		inspectedRequest.BudgetTokens == nil ||
		*inspectedRequest.BudgetTokens != 32000 {
		t.Fatalf("follow_harness changed streaming reasoning request: %+v", inspectedRequest)
	}
	if !bytes.Contains(gotRequestBody, []byte(`"model":"zai-org/GLM-5.2"`)) {
		t.Fatalf("upstream request body = %s, want rewritten GLM model", gotRequestBody)
	}
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(remaining, []byte("message_stop")) {
		t.Fatalf("remaining SSE stream = %q, want message_stop", remaining)
	}
	select {
	case <-upstreamComplete:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not complete after release")
	}
}

func TestReasoningPolicyTelemetryForSuccessfulBasetenAttempt(t *testing.T) {
	upstreamBodies := make(chan []byte, 2)
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			upstreamBodies <- body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id":"msg_baseten",
				"type":"message",
				"role":"assistant",
				"content":[{"type":"text","text":"baseten"}],
				"model":"zai-org/GLM-5.2",
				"usage":{"input_tokens":1,"output_tokens":1}
			}`))
		},
	))
	defer baseten.Close()

	cfg := testConfig(t, baseten.URL, "http://anthropic.invalid")
	rc := resolvedAnthropicBasetenDefaultReasoning(t)
	g, adminListener, _ := newGateway(
		t,
		cfg,
		rc,
	)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"claude-opus-4-8",
		"thinking":{"type":"enabled","budget_tokens":32000},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	req, _ := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	absentBody := []byte(`{
		"model":"claude-opus-4-8",
		"messages":[{"role":"user","content":"hello again"}]
	}`)
	absentRequest, _ := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(absentBody),
	)
	absentRequest.Header.Set("Content-Type", "application/json")
	absentRequest.Header.Set("Authorization", "Bearer tok")
	absentResponse, err := http.DefaultClient.Do(absentRequest)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, absentResponse.Body)
	absentResponse.Body.Close()
	if absentResponse.StatusCode != http.StatusOK {
		t.Fatalf("absent status = %d, want 200", absentResponse.StatusCode)
	}
	assertGatewayThinkingDisabled(t, <-upstreamBodies)
	assertGatewayThinkingDisabled(t, <-upstreamBodies)

	rows := waitForRows(t, cfg.TelemetryDir, 2, 2*time.Second)
	row := rows[0]
	if row.RequestedReasoningPresent == nil ||
		!*row.RequestedReasoningPresent {
		t.Fatalf(
			"requested_reasoning_present = %v, want true",
			row.RequestedReasoningPresent,
		)
	}
	if row.ReasoningPolicyMode == nil ||
		*row.ReasoningPolicyMode != "off" {
		t.Fatalf(
			"reasoning_policy_mode = %v, want off",
			row.ReasoningPolicyMode,
		)
	}
	if row.EffectiveReasoningEnabled == nil ||
		*row.EffectiveReasoningEnabled {
		t.Fatalf(
			"effective_reasoning_enabled = %v, want false",
			row.EffectiveReasoningEnabled,
		)
	}
	if row.ReasoningPolicySource == nil ||
		*row.ReasoningPolicySource != "compatibility_default" {
		t.Fatalf(
			"reasoning_policy_source = %v, want compatibility_default",
			row.ReasoningPolicySource,
		)
	}
	if row.ReasoningCatalogRevision == nil ||
		*row.ReasoningCatalogRevision == "" {
		t.Fatal("reasoning_catalog_revision is missing")
	}
	if row.RequestedReasoningEffort != nil ||
		row.EffectiveReasoningEffort != nil {
		t.Fatalf(
			"Messages effort fields = requested %v effective %v, want null",
			row.RequestedReasoningEffort,
			row.EffectiveReasoningEffort,
		)
	}
	absentRow := rows[1]
	if absentRow.RequestedReasoningPresent == nil ||
		*absentRow.RequestedReasoningPresent {
		t.Fatalf(
			"absent requested_reasoning_present = %v, want observed false",
			absentRow.RequestedReasoningPresent,
		)
	}
	if absentRow.ReasoningPolicyMode == nil ||
		*absentRow.ReasoningPolicyMode != "off" ||
		absentRow.EffectiveReasoningEnabled == nil ||
		*absentRow.EffectiveReasoningEnabled ||
		absentRow.ReasoningPolicySource == nil ||
		*absentRow.ReasoningPolicySource != "compatibility_default" {
		t.Fatalf(
			"absent effective policy = mode %v enabled %v source %v",
			absentRow.ReasoningPolicyMode,
			absentRow.EffectiveReasoningEnabled,
			absentRow.ReasoningPolicySource,
		)
	}
}
