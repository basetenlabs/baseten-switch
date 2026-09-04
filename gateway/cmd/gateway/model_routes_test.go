// model_routes_test.go covers the gateway half of config/schema.md:
// the model_routes family pin overlay, the resolution hook in
// resolveAttemptsLadder, native pins and unpinned traffic,
// context-selection canonicalization, subagent-rewrite interplay, fallback
// waterfall from the original id, SIGHUP reload, load-time validation,
// and the admin status JSON contract (model_routes, families,
// model_catalog). All upstreams are hermetic httptest servers; no real
// state is touched.
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

// modelRoutesClient returns an anthropic-shape resolved client with
// model_aliases and the given model_routes, on the given route.
func modelRoutesClient(t *testing.T, rt string, routes map[string]string) resolvedClientConfig {
	t.Helper()
	rc := resolvedAnthropicBaseten(t)
	rc.Route = rt
	rc.GlobalRoutingEnabled = rt == "baseten"
	rc.ModelAliases = map[string]string{
		"claude-baseten-glm-5-2":  "zai-org/GLM-5.2",
		"anthropic-baseten-kimi":  "moonshotai/Kimi-K2.7-Code",
		"claude-baseten-nemotron": "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B",
	}
	rc.ModelRoutes = routes
	return rc
}

// postModelMessages sends a /v1/messages request with the given model
// and optional agent-id header. Returns the response and body.
func postModelMessages(t *testing.T, g *Gateway, model, agentID string) (*http.Response, string) {
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

// postModelMessagesRaw sends a /v1/messages request with the exact body
// bytes and returns the response and the body the upstream received.
// Used for the byte-identical passthrough assertion.
func postModelMessagesRaw(t *testing.T, g *Gateway, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, rb
}

// recordingStub returns an httptest server that records the model it
// received into gotModel and replies with a 200 anthropic-shape message
// tagged with marker so the caller can tell which upstream answered.
func recordingStub(t *testing.T, gotModel chan string, marker string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if gotModel != nil {
			gotModel <- fmtString(m["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"` + marker + `"}],"model":"x","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
}

// TestNormalizeModelID exercises normalizeModelID: only the documented [1m]
// suffix is stripped, case-insensitively. Other decorations are identifiers.
func TestNormalizeModelID(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"claude-opus-4-8", "claude-opus-4-8"},
		{"claude-opus-4-8[1m]", "claude-opus-4-8"},
		{"claude-opus-4-8[1M]", "claude-opus-4-8"},
		{"claude-fable-5[1m]", "claude-fable-5"},
		{"claude-sonnet-4-6[1m]", "claude-sonnet-4-6"},
		{"claude-3-5-sonnet-20241022", "claude-3-5-sonnet-20241022"},
		{"", ""},
		{"[1m]", ""},
		{"claude-opus-4-8[2m]", "claude-opus-4-8[2m]"},
		{"claude-opus-4-8[extra]", "claude-opus-4-8[extra]"},
		{"claude-opus-4-8[1m][extra]", "claude-opus-4-8[1m][extra]"},
	} {
		if got := normalizeModelID(tc.in); got != tc.want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestConfiguredAliasAcceptsOneMillionContextSuffix(t *testing.T) {
	gotModel := make(chan string, 1)
	basSrv := recordingStub(t, gotModel, "B")
	defer basSrv.Close()
	cfg := testConfig(t, basSrv.URL, basSrv.URL)
	rc := modelRoutesClient(t, "baseten", nil)
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, body := postModelMessages(
		t,
		g,
		"claude-baseten-glm-5-2[1m]",
		"",
	)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}
	select {
	case model := <-gotModel:
		if model != "zai-org/GLM-5.2" {
			t.Fatalf("upstream model = %q, want exact alias target", model)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the request")
	}
}

// TestFamilyOf exercises familyOf across current ids, dated ids, suffixed
// ids, and unknown ids. The first family token found wins.
func TestFamilyOf(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"claude-opus-4-8", "opus"},
		{"claude-sonnet-4-6", "sonnet"},
		{"claude-haiku-4-5", "haiku"},
		{"claude-fable-5", "fable"},
		{"claude-fable-5[1m]", "fable"},
		{"claude-opus-4-8[1m]", "opus"},
		{"claude-3-5-sonnet-20241022", "sonnet"},
		{"claude-3-5-haiku-20241022", "haiku"},
		{"claude-3-opus-20240229", "opus"},
		{"CLAUDE-OPUS-4-8", "opus"},
		{"Claude-Fable-5", "fable"},
		{"claude-instant-1.2", ""},
		{"claude-mythos-5", ""},
		{"gpt-4-turbo", ""},
		{"claude-baseten-glm-5-2", ""},
		{"", ""},
		{"zai-org/GLM-5.2", ""},
	} {
		if got := familyOf(tc.in); got != tc.want {
			t.Errorf("familyOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestModelRouteMatrix exercises the full matrix: family pin value class
// (native/alias/slug) x switch position, plus unpinned traffic. For each
// cell it asserts the upstream model received, the route taken, and
// telemetry conventions (route_effective, requested_model, upstream_model).
func TestModelRouteMatrix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		routes    map[string]string // model_routes config
		route     string            // switch position
		reqModel  string            // model the harness sends
		wantUp    string            // model the upstream should receive
		wantRoute string            // telemetry route
		wantEff   string            // telemetry route_effective
	}{
		// --- family pin: native ---
		{
			name: "family-native/baseten", routes: map[string]string{"opus": "native"},
			route: "baseten", reqModel: "claude-opus-4-8",
			// Native pin overrides the switch: passthrough to anthropic.
			wantUp: "claude-opus-4-8", wantRoute: "baseten", wantEff: "anthropic",
		},
		{
			name: "family-native/anthropic", routes: map[string]string{"opus": "native"},
			route: "anthropic", reqModel: "claude-opus-4-8",
			// Native pin on an already-native switch: passthrough, no eff.
			wantUp: "claude-opus-4-8", wantRoute: "anthropic", wantEff: "",
		},
		// --- family pin: alias ---
		{
			name: "family-alias/baseten", routes: map[string]string{"sonnet": "claude-baseten-glm-5-2"},
			route: "baseten", reqModel: "claude-sonnet-4-6",
			// Alias pin forces the alias slug, bypassing the default model.
			wantUp: "zai-org/GLM-5.2", wantRoute: "baseten", wantEff: "",
		},
		{
			name: "family-alias/anthropic", routes: map[string]string{"sonnet": "claude-baseten-glm-5-2"},
			route: "anthropic", reqModel: "claude-sonnet-4-6",
			// Alias pin overrides native switch: baseten with forced slug.
			wantUp: "zai-org/GLM-5.2", wantRoute: "anthropic", wantEff: "baseten",
		},
		// --- family pin: slug ---
		{
			name: "family-slug/baseten", routes: map[string]string{"haiku": "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"},
			route: "baseten", reqModel: "claude-haiku-4-5",
			wantUp: "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B", wantRoute: "baseten", wantEff: "",
		},
		{
			name: "family-slug/anthropic", routes: map[string]string{"haiku": "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"},
			route: "anthropic", reqModel: "claude-haiku-4-5",
			wantUp: "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B", wantRoute: "anthropic", wantEff: "baseten",
		},
		// --- no pins: switch position only (byte-identical to today) ---
		{
			name: "no-pins/baseten", routes: nil,
			route: "baseten", reqModel: "claude-opus-4-8",
			wantUp: "zai-org/GLM-5.2", wantRoute: "baseten", wantEff: "",
		},
		{
			name: "no-pins/anthropic", routes: nil,
			route: "anthropic", reqModel: "claude-opus-4-8",
			wantUp: "claude-opus-4-8", wantRoute: "anthropic", wantEff: "",
		},
		// --- unpinned family follows the switch ---
		{
			name: "unpinned-family/baseten", routes: map[string]string{"opus": "native"},
			route: "baseten", reqModel: "claude-sonnet-4-6",
			// Sonnet is unpinned; the Baseten switch applies the default model.
			wantUp: "zai-org/GLM-5.2", wantRoute: "baseten", wantEff: "",
		},
		{
			name: "unpinned-family/anthropic", routes: map[string]string{"opus": "native"},
			route: "anthropic", reqModel: "claude-sonnet-4-6",
			wantUp: "claude-sonnet-4-6", wantRoute: "anthropic", wantEff: "",
		},
		// --- no-family id follows the switch (not family-routed) ---
		{
			name: "no-family/baseten", routes: map[string]string{"opus": "native"},
			route: "baseten", reqModel: "claude-instant-1.2",
			// instant is not a configurable family; the default model applies.
			wantUp: "zai-org/GLM-5.2", wantRoute: "baseten", wantEff: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotModel := make(chan string, 2)
			basSrv := recordingStub(t, gotModel, "VIA-BASETEN")
			defer basSrv.Close()
			antSrv := recordingStub(t, gotModel, "VIA-ANTHROPIC")
			defer antSrv.Close()
			cfg := testConfig(t, basSrv.URL, antSrv.URL)
			rc := modelRoutesClient(t, tc.route, tc.routes)
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			resp, rb := postModelMessages(t, g, tc.reqModel, "")
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
			if row.ConfiguredRoute != tc.wantRoute {
				t.Errorf("route = %q, want %q", row.ConfiguredRoute, tc.wantRoute)
			}
			wantEffective := tc.wantEff
			if wantEffective == "" {
				wantEffective = tc.route
			}
			if row.EffectiveProvider != wantEffective {
				t.Errorf("effective_provider = %q, want %q", row.EffectiveProvider, wantEffective)
			}
			if row.ServedModel != tc.wantUp {
				t.Errorf("upstream_model = %q, want %q", row.ServedModel, tc.wantUp)
			}
			if row.RequestedModel != tc.reqModel {
				t.Errorf("requested_model = %q, want %q (original)", row.RequestedModel, tc.reqModel)
			}
		})
	}
}

// TestModelRouteSuffixedIDMatching asserts that a [1m] harness selection
// still matches its family pin while the canonical provider model omits the
// selection suffix.
func TestModelRouteSuffixedIDMatching(t *testing.T) {
	for _, tc := range []struct {
		name     string
		routes   map[string]string
		route    string
		reqModel string // suffixed
		wantUp   string
		wantEff  string
	}{
		{
			name:   "family-pin matches suffixed id",
			routes: map[string]string{"fable": "native"},
			route:  "baseten", reqModel: "claude-fable-5[1m]",
			wantUp: "claude-fable-5", wantEff: "anthropic",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotModel := make(chan string, 2)
			basSrv := recordingStub(t, gotModel, "B")
			defer basSrv.Close()
			antSrv := recordingStub(t, gotModel, "A")
			defer antSrv.Close()
			cfg := testConfig(t, basSrv.URL, antSrv.URL)
			rc := modelRoutesClient(t, tc.route, tc.routes)
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			resp, rb := postModelMessages(t, g, tc.reqModel, "")
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
			wantEffective := tc.wantEff
			if wantEffective == "" {
				wantEffective = tc.route
			}
			if rows[0].EffectiveProvider != wantEffective {
				t.Errorf("effective_provider = %q, want %q", rows[0].EffectiveProvider, wantEffective)
			}
		})
	}
}

func TestResolveModelRoutePinUsesCatalogFamily(t *testing.T) {
	rc := modelRoutesClient(
		t,
		"baseten",
		map[string]string{"opus": "native"},
	)
	if pin := resolveModelRoutePin(
		rc,
		"claude-release-without-known-family-token",
	); pin.pinned {
		t.Fatalf("parser-only resolution unexpectedly pinned: %+v", pin)
	}
	pin := resolveModelRoutePinWithFamily(
		rc,
		"claude-release-without-known-family-token",
		"opus",
	)
	if !pin.pinned || pin.route != pricing.ProviderAnthropic {
		t.Fatalf("catalog family pin = %+v", pin)
	}
}

// TestModelRoutePassthroughByteIdentical asserts that a native pin and
// unpinned native traffic forward the exact original body bytes verbatim
// to the anthropic upstream: no re-serialization.
func TestModelRoutePassthroughByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name   string
		routes map[string]string
		route  string
	}{
		{"native pin", map[string]string{"opus": "native"}, "baseten"},
		{"unpinned on native switch", nil, "anthropic"},
		{"unpinned family while another family pinned", map[string]string{"sonnet": "native"}, "anthropic"},
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
			rc := modelRoutesClient(t, tc.route, tc.routes)
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			// Non-canonical JSON spacing so any re-serialization changes bytes.
			orig := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
			resp, rb := postModelMessagesRaw(t, g, orig)
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

// TestModelRouteFallbackOriginalID asserts the spec's fallback semantics:
// a pinned-family request that gets 5xx from the primary falls through to
// the fallback_route built from the ORIGINAL requested id, which works at
// Anthropic. The fallback is NOT built from the family target.
func TestModelRouteFallbackOriginalID(t *testing.T) {
	var fbHits int32
	gotPrimaryHeaders := make(chan http.Header, 1)
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrimaryHeaders <- r.Header.Clone()
		// Primary (baseten, forced target) returns 503.
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	}))
	defer basSrv.Close()
	gotFbModel := make(chan string, 1)
	gotFallbackHeaders := make(chan http.Header, 1)
	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fbHits, 1)
		gotFallbackHeaders <- r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		gotFbModel <- fmtString(m["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"FALLBACK"}],"model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer antSrv.Close()
	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	rc := modelRoutesClient(t, "baseten", map[string]string{"opus": "claude-baseten-glm-5-2"})
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// Request claude-opus-4-8; pin forces baseten with GLM. Primary 503
	// -> fallback to anthropic with the ORIGINAL id.
	body := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("User-Agent", "header-isolation-test/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(rb), "FALLBACK") {
		t.Fatalf("got %d: %s, want fallback 200", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&fbHits); n != 1 {
		t.Fatalf("fallback should have been tried once, got %d", n)
	}
	select {
	case m := <-gotFbModel:
		// The fallback must receive the ORIGINAL id, not the family target.
		if m != "claude-opus-4-8" {
			t.Fatalf("fallback upstream got model %q, want original claude-opus-4-8", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fallback upstream never received the request")
	}
	select {
	case headers := <-gotPrimaryHeaders:
		wantUserAgent := "baseten-switch/" + version.Version + " header-isolation-test/1.0"
		if got := headers.Get("User-Agent"); got != wantUserAgent {
			t.Errorf("Baseten User-Agent = %q, want %q", got, wantUserAgent)
		}
		if got := headers.Get(switchVersionHeader); got != version.Version {
			t.Errorf("Baseten %s = %q, want %q", switchVersionHeader, got, version.Version)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Baseten primary headers were not captured")
	}
	select {
	case headers := <-gotFallbackHeaders:
		if got := headers.Get("User-Agent"); got != "header-isolation-test/1.0" {
			t.Errorf("Anthropic User-Agent = %q, want unchanged", got)
		}
		if got := headers.Get(switchVersionHeader); got != "" {
			t.Errorf("Anthropic %s = %q, want empty", switchVersionHeader, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Anthropic fallback headers were not captured")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	// The fallback attempt served with route_effective=anthropic (differs
	// from the configured baseten route).
	if rows[0].EffectiveProvider != "anthropic" {
		t.Errorf("route_effective = %q, want anthropic (fallback served)", rows[0].EffectiveProvider)
	}
	if primary := rows[0].Primary; primary == nil ||
		primary.Provider != "baseten" ||
		primary.Model != "zai-org/GLM-5.2" ||
		!primary.Attempted ||
		primary.Outcome != telemetry.PrimaryOutcomeHTTPError ||
		primary.Status == nil || *primary.Status != http.StatusServiceUnavailable {
		t.Errorf("primary = %+v, want attempted baseten zai-org/GLM-5.2 HTTP 503", primary)
	}
}

// TestModelRouteTargetPinSwitchOffWaterfall asserts the feature's
// headline fallback property: with the switch OFF (route anthropic) and
// fallback_route anthropic (equal to the route, the designed dormant
// state), a family pinned to a Baseten target still HAS the waterfall.
// The pinned primary is baseten with the forced target; when it 503s the
// request is served by anthropic with the ORIGINAL requested id.
func TestModelRouteTargetPinSwitchOffWaterfall(t *testing.T) {
	var basHits int32
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&basHits, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	}))
	defer basSrv.Close()
	gotModel := make(chan string, 1)
	antSrv := recordingStub(t, gotModel, "VIA-ANTHROPIC")
	defer antSrv.Close()
	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	rc := modelRoutesClient(t, "anthropic", map[string]string{"sonnet": "claude-baseten-glm-5-2"})
	// fallback_route equals the configured route: dormant for unpinned
	// traffic, live against the pinned baseten primary.
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, rb := postModelMessages(t, g, "claude-sonnet-4-6", "")
	if resp.StatusCode != 200 || !strings.Contains(rb, "VIA-ANTHROPIC") {
		t.Fatalf("got %d: %s, want anthropic fallback 200", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&basHits); n != 1 {
		t.Fatalf("baseten (pinned primary) hits = %d, want 1", n)
	}
	select {
	case m := <-gotModel:
		if m != "claude-sonnet-4-6" {
			t.Fatalf("fallback upstream got model %q, want ORIGINAL claude-sonnet-4-6", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("anthropic fallback never received the request")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	// The fallback served through Anthropic. V1 always records the final
	// effective provider, even when it equals the configured route. The row
	// must carry the original id, not the family target.
	if rows[0].EffectiveProvider != "anthropic" {
		t.Errorf("route_effective = %q, want anthropic", rows[0].EffectiveProvider)
	}
	if rows[0].ServedModel != "claude-sonnet-4-6" {
		t.Errorf("upstream_model = %q, want claude-sonnet-4-6 (fallback with original id)", rows[0].ServedModel)
	}
	if rows[0].StatusCode() != 200 || rows[0].IsHTTPError() {
		t.Errorf("status/errored = %d/%v, want 200/false", rows[0].StatusCode(), rows[0].IsHTTPError())
	}
}

// TestModelRouteNativePinSwitchOnSingleAttempt asserts fallback
// inertness against the EFFECTIVE primary route: switch ON (route
// baseten) with fallback_route anthropic and opus pinned native. The
// primary is anthropic; the fallback (also anthropic) is dormant for
// this request, so a failing anthropic upstream is hit exactly once (no
// duplicate same-route retry), the client sees the error, and the
// cooldown is NOT tripped: a follow-up unpinned request still reaches
// baseten instead of being promoted to the (dead) fallback.
func TestModelRouteNativePinSwitchOnSingleAttempt(t *testing.T) {
	var antHits, basHits int32
	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&antHits, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	}))
	defer antSrv.Close()
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&basHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"VIA-BASETEN"}],"model":"x","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer basSrv.Close()
	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	rc := modelRoutesClient(t, "baseten", map[string]string{"opus": "native"})
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// Pinned-native request: single anthropic attempt, error surfaces.
	resp, rb := postModelMessages(t, g, "claude-opus-4-8", "")
	if resp.StatusCode != 503 {
		t.Fatalf("got %d: %s, want 503 (single attempt, no fallback)", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&antHits); n != 1 {
		t.Fatalf("anthropic hits = %d, want exactly 1 (no duplicate same-route retry)", n)
	}
	if n := atomic.LoadInt32(&basHits); n != 0 {
		t.Fatalf("baseten hits = %d, want 0", n)
	}

	// Follow-up unpinned request: the failed single attempt must not have
	// tripped the cooldown, so the baseten primary serves it. (A tripped
	// cooldown would promote the anthropic fallback, which 503s.)
	resp, rb = postModelMessages(t, g, "claude-sonnet-4-6", "")
	if resp.StatusCode != 200 || !strings.Contains(rb, "VIA-BASETEN") {
		t.Fatalf("follow-up got %d: %s, want baseten 200 (cooldown must not be tripped)", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&basHits); n != 1 {
		t.Fatalf("baseten hits = %d, want 1 (follow-up served by primary)", n)
	}
}

// TestModelRouteSwitchOffUnpinnedSingleAttempt is the regression guard
// for pre-feature behavior: switch OFF (route anthropic) with
// fallback_route anthropic and no matching pin serves exactly one native
// attempt, same as when the dormant fallback used to be cleared at load.
func TestModelRouteSwitchOffUnpinnedSingleAttempt(t *testing.T) {
	var antHits, basHits int32
	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&antHits, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	}))
	defer antSrv.Close()
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&basHits, 1)
		w.WriteHeader(500)
	}))
	defer basSrv.Close()
	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	rc := modelRoutesClient(t, "anthropic", nil)
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, rb := postModelMessages(t, g, "claude-opus-4-8", "")
	if resp.StatusCode != 503 {
		t.Fatalf("got %d: %s, want 503 surfaced from the single attempt", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&antHits); n != 1 {
		t.Fatalf("anthropic hits = %d, want exactly 1 (dormant fallback, no retry)", n)
	}
	if n := atomic.LoadInt32(&basHits); n != 0 {
		t.Fatalf("baseten hits = %d, want 0", n)
	}
}

