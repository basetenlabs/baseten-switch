package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

const publicCatalogGatewayFixture = `{
  "anthropic": {
    "id": "anthropic",
    "models": {
      "claude-opus-5": {
        "id": "claude-opus-5",
        "name": "Claude Opus 5",
        "family": "claude-opus",
        "limit": {"context": 1000000, "output": 128000},
        "cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25}
      },
      "claude-haiku-unpriced": {
        "id": "claude-haiku-unpriced",
        "name": "Claude Haiku Unpriced",
        "family": "claude-haiku",
        "limit": {"context": 200000, "output": 64000}
      },
      "claude-newfamily-1": {
        "id": "claude-newfamily-1",
        "name": "Claude Newfamily 1",
        "family": "claude-newfamily",
        "limit": {"context": 300000, "output": 64000},
        "cost": {"input": 2, "output": 10}
      }
    }
  },
  "openai": {
    "id": "openai",
    "models": {
      "gpt-test": {
        "id": "gpt-test",
        "name": "GPT Test",
        "cost": {"input": 1, "output": 4}
      }
    }
  },
  "baseten": {
    "id": "baseten",
    "models": {
      "zai-org/GLM-Test": {
        "id": "zai-org/GLM-Test",
        "name": "GLM Test",
        "reasoning": true,
        "reasoning_options": [{"type": "toggle"}],
		"limit": {"context": 1048576, "output": 65536},
        "cost": {"input": 0.3, "output": 0.75}
      }
    }
  }
}`

func TestPublicCatalogRefreshPersistsReloadsAndUsesETag(t *testing.T) {
	var responseNotModified atomic.Bool
	ifNoneMatch := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" ||
			r.Header.Get("Authorization") != "" ||
			r.Header.Get("X-Api-Key") != "" {
			t.Errorf(
				"public catalog request leaked query or credential headers: query=%q headers=%v",
				r.URL.RawQuery,
				r.Header,
			)
		}
		ifNoneMatch <- r.Header.Get("If-None-Match")
		if responseNotModified.Load() {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"catalog-v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, publicCatalogGatewayFixture)
	}))
	defer server.Close()

	restorePublicCatalogTestGlobals(t)
	publicCatalogURL = server.URL
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	publicCatalogNow = func() time.Time { return now }

	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	g := gatewayForPublicCatalogTest(configPath, server.Client())
	g.refreshPublicCatalogOnce(context.Background())

	metadata := g.pricing.Capture().ProviderMetadata(pricing.ProviderAnthropic)
	if metadata.Provenance.Source != "models_dev" ||
		metadata.Provenance.LoadedFrom != pricing.LoadedFromLive ||
		metadata.Provenance.ETag != `"catalog-v1"` ||
		metadata.ModelCount != 3 ||
		metadata.PricedModelCount != 2 {
		t.Fatalf("live Anthropic metadata = %+v", metadata)
	}
	if first := <-ifNoneMatch; first != "" {
		t.Fatalf("first If-None-Match = %q, want empty", first)
	}

	cachePath := providerCatalogCachePath(configPath, pricing.ProviderAnthropic)
	cacheInfo, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat Anthropic cache: %v", err)
	}
	if permissions := cacheInfo.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("cache permissions = %#o, want 0600", permissions)
	}

	reloaded := pricing.New()
	loadProviderCatalogCaches(reloaded, configPath)
	reloadedMetadata := reloaded.Capture().
		ProviderMetadata(pricing.ProviderAnthropic)
	if reloadedMetadata.Provenance.LoadedFrom != pricing.LoadedFromRuntimeCache ||
		reloadedMetadata.Provenance.Source != "models_dev" ||
		reloadedMetadata.Provenance.ETag != `"catalog-v1"` ||
		reloadedMetadata.ModelCount != 3 ||
		reloadedMetadata.PricedModelCount != 2 {
		t.Fatalf("runtime-cache Anthropic metadata = %+v", reloadedMetadata)
	}
	quote := reloaded.Capture().QuoteProfile(
		pricing.ProviderAnthropic,
		"claude-opus-5",
		pricing.ProfileStandard,
	)
	if !quote.Priced || quote.Price.Prompt != 5 || quote.Price.Completion != 25 {
		t.Fatalf("runtime-cache quote = %+v", quote)
	}

	beforeNotModified := g.pricing.Capture()
	beforeMetadata := beforeNotModified.ProviderMetadata(
		pricing.ProviderAnthropic,
	)
	beforeQuote := beforeNotModified.Quote(
		pricing.ProviderAnthropic,
		"claude-opus-5",
	)
	responseNotModified.Store(true)
	now = now.Add(49 * time.Hour)
	g.refreshPublicCatalogOnce(context.Background())
	afterNotModified := g.pricing.Capture()
	afterMetadata := afterNotModified.ProviderMetadata(
		pricing.ProviderAnthropic,
	)
	if afterMetadata.Provenance.Revision !=
		beforeMetadata.Provenance.Revision ||
		afterMetadata.Provenance.CapturedAt !=
			beforeMetadata.Provenance.CapturedAt ||
		afterNotModified.Quote(
			pricing.ProviderAnthropic,
			"claude-opus-5",
		) != beforeQuote {
		t.Fatal("304 response changed normalized catalog content")
	}
	if afterMetadata.ModelsDevValidatedAt != now {
		t.Fatalf("304 validated_at = %s, want %s",
			afterMetadata.ModelsDevValidatedAt, now)
	}
	if second := <-ifNoneMatch; second != `"catalog-v1"` {
		t.Fatalf("refresh If-None-Match = %q, want catalog ETag", second)
	}
	health := g.publicCatalogProviderHealth(pricing.ProviderAnthropic)
	if health.LastSuccessAt != now || health.LastError != "" || health.Stale {
		t.Fatalf("health after 304 = %+v", health)
	}
	revalidatedCache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart := pricing.New()
	if err := afterRestart.ImportProviderCache(revalidatedCache); err != nil {
		t.Fatal(err)
	}
	if got := afterRestart.Capture().ProviderMetadata(
		pricing.ProviderAnthropic,
	).ModelsDevValidatedAt; got != now {
		t.Fatalf("persisted 304 validated_at = %s, want %s", got, now)
	}
}

