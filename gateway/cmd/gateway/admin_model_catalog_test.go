package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

type modelCatalogRoundTripFunc func(*http.Request) (*http.Response, error)

func (f modelCatalogRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func modelCatalogClient(handler http.Handler, requests *atomic.Int32) *http.Client {
	return &http.Client{Transport: modelCatalogRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if requests != nil {
			requests.Add(1)
		}
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer oauth-test-token")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Result(), nil
	})}
}

func testModelCatalogGateway(t *testing.T, handler http.Handler, signedIn bool) (*Gateway, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var oauthRequests atomic.Int32
	var fallbackRequests atomic.Int32
	fallback := &http.Client{Transport: modelCatalogRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		fallbackRequests.Add(1)
		return nil, errors.New("API-key fallback must not be used")
	})}
	g := &Gateway{
		cfg: Config{
			OAuthHost:      "https://catalog.test",
			BasetenKey:     "api-key-that-must-not-be-used",
			APIKeyFallback: true,
		},
		client:  fallback,
		pricing: pricing.New(),
	}
	if signedIn {
		g.oauthClient = modelCatalogClient(handler, &oauthRequests)
	}
	mux := http.NewServeMux()
	g.registerAdmin(mux)
	g.adminServer = &http.Server{Handler: mux}
	return g, &oauthRequests, &fallbackRequests
}

func modelCatalogRequest(t *testing.T, g *Gateway, method string, ctx context.Context) (*httptest.ResponseRecorder, modelCatalogResponse) {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/admin/model-catalog", nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	recorder := httptest.NewRecorder()
	g.adminServer.Handler.ServeHTTP(recorder, req)
	var response modelCatalogResponse
	if method != http.MethodHead && recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v\nbody: %s", err, recorder.Body.String())
		}
	}
	return recorder, response
}

func TestAdminModelCatalogReadyNormalizesAndSanitizesModels(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Scheme != "https" || r.URL.Host != "catalog.test" {
			t.Errorf("upstream URL = %s, want https://catalog.test", r.URL)
		}
		if r.URL.Path != "/v1/model_apis" {
			t.Errorf("path = %q, want /v1/model_apis", r.URL.Path)
		}
		if got := r.URL.Query().Get("added_only"); got != "false" {
			t.Errorf("added_only = %q, want false", got)
		}
		if got := r.URL.Query().Get("limit"); got != "1000" {
			t.Errorf("limit = %q, want 1000", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-test-token" {
			t.Errorf("authorization = %q, want OAuth transport header", got)
		}
		_, _ = io.WriteString(w, `{
			"items": [
				{
					"name": "moonshotai/Kimi-K2",
					"display_name": "Kimi K2",
					"cost_per_million_input_tokens": 0.15,
					"cost_per_million_output_tokens": "2.5000",
					"org_details": {"secret": "must not escape"},
					"invoke_url": "https://secret.invalid"
				},
				{
					"name": "zai-org/GLM-5",
					"display_name": "",
					"cost_per_million_input_tokens": null,
					"org_details": null
				},
				{
					"name": "openai/gpt-oss-120b",
					"display_name": "GPT OSS 120B",
					"cost_per_million_input_tokens": 0,
					"cost_per_million_output_tokens": "0"
				},
				{
					"name": "vendor/ignored-account-state",
					"display_name": "Ignored Account State",
					"cost_per_million_input_tokens": "1.2300e+2",
					"cost_per_million_output_tokens": 4.2e-3,
					"org_details": "unexpected but irrelevant"
				},
				{"display_name": "missing name", "org_details": {}},
				{"name": 123, "display_name": "invalid name", "org_details": {}},
				{"name": "   ", "display_name": "blank name", "org_details": {}}
			],
			"pagination": {"has_more": false, "cursor": null},
			"internal": "must not escape"
		}`)
	})
	g, oauthRequests, fallbackRequests := testModelCatalogGateway(t, upstream, true)

	recorder, response := modelCatalogRequest(t, g, http.MethodGet, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if response.State != "ready" || response.Error != "" || response.FetchedAt == "" {
		t.Fatalf("response = %+v, want ready with fetched_at and no error", response)
	}
	if response.SignedOutReason != "" {
		t.Fatalf("signed_out_reason = %q, want empty", response.SignedOutReason)
	}
	want := []modelCatalogModel{
		{Slug: "moonshotai/Kimi-K2", DisplayName: "Kimi K2"},
		{Slug: "zai-org/GLM-5", DisplayName: "GLM 5"},
		{Slug: "openai/gpt-oss-120b", DisplayName: "GPT OSS 120B"},
		{Slug: "vendor/ignored-account-state", DisplayName: "Ignored Account State"},
	}
	if len(response.Models) != len(want) {
		t.Fatalf("models = %+v, want %+v", response.Models, want)
	}
	for i := range want {
		assertModelCatalogModel(t, i, response.Models[i], want[i])
	}
	if containsAny(
		recorder.Body.String(),
		"secret",
		"invoke_url",
		"internal",
		"must not escape",
		"org_details",
		"added_to_workspace",
		"cost_per_million_input_tokens",
		"cost_per_million_output_tokens",
	) {
		t.Fatalf("response leaked unsanitized upstream fields: %s", recorder.Body.String())
	}
	if got := oauthRequests.Load(); got != 1 {
		t.Errorf("OAuth requests = %d, want 1", got)
	}
	if got := fallbackRequests.Load(); got != 0 {
		t.Errorf("API-key fallback requests = %d, want 0", got)
	}
}

