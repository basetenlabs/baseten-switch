package gateway

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

// GET /v1/admin/stats: the read-only glance-strip feed for the menubar
// popup (the menubar control-surface contract, data plumbing). Server-side
// windowing over telemetry v1 segments so the app never parses telemetry
// files or re-implements bucketing: fixed-width zero-filled buckets of
// request counts, error counts, and latency percentiles for the
// trailing window, per-client fallback state, and the last N request
// summaries.

const (
	statsDefaultWindow = 3600  // seconds
	statsMaxWindow     = 21600 // seconds
	statsDefaultBucket = 60    // seconds
	statsMinBucket     = 10    // seconds
	statsRecentN       = 20

	// statsTailMaxBytes is retained for the injected test reader. Production
	// reads use the shared analytics index, whose bootstrap and retention
	// bounds cover both native admin feeds.
	statsTailMaxBytes = 4 << 20

	// statsCacheTTL is how long one parsed tail is reused. The popup
	// glance strip polls every few seconds while visible; the cache
	// keeps that polling cheap without going meaningfully stale.
	statsCacheTTL = 3 * time.Second
)

// Seams for tests: a swappable clock (bucketing and cache TTL) and optional
// tail reader. Production leaves statsTailRead nil and uses the shared
// analytics index. A test may inject a reader to count cache refreshes.
var (
	statsNow      = time.Now
	statsTailRead func(string, int64) ([]telemetry.EventV1, error)
)

// statsCache holds the indexed telemetry tail for statsCacheTTL, keyed
// by the path it was read from (one gateway per process in practice;
// the key guards test processes that construct several). Package-level
// like gatewayStart above.
var (
	statsMu        sync.Mutex
	statsCachePath string
	statsCacheRows []telemetry.EventV1
	statsCacheAt   time.Time
)

type statsBucket struct {
	TS       int64 `json:"ts"`
	Requests int   `json:"requests"`
	Errors   int   `json:"errors"`
	P50Ms    int64 `json:"p50_ms"`
	P95Ms    int64 `json:"p95_ms"`
}

type statsRecentRow struct {
	TS             float64 `json:"ts"`
	Client         string  `json:"client"`
	Route          string  `json:"route"`
	RouteEffective string  `json:"route_effective"`
	RequestedModel string  `json:"requested_model"`
	UpstreamModel  string  `json:"upstream_model"`
	Status         int     `json:"status"`
	DurationMs     int64   `json:"duration_ms"`
	Subagent       bool    `json:"subagent"`
}

