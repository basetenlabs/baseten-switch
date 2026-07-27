package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

// resetStatsForTest clears the package-level stats cache and restores
// the statsNow / statsTailRead seams when the test ends, so tests
// never see another test's parsed tail or frozen clock.
func resetStatsForTest(t *testing.T) {
	t.Helper()
	clear := func() {
		statsMu.Lock()
		statsCachePath = ""
		statsCacheRows = nil
		statsCacheAt = time.Time{}
		statsMu.Unlock()
	}
	clear()
	origNow := statsNow
	origRead := statsTailRead
	t.Cleanup(func() {
		statsNow = origNow
		statsTailRead = origRead
		clear()
	})
}

// statsResponse mirrors the /v1/admin/stats contract for decoding in
// tests (the menubar control-surface contract glance strip).
type statsResponse struct {
	WindowSeconds  int              `json:"window_seconds"`
	BucketSeconds  int              `json:"bucket_seconds"`
	Buckets        []statsBucket    `json:"buckets"`
	FallbackActive map[string]bool  `json:"fallback_active"`
	Recent         []statsRecentRow `json:"recent"`
}

type statsTestRow struct {
	TS             float64
	Event          string
	Client         string
	Route          string
	RouteEffective string
	RequestedModel string
	UpstreamModel  string
	Status         int
	DurationMs     int64
	Subagent       bool
}

func appendStatsRow(t *testing.T, dir string, row statsTestRow) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	completed := time.Unix(0, int64(row.TS*1e9)).UTC()
	if completed.IsZero() {
		completed = time.Now().UTC()
	}
	eventName := row.Event
	if eventName == "" {
		eventName = telemetry.EventRequest
	}
	status := row.Status
	event := telemetry.EventV1{
		SchemaVersion:        telemetry.SchemaVersionV1,
		Event:                eventName,
		EventID:              fmt.Sprintf("%032x", completed.UnixNano()),
		StartedAt:            completed.Add(-time.Millisecond),
		CompletedAt:          completed,
		Client:               row.Client,
		ConfiguredRoute:      row.Route,
		EffectiveProvider:    row.RouteEffective,
		RequestedModel:       row.RequestedModel,
		RequestedModelFamily: "other",
		ModelFamilyRevision:  "test-v1",
		ServedModel:          row.UpstreamModel,
		Status:               &status,
		DurationMS:           row.DurationMs,
		TerminationReason:    telemetry.TerminationCompleted,
		StrippedToolTypes:    []string{},
		Subagent:             row.Subagent,
	}
	if event.EffectiveProvider == "" {
		event.EffectiveProvider = event.ConfiguredRoute
	}
	path := filepath.Join(
		dir,
		fmt.Sprintf("requests-%s-001.jsonl", completed.Format("2006-01")),
	)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(event); err != nil {
		t.Fatal(err)
	}
}

// statsGateway returns a minimal Gateway whose adminStats handler can
// be invoked directly (the handler touches only cfg.TelemetryDir,
// snapshotClients, and fallbackActive, all safe on a zero value).
func statsGateway(telLog string) *Gateway {
	return &Gateway{cfg: Config{TelemetryDir: telLog}}
}

func getStats(t *testing.T, g *Gateway, query string) (*httptest.ResponseRecorder, statsResponse) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/admin/stats"+query, nil)
	rec := httptest.NewRecorder()
	g.adminStats(rec, req)
	var out statsResponse
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode stats: %v\nbody: %s", err, rec.Body.String())
		}
	}
	return rec, out
}

