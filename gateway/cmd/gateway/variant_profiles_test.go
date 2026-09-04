package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/requestprofile"
)

const variantOtherBeta = "context-1m-2025-08-07"

type variantUpstreamRequest struct {
	path    string
	body    map[string]any
	headers http.Header
}

func TestVariantProfileNativeAnthropicCanonicalizesModelAndPreservesFast(t *testing.T) {
	recorded := make(chan variantUpstreamRequest, 1)
	anthropic := variantAnthropicServer(t, recorded, http.StatusOK, "fast")
	defer anthropic.Close()

	cfg := testConfig(t, anthropic.URL, anthropic.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.Route = "anthropic"
	rc.GlobalRoutingEnabled = false
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	response := postVariantMessages(t, g, "claude-opus-5[1m]")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	got := receiveVariantRequest(t, recorded)
	if got.path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", got.path)
	}
	if model := fmtString(got.body["model"]); model != "claude-opus-5" {
		t.Fatalf("upstream model = %q, want canonical claude-opus-5", model)
	}
	if speed := fmtString(got.body["speed"]); speed != "fast" {
		t.Fatalf("upstream speed = %q, want fast", speed)
	}
	assertVariantBetaTokens(t, got.headers, true)

	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].RequestedModelFamily != "opus" {
		t.Fatalf("requested_model_family = %q, want opus", rows[0].RequestedModelFamily)
	}
}

func TestVariantProfileBasetenAnthropicStripsUnsupportedFastOnly(t *testing.T) {
	recorded := make(chan variantUpstreamRequest, 1)
	baseten := variantAnthropicServer(t, recorded, http.StatusOK, "")
	defer baseten.Close()

	cfg := testConfig(t, baseten.URL, baseten.URL)
	rc := resolvedAnthropicBaseten(t)
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	response := postVariantMessages(t, g, "claude-opus-5[1m]")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	got := receiveVariantRequest(t, recorded)
	if got.path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", got.path)
	}
	if _, found := got.body["speed"]; found {
		t.Fatalf("unsupported top-level speed reached Baseten: %#v", got.body["speed"])
	}
	if model := fmtString(got.body["model"]); model == "" ||
		model == "claude-opus-5" ||
		strings.Contains(model, "[1m]") {
		t.Fatalf("Baseten model was not rewritten to a canonical target: %q", model)
	}
	if metadata, ok := got.body["metadata"].(map[string]any); !ok ||
		metadata["speed"] != "nested-keep" ||
		metadata["trace"] != "keep" {
		t.Fatalf("unrelated request fields changed: %#v", got.body["metadata"])
	}
	assertVariantBetaTokens(t, got.headers, false)
}