func (g *Gateway) adminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		g.reject(w, 405, "method not allowed")
		return
	}
	q := r.URL.Query()
	window, ok := statsIntParam(q.Get("window"), statsDefaultWindow)
	if !ok {
		g.reject(w, 400, "invalid window: "+q.Get("window"))
		return
	}
	bucket, ok := statsIntParam(q.Get("bucket"), statsDefaultBucket)
	if !ok {
		g.reject(w, 400, "invalid bucket: "+q.Get("bucket"))
		return
	}
	if window > statsMaxWindow {
		window = statsMaxWindow
	}
	if window < statsMinBucket {
		window = statsMinBucket
	}
	if bucket < statsMinBucket {
		bucket = statsMinBucket
	}
	if bucket > window {
		bucket = window
	}

	rows := g.statsTailRows(g.runtimeConfig().TelemetryDir)

	// Fixed-width buckets, zero-filled across the window, oldest
	// first; the newest bucket is the (partial) one containing now.
	now := statsNow().Unix()
	bkt := int64(bucket)
	until := now/bkt*bkt + bkt
	n := int64(window) / bkt
	if n*bkt < int64(window) {
		n++
	}
	// Buckets are whole, so a window that is not a multiple of the
	// bucket ceils up to the next whole bucket. Report the span the
	// buckets actually cover (so window_seconds stays consistent with
	// the returned buckets: window=90&bucket=60 reports 120).
	window = int(n * bkt)
	since := until - n*bkt
	buckets := make([]statsBucket, n)
	durs := make([][]int64, n)
	for i := range buckets {
		buckets[i].TS = since + int64(i)*bkt
	}
	for _, event := range rows {
		ts := event.CompletedAt.Unix()
		if ts < since || ts >= until {
			continue
		}
		i := (ts - since) / bkt
		buckets[i].Requests++
		if telemetryEventIsError(event) {
			buckets[i].Errors++
		}
		if event.DurationMS > 0 {
			durs[i] = append(durs[i], event.DurationMS)
		}
	}
	for i := range buckets {
		sort.Slice(durs[i], func(a, b int) bool { return durs[i][a] < durs[i][b] })
		buckets[i].P50Ms = statsPercentile(durs[i], 50)
		buckets[i].P95Ms = statsPercentile(durs[i], 95)
	}

	fallback := map[string]bool{}
	for _, cl := range g.snapshotClients() {
		_, fallback[cl.cfg.Name] = g.policyAwareFallbackCooldown(cl.cfg)
	}

	tail := rows
	if len(tail) > statsRecentN {
		tail = tail[len(tail)-statsRecentN:]
	}
	recent := make([]statsRecentRow, 0, len(tail))
	for _, event := range tail {
		recent = append(recent, statsRecentRow{
			TS:             float64(event.CompletedAt.UnixNano()) / 1e9,
			Client:         event.Client,
			Route:          event.ConfiguredRoute,
			RouteEffective: event.EffectiveProvider,
			RequestedModel: event.RequestedModel,
			UpstreamModel:  event.ServedModel,
			Status:         telemetryEventStatus(event),
			DurationMs:     event.DurationMS,
			Subagent:       event.Subagent,
		})
	}

	writeJSON(w, 200, map[string]any{
		"window_seconds":  window,
		"bucket_seconds":  bucket,
		"buckets":         buckets,
		"fallback_active": fallback,
		"recent":          recent,
	})
}

// statsIntParam parses a positive-integer query param. Empty means the
// default; garbage (non-numeric, zero, negative) is a 400 at the
// caller. Range clamping is the caller's job.
func statsIntParam(v string, def int) (int, bool) {
	if v == "" {
		return def, true
	}
	x, err := strconv.Atoi(v)
	if err != nil || x <= 0 {
		return 0, false
	}
	return x, true
}

// statsTailRows returns the indexed telemetry tail, reusing the cached
// snapshot when it is younger than statsCacheTTL so popup polling stays
// cheap. A missing or unreadable store yields an empty (valid) slice.
func (g *Gateway) statsTailRows(path string) []telemetry.EventV1 {
	statsMu.Lock()
	defer statsMu.Unlock()
	now := statsNow()
	if statsCachePath == path && now.Sub(statsCacheAt) < statsCacheTTL {
		return statsCacheRows
	}
	var rows []telemetry.EventV1
	if statsTailRead != nil {
		var err error
		rows, err = statsTailRead(path, statsTailMaxBytes)
		if err != nil {
			rows = nil
		}
	} else {
		snapshot := g.getAnalyticsIndex().Snapshot(
			path,
			now,
			now.Add(-time.Duration(statsMaxWindow)*time.Second).Unix(),
		)
		rows = snapshot.Events
	}
	statsCachePath = path
	statsCacheRows = rows
	statsCacheAt = now
	return rows
}

func telemetryEventStatus(event telemetry.EventV1) int {
	if event.Status == nil {
		return 0
	}
	return *event.Status
}

func telemetryEventIsError(event telemetry.EventV1) bool {
	return event.Status != nil && *event.Status >= 400
}

// statsPercentile returns the nearest-rank q-th percentile of
// ascending sorted samples, or 0 when there are none. Mirrors the
// percentile helper.
func statsPercentile(sorted []int64, q int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted)*q + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}