func TestPublicCatalogRefreshOmitsRootETagWhenProviderCacheIsMissing(
	t *testing.T,
) {
	ifNoneMatch := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			ifNoneMatch <- r.Header.Get("If-None-Match")
			w.Header().Set("ETag", `"repaired"`)
			_, _ = io.WriteString(w, publicCatalogGatewayFixture)
		},
	))
	defer server.Close()

	restorePublicCatalogTestGlobals(t)
	publicCatalogURL = server.URL
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	publicCatalogNow = func() time.Time { return now }

	seed := pricing.New()
	if err := seed.ReplaceModelsDev(
		[]byte(publicCatalogGatewayFixture),
		now.Add(-time.Hour),
		`"root-etag"`,
	); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := persistProviderCatalogCaches(seed, configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(providerCatalogCachePath(
		configPath,
		pricing.ProviderBaseten,
	)); err != nil {
		t.Fatal(err)
	}
	partial := pricing.New()
	loadProviderCatalogCaches(partial, configPath)
	if etag := partial.Capture().ModelsDevRootETag(); etag != "" {
		t.Fatalf("partial root ETag = %q, want empty", etag)
	}
	g := gatewayForPublicCatalogTest(configPath, server.Client())
	g.pricing = partial
	g.refreshPublicCatalogOnce(context.Background())
	if got := <-ifNoneMatch; got != "" {
		t.Fatalf("repair If-None-Match = %q, want empty", got)
	}
	if etag := g.pricing.Capture().ModelsDevRootETag(); etag !=
		`"repaired"` {
		t.Fatalf("repaired root ETag = %q", etag)
	}
}

