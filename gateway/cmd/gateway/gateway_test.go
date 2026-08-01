package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

func testConfig(t *testing.T, upstreamBaseten, upstreamAnthropic string) Config {
	t.Helper()
	t.Setenv("BASETEN_SWITCH_AUTH_NO_KEYRING", "1")
	t.Setenv("BASETEN_SWITCH_AUTH_FILE", filepath.Join(t.TempDir(), "auth.json"))
	return Config{
		TelemetryDir:   filepath.Join(t.TempDir(), "telemetry"),
		PidFile:        filepath.Join(t.TempDir(), "g.pid"),
		BasetenURL:     upstreamBaseten,
		AnthropicURL:   upstreamAnthropic,
		BasetenKey:     "bas-key",
		OAuthProfile:   "default",
		OAuthHost:      "https://api.baseten.co",
		APIKeyFallback: true,
	}
}

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

// resolvedAnthropicBaseten returns a generic resolvedClientConfig for an
// anthropic-shape listener pointed at baseten with the shipped default
// model so rewrite resolves "claude-opus-4-8" -> "zai-org/GLM-5.2".
// Generic gateway tests opt into Follow Harness so they exercise their
// intended behavior independently of the GLM compatibility default.
func resolvedAnthropicBaseten(t *testing.T) resolvedClientConfig {
	t.Helper()
	return resolvedClientConfig{
		Name:                 "claude-code",
		BindAddr:             "127.0.0.1:0",
		ProtocolShape:        "anthropic",
		Route:                "baseten",
		GlobalRoutingEnabled: true,
		DefaultModel:         "zai-org/GLM-5.2",
		ModelOptions: config.ModelOptions{
			pricing.ProviderBaseten: {
				"zai-org/GLM-5.2": {
					Reasoning: &config.ReasoningPolicy{
						Mode: config.ReasoningFollowHarness,
					},
				},
			},
		},
	}
}

func resolvedOpenAIBaseten(t *testing.T, name, route string) resolvedClientConfig {
	t.Helper()
	return resolvedClientConfig{
		Name:                 name,
		BindAddr:             "127.0.0.1:0",
		ProtocolShape:        "openai",
		Route:                route,
		GlobalRoutingEnabled: route == "baseten",
		DefaultModel:         "zai-org/GLM-5.2",
	}
}

func TestValidateAdminAddrLoopback(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:8787",
		"[::1]:8787",
		"localhost:8787",
		"LOCALHOST:8787",
	} {
		if err := validateAdminAddrLoopback(addr); err != nil {
			t.Errorf("validateAdminAddrLoopback(%q) = %v", addr, err)
		}
	}
	for _, addr := range []string{
		"0.0.0.0:8787",
		"[::]:8787",
		"192.168.1.20:8787",
		"gateway.example:8787",
		":8787",
		"127.0.0.1",
	} {
		if err := validateAdminAddrLoopback(addr); err == nil {
			t.Errorf("validateAdminAddrLoopback(%q) succeeded, want rejection", addr)
		}
	}
}

func TestStableAdminPortContract(t *testing.T) {
	if DefaultPort != 45273 {
		t.Fatalf("DefaultPort = %d, want 45273", DefaultPort)
	}
	if DefaultAdminAddr != "127.0.0.1:45273" {
		t.Fatalf("DefaultAdminAddr = %q, want 127.0.0.1:45273", DefaultAdminAddr)
	}
}

func TestResolveAttemptsMarksAuthUnavailableFallback(t *testing.T) {
	cfg := testConfig(t, "http://baseten.invalid", "http://anthropic.invalid")
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	g := &Gateway{
		cfg:     cfg,
		pricing: pricing.New(),
		client:  &http.Client{Transport: defaultTransport()},
	}
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	cl := &clientListener{cfg: rc}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	attempts, err := g.resolveAttemptsLadder(
		cl,
		req,
		[]byte(`{"model":"claude-opus-4-8"}`),
		"messages",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d, want 1", len(attempts))
	}
	attempt := attempts[0]
	if attempt.route != "anthropic" ||
		attempt.fallbackCount != 1 ||
		attempt.fallbackTrigger != fallbackTriggerAuthUnavailable {
		t.Fatalf("fallback attempt = route %q count %d trigger %q, want anthropic/1/%s",
			attempt.route,
			attempt.fallbackCount,
			attempt.fallbackTrigger,
			fallbackTriggerAuthUnavailable,
		)
	}
}

func newGateway(t *testing.T, cfg Config, resolved ...resolvedClientConfig) (*Gateway, net.Listener, []net.Listener) {
	t.Helper()
	adminL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg, pricing.New(), adminL, resolved)
	if err != nil {
		t.Fatal(err)
	}
	clientListeners := make([]net.Listener, 0, len(resolved))
	for _, rc := range resolved {
		addr := g.ClientAddr(rc.Name)
		if addr == nil {
			t.Fatalf("listener for %q not bound", rc.Name)
		}
		clientListeners = append(clientListeners, listenFromAddr(t, addr))
	}
	return g, adminL, clientListeners
}

func listenFromAddr(t *testing.T, addr net.Addr) net.Listener {
	// We don't own the listener; wrap a fake net.Listener that
	// reports the live address. Tests use g.ClientAddr for the
	// real host:port and never Close it directly.
	return &addrOnlyListener{addr: addr.(*net.TCPAddr)}
}

type addrOnlyListener struct {
	addr *net.TCPAddr
}

func (a *addrOnlyListener) Accept() (net.Conn, error) { return nil, nil }
func (a *addrOnlyListener) Close() error              { return nil }
func (a *addrOnlyListener) Addr() net.Addr            { return a.addr }

func start(t *testing.T, g *Gateway) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = g.Serve(ctx)
		close(done)
	}()
	stopped := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	pollHealthz(t, g)
	return stopped
}

func pollHealthz(t *testing.T, g *Gateway) {
	t.Helper()
	addr := g.AdminAddr().(*net.TCPAddr)
	url := "http://" + addr.String() + "/healthz"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && strings.Contains(string(body), `"ok"`) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("gateway did not become healthy")
}

func adminURL(g *Gateway, path string) string {
	addr := g.AdminAddr().(*net.TCPAddr)
	return "http://" + addr.String() + path
}

func clientURL(g *Gateway, name, path string) string {
	addr := g.ClientAddr(name).(*net.TCPAddr)
	return "http://" + addr.String() + path
}

func valueOrZero[T any](value *T) T {
	if value == nil {
		var zero T
		return zero
	}
	return *value
}

