package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

func noNativePolicyFile() *config.File {
	noNative, routing := false, false
	return &config.File{Global: config.Global{RoutingEnabled: &routing}, Clients: []config.Client{{
		Name: "claude-desktop", Enabled: true, BindAddr: "127.0.0.1:45274", ProtocolShape: "anthropic", AllowNative: &noNative,
		ModelAliases: map[string]string{"claude-baseten-alpha": "example/Alpha", "claude-baseten-beta": "example/Beta", "claude-baseten-removed": "example/Removed"},
		ModelPicker:  &config.ModelPicker{Enabled: true, Models: []config.ModelPickerModel{{Alias: "claude-baseten-beta"}, {Alias: "claude-baseten-alpha"}}},
	}}}
}

// Exercise real handlers without starting background refresh or auth loops.
func noNativeTestGateway(t *testing.T, upstream, native string) (*Gateway, *clientListener) {
	t.Helper()
	resolved, err := resolveFromFile(noNativePolicyFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, upstream, native)
	cfg.OpenAIURL = native
	collection := false
	cfg.TelemetryEnabled = &collection
	return &Gateway{cfg: cfg, pricing: pricing.New(), client: &http.Client{Timeout: time.Second}, ctx: context.Background()}, &clientListener{name: resolved[0].Name, cfg: resolved[0]}
}

func TestNoNativeSelectedAliasesAreIndependentOfRoutingToggle(t *testing.T) {
	for _, routing := range []bool{false, true} {
		file := noNativePolicyFile()
		file.Global.RoutingEnabled = &routing
		resolved, err := resolveFromFile(file)
		if err != nil {
			t.Fatal(err)
		}
		rc := resolved[0]
		if rc.Route != "baseten" || !rc.NativeDisabled || rc.usesGlobalRouting() || rc.globalRoutingOff() {
			t.Fatalf("routing=%t policy=%+v", routing, rc)
		}
		if !reflect.DeepEqual(rc.SelectedModelAliases, []string{"claude-baseten-beta", "claude-baseten-alpha"}) {
			t.Fatalf("selected order=%v", rc.SelectedModelAliases)
		}
		if got := ExpectedPrimaryRoute(file.Clients[0], routing, "claude-baseten-alpha"); got != "baseten" {
			t.Fatalf("ExpectedPrimaryRoute=%q", got)
		}
		changed := rc
		changed.SelectedModelAliases = []string{"claude-baseten-alpha", "claude-baseten-beta"}
		if changed.hash() == rc.hash() {
			t.Fatal("picker order absent from runtime hash")
		}
		changed = rc
		changed.NativeDisabled = false
		if changed.hash() == rc.hash() {
			t.Fatal("native policy absent from runtime hash")
		}
	}
}

