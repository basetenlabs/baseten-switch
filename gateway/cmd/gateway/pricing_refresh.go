package gateway

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	catalogRefreshMinDelay = 55 * time.Minute
	catalogRefreshMaxDelay = 65 * time.Minute
	catalogRefreshTimeout  = 20 * time.Second
	catalogResponseMaxSize = 32 << 20
	catalogStaleAfter      = 2 * time.Hour
)

var (
	catalogNow       = time.Now
	catalogNextDelay = randomCatalogRefreshDelay
)

type catalogRefreshManager struct {
	started  atomic.Bool
	stop     chan struct{}
	done     chan struct{}
	kick     chan struct{}
	stopOnce sync.Once

	mu          sync.Mutex
	startedAt   time.Time
	nextAt      time.Time
	lastAttempt time.Time
	lastSuccess time.Time
	lastError   string
	cancel      context.CancelFunc
}

type catalogHealth struct {
	Source        string
	Revision      string
	ModelCount    int
	LiveHydrated  bool
	LastAttemptAt time.Time
	LastSuccessAt time.Time
	NextRefreshAt time.Time
	Stale         bool
	LastError     string
}

func newCatalogRefreshManager() *catalogRefreshManager {
	return &catalogRefreshManager{
		stop: make(chan struct{}),
		done: make(chan struct{}),
		kick: make(chan struct{}, 1),
	}
}

func randomCatalogRefreshDelay() time.Duration {
	width := catalogRefreshMaxDelay - catalogRefreshMinDelay
	if width <= 0 {
		return catalogRefreshMinDelay
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(width)+1))
	if err != nil {
		return time.Hour
	}
	return catalogRefreshMinDelay + time.Duration(n.Int64())
}

// startCatalogRefresh starts exactly one refresh loop. The first refresh is
// asynchronous so listener readiness never depends on catalog availability.
func (g *Gateway) startCatalogRefresh(ctx context.Context) {
	manager := g.catalogRefresh
	if manager == nil || !manager.started.CompareAndSwap(false, true) {
		return
	}
	manager.mu.Lock()
	manager.startedAt = catalogNow().UTC()
	refreshCtx, cancel := context.WithCancel(ctx)
	manager.cancel = cancel
	manager.mu.Unlock()
	g.wg.Add(1)
	go g.runCatalogRefresh(refreshCtx)
}

func (g *Gateway) runCatalogRefresh(ctx context.Context) {
	manager := g.catalogRefresh
	defer g.wg.Done()
	defer close(manager.done)
	defer func() {
		manager.mu.Lock()
		manager.cancel = nil
		manager.mu.Unlock()
	}()

	// Immediate startup attempt. A signed-out gateway simply keeps its
	// embedded fallback until a credential-change kick or scheduled check.
	g.refreshCatalogOnce(ctx)
	timer := time.NewTimer(g.scheduleNextCatalogRefresh())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-manager.stop:
			return
		case <-manager.kick:
			stopAndDrainTimer(timer)
			g.refreshCatalogOnce(ctx)
			timer.Reset(g.scheduleNextCatalogRefresh())
		case <-timer.C:
			g.refreshCatalogOnce(ctx)
			timer.Reset(g.scheduleNextCatalogRefresh())
		}
	}
}

func stopAndDrainTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (g *Gateway) scheduleNextCatalogRefresh() time.Duration {
	delay := catalogNextDelay()
	if delay < catalogRefreshMinDelay {
		delay = catalogRefreshMinDelay
	}
	if delay > catalogRefreshMaxDelay {
		delay = catalogRefreshMaxDelay
	}
	g.catalogRefresh.mu.Lock()
	g.catalogRefresh.nextAt = catalogNow().UTC().Add(delay)
	g.catalogRefresh.mu.Unlock()
	return delay
}

// kickCatalogRefresh requests one immediate refresh. Kicks before Serve starts
// are ignored because the loop always performs an immediate startup attempt.
func (g *Gateway) kickCatalogRefresh() {
	manager := g.catalogRefresh
	if manager == nil || !manager.started.Load() {
		return
	}
	select {
	case manager.kick <- struct{}{}:
	default:
	}
}

func (g *Gateway) kickCatalogRefreshForPricingMiss() {
	manager := g.catalogRefresh
	if manager == nil || !manager.started.Load() {
		return
	}
	manager.mu.Lock()
	lastAttempt := manager.lastAttempt
	manager.mu.Unlock()
	if !lastAttempt.IsZero() &&
		catalogNow().UTC().Sub(lastAttempt) < 5*time.Minute {
		return
	}
	g.kickCatalogRefresh()
}

