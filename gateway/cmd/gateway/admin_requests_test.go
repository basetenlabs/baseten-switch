package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/analytics"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

func TestAdminRequestsContractDefaultLimitAndRouteRegistration(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	setAdminRequestsNow(t, now)
	gateway, dir := newAdminRequestsGateway(t, analytics.IndexOptions{})

	events := make([]telemetry.EventV1, adminRequestsDefaultLimit+1)
	for index := range events {
		events[index] = adminRequestsEvent(
			index+1,
			now.Add(time.Duration(index-adminRequestsDefaultLimit)*time.Second),
		)
	}
	writeAdminRequestEvents(t, dir, events...)

	mux := http.NewServeMux()
	gateway.registerAdmin(mux)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/v1/admin/requests", nil),
	)
	response := decodeAdminRequestsResponse(t, recorder)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q", recorder.Header().Get("Cache-Control"))
	}
	if len(response.Items) != adminRequestsDefaultLimit {
		t.Fatalf("items = %d, want %d", len(response.Items), adminRequestsDefaultLimit)
	}
	if response.Items[0].EventID != formatTelemetryEventID(adminRequestsDefaultLimit+1) ||
		response.Items[len(response.Items)-1].EventID != formatTelemetryEventID(2) {
		t.Fatalf(
			"newest-first IDs = %s..%s",
			response.Items[0].EventID,
			response.Items[len(response.Items)-1].EventID,
		)
	}
	if !response.HasMore || response.NextCursor == nil || *response.NextCursor == "" {
		t.Fatalf("pagination = has_more %v cursor %v", response.HasMore, response.NextCursor)
	}
	if !response.Coverage.Complete || response.Coverage.Reason != "" {
		t.Fatalf("coverage = %+v", response.Coverage)
	}
}

func TestAdminRequestsPaginationIsStableWhenNewRowsArrive(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	setAdminRequestsNow(t, now)
	gateway, dir := newAdminRequestsGateway(t, analytics.IndexOptions{})
	for index := 1; index <= 5; index++ {
		writeAdminRequestEvents(
			t,
			dir,
			adminRequestsEvent(index, now.Add(time.Duration(index-10)*time.Second)),
		)
	}

	first := getAdminRequests(t, gateway, "/v1/admin/requests?limit=2")
	assertAdminRequestIDs(t, first, 5, 4)
	if !first.HasMore || first.NextCursor == nil {
		t.Fatalf("first pagination = %+v", first)
	}

	writeAdminRequestEvents(t, dir, adminRequestsEvent(6, now))
	second := getAdminRequests(
		t,
		gateway,
		"/v1/admin/requests?limit=2&cursor="+url.QueryEscape(*first.NextCursor),
	)
	assertAdminRequestIDs(t, second, 3, 2)
	if !second.HasMore || second.NextCursor == nil {
		t.Fatalf("second pagination = %+v", second)
	}

	third := getAdminRequests(
		t,
		gateway,
		"/v1/admin/requests?limit=2&cursor="+url.QueryEscape(*second.NextCursor),
	)
	assertAdminRequestIDs(t, third, 1)
	if third.HasMore || third.NextCursor != nil {
		t.Fatalf("final pagination = %+v", third)
	}
}

func TestAdminRequestsStableTieBreaksOnEventID(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	setAdminRequestsNow(t, now)
	gateway, dir := newAdminRequestsGateway(t, analytics.IndexOptions{})
	writeAdminRequestEvents(
		t,
		dir,
		adminRequestsEvent(1, now.Add(-time.Minute)),
		adminRequestsEvent(3, now.Add(-time.Minute)),
		adminRequestsEvent(2, now.Add(-time.Minute)),
	)

	first := getAdminRequests(t, gateway, "/v1/admin/requests?limit=2")
	assertAdminRequestIDs(t, first, 3, 2)
	second := getAdminRequests(
		t,
		gateway,
		"/v1/admin/requests?limit=2&cursor="+url.QueryEscape(*first.NextCursor),
	)
	assertAdminRequestIDs(t, second, 1)
}