func TestAdminModelCatalogPublishesNamesWithoutReplacingPricing(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"items": [{
				"name": "zai-org/GLM-5.2",
				"display_name": "Account Inkling",
				"cost_per_million_input_tokens": 999999,
				"cost_per_million_output_tokens": 999999
			}],
			"pagination": {"has_more": false, "cursor": null}
		}`)
	})
	g, _, _ := testModelCatalogGateway(t, upstream, true)
	before := g.pricing.Capture().Quote(
		pricing.ProviderBaseten,
		"zai-org/GLM-5.2",
	)

	recorder, response := modelCatalogRequest(t, g, http.MethodGet, nil)
	if recorder.Code != http.StatusOK || response.State != "ready" ||
		len(response.Models) != 1 ||
		response.Models[0].DisplayName != "Account Inkling" {
		t.Fatalf("response = %+v, status=%d", response, recorder.Code)
	}
	snapshot := g.pricing.Capture()
	if name, ok := snapshot.DisplayName(
		pricing.ProviderBaseten,
		"zai-org/GLM-5.2",
	); !ok || name != "Account Inkling" {
		t.Fatalf("published display name = %q, found=%t", name, ok)
	}
	if after := snapshot.Quote(
		pricing.ProviderBaseten,
		"zai-org/GLM-5.2",
	); after != before {
		t.Fatalf("Model APIs response changed pricing: before=%+v after=%+v", before, after)
	}

	rc := resolvedClientConfig{
		ProtocolShape: "anthropic",
		Route:         "baseten",
		DefaultModel:  "zai-org/GLM-5.2",
	}
	catalog := computeModelCatalog(rc, snapshot)
	if len(catalog) != 1 || catalog[0].Label != "Account Inkling" {
		t.Fatalf("status catalog = %+v, want unified display name", catalog)
	}
	if summary := effectiveSummary(
		rc,
		computeFamilies(rc),
		snapshot,
	); summary != "Baseten · Account Inkling" {
		t.Fatalf("effective summary = %q", summary)
	}
}

func TestAdminModelCatalogSignedOutPreservesLastGoodNames(t *testing.T) {
	var attempts atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(w, `{
				"items": [{
					"name": "vendor/model",
					"display_name": "Last Good Name"
				}],
				"pagination": {"has_more": false, "cursor": null}
			}`)
			return
		}
		http.Error(w, "expired", http.StatusUnauthorized)
	})
	g, _, _ := testModelCatalogGateway(t, upstream, true)

	_, ready := modelCatalogRequest(t, g, http.MethodGet, nil)
	if ready.State != "ready" {
		t.Fatalf("first response = %+v", ready)
	}
	snapshot := g.pricing.Capture()
	_, signedOut := modelCatalogRequest(t, g, http.MethodGet, nil)
	if signedOut.State != "signed_out" {
		t.Fatalf("second response = %+v", signedOut)
	}
	if name, ok := g.pricing.Capture().DisplayName(
		pricing.ProviderBaseten,
		"vendor/model",
	); !ok || name != "Last Good Name" {
		t.Fatalf("last-good display name = %q, found=%t", name, ok)
	}
	if snapshot.PresentationRevision(pricing.ProviderBaseten) !=
		g.pricing.Capture().PresentationRevision(pricing.ProviderBaseten) {
		t.Fatal("signed-out refresh changed presentation revision")
	}
}

func TestAdminModelCatalogEmptyReadyPreservesNamesWithoutStaleSelections(t *testing.T) {
	var attempts atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(w, `{
				"items": [{
					"name": "vendor/model",
					"display_name": "Last Good Name"
				}],
				"pagination": {"has_more": false, "cursor": null}
			}`)
			return
		}
		_, _ = io.WriteString(w,
			`{"items":[],"pagination":{"has_more":false,"cursor":null}}`)
	})
	g, _, _ := testModelCatalogGateway(t, upstream, true)

	_, ready := modelCatalogRequest(t, g, http.MethodGet, nil)
	if ready.State != "ready" || len(ready.Models) != 1 {
		t.Fatalf("first response = %+v", ready)
	}
	_, empty := modelCatalogRequest(t, g, http.MethodGet, nil)
	if empty.State != "ready" || len(empty.Models) != 0 {
		t.Fatalf("empty response = %+v, want ready with no selectable models", empty)
	}
	if name, ok := g.pricing.Capture().DisplayName(
		pricing.ProviderBaseten,
		"vendor/model",
	); !ok || name != "Last Good Name" {
		t.Fatalf("last-good display name = %q, found=%t", name, ok)
	}
}

func TestAdminModelCatalogIgnoresNonSelectionFieldsAndUsesMetadataFallback(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"items": [
				{
					"name":"zai-org/GLM-5.2",
					"display_name":null,
					"cost_per_million_input_tokens":"contact sales",
					"cost_per_million_output_tokens":{"unexpected":true},
					"description":"must not escape"
				}
			],
			"pagination":{"has_more":false,"cursor":null}
		}`)
	})
	g, _, _ := testModelCatalogGateway(t, upstream, true)

	recorder, response := modelCatalogRequest(t, g, http.MethodGet, nil)
	if response.State != "ready" || len(response.Models) != 1 {
		t.Fatalf("status = %d response = %+v, want one ready model", recorder.Code, response)
	}
	want := modelCatalogModel{
		Slug:        "zai-org/GLM-5.2",
		DisplayName: "GLM 5.2",
	}
	if response.Models[0].Slug != want.Slug ||
		response.Models[0].DisplayName != want.DisplayName {
		t.Fatalf("model = %#v, want %#v", response.Models[0], want)
	}
	reasoning := response.Models[0].Reasoning
	if reasoning == nil || !reasoning.Supported ||
		len(reasoning.Options) != 1 ||
		reasoning.Options[0].Type != pricing.ReasoningToggle ||
		reasoning.Source != "models_dev" ||
		reasoning.LoadedFrom !=
			string(pricing.LoadedFromVendoredFallback) ||
		reasoning.Stale {
		t.Fatalf("reasoning projection = %+v", reasoning)
	}
	if containsAny(
		recorder.Body.String(),
		"cost_per_million",
		"contact sales",
		"unexpected",
		"description",
		"must not escape",
	) {
		t.Fatalf("response leaked non-selection fields: %s", recorder.Body.String())
	}
}

