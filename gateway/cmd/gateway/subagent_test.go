// subagent_test.go covers the gateway half of the subagent-routing contract:
// the x-claude-code-agent-id header gate, the model rewrite, telemetry
// attribution, byte-identical passthrough, fallback interplay, SIGHUP
// reload, admin status, and load-time validation. All upstreams are
// hermetic httptest servers; no real state is touched.
package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

// subagentAgentIDHeader mirrors the gateway const so tests stay
// decoupled from the exact spelling in gateway.go.
const subagentAgentIDHeaderTest = "x-claude-code-agent-id"

// subagentClient returns an anthropic-shape resolved client with
// model_aliases and the given subagent_model/subagent_routing, on the
// given route.
func subagentClient(t *testing.T, rt, model, routing string) resolvedClientConfig {
	t.Helper()
	rc := resolvedAnthropicBaseten(t)
	rc.Route = rt
	rc.GlobalRoutingEnabled = rt == "baseten"
	rc.ModelAliases = map[string]string{
		"claude-baseten-glm-5-2": "zai-org/GLM-5.2",
		"anthropic-baseten-kimi": "moonshotai/Kimi-K2.7-Code",
	}
	rc.SubagentModel = model
	rc.SubagentRouting = routing
	return rc
}

// postMessagesWithAgent sends a /v1/messages request with the given
// model and an optional agent-id header. Returns the response and body.
func postMessagesWithAgent(t *testing.T, g *Gateway, model string, agentID string) (*http.Response, string) {
	t.Helper()
	body := []byte(`{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if agentID != "" {
		req.Header.Set(subagentAgentIDHeaderTest, agentID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(rb)
}

// postMessagesRaw sends a /v1/messages request with the exact body
// bytes and an optional agent-id header, returning the response, the
// body the upstream received, and the response body. Used for the
// byte-identical passthrough assertion.
func postMessagesRaw(t *testing.T, g *Gateway, body []byte, agentID string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if agentID != "" {
		req.Header.Set(subagentAgentIDHeaderTest, agentID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, rb
}

// basetenStub returns an httptest server that records the model it
// received and replies with a 200 anthropic-shape message.
func basetenStub(t *testing.T, gotModel chan string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if gotModel != nil {
			gotModel <- fmtString(m["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"VIA-BASETEN"}],"model":"zai-org/GLM-5.2","usage":{"input_tokens":5,"output_tokens":1}}`))
	}))
}

// anthropicStub returns an httptest server that records the model and
// body it received and replies with a 200 anthropic-shape message.
func anthropicStub(t *testing.T, gotModel chan string, gotBody chan []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if gotModel != nil {
			gotModel <- fmtString(m["model"])
		}
		if gotBody != nil {
			gotBody <- b
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"VIA-ANTHROPIC"}],"model":"claude-opus-4-8","usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
}