// TestAdminStatsContract exercises the full contract shape over a
// synthetic telemetry file through a running gateway (covering route
// registration): bucket boundaries, zero-fill, percentiles, error
// counting, fallback_active, and the recent projection.
func TestAdminStatsContract(t *testing.T) {
	resetStatsForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)

	// Frozen clock on a bucket boundary: with bucket=60 the newest
	// bucket is [1783998000, 1783998060) and window=180 spans
	// [1783997880, 1783998060). Swapped before the gateway's serving
	// goroutines start so the handler observes the frozen clock.
	now := time.Unix(1783998000, 0)
	statsNow = func() time.Time { return now }
	const since = int64(1783997880)

	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	rows := []statsTestRow{
		// Outside the window: excluded from buckets, still in recent.
		{TS: float64(since - 1), Client: "claude-code", Route: "anthropic", Status: 200, DurationMs: 50},
		// Bucket 0: three requests, one error, durations 100/300/200.
		{TS: float64(since), Client: "claude-code", Route: "anthropic", RequestedModel: "claude-fable-5", UpstreamModel: "claude-fable-5", Status: 200, DurationMs: 100},
		{TS: float64(since + 20), Client: "claude-code", Route: "anthropic", Status: 200, DurationMs: 300},
		// ts truncates to since+59: last instant of bucket 0.
		{TS: float64(since+59) + 0.9, Client: "claude-code", Route: "anthropic", Status: 500, DurationMs: 200},
		// Bucket 1: exactly on the boundary; a status-0 row (no upstream
		// response) with no duration, via the subagent fallback path.
		// Status 0 is deliberately not an HTTP error, so it
		// counts as a request but not an error.
		{TS: float64(since + 60), Client: "claude-code", Route: "baseten", RouteEffective: "anthropic", RequestedModel: "claude-haiku-4-5", UpstreamModel: "zai-org/GLM-5.2", Status: 0, Subagent: true},
	}
	for _, row := range rows {
		appendStatsRow(t, cfg.TelemetryDir, row)
	}
	// Non-request events must not count anywhere.
	appendStatsRow(t, cfg.TelemetryDir, statsTestRow{TS: float64(since + 30), Event: "boot"})

	resp, err := http.Get(adminURL(g, "/v1/admin/stats?window=180&bucket=60"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}

	if out.WindowSeconds != 180 || out.BucketSeconds != 60 {
		t.Fatalf("window/bucket = %d/%d, want 180/60", out.WindowSeconds, out.BucketSeconds)
	}
	want := []statsBucket{
		{TS: since, Requests: 3, Errors: 1, P50Ms: 200, P95Ms: 300},
		// Status-0 event: a request, but not an HTTP error.
		{TS: since + 60, Requests: 1, Errors: 0, P50Ms: 0, P95Ms: 0},
		{TS: since + 120, Requests: 0, Errors: 0, P50Ms: 0, P95Ms: 0},
	}
	if len(out.Buckets) != len(want) {
		t.Fatalf("buckets = %d, want %d: %+v", len(out.Buckets), len(want), out.Buckets)
	}
	for i, b := range out.Buckets {
		if b != want[i] {
			t.Errorf("bucket[%d] = %+v, want %+v", i, b, want[i])
		}
	}

	if active, ok := out.FallbackActive["claude-code"]; !ok || active {
		t.Errorf("fallback_active[claude-code] = %v, %v; want false, true", active, ok)
	}

	if len(out.Recent) != 5 {
		t.Fatalf("recent = %d rows, want 5: %+v", len(out.Recent), out.Recent)
	}
	first := out.Recent[0]
	if first.TS != float64(since-1) || first.Status != 200 || first.Subagent {
		t.Errorf("recent[0] (oldest) = %+v", first)
	}
	last := out.Recent[len(out.Recent)-1]
	if last.Client != "claude-code" || last.Route != "baseten" ||
		last.RouteEffective != "anthropic" || last.RequestedModel != "claude-haiku-4-5" ||
		last.UpstreamModel != "zai-org/GLM-5.2" || last.Status != 0 ||
		last.DurationMs != 0 || !last.Subagent {
		t.Errorf("recent[last] = %+v", last)
	}
	mid := out.Recent[1]
	if mid.Subagent || mid.RouteEffective != "anthropic" || mid.RequestedModel != "claude-fable-5" {
		t.Errorf("recent[1] = %+v", mid)
	}
}

func TestAdminStatsRecentCapsAtTwenty(t *testing.T) {
	resetStatsForTest(t)
	telLog := filepath.Join(t.TempDir(), "tel.log")
	now := time.Unix(1783998000, 0)
	statsNow = func() time.Time { return now }
	for i := 0; i < 30; i++ {
		appendStatsRow(t, telLog, statsTestRow{
			TS: float64(now.Unix()-40) + float64(i), Client: "claude-code", Status: 200 + i, DurationMs: 10,
		})
	}
	rec, out := getStats(t, statsGateway(telLog), "")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(out.Recent) != 20 {
		t.Fatalf("recent = %d rows, want 20", len(out.Recent))
	}
	// Oldest first: the first surviving row is the 11th appended.
	if out.Recent[0].Status != 210 || out.Recent[19].Status != 229 {
		t.Errorf("recent window = status %d..%d, want 210..229", out.Recent[0].Status, out.Recent[19].Status)
	}
}

// TestAdminStatsWindowRoundsUpToWholeBuckets: a window that is not a
// multiple of the bucket ceils up to the next whole bucket, and
// window_seconds reports the span the buckets actually cover (not the
// raw requested value). window=90, bucket=60 covers two 60s buckets, so
// window_seconds is 120.
func TestAdminStatsWindowRoundsUpToWholeBuckets(t *testing.T) {
	resetStatsForTest(t)
	telLog := filepath.Join(t.TempDir(), "tel.log")
	// Frozen clock on a bucket boundary: until = 1783998060, so the two
	// buckets span [1783997940, 1783998060).
	now := time.Unix(1783998000, 0)
	statsNow = func() time.Time { return now }

	rec, out := getStats(t, statsGateway(telLog), "?window=90&bucket=60")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if out.WindowSeconds != 120 {
		t.Errorf("window_seconds = %d, want 120 (90 ceils to two 60s buckets)", out.WindowSeconds)
	}
	if out.BucketSeconds != 60 {
		t.Errorf("bucket_seconds = %d, want 60", out.BucketSeconds)
	}
	if len(out.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2: %+v", len(out.Buckets), out.Buckets)
	}
	if span := out.Buckets[1].TS - out.Buckets[0].TS; span != 60 {
		t.Errorf("bucket span = %d, want 60", span)
	}
	// The covered span (count * bucket) equals the reported window.
	if covered := int(len(out.Buckets)) * out.BucketSeconds; covered != out.WindowSeconds {
		t.Errorf("covered span = %d, window_seconds = %d; want equal", covered, out.WindowSeconds)
	}
	if out.Buckets[0].TS != 1783997940 {
		t.Errorf("oldest bucket TS = %d, want 1783997940", out.Buckets[0].TS)
	}
}