func TestModelCatalogReasoningProjectionUsesExactBasetenRecord(t *testing.T) {
	capturedAt := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	originalNow := modelCatalogNow
	modelCatalogNow = func() time.Time { return capturedAt }
	t.Cleanup(func() { modelCatalogNow = originalNow })
	p := pricing.New()
	if err := p.ReplaceModelsDev(
		[]byte(publicCatalogGatewayFixture),
		capturedAt,
		`"catalog-v1"`,
	); err != nil {
		t.Fatal(err)
	}
	projected := modelCatalogModelsFromSnapshot(
		p.Capture(),
		[]modelCatalogModel{
			{Slug: "zai-org/GLM-Test"},
			{Slug: "missing/model"},
		},
	)
	if len(projected) != 2 {
		t.Fatalf("projected models = %+v", projected)
	}
	reasoning := projected[0].Reasoning
	if reasoning == nil || !reasoning.Supported ||
		len(reasoning.Options) != 1 ||
		reasoning.Options[0].Type != pricing.ReasoningToggle ||
		reasoning.Source != "models_dev" ||
		reasoning.LoadedFrom != string(pricing.LoadedFromLive) ||
		reasoning.CapturedAt != capturedAt.Format(time.RFC3339) ||
		reasoning.Revision == "" ||
		reasoning.Stale {
		t.Fatalf("live reasoning projection = %+v", reasoning)
	}
	if projected[1].Reasoning != nil {
		t.Fatalf("missing model reasoning = %+v",
			projected[1].Reasoning)
	}

	freshAvailabilityAt := capturedAt.Add(72 * time.Hour)
	if err := p.ReplaceProviderAvailability(
		pricing.ProviderBaseten,
		[]pricing.AvailabilityModel{{
			CanonicalModelID: "zai-org/GLM-Test",
			DisplayName:      "Account GLM Test",
		}},
		"baseten_model_apis",
		freshAvailabilityAt,
		"sha256:fresh-availability",
	); err != nil {
		t.Fatal(err)
	}
	mixed := modelCatalogReasoningFromSnapshot(
		p.Capture(),
		"zai-org/GLM-Test",
		freshAvailabilityAt,
	)
	if mixed == nil || !mixed.Stale {
		t.Fatalf(
			"fresh Model APIs availability refreshed stale reasoning: %+v",
			mixed,
		)
	}

	cache, err := p.ExportProviderCache(pricing.ProviderBaseten)
	if err != nil {
		t.Fatal(err)
	}
	restored := pricing.New()
	if err := restored.ImportProviderCache(cache); err != nil {
		t.Fatal(err)
	}
	stale := modelCatalogReasoningFromSnapshot(
		restored.Capture(),
		"zai-org/GLM-Test",
		freshAvailabilityAt,
	)
	if stale == nil || !stale.Stale ||
		stale.LoadedFrom != string(pricing.LoadedFromRuntimeCache) {
		t.Fatalf("stale cache projection = %+v", stale)
	}
}