func TestNoNativeInferenceNeverEscapesToNative(t *testing.T) {
	const alpha = `{"model":"claude-baseten-alpha","messages":[{"role":"user","content":"synthetic request"}],"max_tokens":8}`
	const image = `{"model":"claude-baseten-alpha","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}]}],"max_tokens":8}`
	for _, test := range []struct {
		name               string
		body               string
		status, wantStatus int
		wantModel          string
		edit               func(*Gateway, *clientListener)
		behavior           string
	}{
		{name: "exact alpha", body: alpha, status: 200, wantStatus: 200, wantModel: "example/Alpha"},
		{name: "exact beta", body: strings.ReplaceAll(alpha, "alpha", "beta"), status: 200, wantStatus: 200, wantModel: "example/Beta"},
		{name: "opaque alpha", body: strings.ReplaceAll(alpha, "claude-baseten-alpha", desktopModelID("claude-baseten-alpha", "example/Alpha")), status: 200, wantStatus: 200, wantModel: "example/Alpha"},
		{name: "opaque beta", body: strings.ReplaceAll(alpha, "claude-baseten-alpha", desktopModelID("claude-baseten-beta", "example/Beta")), status: 200, wantStatus: 200, wantModel: "example/Beta"},
		{name: "removed opaque ID", body: strings.ReplaceAll(alpha, "claude-baseten-alpha", desktopModelID("claude-baseten-removed", "example/Removed")), wantStatus: 400},
		{name: "changed target invalidates opaque ID", body: strings.ReplaceAll(alpha, "claude-baseten-alpha", desktopModelID("claude-baseten-alpha", "example/Other")), wantStatus: 400},
		{name: "other client rejects opaque ID", body: strings.ReplaceAll(alpha, "claude-baseten-alpha", desktopModelID("claude-baseten-alpha", "example/Alpha")), wantStatus: 400, edit: func(_ *Gateway, cl *clientListener) { cl.cfg.Name = "another-client" }},
		{name: "opaque rate limit stays terminal", body: strings.ReplaceAll(alpha, "claude-baseten-alpha", desktopModelID("claude-baseten-alpha", "example/Alpha")), status: 429, wantStatus: 429, wantModel: "example/Alpha"},
		{name: "ambiguous legacy identity is rejected", body: strings.ReplaceAll(alpha, "claude-baseten-alpha", desktopModelID("claude-baseten-alpha", "example/Alpha")), wantStatus: 400, edit: func(_ *Gateway, cl *clientListener) {
			id := desktopModelID("claude-baseten-alpha", "example/Alpha")
			cl.cfg.ModelAliases[id] = "example/Beta"
			cl.cfg.SelectedModelAliases = append(cl.cfg.SelectedModelAliases, id)
		}},
		{name: "unknown", body: strings.ReplaceAll(alpha, "alpha", "unknown"), wantStatus: 400},
		{name: "removed", body: strings.ReplaceAll(alpha, "alpha", "removed"), wantStatus: 400},
		{name: "native model", body: strings.ReplaceAll(alpha, "claude-baseten-alpha", "claude-opus-5"), wantStatus: 400},
		{name: "raw slug", body: strings.ReplaceAll(alpha, "claude-baseten-alpha", "example/Alpha"), wantStatus: 400},
		{name: "decorated alias", body: strings.ReplaceAll(alpha, "alpha", "alpha[1m]"), wantStatus: 400},
		{name: "malformed", body: `{"model":`, wantStatus: 400},
		{name: "missing model", body: `{}`, wantStatus: 400},
		{name: "classifier alias", body: string(syntheticAutoPermissionBody("claude-baseten-alpha")), status: 200, wantStatus: 200, wantModel: "example/Alpha", edit: func(_ *Gateway, cl *clientListener) { cl.cfg.Name = "claude-code" }},
		{name: "classifier native model", body: string(syntheticAutoPermissionBody("claude-opus-5")), wantStatus: 400, edit: func(_ *Gateway, cl *clientListener) { cl.cfg.Name = "claude-code" }},
		{name: "missing auth", body: alpha, wantStatus: 503, edit: func(g *Gateway, _ *clientListener) { g.cfg.BasetenKey = ""; g.cfg.APIKeyFallback = false }},
		{name: "empty selected list", body: alpha, wantStatus: 400, edit: func(_ *Gateway, cl *clientListener) { cl.cfg.SelectedModelAliases = nil }},
		{name: "unsupported translated image", body: image, wantStatus: 400, edit: func(_ *Gateway, cl *clientListener) { cl.cfg.UpstreamShape = "openai" }},
		{name: "native family pin", body: strings.ReplaceAll(alpha, "claude-baseten-alpha", "claude-opus-5"), wantStatus: 400, edit: func(_ *Gateway, cl *clientListener) { cl.cfg.ModelRoutes = map[string]string{"opus": "native"} }},
		{name: "image rejection", body: image, status: 400, wantStatus: 400, wantModel: "example/Alpha"},
		{name: "rate limit", body: alpha, status: 429, wantStatus: 429, wantModel: "example/Alpha"},
		{name: "server failure", body: alpha, status: 503, wantStatus: 503, wantModel: "example/Alpha"},
		{name: "transport failure", body: alpha, behavior: "disconnect", wantStatus: 502, wantModel: "example/Alpha"},
		{name: "TTFT timeout", body: alpha, behavior: "timeout", wantStatus: 504, wantModel: "example/Alpha", edit: func(_ *Gateway, cl *clientListener) { cl.cfg.TTFTTimeout = 20 * time.Millisecond }},
		{name: "redirect", body: alpha, behavior: "redirect", wantStatus: 502, wantModel: "example/Alpha"},
		{name: "request build failure", body: alpha, wantStatus: 502, edit: func(g *Gateway, _ *clientListener) { g.cfg.BasetenURL = "://invalid" }},
		{name: "subagent target cannot replace alias", body: alpha, status: 200, wantStatus: 200, wantModel: "example/Alpha", edit: func(_ *Gateway, cl *clientListener) { cl.cfg.SubagentModel = "example/Worker" }},
		{name: "native pin cannot replace alias", body: alpha, status: 200, wantStatus: 200, wantModel: "example/Alpha", edit: func(_ *Gateway, cl *clientListener) { cl.cfg.ModelRoutes = map[string]string{"opus": "native"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var nativeHits, basetenHits atomic.Int32
			native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { nativeHits.Add(1); w.WriteHeader(500) }))
			defer native.Close()
			baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				basetenHits.Add(1)
				var payload struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload.Model != test.wantModel {
					t.Errorf("Baseten model=%q, want %q", payload.Model, test.wantModel)
				}
				if got := r.Header.Get("Authorization"); got != "Api-Key bas-key" {
					t.Errorf("Baseten authorization mode=%q", got)
				}
				switch test.behavior {
				case "disconnect":
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				case "timeout":
					<-r.Context().Done()
					return
				case "redirect":
					http.Redirect(w, r, native.URL+"/v1/messages", http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{"id":"msg_synthetic","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer baseten.Close()
			g, cl := noNativeTestGateway(t, baseten.URL, native.URL)
			// A stale or embedded fallback must not bypass the terminal policy.
			cl.cfg.FallbackRoute = "anthropic"
			cl.cfg.NativeFallbackModel = "claude-opus-5"
			g.tripFallback(cl.cfg.Name, fallbackCooldownHTTP5xx)
			if test.edit != nil {
				test.edit(g, cl)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(test.body))
			r.Header.Set("Authorization", "Bearer synthetic-ingress")
			r.Header.Set(subagentAgentIDHeader, "synthetic-worker")
			w := httptest.NewRecorder()
			g.handleClient(cl, w, r)
			if w.Code != test.wantStatus {
				t.Fatalf("status=%d, want %d: %s", w.Code, test.wantStatus, w.Body.String())
			}
			if nativeHits.Load() != 0 {
				t.Fatalf("native requests=%d, want zero", nativeHits.Load())
			}
			wantHits := int32(0)
			if test.wantModel != "" {
				wantHits = 1
			}
			if basetenHits.Load() != wantHits {
				t.Fatalf("Baseten requests=%d, want %d", basetenHits.Load(), wantHits)
			}
			if w.Header().Get("Location") != "" {
				t.Fatal("upstream redirect escaped through response")
			}
		})
	}
}

func TestNoNativeAttemptBuilderRejectsEveryOtherRoute(t *testing.T) {
	g, cl := noNativeTestGateway(t, "http://example.invalid", "http://example.invalid")
	for _, route := range []string{"anthropic", "openai", "monitor", "unknown"} {
		_, err := g.buildAttempt(cl, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), []byte(`{"model":"claude-opus-5"}`), route, "messages")
		if err == nil || !strings.Contains(err.Error(), "does not allow native provider routing") {
			t.Fatalf("route %q error=%v", route, err)
		}
	}
}

func TestNoNativeDiscoveryIsLocalSelectedAndOrdered(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { upstreamHits.Add(1); w.WriteHeader(500) }))
	defer upstream.Close()
	g, cl := noNativeTestGateway(t, upstream.URL, upstream.URL)
	if err := g.pricing.ReplaceModelsDev([]byte(publicCatalogGatewayFixture), time.Now(), "synthetic"); err != nil {
		t.Fatal(err)
	}
	g.cfg.BasetenKey = ""
	g.cfg.APIKeyFallback = false
	cl.cfg.Route = "anthropic"
	alphaID := desktopModelID("claude-baseten-alpha", "example/Alpha")
	betaID := desktopModelID("claude-baseten-beta", "example/Beta")
	for _, test := range []struct {
		query string
		ids   []string
		more  bool
	}{
		{"", []string{betaID, alphaID}, false},
		{"?limit=1", []string{betaID}, true},
		{"?limit=1&after_id=" + betaID, []string{alphaID}, false},
	} {
		w := httptest.NewRecorder()
		g.handleClient(cl, w, httptest.NewRequest(http.MethodGet, "/v1/models"+test.query, nil))
		var list modelsList
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(list.Data))
		for _, entry := range list.Data {
			ids = append(ids, entry.ID)
			if entry.DisplayName == "" {
				t.Fatal("missing label")
			}
		}
		if w.Code != 200 || !reflect.DeepEqual(ids, test.ids) || list.HasMore != test.more {
			t.Fatalf("query %q: status=%d list=%+v", test.query, w.Code, list)
		}
	}
	cl.cfg.SelectedModelAliases = nil
	w := httptest.NewRecorder()
	g.handleClient(cl, w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !bytes.Contains(w.Body.Bytes(), []byte(`"data":[]`)) {
		t.Fatalf("empty selected list=%s", w.Body.String())
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("discovery upstream requests=%d", upstreamHits.Load())
	}
}