// TestModelRouteCooldownPromotionUnpinned asserts the cooldown promotion
// branch still works for genuinely different routes when model_routes
// pins exist: unpinned baseten traffic with an anthropic fallback trips
// the cooldown on a 503 and the next request skips the primary.
func TestModelRouteCooldownPromotionUnpinned(t *testing.T) {
	var basHits int32
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&basHits, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	}))
	defer basSrv.Close()
	antSrv := recordingStub(t, nil, "FALLBACK")
	defer antSrv.Close()
	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	// A pin on opus exists but the requests target unpinned sonnet, so
	// the primary is the configured baseten route.
	rc := modelRoutesClient(t, "baseten", map[string]string{"opus": "native"})
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, rb := postModelMessages(t, g, "claude-sonnet-4-6", "")
	if resp.StatusCode != 200 || !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("got %d: %s, want fallback 200", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&basHits); n != 1 {
		t.Fatalf("baseten hits = %d, want 1", n)
	}
	// Cooldown active: the second request must not touch the primary.
	resp, rb = postModelMessages(t, g, "claude-sonnet-4-6", "")
	if resp.StatusCode != 200 || !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("second request got %d: %s, want fallback 200", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&basHits); n != 1 {
		t.Fatalf("baseten hit during cooldown: hits = %d, want 1", n)
	}
}