func waitForRows(
	t *testing.T,
	telemetryDir string,
	want int,
	dur time.Duration,
) []telemetry.EventV1 {
	t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		events := telemetryEventsFromSegments(t, telemetryDir)
		if len(events) >= want {
			return events
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf(
		"telemetry events did not reach %d within %s (got %d)",
		want,
		dur,
		len(telemetryEventsFromSegments(t, telemetryDir)),
	)
	return nil
}

func waitForEvents(
	t *testing.T,
	telemetryDir string,
	want int,
	dur time.Duration,
) []telemetry.EventV1 {
	t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		events := telemetryEventsFromSegments(t, telemetryDir)
		if len(events) >= want {
			return events
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf(
		"telemetry events did not reach %d within %s (got %d)",
		want,
		dur,
		len(telemetryEventsFromSegments(t, telemetryDir)),
	)
	return nil
}

func telemetryEventsFromSegments(t *testing.T, dir string) []telemetry.EventV1 {
	t.Helper()
	segments, err := telemetry.DiscoverSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	var events []telemetry.EventV1
	for _, segment := range segments {
		file, err := os.Open(segment.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(file)
		for {
			line, readErr := reader.ReadBytes('\n')
			if len(line) > 0 && line[len(line)-1] == '\n' {
				var event telemetry.EventV1
				if err := json.Unmarshal(line, &event); err != nil {
					_ = file.Close()
					t.Fatalf("decode telemetry segment %s: %v", segment.Name, err)
				}
				events = append(events, event)
			}
			if readErr != nil {
				if readErr != io.EOF {
					_ = file.Close()
					t.Fatalf("read telemetry segment %s: %v", segment.Name, readErr)
				}
				break
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return events
}

func TestTelemetryEventsFromSegmentsReadsV1AndIgnoresPartialTail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "telemetry")
	event := gatewayTelemetryEvent(1)
	status := http.StatusGatewayTimeout
	trigger := "ttft_timeout"
	subagentModel := "anthropic-baseten-kimi"
	event.Client = "claude-code"
	event.ConfiguredRoute = "baseten"
	event.EffectiveProvider = "anthropic"
	event.RequestedModel = "claude-opus-4-8"
	event.ServedModel = "zai-org/GLM-5.2"
	event.Status = &status
	event.Fallback.Trigger = &trigger
	event.Subagent = true
	event.SubagentModel = &subagentModel

	writer, err := telemetry.NewWriter(telemetry.WriterOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(event); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	segments, err := telemetry.DiscoverSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(segments))
	}
	file, err := os.OpenFile(segments[0].Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"schema_version":1`); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	events := telemetryEventsFromSegments(t, dir)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0]
	if got.ConfiguredRoute != "baseten" ||
		got.EffectiveProvider != "anthropic" ||
		got.StatusCode() != http.StatusGatewayTimeout ||
		!got.IsHTTPError() ||
		got.Fallback.Trigger == nil || *got.Fallback.Trigger != trigger ||
		got.SubagentModel == nil || *got.SubagentModel != subagentModel {
		t.Fatalf("event = %+v", got)
	}
}

func TestCountTokensSynthetic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello world this is a test message"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages/count_tokens?beta=true"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var ct map[string]int
	if err := json.Unmarshal(b, &ct); err != nil {
		t.Fatalf("bad body %s: %v", b, err)
	}
	if ct["input_tokens"] < 1 {
		t.Fatalf("input_tokens = %d", ct["input_tokens"])
	}
}

func TestPostMessagesBasetenForwardsRewrittenModel(t *testing.T) {
	gotModel := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Api-Key bas-key" {
			t.Errorf("bad upstream auth: %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		gotModel <- fmtString(m["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"model":"zai-org/GLM-5.2","usage":{"input_tokens":10,"output_tokens":1}}`))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	rb, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rb), "PONG") {
		t.Fatalf("missing PONG in body: %s", rb)
	}
	select {
	case m := <-gotModel:
		if m != "zai-org/GLM-5.2" {
			t.Fatalf("upstream got model %q, want zai-org/GLM-5.2", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received model")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].ConfiguredRoute != "baseten" || rows[0].ServedModel != "zai-org/GLM-5.2" {
		t.Fatalf("telemetry row mismatch: %+v", rows[0])
	}
	if valueOrZero(rows[0].Usage.InputTokens) != 10 || valueOrZero(rows[0].Usage.OutputTokens) != 1 {
		t.Fatalf("telemetry tokens mismatch: %+v", rows[0])
	}
	if rows[0].Client != "claude-code" {
		t.Fatalf("telemetry client = %q, want claude-code", rows[0].Client)
	}
}

func TestPostMessagesBasetenSSERelaysAndParsesUsage(t *testing.T) {
	sseBody := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n" +
		"event: message_delta\ndata: {\"usage\":{\"input_tokens\":42,\"output_tokens\":3,\"cache_read_input_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(sseBody))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-sonnet-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	rb, _ := io.ReadAll(resp.Body)
	// Bug A: the baseten anthropic-shape SSE relay de-double-counts cached
	// tokens per event, so the message_delta input_tokens (42, inclusive of
	// the 5 cache reads) reaches the client as the exclusive 37. Framing and
	// the message_start event (no cache fields) are otherwise untouched.
	if !strings.Contains(string(rb), "message_delta") || !strings.Contains(string(rb), "input_tokens\":37") {
		t.Fatalf("SSE usage not normalized in relay: %q", rb)
	}
	if strings.Contains(string(rb), "input_tokens\":42") {
		t.Fatalf("inclusive input_tokens leaked to client: %q", rb)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if valueOrZero(rows[0].Usage.InputTokens) != 37 || valueOrZero(rows[0].Usage.OutputTokens) != 3 || valueOrZero(rows[0].Usage.CacheReadInputTokens) != 5 {
		t.Fatalf("SSE usage parsed wrong (want normalized input 37): %+v", rows[0])
	}
	if !rows[0].IsStream {
		t.Fatal("is_stream should be true")
	}
}

// TestPostMessagesBasetenNormalizesInclusiveInput is the non-streaming half of
// Bug A: Baseten's anthropic-shape endpoint reports input_tokens inclusive of
// cache_read_input_tokens. The baseten-route relay must de-double-count so the
// client (and telemetry) see the exclusive input, with Content-Length adjusted
// to the rewritten body. Native passthrough (route=anthropic) is unaffected.
func TestPostMessagesBasetenNormalizesInclusiveInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"model":"zai-org/GLM-5.2","usage":{"input_tokens":46292,"output_tokens":16,"cache_read_input_tokens":46272}}`))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	rb, _ := io.ReadAll(resp.Body)
	// 46292 - 46272 = 20 exclusive input tokens reach the client.
	if !strings.Contains(string(rb), "\"input_tokens\":20") {
		t.Fatalf("input_tokens not normalized in body: %s", rb)
	}
	if strings.Contains(string(rb), "46292") {
		t.Fatalf("inclusive input_tokens leaked to client: %s", rb)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" && cl != itoa(len(rb)) {
		t.Fatalf("Content-Length %q does not match rewritten body length %d", cl, len(rb))
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if valueOrZero(rows[0].Usage.InputTokens) != 20 || valueOrZero(rows[0].Usage.CacheReadInputTokens) != 46272 {
		t.Fatalf("telemetry not normalized (want input 20, cache_read 46272): %+v", rows[0])
	}
}

func TestHEADReturnsBodylessResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, err := http.Head(clientURL(g, "claude-code", "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", resp.Header.Get("Content-Type"))
	}
	if resp.ContentLength != 0 {
		t.Fatalf("content-length = %d, want 0", resp.ContentLength)
	}
	b, _ := io.ReadAll(resp.Body)
	if len(b) != 0 {
		t.Fatalf("HEAD should have no body, got %d bytes", len(b))
	}
}

func TestGracefulShutdownWithin3s(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Serve(ctx) }()
	pollHealthz(t, g)

	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	start := time.Now()
	shutdownCtx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer scancel()
	if err := g.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("shutdown took %v, want < 3s", elapsed)
	}
}

func TestHealthzClientReturnsEffectiveRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, err := http.Get(clientURL(g, "claude-code", "/healthz"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var h map[string]any
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatalf("bad healthz body %s: %v", b, err)
	}
	if h["effective_route"] != "baseten" || h["upstream_model"] != "zai-org/GLM-5.2" || h["status"] != "ok" {
		t.Fatalf("healthz mismatch: %+v", h)
	}
	if _, ok := h["route"]; ok {
		t.Fatalf("healthz unexpectedly exposes route: %+v", h)
	}
	if h["client"] != "claude-code" {
		t.Fatalf("healthz client = %v want claude-code", h["client"])
	}
}

func fmtString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestBasetenRouteRejectsNeedsLoginWhenNoProfileAndNoFallback(t *testing.T) {
	upstreamHit := false
	var hitMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitMu.Lock()
		upstreamHit = true
		hitMu.Unlock()
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.APIKeyFallback = false
	cfg.BasetenKey = "bas-key"
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("got %d want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Baseten-Switch"); got != "needs-login" {
		t.Fatalf("X-Baseten-Switch = %q, want needs-login", got)
	}
	if upstreamHit {
		t.Fatal("upstream must not be hit on needs-login rejection")
	}
}

func TestBasetenRouteAPIKeyFallbackForwardsAPIKeyHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"zai-org/GLM-5.2","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.APIKeyFallback = true
	cfg.BasetenKey = "fallback-key"
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	if gotAuth != "Api-Key fallback-key" {
		t.Fatalf("upstream Authorization = %q, want Api-Key fallback-key", gotAuth)
	}
}

func TestBasetenCLIAPIKeyProfileAuthenticatesGatewaySurfaces(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	profileKey := "synthetic-profile-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Api-Key "+profileKey {
			t.Errorf("%s Authorization = %q, want selected profile API key", r.URL.Path, got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("%s forwarded inbound X-Api-Key = %q", r.URL.Path, got)
		}
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages":
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"example/model","usage":{"input_tokens":1,"output_tokens":1}}`)
		case "/v1/responses":
			_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig(t, server.URL, server.URL)
	cfg.BasetenKey = "environment-key-must-not-win"
	cfg.APIKeyFallback = true
	cfg.OAuthProfile = "p"
	writeAPIKeyProfile(t, profileKey)
	claude := resolvedAnthropicBaseten(t)
	codex := resolvedOpenAIBaseten(t, "codex", "baseten")
	g, adminL, _ := newGateway(t, cfg, claude, codex)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	requests := []struct {
		method string
		url    string
		body   string
	}{
		{http.MethodPost, clientURL(g, claude.Name, "/v1/messages"), `{"model":"alias","messages":[{"role":"user","content":"ping"}]}`},
		{http.MethodPost, clientURL(g, codex.Name, "/v1/responses"), `{"model":"alias","input":"ping"}`},
		{http.MethodGet, clientURL(g, codex.Name, "/v1/models"), ""},
	}
	for _, test := range requests {
		req, err := http.NewRequest(test.method, test.url, strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer harness-secret")
		req.Header.Set("X-Api-Key", "harness-api-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s status = %d", test.method, test.url, resp.StatusCode)
		}
	}

	resp, err := http.Get(adminURL(g, "/v1/admin/auth/status"))
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if status["signed_in"] != true || status["auth_type"] != "api_key" || status["fallback_in_use"] != false {
		t.Fatalf("auth status = %+v", status)
	}
	encoded, _ := json.Marshal(status)
	if bytes.Contains(encoded, []byte(profileKey)) {
		t.Fatal("admin auth status leaked selected API key")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/models"} {
		if seen[path] == 0 {
			t.Errorf("upstream did not receive %s", path)
		}
	}
}

