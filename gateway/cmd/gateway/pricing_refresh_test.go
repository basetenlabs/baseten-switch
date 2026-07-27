package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

const liveCatalogFixture = `{
  "data": [
    {
      "id": "new/model",
      "pricing": {
        "prompt": 0.000002,
        "completion": 0.000003
      }
    }
  ]
}`

func catalogTestGateway(server *httptest.Server) *Gateway {
	return &Gateway{
		cfg: Config{
			BasetenURL:     server.URL,
			BasetenKey:     "catalog-key",
			APIKeyFallback: true,
		},
		pricing:        pricing.New(),
		client:         server.Client(),
		catalogRefresh: newCatalogRefreshManager(),
	}
}

func TestCatalogRefreshPublishesLiveSnapshotWithBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer catalog-key" {
			t.Fatalf("Authorization = %q, want Bearer catalog-key", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q", got)
		}
		_, _ = w.Write([]byte(liveCatalogFixture))
	}))
	defer server.Close()

	g := catalogTestGateway(server)
	g.cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
	g.refreshCatalogOnce(context.Background())

	metadata := g.pricing.Capture().BasetenMetadata()
	if metadata.Source != "baseten_v1_models" || metadata.ModelCount != 1 {
		t.Fatalf("metadata = %+v", metadata)
	}
	if got := g.pricing.BasetenPrice("new/model").Prompt; got != 2 {
		t.Fatalf("live prompt = %v, want 2", got)
	}
	health := g.catalogHealth()
	if !health.LiveHydrated || health.LastAttemptAt.IsZero() ||
		health.LastSuccessAt.IsZero() || health.LastError != "" {
		t.Fatalf("health = %+v", health)
	}
	restored := pricing.New()
	loadProviderCatalogCaches(restored, g.cfg.ConfigPath)
	if quote := restored.Quote(
		pricing.ProviderBaseten,
		"new/model",
	); !quote.Priced || quote.Price.Prompt != 2 {
		t.Fatalf("restored Baseten quote = %+v", quote)
	}
	restoredMetadata := restored.Capture().BasetenMetadata()
	if restoredMetadata.Source != "baseten_v1_models" ||
		restoredMetadata.Revision != metadata.Revision ||
		restoredMetadata.ModelCount != 1 ||
		restoredMetadata.PricedModelCount != 1 {
		t.Fatalf(
			"cache-restored Baseten metadata = %+v, live = %+v",
			restoredMetadata,
			metadata,
		)
	}
	if err := restored.CheckBasetenModel("new/model"); err != nil ||
		restored.BasetenModelCount() != 1 ||
		restored.BasetenCount() != 1 {
		t.Fatalf(
			"cache-restored Baseten catalog APIs disagree: check=%v models=%d prices=%d",
			err,
			restored.BasetenModelCount(),
			restored.BasetenCount(),
		)
	}
	restoredGateway := &Gateway{pricing: restored}
	restoredHealth := restoredGateway.catalogHealth()
	if !restoredHealth.LiveHydrated ||
		restoredHealth.Source != "baseten_v1_models" ||
		restoredHealth.Revision != metadata.Revision ||
		restoredHealth.ModelCount != 1 {
		t.Fatalf(
			"cache-restored catalog health = %+v",
			restoredHealth,
		)
	}
}

func TestCatalogRefreshFailureRetainsLastKnownGood(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(liveCatalogFixture))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"new/model","pricing":{"prompt":-1}}]}`))
	}))
	defer server.Close()

	g := catalogTestGateway(server)
	g.refreshCatalogOnce(context.Background())
	lastGood := g.pricing.Capture()
	g.refreshCatalogOnce(context.Background())

	if g.pricing.Capture() != lastGood {
		t.Fatal("invalid refresh replaced last-known-good snapshot")
	}
	health := g.catalogHealth()
	if !health.LiveHydrated || health.LastError == "" {
		t.Fatalf("health after failed refresh = %+v", health)
	}
	if !strings.Contains(health.LastError, "nonnegative") {
		t.Fatalf("last error = %q", health.LastError)
	}
}

func TestCatalogRefreshLoopAttemptsImmediatelyAndRespondsToKick(t *testing.T) {
	var calls atomic.Int32
	callCh := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		callCh <- struct{}{}
		_, _ = w.Write([]byte(liveCatalogFixture))
	}))
	defer server.Close()

	g := catalogTestGateway(server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.startCatalogRefresh(ctx)
	waitForCatalogCall(t, callCh)
	g.kickCatalogRefresh()
	waitForCatalogCall(t, callCh)
	if got := calls.Load(); got != 2 {
		t.Fatalf("catalog calls = %d, want 2", got)
	}

	cancel()
	select {
	case <-g.catalogRefresh.done:
	case <-time.After(2 * time.Second):
		t.Fatal("catalog refresh loop did not stop")
	}
}

func TestCatalogRefreshStopCancelsInFlightRequest(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()

	g := catalogTestGateway(server)
	g.startCatalogRefresh(context.Background())
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("catalog request did not start")
	}
	g.stopCatalogRefresh()
	select {
	case <-g.catalogRefresh.done:
	case <-time.After(2 * time.Second):
		t.Fatal("catalog refresh loop did not stop after cancel")
	}
}

func TestRandomCatalogRefreshDelayWithinBounds(t *testing.T) {
	for range 100 {
		delay := randomCatalogRefreshDelay()
		if delay < catalogRefreshMinDelay || delay > catalogRefreshMaxDelay {
			t.Fatalf("delay %s outside [%s, %s]", delay, catalogRefreshMinDelay, catalogRefreshMaxDelay)
		}
	}
}

func waitForCatalogCall(t *testing.T, calls <-chan struct{}) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for catalog refresh")
	}
}