// TestModelRouteSubagentInterplay asserts the spec's subagent interplay:
// a native subagent_model in a pinned family gets the pin applied to the
// rewritten id; an alias subagent target stays explicit and skips
// family logic.
func TestModelRouteSubagentInterplay(t *testing.T) {
	t.Run("native subagent target in pinned family gets pin", func(t *testing.T) {
		gotModel := make(chan string, 2)
		basSrv := recordingStub(t, gotModel, "B")
		defer basSrv.Close()
		antSrv := recordingStub(t, gotModel, "A")
		defer antSrv.Close()
		cfg := testConfig(t, basSrv.URL, antSrv.URL)
		rc := modelRoutesClient(t, "anthropic", map[string]string{"sonnet": "claude-baseten-glm-5-2"})
		// Subagent rewrite to a native sonnet id; the sonnet family pin
		// then applies to the rewritten id.
		rc.SubagentModel = "claude-sonnet-4-6"
		rc.SubagentRouting = "on"
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		// Harness sends opus; subagent gate rewrites to sonnet; family pin
		// forces the sonnet family target (GLM) via baseten.
		resp, _ := postModelMessages(t, g, "claude-opus-4-8", "agent-1")
		if resp.StatusCode != 200 {
			t.Fatal("request failed")
		}
		select {
		case m := <-gotModel:
			if m != "zai-org/GLM-5.2" {
				t.Fatalf("upstream got %q, want zai-org/GLM-5.2 (pin applied to rewritten id)", m)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("upstream never received the request")
		}
		rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
		if rows[0].RequestedModel != "claude-opus-4-8" {
			t.Errorf("requested_model = %q, want original claude-opus-4-8", rows[0].RequestedModel)
		}
		if valueOrZero(rows[0].SubagentModel) != "claude-sonnet-4-6" {
			t.Errorf("subagent_model = %q, want claude-sonnet-4-6", valueOrZero(rows[0].SubagentModel))
		}
	})
	t.Run("alias subagent target stays explicit, skips family logic", func(t *testing.T) {
		gotModel := make(chan string, 2)
		basSrv := recordingStub(t, gotModel, "B")
		defer basSrv.Close()
		antSrv := recordingStub(t, gotModel, "A")
		defer antSrv.Close()
		cfg := testConfig(t, basSrv.URL, antSrv.URL)
		// opus pinned native, but the subagent target is an alias, which
		// is explicit-choice-wins and skips the family pin.
		rc := modelRoutesClient(t, "anthropic", map[string]string{"opus": "native"})
		rc.SubagentModel = "claude-baseten-glm-5-2"
		rc.SubagentRouting = "on"
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		resp, _ := postModelMessages(t, g, "claude-opus-4-8", "agent-1")
		if resp.StatusCode != 200 {
			t.Fatal("request failed")
		}
		select {
		case m := <-gotModel:
			// Alias target honored verbatim (explicit choice), not native.
			if m != "zai-org/GLM-5.2" {
				t.Fatalf("upstream got %q, want zai-org/GLM-5.2 (alias explicit, skips family pin)", m)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("upstream never received the request")
		}
	})
}

// TestModelRouteSIGHUPReload verifies the reloadConfig path: starting
// with no pins (switch Baseten, default model applies), adding a family pin in the
// config file and reloading changes behavior (hash change respawns the
// listener). The pin goes from inactive to active.
func TestModelRouteSIGHUPReload(t *testing.T) {
	gotModel := make(chan string, 2)
	basSrv := recordingStub(t, gotModel, "B")
	defer basSrv.Close()
	antSrv := recordingStub(t, gotModel, "A")
	defer antSrv.Close()

	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	port := freeTCPPort(t)
	bindAddr := "127.0.0.1:" + itoa(port)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")

	rc := modelRoutesClient(t, "baseten", nil) // no pins
	rc.BindAddr = bindAddr
	writeModelRoutesYAML(t, cfg.ConfigPath, rc)

	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// Before reload: no pins, Baseten switch, default model -> GLM.
	resp, _ := postModelMessages(t, g, "claude-opus-4-8", "")
	if resp.StatusCode != 200 {
		t.Fatal("pre-reload request failed")
	}
	select {
	case m := <-gotModel:
		if m != "zai-org/GLM-5.2" {
			t.Fatalf("pre-reload upstream got %q, want default zai-org/GLM-5.2", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pre-reload upstream never received the request")
	}

	// Add a native pin for opus and reload.
	rc.ModelRoutes = map[string]string{"opus": "native"}
	writeModelRoutesYAML(t, cfg.ConfigPath, rc)
	g.reloadConfig()

	// After reload: opus pinned native -> passthrough to anthropic.
	deadline := time.Now().Add(3 * time.Second)
	var ok bool
	for time.Now().Before(deadline) {
		resp, _ = postModelMessages(t, g, "claude-opus-4-8", "")
		if resp.StatusCode != 200 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case m := <-gotModel:
			if m == "claude-opus-4-8" {
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
		t.Fatal("after reload the native pin never activated (upstream never got claude-opus-4-8 passthrough)")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 2, 3*time.Second)
	last := rows[len(rows)-1]
	if last.EffectiveProvider != "anthropic" {
		t.Errorf("post-reload route_effective = %q, want anthropic (native pin on baseten switch)", last.EffectiveProvider)
	}
}

// TestModelRouteHashCoversModelRoutes verifies that changing model_routes
// changes the resolvedClientConfig hash, so SIGHUP respawns the listener.
func TestModelRouteHashCoversModelRoutes(t *testing.T) {
	base := resolvedClientConfig{
		Name:          "claude-code",
		BindAddr:      "127.0.0.1:18081",
		ProtocolShape: "anthropic",
		Route:         "baseten",
	}
	a := base
	a.ModelRoutes = map[string]string{"opus": "native"}
	if base.hash() == a.hash() {
		t.Fatal("hash must change when model_routes is set")
	}
	b := a
	b.ModelRoutes = map[string]string{"opus": "native", "sonnet": "claude-baseten-glm-5-2"}
	if a.hash() == b.hash() {
		t.Fatal("hash must change when a model_routes entry is added")
	}
	c := b
	c.ModelRoutes = map[string]string{"opus": "claude-baseten-glm-5-2", "sonnet": "claude-baseten-glm-5-2"}
	if b.hash() == c.hash() {
		t.Fatal("hash must change when a model_routes value changes")
	}
	d := b
	d.ModelRoutes = map[string]string{"sonnet": "claude-baseten-glm-5-2"}
	if b.hash() == d.hash() {
		t.Fatal("hash must change when a model_routes entry is removed")
	}
}

// TestModelRouteConfigValidation exercises validateModelRoutes error
// cases: non-anthropic shape, non-family keys,
// empty value, alias-namespace value absent from model_aliases, and a
// value matching none of the three classes.
func TestModelRouteConfigValidation(t *testing.T) {
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
		{"empty map ok", func(c *config.Client) {
			c.ModelRoutes = map[string]string{}
		}, ""},
		{"valid family native", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"opus": "native"}
		}, ""},
		{"valid family alias", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"sonnet": "claude-baseten-glm-5-2"}
		}, ""},
		{"valid family slug", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"haiku": "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"}
		}, ""},
		{"exact id rejected", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"claude-opus-4-8": "native"}
		}, "is invalid"},
		{"non-anthropic shape", func(c *config.Client) {
			// Clear ModelAliases so validateModelAliases is a no-op and
			// validateModelRoutes's own shape guard is reached.
			c.ModelAliases = map[string]string{}
			c.ProtocolShape = "openai"
			c.ModelRoutes = map[string]string{"opus": "native"}
		}, "model_routes requires protocol_shape anthropic"},
		{"empty value", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"opus": ""}
		}, "empty value"},
		{"bracketed key", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"claude-opus-4-8[1m]": "native"}
		}, "is invalid"},
		{"slash key rejected", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"zai-org/GLM-5.2": "native"}
		}, "is invalid"},
		{"non-prefix non-family key", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"gpt-4-turbo": "native"}
		}, "is invalid"},
		{"alias-namespace key (configured alias)", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"claude-baseten-glm-5-2": "native"}
		}, "is invalid"},
		{"alias-namespace key (unconfigured)", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"anthropic-baseten-kimi": "native"}
		}, "is invalid"},
		{"alias-namespace value absent from model_aliases", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"opus": "claude-baseten-removed"}
		}, "absent from model_aliases"},
		{"value not any of three classes", func(c *config.Client) {
			c.ModelRoutes = map[string]string{"opus": "gpt-4-turbo"}
		}, "not \"native\""},
		{"disabled client skipped", func(c *config.Client) {
			c.Enabled = false
			c.ModelRoutes = map[string]string{"opus": "gpt-4-turbo"}
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

func TestAdminConfigPutRejectsInvalidRouteFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	original := []byte("global:\n  routing_enabled: true\nclients: []\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{cfg: Config{ConfigPath: path}}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "exact model_routes key",
			body: `{"global":{"routing_enabled":true},"clients":[{"name":"claude-code","protocol_shape":"anthropic","model_routes":{"claude-opus-4-8":"native"}}]}`,
			want: "model_routes key",
		},
		{
			name: "trailing JSON document",
			body: `{"global":{"routing_enabled":true},"clients":[]} {}`,
			want: "multiple JSON values",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/v1/admin/config", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			g.adminConfig(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status=%d body=%s, want 400 containing %q", rec.Code, rec.Body.String(), tc.want)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("rejected PUT changed config:\n%s", got)
			}
		})
	}
}