func TestBasetenAuthorizationFailuresDoNotPromoteNativeFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var nativeHits atomic.Int32
			baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"authorization failed"}`)
			}))
			defer baseten.Close()
			native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nativeHits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer native.Close()

			cfg := testConfig(t, baseten.URL, native.URL)
			cfg.BasetenKey = ""
			cfg.APIKeyFallback = false
			cfg.OAuthProfile = "p"
			writeAPIKeyProfile(t, "synthetic-restricted-key")
			rc := resolvedAnthropicBaseten(t)
			rc.FallbackRoute = "anthropic"
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			req, _ := http.NewRequest(http.MethodPost, clientURL(g, rc.Name, "/v1/messages"), strings.NewReader(`{"model":"alias","messages":[]}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, status)
			}
			if nativeHits.Load() != 0 {
				t.Fatalf("native fallback received %d requests", nativeHits.Load())
			}
		})
	}
}

func TestAdminAuthStatusReportsNotSignedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, err := http.Get(adminURL(g, "/v1/admin/auth/status"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("bad status body %s: %v", b, err)
	}
	if st["signed_in"] != false {
		t.Fatalf("signed_in = %v, want false", st["signed_in"])
	}
	if st["profile"] != "default" {
		t.Fatalf("profile = %v, want default", st["profile"])
	}
	if st["fallback_enabled"] != false {
		t.Fatalf("fallback_enabled = %v, want false", st["fallback_enabled"])
	}
}

// TestAdminStatusReportsPerListenerClients verifies the new
// per-listener fields on /v1/admin/status: name, bind_addr,
// protocol_shape, route, native_route, enabled, currently_bound, and
// unmatched_native_model. native_route is the server-owned shape-to-native
// resolution (config.NativeRoute) so UI consumers do not re-implement it.
func TestAdminStatusReportsPerListenerClients(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")

	bas := resolvedAnthropicBaseten(t)
	ant := resolvedClientConfig{
		Name:          "cursor",
		BindAddr:      "127.0.0.1:0",
		ProtocolShape: "anthropic",
		Route:         "anthropic",
	}
	oai := resolvedOpenAIBaseten(t, "codex", "baseten")
	writeGatewayYAML(t, cfg.ConfigPath, []resolvedClientConfig{bas, ant, oai})
	g, adminL, _ := newGateway(t, cfg, bas, ant, oai)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, err := http.Get(adminURL(g, "/v1/admin/status"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var st struct {
		Clients []struct {
			Name           string `json:"name"`
			ProtocolShape  string `json:"protocol_shape"`
			NativeRoute    string `json:"native_route"`
			CurrentlyBound bool   `json:"currently_bound"`
			Unmatched      struct {
				EffectiveModel string `json:"effective_model"`
			} `json:"unmatched_native_model"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("bad status body %s: %v", b, err)
	}
	if len(st.Clients) != 3 {
		t.Fatalf("status returned %d clients, want 3: %s", len(st.Clients), b)
	}
	cc := st.Clients[0]
	if cc.Name != "claude-code" || cc.ProtocolShape != "anthropic" || !cc.CurrentlyBound {
		t.Fatalf("client status mismatch: %+v", cc)
	}
	if cc.Unmatched.EffectiveModel != "zai-org/GLM-5.2" {
		t.Fatalf("claude-code unmatched effective_model = %q, want zai-org/GLM-5.2", cc.Unmatched.EffectiveModel)
	}
	if cc.NativeRoute != "anthropic" {
		t.Fatalf("claude-code native_route = %q, want anthropic", cc.NativeRoute)
	}
	ant2 := st.Clients[1]
	if ant2.Name != "cursor" {
		t.Fatalf("cursor status mismatch: %+v", ant2)
	}
	if ant2.Unmatched.EffectiveModel != "" {
		t.Fatalf("cursor unmatched effective_model = %q, want empty (non-baseten route)", ant2.Unmatched.EffectiveModel)
	}
	if ant2.NativeRoute != "anthropic" {
		t.Fatalf("cursor native_route = %q, want anthropic", ant2.NativeRoute)
	}
	ox := st.Clients[2]
	if ox.Name != "codex" || ox.ProtocolShape != "openai" {
		t.Fatalf("codex status mismatch: %+v", ox)
	}
	if ox.NativeRoute != "openai" {
		t.Fatalf("codex native_route = %q, want openai (openai shape)", ox.NativeRoute)
	}
}

// TestOpenAIShapeBasetenForwardsChatCompletions verifies an
// openai-shape listener forwards /v1/chat/completions to the baseten
// upstream, model rewriting applies, and telemetry carries the
// listener name.
func TestOpenAIShapeBasetenForwardsChatCompletions(t *testing.T) {
	gotModel := make(chan string, 1)
	gotPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		gotModel <- fmtString(m["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"zai-org/GLM-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedOpenAIBaseten(t, "opencode", "baseten")
	rc.DefaultModel = reasoningNeutralBasetenModel
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "opencode", "/v1/chat/completions"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	select {
	case p := <-gotPath:
		if p != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never hit")
	}
	select {
	case m := <-gotModel:
		if m != reasoningNeutralBasetenModel {
			t.Fatalf("upstream got model %q, want %s", m, reasoningNeutralBasetenModel)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received model")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].ConfiguredRoute != "baseten" {
		t.Fatalf("route = %q want baseten", rows[0].ConfiguredRoute)
	}
	if rows[0].Client != "opencode" {
		t.Fatalf("client = %q want opencode", rows[0].Client)
	}
}

// TestTwoClientListenersInOneGateway verifies that two per-client
// listeners (one anthropic+baseten, one openai+baseten) coexist in a
// single Gateway and each forwards to its own upstream mock.
func TestTwoClientListenersInOneGateway(t *testing.T) {
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			_, _ = w.Write([]byte(`{"id":"cc","object":"chat.completion","choices":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message"}`))
	}))
	defer basSrv.Close()

	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ant","type":"message"}`))
	}))
	defer antSrv.Close()

	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	antRc := resolvedAnthropicBaseten(t)
	antRc.Name = "claude-code"
	antRc.Route = "anthropic" // passthrough to antSrv
	antRc.ProtocolShape = "anthropic"

	oaiRc := resolvedOpenAIBaseten(t, "opencode", "baseten")
	oaiRc.DefaultModel = reasoningNeutralBasetenModel
	// Point baseten upstream at the same mock server; the openai
	// listener must hit /v1/chat/completions on the baseten mock.

	g, adminL, _ := newGateway(t, cfg, antRc, oaiRc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// anthropic listener -> anthropic upstream, passthrough auth.
	antReq, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader([]byte(`{"model":"claude-sonnet-4-5","messages":[]}`)))
	antReq.Header.Set("Content-Type", "application/json")
	antReq.Header.Set("Authorization", "Bearer ant-tok")
	antResp, err := http.DefaultClient.Do(antReq)
	if err != nil {
		t.Fatal(err)
	}
	antResp.Body.Close()
	if antResp.StatusCode != 200 {
		t.Fatalf("anthropic listener got %d want 200", antResp.StatusCode)
	}

	// openai listener -> baseten upstream, model rewrite + api key.
	oaiReq, _ := http.NewRequest("POST", clientURL(g, "opencode", "/v1/chat/completions"),
		bytes.NewReader([]byte(`{"model":"claude-opus-4-8","messages":[]}`)))
	oaiReq.Header.Set("Content-Type", "application/json")
	oaiReq.Header.Set("Authorization", "Bearer tok")
	oaiResp, err := http.DefaultClient.Do(oaiReq)
	if err != nil {
		t.Fatal(err)
	}
	oaiResp.Body.Close()
	if oaiResp.StatusCode != 200 {
		t.Fatalf("openai listener got %d want 200", oaiResp.StatusCode)
	}

	antRows := 0
	oaiRows := 0
	rows := waitForRows(t, cfg.TelemetryDir, 2, 2*time.Second)
	for _, r := range rows {
		switch r.Client {
		case "claude-code":
			antRows++
		case "opencode":
			oaiRows++
		}
	}
	if antRows == 0 {
		t.Fatalf("no telemetry rows for claude-code in %+v", rows)
	}
	if oaiRows == 0 {
		t.Fatalf("no telemetry rows for opencode in %+v", rows)
	}
}

