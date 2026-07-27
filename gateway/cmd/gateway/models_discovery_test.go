// models_discovery_test.go covers the the model-discovery contract work:
// /v1/models alias synthesis, explicit-choice-wins routing, raw-slug
// routing, the unknown-alias guard, and config-load validation. All
// upstreams are hermetic httptest servers; no real state is touched.
package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

// aliasedClient returns an anthropic-shape resolved client with two
// model_aliases configured, on the given route.
func aliasedClient(t *testing.T, rt string) resolvedClientConfig {
	t.Helper()
	rc := resolvedAnthropicBaseten(t)
	rc.Route = rt
	rc.GlobalRoutingEnabled = rt == "baseten"
	rc.ModelAliases = map[string]string{
		"claude-baseten-glm-5-2": "zai-org/GLM-5.2",
		"anthropic-baseten-kimi": "moonshotai/Kimi-K2.7-Code",
	}
	return rc
}

type modelsList struct {
	Data []struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	} `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID *string `json:"first_id"`
	LastID  *string `json:"last_id"`
}

func getModelsList(t *testing.T, g *Gateway, name, query string) (int, modelsList) {
	t.Helper()
	req, _ := http.NewRequest("GET", clientURL(g, name, "/v1/models"+query), nil)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-Api-Key", "sk-harness")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var ml modelsList
	if err := json.Unmarshal(b, &ml); err != nil {
		t.Fatalf("bad models body %s: %v", b, err)
	}
	return resp.StatusCode, ml
}

// TestAliasModelsSynthesisServedLocally: with model_aliases configured,
// GET /v1/models is synthesized without contacting any upstream on the
// baseten/monitor/openai routes, in the Anthropic list shape, sorted by
// alias id, and every id survives the picker's claude/anthropic prefix
// filter.
func TestAliasModelsSynthesisServedLocally(t *testing.T) {
	for _, rt := range []string{"baseten", "monitor", "openai"} {
		t.Run(rt, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("upstream must not be contacted for alias /v1/models (route %s), got %s %s", rt, r.Method, r.URL.Path)
			}))
			defer srv.Close()
			cfg := testConfig(t, srv.URL, srv.URL)
			cfg.OpenAIURL = srv.URL
			g, adminL, _ := newGateway(t, cfg, aliasedClient(t, rt))
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			status, ml := getModelsList(t, g, "claude-code", "?limit=1000")
			if status != 200 {
				t.Fatalf("GET /v1/models got %d", status)
			}
			if len(ml.Data) != 2 {
				t.Fatalf("data has %d entries, want 2: %+v", len(ml.Data), ml.Data)
			}
			// Stable ordering: sorted by alias id.
			if ml.Data[0].ID != "anthropic-baseten-kimi" || ml.Data[1].ID != "claude-baseten-glm-5-2" {
				t.Fatalf("alias order wrong: %+v", ml.Data)
			}
			for _, e := range ml.Data {
				if e.Type != "model" {
					t.Errorf("entry %s type = %q, want model", e.ID, e.Type)
				}
				if e.DisplayName == "" || e.CreatedAt == "" {
					t.Errorf("entry %s missing display_name/created_at: %+v", e.ID, e)
				}
				// The documented discovery filter (validated 2026-07-07).
				if !strings.HasPrefix(e.ID, "claude") && !strings.HasPrefix(e.ID, "anthropic") {
					t.Errorf("entry id %q would be dropped by the picker prefix filter", e.ID)
				}
			}
			if ml.HasMore {
				t.Error("has_more should be false for the full list")
			}
			if ml.FirstID == nil || *ml.FirstID != "anthropic-baseten-kimi" || ml.LastID == nil || *ml.LastID != "claude-baseten-glm-5-2" {
				t.Errorf("first_id/last_id wrong: %v %v", ml.FirstID, ml.LastID)
			}
		})
	}
}

// TestAliasModelsRespectsLimit: ?limit truncates the list and flips
// has_more.
func TestAliasModelsRespectsLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	g, adminL, _ := newGateway(t, cfg, aliasedClient(t, "baseten"))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	status, ml := getModelsList(t, g, "claude-code", "?limit=1")
	if status != 200 {
		t.Fatalf("GET /v1/models?limit=1 got %d", status)
	}
	if len(ml.Data) != 1 || ml.Data[0].ID != "anthropic-baseten-kimi" {
		t.Fatalf("limit=1 data wrong: %+v", ml.Data)
	}
	if !ml.HasMore {
		t.Error("has_more should be true when limit truncates")
	}
	if ml.FirstID == nil || *ml.FirstID != "anthropic-baseten-kimi" || ml.LastID == nil || *ml.LastID != "anthropic-baseten-kimi" {
		t.Errorf("first_id/last_id wrong: %v %v", ml.FirstID, ml.LastID)
	}

	status, next := getModelsList(
		t,
		g,
		"claude-code",
		"?limit=1&after_id=anthropic-baseten-kimi",
	)
	if status != 200 {
		t.Fatalf("second page got %d", status)
	}
	if len(next.Data) != 1 ||
		next.Data[0].ID != "claude-baseten-glm-5-2" ||
		next.HasMore {
		t.Fatalf("second page = %+v", next)
	}
}

// TestAliasModelsNativeRouteMergesProxiedList: on the native anthropic
// route (switch OFF) the gateway appends the proxied native list after
// the aliases (deduped, passthrough credential); a dead native upstream
// degrades to aliases-only instead of failing discovery.
func TestAliasModelsNativeRouteMergesProxiedList(t *testing.T) {
	t.Run("merge", func(t *testing.T) {
		gotKey := make(chan string, 1)
		antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotKey <- r.Header.Get("X-Api-Key")
			w.Header().Set("Content-Type", "application/json")
			// One real model plus a duplicate of a configured alias id.
			_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"claude-opus-4-8","display_name":"Claude Opus 4.8","created_at":"2025-08-01T00:00:00Z"},{"type":"model","id":"claude-baseten-glm-5-2","display_name":"dupe","created_at":"2025-08-01T00:00:00Z"}],"has_more":false,"first_id":"claude-opus-4-8","last_id":"claude-baseten-glm-5-2"}`))
		}))
		defer antSrv.Close()
		cfg := testConfig(t, antSrv.URL, antSrv.URL)
		g, adminL, _ := newGateway(t, cfg, aliasedClient(t, "anthropic"))
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		status, ml := getModelsList(t, g, "claude-code", "?limit=1000")
		if status != 200 {
			t.Fatalf("GET /v1/models got %d", status)
		}
		ids := make([]string, 0, len(ml.Data))
		for _, e := range ml.Data {
			ids = append(ids, e.ID)
		}
		want := "anthropic-baseten-kimi,claude-baseten-glm-5-2,claude-opus-4-8"
		if strings.Join(ids, ",") != want {
			t.Fatalf("merged ids = %v, want %s (aliases first, native deduped)", ids, want)
		}
		select {
		case k := <-gotKey:
			if k != "sk-harness" {
				t.Fatalf("native fetch credential = %q, want passthrough sk-harness", k)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("native upstream never contacted")
		}
	})
	t.Run("native upstream down degrades to aliases", func(t *testing.T) {
		antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		}))
		cfg := testConfig(t, antSrv.URL, antSrv.URL)
		antSrv.Close() // connection refused
		g, adminL, _ := newGateway(t, cfg, aliasedClient(t, "anthropic"))
		defer adminL.Close()
		stop := start(t, g)
		defer stop()

		status, ml := getModelsList(t, g, "claude-code", "")
		if status != 200 {
			t.Fatalf("GET /v1/models got %d, want 200 despite dead native upstream", status)
		}
		if len(ml.Data) != 2 {
			t.Fatalf("data has %d entries, want the 2 aliases: %+v", len(ml.Data), ml.Data)
		}
	})
}