func TestAdminModelCatalogSignedOutDoesNotUseAPIKeyFallback(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("signed-out catalog must not call upstream")
	})
	g, oauthRequests, fallbackRequests := testModelCatalogGateway(t, upstream, false)

	recorder, response := modelCatalogRequest(t, g, http.MethodGet, nil)
	if recorder.Code != http.StatusOK || response.State != "signed_out" {
		t.Fatalf("status = %d response = %+v, want 200 signed_out", recorder.Code, response)
	}
	if response.SignedOutReason != modelCatalogSignedOutReasonNotSignedIn {
		t.Fatalf("signed_out_reason = %q, want %q",
			response.SignedOutReason, modelCatalogSignedOutReasonNotSignedIn)
	}
	assertEmptyModelCatalogState(t, response)
	if got := oauthRequests.Load(); got != 0 {
		t.Errorf("OAuth requests = %d, want 0", got)
	}
	if got := fallbackRequests.Load(); got != 0 {
		t.Errorf("API-key fallback requests = %d, want 0", got)
	}
}

func TestAdminModelCatalogUsesSelectedAPIKeyProfile(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Api-Key selected-profile-key" {
			t.Fatalf("Authorization = %q, want selected API-key profile", got)
		}
		_, _ = io.WriteString(w, `{"items":[{"name":"example/model","display_name":"Example"}],"pagination":{"has_more":false,"cursor":null}}`)
	}))
	defer upstream.Close()

	g := &Gateway{
		cfg: Config{
			OAuthHost:      upstream.URL,
			BasetenKey:     "environment-fallback-must-not-win",
			APIKeyFallback: true,
		},
		client:           upstream.Client(),
		pricing:          pricing.New(),
		cliProfileAPIKey: "selected-profile-key",
	}
	mux := http.NewServeMux()
	g.registerAdmin(mux)
	g.adminServer = &http.Server{Handler: mux}

	recorder, response := modelCatalogRequest(t, g, http.MethodGet, nil)
	if recorder.Code != http.StatusOK || response.State != "ready" || len(response.Models) != 1 {
		t.Fatalf("status = %d response = %+v, want ready catalog", recorder.Code, response)
	}
}