func TestVariantProfileBasetenOpenAIStripsUnsupportedFastOnly(t *testing.T) {
	recorded := make(chan variantUpstreamRequest, 1)
	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordVariantRequest(t, recorded, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl_variant",
			"model":"zai-org/GLM-5.2",
			"choices":[{"finish_reason":"stop","message":{"content":"ok"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}
		}`))
	}))
	defer baseten.Close()

	cfg := testConfig(t, baseten.URL, baseten.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.DefaultModel = "moonshotai/Kimi-K2.7-Code"
	rc.UpstreamShape = "openai"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	response := postVariantMessages(t, g, "claude-opus-5[1m]")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	got := receiveVariantRequest(t, recorded)
	if got.path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", got.path)
	}
	if _, found := got.body["speed"]; found {
		t.Fatalf("unsupported top-level speed reached OpenAI upstream: %#v", got.body["speed"])
	}
	if model := fmtString(got.body["model"]); model == "" ||
		model == "claude-opus-5" ||
		strings.Contains(model, "[1m]") {
		t.Fatalf("OpenAI upstream model was not rewritten to a canonical target: %q", model)
	}
	assertVariantBetaTokens(t, got.headers, false)
}

func TestVariantProfileFallbackPreservesFastForNativeAnthropic(t *testing.T) {
	primaryRequests := make(chan variantUpstreamRequest, 1)
	primary := variantAnthropicServer(t, primaryRequests, http.StatusServiceUnavailable, "")
	defer primary.Close()
	fallbackRequests := make(chan variantUpstreamRequest, 1)
	fallback := variantAnthropicServer(t, fallbackRequests, http.StatusOK, "fast")
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, fallback.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	response := postVariantMessages(t, g, "claude-opus-5[1m]")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want fallback 200", response.StatusCode)
	}

	primaryGot := receiveVariantRequest(t, primaryRequests)
	if _, found := primaryGot.body["speed"]; found {
		t.Fatalf("unsupported speed reached Baseten primary: %#v", primaryGot.body["speed"])
	}
	assertVariantBetaTokens(t, primaryGot.headers, false)

	fallbackGot := receiveVariantRequest(t, fallbackRequests)
	if model := fmtString(fallbackGot.body["model"]); model != "claude-opus-5[1m]" {
		t.Fatalf("fallback model = %q, want exact decorated ingress model", model)
	}
	if speed := fmtString(fallbackGot.body["speed"]); speed != "fast" {
		t.Fatalf("fallback speed = %q, want fast", speed)
	}
	assertVariantBetaTokens(t, fallbackGot.headers, true)
}

func postVariantMessages(t *testing.T, g *Gateway, model string) *http.Response {
	t.Helper()
	body := []byte(`{
		"model":` + strconv.Quote(model) + `,
		"speed":"fast",
		"max_tokens":8,
		"metadata":{"speed":"nested-keep","trace":"keep"},
		"messages":[{"role":"user","content":"ping"}]
	}`)
	request, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer tok")
	request.Header.Set("Anthropic-Version", "2023-06-01")
	request.Header.Add(
		"Anthropic-Beta",
		variantOtherBeta+", "+requestprofile.AnthropicFastBeta,
	)
	request.Header.Set("X-Variant-Keep", "keep")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response
}

func variantAnthropicServer(
	t *testing.T,
	recorded chan<- variantUpstreamRequest,
	status int,
	effectiveSpeed string,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordVariantRequest(t, recorded, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 400 {
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"retry"}}`))
			return
		}
		usage := `"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0`
		if effectiveSpeed != "" {
			usage += `,"speed":` + strconv.Quote(effectiveSpeed)
		}
		_, _ = w.Write([]byte(`{
			"id":"msg_variant",
			"type":"message",
			"role":"assistant",
			"content":[{"type":"text","text":"ok"}],
			"model":"claude-opus-5",
			"usage":{` + usage + `}
		}`))
	}))
}

func recordVariantRequest(t *testing.T, recorded chan<- variantUpstreamRequest, r *http.Request) {
	t.Helper()
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read upstream body: %v", err)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Errorf("decode upstream body: %v", err)
		return
	}
	recorded <- variantUpstreamRequest{
		path:    r.URL.Path,
		body:    body,
		headers: r.Header.Clone(),
	}
}

func receiveVariantRequest(t *testing.T, recorded <-chan variantUpstreamRequest) variantUpstreamRequest {
	t.Helper()
	select {
	case request := <-recorded:
		return request
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not observed")
		return variantUpstreamRequest{}
	}
}

func assertVariantBetaTokens(t *testing.T, headers http.Header, wantFast bool) {
	t.Helper()
	beta := headers.Get("Anthropic-Beta")
	if !strings.Contains(beta, variantOtherBeta) {
		t.Fatalf("unrelated beta token was removed: %q", beta)
	}
	if gotFast := strings.Contains(beta, requestprofile.AnthropicFastBeta); gotFast != wantFast {
		t.Fatalf("fast beta presence = %t, want %t in %q", gotFast, wantFast, beta)
	}
	if headers.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("Anthropic-Version changed: %q", headers.Get("Anthropic-Version"))
	}
	if headers.Get("X-Variant-Keep") != "keep" {
		t.Fatalf("unrelated header changed: %q", headers.Get("X-Variant-Keep"))
	}
}
