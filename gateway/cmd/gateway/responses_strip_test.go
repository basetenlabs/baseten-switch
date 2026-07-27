package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/proxy"
)

// TestResponsesStripToolTypesConfigLoad: the knob parses from YAML into
// the resolved client in config order, and a config without it (every
// pre-knob gateway.yaml) resolves with a nil list.
func TestResponsesStripToolTypesConfigLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	body := `global:
  routing_enabled: true
clients:
  - name: codex
    enabled: true
    bind_addr: 127.0.0.1:0
    protocol_shape: openai
    default_model: zai-org/GLM-5.2
    responses_strip_tool_types: [tool_search, web_search_preview]
  - name: opencode
    enabled: true
    bind_addr: 127.0.0.1:0
    protocol_shape: openai
    default_model: zai-org/GLM-5.2
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveFromFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("resolved %d clients, want 2", len(got))
	}
	want := map[string][]string{
		"codex":    {"tool_search", "web_search_preview"},
		"opencode": nil,
	}
	for _, rc := range got {
		if !reflect.DeepEqual(rc.ResponsesStripToolTypes, want[rc.Name]) {
			t.Errorf("client %s responses_strip_tool_types = %v, want %v",
				rc.Name, rc.ResponsesStripToolTypes, want[rc.Name])
		}
	}
}

// TestResponsesStripHashCoversStripList verifies that changing
// responses_strip_tool_types changes the resolvedClientConfig hash, so
// SIGHUP respawns the listener. The hash is deliberately
// order-sensitive over the list: a pure reorder also changes it (a
// spurious respawn is safe, a missed one silently breaks hot-reload).
func TestResponsesStripHashCoversStripList(t *testing.T) {
	base := resolvedClientConfig{
		Name:          "codex",
		BindAddr:      "127.0.0.1:18081",
		ProtocolShape: "openai",
		Route:         "baseten",
	}
	a := base
	a.ResponsesStripToolTypes = []string{"tool_search"}
	if base.hash() == a.hash() {
		t.Fatal("hash must change when responses_strip_tool_types is set")
	}
	b := a
	b.ResponsesStripToolTypes = []string{"tool_search", "web_search_preview"}
	if a.hash() == b.hash() {
		t.Fatal("hash must change when an entry is added")
	}
	c := a
	c.ResponsesStripToolTypes = []string{"web_search_preview"}
	if a.hash() == c.hash() {
		t.Fatal("hash must change when an entry value changes")
	}
	d := b
	d.ResponsesStripToolTypes = []string{"web_search_preview", "tool_search"}
	if b.hash() == d.hash() {
		t.Fatal("hash must change on a pure reorder (order-sensitive by design)")
	}
	e := b
	e.ResponsesStripToolTypes = []string{"tool_search"}
	if b.hash() == e.hash() {
		t.Fatal("hash must change when an entry is removed")
	}
}

// TestResponsesStripConfigValidation exercises
// validateResponsesStripToolTypes: the field on an anthropic-shape
// client and empty entries are load errors;
// openai-shape clients and disabled clients pass.
func TestResponsesStripConfigValidation(t *testing.T) {
	base := func(mut func(*config.Client)) *config.File {
		routingEnabled := true
		c := config.Client{
			Name:          "codex",
			Enabled:       true,
			BindAddr:      "127.0.0.1:0",
			ProtocolShape: "openai",
			DefaultModel:  "zai-org/GLM-5.2",
		}
		mut(&c)
		return &config.File{
			Global:  config.Global{RoutingEnabled: &routingEnabled},
			Clients: []config.Client{c},
		}
	}
	for _, tc := range []struct {
		name    string
		mut     func(*config.Client)
		wantErr string
	}{
		{"valid openai shape", func(c *config.Client) {
			c.ResponsesStripToolTypes = []string{"tool_search"}
		}, ""},
		{"unset is ok", func(c *config.Client) {}, ""},
		{"anthropic shape", func(c *config.Client) {
			c.ProtocolShape = "anthropic"
			c.ResponsesStripToolTypes = []string{"tool_search"}
		}, "responses_strip_tool_types requires protocol_shape openai"},
		{"empty entry", func(c *config.Client) {
			c.ResponsesStripToolTypes = []string{"tool_search", ""}
		}, "empty entry"},
		{"disabled client skipped", func(c *config.Client) {
			c.Enabled = false
			c.ProtocolShape = "anthropic"
			c.ResponsesStripToolTypes = []string{"tool_search"}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveFromFile(base(tc.mut))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// postResponses sends body to the listener's /v1/responses and fails
// the test on a non-200.
func postResponses(t *testing.T, g *Gateway, client string, body []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", clientURL(g, client, "/v1/responses"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d want 200: %s", resp.StatusCode, b)
	}
}

// captureServer returns an httptest server that records each request
// body on the channel and answers with a completed Responses object.
func captureServer(t *testing.T, gotBody chan []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody <- b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"m","output":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const reasoningNeutralBasetenModel = "moonshotai/Kimi-K2.7-Code"

func resolvedResponsesBaseten(t *testing.T) resolvedClientConfig {
	t.Helper()
	rc := resolvedOpenAIBaseten(t, "codex", "baseten")
	rc.DefaultModel = reasoningNeutralBasetenModel
	return rc
}

// TestResponsesStripBasetenAttempt: with the knob set, the baseten
// /v1/responses attempt has denylisted tools[] entries stripped and the
// model rewritten (default model) in the same pass; the telemetry row
// carries the stripped_tool_types marker and one stderr line names the
// client and the stripped types.
func TestResponsesStripBasetenAttempt(t *testing.T) {
	gotBody := make(chan []byte, 1)
	srv := captureServer(t, gotBody)
	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedResponsesBaseten(t)
	rc.ResponsesStripToolTypes = []string{"tool_search"}
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	origStderr := os.Stderr
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = pw
	defer func() { os.Stderr = origStderr }()

	body := []byte(`{"model":"gpt-5","tools":[{"type":"function","name":"shell"},{"type":"tool_search"}],"tool_choice":"auto","input":"hi"}`)
	postResponses(t, g, "codex", body)
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)

	os.Stderr = origStderr
	pw.Close()
	stderrOut, _ := io.ReadAll(pr)

	var up map[string]any
	if err := json.Unmarshal(<-gotBody, &up); err != nil {
		t.Fatalf("upstream body invalid json: %v", err)
	}
	wantTools := []any{map[string]any{"type": "function", "name": "shell"}}
	if !reflect.DeepEqual(up["tools"], wantTools) {
		t.Fatalf("upstream tools = %v, want %v", up["tools"], wantTools)
	}
	if up["model"] != reasoningNeutralBasetenModel {
		t.Fatalf("upstream model = %v, want %s (default-model rewrite must share the strip's decode)", up["model"], reasoningNeutralBasetenModel)
	}
	if up["tool_choice"] != "auto" || up["input"] != "hi" {
		t.Fatalf("unrelated fields not preserved: %v", up)
	}
	if len(rows[0].StrippedToolTypes) != 1 || rows[0].StrippedToolTypes[0] != "tool_search" {
		t.Fatalf("row stripped_tool_types = %q, want tool_search", strings.Join(rows[0].StrippedToolTypes, ","))
	}
	want := "[gateway] responses_strip client=codex types=tool_search"
	if !strings.Contains(string(stderrOut), want) {
		t.Fatalf("stderr missing %q, got:\n%s", want, stderrOut)
	}
}

// TestResponsesStripForcedBasetenTarget verifies that an explicitly
// mapped Baseten target still passes through the Responses strip.
// Explicit mappings use the forced-target attempt path, but every
// Baseten Responses attempt must apply the configured compatibility
// transform.
func TestResponsesStripForcedBasetenTarget(t *testing.T) {
	gotBody := make(chan []byte, 1)
	srv := captureServer(t, gotBody)
	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedResponsesBaseten(t)
	rc.ResponsesStripToolTypes = []string{"tool_search"}
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()

	body := []byte(`{"model":"gpt-5","tools":[{"type":"function","name":"shell"},{"type":"tool_search"}],"input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	at, err := g.buildAttemptTarget(
		g.clients["codex"],
		req,
		body,
		"baseten",
		"responses",
		"moonshotai/Kimi-K2.7-Code",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	var up map[string]any
	if err := json.Unmarshal(at.res.NewBody, &up); err != nil {
		t.Fatalf("upstream body invalid json: %v", err)
	}
	if up["model"] != "moonshotai/Kimi-K2.7-Code" {
		t.Fatalf("upstream model = %v, want forced Baseten target", up["model"])
	}
	wantTools := []any{map[string]any{"type": "function", "name": "shell"}}
	if !reflect.DeepEqual(up["tools"], wantTools) {
		t.Fatalf("upstream tools = %v, want %v", up["tools"], wantTools)
	}
	if got := strings.Join(at.strippedToolTypes, ","); got != "tool_search" {
		t.Fatalf("stripped tool types = %q, want tool_search", got)
	}
}