func TestAdminModelCatalogAPIKeyAuthorizationOutcomes(t *testing.T) {
	for _, test := range []struct {
		status int
		state  string
		reason string
		error  string
	}{
		{http.StatusUnauthorized, "signed_out", modelCatalogSignedOutReasonRejected, ""},
		{http.StatusForbidden, "error", "", "The selected Baseten credential does not have access to the model catalog"},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "synthetic upstream secret", test.status)
			}))
			defer upstream.Close()
			g := &Gateway{
				cfg:              Config{OAuthHost: upstream.URL},
				client:           upstream.Client(),
				pricing:          pricing.New(),
				cliProfileAPIKey: "selected-profile-key",
			}
			mux := http.NewServeMux()
			g.registerAdmin(mux)
			g.adminServer = &http.Server{Handler: mux}

			recorder, response := modelCatalogRequest(t, g, http.MethodGet, nil)
			if response.State != test.state || response.SignedOutReason != test.reason || response.Error != test.error {
				t.Fatalf("response = %+v", response)
			}
			if strings.Contains(recorder.Body.String(), "synthetic upstream secret") || strings.Contains(recorder.Body.String(), "selected-profile-key") {
				t.Fatal("catalog response leaked credential or upstream body")
			}
		})
	}
}

func TestAdminModelCatalogUnauthorizedMeansExpiredSession(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token=upstream-secret", http.StatusUnauthorized)
	})
	g, oauthRequests, fallbackRequests := testModelCatalogGateway(t, upstream, true)

	recorder, response := modelCatalogRequest(t, g, http.MethodGet, nil)
	if recorder.Code != http.StatusOK || response.State != "signed_out" {
		t.Fatalf("status = %d response = %+v, want 200 signed_out", recorder.Code, response)
	}
	if response.SignedOutReason != modelCatalogSignedOutReasonSessionExpired {
		t.Fatalf("signed_out_reason = %q, want %q",
			response.SignedOutReason, modelCatalogSignedOutReasonSessionExpired)
	}
	assertEmptyModelCatalogState(t, response)
	if containsAny(recorder.Body.String(), "upstream-secret", "token=", "401") {
		t.Fatalf("response leaked upstream details: %s", recorder.Body.String())
	}
	if got := oauthRequests.Load(); got != 1 {
		t.Errorf("OAuth requests = %d, want 1", got)
	}
	if got := fallbackRequests.Load(); got != 0 {
		t.Errorf("API-key fallback requests = %d, want 0", got)
	}
}

func TestAdminModelCatalogFollowsOpaquePaginationCursor(t *testing.T) {
	opaqueCursor := "page 2&added_only=true?limit=1"
	var mu sync.Mutex
	var queries []url.Values
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = io.WriteString(w, `{
				"items":[{"name":"first","display_name":"First","org_details":null}],
				"pagination":{"has_more":true,"cursor":`+strconv.Quote(opaqueCursor)+`}
			}`)
		case opaqueCursor:
			_, _ = io.WriteString(w, `{
				"items":[{"name":"second","display_name":"Second","org_details":{}}],
				"pagination":{"has_more":false,"cursor":null}
			}`)
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	})
	g, _, _ := testModelCatalogGateway(t, upstream, true)

	_, response := modelCatalogRequest(t, g, http.MethodGet, nil)
	if response.State != "ready" || len(response.Models) != 2 {
		t.Fatalf("response = %+v, want two paginated models", response)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 {
		t.Fatalf("queries = %v, want two requests", queries)
	}
	for _, query := range queries {
		if query.Get("added_only") != "false" || query.Get("limit") != "1000" {
			t.Errorf("pagination query lost fixed parameters: %v", query)
		}
	}
	if got := queries[1].Get("cursor"); got != opaqueCursor {
		t.Errorf("cursor = %q, want opaque value %q", got, opaqueCursor)
	}
}

