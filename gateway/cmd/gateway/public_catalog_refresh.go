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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

const (
	publicCatalogRefreshMinDelay = 6 * time.Hour
	publicCatalogRefreshMaxDelay = 24 * time.Hour
	publicCatalogRefreshTimeout  = 10 * time.Second
	publicCatalogResponseMaxSize = 8 << 20
	providerCatalogCacheMaxSize  = 16 << 20
	publicCatalogStaleAfter      = 48 * time.Hour
	publicCatalogMissRetryAfter  = 5 * time.Minute
)

var (
	publicCatalogURL       = "https://models.dev/api.json"
	publicCatalogNow       = time.Now
	publicCatalogNextDelay = randomPublicCatalogRefreshDelay
)

type publicCatalogRefreshManager struct {
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

type publicCatalogHealth struct {
	Source        string
	LoadedFrom    string
	Revision      string
	ModelCount    int
	PricedModels  int
	LastAttemptAt time.Time
	LastSuccessAt time.Time
	NextRefreshAt time.Time
	Stale         bool
	LastError     string
	Diagnostics   []string
}

func newPublicCatalogRefreshManager() *publicCatalogRefreshManager {
	return &publicCatalogRefreshManager{
		stop: make(chan struct{}),
		done: make(chan struct{}),
		kick: make(chan struct{}, 1),
	}
}

func randomPublicCatalogRefreshDelay() time.Duration {
	width := publicCatalogRefreshMaxDelay - publicCatalogRefreshMinDelay
	if width <= 0 {
		return publicCatalogRefreshMinDelay
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(width)+1))
	if err != nil {
		return 12 * time.Hour
	}
	return publicCatalogRefreshMinDelay + time.Duration(n.Int64())
}

func (g *Gateway) startPublicCatalogRefresh(ctx context.Context) {
	manager := g.publicCatalogRefresh
	if manager == nil || !manager.started.CompareAndSwap(false, true) {
		return
	}
	manager.mu.Lock()
	manager.startedAt = publicCatalogNow().UTC()
	refreshCtx, cancel := context.WithCancel(ctx)
	manager.cancel = cancel
	manager.mu.Unlock()
	g.wg.Add(1)
	go g.runPublicCatalogRefresh(refreshCtx)
}

func (g *Gateway) runPublicCatalogRefresh(ctx context.Context) {
	manager := g.publicCatalogRefresh
	defer g.wg.Done()
	defer close(manager.done)
	defer func() {
		manager.mu.Lock()
		manager.cancel = nil
		manager.mu.Unlock()
	}()

	g.refreshPublicCatalogOnce(ctx)
	timer := time.NewTimer(g.scheduleNextPublicCatalogRefresh())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-manager.stop:
			return
		case <-manager.kick:
			stopAndDrainTimer(timer)
			g.refreshPublicCatalogOnce(ctx)
			timer.Reset(g.scheduleNextPublicCatalogRefresh())
		case <-timer.C:
			g.refreshPublicCatalogOnce(ctx)
			timer.Reset(g.scheduleNextPublicCatalogRefresh())
		}
	}
}

func (g *Gateway) scheduleNextPublicCatalogRefresh() time.Duration {
	delay := publicCatalogNextDelay()
	if delay < publicCatalogRefreshMinDelay {
		delay = publicCatalogRefreshMinDelay
	}
	if delay > publicCatalogRefreshMaxDelay {
		delay = publicCatalogRefreshMaxDelay
	}
	g.publicCatalogRefresh.mu.Lock()
	g.publicCatalogRefresh.nextAt = publicCatalogNow().UTC().Add(delay)
	g.publicCatalogRefresh.mu.Unlock()
	return delay
}

func (g *Gateway) kickPublicCatalogRefresh() {
	manager := g.publicCatalogRefresh
	if manager == nil || !manager.started.Load() {
		return
	}
	manager.mu.Lock()
	lastAttempt := manager.lastAttempt
	manager.mu.Unlock()
	if !lastAttempt.IsZero() &&
		publicCatalogNow().UTC().Sub(lastAttempt) < publicCatalogMissRetryAfter {
		return
	}
	select {
	case manager.kick <- struct{}{}:
	default:
	}
}

func (g *Gateway) stopPublicCatalogRefresh() {
	manager := g.publicCatalogRefresh
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

func (g *Gateway) refreshPublicCatalogOnce(parent context.Context) {
	attemptedAt := publicCatalogNow().UTC()
	manager := g.publicCatalogRefresh
	manager.mu.Lock()
	manager.lastAttempt = attemptedAt
	manager.mu.Unlock()

	ctx, cancel := context.WithTimeout(parent, publicCatalogRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicCatalogURL, nil)
	if err == nil {
		req.Header.Set("Accept", "application/json")
		if etag := g.pricing.Capture().ModelsDevRootETag(); etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		err = g.fetchAndPublishPublicCatalog(req)
	}
	if err != nil {
		message := err.Error()
		if errors.Is(err, context.Canceled) {
			message = "refresh canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			message = "refresh timed out"
		}
		manager.mu.Lock()
		manager.lastError = message
		manager.mu.Unlock()
		fmt.Fprintf(os.Stderr,
			"[gateway] public model catalog refresh failed: %v; keeping last-known-good catalog\n",
			err)
		return
	}

	manager.mu.Lock()
	manager.lastSuccess = publicCatalogNow().UTC()
	manager.lastError = ""
	manager.mu.Unlock()
}

func (g *Gateway) fetchAndPublishPublicCatalog(req *http.Request) error {
	clientCopy := *g.client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := clientCopy.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if err := g.pricing.RevalidateModelsDev(
			publicCatalogNow().UTC(),
		); err != nil {
			return err
		}
		return g.persistProviderCatalogCaches()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("models.dev returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(
		io.LimitReader(resp.Body, publicCatalogResponseMaxSize+1),
	)
	if err != nil {
		return err
	}
	if len(body) > publicCatalogResponseMaxSize {
		return fmt.Errorf(
			"models.dev response exceeds %d bytes",
			publicCatalogResponseMaxSize,
		)
	}
	if err := g.pricing.ReplaceModelsDev(
		body,
		publicCatalogNow().UTC(),
		resp.Header.Get("ETag"),
	); err != nil {
		return err
	}
	return g.persistProviderCatalogCaches()
}

func providerCatalogCacheDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "catalogs")
}