// TestResponsesStripWithoutModelRewrite: knob set, no default model resolves and
// therefore the strip is the only change. The stripped body must be
// forwarded (not the client's original bytes) and the telemetry marker
// must survive.
func TestResponsesStripWithoutModelRewrite(t *testing.T) {
	gotBody := make(chan []byte, 1)
	srv := captureServer(t, gotBody)
	cfg := testConfig(t, srv.URL, srv.URL)
	rc := resolvedClientConfig{
		Name:                    "codex",
		BindAddr:                "127.0.0.1:0",
		ProtocolShape:           "openai",
		Route:                   "baseten",
		ResponsesStripToolTypes: []string{"tool_search"},
	}
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-5","tools":[{"type":"tool_search"},{"type":"function","name":"shell"}],"input":"hi"}`)
	postResponses(t, g, "codex", body)
	var up map[string]any
	if err := json.Unmarshal(<-gotBody, &up); err != nil {
		t.Fatalf("upstream body invalid json: %v", err)
	}
	if up["model"] != "gpt-5" {
		t.Fatalf("upstream model = %v, want gpt-5 untouched (nothing resolves a rewrite)", up["model"])
	}
	wantTools := []any{map[string]any{"type": "function", "name": "shell"}}
	if !reflect.DeepEqual(up["tools"], wantTools) {
		t.Fatalf("upstream tools = %v, want %v", up["tools"], wantTools)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if len(rows[0].StrippedToolTypes) != 1 || rows[0].StrippedToolTypes[0] != "tool_search" {
		t.Fatalf("row stripped_tool_types = %q, want tool_search", strings.Join(rows[0].StrippedToolTypes, ","))
	}
}

// TestResponsesStripFallbackAttemptByteOriginal: when the baseten
// attempt trips the fallback waterfall, the fallback attempt's body is
// byte-identical to what the client sent (no strip, no rewrite, no
// re-marshal) and its telemetry row omits the marker.
func TestResponsesStripFallbackAttemptByteOriginal(t *testing.T) {
	basetenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer basetenSrv.Close()
	gotBody := make(chan []byte, 1)
	openaiSrv := captureServer(t, gotBody)
	cfg := testConfig(t, basetenSrv.URL, basetenSrv.URL)
	cfg.OpenAIURL = openaiSrv.URL
	rc := resolvedResponsesBaseten(t)
	rc.ResponsesStripToolTypes = []string{"tool_search"}
	rc.FallbackRoute = "openai"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-5","tools":[{"type":"tool_search"},{"type":"function","name":"shell"}],"input":"hi"}`)
	postResponses(t, g, "codex", body)
	if got := <-gotBody; !bytes.Equal(got, body) {
		t.Fatalf("fallback attempt body not byte-identical to the client original\ngot:  %s\nwant: %s", got, body)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].EffectiveProvider != "openai" {
		t.Fatalf("route_effective = %q, want openai (fallback must have served)", rows[0].EffectiveProvider)
	}
	if len(strings.Join(rows[0].StrippedToolTypes, ",")) != 0 {
		t.Fatalf("fallback row stripped_tool_types = %q, want empty", strings.Join(rows[0].StrippedToolTypes, ","))
	}
}

// TestResponsesStripKnobUnsetBytesUnchanged pins the knob-unset path to
// today's behavior on /v1/responses, byte for byte. Bodies put "model"
// before "tools" so any stealth re-marshal (which sorts keys) or strip
// changes the bytes.
func TestResponsesStripKnobUnsetBytesUnchanged(t *testing.T) {
	// No default model: today's Baseten attempt forwards the exact client bytes.
	// The knob-unset path must keep doing that.
	t.Run("no rewrite forwards exact bytes", func(t *testing.T) {
		gotBody := make(chan []byte, 1)
		srv := captureServer(t, gotBody)
		cfg := testConfig(t, srv.URL, srv.URL)
		rc := resolvedClientConfig{
			Name:          "codex",
			BindAddr:      "127.0.0.1:0",
			ProtocolShape: "openai",
			Route:         "baseten",
		}
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		body := []byte(`{"model":"gpt-5","tools":[{"type":"tool_search"}],"input":"hi"}`)
		postResponses(t, g, "codex", body)
		if got := <-gotBody; !bytes.Equal(got, body) {
			t.Fatalf("knob unset must forward exact bytes\ngot:  %s\nwant: %s", got, body)
		}
		rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
		if len(strings.Join(rows[0].StrippedToolTypes, ",")) != 0 {
			t.Fatalf("stripped_tool_types = %q, want empty", strings.Join(rows[0].StrippedToolTypes, ","))
		}
	})

	// Tier set: today's behavior is the RewriteModelInBody re-marshal
	// with tools untouched; the knob-unset path must match it exactly.
	t.Run("default-model rewrite matches prior bytes", func(t *testing.T) {
		gotBody := make(chan []byte, 1)
		srv := captureServer(t, gotBody)
		cfg := testConfig(t, srv.URL, srv.URL)
		rc := resolvedResponsesBaseten(t)
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		body := []byte(`{"model":"gpt-5","tools":[{"type":"tool_search"},{"type":"function","name":"shell"}],"input":"hi"}`)
		postResponses(t, g, "codex", body)
		want := proxy.RewriteModelInBody(body, reasoningNeutralBasetenModel).NewBody
		if got := <-gotBody; !bytes.Equal(got, want) {
			t.Fatalf("knob unset must match the pre-knob rewrite bytes\ngot:  %s\nwant: %s", got, want)
		}
	})

	// Knob set but nothing matches and nothing rewrites: the combined
	// path must also return the original bytes.
	t.Run("knob set no match forwards exact bytes", func(t *testing.T) {
		gotBody := make(chan []byte, 1)
		srv := captureServer(t, gotBody)
		cfg := testConfig(t, srv.URL, srv.URL)
		rc := resolvedClientConfig{
			Name:                    "codex",
			BindAddr:                "127.0.0.1:0",
			ProtocolShape:           "openai",
			Route:                   "baseten",
			ResponsesStripToolTypes: []string{"tool_search"},
		}
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		body := []byte(`{"model":"gpt-5","tools":[{"type":"function","name":"shell"}],"input":"hi"}`)
		postResponses(t, g, "codex", body)
		if got := <-gotBody; !bytes.Equal(got, body) {
			t.Fatalf("no-match strip must forward exact bytes\ngot:  %s\nwant: %s", got, body)
		}
		rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
		if len(strings.Join(rows[0].StrippedToolTypes, ",")) != 0 {
			t.Fatalf("stripped_tool_types = %q, want empty", strings.Join(rows[0].StrippedToolTypes, ","))
		}
	})
}

// TestResponsesStripOtherEndpointsUnaffected: the strip is gated on the
// responses kind. /v1/chat/completions on a knob-carrying client keeps
// its tools; an anthropic-shape listener's /v1/messages keeps its tools
// even if the knob were forced past config validation (which rejects it
// on anthropic shape).
func TestResponsesStripOtherEndpointsUnaffected(t *testing.T) {
	t.Run("chat completions", func(t *testing.T) {
		gotBody := make(chan []byte, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody <- b
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[]}`))
		}))
		defer srv.Close()
		cfg := testConfig(t, srv.URL, srv.URL)
		rc := resolvedResponsesBaseten(t)
		rc.ResponsesStripToolTypes = []string{"tool_search"}
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		body := []byte(`{"model":"gpt-5","tools":[{"type":"tool_search"},{"type":"function","name":"shell"}],"messages":[]}`)
		req, _ := http.NewRequest("POST", clientURL(g, "codex", "/v1/chat/completions"), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		var up map[string]any
		if err := json.Unmarshal(<-gotBody, &up); err != nil {
			t.Fatal(err)
		}
		tools, _ := up["tools"].([]any)
		if len(tools) != 2 {
			t.Fatalf("chat.completions tools = %v, want both entries intact", up["tools"])
		}
		rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
		if len(strings.Join(rows[0].StrippedToolTypes, ",")) != 0 {
			t.Fatalf("stripped_tool_types = %q, want empty", strings.Join(rows[0].StrippedToolTypes, ","))
		}
	})

	t.Run("anthropic messages", func(t *testing.T) {
		gotBody := make(chan []byte, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody <- b
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		defer srv.Close()
		cfg := testConfig(t, srv.URL, srv.URL)
		rc := resolvedAnthropicBaseten(t)
		rc.ResponsesStripToolTypes = []string{"tool_search"}
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		body := []byte(`{"model":"claude-opus-4-8","tools":[{"type":"tool_search"},{"name":"shell","input_schema":{}}],"messages":[]}`)
		req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		var up map[string]any
		if err := json.Unmarshal(<-gotBody, &up); err != nil {
			t.Fatal(err)
		}
		tools, _ := up["tools"].([]any)
		if len(tools) != 2 {
			t.Fatalf("messages tools = %v, want both entries intact", up["tools"])
		}
	})
}