func TestAdminModelCatalogRejectsInvalidPagination(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
		wantN   int32
	}{
		{
			name: "empty cursor",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"items":[],"pagination":{"has_more":true,"cursor":""}}`)
			}),
			wantN: 1,
		},
		{
			name: "repeated cursor",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"items":[],"pagination":{"has_more":true,"cursor":"same"}}`)
			}),
			wantN: 2,
		},
		{
			name: "page bound",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
				_, _ = io.WriteString(w, `{"items":[],"pagination":{"has_more":true,"cursor":"`+strconv.Itoa(page+1)+`"}}`)
			}),
			wantN: modelCatalogMaxPages,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, oauthRequests, _ := testModelCatalogGateway(t, tt.handler, true)
			_, response := modelCatalogRequest(t, g, http.MethodGet, nil)
			if response.State != "error" {
				t.Fatalf("response = %+v, want error", response)
			}
			assertEmptyModelCatalogError(t, response)
			if got := oauthRequests.Load(); got != tt.wantN {
				t.Errorf("upstream requests = %d, want %d", got, tt.wantN)
			}
		})
	}
}

func TestAdminModelCatalogRejectsSchemaDrift(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"items":`},
		{name: "multiple JSON values", body: `{"items":[],"pagination":{"has_more":false}} {}`},
		{name: "top-level array", body: `[]`},
		{name: "missing items", body: `{"pagination":{"has_more":false}}`},
		{name: "null items", body: `{"items":null,"pagination":{"has_more":false}}`},
		{name: "wrong items type", body: `{"items":{},"pagination":{"has_more":false}}`},
		{name: "wrong model type", body: `{"items":[[]],"pagination":{"has_more":false}}`},
		{name: "missing pagination", body: `{"items":[]}`},
		{name: "null pagination", body: `{"items":[],"pagination":null}`},
		{name: "wrong pagination type", body: `{"items":[],"pagination":[]}`},
		{name: "missing has more", body: `{"items":[],"pagination":{}}`},
		{name: "wrong has more type", body: `{"items":[],"pagination":{"has_more":"false"}}`},
		{name: "wrong cursor type", body: `{"items":[],"pagination":{"has_more":true,"cursor":123}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tt.body)
			})
			g, _, _ := testModelCatalogGateway(t, upstream, true)
			_, response := modelCatalogRequest(t, g, http.MethodGet, nil)
			if response.State != "error" {
				t.Fatalf("response = %+v, want error", response)
			}
			assertEmptyModelCatalogError(t, response)
		})
	}
}

func TestAdminModelCatalogSanitizesUpstreamFailures(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
	}{
		{
			name: "non-success response",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "token=upstream-secret", http.StatusForbidden)
			}),
		},
		{
			name: "oversized response",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, strings.Repeat("x", modelCatalogMaxBody+1))
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, _, _ := testModelCatalogGateway(t, tt.handler, true)
			recorder, response := modelCatalogRequest(t, g, http.MethodGet, nil)
			if response.State != "error" {
				t.Fatalf("response = %+v, want error", response)
			}
			if tt.name == "non-success response" {
				if response.Error != "The selected Baseten credential does not have access to the model catalog" {
					t.Fatalf("error = %q, want stable access-denied guidance", response.Error)
				}
			} else {
				assertEmptyModelCatalogError(t, response)
			}
			if containsAny(recorder.Body.String(), "upstream-secret", "token=", "401") {
				t.Fatalf("response leaked upstream details: %s", recorder.Body.String())
			}
		})
	}
}

func TestAdminModelCatalogSanitizesTransportError(t *testing.T) {
	var fallbackRequests atomic.Int32
	g := &Gateway{
		cfg: Config{
			OAuthHost:      "https://catalog.test",
			BasetenKey:     "api-key-that-must-not-be-used",
			APIKeyFallback: true,
		},
		client: &http.Client{Transport: modelCatalogRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			fallbackRequests.Add(1)
			return nil, errors.New("fallback used")
		})},
		oauthClient: &http.Client{Transport: modelCatalogRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("refresh token oauth-secret")
		})},
	}
	mux := http.NewServeMux()
	g.registerAdmin(mux)
	g.adminServer = &http.Server{Handler: mux}

	recorder, response := modelCatalogRequest(t, g, http.MethodGet, nil)
	if response.State != "error" {
		t.Fatalf("response = %+v, want error", response)
	}
	assertEmptyModelCatalogError(t, response)
	if containsAny(recorder.Body.String(), "oauth-secret", "refresh token") {
		t.Fatalf("response leaked transport error: %s", recorder.Body.String())
	}
	if got := fallbackRequests.Load(); got != 0 {
		t.Errorf("API-key fallback requests = %d, want 0", got)
	}
}