// TestCrossShapeReturns501 verifies that an anthropic port with
// route=openai returns 501 (cross-shape translation not implemented).
// TestAnthropicListenerOpenAIRouteTranslates: the combo that returned
// 501 pre-WS2 now translates the request to chat.completions and the
// response back to the anthropic shape.
func TestAnthropicListenerOpenAIRouteTranslates(t *testing.T) {
	gotBody := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody <- b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-x","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"PONG"}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.OpenAIURL = srv.URL
	rc := resolvedAnthropicBaseten(t)
	rc.Route = "openai"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-x","max_tokens":16,"system":"be brief","messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		rb, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d want 200: %s", resp.StatusCode, rb)
	}
	var up map[string]interface{}
	select {
	case b := <-gotBody:
		if err := json.Unmarshal(b, &up); err != nil {
			t.Fatalf("upstream body not json: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never hit")
	}
	msgs := up["messages"].([]interface{})
	first := msgs[0].(map[string]interface{})
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Fatalf("system prompt not converted: %v", msgs)
	}
	rb, _ := io.ReadAll(resp.Body)
	var am map[string]interface{}
	if err := json.Unmarshal(rb, &am); err != nil {
		t.Fatalf("client body not json: %v", err)
	}
	if am["type"] != "message" || am["stop_reason"] != "end_turn" {
		t.Fatalf("response not anthropic-shaped: %s", rb)
	}
	content := am["content"].([]interface{})
	if content[0].(map[string]interface{})["text"] != "PONG" {
		t.Fatalf("text lost in translation: %s", rb)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if !rows[0].Translated || valueOrZero(rows[0].Usage.InputTokens) != 7 || valueOrZero(rows[0].Usage.OutputTokens) != 2 {
		t.Fatalf("telemetry row wrong: %+v", rows[0])
	}
}

// OpenAI port with route=anthropic is also cross-shape and must 501.
func TestCrossShapeOpenAIPortAnthropicRouteReturns501(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedOpenAIBaseten(t, "opencode", "anthropic")
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-4o","messages":[]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "opencode", "/v1/chat/completions"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 501 {
		t.Fatalf("got %d want 501", resp.StatusCode)
	}
}

// TestMonitorRouteStubAnthropic verifies a monitor-route listener
// returns a stub message and records a telemetry row.
func TestMonitorRouteStubAnthropic(t *testing.T) {
	cfg := testConfig(t, "http://no-upstream.invalid", "http://no-upstream.invalid")
	rc := resolvedAnthropicBaseten(t)
	rc.Route = "monitor"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-opus-4-8","messages":[]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "message") {
		t.Fatalf("monitor stub body unexpected: %s", b)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].ConfiguredRoute != "monitor" {
		t.Fatalf("route = %q want monitor", rows[0].ConfiguredRoute)
	}
	if rows[0].Client != "claude-code" {
		t.Fatalf("client = %q want claude-code", rows[0].Client)
	}
}

// TestMonitorRouteStubOpenAI verifies a monitor-route openai-shape
// listener returns a stub chat completion.
func TestMonitorRouteStubOpenAI(t *testing.T) {
	cfg := testConfig(t, "http://no-upstream.invalid", "http://no-upstream.invalid")
	rc := resolvedOpenAIBaseten(t, "opencode", "monitor")
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-4o","messages":[]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "opencode", "/v1/chat/completions"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "chat.completion") {
		t.Fatalf("monitor openai stub body unexpected: %s", b)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].ConfiguredRoute != "monitor" || rows[0].Client != "opencode" {
		t.Fatalf("monitor openai telemetry: %+v", rows[0])
	}
}

// TestReloadConfigChangesRoute verifies the in-process SIGHUP
// equivalent: starting with one client (anthropic+baseten) on a
// fixed port, calling reloadConfig after rewriting gateway.yaml to
// flip its route to anthropic changes the upstream the listener
// talks to. Config-driven rebind replaces the listener at the same
// bind_addr, so we re-query ClientAddr after reload.
func TestReloadConfigChangesRoute(t *testing.T) {
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_from_baseten","type":"message","content":[{"type":"text","text":"BASETEN"}]}`))
	}))
	defer basSrv.Close()
	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_from_anthropic","type":"message","content":[{"type":"text","text":"ANTHROPIC"}]}`))
	}))
	defer antSrv.Close()

	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	// Use a fixed port so reload can rebind at the same addr.
	port := freeTCPPort(t)
	bindAddr := "127.0.0.1:" + itoa(port)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")

	rc1 := resolvedAnthropicBaseten(t)
	rc1.BindAddr = bindAddr
	rc1.Route = "baseten"
	// Write the gateway.yaml so reloadConfig can re-read the same
	// file (with a different route) later.
	writeGatewayYAML(t, cfg.ConfigPath, []resolvedClientConfig{rc1})

	g, adminL, _ := newGateway(t, cfg, rc1)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// First request: should hit baseten mock.
	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)
	req1, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer tok")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	rb1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if !strings.Contains(string(rb1), "BASETEN") {
		t.Fatalf("first response should come from baseten mock, got %s", rb1)
	}

	// Rewrite gateway.yaml with route=anthropic (still anthropic
	// shape, so compatible) and trigger in-process reload.
	rc2 := resolvedAnthropicBaseten(t)
	rc2.BindAddr = bindAddr
	rc2.Route = "anthropic"
	writeGatewayYAML(t, cfg.ConfigPath, []resolvedClientConfig{rc2})

	g.reloadConfig()
	// Wait briefly for the rebind to swap handlers.
	deadline := time.Now().Add(3 * time.Second)
	var ok bool
	for time.Now().Before(deadline) {
		req2, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("Authorization", "Bearer tok")
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		rb2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		if strings.Contains(string(rb2), "ANTHROPIC") {
			ok = true
			break
		}
		if strings.Contains(string(rb2), "BASETEN") {
			// Still old handler; retry after a tick.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		t.Fatalf("unexpected response after reload: %s", rb2)
	}
	if !ok {
		t.Fatal("after reloadConfig the listener never switched to the anthropic mock")
	}

	rows := waitForRows(t, cfg.TelemetryDir, 2, 3*time.Second)
	basRows := 0
	antRows := 0
	for _, r := range rows {
		if r.ConfiguredRoute == "anthropic" {
			antRows++
		} else if r.ConfiguredRoute == "baseten" {
			basRows++
		}
	}
	if basRows == 0 {
		t.Fatalf("expected at least one baseten row pre-reload; rows=%+v", rows)
	}
	if antRows == 0 {
		t.Fatalf("expected at least one anthropic row post-reload; rows=%+v", rows)
	}
}

// TestTelemetryCarriesClientName asserts the per-request telemetry
// row carries the listener's `client` name on a second listener.
func TestTelemetryCarriesClientName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","model":"zai-org/GLM-5.2","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.Name = "custom-listener-xyz"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "custom-listener-xyz", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	events := waitForEvents(t, cfg.TelemetryDir, 1, 2*time.Second)
	if events[0].Client != "custom-listener-xyz" {
		t.Fatalf("telemetry client = %q want custom-listener-xyz", events[0].Client)
	}
	if events[0].ProviderReportedModel == nil ||
		*events[0].ProviderReportedModel != "zai-org/GLM-5.2" {
		t.Fatalf("telemetry provider model = %v want zai-org/GLM-5.2",
			events[0].ProviderReportedModel)
	}
}

// TestMissingConfigRefusal pins the no-config landmine fix
// (the lifecycle contract): a missing config file is a hard error
// naming the file and the fix, never an in-memory default that would
// bind the door's own ports.
func TestMissingConfigRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	cfg := Config{ConfigPath: path}
	got, err := loadResolvedClients(&cfg)
	if err == nil {
		t.Fatalf("expected refusal for missing config, got %d clients", len(got))
	}
	for _, want := range []string{path, "baseten-switch config init", "gateway.example.yaml", "BASETEN_SWITCH_CONFIG_PATH"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not mention %q", err.Error(), want)
		}
	}
}

// TestMalformedConfigRefusal: a config file that exists but does not
// parse is always a hard error.
func TestMalformedConfigRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte("clients: [::not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ConfigPath: path}
	if _, err := loadResolvedClients(&cfg); err == nil {
		t.Fatal("expected refusal for malformed config")
	} else if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error %q does not say malformed", err.Error())
	}
}

// --- helpers ---

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeGatewayYAML(t *testing.T, path string, rcc []resolvedClientConfig) {
	t.Helper()
	enabled := false
	for _, rc := range rcc {
		if rc.Route == "baseten" {
			enabled = true
			break
		}
	}
	f := config.File{
		Global: config.Global{
			RoutingEnabled: &enabled,
		},
	}
	for _, rc := range rcc {
		f.Clients = append(f.Clients, config.Client{
			Name:          rc.Name,
			Enabled:       true,
			BindAddr:      rc.BindAddr,
			ProtocolShape: rc.ProtocolShape,
			DefaultModel:  rc.DefaultModel,
		})
	}
	if err := config.Save(path, &f); err != nil {
		t.Fatal(err)
	}
}