// TestSubagentMatrix exercises the full matrix: header present/absent
// x toggle on/off/unset x target class alias/slug/native x switch
// position. For each cell it asserts the upstream model received, the
// route taken, and telemetry rows (subagent flag, subagent_model,
// requested_model preserved as original).
func TestSubagentMatrix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		header    string // "" = absent, "agent-1" = present
		model     string // subagent_model config value
		routing   string // subagent_routing config value
		route     string // switch position
		reqModel  string // model the harness sends
		wantUp    string // model the upstream should receive
		wantRoute string // telemetry route
		wantEff   string // telemetry route_effective
		wantSub   bool   // telemetry subagent flag
		wantSubM  string // telemetry subagent_model
		wantReq   string // telemetry requested_model (original)
	}{
		// --- toggle ON, header present ---
		{
			name: "on/alias/baseten", header: "agent-1", model: "claude-baseten-glm-5-2", routing: "on",
			route: "baseten", reqModel: "claude-opus-4-8",
			wantUp: "zai-org/GLM-5.2", wantRoute: "baseten", wantEff: "",
			wantSub: true, wantSubM: "claude-baseten-glm-5-2", wantReq: "claude-opus-4-8",
		},
		{
			name: "on/alias/anthropic", header: "agent-1", model: "claude-baseten-glm-5-2", routing: "on",
			route: "anthropic", reqModel: "claude-opus-4-8",
			wantUp: "zai-org/GLM-5.2", wantRoute: "anthropic", wantEff: "baseten",
			wantSub: true, wantSubM: "claude-baseten-glm-5-2", wantReq: "claude-opus-4-8",
		},
		{
			name: "on/slug/baseten", header: "agent-1", model: "moonshotai/Kimi-K2.7-Code", routing: "on",
			route: "baseten", reqModel: "claude-opus-4-8",
			wantUp: "moonshotai/Kimi-K2.7-Code", wantRoute: "baseten", wantEff: "",
			wantSub: true, wantSubM: "moonshotai/Kimi-K2.7-Code", wantReq: "claude-opus-4-8",
		},
		{
			name: "on/slug/anthropic", header: "agent-1", model: "moonshotai/Kimi-K2.7-Code", routing: "on",
			route: "anthropic", reqModel: "claude-opus-4-8",
			wantUp: "moonshotai/Kimi-K2.7-Code", wantRoute: "anthropic", wantEff: "baseten",
			wantSub: true, wantSubM: "moonshotai/Kimi-K2.7-Code", wantReq: "claude-opus-4-8",
		},
		{
			name: "on/native/baseten", header: "agent-1", model: "claude-sonnet-4-6", routing: "on",
			route: "baseten", reqModel: "claude-opus-4-8",
			// Native target on Baseten route: default-model rewrite applies.
			wantUp: "zai-org/GLM-5.2", wantRoute: "baseten", wantEff: "",
			wantSub: true, wantSubM: "claude-sonnet-4-6", wantReq: "claude-opus-4-8",
		},
		{
			name: "on/native/anthropic", header: "agent-1", model: "claude-sonnet-4-6", routing: "on",
			route: "anthropic", reqModel: "claude-opus-4-8",
			// Native target on anthropic route: passthrough of rewritten id.
			wantUp: "claude-sonnet-4-6", wantRoute: "anthropic", wantEff: "",
			wantSub: true, wantSubM: "claude-sonnet-4-6", wantReq: "claude-opus-4-8",
		},
		// --- toggle ON, header ABSENT (main thread) ---
		{
			name: "on/noheader/baseten", header: "", model: "claude-baseten-glm-5-2", routing: "on",
			route: "baseten", reqModel: "claude-opus-4-8",
			// No header: no subagent rewrite; the default model applies.
			wantUp: "zai-org/GLM-5.2", wantRoute: "baseten", wantEff: "",
			wantSub: false, wantSubM: "", wantReq: "claude-opus-4-8",
		},
		{
			name: "on/noheader/anthropic", header: "", model: "claude-baseten-glm-5-2", routing: "on",
			route: "anthropic", reqModel: "claude-opus-4-8",
			// No header: passthrough of original model.
			wantUp: "claude-opus-4-8", wantRoute: "anthropic", wantEff: "",
			wantSub: false, wantSubM: "", wantReq: "claude-opus-4-8",
		},
		// --- toggle OFF, header present ---
		{
			name: "off/header/baseten", header: "agent-1", model: "claude-baseten-glm-5-2", routing: "off",
			route: "baseten", reqModel: "claude-opus-4-8",
			// Toggle off: no subagent rewrite; the default model applies, but subagent flag remains set.
			wantUp: "zai-org/GLM-5.2", wantRoute: "baseten", wantEff: "",
			wantSub: true, wantSubM: "", wantReq: "claude-opus-4-8",
		},
		{
			name: "off/header/anthropic", header: "agent-1", model: "claude-baseten-glm-5-2", routing: "off",
			route: "anthropic", reqModel: "claude-opus-4-8",
			// Toggle off, switch off: passthrough of original model.
			wantUp: "claude-opus-4-8", wantRoute: "anthropic", wantEff: "",
			wantSub: true, wantSubM: "", wantReq: "claude-opus-4-8",
		},
		// --- toggle UNSET (absent = on), header present ---
		{
			name: "unset/header/baseten", header: "agent-1", model: "claude-baseten-glm-5-2", routing: "",
			route: "baseten", reqModel: "claude-opus-4-8",
			wantUp: "zai-org/GLM-5.2", wantRoute: "baseten", wantEff: "",
			wantSub: true, wantSubM: "claude-baseten-glm-5-2", wantReq: "claude-opus-4-8",
		},
		// --- subagent_model UNSET, header present (no rewrite) ---
		{
			name: "nomodel/header/baseten", header: "agent-1", model: "", routing: "",
			route: "baseten", reqModel: "claude-opus-4-8",
			wantUp: "zai-org/GLM-5.2", wantRoute: "baseten", wantEff: "",
			wantSub: true, wantSubM: "", wantReq: "claude-opus-4-8",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotModel := make(chan string, 1)
			basSrv := basetenStub(t, gotModel)
			defer basSrv.Close()
			antSrv := anthropicStub(t, gotModel, nil)
			defer antSrv.Close()
			cfg := testConfig(t, basSrv.URL, antSrv.URL)
			rc := subagentClient(t, tc.route, tc.model, tc.routing)
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			resp, rb := postMessagesWithAgent(t, g, tc.reqModel, tc.header)
			if resp.StatusCode != 200 {
				t.Fatalf("got %d: %s", resp.StatusCode, rb)
			}
			select {
			case m := <-gotModel:
				if m != tc.wantUp {
					t.Fatalf("upstream got model %q, want %q", m, tc.wantUp)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("upstream never received the request")
			}
			rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
			row := rows[0]
			if row.Subagent != tc.wantSub {
				t.Errorf("subagent = %v, want %v", row.Subagent, tc.wantSub)
			}
			if valueOrZero(row.SubagentModel) != tc.wantSubM {
				t.Errorf("subagent_model = %q, want %q", valueOrZero(row.SubagentModel), tc.wantSubM)
			}
			if row.RequestedModel != tc.wantReq {
				t.Errorf("requested_model = %q, want %q (original)", row.RequestedModel, tc.wantReq)
			}
			if row.ConfiguredRoute != tc.wantRoute {
				t.Errorf("route = %q, want %q", row.ConfiguredRoute, tc.wantRoute)
			}
			wantEffective := tc.wantEff
			if wantEffective == "" {
				wantEffective = tc.wantRoute
			}
			if row.EffectiveProvider != wantEffective {
				t.Errorf("effective_provider = %q, want %q", row.EffectiveProvider, wantEffective)
			}
			if row.ServedModel != tc.wantUp {
				t.Errorf("upstream_model = %q, want %q", row.ServedModel, tc.wantUp)
			}
		})
	}
}

