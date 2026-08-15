package gateway

// TTFT waterfall timeout tests cover a per-attempt first-byte deadline that
// turns a slow upstream into a fallback_route fall-through instead of
// a frozen harness. All upstreams are hermetic httptest servers with
// controllable first-byte delay. Stalled handlers must drain the
// request body BEFORE blocking on the request context: net/http
// servers only watch for client disconnect once the body is consumed,
// so an undrained handler never sees the gateway's attempt cancel and
// httptest.Server.Close hangs. A release channel (closed before
// Server.Close by defer ordering) backstops the shutdown either way.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func ttftPost(t *testing.T, g *Gateway, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp, string(rb)
}

// TestTTFTDisabledByDefaultAllowsSlowFirstByte: with no ttft_timeout
// configured (the shipped default) a slow-to-first-byte upstream is
// waited out; the fallback never fires.
func TestTTFTDisabledByDefaultAllowsSlowFirstByte(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"SLOW-PRIMARY"}],"model":"m","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer primary.Close()
	var fbHits int32
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fbHits, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fb.Close()

	cfg := testConfig(t, primary.URL, fb.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, rb := ttftPost(t, g, `{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(rb, "SLOW-PRIMARY") {
		t.Fatalf("status=%d body=%s, want 200 from slow primary", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&fbHits); n != 0 {
		t.Fatalf("fallback hit %d times with ttft_timeout disabled, want 0", n)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].EffectiveProvider != "baseten" || valueOrZero(rows[0].Fallback.Trigger) != "" {
		t.Fatalf("row route_effective/fallback_trigger = %q/%q, want baseten/(empty)", rows[0].EffectiveProvider, valueOrZero(rows[0].Fallback.Trigger))
	}
}

// TestTTFTFiresFallbackBeforeFirstByteWithCooldown: the primary
// accepts the connection but never sends headers; the deadline
// abandons it, the fallback serves invisibly, telemetry records the
// trigger, and the 30s cooldown routes the next request straight to
// the fallback without touching the primary again.
func TestTTFTFiresFallbackBeforeFirstByteWithCooldown(t *testing.T) {
	var primaryHits int32
	release := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		_, _ = io.Copy(io.Discard, r.Body) // unread body blocks disconnect detection
		select {
		case <-r.Context().Done(): // gateway cancelled the attempt
		case <-release:
		}
	}))
	defer primary.Close()
	defer close(release) // LIFO: unblock handlers before Close waits on them
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-x","usage":{"input_tokens":2,"output_tokens":3}}`))
	}))
	defer fb.Close()

	cfg := testConfig(t, primary.URL, fb.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	rc.TTFTTimeout = 100 * time.Millisecond
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := `{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`
	resp, rb := ttftPost(t, g, body)
	if resp.StatusCode != 200 || !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("status=%d body=%s, want invisible 200 from fallback", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&primaryHits); n != 1 {
		t.Fatalf("primary hits = %d, want 1", n)
	}
	// Cooldown active: the second request must not re-try the primary.
	resp, rb = ttftPost(t, g, body)
	if resp.StatusCode != 200 || !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("cooldown request status=%d body=%s, want 200 from fallback", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&primaryHits); n != 1 {
		t.Fatalf("primary hit during cooldown: hits = %d, want 1", n)
	}

	rows := waitForRows(t, cfg.TelemetryDir, 2, 2*time.Second)
	if rows[0].EffectiveProvider != "anthropic" || valueOrZero(rows[0].Fallback.Trigger) != "ttft_timeout" {
		t.Fatalf("row 0 route_effective/fallback_trigger = %q/%q, want anthropic/ttft_timeout", rows[0].EffectiveProvider, valueOrZero(rows[0].Fallback.Trigger))
	}
	if rows[1].EffectiveProvider != "anthropic" ||
		!rows[1].Fallback.Attempted ||
		rows[1].Fallback.Count != 1 ||
		valueOrZero(rows[1].Fallback.Trigger) != fallbackTriggerCooldown {
		t.Fatalf("row 1 fallback telemetry = provider %q, %+v; want anthropic cooldown bypass",
			rows[1].EffectiveProvider, rows[1].Fallback)
	}
}

