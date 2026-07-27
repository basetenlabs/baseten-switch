package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/analytics"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

func analyticsGateway(t *testing.T, now time.Time) (*Gateway, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telemetry")
	return &Gateway{
		cfg:            Config{TelemetryDir: path},
		analyticsIndex: analytics.NewIndex(analytics.IndexOptions{}),
	}, path
}

func appendAnalyticsEvent(t *testing.T, dir string, event telemetry.EventV1) {
	t.Helper()
	writer, err := telemetry.NewWriter(telemetry.WriterOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(event); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func analyticsAdminEvent(
	index int,
	completedAt time.Time,
	provider string,
	requestedModel string,
	servedModel string,
	actualNanoUSD int64,
) telemetry.EventV1 {
	event := gatewayTelemetryEvent(index)
	event.StartedAt = completedAt.Add(-1200 * time.Millisecond)
	event.CompletedAt = completedAt
	event.Client = "claude-code"
	event.ConfiguredRoute = provider
	event.EffectiveProvider = provider
	event.RequestedModel = requestedModel
	event.RequestedModelFamily = "opus"
	event.ModelFamilyRevision = "test-v1"
	event.ServedModel = servedModel
	status := 200
	event.Status = &status
	event.DurationMS = 1200
	ttft := int64(200)
	event.TTFTMS = &ttft
	input := int64(1_000_000)
	output := int64(100_000)
	cacheRead := int64(0)
	cacheWrite := int64(0)
	cacheWrite1h := int64(0)
	event.UsageComplete = true
	event.Usage = telemetry.UsageV1{
		InputTokens:             &input,
		OutputTokens:            &output,
		CacheReadInputTokens:    &cacheRead,
		CacheWrite5mInputTokens: &cacheWrite,
		CacheWrite1hInputTokens: &cacheWrite1h,
	}
	event.ActualCost = analyticsAdminCost(actualNanoUSD, event.StartedAt)
	return event
}

func analyticsAdminCost(nanoUSD int64, capturedAt time.Time) telemetry.CostSnapshotV1 {
	revision := "test-revision"
	rate := int64(1)
	return telemetry.CostSnapshotV1{
		Priced:     true,
		NanoUSD:    &nanoUSD,
		Source:     "test",
		Revision:   &revision,
		CapturedAt: &capturedAt,
		RatesNanoUSDPerToken: &telemetry.TokenRatesV1{
			Input:             &rate,
			Output:            &rate,
			CacheReadInput:    &rate,
			CacheWrite5mInput: &rate,
			CacheWrite1hInput: &rate,
		},
		RateProvenance: analyticsAdminRateProvenance(capturedAt),
	}
}

func analyticsAdminRateProvenance(
	capturedAt time.Time,
) map[string]telemetry.RateProvenanceV1 {
	value := telemetry.RateProvenanceV1{
		Source:     "test",
		LoadedFrom: "live",
		Revision:   "test-revision",
		CapturedAt: capturedAt,
	}
	return map[string]telemetry.RateProvenanceV1{
		"input":          value,
		"output":         value,
		"cache_read":     value,
		"cache_write_5m": value,
		"cache_write_1h": value,
	}
}

func requestAnalytics(t *testing.T, g *Gateway, target, method string) (*httptest.ResponseRecorder, analytics.Response) {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	recorder := httptest.NewRecorder()
	g.adminAnalytics(recorder, request)
	var response analytics.Response
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v\n%s", err, recorder.Body.String())
		}
	}
	return recorder, response
}