// TestOpenAIShapeResponsesPassthroughForwardsVerbatim verifies an
// openai-shape listener with route=openai (native passthrough)
// forwards /v1/responses to the openai upstream verbatim: the model
// field is unchanged and the request hits /v1/responses.
func TestOpenAIShapeResponsesPassthroughForwardsVerbatim(t *testing.T) {
	gotModel := make(chan string, 1)
	gotPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		gotModel <- fmtString(m["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-4-turbo","output":[]}`))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.OpenAIURL = srv.URL
	// OpenAI shape, route=openai passthrough.
	rc := resolvedClientConfig{
		Name:          "opencode",
		BindAddr:      "127.0.0.1:0",
		ProtocolShape: "openai",
		Route:         "openai",
	}
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-4-turbo","input":"hi"}`)
	req, _ := http.NewRequest("POST", clientURL(g, "opencode", "/v1/responses"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	select {
	case p := <-gotPath:
		if p != "/v1/responses" {
			t.Fatalf("upstream path = %q, want /v1/responses", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never hit")
	}
	select {
	case m := <-gotModel:
		if m != "gpt-4-turbo" {
			t.Fatalf("passthrough must forward model verbatim; upstream got %q, want gpt-4-turbo", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received model")
	}
}

func TestNativeOpenAIResponsesAccountsForCachedInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("upstream path = %q, want /v1/responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "resp_1",
			"object": "response",
			"status": "completed",
			"model": "gpt-test",
			"output": [],
			"usage": {
				"input_tokens": 100,
				"input_tokens_details": {"cached_tokens": 80},
				"output_tokens": 7
			}
		}`)
	}))
	defer srv.Close()

	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.OpenAIURL = srv.URL
	rc := resolvedClientConfig{
		Name:          "opencode",
		BindAddr:      "127.0.0.1:0",
		ProtocolShape: "openai",
		Route:         "openai",
	}
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	if err := g.pricing.ReplaceModelsDev(
		[]byte(telemetryVariantPricingFixture),
		time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		`"openai-cached-input"`,
	); err != nil {
		t.Fatal(err)
	}
	stop := start(t, g)
	defer stop()

	req, _ := http.NewRequest(
		http.MethodPost,
		clientURL(g, "opencode", "/v1/responses"),
		bytes.NewReader([]byte(`{"model":"gpt-test","input":"hi"}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	event := rows[0]
	if valueOrZero(event.Usage.InputTokens) != 20 ||
		valueOrZero(event.Usage.CacheReadInputTokens) != 80 ||
		valueOrZero(event.Usage.OutputTokens) != 7 {
		t.Fatalf("OpenAI cached-input usage = %+v", event.Usage)
	}
	if !event.ActualCost.Priced ||
		valueOrZero(event.ActualCost.NanoUSD) != 42_000 {
		t.Fatalf("OpenAI cached-input cost = %+v, want 42000 nano-USD",
			event.ActualCost)
	}
}

// TestOpenAIShapeResponsesBasetenRewritesModel verifies an openai-shape
// listener with route=baseten rewrites the model field via its default
// default model when forwarding /v1/responses to the Baseten upstream.
func TestOpenAIShapeResponsesBasetenRewritesModel(t *testing.T) {
	gotModel := make(chan string, 1)
	gotPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		gotModel <- fmtString(m["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"zai-org/GLM-5.2","output":[]}`))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedOpenAIBaseten(t, "codex", "baseten")
	rc.DefaultModel = reasoningNeutralBasetenModel
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-4-turbo","input":"hi"}`)
	req, _ := http.NewRequest("POST", clientURL(g, "codex", "/v1/responses"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	select {
	case p := <-gotPath:
		if p != "/v1/responses" {
			t.Fatalf("upstream path = %q, want /v1/responses", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never hit")
	}
	select {
	case m := <-gotModel:
		if m != reasoningNeutralBasetenModel {
			t.Fatalf("baseten route must rewrite via default model; upstream got %q, want %s", m, reasoningNeutralBasetenModel)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received model")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].ConfiguredRoute != "baseten" {
		t.Fatalf("route = %q want baseten", rows[0].ConfiguredRoute)
	}
	if rows[0].Client != "codex" {
		t.Fatalf("client = %q want codex", rows[0].Client)
	}
}

func TestCodexCompatibilityModelRoutingBoundary(t *testing.T) {
	requestBody := []byte(`{"model":"` + CodexCompatibilityModel + `","input":"hi"}`)

	t.Run("global on resolves default and never adds native fallback", func(t *testing.T) {
		var basetenRequests atomic.Int64
		gotModel := make(chan string, 1)
		baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			basetenRequests.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotModel <- fmtString(body["model"])
			http.Error(w, "upstream failure", http.StatusInternalServerError)
		}))
		defer baseten.Close()
		var openAIRequests atomic.Int64
		openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			openAIRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer openai.Close()

		cfg := testConfig(t, baseten.URL, baseten.URL)
		cfg.OpenAIURL = openai.URL
		rc := resolvedOpenAIBaseten(t, "codex", "baseten")
		rc.HasGlobalRoutingGate = true
		rc.GlobalRoutingEnabled = true
		rc.DefaultModel = "moonshotai/Kimi-K2.7-Code"
		rc.FallbackRoute = "openai"
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		req, _ := http.NewRequest(
			http.MethodPost,
			clientURL(g, "codex", "/v1/responses"),
			bytes.NewReader(requestBody),
		)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := <-gotModel; got != rc.DefaultModel {
			t.Fatalf("Baseten model = %q, want %q", got, rc.DefaultModel)
		}
		if basetenRequests.Load() != 1 {
			t.Fatalf("Baseten requests = %d", basetenRequests.Load())
		}
		if openAIRequests.Load() != 0 {
			t.Fatalf("compatibility sentinel made %d native fallback requests", openAIRequests.Load())
		}
	})

	t.Run("global off rejects locally", func(t *testing.T) {
		var upstreamRequests atomic.Int64
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer upstream.Close()
		cfg := testConfig(t, upstream.URL, upstream.URL)
		cfg.OpenAIURL = upstream.URL
		rc := resolvedOpenAIBaseten(t, "codex", "baseten")
		rc.HasGlobalRoutingGate = true
		rc.GlobalRoutingEnabled = false
		rc.FallbackRoute = "openai"
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		req, _ := http.NewRequest(
			http.MethodPost,
			clientURL(g, "codex", "/v1/responses"),
			bytes.NewReader(requestBody),
		)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest ||
			!strings.Contains(string(body), "global routing is Off") {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		if upstreamRequests.Load() != 0 {
			t.Fatalf("global-Off sentinel reached upstream %d times", upstreamRequests.Load())
		}
	})
}

// TestOpenAIShapeResponsesAnthropicRouteReturns501 verifies that an
// openai-shape listener with route=anthropic (cross-shape) returns
// 501 for /v1/responses, mirroring forwardChatCompletions' shapeCompatible gate.
func TestOpenAIShapeResponsesAnthropicRouteReturns501(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedOpenAIBaseten(t, "opencode", "anthropic")
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-4o","input":"hi"}`)
	req, _ := http.NewRequest("POST", clientURL(g, "opencode", "/v1/responses"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 501 {
		t.Fatalf("got %d want 501", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "cross-shape translation not implemented") {
		t.Fatalf("unexpected 501 body: %s", b)
	}
}

func TestTelemetryDirExpandsTildeInYAML(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	tmpYAML := filepath.Join(t.TempDir(), "gateway.yaml")
	body := "global:\n  routing_enabled: false\n  telemetry_dir: ~/baseten-tilde-test\nclients:\n  - name: claude-code\n    enabled: true\n    bind_addr: 127.0.0.1:0\n    protocol_shape: anthropic\n    default_model: zai-org/GLM-5.2\n"
	if err := os.WriteFile(tmpYAML, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Join(home, "baseten-tilde-test"))

	f, err := config.Load(tmpYAML)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{TelemetryDir: "/should-be-overridden"}
	if _, err := loadResolvedClientsInto(cfg, f, tmpYAML); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "baseten-tilde-test")
	if cfg.TelemetryDir != want {
		t.Fatalf("TelemetryDir not tilde-expanded\ngot:  %s\nwant: %s", cfg.TelemetryDir, want)
	}
}

func TestTelemetryEnabledFalseSkipsCollection(t *testing.T) {
	enabled := false
	path := filepath.Join(t.TempDir(), "telemetry")
	g := &Gateway{
		cfg: Config{
			TelemetryDir:     path,
			TelemetryEnabled: &enabled,
		},
		pricing: pricing.New(),
	}

	g.writeTelemetryV1(gatewayTelemetryEvent(1))

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled telemetry created %q: %v", path, err)
	}
}

// TestPostMessagesSanitizeHistory verifies the WS1 history shims: with
// sanitize_history on, empty text blocks are stripped and tool ids
// normalized before the body reaches an anthropic-shape upstream; with
// it off, the body is forwarded byte-for-byte. Telemetry records which.
func TestPostMessagesSanitizeHistory(t *testing.T) {
	dirty := `{"model":"claude-opus-4-8","stream":false,"messages":[` +
		`{"role":"assistant","content":[{"type":"text","text":""},{"type":"tool_use","id":"functions.Bash:0","name":"Bash","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"functions.Bash:0","content":"ok"}]}]}`

	for _, tc := range []struct {
		name     string
		sanitize bool
	}{
		{"on", true},
		{"off", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotBody := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotBody <- b
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"model":"m","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer srv.Close()
			cfg := testConfig(t, srv.URL, srv.URL)
			rc := resolvedAnthropicBaseten(t)
			rc.SanitizeHistory = tc.sanitize
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), strings.NewReader(dirty))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			var upstream []byte
			select {
			case upstream = <-gotBody:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream never received body")
			}
			var m map[string]interface{}
			if err := json.Unmarshal(upstream, &m); err != nil {
				t.Fatalf("upstream body not json: %v", err)
			}
			msgs := m["messages"].([]interface{})
			first := msgs[0].(map[string]interface{})["content"].([]interface{})
			if tc.sanitize {
				if len(first) != 1 {
					t.Fatalf("empty text block not stripped: %s", upstream)
				}
				if id := first[0].(map[string]interface{})["id"]; id != "functions_Bash_0" {
					t.Fatalf("tool id not normalized, got %v", id)
				}
			} else {
				if len(first) != 2 {
					t.Fatalf("body modified with sanitize off: %s", upstream)
				}
				if id := first[1].(map[string]interface{})["id"]; id != "functions.Bash:0" {
					t.Fatalf("tool id modified with sanitize off, got %v", id)
				}
			}
			rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
			if rows[0].Sanitized != tc.sanitize {
				t.Fatalf("telemetry sanitized = %v, want %v", rows[0].Sanitized, tc.sanitize)
			}
		})
	}
}

// TestAnthropic429RelaysWithoutFallbackOrCooldown verifies that a rate
// limit is a terminal response for an Anthropic-shape request. The
// original retry contract reaches the harness, and a later request
// rechecks the configured primary instead of inheriting a cooldown.
func TestAnthropic429RelaysWithoutFallbackOrCooldown(t *testing.T) {
	const rateLimitBody = `{"type":"error","error":{"type":"rate_limit_error","message":"try again later"}}`
	var primaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, rateLimitBody)
	}))
	defer primary.Close()

	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, fallback.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	send := func() {
		t.Helper()
		body := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
		req, _ := http.NewRequest(
			http.MethodPost,
			clientURL(g, "claude-code", "/v1/messages"),
			bytes.NewReader(body),
		)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		gotBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", resp.StatusCode)
		}
		if got := resp.Header.Get("Retry-After"); got != "17" {
			t.Fatalf("Retry-After = %q, want 17", got)
		}
		if string(gotBody) != rateLimitBody {
			t.Fatalf("body = %q, want exact upstream body %q", gotBody, rateLimitBody)
		}
		if g.fallbackActive("claude-code") {
			t.Fatal("Anthropic 429 activated fallback cooldown")
		}
	}

	send()
	send()

	if got := primaryHits.Load(); got != 2 {
		t.Fatalf("primary hits = %d, want 2", got)
	}
	if got := fallbackHits.Load(); got != 0 {
		t.Fatalf("fallback hits = %d, want 0", got)
	}

	rows := waitForRows(t, cfg.TelemetryDir, 2, 2*time.Second)
	for i, row := range rows {
		if row.ConfiguredRoute != "baseten" ||
			row.EffectiveProvider != "baseten" {
			t.Fatalf(
				"row %d route/effective = %q/%q, want baseten/baseten",
				i,
				row.ConfiguredRoute,
				row.EffectiveProvider,
			)
		}
		if row.StatusCode() != http.StatusTooManyRequests ||
			!row.IsHTTPError() {
			t.Fatalf(
				"row %d status/error = %d/%v, want 429/true",
				i,
				row.StatusCode(),
				row.IsHTTPError(),
			)
		}
		if row.Fallback.Attempted ||
			row.Fallback.Count != 0 ||
			row.Fallback.Trigger != nil {
			t.Fatalf("row %d fallback = %+v, want no fallback", i, row.Fallback)
		}
	}
}

// TestTranslatedAnthropic429PreservesRetryAfter verifies the terminal
// rate-limit policy also applies when an Anthropic listener translates
// its Baseten request through the OpenAI wire shape. The error envelope
// keeps the existing translation behavior while response control
// headers survive that body rewrite.
func TestTranslatedAnthropic429PreservesRetryAfter(t *testing.T) {
	var primaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "23")
		w.Header().Set("ETag", `"stale-error-body"`)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(
			w,
			`{"error":{"type":"rate_limit","message":"translated retry later"}}`,
		)
	}))
	defer primary.Close()

	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, fallback.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.UpstreamShape = "openai"
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gotBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf(
			"status/body/primary/fallback = %d/%s/%d/%d, want 429 from primary",
			resp.StatusCode,
			gotBody,
			primaryHits.Load(),
			fallbackHits.Load(),
		)
	}
	if got := resp.Header.Get("Retry-After"); got != "23" {
		t.Fatalf("Retry-After = %q, want 23", got)
	}
	if got := resp.Header.Get("ETag"); got != "" {
		t.Fatalf("ETag = %q, want stale representation header removed", got)
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(gotBody, &envelope); err != nil {
		t.Fatalf("decode translated error: %v: %s", err, gotBody)
	}
	if envelope.Type != "error" ||
		envelope.Error.Type != "rate_limit_error" ||
		envelope.Error.Message != "translated retry later" {
		t.Fatalf("translated error = %+v, want Anthropic rate-limit envelope", envelope)
	}
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary hits = %d, want 1", got)
	}
	if got := fallbackHits.Load(); got != 0 {
		t.Fatalf("fallback hits = %d, want 0", got)
	}
	if g.fallbackActive("claude-code") {
		t.Fatal("translated Anthropic 429 activated fallback cooldown")
	}

	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	row := rows[0]
	if row.EffectiveProvider != "baseten" ||
		row.StatusCode() != http.StatusTooManyRequests ||
		!row.IsHTTPError() ||
		row.Fallback.Attempted ||
		row.Fallback.Trigger != nil {
		t.Fatalf(
			"provider/status/error/fallback = %q/%d/%v/%+v, want baseten/429/true/none",
			row.EffectiveProvider,
			row.StatusCode(),
			row.IsHTTPError(),
			row.Fallback,
		)
	}
}

