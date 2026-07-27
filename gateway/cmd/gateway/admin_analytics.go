package gateway

import (
	"net/http"
	"strconv"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/analytics"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

const (
	analyticsDefaultWindow = 7 * 24 * time.Hour
	analyticsMaxWindow     = 30 * 24 * time.Hour
	analyticsCacheEntries  = 8
)

var (
	analyticsNow = time.Now
	// analyticsBuild remains injectable for deterministic cache tests.
	analyticsBuild = analytics.Build
)

type analyticsResponseCacheKey struct {
	Generation           uint64
	Since                int64
	Until                int64
	CollectionEnabled    bool
	CoverageComplete     bool
	CoverageReason       string
	PresentationRevision string
}

type analyticsResponseCacheEntry struct {
	key      analyticsResponseCacheKey
	response analytics.Response
}

func (g *Gateway) adminAnalytics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		g.reject(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	now := analyticsNow()
	until, ok := analyticsUnixParam(r.URL.Query().Get("until"), now.Unix())
	if !ok {
		g.reject(w, http.StatusBadRequest, "invalid until: "+r.URL.Query().Get("until"))
		return
	}
	since, ok := analyticsUnixParam(
		r.URL.Query().Get("since"),
		until-int64(analyticsDefaultWindow/time.Second),
	)
	if !ok {
		g.reject(w, http.StatusBadRequest, "invalid since: "+r.URL.Query().Get("since"))
		return
	}
	if since >= until {
		g.reject(w, http.StatusBadRequest, "since must be before until")
		return
	}
	if until-since > int64(analyticsMaxWindow/time.Second) {
		g.reject(w, http.StatusBadRequest, "range exceeds 30 days")
		return
	}

	index := g.getAnalyticsIndex()
	runtimeCfg := g.runtimeConfig()
	retained := index.Snapshot(runtimeCfg.TelemetryDir, now, since)
	collectionEnabled := runtimeCfg.TelemetryEnabled == nil ||
		*runtimeCfg.TelemetryEnabled
	window := analytics.Window{Since: since, Until: until}
	response := g.analyticsResponse(
		retained,
		window,
		now.Unix(),
		collectionEnabled,
	)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (g *Gateway) analyticsResponse(
	retained analytics.Snapshot,
	window analytics.Window,
	generatedAt int64,
	collectionEnabled bool,
) analytics.Response {
	var catalog *pricing.Snapshot
	if g.pricing != nil {
		catalog = g.pricing.Capture()
	}
	presentationRevision := ""
	if catalog != nil {
		presentationRevision = catalog.PresentationRevision(
			pricing.ProviderBaseten,
		)
	}
	key := analyticsResponseCacheKey{
		Generation:           retained.Generation,
		Since:                window.Since,
		Until:                window.Until,
		CollectionEnabled:    collectionEnabled,
		CoverageComplete:     retained.Complete,
		CoverageReason:       retained.Reason,
		PresentationRevision: presentationRevision,
	}

	g.analyticsCacheMu.Lock()
	defer g.analyticsCacheMu.Unlock()
	for index := range g.analyticsCache {
		entry := g.analyticsCache[index]
		if entry.key != key {
			continue
		}
		response := entry.response
		response.GeneratedAt = generatedAt
		if index != len(g.analyticsCache)-1 {
			copy(g.analyticsCache[index:], g.analyticsCache[index+1:])
			g.analyticsCache[len(g.analyticsCache)-1] = entry
		}
		return response
	}

	response := analyticsBuild(
		retained.Events,
		window,
		generatedAt,
		retained,
		collectionEnabled,
		catalog,
	)
	g.analyticsCache = append(g.analyticsCache, analyticsResponseCacheEntry{
		key:      key,
		response: response,
	})
	if len(g.analyticsCache) > analyticsCacheEntries {
		copy(
			g.analyticsCache,
			g.analyticsCache[len(g.analyticsCache)-analyticsCacheEntries:],
		)
		g.analyticsCache = g.analyticsCache[:analyticsCacheEntries]
	}
	return response
}

func analyticsUnixParam(value string, defaultValue int64) (int64, bool) {
	if value == "" {
		return defaultValue, true
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func (g *Gateway) getAnalyticsIndex() *analytics.Index {
	g.analyticsMu.Lock()
	defer g.analyticsMu.Unlock()
	if g.analyticsIndex == nil {
		g.analyticsIndex = analytics.NewIndex(analytics.IndexOptions{})
	}
	return g.analyticsIndex
}