func TestAdminAnalyticsContractAndDefaultWindow(t *testing.T) {
	now := time.Unix(2_000_000_030, 0)
	oldNow := analyticsNow
	analyticsNow = func() time.Time { return now }
	t.Cleanup(func() { analyticsNow = oldNow })

	g, path := analyticsGateway(t, now)
	event := analyticsAdminEvent(
		1,
		now.Add(-60*time.Second),
		"baseten",
		"claude-opus-4-8",
		"zai-org/GLM-5.2",
		2_000_000_000,
	)
	counterfactual := analyticsAdminCost(12_000_000_000, event.StartedAt)
	event.NativeCounterfactualCost = &counterfactual
	appendAnalyticsEvent(t, path, event)

	recorder, response := requestAnalytics(t, g, "/v1/admin/analytics", http.MethodGet)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("content-type = %q", contentType)
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("cache-control = %q", cache)
	}
	if response.GeneratedAt != now.Unix() ||
		response.Window.Until != now.Unix() ||
		response.Window.Since != now.Add(-7*24*time.Hour).Unix() {
		t.Fatalf("time contract = generated %d window %+v", response.GeneratedAt, response.Window)
	}
	if response.Coverage.RequestRows != 1 ||
		response.Coverage.SavingsEligibleRows != 1 ||
		len(response.Cost.Providers) != 1 ||
		len(response.Performance.Models) != 1 {
		t.Fatalf("response = %+v", response)
	}
	if response.Cost.Summary.ActualBasetenCostUSD != 2 ||
		response.Cost.Summary.EstimatedNativeCostForBasetenUSD != 12 ||
		response.Cost.Summary.SavedUSD != 10 {
		t.Fatalf("cost summary = %+v", response.Cost.Summary)
	}

	body := recorder.Body.String()
	for _, forbidden := range []string{"prompt", "completion", "authorization", "api_key"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("analytics response contains forbidden body-content term %q: %s", forbidden, body)
		}
	}
}

func TestAdminAnalyticsQueryValidationAndMethod(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	oldNow := analyticsNow
	analyticsNow = func() time.Time { return now }
	t.Cleanup(func() { analyticsNow = oldNow })
	g, _ := analyticsGateway(t, now)

	for _, target := range []string{
		"/v1/admin/analytics?since=nope",
		"/v1/admin/analytics?until=nope",
		"/v1/admin/analytics?since=20&until=20",
		"/v1/admin/analytics?since=21&until=20",
		"/v1/admin/analytics?since=1&until=2592002",
	} {
		recorder, _ := requestAnalytics(t, g, target, http.MethodGet)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d body=%s", target, recorder.Code, recorder.Body.String())
		}
	}

	recorder, _ := requestAnalytics(t, g, "/v1/admin/analytics", http.MethodPost)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", recorder.Code)
	}
}

func TestAdminAnalyticsCustomSupportedWindows(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	oldNow := analyticsNow
	analyticsNow = func() time.Time { return now }
	t.Cleanup(func() { analyticsNow = oldNow })
	g, _ := analyticsGateway(t, now)

	for _, duration := range []time.Duration{
		24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour,
	} {
		since := now.Add(-duration).Unix()
		recorder, response := requestAnalytics(
			t,
			g,
			"/v1/admin/analytics?since="+strconv.FormatInt(since, 10)+
				"&until="+strconv.FormatInt(now.Unix(), 10),
			http.MethodGet,
		)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s window: status = %d body=%s", duration, recorder.Code, recorder.Body.String())
		}
		if response.Window.Until-response.Window.Since != int64(duration/time.Second) {
			t.Errorf("%s window returned %+v", duration, response.Window)
		}
	}
}

func TestStatsAndAnalyticsShareIncrementalIndex(t *testing.T) {
	resetStatsForTest(t)
	now := time.Unix(2_000_000_030, 0)
	oldAnalyticsNow := analyticsNow
	analyticsNow = func() time.Time { return now }
	statsNow = func() time.Time { return now }
	t.Cleanup(func() { analyticsNow = oldAnalyticsNow })

	g, path := analyticsGateway(t, now)
	shared := g.getAnalyticsIndex()
	appendAnalyticsEvent(t, path, analyticsAdminEvent(
		2,
		now.Add(-10*time.Second),
		"anthropic",
		"claude-opus-4-8",
		"claude-opus-4-8",
		1_000_000_000,
	))
	_, stats := getStats(t, g, "?window=60&bucket=60")
	if stats.Buckets[0].Requests != 1 {
		t.Fatalf("stats requests = %+v", stats.Buckets)
	}
	if g.analyticsIndex != shared {
		t.Fatal("stats replaced the gateway analytics index")
	}

	appendAnalyticsEvent(t, path, analyticsAdminEvent(
		3,
		now.Add(-5*time.Second),
		"baseten",
		"claude-opus-4-8",
		"zai-org/GLM-5.2",
		1_000_000_000,
	))
	recorder, response := requestAnalytics(
		t,
		g,
		"/v1/admin/analytics?since="+strconv.FormatInt(now.Unix()-60, 10)+
			"&until="+strconv.FormatInt(now.Unix(), 10),
		http.MethodGet,
	)
	if recorder.Code != http.StatusOK || response.Coverage.RequestRows != 2 {
		t.Fatalf("analytics after stats: status=%d coverage=%+v",
			recorder.Code, response.Coverage)
	}
	if g.analyticsIndex != shared {
		t.Fatal("analytics replaced the index used by stats")
	}
}