func TestAdminModelCatalogTimeoutIsStable(t *testing.T) {
	g, _, _ := testModelCatalogGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("expired request must not reach upstream handler")
	}), true)
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	recorder, response := modelCatalogRequest(t, g, http.MethodGet, expired)
	if recorder.Code != http.StatusOK || response.State != "error" {
		t.Fatalf("status = %d response = %+v, want 200 error", recorder.Code, response)
	}
	if response.Error != "Unable to load the Baseten model catalog" {
		t.Fatalf("error = %q, want stable request failure", response.Error)
	}

	deadline, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	recorder, response = modelCatalogRequest(t, g, http.MethodGet, deadline)
	if recorder.Code != http.StatusOK || response.State != "error" {
		t.Fatalf("status = %d response = %+v, want 200 error", recorder.Code, response)
	}
	if response.Error != "Baseten model catalog request timed out" {
		t.Fatalf("error = %q, want stable timeout", response.Error)
	}
}

func TestAdminModelCatalogMethodAndHeadHandling(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"items":[],"pagination":{"has_more":false,"cursor":null}}`)
	})
	g, oauthRequests, _ := testModelCatalogGateway(t, upstream, true)

	getRecorder, response := modelCatalogRequest(t, g, http.MethodGet, nil)
	if getRecorder.Code != http.StatusOK || response.State != "ready" {
		t.Fatalf("GET status = %d response = %+v, want ready", getRecorder.Code, response)
	}
	headRecorder, _ := modelCatalogRequest(t, g, http.MethodHead, nil)
	if headRecorder.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", headRecorder.Code)
	}
	if headRecorder.Body.Len() != 0 {
		t.Fatalf("HEAD body length = %d, want 0", headRecorder.Body.Len())
	}
	if got, want := headRecorder.Header().Get("Content-Type"), getRecorder.Header().Get("Content-Type"); got != want {
		t.Errorf("HEAD content type = %q, want %q", got, want)
	}
	if got, want := headRecorder.Header().Get("Content-Length"), getRecorder.Header().Get("Content-Length"); got != want {
		t.Errorf("HEAD content length = %q, want %q", got, want)
	}
	if got := getRecorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("GET cache control = %q, want no-store", got)
	}
	if got := headRecorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("HEAD cache control = %q, want no-store", got)
	}

	postRecorder, _ := modelCatalogRequest(t, g, http.MethodPost, nil)
	if postRecorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", postRecorder.Code)
	}
	if got := oauthRequests.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want GET and HEAD only", got)
	}
}

func assertEmptyModelCatalogState(t *testing.T, response modelCatalogResponse) {
	t.Helper()
	if response.Models == nil || len(response.Models) != 0 {
		t.Errorf("models = %#v, want non-nil empty array", response.Models)
	}
	if response.FetchedAt != "" || response.Error != "" {
		t.Errorf("response = %+v, want empty fetched_at and error", response)
	}
}

func assertEmptyModelCatalogError(t *testing.T, response modelCatalogResponse) {
	t.Helper()
	if response.Models == nil || len(response.Models) != 0 {
		t.Errorf("models = %#v, want non-nil empty array", response.Models)
	}
	if response.FetchedAt != "" {
		t.Errorf("fetched_at = %q, want empty", response.FetchedAt)
	}
	if response.SignedOutReason != "" {
		t.Errorf("signed_out_reason = %q, want empty", response.SignedOutReason)
	}
	if response.Error != "Unable to load the Baseten model catalog" {
		t.Errorf("error = %q, want stable public error", response.Error)
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func assertModelCatalogModel(t *testing.T, index int, got, want modelCatalogModel) {
	t.Helper()
	if got.Slug != want.Slug || got.DisplayName != want.DisplayName {
		t.Errorf("models[%d] identity = (%q, %q), want (%q, %q)",
			index, got.Slug, got.DisplayName, want.Slug, want.DisplayName)
	}
}
