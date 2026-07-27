package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

func TestCatalogBackedClaudeModelsDiscoveryProjectsOnlyPricedModels(t *testing.T) {
	var nativeHits atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeHits.Add(1)
		_, _ = w.Write([]byte(`{
			"data":[
				{"type":"model","id":"claude-opus-5","display_name":"duplicate","created_at":"2026-01-01T00:00:00Z"},
				{"type":"model","id":"claude-native-only","display_name":"Native only","created_at":"2026-01-01T00:00:00Z"}
			],
			"has_more": false
		}`))
	}))
	defer native.Close()

	for _, test := range []struct {
		route      string
		wantNative bool
	}{
		{route: "baseten", wantNative: false},
		{route: "anthropic", wantNative: true},
	} {
		t.Run(test.route, func(t *testing.T) {
			nativeHits.Store(0)
			modelPricing := pricing.New()
			if err := modelPricing.ReplaceModelsDev(
				[]byte(publicCatalogGatewayFixture),
				time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
				`"catalog-v1"`,
			); err != nil {
				t.Fatal(err)
			}
			g := &Gateway{
				cfg: Config{
					AnthropicURL: native.URL,
					BasetenURL:   native.URL,
				},
				pricing: modelPricing,
				client:  native.Client(),
			}
			client := &clientListener{cfg: aliasedClient(t, test.route)}
			request := httptest.NewRequest(
				http.MethodGet,
				"http://gateway.local/v1/models?limit=1000",
				nil,
			)
			request.Header.Set("X-Api-Key", "sk-harness")
			request.Header.Set("Anthropic-Version", "2023-06-01")
			recorder := httptest.NewRecorder()

			g.forwardModelsGet(client, recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var list modelsList
			if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
				t.Fatalf("decode models response: %v", err)
			}
			ids := make([]string, 0, len(list.Data))
			displayNames := map[string]string{}
			for _, entry := range list.Data {
				ids = append(ids, entry.ID)
				displayNames[entry.ID] = entry.DisplayName
			}
			joined := strings.Join(ids, ",")
			for _, required := range []string{
				"anthropic-baseten-kimi",
				"claude-baseten-glm-5-2",
				"claude-newfamily-1",
				"claude-opus-5",
			} {
				if !strings.Contains(","+joined+",", ","+required+",") {
					t.Fatalf("models %v omitted %q", ids, required)
				}
			}
			if strings.Contains(joined, "claude-haiku-unpriced") {
				t.Fatalf("unpriced catalog model was published: %v", ids)
			}
			if displayNames["claude-opus-5"] != "Claude Opus 5" {
				t.Fatalf("catalog display name = %q", displayNames["claude-opus-5"])
			}
			family, ok := modelPricing.Capture().ModelFamily(
				pricing.ProviderAnthropic,
				"claude-newfamily-1",
			)
			if !ok || family != "newfamily" {
				t.Fatalf("invented model family = %q, found=%t", family, ok)
			}
			if quote := modelPricing.Quote(
				pricing.ProviderAnthropic,
				"claude-newfamily-1",
			); !quote.Priced {
				t.Fatalf("invented model quote = %+v", quote)
			}
			decision := resolveNativeModelPolicyWithFamily(
				client.cfg,
				"claude-newfamily-1",
				family,
			)
			if decision.route != test.route {
				t.Fatalf(
					"invented family route = %q, want client default %q",
					decision.route,
					test.route,
				)
			}
			if test.wantNative {
				if !strings.Contains(","+joined+",", ",claude-native-only,") {
					t.Fatalf("native-route models omitted proxied native entry: %v", ids)
				}
				if countString(ids, "claude-opus-5") != 1 {
					t.Fatalf("catalog/native duplicate was not removed: %v", ids)
				}
				if hits := nativeHits.Load(); hits != 1 {
					t.Fatalf("native upstream hits = %d, want 1", hits)
				}
			} else {
				if strings.Contains(joined, "claude-native-only") {
					t.Fatalf("Baseten-route models included native-only entry: %v", ids)
				}
				if hits := nativeHits.Load(); hits != 0 {
					t.Fatalf("Baseten route contacted native upstream %d times", hits)
				}
			}
		})
	}
}

func countString(values []string, target string) int {
	count := 0
	for _, value := range values {
		if value == target {
			count++
		}
	}
	return count
}