func postMessages(t *testing.T, g *Gateway, model string) (*http.Response, string) {
	t.Helper()
	body := []byte(`{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp, string(rb)
}

// TestAliasRoutingExplicitChoiceWins: a request naming a configured
// alias is served via Baseten with the mapped slug regardless of the
// switch position, and telemetry attributes it (requested_model =
// alias, upstream_model = slug, route_effective = baseten when the
// configured route differs).
func TestAliasRoutingExplicitChoiceWins(t *testing.T) {
	for _, tc := range []struct {
		route         string
		wantEffective string
	}{
		{"baseten", "baseten"},
		{"anthropic", "baseten"},
	} {
		t.Run("route="+tc.route, func(t *testing.T) {
			gotModel := make(chan string, 1)
			basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages" {
					t.Errorf("unexpected baseten path %q", r.URL.Path)
				}
				b, _ := io.ReadAll(r.Body)
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				gotModel <- fmtString(m["model"])
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"VIA-BASETEN"}],"model":"zai-org/GLM-5.2","usage":{"input_tokens":5,"output_tokens":1}}`))
			}))
			defer basSrv.Close()
			antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("anthropic upstream must never see an alias request, got %s %s", r.Method, r.URL.Path)
			}))
			defer antSrv.Close()
			cfg := testConfig(t, basSrv.URL, antSrv.URL)
			g, adminL, _ := newGateway(t, cfg, aliasedClient(t, tc.route))
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			resp, rb := postMessages(t, g, "claude-baseten-glm-5-2")
			if resp.StatusCode != 200 || !strings.Contains(rb, "VIA-BASETEN") {
				t.Fatalf("alias request got %d: %s", resp.StatusCode, rb)
			}
			select {
			case m := <-gotModel:
				if m != "zai-org/GLM-5.2" {
					t.Fatalf("baseten upstream got model %q, want zai-org/GLM-5.2", m)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("baseten upstream never received the request")
			}
			rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
			row := rows[0]
			if row.RequestedModel != "claude-baseten-glm-5-2" || row.ServedModel != "zai-org/GLM-5.2" {
				t.Fatalf("telemetry attribution wrong: %+v", row)
			}
			if row.ConfiguredRoute != tc.route || row.EffectiveProvider != tc.wantEffective {
				t.Fatalf("telemetry route/effective = %q/%q, want %q/%q", row.ConfiguredRoute, row.EffectiveProvider, tc.route, tc.wantEffective)
			}
		})
	}
}