// TestModelRouteAdminStatusContract verifies the admin status JSON
// contract: model_routes raw family pins, the families effective table,
// and model_catalog dedup. Both the config-readable and snapshot
// branches are covered.
func TestModelRouteAdminStatusContract(t *testing.T) {
	routes := map[string]string{
		"opus":   "native",
		"sonnet": "claude-baseten-glm-5-2",
	}
	assertContract := func(t *testing.T, c map[string]any) {
		// model_routes: raw pins.
		mr, ok := c["model_routes"].(map[string]any)
		if !ok {
			t.Fatalf("model_routes = %v, want a map", c["model_routes"])
		}
		if len(mr) != 2 {
			t.Errorf("model_routes has %d entries, want 2", len(mr))
		}
		if mr["opus"] != "native" {
			t.Errorf("model_routes[opus] = %v, want native", mr["opus"])
		}
		// families: one entry per family.
		fams, ok := c["families"].([]any)
		if !ok {
			t.Fatalf("families = %v, want a slice", c["families"])
		}
		if len(fams) != 4 {
			t.Fatalf("families has %d entries, want 4 (one per family)", len(fams))
		}
		byFam := map[string]map[string]any{}
		for _, f := range fams {
			fe := f.(map[string]any)
			byFam[fe["family"].(string)] = fe
		}
		// Opus: the family pin routes native.
		opus := byFam["opus"]
		if _, ok := opus["pin"]; ok {
			t.Error("opus unexpectedly exposes removed pin field")
		}
		if opus["configured_target"] != "native" || opus["configured_source"] != "explicit" {
			t.Errorf("opus configured state = target %v source %v, want native/explicit",
				opus["configured_target"], opus["configured_source"])
		}
		if opus["effective_route"] != "anthropic" {
			t.Errorf("opus effective_route = %v, want anthropic", opus["effective_route"])
		}
		if opus["effective_model"] != "" {
			t.Errorf("opus effective_model = %v, want \"\" (native passthrough)", opus["effective_model"])
		}
		// sonnet: family pin alias -> baseten GLM.
		sonnet := byFam["sonnet"]
		if sonnet["configured_target"] != "claude-baseten-glm-5-2" ||
			sonnet["configured_source"] != "explicit" {
			t.Errorf("sonnet configured state = target %v source %v, want alias/explicit",
				sonnet["configured_target"], sonnet["configured_source"])
		}
		if sonnet["effective_route"] != "baseten" {
			t.Errorf("sonnet effective_route = %v, want baseten", sonnet["effective_route"])
		}
		if sonnet["effective_model"] != "zai-org/GLM-5.2" {
			t.Errorf("sonnet effective_model = %v, want zai-org/GLM-5.2", sonnet["effective_model"])
		}
		// haiku: unpinned, follows the switch (Baseten) -> default model.
		haiku := byFam["haiku"]
		if haiku["configured_target"] != "zai-org/GLM-5.2" ||
			haiku["configured_source"] != "default" {
			t.Errorf("haiku configured state = target %v source %v, want default model/default",
				haiku["configured_target"], haiku["configured_source"])
		}
		if haiku["effective_route"] != "baseten" {
			t.Errorf("haiku effective_route = %v, want baseten (switch)", haiku["effective_route"])
		}
		if haiku["effective_model"] != "zai-org/GLM-5.2" {
			t.Errorf("haiku effective_model = %v, want zai-org/GLM-5.2 (default model)", haiku["effective_model"])
		}
		// model_catalog: aliases (target = alias id) plus the default model when not
		// covered by an alias, deduped by slug.
		cat, ok := c["model_catalog"].([]any)
		if !ok {
			t.Fatalf("model_catalog = %v, want a slice", c["model_catalog"])
		}
		seenSlugs := map[string]bool{}
		for _, e := range cat {
			ce := e.(map[string]any)
			slug := ce["slug"].(string)
			if seenSlugs[slug] {
				t.Errorf("model_catalog has duplicate slug %q", slug)
			}
			seenSlugs[slug] = true
		}
		// The alias claude-baseten-glm-5-2 -> zai-org/GLM-5.2 and default
		// model zai-org/GLM-5.2 dedup to one entry.
		if !seenSlugs["zai-org/GLM-5.2"] {
			t.Errorf("model_catalog missing zai-org/GLM-5.2 (alias or default model)")
		}
		// Alias targets retain their raw IDs while labels come from shared
		// model metadata.
		foundAliasTarget := false
		wantLabels := map[string]string{
			"zai-org/GLM-5.2":                          "GLM 5.2",
			"moonshotai/Kimi-K2.7-Code":                "Kimi K2.7 Code",
			"nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B": "NVIDIA Nemotron 3 Ultra 550B A55B",
		}
		for _, e := range cat {
			ce := e.(map[string]any)
			if _, ok := ce["target"]; ok {
				t.Errorf("model_catalog unexpectedly exposes removed target field: %v", ce)
			}
			if ce["alias"] == "claude-baseten-glm-5-2" &&
				ce["storage_target"] == "zai-org/GLM-5.2" {
				foundAliasTarget = true
			}
			slug := ce["slug"].(string)
			if want, ok := wantLabels[slug]; ok && ce["label"] != want {
				t.Errorf("label for %s = %v, want %q", slug, ce["label"], want)
			}
		}
		if !foundAliasTarget {
			t.Errorf("model_catalog missing alias target claude-baseten-glm-5-2")
		}
	}

	t.Run("config readable", func(t *testing.T) {
		basSrv := recordingStub(t, nil, "B")
		defer basSrv.Close()
		cfg := testConfig(t, basSrv.URL, basSrv.URL)
		cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
		rc := modelRoutesClient(t, "baseten", routes)
		writeModelRoutesYAML(t, cfg.ConfigPath, rc)
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		status := adminStatusGet(t, g)
		clients := status["clients"].([]any)
		if len(clients) == 0 {
			t.Fatal("no clients in admin status")
		}
		assertContract(t, clients[0].(map[string]any))
	})
	t.Run("snapshot fallback", func(t *testing.T) {
		basSrv := recordingStub(t, nil, "B")
		defer basSrv.Close()
		cfg := testConfig(t, basSrv.URL, basSrv.URL)
		// Nonexistent path -> config.Load fails -> snapshot branch.
		cfg.ConfigPath = filepath.Join(t.TempDir(), "nonexistent.yaml")
		rc := modelRoutesClient(t, "baseten", routes)
		g, adminL, _ := newGateway(t, cfg, rc)
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		status := adminStatusGet(t, g)
		clients := status["clients"].([]any)
		if len(clients) == 0 {
			t.Fatal("no clients in admin status")
		}
		assertContract(t, clients[0].(map[string]any))
	})
}