func TestAdminRequestsProjectsFallbackAndNullableFields(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	setAdminRequestsNow(t, now)
	gateway, dir := newAdminRequestsGateway(t, analytics.IndexOptions{})
	event := adminRequestsEvent(1, now.Add(-time.Second))
	event.Client = "claude-code"
	event.ConfiguredRoute = "baseten"
	event.EffectiveProvider = "anthropic"
	event.RequestedModel = "claude-opus-4-8"
	event.RequestedModelFamily = "opus"
	event.ServedModel = "claude-opus-4-8"
	event.Status = nil
	event.TTFTMS = nil
	event.TerminationReason = telemetry.TerminationGatewayError
	event.DurationMS = 321
	event.Subagent = true
	trigger := "image_input_unsupported"
	event.Fallback = telemetry.FallbackV1{
		Attempted: true,
		Count:     1,
		Trigger:   &trigger,
	}
	primaryStatus := http.StatusBadRequest
	event.Primary = &telemetry.PrimaryV1{
		Provider:  "baseten",
		Model:     "zai-org/GLM-5.2",
		Attempted: true,
		Outcome:   telemetry.PrimaryOutcomeImageInputUnsupported,
		Status:    &primaryStatus,
	}
	writeAdminRequestEvents(t, dir, event)

	recorder, response := requestAdminRequests(
		t,
		gateway,
		"/v1/admin/requests?filter=fallbacks",
		http.MethodGet,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(response.Items) != 1 {
		t.Fatalf("items = %+v", response.Items)
	}
	row := response.Items[0]
	if row.EventID != event.EventID ||
		row.CompletedAt != event.CompletedAt ||
		row.Client != "claude-code" ||
		row.ConfiguredRoute != "baseten" ||
		row.EffectiveProvider != "anthropic" ||
		row.RequestedModel != "claude-opus-4-8" ||
		row.RequestedModelFamily != "opus" ||
		row.ServedModel != "claude-opus-4-8" ||
		row.Status != nil ||
		row.TerminationReason != telemetry.TerminationGatewayError ||
		row.DurationMS != 321 ||
		row.TTFTMS != nil ||
		!row.Subagent ||
		!row.Fallback.Attempted ||
		row.Fallback.Count != 1 ||
		row.Fallback.Trigger == nil ||
		*row.Fallback.Trigger != trigger ||
		row.Primary == nil ||
		row.Primary.Provider != "baseten" ||
		row.Primary.Model != "zai-org/GLM-5.2" ||
		!row.Primary.Attempted ||
		row.Primary.Outcome != telemetry.PrimaryOutcomeImageInputUnsupported ||
		row.Primary.Status == nil || *row.Primary.Status != http.StatusBadRequest {
		t.Fatalf("row = %+v", row)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":null`) ||
		!strings.Contains(body, `"ttft_ms":null`) {
		t.Fatalf("nullable fields not explicit: %s", body)
	}
}

func TestAdminRequestsFilters(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	setAdminRequestsNow(t, now)
	gateway, dir := newAdminRequestsGateway(t, analytics.IndexOptions{})

	success := adminRequestsEvent(1, now.Add(-3*time.Second))
	fallback := adminRequestsEvent(2, now.Add(-2*time.Second))
	fallback.Fallback.Attempted = true
	fallback.Fallback.Count = 1
	fallbackTrigger := "image_input_unsupported"
	fallback.Fallback.Trigger = &fallbackTrigger
	httpError := adminRequestsEvent(3, now.Add(-time.Second))
	status := http.StatusBadRequest
	httpError.Status = &status
	httpError.TerminationReason = telemetry.TerminationUpstreamHTTPError
	localError := adminRequestsEvent(4, now)
	localError.TerminationReason = telemetry.TerminationGatewayError
	writeAdminRequestEvents(t, dir, success, fallback, httpError, localError)

	assertAdminRequestIDs(
		t,
		getAdminRequests(t, gateway, "/v1/admin/requests?filter=all"),
		4, 3, 2, 1,
	)
	assertAdminRequestIDs(
		t,
		getAdminRequests(t, gateway, "/v1/admin/requests?filter=fallbacks"),
		2,
	)
	assertAdminRequestIDs(
		t,
		getAdminRequests(t, gateway, "/v1/admin/requests?filter=errors"),
		4, 3,
	)
}

func TestAdminRequestsRejectsInvalidLimitsFiltersCursorsAndMethods(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	setAdminRequestsNow(t, now)
	gateway, _ := newAdminRequestsGateway(t, analytics.IndexOptions{})

	for _, limit := range []string{"0", "-1", "501", "garbage"} {
		recorder, _ := requestAdminRequests(
			t,
			gateway,
			"/v1/admin/requests?limit="+url.QueryEscape(limit),
			http.MethodGet,
		)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("limit %q status = %d, want 400", limit, recorder.Code)
		}
	}
	for _, limit := range []string{"1", "500"} {
		if parsed, ok := adminRequestsLimit(limit); !ok || parsed != mustAtoi(t, limit) {
			t.Errorf("limit %q = %d, %v", limit, parsed, ok)
		}
	}

	recorder, _ := requestAdminRequests(
		t,
		gateway,
		"/v1/admin/requests?filter=unknown",
		http.MethodGet,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("unknown filter status = %d, want 400", recorder.Code)
	}

	malformed := []string{
		"not-base64!",
		encodeRawAdminRequestsCursor(t, `{"v":2,"completed_at":"2026-07-26T12:00:00Z","event_id":"00000000000000000000000000000001"}`),
		encodeRawAdminRequestsCursor(t, `{"v":1,"completed_at":"2026-07-26T12:00:00Z","event_id":"bad"}`),
		encodeRawAdminRequestsCursor(t, `{"v":1,"completed_at":"2026-07-26T12:00:00Z","event_id":"00000000000000000000000000000001","extra":true}`),
		encodeRawAdminRequestsCursor(t, `{"v":1,"completed_at":"2026-07-26T12:00:00Z","event_id":"00000000000000000000000000000001"} {}`),
	}
	for _, cursor := range malformed {
		recorder, _ := requestAdminRequests(
			t,
			gateway,
			"/v1/admin/requests?cursor="+url.QueryEscape(cursor),
			http.MethodGet,
		)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("cursor %q status = %d, want 400", cursor, recorder.Code)
		}
	}

	recorder, _ = requestAdminRequests(
		t,
		gateway,
		"/v1/admin/requests",
		http.MethodPost,
	)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", recorder.Code)
	}
}