// TestRawSlugRoutingSwitchOff: a raw Baseten slug (contains "/") on an
// anthropic-shape client is honored verbatim to the baseten route even
// with the switch off and without any model_aliases configured.
func TestRawSlugRoutingSwitchOff(t *testing.T) {
	gotModel := make(chan string, 1)
	basSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		gotModel <- fmtString(m["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"RAW-SLUG"}],"model":"zai-org/GLM-5.2","usage":{"input_tokens":5,"output_tokens":1}}`))
	}))
	defer basSrv.Close()
	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("anthropic upstream must never see a raw-slug request, got %s %s", r.Method, r.URL.Path)
	}))
	defer antSrv.Close()
	cfg := testConfig(t, basSrv.URL, antSrv.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.Route = "anthropic" // switch OFF
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, rb := postMessages(t, g, "zai-org/GLM-5.2")
	if resp.StatusCode != 200 || !strings.Contains(rb, "RAW-SLUG") {
		t.Fatalf("raw-slug request got %d: %s", resp.StatusCode, rb)
	}
	select {
	case m := <-gotModel:
		if m != "zai-org/GLM-5.2" {
			t.Fatalf("baseten upstream got model %q, want verbatim zai-org/GLM-5.2", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("baseten upstream never received the request")
	}
	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	row := rows[0]
	if row.RequestedModel != "zai-org/GLM-5.2" || row.ServedModel != "zai-org/GLM-5.2" {
		t.Fatalf("telemetry attribution wrong: %+v", row)
	}
	if row.ConfiguredRoute != "anthropic" || row.EffectiveProvider != "baseten" {
		t.Fatalf("telemetry route/effective = %q/%q, want anthropic/baseten", row.ConfiguredRoute, row.EffectiveProvider)
	}
}

// TestAliasRequestNoSilentFallback: an alias request is a single
// baseten attempt. It never falls back to the configured
// fallback_route (an alias sent to Anthropic would 404, and silent
// substitution is what aliases remove), and it bypasses an active
// fallback cooldown, while native model requests keep the existing
// fallback behavior.
func TestAliasRequestNoSilentFallback(t *testing.T) {
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
	rc := aliasedClient(t, "baseten")
	rc.FallbackRoute = "anthropic"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// Alias request: 503 relayed, no fallback.
	if resp, _ := postMessages(t, g, "claude-baseten-glm-5-2"); resp.StatusCode != 503 {
		t.Fatalf("alias request got %d, want the relayed 503", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&fbHits); n != 0 {
		t.Fatalf("alias request must not fall back; fallback hits = %d", n)
	}
	// Native model request keeps the fallback path (and trips cooldown).
	if resp, rb := postMessages(t, g, "claude-opus-4-8"); resp.StatusCode != 200 || !strings.Contains(rb, "FALLBACK") {
		t.Fatalf("native request got %d: %s", resp.StatusCode, rb)
	}
	if n := atomic.LoadInt32(&fbHits); n != 1 {
		t.Fatalf("native request should have fallen back once, got %d", n)
	}
	// Alias request during cooldown still goes to baseten, not fallback.
	if resp, _ := postMessages(t, g, "claude-baseten-glm-5-2"); resp.StatusCode != 503 {
		t.Fatalf("alias request during cooldown got %d, want 503 from baseten", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&fbHits); n != 1 {
		t.Fatalf("alias request during cooldown must not fall back; fallback hits = %d", n)
	}
	if n := atomic.LoadInt32(&primaryHits); n != 3 {
		t.Fatalf("primary hits = %d, want 3 (two alias + one native)", n)
	}
}

// TestUnknownAliasLoud400: an unrecognized claude-baseten-*/anthropic-baseten-*
// id is a 400 naming the id, the configured aliases, and the fix,
// never a silent default-model route and never a pass-through to Anthropic.
func TestUnknownAliasLoud400(t *testing.T) {
	for _, tc := range []struct {
		name    string
		route   string
		aliases bool
	}{
		{"switch on with aliases", "baseten", true},
		{"switch off with aliases", "anthropic", true},
		{"all aliases removed", "baseten", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no upstream may see an unknown-alias request, got %s %s", r.Method, r.URL.Path)
			}))
			defer srv.Close()
			cfg := testConfig(t, srv.URL, srv.URL)
			var rc resolvedClientConfig
			if tc.aliases {
				rc = aliasedClient(t, tc.route)
			} else {
				rc = resolvedAnthropicBaseten(t)
				rc.Route = tc.route
			}
			g, adminL, _ := newGateway(t, cfg, rc)
			defer adminL.Close()
			stop := start(t, g)
			defer stop()

			resp, rb := postMessages(t, g, "claude-baseten-removed")
			if resp.StatusCode != 400 {
				t.Fatalf("got %d, want 400: %s", resp.StatusCode, rb)
			}
			for _, want := range []string{"claude-baseten-removed", "model_aliases", "gateway.yaml", "HUP"} {
				if !strings.Contains(rb, want) {
					t.Errorf("400 body missing %q: %s", want, rb)
				}
			}
			if tc.aliases {
				for _, want := range []string{"anthropic-baseten-kimi", "claude-baseten-glm-5-2"} {
					if !strings.Contains(rb, want) {
						t.Errorf("400 body should list configured alias %q: %s", want, rb)
					}
				}
			}
		})
	}
}