func (g *Gateway) stopCatalogRefresh() {
	manager := g.catalogRefresh
	if manager == nil {
		return
	}
	manager.stopOnce.Do(func() {
		close(manager.stop)
		manager.mu.Lock()
		cancel := manager.cancel
		manager.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
}

// refreshCatalogOnce performs one authenticated GET /v1/models without
// holding configuration, auth, routing, or pricing publication locks.
func (g *Gateway) refreshCatalogOnce(parent context.Context) {
	cfg := g.runtimeConfig()
	client, authorization, ok := g.catalogRequestClient(cfg)
	if !ok {
		return
	}

	attemptedAt := catalogNow().UTC()
	g.catalogRefresh.mu.Lock()
	g.catalogRefresh.lastAttempt = attemptedAt
	g.catalogRefresh.mu.Unlock()

	ctx, cancel := context.WithTimeout(parent, catalogRefreshTimeout)
	defer cancel()
	url := strings.TrimRight(cfg.BasetenURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err == nil {
		req.Header.Set("Accept", "application/json")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		err = g.fetchAndPublishCatalog(client, req)
	}
	if err != nil {
		g.recordCatalogRefreshError(err)
		fmt.Fprintf(os.Stderr, "[gateway] baseten catalog refresh failed: %v; keeping last-known-good pricing\n", err)
		return
	}

	succeededAt := catalogNow().UTC()
	g.catalogRefresh.mu.Lock()
	g.catalogRefresh.lastSuccess = succeededAt
	g.catalogRefresh.lastError = ""
	g.catalogRefresh.mu.Unlock()
	if strings.TrimSpace(g.activeConfigPath()) != "" {
		if err := g.persistProviderCatalogCaches(); err != nil {
			fmt.Fprintf(
				os.Stderr,
				"[gateway] could not cache authenticated Baseten model catalog: %v\n",
				err,
			)
		}
	}
	metadata := g.pricing.Capture().BasetenMetadata()
	fmt.Fprintf(os.Stderr,
		"[gateway] baseten catalog refreshed models=%d priced=%d revision=%s\n",
		metadata.ModelCount, metadata.PricedModelCount, metadata.Revision)
}

func (g *Gateway) catalogRequestClient(cfg Config) (*http.Client, string, bool) {
	g.authMu.Lock()
	oauthClient := g.oauthClient
	g.authMu.Unlock()
	if oauthClient != nil {
		return oauthClient, "", true
	}
	if cfg.APIKeyFallback && cfg.BasetenKey != "" {
		return g.client, "Bearer " + cfg.BasetenKey, true
	}
	return nil, "", false
}

func (g *Gateway) fetchAndPublishCatalog(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("baseten /v1/models returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, catalogResponseMaxSize+1))
	if err != nil {
		return err
	}
	if len(body) > catalogResponseMaxSize {
		return fmt.Errorf("baseten /v1/models response exceeds %d bytes", catalogResponseMaxSize)
	}
	return g.pricing.ReplaceBasetenCatalog(
		body,
		"baseten_v1_models",
		catalogNow().UTC(),
		"",
	)
}

func (g *Gateway) recordCatalogRefreshError(err error) {
	message := err.Error()
	if errors.Is(err, context.Canceled) {
		message = "refresh canceled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		message = "refresh timed out"
	}
	g.catalogRefresh.mu.Lock()
	g.catalogRefresh.lastError = message
	g.catalogRefresh.mu.Unlock()
}

func (g *Gateway) catalogHealth() catalogHealth {
	now := catalogNow().UTC()
	metadata := g.pricing.Capture().BasetenMetadata()
	manager := g.catalogRefresh
	if manager == nil {
		return catalogHealth{
			Source:       metadata.Source,
			Revision:     metadata.Revision,
			ModelCount:   metadata.ModelCount,
			LiveHydrated: metadata.Source == "baseten_v1_models",
		}
	}
	manager.mu.Lock()
	startedAt := manager.startedAt
	health := catalogHealth{
		Source:        metadata.Source,
		Revision:      metadata.Revision,
		ModelCount:    metadata.ModelCount,
		LiveHydrated:  metadata.Source == "baseten_v1_models",
		LastAttemptAt: manager.lastAttempt,
		LastSuccessAt: manager.lastSuccess,
		NextRefreshAt: manager.nextAt,
		LastError:     manager.lastError,
	}
	manager.mu.Unlock()

	freshFrom := health.LastSuccessAt
	if freshFrom.IsZero() {
		freshFrom = startedAt
	}
	health.Stale = !freshFrom.IsZero() && now.Sub(freshFrom) > catalogStaleAfter
	return health
}

func catalogHealthJSON(health catalogHealth) map[string]any {
	var lastError any
	if health.LastError != "" {
		lastError = health.LastError
	}
	return map[string]any{
		"source":          health.Source,
		"revision":        health.Revision,
		"model_count":     health.ModelCount,
		"live_hydrated":   health.LiveHydrated,
		"last_attempt_at": rfc3339OrEmpty(health.LastAttemptAt),
		"last_success_at": rfc3339OrEmpty(health.LastSuccessAt),
		"next_refresh_at": rfc3339OrEmpty(health.NextRefreshAt),
		"stale":           health.Stale,
		"last_error":      lastError,
	}
}