func TestComputeModelCatalogIsProtocolAgnostic(t *testing.T) {
	catalog := computeModelCatalog(resolvedClientConfig{
		ProtocolShape: "openai",
		DefaultModel:  "zai-org/GLM-5.2",
	})
	if len(catalog) != 1 {
		t.Fatalf("catalog length = %d, want 1", len(catalog))
	}
	if catalog[0].Slug != "zai-org/GLM-5.2" ||
		catalog[0].Label != "GLM 5.2" {
		t.Fatalf("catalog[0] = %#v, want raw slug plus display label", catalog[0])
	}
}

func TestComputeModelCatalogRetainsConfiguredRawSlugs(t *testing.T) {
	catalog := computeModelCatalog(resolvedClientConfig{
		ProtocolShape: "anthropic",
		ModelAliases: map[string]string{
			"claude-baseten-glm-5-2": "zai-org/GLM-5.2",
		},
		DefaultModel: "zai-org/GLM-5.2",
		ModelRoutes: map[string]string{
			"opus":   "moonshotai/Kimi-K3",
			"sonnet": "claude-baseten-glm-5-2",
			"haiku":  "deepseek-ai/DeepSeek-V4-Pro",
		},
		SubagentModel: "deepseek-ai/DeepSeek-V4-Pro",
	})

	want := []modelCatalogEntry{
		{
			Label: "GLM 5.2",
			Slug:  "zai-org/GLM-5.2", StorageTarget: "zai-org/GLM-5.2",
			Alias: "claude-baseten-glm-5-2", Available: true,
		},
		{
			Label:         "DeepSeek V4 Pro",
			Slug:          "deepseek-ai/DeepSeek-V4-Pro",
			StorageTarget: "deepseek-ai/DeepSeek-V4-Pro", Available: true,
		},
		{
			Label:         "Kimi K3",
			Slug:          "moonshotai/Kimi-K3",
			StorageTarget: "moonshotai/Kimi-K3", Available: true,
		},
	}
	if !reflect.DeepEqual(catalog, want) {
		t.Fatalf("catalog = %#v, want %#v", catalog, want)
	}
}