// TestModelAliasConfigValidation: resolveFromFile rejects alias ids
// that would not survive the picker filter, shadow real Anthropic
// model names, and aliases on
// non-anthropic-shape clients.
func TestModelAliasConfigValidation(t *testing.T) {
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
		{"valid", func(c *config.Client) {}, ""},
		{"anthropic-baseten namespace valid", func(c *config.Client) {
			c.ModelAliases = map[string]string{"anthropic-baseten-kimi": "moonshotai/Kimi-K2.7-Code"}
		}, ""},
		{"openai shape rejected", func(c *config.Client) {
			c.ProtocolShape = "openai"
		}, "protocol_shape anthropic"},
		{"picker filter", func(c *config.Client) {
			c.ModelAliases = map[string]string{"glm-baseten-5-2": "zai-org/GLM-5.2"}
		}, "discovery filter"},
		{"real anthropic name", func(c *config.Client) {
			c.ModelAliases = map[string]string{"claude-sonnet-4-6": "zai-org/GLM-5.2"}
		}, "real Anthropic model names"},
		{"dated anthropic name", func(c *config.Client) {
			c.ModelAliases = map[string]string{"claude-3-5-sonnet-20241022": "zai-org/GLM-5.2"}
		}, "real Anthropic model names"},
		{"claude 5 family name", func(c *config.Client) {
			// Claude Code's current default model family; an alias here
			// would shadow every native claude-fable request.
			c.ModelAliases = map[string]string{"claude-fable-5": "zai-org/GLM-5.2"}
		}, "real Anthropic model names"},
		{"empty slug", func(c *config.Client) {
			c.ModelAliases = map[string]string{"claude-baseten-x": ""}
		}, "empty Baseten slug"},
		{"disabled client skipped", func(c *config.Client) {
			c.Enabled = false
			c.ModelAliases = map[string]string{"bad-id": "x"}
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

// TestResolvedClientHashCoversModelAliases: an alias change must
// respawn the listener on SIGHUP.
func TestResolvedClientHashCoversModelAliases(t *testing.T) {
	a := resolvedClientConfig{Name: "claude-code", BindAddr: "127.0.0.1:18081", ProtocolShape: "anthropic", Route: "baseten"}
	b := a
	b.ModelAliases = map[string]string{"claude-baseten-glm-5-2": "zai-org/GLM-5.2"}
	if a.hash() == b.hash() {
		t.Fatal("hash must change when model_aliases change")
	}
	c := b
	c.ModelAliases = map[string]string{"claude-baseten-glm-5-2": "other/slug"}
	if b.hash() == c.hash() {
		t.Fatal("hash must change when an alias target changes")
	}
}