func TestAdminRequestsReportsPartialIndexCoverage(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	setAdminRequestsNow(t, now)
	gateway, dir := newAdminRequestsGateway(t, analytics.IndexOptions{
		MaxAge:            analyticsMaxWindow,
		MaxRows:           2,
		BootstrapMaxBytes: 1 << 20,
	})
	writeAdminRequestEvents(
		t,
		dir,
		adminRequestsEvent(1, now.Add(-3*time.Second)),
		adminRequestsEvent(2, now.Add(-2*time.Second)),
		adminRequestsEvent(3, now.Add(-time.Second)),
	)

	response := getAdminRequests(t, gateway, "/v1/admin/requests")
	assertAdminRequestIDs(t, response, 3, 2)
	if response.Coverage.Complete || response.Coverage.Reason != "row cap" {
		t.Fatalf("coverage = %+v", response.Coverage)
	}
}

func newAdminRequestsGateway(
	t *testing.T,
	options analytics.IndexOptions,
) (*Gateway, string) {
	t.Helper()
	dir := t.TempDir()
	return &Gateway{
		cfg:            Config{TelemetryDir: dir},
		analyticsIndex: analytics.NewIndex(options),
	}, dir
}

func setAdminRequestsNow(t *testing.T, now time.Time) {
	t.Helper()
	original := adminRequestsNow
	adminRequestsNow = func() time.Time { return now }
	t.Cleanup(func() { adminRequestsNow = original })
}

func adminRequestsEvent(index int, completedAt time.Time) telemetry.EventV1 {
	event := gatewayTelemetryEvent(index)
	event.StartedAt = completedAt.Add(-time.Second)
	event.CompletedAt = completedAt
	event.Client = "codex"
	event.ConfiguredRoute = "baseten"
	event.EffectiveProvider = "baseten"
	event.RequestedModel = "requested-model"
	event.RequestedModelFamily = "opus"
	event.ServedModel = "served-model"
	status := http.StatusOK
	event.Status = &status
	return event
}

func writeAdminRequestEvents(
	t *testing.T,
	dir string,
	events ...telemetry.EventV1,
) {
	t.Helper()
	writer, err := telemetry.NewWriter(telemetry.WriterOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := writer.Write(event); err != nil {
			_ = writer.Close()
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func requestAdminRequests(
	t *testing.T,
	gateway *Gateway,
	target string,
	method string,
) (*httptest.ResponseRecorder, adminRequestsResponse) {
	t.Helper()
	recorder := httptest.NewRecorder()
	gateway.adminRequests(recorder, httptest.NewRequest(method, target, nil))
	if recorder.Code != http.StatusOK {
		return recorder, adminRequestsResponse{}
	}
	return recorder, decodeAdminRequestsResponse(t, recorder)
}

func getAdminRequests(
	t *testing.T,
	gateway *Gateway,
	target string,
) adminRequestsResponse {
	t.Helper()
	recorder, response := requestAdminRequests(t, gateway, target, http.MethodGet)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	return response
}

func decodeAdminRequestsResponse(
	t *testing.T,
	recorder *httptest.ResponseRecorder,
) adminRequestsResponse {
	t.Helper()
	var response adminRequestsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v\n%s", err, recorder.Body.String())
	}
	return response
}

func assertAdminRequestIDs(
	t *testing.T,
	response adminRequestsResponse,
	indexes ...int,
) {
	t.Helper()
	if len(response.Items) != len(indexes) {
		t.Fatalf("items = %d, want %d: %+v", len(response.Items), len(indexes), response.Items)
	}
	for position, index := range indexes {
		if response.Items[position].EventID != formatTelemetryEventID(index) {
			t.Fatalf(
				"items[%d].event_id = %s, want %s",
				position,
				response.Items[position].EventID,
				formatTelemetryEventID(index),
			)
		}
	}
}

func encodeRawAdminRequestsCursor(t *testing.T, value string) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}