func TestAdminAnalyticsCollectionDisabledKeepsRetainedHistory(t *testing.T) {
	now := time.Unix(2_000_000_030, 0)
	oldNow := analyticsNow
	analyticsNow = func() time.Time { return now }
	t.Cleanup(func() { analyticsNow = oldNow })

	g, path := analyticsGateway(t, now)
	disabled := false
	g.cfg.TelemetryEnabled = &disabled
	appendAnalyticsEvent(t, path, analyticsAdminEvent(
		4,
		now.Add(-time.Minute),
		"anthropic",
		"claude-opus-4-8",
		"claude-opus-4-8",
		1_000_000_000,
	))

	recorder, response := requestAnalytics(
		t,
		g,
		"/v1/admin/analytics",
		http.MethodGet,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if response.Coverage.CollectionEnabled {
		t.Fatal("collection_enabled = true")
	}
	if response.Coverage.RequestRows != 1 ||
		response.Cost.Summary.ActualClaudeCostUSD != 1 {
		t.Fatalf("retained history missing while disabled: %+v", response)
	}
}

func TestAdminAnalyticsCachesExactWindowAndInvalidatesOnChanges(t *testing.T) {
	currentNow := time.Unix(2_000_000_030, 0)
	oldNow := analyticsNow
	oldBuild := analyticsBuild
	analyticsNow = func() time.Time { return currentNow }
	builds := 0
	analyticsBuild = func(
		events []telemetry.EventV1,
		window analytics.Window,
		generatedAt int64,
		retained analytics.Snapshot,
		collectionEnabled bool,
		catalog *pricing.Snapshot,
	) analytics.Response {
		builds++
		return analytics.Build(
			events,
			window,
			generatedAt,
			retained,
			collectionEnabled,
			catalog,
		)
	}
	t.Cleanup(func() {
		analyticsNow = oldNow
		analyticsBuild = oldBuild
	})

	g, path := analyticsGateway(t, currentNow)
	appendAnalyticsEvent(t, path, analyticsAdminEvent(
		40,
		currentNow.Add(-time.Minute),
		"anthropic",
		"claude-opus-4-8",
		"claude-opus-4-8",
		1_000_000_000,
	))
	until := currentNow.Unix()
	target := "/v1/admin/analytics?since=" +
		strconv.FormatInt(until-3600, 10) +
		"&until=" + strconv.FormatInt(until, 10)

	_, first := requestAnalytics(t, g, target, http.MethodGet)
	currentNow = currentNow.Add(time.Second)
	_, cached := requestAnalytics(t, g, target, http.MethodGet)
	if builds != 1 {
		t.Fatalf("unchanged exact window builds = %d, want 1", builds)
	}
	if cached.GeneratedAt != currentNow.Unix() ||
		cached.Coverage.RequestRows != first.Coverage.RequestRows {
		t.Fatalf("cached response = %+v, first = %+v", cached, first)
	}

	otherTarget := "/v1/admin/analytics?since=" +
		strconv.FormatInt(until-1800, 10) +
		"&until=" + strconv.FormatInt(until, 10)
	requestAnalytics(t, g, otherTarget, http.MethodGet)
	if builds != 2 {
		t.Fatalf("different exact window builds = %d, want 2", builds)
	}

	disabled := false
	g.cfg.TelemetryEnabled = &disabled
	_, paused := requestAnalytics(t, g, target, http.MethodGet)
	if builds != 3 || paused.Coverage.CollectionEnabled {
		t.Fatalf("collection-state invalidation builds=%d response=%+v",
			builds, paused.Coverage)
	}

	enabled := true
	g.cfg.TelemetryEnabled = &enabled
	requestAnalytics(t, g, target, http.MethodGet)
	if builds != 3 {
		t.Fatalf("restored collection state missed cached response: %d", builds)
	}

	appendAnalyticsEvent(t, path, analyticsAdminEvent(
		41,
		time.Unix(until-30, 0),
		"baseten",
		"claude-opus-4-8",
		"zai-org/GLM-5.2",
		1_000_000_000,
	))
	_, appended := requestAnalytics(t, g, target, http.MethodGet)
	if builds != 4 || appended.Coverage.RequestRows != 2 {
		t.Fatalf("append invalidation builds=%d response=%+v",
			builds, appended.Coverage)
	}
}

func TestAnalyticsCacheIncludesCoverageState(t *testing.T) {
	oldBuild := analyticsBuild
	builds := 0
	analyticsBuild = func(
		events []telemetry.EventV1,
		window analytics.Window,
		generatedAt int64,
		retained analytics.Snapshot,
		collectionEnabled bool,
		catalog *pricing.Snapshot,
	) analytics.Response {
		builds++
		return analytics.Build(
			events,
			window,
			generatedAt,
			retained,
			collectionEnabled,
			catalog,
		)
	}
	t.Cleanup(func() { analyticsBuild = oldBuild })

	g := &Gateway{}
	window := analytics.Window{Since: 100, Until: 200}
	retained := analytics.Snapshot{Generation: 7, Complete: true}
	g.analyticsResponse(retained, window, 200, true)
	g.analyticsResponse(retained, window, 201, true)
	retained.Complete = false
	retained.Reason = "age cap"
	response := g.analyticsResponse(retained, window, 202, true)

	if builds != 2 || response.Coverage.Complete ||
		response.Coverage.Reason != "age cap" {
		t.Fatalf("coverage invalidation builds=%d response=%+v",
			builds, response.Coverage)
	}
}

func TestAnalyticsCacheInvalidatesOnDisplayNameRenameWithoutRegrouping(t *testing.T) {
	catalog := pricing.New()
	publish := func(name, revision string) {
		t.Helper()
		if err := catalog.ReplaceProviderAvailability(
			pricing.ProviderBaseten,
			[]pricing.AvailabilityModel{{
				CanonicalModelID: "baseten/inkling-v1",
				DisplayName:      name,
			}},
			"test_model_apis",
			time.Unix(90, 0).UTC(),
			revision,
		); err != nil {
			t.Fatal(err)
		}
	}
	publish("Inkling Preview", "presentation-v1")

	event := analyticsAdminEvent(
		1,
		time.Unix(100, 0).UTC(),
		"baseten",
		"claude-opus-4-8",
		"baseten/inkling-v1",
		1_000_000_000,
	)
	retained := analytics.Snapshot{
		Events:     []telemetry.EventV1{event},
		Generation: 7,
		Complete:   true,
	}
	g := &Gateway{pricing: catalog}
	window := analytics.Window{Since: 90, Until: 110}

	first := g.analyticsResponse(retained, window, 110, true)
	publish("Inkling", "presentation-updated")
	renamed := g.analyticsResponse(retained, window, 111, true)

	if len(first.Cost.Models) != 1 ||
		first.Cost.Models[0].ModelID != "baseten/inkling-v1" ||
		first.Cost.Models[0].DisplayName != "Inkling Preview" {
		t.Fatalf("first model group = %+v", first.Cost.Models)
	}
	if len(renamed.Cost.Models) != 1 ||
		renamed.Cost.Models[0].ModelID != "baseten/inkling-v1" ||
		renamed.Cost.Models[0].DisplayName != "Inkling" ||
		renamed.Cost.Models[0].Requests != first.Cost.Models[0].Requests {
		t.Fatalf("renamed model group = %+v", renamed.Cost.Models)
	}
	if len(g.analyticsCache) != 2 {
		t.Fatalf("cache entries = %d, want one per presentation revision", len(g.analyticsCache))
	}
}

func TestAdminRawTelemetryReadsV1SegmentsAndAppliesTail(t *testing.T) {
	g, path := analyticsGateway(t, time.Now())
	first := analyticsAdminEvent(
		5,
		time.Date(2026, time.July, 31, 23, 59, 0, 0, time.UTC),
		"anthropic",
		"claude-opus-4-8",
		"claude-opus-4-8",
		1_000_000_000,
	)
	second := analyticsAdminEvent(
		6,
		time.Date(2026, time.August, 1, 0, 1, 0, 0, time.UTC),
		"baseten",
		"claude-opus-4-8",
		"zai-org/GLM-5.2",
		2_000_000_000,
	)
	appendAnalyticsEvent(t, path, first)
	appendAnalyticsEvent(t, path, second)
	segments, err := telemetry.DiscoverSegments(path)
	if err != nil || len(segments) != 2 {
		t.Fatalf("segments = %+v, error = %v", segments, err)
	}
	file, err := os.OpenFile(
		segments[len(segments)-1].Path,
		os.O_APPEND|os.O_WRONLY,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{malformed}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/admin/telemetry?tail=1",
		nil,
	)
	recorder := httptest.NewRecorder()
	g.adminTelemetry(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Baseten-Switch-Telemetry-Partial") != "true" {
		t.Fatalf("partial header missing: %v", recorder.Header())
	}
	var events []telemetry.EventV1
	if err := json.Unmarshal(recorder.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != second.EventID {
		t.Fatalf("raw tail = %+v", events)
	}
}

var benchmarkAnalyticsResponse analytics.Response

func benchmarkAnalyticsEvents100K(now time.Time) []telemetry.EventV1 {
	claudeModels := []struct {
		id     string
		family string
	}{
		{id: "claude-fable-4-8", family: "fable"},
		{id: "claude-opus-4-8", family: "opus"},
		{id: "claude-sonnet-4-8", family: "sonnet"},
		{id: "claude-haiku-4-5", family: "haiku"},
	}
	events := make([]telemetry.EventV1, 100_000)
	for index := range events {
		claude := claudeModels[index%len(claudeModels)]
		provider := "anthropic"
		served := claude.id
		if index%2 == 1 {
			provider = "baseten"
			served = "zai-org/GLM-5.2"
			if index%4 == 3 {
				served = "moonshotai/Kimi-K2.7-Code"
			}
		}
		events[index] = analyticsAdminEvent(
			index+1000,
			now.Add(-time.Duration(index)*time.Second),
			provider,
			claude.id,
			served,
			1_000_000,
		)
		events[index].RequestedModelFamily = claude.family
		if provider == "baseten" {
			counterfactual := analyticsAdminCost(
				5_000_000,
				events[index].StartedAt,
			)
			events[index].NativeCounterfactualCost = &counterfactual
		}
	}
	return events
}

func BenchmarkAnalyticsResponseCacheHit100K(b *testing.B) {
	now := time.Unix(2_000_000_000, 0)
	events := benchmarkAnalyticsEvents100K(now)
	retained := analytics.Snapshot{
		Generation: 1,
		Events:     events,
		Complete:   true,
		Earliest:   events[len(events)-1].CompletedAt,
		Latest:     events[0].CompletedAt,
	}
	window := analytics.Window{
		Since: now.Add(-30 * 24 * time.Hour).Unix(),
		Until: now.Unix(),
	}
	g := &Gateway{}
	g.analyticsResponse(retained, window, now.Unix(), true)

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		benchmarkAnalyticsResponse = g.analyticsResponse(
			retained,
			window,
			now.Unix()+int64(index),
			true,
		)
	}
}

func BenchmarkAnalyticsAggregation100K(b *testing.B) {
	now := time.Unix(2_000_000_000, 0)
	events := benchmarkAnalyticsEvents100K(now)
	retained := analytics.Snapshot{
		Generation: 1,
		Events:     events,
		Complete:   true,
		Earliest:   events[len(events)-1].CompletedAt,
		Latest:     events[0].CompletedAt,
	}
	window := analytics.Window{
		Since: now.Add(-30 * 24 * time.Hour).Unix(),
		Until: now.Unix(),
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkAnalyticsResponse = analytics.Build(
			events,
			window,
			now.Unix(),
			retained,
			true,
			nil,
		)
	}
}

func BenchmarkAnalyticsColdIndexAndAggregation100K(b *testing.B) {
	now := time.Unix(2_000_000_000, 0)
	events := benchmarkAnalyticsEvents100K(now)
	dir := b.TempDir()
	writer, err := telemetry.NewWriter(telemetry.WriterOptions{
		Dir:             dir,
		MaxSegmentBytes: 16 << 20,
	})
	if err != nil {
		b.Fatal(err)
	}
	for _, event := range events {
		if err := writer.Write(event); err != nil {
			_ = writer.Close()
			b.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	segments, err := telemetry.DiscoverSegments(dir)
	if err != nil {
		b.Fatal(err)
	}
	var fixtureBytes int64
	for _, segment := range segments {
		fixtureBytes += segment.Size
	}
	window := analytics.Window{
		Since: now.Add(-30 * 24 * time.Hour).Unix(),
		Until: now.Unix(),
	}
	b.ReportAllocs()
	b.ResetTimer()

	var retained analytics.Snapshot
	for range b.N {
		index := analytics.NewIndex(analytics.IndexOptions{
			BootstrapMaxBytes: fixtureBytes + 1,
		})
		retained = index.Snapshot(dir, now, window.Since)
		benchmarkAnalyticsResponse = analytics.Build(
			retained.Events,
			window,
			now.Unix(),
			retained,
			true,
			nil,
		)
	}
	b.StopTimer()
	b.ReportMetric(float64(fixtureBytes)/(1<<20), "fixture_MiB")
	b.ReportMetric(float64(len(segments)), "segments")
	b.ReportMetric(float64(len(retained.Events)), "indexed_rows")
}

func BenchmarkAnalyticsColdProductionCaps100K(b *testing.B) {
	now := time.Unix(2_000_000_000, 0)
	events := benchmarkAnalyticsEvents100K(now)
	dir := b.TempDir()
	writer, err := telemetry.NewWriter(telemetry.WriterOptions{
		Dir:             dir,
		MaxSegmentBytes: 16 << 20,
	})
	if err != nil {
		b.Fatal(err)
	}
	for _, event := range events {
		if err := writer.Write(event); err != nil {
			_ = writer.Close()
			b.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	segments, err := telemetry.DiscoverSegments(dir)
	if err != nil {
		b.Fatal(err)
	}
	var fixtureBytes int64
	for _, segment := range segments {
		fixtureBytes += segment.Size
	}
	window := analytics.Window{
		Since: now.Add(-30 * 24 * time.Hour).Unix(),
		Until: now.Unix(),
	}
	b.ReportAllocs()
	b.ResetTimer()

	var retained analytics.Snapshot
	for range b.N {
		index := analytics.NewIndex(analytics.IndexOptions{})
		retained = index.Snapshot(dir, now, window.Since)
		benchmarkAnalyticsResponse = analytics.Build(
			retained.Events,
			window,
			now.Unix(),
			retained,
			true,
			nil,
		)
	}
	b.StopTimer()
	b.ReportMetric(float64(fixtureBytes)/(1<<20), "fixture_MiB")
	b.ReportMetric(float64(len(segments)), "segments")
	b.ReportMetric(float64(len(retained.Events)), "indexed_rows")
}

func BenchmarkAnalyticsWarmSnapshotAndCacheHit100K(b *testing.B) {
	now := time.Unix(2_000_000_000, 0)
	events := benchmarkAnalyticsEvents100K(now)
	dir := b.TempDir()
	writer, err := telemetry.NewWriter(telemetry.WriterOptions{
		Dir:             dir,
		MaxSegmentBytes: 16 << 20,
	})
	if err != nil {
		b.Fatal(err)
	}
	for _, event := range events {
		if err := writer.Write(event); err != nil {
			_ = writer.Close()
			b.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	window := analytics.Window{
		Since: now.Add(-30 * 24 * time.Hour).Unix(),
		Until: now.Unix(),
	}
	index := analytics.NewIndex(analytics.IndexOptions{})
	retained := index.Snapshot(dir, now, window.Since)
	g := &Gateway{}
	g.analyticsResponse(retained, window, now.Unix(), true)

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := range b.N {
		retained = index.Snapshot(dir, now, window.Since)
		benchmarkAnalyticsResponse = g.analyticsResponse(
			retained,
			window,
			now.Unix()+int64(iteration),
			true,
		)
	}
	b.StopTimer()
	b.ReportMetric(float64(len(retained.Events)), "indexed_rows")
}