// TestModelRouteAdminStatusNativeSwitch asserts the families effective
// table reflects the switch position for unpinned families: with the
// switch off (anthropic), an unpinned family shows effective_route
// anthropic and effective_model "" (native passthrough).
func TestModelRouteAdminStatusNativeSwitch(t *testing.T) {
	basSrv := recordingStub(t, nil, "B")
	defer basSrv.Close()
	cfg := testConfig(t, basSrv.URL, basSrv.URL)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
	rc := modelRoutesClient(t, "anthropic", map[string]string{"opus": "native"})
	writeModelRoutesYAML(t, cfg.ConfigPath, rc)
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	status := adminStatusGet(t, g)
	if got := status["config_path"]; got != cfg.ConfigPath {
		t.Fatalf("config_path = %v, want %s", got, cfg.ConfigPath)
	}
	clients := status["clients"].([]any)
	c := clients[0].(map[string]any)
	fams := c["families"].([]any)
	for _, f := range fams {
		fe := f.(map[string]any)
		fam := fe["family"].(string)
		// All families on an anthropic switch: effective_route anthropic,
		// effective_model "" (native passthrough). opus is pinned native
		// (same result); the rest follow the switch.
		if fe["effective_route"] != "anthropic" {
			t.Errorf("%s effective_route = %v, want anthropic", fam, fe["effective_route"])
		}
		if fe["effective_model"] != "" {
			t.Errorf("%s effective_model = %v, want \"\" (native passthrough)", fam, fe["effective_model"])
		}
	}
}