func TestAuthUnavailableBypassRecordsFallback(t *testing.T) {
	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if fallbackHits.Add(1) == 2 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid native credential"}}`))
			return
		}
		_, _ = w.Write([]byte(
			`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-opus-4-8","usage":{"input_tokens":2,"output_tokens":1}}`,
		))
	}))
	defer fallback.Close()

	cfg := testConfig(t, "http://baseten.invalid", fallback.URL)
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderrWriter
	defer func() {
		os.Stderr = oldStderr
		_ = stderrWriter.Close()
		_ = stderrReader.Close()
	}()

	resp, body := ttftPost(
		t,
		g,
		`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`,
	)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "FALLBACK") {
		t.Fatalf("status=%d body=%s, want fallback response", resp.StatusCode, body)
	}
	if got := resp.Header.Get(authUnavailableFallbackHeader); got != fallbackTriggerAuthUnavailable {
		t.Fatalf("%s = %q, want %q", authUnavailableFallbackHeader, got, fallbackTriggerAuthUnavailable)
	}

	resp, _ = ttftPost(
		t,
		g,
		`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping again"}]}`,
	)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second fallback status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get(authUnavailableFallbackHeader); got != fallbackTriggerAuthUnavailable {
		t.Fatalf("second %s = %q, want %q", authUnavailableFallbackHeader, got, fallbackTriggerAuthUnavailable)
	}

	rows := waitForRows(t, cfg.TelemetryDir, 2, 2*time.Second)
	for i, row := range rows {
		if row.EffectiveProvider != "anthropic" ||
			!row.Fallback.Attempted ||
			row.Fallback.Count != 1 ||
			valueOrZero(row.Fallback.Trigger) != fallbackTriggerAuthUnavailable {
			t.Fatalf("row %d fallback telemetry = provider %q, %+v; want anthropic auth-unavailable bypass",
				i, row.EffectiveProvider, row.Fallback)
		}
	}

	statusResp, err := http.Get(adminURL(g, "/v1/admin/status"))
	if err != nil {
		t.Fatal(err)
	}
	defer statusResp.Body.Close()
	var status struct {
		AuthUnavailableFallback struct {
			Count      uint64 `json:"count"`
			LastAt     string `json:"last_at"`
			LastClient string `json:"last_client"`
			LastRoute  string `json:"last_route"`
		} `json:"auth_unavailable_fallback"`
	}
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	gotStatus := status.AuthUnavailableFallback
	if gotStatus.Count != 2 || gotStatus.LastAt == "" ||
		gotStatus.LastClient != "claude-code" || gotStatus.LastRoute != "anthropic" {
		t.Fatalf("auth_unavailable_fallback status = %+v", gotStatus)
	}

	os.Stderr = oldStderr
	if err := stderrWriter.Close(); err != nil {
		t.Fatal(err)
	}
	stderr, err := io.ReadAll(stderrReader)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"fallback client=claude-code",
		"trigger=auth_unavailable",
		"serving anthropic count=1",
		"serving anthropic count=2",
	} {
		if !strings.Contains(string(stderr), want) {
			t.Fatalf("stderr missing %q, got:\n%s", want, stderr)
		}
	}
}