func TestNoNativeStatusExposesIndependentPolicy(t *testing.T) {
	g, cl := noNativeTestGateway(t, "http://example.invalid", "http://example.invalid")
	file := noNativePolicyFile()
	g.cfg.ConfigPath = t.TempDir() + "/absent.yaml"
	g.activeConfigFile = file
	g.clients = map[string]*clientListener{cl.cfg.Name: cl}
	w := httptest.NewRecorder()
	g.adminStatus(w, httptest.NewRequest(http.MethodGet, "/v1/admin/status", nil))
	var status struct {
		Clients []struct {
			AllowNative       bool            `json:"allow_native"`
			UsesGlobalRouting bool            `json:"uses_global_routing"`
			EffectiveRoute    string          `json:"effective_route"`
			Selected          []string        `json:"selected_model_aliases"`
			Families          []familyEntry   `json:"families"`
			Fallback          json.RawMessage `json:"baseten_model_fallback"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(status.Clients) != 1 {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	client := status.Clients[0]
	if client.AllowNative || client.UsesGlobalRouting || client.EffectiveRoute != "baseten" || len(client.Families) != 0 || len(client.Fallback) != 0 || !reflect.DeepEqual(client.Selected, cl.cfg.SelectedModelAliases) {
		t.Fatalf("client policy = %+v", client)
	}
	health := httptest.NewRecorder()
	g.handleClient(cl, health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !bytes.Contains(health.Body.Bytes(), []byte(`"allow_native":false`)) || !bytes.Contains(health.Body.Bytes(), []byte(`"uses_global_routing":false`)) {
		t.Fatalf("client health = %s", health.Body.String())
	}
}

func TestNoNativeSelectedAliasUsesTranslatedBasetenEndpoint(t *testing.T) {
	var nativeHits atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { nativeHits.Add(1); w.WriteHeader(500) }))
	defer native.Close()
	baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "example/Alpha" {
			t.Errorf("upstream model = %q", body.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_synthetic","object":"chat.completion","model":"example/Alpha","choices":[{"index":0,"message":{"role":"assistant","content":"translated result"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer baseten.Close()
	g, cl := noNativeTestGateway(t, baseten.URL, native.URL)
	cl.cfg.UpstreamShape = "openai"
	w := httptest.NewRecorder()
	g.handleClient(cl, w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-baseten-alpha","messages":[{"role":"user","content":"synthetic"}],"max_tokens":8}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"text":"translated result"`) || nativeHits.Load() != 0 {
		t.Fatalf("status=%d native=%d body=%s", w.Code, nativeHits.Load(), w.Body.String())
	}
}