func TestPublicCatalogRefreshFailuresPreserveLastKnownGood(t *testing.T) {
	const (
		catalogResponseSuccess int32 = iota
		catalogResponseMalformed
		catalogResponseUnavailable
	)
	var mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case catalogResponseSuccess:
			w.Header().Set("ETag", `"catalog-good"`)
			_, _ = io.WriteString(w, publicCatalogGatewayFixture)
		case catalogResponseMalformed:
			_, _ = io.WriteString(w, `{"anthropic":`)
		case catalogResponseUnavailable:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"unavailable"}`)
		}
	}))
	defer server.Close()

	restorePublicCatalogTestGlobals(t)
	publicCatalogURL = server.URL
	now := time.Date(2026, time.July, 26, 14, 0, 0, 0, time.UTC)
	publicCatalogNow = func() time.Time { return now }

	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	g := gatewayForPublicCatalogTest(configPath, server.Client())
	g.refreshPublicCatalogOnce(context.Background())
	lastKnownGood := g.pricing.Capture()
	cachePath := providerCatalogCachePath(configPath, pricing.ProviderAnthropic)
	cacheBefore, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}

	for _, failure := range []struct {
		name string
		mode int32
	}{
		{name: "malformed", mode: catalogResponseMalformed},
		{name: "unavailable", mode: catalogResponseUnavailable},
	} {
		t.Run(failure.name, func(t *testing.T) {
			mode.Store(failure.mode)
			now = now.Add(time.Hour)
			g.refreshPublicCatalogOnce(context.Background())
			if g.pricing.Capture() != lastKnownGood {
				t.Fatalf("%s response replaced the active pricing snapshot", failure.name)
			}
			cacheAfter, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(cacheAfter) != string(cacheBefore) {
				t.Fatalf("%s response replaced the runtime cache", failure.name)
			}
			health := g.publicCatalogProviderHealth(pricing.ProviderAnthropic)
			if strings.TrimSpace(health.LastError) == "" {
				t.Fatalf("%s response did not surface a refresh error", failure.name)
			}
		})
	}
}

func TestProviderCatalogCacheReadIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.json")
	body := make([]byte, providerCatalogCacheMaxSize+1)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProviderCatalogCache(path); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized cache error = %v", err)
	}
}

func TestPublicCatalogRefreshRejectsRedirectAndOversizedResponse(t *testing.T) {
	var redirectTargetReached atomic.Bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			redirectTargetReached.Store(true)
			_, _ = io.WriteString(w, publicCatalogGatewayFixture)
		},
	))
	defer redirectTarget.Close()

	tests := []struct {
		name      string
		handler   http.Handler
		errorText string
	}{
		{
			name: "redirect",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
			}),
			errorText: "302",
		},
		{
			name: "oversized response",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(bytes.Repeat(
					[]byte("x"),
					publicCatalogResponseMaxSize+1,
				))
			}),
			errorText: "exceeds",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()

			restorePublicCatalogTestGlobals(t)
			publicCatalogURL = server.URL
			g := gatewayForPublicCatalogTest(
				filepath.Join(t.TempDir(), "gateway.yaml"),
				server.Client(),
			)
			g.refreshPublicCatalogOnce(context.Background())
			health := g.publicCatalogProviderHealth(
				pricing.ProviderAnthropic,
			)
			if !strings.Contains(health.LastError, test.errorText) {
				t.Fatalf(
					"refresh error = %q, want %q",
					health.LastError,
					test.errorText,
				)
			}
			if metadata := g.pricing.Capture().ProviderMetadata(
				pricing.ProviderAnthropic,
			); metadata.Provenance.LoadedFrom == pricing.LoadedFromLive {
				t.Fatalf("rejected response published live metadata: %+v", metadata)
			}
		})
	}
	if redirectTargetReached.Load() {
		t.Fatal("public catalog refresh followed a redirect")
	}
}

func gatewayForPublicCatalogTest(
	configPath string,
	client *http.Client,
) *Gateway {
	return &Gateway{
		cfg:                  Config{ConfigPath: configPath},
		pricing:              pricing.New(),
		client:               client,
		publicCatalogRefresh: newPublicCatalogRefreshManager(),
	}
}

func restorePublicCatalogTestGlobals(t *testing.T) {
	t.Helper()
	originalURL := publicCatalogURL
	originalNow := publicCatalogNow
	t.Cleanup(func() {
		publicCatalogURL = originalURL
		publicCatalogNow = originalNow
	})
}