// TestOpenAI429RetainsFallbackAndCooldown verifies that making the
// status policy shape-aware does not change the OpenAI-shape contract.
func TestOpenAI429RetainsFallbackAndCooldown(t *testing.T) {
	var primaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"try later"}}`)
	}))
	defer primary.Close()

	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(
			w,
			`{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		)
	}))
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, primary.URL)
	cfg.OpenAIURL = fallback.URL
	rc := resolvedOpenAIBaseten(t, "codex", "baseten")
	rc.FallbackRoute = "openai"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-5","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest(
		http.MethodPost,
		clientURL(g, "codex", "/v1/chat/completions"),
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer synthetic-native-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gotBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(gotBody), `"content":"ok"`) {
		t.Fatalf("status/body = %d/%s, want 200 from fallback", resp.StatusCode, gotBody)
	}
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary hits = %d, want 1", got)
	}
	if got := fallbackHits.Load(); got != 1 {
		t.Fatalf("fallback hits = %d, want 1", got)
	}
	if !g.fallbackActive("codex") {
		t.Fatal("OpenAI 429 did not activate fallback cooldown")
	}

	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	row := rows[0]
	if row.ConfiguredRoute != "baseten" ||
		row.EffectiveProvider != "openai" ||
		row.StatusCode() != http.StatusOK ||
		row.IsHTTPError() {
		t.Fatalf(
			"route/effective/status/error = %q/%q/%d/%v, want baseten/openai/200/false",
			row.ConfiguredRoute,
			row.EffectiveProvider,
			row.StatusCode(),
			row.IsHTTPError(),
		)
	}
	if !row.Fallback.Attempted ||
		row.Fallback.Count != 1 ||
		valueOrZero(row.Fallback.Trigger) != "http_429" {
		t.Fatalf("fallback = %+v, want attempted count=1 trigger=http_429", row.Fallback)
	}
}

// TestFallbackRouteOn503WithCooldown: a baseten-routed listener with
// fallback_route=anthropic retries the fallback when the primary
// returns 503 (invisible to the client), then routes straight to the
// fallback during the cooldown window without re-trying the primary.
func TestFallbackRouteOn503WithCooldown(t *testing.T) {
	var primaryHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"overloaded"}`))
	}))
	defer primary.Close()
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-x","usage":{"input_tokens":2,"output_tokens":3}}`))
	}))
	defer fb.Close()

	cfg := testConfig(t, primary.URL, fb.URL) // baseten -> primary, anthropic -> fb
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	send := func() string {
		body := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
		req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("got %d want 200 (fallback should serve)", resp.StatusCode)
		}
		rb, _ := io.ReadAll(resp.Body)
		return string(rb)
	}

	if rb := send(); !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("first request not served by fallback: %s", rb)
	}
	if n := atomic.LoadInt32(&primaryHits); n != 1 {
		t.Fatalf("primary hits = %d, want 1", n)
	}
	// Cooldown active: second request must not touch the primary.
	if rb := send(); !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("second request not served by fallback: %s", rb)
	}
	if n := atomic.LoadInt32(&primaryHits); n != 1 {
		t.Fatalf("primary hit during cooldown: hits = %d, want 1", n)
	}

	rows := waitForRows(t, cfg.TelemetryDir, 2, 2*time.Second)
	for i, row := range rows {
		if row.ConfiguredRoute != "baseten" || row.EffectiveProvider != "anthropic" {
			t.Fatalf("row %d route/effective = %q/%q, want baseten/anthropic", i, row.ConfiguredRoute, row.EffectiveProvider)
		}
		if row.IsHTTPError() || row.StatusCode() != 200 {
			t.Fatalf("row %d status/errored = %d/%v", i, row.StatusCode(), row.IsHTTPError())
		}
	}
	if rows[0].ServedModel != "claude-opus-4-8" {
		t.Fatalf("fallback must not carry baseten model rewrite, got %q", rows[0].ServedModel)
	}
}