// TestSubagentPassthroughByteIdentical asserts that untoggled and
// header-absent native traffic forwards the exact original body bytes
// verbatim: the upstream receives the same JSON the harness sent, with
// no re-serialization.
func TestSubagentPassthroughByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name    string
		header  string
		model   string
		routing string
	}{
		{"no header", "", "claude-baseten-glm-5-2", "on"},
		{"header but toggle off", "agent-1", "claude-baseten-glm-5-2", "off"},
		{"header but no model", "agent-1", "", ""},
		{"no header no model", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotBody := make(chan []byte, 1)
			antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotBody <- b
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer antSrv.Close()
			cfg := testConfig(t, antSrv.URL, antSrv.URL)
			rc := subagentClient(t, "anthropic", tc.model, tc.routing)
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			// Use a body with non-canonical JSON spacing so any
			// re-serialization would change the bytes.
			orig := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
			resp, rb := postMessagesRaw(t, g, orig, tc.header)
			if resp.StatusCode != 200 {
				t.Fatalf("got %d: %s", resp.StatusCode, rb)
			}
			select {
			case got := <-gotBody:
				if !bytes.Equal(got, orig) {
					t.Fatalf("upstream body not byte-identical:\n got: %s\n want: %s", got, orig)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("upstream never received the request")
			}
		})
	}
}