// TestTTFTHeadersWithoutBodyFiresFallback: an SSE upstream that sends
// the status line and headers but stalls before the first body byte is
// the harness-freezing case; the deadline covers it too.
func TestTTFTHeadersWithoutBodyFiresFallback(t *testing.T) {
	release := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		// Headers out, body stalled.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer primary.Close()
	defer close(release)
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer fb.Close()

	cfg := testConfig(t, primary.URL, fb.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	rc.TTFTTimeout = 100 * time.Millisecond
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, rb := ttftPost(t, g, `{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("status=%d body=%s, want 200 from fallback", resp.StatusCode, rb)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].EffectiveProvider != "anthropic" || valueOrZero(rows[0].Fallback.Trigger) != "ttft_timeout" {
		t.Fatalf("route_effective/fallback_trigger = %q/%q, want anthropic/ttft_timeout", rows[0].EffectiveProvider, valueOrZero(rows[0].Fallback.Trigger))
	}
}

// TestTTFTInertAfterFirstByte: once the first response byte has
// arrived the deadline must never truncate the stream, even when a
// later inter-chunk gap exceeds it.
func TestTTFTInertAfterFirstByte(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"FIRST-CHUNK\"}}\n\n"))
		fl.Flush()
		time.Sleep(400 * time.Millisecond) // longer than the deadline
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\",\"marker\":\"LAST-CHUNK\"}\n\n"))
		fl.Flush()
	}))
	defer primary.Close()
	var fbHits int32
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fbHits, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fb.Close()

	cfg := testConfig(t, primary.URL, fb.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	rc.TTFTTimeout = 120 * time.Millisecond
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, rb := ttftPost(t, g, `{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(rb, "FIRST-CHUNK") || !strings.Contains(rb, "LAST-CHUNK") {
		t.Fatalf("stream truncated after first byte: %s", rb)
	}
	if n := atomic.LoadInt32(&fbHits); n != 0 {
		t.Fatalf("fallback hit %d times, want 0 (deadline inert after first byte)", n)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].StatusCode() != 200 || valueOrZero(rows[0].Fallback.Trigger) != "" {
		t.Fatalf("row status/fallback_trigger = %d/%q, want 200/(empty)", rows[0].StatusCode(), valueOrZero(rows[0].Fallback.Trigger))
	}
}

func TestOpenAIChatCompletionsCapturesUsageAndFirstOutput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte(
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"zai-org/GLM-5.2\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		))
		flusher.Flush()
		time.Sleep(25 * time.Millisecond)
		_, _ = w.Write([]byte(
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"zai-org/GLM-5.2\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n",
		))
		_, _ = w.Write([]byte(
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"zai-org/GLM-5.2\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n",
		))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, upstream.URL)
	client := resolvedOpenAIBaseten(t, "opencode", "baseten")
	client.DefaultModel = "moonshotai/Kimi-K2.7-Code"
	g, adminL, _ := newGateway(t, cfg, client)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	req, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, "opencode", "/v1/chat/completions"),
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	row := rows[0]
	if !row.UsageComplete ||
		valueOrZero(row.Usage.InputTokens) != 12 ||
		valueOrZero(row.Usage.OutputTokens) != 3 {
		t.Fatalf("OpenAI usage = complete %v, %+v", row.UsageComplete, row.Usage)
	}
	if row.TTFTMS == nil || *row.TTFTMS < 20 {
		t.Fatalf("OpenAI TTFT = %v, want at least 20ms", row.TTFTMS)
	}
}

// TestTTFTExplicitAliasExpiryLoudError: an explicitly chosen alias is
// a single attempt with no fallback (the model-discovery contract), so a
// TTFT expiry surfaces as the gateway's upstream-timeout 504 naming
// the model and the deadline, never a silent reroute.
func TestTTFTExplicitAliasExpiryLoudError(t *testing.T) {
	var primaryHits int32
	release := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer primary.Close()
	defer close(release)
	var fbHits int32
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fbHits, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fb.Close()

	cfg := testConfig(t, primary.URL, fb.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	rc.TTFTTimeout = 100 * time.Millisecond
	rc.ModelAliases = map[string]string{"claude-baseten-glm": "zai-org/GLM-5.2"}
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, rb := ttftPost(t, g, `{"model":"claude-baseten-glm","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if resp.StatusCode != 504 {
		t.Fatalf("status = %d body=%s, want 504", resp.StatusCode, rb)
	}
	for _, want := range []string{"zai-org/GLM-5.2", "ttft_timeout", "100ms"} {
		if !strings.Contains(rb, want) {
			t.Fatalf("504 body %q does not name %q", rb, want)
		}
	}
	if n := atomic.LoadInt32(&fbHits); n != 0 {
		t.Fatalf("fallback hit %d times for an explicit alias, want 0", n)
	}
	if n := atomic.LoadInt32(&primaryHits); n != 1 {
		t.Fatalf("primary hits = %d, want 1", n)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].StatusCode() != 504 || !rows[0].IsHTTPError() || valueOrZero(rows[0].Fallback.Trigger) != "ttft_timeout" {
		t.Fatalf("row status/errored/fallback_trigger = %d/%v/%q, want 504/true/ttft_timeout", rows[0].StatusCode(), rows[0].IsHTTPError(), valueOrZero(rows[0].Fallback.Trigger))
	}
	if rows[0].ServedModel != "zai-org/GLM-5.2" {
		t.Fatalf("row upstream_model = %q, want zai-org/GLM-5.2", rows[0].ServedModel)
	}
}

// TestResolveFromFileTTFTOverrides: per-client ttft_timeout beats the
// global one; an explicit client "0" disables even when the global
// deadline is set; absent inherits.
func TestResolveFromFileTTFTOverrides(t *testing.T) {
	enabled := true
	f := &config.File{
		Global: config.Global{
			RoutingEnabled: &enabled,
			TTFTTimeout:    "45s",
		},
		Clients: []config.Client{
			{Name: "inherits", Enabled: true, BindAddr: "127.0.0.1:0", ProtocolShape: "anthropic", DefaultModel: "zai-org/GLM-5.2"},
			{Name: "overrides", Enabled: true, BindAddr: "127.0.0.1:0", ProtocolShape: "anthropic", DefaultModel: "zai-org/GLM-5.2", TTFTTimeout: "200ms"},
			{Name: "disables", Enabled: true, BindAddr: "127.0.0.1:0", ProtocolShape: "anthropic", DefaultModel: "zai-org/GLM-5.2", TTFTTimeout: "0"},
		},
	}
	got, err := resolveFromFile(f)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Duration{
		"inherits":  45 * time.Second,
		"overrides": 200 * time.Millisecond,
		"disables":  0,
	}
	if len(got) != len(want) {
		t.Fatalf("resolved %d clients, want %d", len(got), len(want))
	}
	for _, rc := range got {
		if rc.TTFTTimeout != want[rc.Name] {
			t.Errorf("client %s ttft = %s, want %s", rc.Name, rc.TTFTTimeout, want[rc.Name])
		}
	}
}

// TestResolveFromFileTTFTParseErrors: malformed or negative
// ttft_timeout values refuse the config with the offending field
// named, never a silent disable.
func TestResolveFromFileTTFTParseErrors(t *testing.T) {
	enabled := true
	global := func(timeout string) config.Global {
		return config.Global{
			RoutingEnabled: &enabled,
			TTFTTimeout:    timeout,
		}
	}
	cases := []struct {
		name    string
		file    *config.File
		wantSub string
	}{
		{
			name:    "global malformed",
			file:    &config.File{Global: global("banana")},
			wantSub: "global.ttft_timeout",
		},
		{
			name: "client malformed",
			file: &config.File{Global: global(""), Clients: []config.Client{
				{Name: "cc", Enabled: true, BindAddr: "127.0.0.1:0", ProtocolShape: "anthropic", DefaultModel: "zai-org/GLM-5.2", TTFTTimeout: "fast"},
			}},
			wantSub: `client "cc" ttft_timeout`,
		},
		{
			name: "client negative",
			file: &config.File{Global: global(""), Clients: []config.Client{
				{Name: "cc", Enabled: true, BindAddr: "127.0.0.1:0", ProtocolShape: "anthropic", DefaultModel: "zai-org/GLM-5.2", TTFTTimeout: "-5s"},
			}},
			wantSub: "negative",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveFromFile(tc.file)
			if err == nil {
				t.Fatalf("resolveFromFile accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}