func providerCatalogCachePath(configPath, provider string) string {
	return filepath.Join(
		providerCatalogCacheDir(configPath),
		provider+"-public.json",
	)
}

func loadProviderCatalogCaches(p *pricing.Pricing, configPath string) {
	for _, provider := range []string{
		pricing.ProviderAnthropic,
		pricing.ProviderOpenAI,
		pricing.ProviderBaseten,
	} {
		path := providerCatalogCachePath(configPath, provider)
		body, err := readProviderCatalogCache(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"[gateway] could not read %s model catalog cache: %v\n",
				provider,
				err)
			continue
		}
		if err := p.ImportProviderCache(body); err != nil {
			fmt.Fprintf(os.Stderr,
				"[gateway] ignored invalid %s model catalog cache: %v\n",
				provider,
				err)
		}
	}
}

func readProviderCatalogCache(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(
		io.LimitReader(file, providerCatalogCacheMaxSize+1),
	)
	if err != nil {
		return nil, err
	}
	if len(body) > providerCatalogCacheMaxSize {
		return nil, fmt.Errorf(
			"model catalog cache exceeds %d bytes",
			providerCatalogCacheMaxSize,
		)
	}
	return body, nil
}

func persistProviderCatalogCaches(
	p *pricing.Pricing,
	configPath string,
) error {
	return persistProviderCatalogSnapshotCaches(p.Capture(), configPath)
}

func (g *Gateway) persistProviderCatalogCaches() error {
	g.catalogCacheMu.Lock()
	defer g.catalogCacheMu.Unlock()
	return persistProviderCatalogSnapshotCaches(
		g.pricing.Capture(),
		g.activeConfigPath(),
	)
}

func persistProviderCatalogSnapshotCaches(
	snapshot *pricing.Snapshot,
	configPath string,
) error {
	dir := providerCatalogCacheDir(configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create model catalog cache directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure model catalog cache directory: %w", err)
	}
	for _, provider := range []string{
		pricing.ProviderAnthropic,
		pricing.ProviderOpenAI,
		pricing.ProviderBaseten,
	} {
		body, err := snapshot.ExportProviderCache(provider)
		if err != nil {
			return fmt.Errorf("export %s model catalog cache: %w", provider, err)
		}
		if len(body) == 0 {
			continue
		}
		if err := writePrivateAtomic(
			providerCatalogCachePath(configPath, provider),
			body,
		); err != nil {
			return fmt.Errorf("persist %s model catalog cache: %w", provider, err)
		}
	}
	return nil
}

func writePrivateAtomic(path string, body []byte) (err error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		if temp != nil {
			_ = temp.Close()
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err = temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err = temp.Write(body); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		temp = nil
		return err
	}
	temp = nil
	return os.Rename(tempPath, path)
}

func (g *Gateway) publicCatalogProviderHealth(
	provider string,
) publicCatalogHealth {
	metadata := g.pricing.Capture().ProviderMetadata(provider)
	manager := g.publicCatalogRefresh
	health := publicCatalogHealth{
		Source:       metadata.Provenance.Source,
		LoadedFrom:   string(metadata.Provenance.LoadedFrom),
		Revision:     metadata.Provenance.Revision,
		ModelCount:   metadata.ModelCount,
		PricedModels: metadata.PricedModelCount,
		Diagnostics:  append([]string(nil), metadata.Diagnostics...),
	}
	if manager == nil {
		return health
	}
	manager.mu.Lock()
	startedAt := manager.startedAt
	health.LastAttemptAt = manager.lastAttempt
	health.LastSuccessAt = manager.lastSuccess
	health.NextRefreshAt = manager.nextAt
	health.LastError = manager.lastError
	manager.mu.Unlock()
	freshFrom := health.LastSuccessAt
	if freshFrom.IsZero() {
		freshFrom = metadata.ModelsDevValidatedAt
	}
	if freshFrom.IsZero() {
		freshFrom = metadata.Provenance.CapturedAt
	}
	if freshFrom.IsZero() {
		freshFrom = startedAt
	}
	health.Stale = !freshFrom.IsZero() &&
		publicCatalogNow().UTC().Sub(freshFrom) > publicCatalogStaleAfter
	return health
}

func publicCatalogHealthJSON(health publicCatalogHealth) map[string]any {
	var lastError any
	if strings.TrimSpace(health.LastError) != "" {
		lastError = health.LastError
	}
	return map[string]any{
		"source":             health.Source,
		"loaded_from":        health.LoadedFrom,
		"revision":           health.Revision,
		"model_count":        health.ModelCount,
		"priced_model_count": health.PricedModels,
		"last_attempt_at":    rfc3339OrEmpty(health.LastAttemptAt),
		"last_success_at":    rfc3339OrEmpty(health.LastSuccessAt),
		"next_refresh_at":    rfc3339OrEmpty(health.NextRefreshAt),
		"stale":              health.Stale,
		"last_error":         lastError,
		"diagnostics":        health.Diagnostics,
	}
}