// TestClientCancellationDoesNotTripFallback verifies that a caller abandoning
// one request does not mark the provider unhealthy for later requests. Claude
// Code can cancel an auxiliary request while its main turn remains active;
// retrying the canceled context cannot help, and a shared cooldown would move
// the unrelated main turn to the native fallback.
func TestClientCancellationDoesNotTripFallback(t *testing.T) {
	var primaryHits atomic.Int32
	var fallbackHits atomic.Int32
	primaryStarted := make(chan struct{})
	releasePrimary := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch primaryHits.Add(1) {
		case 1:
			close(primaryStarted)
			select {
			case <-r.Context().Done():
			case <-releasePrimary:
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PRIMARY"}],"model":"zai-org/GLM-5.2","usage":{"input_tokens":2,"output_tokens":1}}`))
		}
	}))
	defer primary.Close()
	defer close(releasePrimary)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-opus-4-8","usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer fallback.Close()

	cfg := testConfig(t, primary.URL, fallback.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-primaryStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("primary did not receive canceled request")
	}
	cancel()
	select {
	case err := <-requestDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled request did not return")
	}

	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].TerminationReason != telemetry.TerminationClientCancelled ||
		rows[0].Status != nil ||
		rows[0].EffectiveProvider != "baseten" ||
		rows[0].Fallback.Attempted ||
		rows[0].Fallback.Count != 0 ||
		valueOrZero(rows[0].Fallback.Trigger) != "" {
		t.Fatalf(
			"canceled row termination/status/provider/fallback = %q/%v/%q/%+v, want client_cancelled/nil/baseten/no fallback",
			rows[0].TerminationReason,
			rows[0].Status,
			rows[0].EffectiveProvider,
			rows[0].Fallback,
		)
	}
	if g.fallbackActive("claude-code") {
		t.Fatal("client cancellation tripped fallback cooldown")
	}

	followup, err := http.Post(
		clientURL(g, "claude-code", "/v1/messages"),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	followupBody, _ := io.ReadAll(followup.Body)
	followup.Body.Close()
	if followup.StatusCode != http.StatusOK ||
		!strings.Contains(string(followupBody), "PRIMARY") {
		t.Fatalf(
			"follow-up status/body = %d/%s, want 200 from primary",
			followup.StatusCode,
			followupBody,
		)
	}
	if got := primaryHits.Load(); got != 2 {
		t.Fatalf("primary hits = %d, want 2", got)
	}
	if got := fallbackHits.Load(); got != 0 {
		t.Fatalf("fallback hits = %d, want 0", got)
	}
}

// TestResolveFromFileRejectsInvalidFallback verifies the clean schema
// accepts only the protocol's native provider as fallback.
func TestResolveFromFileRejectsInvalidFallback(t *testing.T) {
	enabled := true
	for _, tc := range []struct {
		shape, fallback string
	}{
		{"openai", "anthropic"},
		{"anthropic", "baseten"},
		{"anthropic", "monitor"},
		{"anthropic", "openai"},
	} {
		f := &config.File{
			Global: config.Global{
				RoutingEnabled: &enabled,
			},
			Clients: []config.Client{{
				Name: "client", Enabled: true, BindAddr: "127.0.0.1:0",
				ProtocolShape: tc.shape, FallbackRoute: tc.fallback,
				DefaultModel: "zai-org/GLM-5.2",
			}},
		}
		if _, err := resolveFromFile(f); err == nil {
			t.Errorf("shape %q fallback %q unexpectedly accepted", tc.shape, tc.fallback)
		}
	}
}

// TestMessagesTranslatedStreaming: anthropic-shape listener, openai
// route, streaming. The gateway converts the openai SSE stream into
// anthropic events and telemetry sees the usage from the final chunk.
func TestMessagesTranslatedStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"include_usage":true`) {
			t.Errorf("stream_options.include_usage not requested: %s", b)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"c1","model":"gpt-x","choices":[{"delta":{"role":"assistant","content":"PO"}}]}`,
			`{"id":"c1","model":"gpt-x","choices":[{"delta":{"content":"NG"}}]}`,
			`{"id":"c1","model":"gpt-x","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c1","model":"gpt-x","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2}}`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.OpenAIURL = srv.URL
	rc := resolvedAnthropicBaseten(t)
	rc.Route = "openai"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-x","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	rb, _ := io.ReadAll(resp.Body)
	s := string(rb)
	for _, want := range []string{
		"event: message_start", `"text_delta"`, `"text":"PO"`, `"text":"NG"`,
		"event: message_delta", `"stop_reason":"end_turn"`, "event: message_stop",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("emitted stream missing %q:\n%s", want, s)
		}
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if !rows[0].Translated || !rows[0].IsStream {
		t.Fatalf("row flags wrong: %+v", rows[0])
	}
	if valueOrZero(rows[0].Usage.InputTokens) != 9 || valueOrZero(rows[0].Usage.OutputTokens) != 2 {
		t.Fatalf("usage not parsed from translated stream: %+v", rows[0])
	}
	if valueOrZero(rows[0].ProviderStopReason) != "end_turn" {
		t.Fatalf("stop_reason = %q", valueOrZero(rows[0].ProviderStopReason))
	}
}

// TestBasetenUpstreamShapeOpenAITranslatesAndRewritesModel: route=baseten
// with upstream_shape=openai sends translated chat.completions to the
// baseten upstream with the default-model rewrite (Claude Code on an
// openai-only Baseten model).
func TestBasetenUpstreamShapeOpenAITranslatesAndRewritesModel(t *testing.T) {
	gotBody := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody <- b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c2","model":"m","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.UpstreamShape = "openai"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-opus-4-8","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		rb, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d: %s", resp.StatusCode, rb)
	}
	var up map[string]interface{}
	select {
	case b := <-gotBody:
		_ = json.Unmarshal(b, &up)
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never hit")
	}
	if m := fmtString(up["model"]); m == "claude-opus-4-8" || m == "" {
		t.Fatalf("model not rewritten for baseten, got %q", m)
	}
	if _, hasSystem := up["system"]; hasSystem {
		t.Fatalf("anthropic system field leaked into openai body: %v", up)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if !rows[0].Translated || rows[0].ConfiguredRoute != "baseten" {
		t.Fatalf("row wrong: %+v", rows[0])
	}
}

// TestMessagesTranslateRejectsImages: unsupported content on the
// translation path fails with a clean 400 instead of a mistranslation.
func TestMessagesTranslateRejectsImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be hit")
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.OpenAIURL = srv.URL
	rc := resolvedAnthropicBaseten(t)
	rc.Route = "openai"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("got %d want 400", resp.StatusCode)
	}
	rb, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rb), "not supported over cross-shape translation") {
		t.Fatalf("unexpected 400 body: %s", rb)
	}
}

func TestUpstreamModelForConfiguredDefaultModel(t *testing.T) {
	g := &Gateway{}
	rc := resolvedClientConfig{
		Route:        "baseten",
		DefaultModel: "org/default-slug",
	}
	if got := g.upstreamModelFor(rc); got != "org/default-slug" {
		t.Fatalf("upstreamModelFor = %q, want org/default-slug", got)
	}
	rc.Route = "anthropic"
	if got := g.upstreamModelFor(rc); got != "" {
		t.Fatalf("upstreamModelFor = %q, want empty for non-Baseten route", got)
	}
}