// TestSubagentNativeOriginAliasTargetFallback preserves request ingress
// provenance: the alias target selects Baseten, while a status fallback and
// later cooldown bypass replay the original native request model.
func TestSubagentNativeOriginAliasTargetFallback(t *testing.T) {
	var primaryHits, fbHits int32
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	}))
	defer basSrv.Close()
	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fbHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer antSrv.Close()
	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	rc := subagentClient(t, "baseten", "claude-baseten-glm-5-2", "on")
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// Subagent alias request: Baseten 503 falls back with the original model.
	resp, rb := postMessagesWithAgent(t, g, "claude-opus-4-8", "agent-1")
	if resp.StatusCode != 200 || !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("subagent alias request got %d: %s, want fallback 200", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&fbHits); n != 1 {
		t.Fatalf("subagent alias fallback hits = %d, want 1", n)
	}
	// The status cooldown resolves the next native request independently.
	resp, rb = postMessagesWithAgent(t, g, "claude-opus-4-8", "")
	if resp.StatusCode != 200 || !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("native request got %d: %s, want fallback 200", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&fbHits); n != 2 {
		t.Fatalf("fallback hits after native cooldown request = %d, want 2", n)
	}
	resp, rb = postMessagesWithAgent(t, g, "claude-opus-4-8", "agent-1")
	if resp.StatusCode != 200 || !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("subagent alias cooldown request got %d: %s, want fallback 200", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&fbHits); n != 3 {
		t.Fatalf("fallback hits after subagent cooldown request = %d, want 3", n)
	}
	if n := atomic.LoadInt32(&primaryHits); n != 1 {
		t.Fatalf("primary hits = %d, want 1 before status cooldown", n)
	}
}

// TestSubagentNativeTargetFallbackWaterfall asserts a native-target
// subagent rewrite still walks the fallback waterfall on upstream 5xx:
// the rewritten native id falls through to the switch position, and the
// fallback_route is tried when the primary returns 5xx.
func TestSubagentNativeTargetFallbackWaterfall(t *testing.T) {
	var fbHits int32
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	}))
	defer basSrv.Close()
	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fbHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-sonnet-4-6","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer antSrv.Close()
	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	rc := subagentClient(t, "baseten", "claude-sonnet-4-6", "on")
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, rb := postMessagesWithAgent(t, g, "claude-opus-4-8", "agent-1")
	if resp.StatusCode != 200 || !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("native-target subagent request got %d: %s, want fallback 200", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&fbHits); n != 1 {
		t.Fatalf("fallback should have been tried once, got %d", n)
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	row := rows[0]
	if !row.Subagent {
		t.Errorf("subagent flag should be true")
	}
	if valueOrZero(row.SubagentModel) != "claude-sonnet-4-6" {
		t.Errorf("subagent_model = %q, want claude-sonnet-4-6", valueOrZero(row.SubagentModel))
	}
	if row.RequestedModel != "claude-opus-4-8" {
		t.Errorf("requested_model = %q, want original claude-opus-4-8", row.RequestedModel)
	}
	if primary := row.Primary; primary == nil ||
		primary.Provider != "baseten" ||
		primary.Model != "zai-org/GLM-5.2" ||
		!primary.Attempted ||
		primary.Outcome != telemetry.PrimaryOutcomeHTTPError ||
		primary.Status == nil || *primary.Status != http.StatusServiceUnavailable {
		t.Errorf("subagent primary = %+v, want attempted Baseten default target HTTP 503", primary)
	}
}