func TestAdminStatsParamValidation(t *testing.T) {
	resetStatsForTest(t)
	telLog := filepath.Join(t.TempDir(), "tel.log")
	g := statsGateway(telLog)

	for _, q := range []string{
		"?window=abc", "?window=0", "?window=-5", "?window=1.5",
		"?bucket=abc", "?bucket=0", "?bucket=-60",
	} {
		rec, _ := getStats(t, g, q)
		if rec.Code != 400 {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}

	// Defaults.
	rec, out := getStats(t, g, "")
	if rec.Code != 200 || out.WindowSeconds != 3600 || out.BucketSeconds != 60 {
		t.Errorf("defaults: status %d window %d bucket %d", rec.Code, out.WindowSeconds, out.BucketSeconds)
	}
	if len(out.Buckets) != 60 {
		t.Errorf("default buckets = %d, want 60", len(out.Buckets))
	}

	// Clamps: window to the 21600 max, bucket up to the 10 min and
	// down to the window.
	if _, out := getStats(t, g, "?window=999999"); out.WindowSeconds != 21600 {
		t.Errorf("window clamp = %d, want 21600", out.WindowSeconds)
	}
	if _, out := getStats(t, g, "?bucket=1"); out.BucketSeconds != 10 {
		t.Errorf("bucket min clamp = %d, want 10", out.BucketSeconds)
	}
	if _, out := getStats(t, g, "?window=120&bucket=600"); out.BucketSeconds != 120 || len(out.Buckets) != 1 {
		t.Errorf("bucket>window clamp: bucket %d buckets %d, want 120 and 1", out.BucketSeconds, len(out.Buckets))
	}
}

func TestAdminStatsMethodNotAllowed(t *testing.T) {
	resetStatsForTest(t)
	g := statsGateway(filepath.Join(t.TempDir(), "tel.log"))
	req := httptest.NewRequest("POST", "/v1/admin/stats", nil)
	rec := httptest.NewRecorder()
	g.adminStats(rec, req)
	if rec.Code != 405 {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestAdminStatsEmptyTelemetry: a missing telemetry file yields an
// empty-but-valid payload (zero-filled buckets, empty recent).
func TestAdminStatsEmptyTelemetry(t *testing.T) {
	resetStatsForTest(t)
	g := statsGateway(filepath.Join(t.TempDir(), "does-not-exist.log"))
	rec, out := getStats(t, g, "?window=120&bucket=60")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(out.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(out.Buckets))
	}
	for i, b := range out.Buckets {
		if b.Requests != 0 || b.Errors != 0 || b.P50Ms != 0 || b.P95Ms != 0 {
			t.Errorf("bucket[%d] not zero: %+v", i, b)
		}
	}
	if out.Recent == nil || len(out.Recent) != 0 {
		t.Errorf("recent = %#v, want empty non-null", out.Recent)
	}
	if out.FallbackActive == nil {
		t.Error("fallback_active missing")
	}
}

// TestAdminStatsCacheTTL: two requests within the TTL parse the tail
// once; a request after the TTL parses again.
func TestAdminStatsCacheTTL(t *testing.T) {
	resetStatsForTest(t)
	telLog := filepath.Join(t.TempDir(), "tel.log")
	// Mid-bucket now: with window=bucket=60 the lone bucket is
	// [1783998000, 1783998060) and a row 20s in the past lands in it.
	now := time.Unix(1783998030, 0)
	statsNow = func() time.Time { return now }
	reads := 0
	statsTailRead = func(path string, maxBytes int64) ([]telemetry.EventV1, error) {
		reads++
		return telemetry.ReadEvents(path)
	}
	appendStatsRow(t, telLog, statsTestRow{TS: float64(now.Unix() - 20), Client: "claude-code", Status: 200, DurationMs: 10})
	g := statsGateway(telLog)

	if _, out := getStats(t, g, "?window=60&bucket=60"); out.Buckets[0].Requests != 1 {
		t.Fatalf("first read: %+v", out.Buckets)
	}
	getStats(t, g, "?window=60&bucket=60")
	if reads != 1 {
		t.Fatalf("reads within TTL = %d, want 1", reads)
	}

	// Advance past the TTL: the next request re-reads.
	now = now.Add(statsCacheTTL + time.Second)
	getStats(t, g, "?window=60&bucket=60")
	if reads != 2 {
		t.Fatalf("reads after TTL = %d, want 2", reads)
	}
}