// TestResolveFromFileSkipsSameShapeSharedAddr: two clients sharing a
// concrete bind_addr with the same protocol_shape is a config error;
// the later one is skipped (same leniency as invalid fallback_route).
// Distinct shapes may share, and port-0 binds are never shared (the
// kernel assigns a fresh port per listener).
func TestResolveFromFileSkipsSameShapeSharedAddr(t *testing.T) {
	enabled := true
	f := &config.File{
		Global: config.Global{
			RoutingEnabled: &enabled,
		},
		Clients: []config.Client{
			{Name: "claude-code", Enabled: true, BindAddr: "127.0.0.1:18081", ProtocolShape: "anthropic", DefaultModel: "zai-org/GLM-5.2"},
			{Name: "codex", Enabled: true, BindAddr: "127.0.0.1:18081", ProtocolShape: "openai", DefaultModel: "zai-org/GLM-5.2"},
			{Name: "opencode", Enabled: true, BindAddr: "127.0.0.1:18081", ProtocolShape: "openai", DefaultModel: "zai-org/GLM-5.2"},
			{Name: "eph-a", Enabled: true, BindAddr: "127.0.0.1:0", ProtocolShape: "anthropic", DefaultModel: "zai-org/GLM-5.2"},
			{Name: "eph-b", Enabled: true, BindAddr: "127.0.0.1:0", ProtocolShape: "anthropic", DefaultModel: "zai-org/GLM-5.2"},
		},
	}
	got, err := resolveFromFile(f)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for _, rc := range got {
		names = append(names, rc.Name)
	}
	want := "claude-code,codex,eph-a,eph-b"
	if strings.Join(names, ",") != want {
		t.Fatalf("resolved clients = %v, want %s", names, want)
	}
	specs := groupResolved(got)
	if len(specs) != 3 {
		t.Fatalf("groups = %d, want 3 (one shared + two port-0): %+v", len(specs), specs)
	}
	shared := specs[0]
	if shared.addr != "127.0.0.1:18081" || len(shared.cfgs) != 2 ||
		shared.cfgs[0].Name != "claude-code" || shared.cfgs[1].Name != "codex" {
		t.Fatalf("shared group wrong: %+v", shared)
	}
	for _, s := range specs[1:] {
		if len(s.cfgs) != 1 {
			t.Fatalf("port-0 bind grouped as shared: %+v", s)
		}
	}
}

// TestGroupContentHashCoversAllClients: the rebind hash for a shared
// listener must be order-independent and change when any member's
// config changes.
func TestGroupContentHashCoversAllClients(t *testing.T) {
	a := resolvedClientConfig{Name: "claude-code", BindAddr: "127.0.0.1:18081", ProtocolShape: "anthropic", Route: "baseten"}
	b := resolvedClientConfig{Name: "codex", BindAddr: "127.0.0.1:18081", ProtocolShape: "openai", Route: "baseten"}
	base := groupContentHash([]resolvedClientConfig{a, b})
	if groupContentHash([]resolvedClientConfig{b, a}) != base {
		t.Fatal("group hash must be order-independent")
	}
	b2 := b
	b2.Route = "openai"
	if groupContentHash([]resolvedClientConfig{a, b2}) == base {
		t.Fatal("hash unchanged after second client's route changed")
	}
	a2 := a
	a2.ModelRoutes = map[string]string{"opus": "zai-org/GLM-5.2"}
	if groupContentHash([]resolvedClientConfig{a2, b}) == base {
		t.Fatal("hash unchanged after first client's model_routes changed")
	}
	a3 := a
	a3.Name = "renamed"
	if groupContentHash([]resolvedClientConfig{a3, b}) == base {
		t.Fatal("hash unchanged after a client rename")
	}
}

// TestSharedBindAddrDispatchByPath: one listener at a shared bind_addr
// serves an anthropic-shape and an openai-shape client; the owning
// client is resolved per request from the path, the existing per-client
// pipeline runs, and telemetry rows keep the resolved client's name.
// Also covers admin status (both clients report bound) and 404 for
// paths neither shape owns.
func TestSharedBindAddrDispatchByPath(t *testing.T) {
	antPaths := make(chan string, 4)
	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		antPaths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ANT"}],"model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer antSrv.Close()
	oaiPaths := make(chan string, 4)
	oaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oaiPaths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/responses" {
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-x","output":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"OAI"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer oaiSrv.Close()

	cfg := testConfig(t, antSrv.URL, antSrv.URL)
	cfg.OpenAIURL = oaiSrv.URL
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")

	addr := "127.0.0.1:" + itoa(freeTCPPort(t))
	ant := resolvedClientConfig{Name: "claude-code", BindAddr: addr, ProtocolShape: "anthropic", Route: "anthropic"}
	oai := resolvedClientConfig{Name: "codex", BindAddr: addr, ProtocolShape: "openai", Route: "openai"}
	writeGatewayYAML(t, cfg.ConfigPath, []resolvedClientConfig{ant, oai})
	g, adminL, _ := newGateway(t, cfg, ant, oai)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	if a, b := g.ClientAddr("claude-code").String(), g.ClientAddr("codex").String(); a != b {
		t.Fatalf("shared clients bound to different addrs: %s vs %s", a, b)
	}

	post := func(path, body string) *http.Response {
		req, _ := http.NewRequest("POST", clientURL(g, "claude-code", path), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post("/v1/messages", `{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`)
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(rb), "ANT") {
		t.Fatalf("/v1/messages got %d %s, want anthropic upstream", resp.StatusCode, rb)
	}
	select {
	case p := <-antPaths:
		if p != "/v1/messages" {
			t.Fatalf("anthropic upstream path = %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("anthropic upstream never hit")
	}

	resp = post("/v1/chat/completions", `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	rb, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(rb), "OAI") {
		t.Fatalf("/v1/chat/completions got %d %s, want openai upstream", resp.StatusCode, rb)
	}
	resp = post("/v1/responses", `{"model":"gpt-x","input":"hi"}`)
	rb, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(rb), "resp_1") {
		t.Fatalf("/v1/responses got %d %s, want openai upstream", resp.StatusCode, rb)
	}
	gotOAI := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case p := <-oaiPaths:
			gotOAI[p] = true
		case <-time.After(2 * time.Second):
			t.Fatal("openai upstream never hit")
		}
	}
	if !gotOAI["/v1/chat/completions"] || !gotOAI["/v1/responses"] {
		t.Fatalf("openai upstream paths = %v", gotOAI)
	}

	// Neither shape owns /v1/embeddings: existing error envelope.
	resp = post("/v1/embeddings", `{}`)
	rb, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || !strings.Contains(string(rb), `"error"`) {
		t.Fatalf("unmatched path got %d %s, want 404 error envelope", resp.StatusCode, rb)
	}

	// /healthz stays client-agnostic on a shared addr.
	hResp, err := http.Get(clientURL(g, "codex", "/healthz"))
	if err != nil {
		t.Fatal(err)
	}
	hResp.Body.Close()
	if hResp.StatusCode != 200 {
		t.Fatalf("shared /healthz got %d", hResp.StatusCode)
	}

	rows := waitForRows(t, cfg.TelemetryDir, 3, 2*time.Second)
	byClient := map[string]int{}
	for _, row := range rows {
		byClient[row.Client]++
	}
	if byClient["claude-code"] != 1 || byClient["codex"] != 2 {
		t.Fatalf("telemetry client counts = %v, want claude-code:1 codex:2", byClient)
	}

	// Both clients on the bound shared address report bound.
	sResp, err := http.Get(adminURL(g, "/v1/admin/status"))
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := io.ReadAll(sResp.Body)
	sResp.Body.Close()
	var st struct {
		Clients []struct {
			Name           string `json:"name"`
			CurrentlyBound bool   `json:"currently_bound"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(sb, &st); err != nil {
		t.Fatalf("bad status body %s: %v", sb, err)
	}
	bound := map[string]bool{}
	for _, c := range st.Clients {
		bound[c.Name] = c.CurrentlyBound
	}
	if !bound["claude-code"] || !bound["codex"] {
		t.Fatalf("currently_bound = %v, want both true", bound)
	}
}

// TestSharedBindAddrModelsHeaderDisambiguation: GET /v1/models on a
// shared address resolves to the anthropic-shape client when the
// anthropic-version header is present, else the openai-shape client.
func TestSharedBindAddrModelsHeaderDisambiguation(t *testing.T) {
	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"ant-model"}]}`))
	}))
	defer antSrv.Close()
	oaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"oai-model"}]}`))
	}))
	defer oaiSrv.Close()

	cfg := testConfig(t, antSrv.URL, antSrv.URL)
	cfg.OpenAIURL = oaiSrv.URL

	addr := "127.0.0.1:" + itoa(freeTCPPort(t))
	ant := resolvedClientConfig{Name: "claude-code", BindAddr: addr, ProtocolShape: "anthropic", Route: "anthropic"}
	oai := resolvedClientConfig{Name: "codex", BindAddr: addr, ProtocolShape: "openai", Route: "openai"}
	g, adminL, _ := newGateway(t, cfg, ant, oai)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	getModels := func(anthropicVersion string) string {
		req, _ := http.NewRequest("GET", clientURL(g, "claude-code", "/v1/models"), nil)
		if anthropicVersion != "" {
			req.Header.Set("Anthropic-Version", anthropicVersion)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET /v1/models got %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if b := getModels("2023-06-01"); !strings.Contains(b, "ant-model") {
		t.Fatalf("with anthropic-version header, models = %s, want anthropic upstream", b)
	}
	if b := getModels(""); !strings.Contains(b, "oai-model") {
		t.Fatalf("without anthropic-version header, models = %s, want openai upstream", b)
	}
}