// TestValidModelRouteKey exercises the family-only key predicate.
func TestValidModelRouteKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"opus", true},
		{"fable", true},
		{"sonnet", true},
		{"haiku", true},
		{"claude-opus-4-8", false},
		{"claude-3-5-sonnet-20241022", false},
		{"anthropic-foo.bar_baz", false},
		{"", false},
		{"zai-org/GLM-5.2", false},
		{"claude-opus-4-8[1m]", false},
		{"a b", false},
		{"opus,sonnet", false},
	} {
		if got := ValidModelRouteKey(tc.key); got != tc.want {
			t.Errorf("ValidModelRouteKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestComputeFamiliesFamilyPin asserts the family row reflects the
// configured family pin.
func TestComputeFamiliesFamilyPin(t *testing.T) {
	rc := modelRoutesClient(t, "baseten", map[string]string{
		"opus": "native",
	})
	fams := computeFamilies(rc)
	var opus *familyEntry
	for i := range fams {
		if fams[i].Family == "opus" {
			opus = &fams[i]
		}
	}
	if opus == nil {
		t.Fatal("no opus family row")
	}
	if opus.ConfiguredTarget == nil || *opus.ConfiguredTarget != "native" {
		t.Errorf("configured target = %v, want native", opus.ConfiguredTarget)
	}
	if opus.EffectiveRoute != "anthropic" {
		t.Errorf("effective_route = %q, want anthropic (family key only)", opus.EffectiveRoute)
	}
	if opus.EffectiveModel != "" {
		t.Errorf("effective_model = %q, want \"\" (native passthrough)", opus.EffectiveModel)
	}
}

// writeModelRoutesYAML writes a gateway.yaml for one resolved client,
// including model_aliases, model_routes, and the other resolved fields.
func writeModelRoutesYAML(t *testing.T, path string, rc resolvedClientConfig) {
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