// TestSubagentSIGHUPReloadFlipsRouting verifies the reloadConfig path:
// starting with subagent_routing=off, flipping it to "on" in the
// config file and reloading changes behavior (hash change respawns the
// listener). The subagent rewrite goes from inactive to active.
func TestSubagentSIGHUPReloadFlipsRouting(t *testing.T) {
	gotModel := make(chan string, 1)
	basSrv := basetenStub(t, gotModel)
	defer basSrv.Close()
	antSrv := anthropicStub(t, gotModel, nil)
	defer antSrv.Close()

	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	port := freeTCPPort(t)
	bindAddr := "127.0.0.1:" + itoa(port)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")

	rc := subagentClient(t, "baseten", "anthropic-baseten-kimi", "off")
	rc.BindAddr = bindAddr
	writeSubagentYAML(t, cfg.ConfigPath, rc)

	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// Before reload: toggle off, so the normal global Baseten mapping
	// sends the request to GLM-5.2.
	resp, _ := postMessagesWithAgent(t, g, "claude-opus-4-8", "agent-1")
	if resp.StatusCode != 200 {
		t.Fatal("pre-reload request failed")
	}
	select {
	case m := <-gotModel:
		if m != "zai-org/GLM-5.2" {
			t.Fatalf("pre-reload upstream got %q, want default GLM-5.2", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pre-reload upstream never received the request")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if !rows[0].Subagent {
		t.Errorf("pre-reload subagent flag should be true (header present)")
	}
	if valueOrZero(rows[0].SubagentModel) != "" {
		t.Errorf("pre-reload subagent_model should be empty (toggle off)")
	}

	// Flip routing to "on" in the config file and reload.
	rc.SubagentRouting = "on"
	writeSubagentYAML(t, cfg.ConfigPath, rc)
	g.reloadConfig()

	// After reload: toggle on, subagent header triggers rewrite to Kimi.
	deadline := time.Now().Add(3 * time.Second)
	var ok bool
	for time.Now().Before(deadline) {
		resp, _ = postMessagesWithAgent(t, g, "claude-opus-4-8", "agent-1")
		if resp.StatusCode != 200 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case m := <-gotModel:
			if m == "moonshotai/Kimi-K2.7-Code" {
				ok = true
			}
		case <-time.After(500 * time.Millisecond):
		}
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		t.Fatal("after reload the subagent rewrite never activated (upstream never got Kimi)")
	}
	rows = waitForRows(t, cfg.TelemetryDir, 2, 3*time.Second)
	last := rows[len(rows)-1]
	if !last.Subagent {
		t.Errorf("post-reload subagent flag should be true")
	}
	if valueOrZero(last.SubagentModel) != "anthropic-baseten-kimi" {
		t.Errorf("post-reload subagent_model = %q, want anthropic-baseten-kimi", valueOrZero(last.SubagentModel))
	}
	if last.RequestedModel != "claude-opus-4-8" {
		t.Errorf("post-reload requested_model = %q, want original claude-opus-4-8", last.RequestedModel)
	}
	if last.EffectiveProvider != "baseten" {
		t.Errorf("post-reload route_effective = %q, want baseten", last.EffectiveProvider)
	}
}

// TestSubagentHashCoversSubagentFields verifies that changing
// subagent_model or subagent_routing changes the resolvedClientConfig
// hash, so SIGHUP respawns the listener.
func TestSubagentHashCoversSubagentFields(t *testing.T) {
	base := resolvedClientConfig{
		Name:          "claude-code",
		BindAddr:      "127.0.0.1:18081",
		ProtocolShape: "anthropic",
		Route:         "baseten",
	}
	a := base
	a.SubagentModel = "claude-baseten-glm-5-2"
	if base.hash() == a.hash() {
		t.Fatal("hash must change when subagent_model is set")
	}
	b := a
	b.SubagentRouting = "on"
	if a.hash() == b.hash() {
		t.Fatal("hash must change when subagent_routing is set")
	}
	c := b
	c.SubagentModel = "moonshotai/Kimi-K2.7-Code"
	if b.hash() == c.hash() {
		t.Fatal("hash must change when subagent_model value changes")
	}
	d := b
	d.SubagentRouting = "off"
	if b.hash() == d.hash() {
		t.Fatal("hash must change when subagent_routing value changes")
	}
}

// TestSubagentAdminStatusFields verifies admin status serves the two new
// fields in both the config-readable and snapshot fallback branches.
func TestSubagentAdminStatusFields(t *testing.T) {
	t.Run("config readable", func(t *testing.T) {
		basSrv := basetenStub(t, nil)
		defer basSrv.Close()
		cfg := testConfig(t, basSrv.URL, basSrv.URL)
		cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
		rc := subagentClient(t, "baseten", "claude-baseten-glm-5-2", "on")
		writeSubagentYAML(t, cfg.ConfigPath, rc)
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		status := adminStatusGet(t, g)
		clients := status["clients"].([]any)
		if len(clients) == 0 {
			t.Fatal("no clients in admin status")
		}
		c := clients[0].(map[string]any)
		if c["subagent_model"] != "claude-baseten-glm-5-2" {
			t.Errorf("subagent_model = %v, want claude-baseten-glm-5-2", c["subagent_model"])
		}
		if c["subagent_routing"] != "on" {
			t.Errorf("subagent_routing = %v, want on", c["subagent_routing"])
		}
	})
	t.Run("snapshot fallback", func(t *testing.T) {
		basSrv := basetenStub(t, nil)
		defer basSrv.Close()
		cfg := testConfig(t, basSrv.URL, basSrv.URL)
		// Point ConfigPath at a nonexistent path so config.Load fails
		// and adminStatus falls to the snapshot branch.
		cfg.ConfigPath = filepath.Join(t.TempDir(), "nonexistent.yaml")
		rc := subagentClient(t, "baseten", "claude-baseten-glm-5-2", "on")
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		status := adminStatusGet(t, g)
		clients := status["clients"].([]any)
		if len(clients) == 0 {
			t.Fatal("no clients in admin status")
		}
		c := clients[0].(map[string]any)
		if c["subagent_model"] != "claude-baseten-glm-5-2" {
			t.Errorf("subagent_model = %v, want claude-baseten-glm-5-2", c["subagent_model"])
		}
		if c["subagent_routing"] != "on" {
			t.Errorf("subagent_routing = %v, want on", c["subagent_routing"])
		}
	})
}

// TestSubagentConfigValidation exercises validateSubagentConfig error
// cases: non-anthropic shape, bad routing value, routing without model,
// alias-namespace model absent from model_aliases, and a value matching
// none of the three classes.
func TestSubagentConfigValidation(t *testing.T) {
	base := func(mut func(*config.Client)) *config.File {
		enabled := true
		c := config.Client{
			Name:          "claude-code",
			Enabled:       true,
			BindAddr:      "127.0.0.1:0",
			ProtocolShape: "anthropic",
			DefaultModel:  "zai-org/GLM-5.2",
			ModelAliases:  map[string]string{"claude-baseten-glm-5-2": "zai-org/GLM-5.2"},
		}
		mut(&c)
		return &config.File{
			Global: config.Global{
				RoutingEnabled: &enabled,
			},
			Clients: []config.Client{c},
		}
	}
	for _, tc := range []struct {
		name    string
		mut     func(*config.Client)
		wantErr string
	}{
		{"valid alias", func(c *config.Client) {
			c.SubagentModel = "claude-baseten-glm-5-2"
		}, ""},
		{"valid slug", func(c *config.Client) {
			c.SubagentModel = "moonshotai/Kimi-K2.7-Code"
		}, ""},
		{"valid native", func(c *config.Client) {
			c.SubagentModel = "claude-sonnet-4-6"
		}, ""},
		{"valid routing on", func(c *config.Client) {
			c.SubagentModel = "claude-baseten-glm-5-2"
			c.SubagentRouting = "on"
		}, ""},
		{"valid routing off", func(c *config.Client) {
			c.SubagentModel = "claude-baseten-glm-5-2"
			c.SubagentRouting = "off"
		}, ""},
		{"unset is ok", func(c *config.Client) {}, ""},
		{"non-anthropic shape with aliases", func(c *config.Client) {
			// Inherited non-empty ModelAliases makes validateModelAliases
			// fire first (its own shape guard), so this case exercises that
			// guard, not validateSubagentConfig's.
			c.ProtocolShape = "openai"
			c.SubagentModel = "claude-baseten-glm-5-2"
		}, "protocol_shape anthropic"},
		{"non-anthropic shape", func(c *config.Client) {
			// Clear ModelAliases so validateModelAliases is a no-op and
			// validateSubagentConfig's own shape guard is reached.
			c.ModelAliases = map[string]string{}
			c.ProtocolShape = "openai"
			c.SubagentModel = "claude-baseten-glm-5-2"
		}, "subagent_model requires protocol_shape anthropic"},
		{"bad routing value", func(c *config.Client) {
			c.SubagentModel = "claude-baseten-glm-5-2"
			c.SubagentRouting = "maybe"
		}, "must be"},
		{"routing without model", func(c *config.Client) {
			c.SubagentRouting = "on"
		}, "subagent_model is empty"},
		{"alias namespace absent from model_aliases", func(c *config.Client) {
			c.SubagentModel = "claude-baseten-removed"
		}, "absent from model_aliases"},
		{"not any of three classes", func(c *config.Client) {
			c.SubagentModel = "gpt-4-turbo"
		}, "not a configured alias"},
		{"disabled client skipped", func(c *config.Client) {
			c.Enabled = false
			c.SubagentModel = "gpt-4-turbo"
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

// TestSubagentUnparseableBodySkipsRewrite asserts the spec invariant: if
// the body does not parse as JSON, the subagent gate skips the rewrite
// and passes through untouched (degrade to pass-through).
func TestSubagentUnparseableBodySkipsRewrite(t *testing.T) {
	gotBody := make(chan []byte, 1)
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody <- b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"x","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer basSrv.Close()
	cfg := testConfig(t, basSrv.URL, basSrv.URL)
	rc := subagentClient(t, "baseten", "claude-baseten-glm-5-2", "on")
	// Keep this test focused on the subagent gate's malformed-body
	// passthrough contract by selecting Follow Harness for the default
	// toggle model. Default Off cannot rewrite malformed JSON.
	rc.DefaultModel = "moonshotai/Kimi-K2.7-Code"
	rc.ModelOptions = config.ModelOptions{
		"baseten": {
			"moonshotai/Kimi-K2.7-Code": {
				Reasoning: &config.ReasoningPolicy{
					Mode: config.ReasoningFollowHarness,
				},
			},
		},
	}
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// A body that is not valid JSON.
	orig := []byte(`not-json-at-all`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(orig))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set(subagentAgentIDHeaderTest, "agent-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case got := <-gotBody:
		if !bytes.Equal(got, orig) {
			t.Fatalf("unparseable body should pass through verbatim:\n got: %s\n want: %s", got, orig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the request")
	}
}

// TestSubagentRequestedModelPreserved asserts requested_model in
// telemetry is the original harness model even after the rewrite,
// across target classes. This is the spec's telemetry invariant.
func TestSubagentRequestedModelPreserved(t *testing.T) {
	for _, target := range []string{"claude-baseten-glm-5-2", "moonshotai/Kimi-K2.7-Code", "claude-sonnet-4-6"} {
		t.Run(target, func(t *testing.T) {
			gotModel := make(chan string, 1)
			basSrv := basetenStub(t, gotModel)
			defer basSrv.Close()
			antSrv := anthropicStub(t, gotModel, nil)
			defer antSrv.Close()
			cfg := testConfig(t, basSrv.URL, antSrv.URL)
			rc := subagentClient(t, "anthropic", target, "on")
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			// Harness sends claude-opus-4-8; subagent rewrite changes it.
			resp, _ := postMessagesWithAgent(t, g, "claude-opus-4-8", "agent-1")
			if resp.StatusCode != 200 {
				t.Fatal("request failed")
			}
			rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
			row := rows[0]
			if row.RequestedModel != "claude-opus-4-8" {
				t.Errorf("requested_model = %q, want original claude-opus-4-8", row.RequestedModel)
			}
			if valueOrZero(row.SubagentModel) != target {
				t.Errorf("subagent_model = %q, want %q", valueOrZero(row.SubagentModel), target)
			}
			if !row.Subagent {
				t.Errorf("subagent flag should be true")
			}
		})
	}
}

// TestSubagentNativeTargetFamilyRoute asserts that a dedicated native
// subagent model re-enters the normal family router. A Sonnet subagent
// request therefore uses the configured Sonnet family route.
func TestSubagentNativeTargetFamilyRoute(t *testing.T) {
	gotModel := make(chan string, 1)
	basSrv := basetenStub(t, gotModel)
	defer basSrv.Close()
	cfg := testConfig(t, basSrv.URL, basSrv.URL)
	rc := subagentClient(t, "baseten", "claude-sonnet-4-6", "on")
	rc.ModelRoutes = map[string]string{"sonnet": "moonshotai/Kimi-K2.7-Code"}
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, _ := postMessagesWithAgent(t, g, "claude-opus-4-8", "agent-1")
	if resp.StatusCode != 200 {
		t.Fatal("request failed")
	}
	select {
	case m := <-gotModel:
		if m != "moonshotai/Kimi-K2.7-Code" {
			t.Fatalf("upstream got %q, want moonshotai/Kimi-K2.7-Code (Sonnet family route)", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the request")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	row := rows[0]
	if row.RequestedModel != "claude-opus-4-8" {
		t.Errorf("requested_model = %q, want original claude-opus-4-8", row.RequestedModel)
	}
	if valueOrZero(row.SubagentModel) != "claude-sonnet-4-6" {
		t.Errorf("subagent_model = %q, want claude-sonnet-4-6", valueOrZero(row.SubagentModel))
	}
}

// writeSubagentYAML writes a gateway.yaml for one resolved client,
// including model_aliases, subagent_model, and subagent_routing.
func writeSubagentYAML(t *testing.T, path string, rc resolvedClientConfig) {
	t.Helper()
	enabled := rc.Route == "baseten"
	f := config.File{Global: config.Global{
		RoutingEnabled: &enabled,
	}}
	c := config.Client{
		Name:            rc.Name,
		Enabled:         true,
		BindAddr:        rc.BindAddr,
		ProtocolShape:   rc.ProtocolShape,
		DefaultModel:    rc.DefaultModel,
		ModelAliases:    rc.ModelAliases,
		ModelRoutes:     rc.ModelRoutes,
		SubagentModel:   rc.SubagentModel,
		SubagentRouting: rc.SubagentRouting,
		FallbackRoute:   rc.FallbackRoute,
	}
	f.Clients = append(f.Clients, c)
	if err := config.Save(path, &f); err != nil {
		t.Fatal(err)
	}
}

// adminStatusGet fetches the admin status JSON and unmarshals it.
func adminStatusGet(t *testing.T, g *Gateway) map[string]any {
	t.Helper()
	resp, err := http.Get(adminURL(g, "/v1/admin/status"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("bad admin status %s: %v", b, err)
	}
	return out
}
