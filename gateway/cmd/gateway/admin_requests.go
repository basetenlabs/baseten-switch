package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

const (
	adminRequestsDefaultLimit = 100
	adminRequestsMaxLimit     = 500
	adminRequestsCursorMaxLen = 2048
	adminRequestsCursorV1     = 1
)

var adminRequestsNow = time.Now

type adminRequestRow struct {
	EventID              string                      `json:"event_id"`
	CompletedAt          time.Time                   `json:"completed_at"`
	Client               string                      `json:"client"`
	ConfiguredRoute      string                      `json:"configured_route"`
	EffectiveProvider    string                      `json:"effective_provider"`
	RequestedModel       string                      `json:"requested_model"`
	RequestedModelFamily string                      `json:"requested_model_family"`
	ServedModel          string                      `json:"served_model"`
	Status               *int                        `json:"status"`
	TerminationReason    telemetry.TerminationReason `json:"termination_reason"`
	DurationMS           int64                       `json:"duration_ms"`
	TTFTMS               *int64                      `json:"ttft_ms"`
	Subagent             bool                        `json:"subagent"`
	Fallback             telemetry.FallbackV1        `json:"fallback"`
	Primary              *telemetry.PrimaryV1        `json:"primary,omitempty"`
}

type adminRequestsCoverage struct {
	Complete bool   `json:"complete"`
	Reason   string `json:"reason"`
}

type adminRequestsResponse struct {
	Items      []adminRequestRow     `json:"items"`
	NextCursor *string               `json:"next_cursor"`
	HasMore    bool                  `json:"has_more"`
	Coverage   adminRequestsCoverage `json:"coverage"`
}

type adminRequestsCursor struct {
	Version     int       `json:"v"`
	CompletedAt time.Time `json:"completed_at"`
	EventID     string    `json:"event_id"`
}

func (g *Gateway) adminRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		g.reject(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	limit, ok := adminRequestsLimit(r.URL.Query().Get("limit"))
	if !ok {
		g.reject(w, http.StatusBadRequest, "invalid limit: "+r.URL.Query().Get("limit"))
		return
	}
	filter, ok := adminRequestsFilter(r.URL.Query().Get("filter"))
	if !ok {
		g.reject(w, http.StatusBadRequest, "invalid filter: "+r.URL.Query().Get("filter"))
		return
	}
	cursor, err := decodeAdminRequestsCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		g.reject(w, http.StatusBadRequest, "invalid cursor")
		return
	}

	now := adminRequestsNow().UTC()
	since := now.Add(-analyticsMaxWindow).Unix()
	snapshot := g.getAnalyticsIndex().Snapshot(
		g.runtimeConfig().TelemetryDir,
		now,
		since,
	)

	start := len(snapshot.Events) - 1
	if cursor != nil {
		start = sort.Search(len(snapshot.Events), func(index int) bool {
			return compareAdminRequestKey(snapshot.Events[index], *cursor) >= 0
		}) - 1
	}

	items := make([]adminRequestRow, 0, min(limit, max(start+1, 0)))
	hasMore := false
	for index := start; index >= 0; index-- {
		event := snapshot.Events[index]
		if !adminRequestMatchesFilter(event, filter) {
			continue
		}
		if len(items) == limit {
			hasMore = true
			break
		}
		items = append(items, projectAdminRequest(event))
	}

	var nextCursor *string
	if hasMore {
		value, encodeErr := encodeAdminRequestsCursor(items[len(items)-1])
		if encodeErr != nil {
			g.reject(w, http.StatusInternalServerError, "encode pagination cursor")
			return
		}
		nextCursor = &value
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, adminRequestsResponse{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
		Coverage: adminRequestsCoverage{
			Complete: snapshot.Complete,
			Reason:   snapshot.Reason,
		},
	})
}

func adminRequestsLimit(value string) (int, bool) {
	if value == "" {
		return adminRequestsDefaultLimit, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 || limit > adminRequestsMaxLimit {
		return 0, false
	}
	return limit, true
}

func adminRequestsFilter(value string) (string, bool) {
	if value == "" {
		return "all", true
	}
	switch value {
	case "all", "fallbacks", "errors":
		return value, true
	default:
		return "", false
	}
}

func adminRequestMatchesFilter(event telemetry.EventV1, filter string) bool {
	switch filter {
	case "fallbacks":
		return event.Fallback.Attempted
	case "errors":
		return event.IsHTTPError() ||
			event.TerminationReason != telemetry.TerminationCompleted
	default:
		return true
	}
}

func projectAdminRequest(event telemetry.EventV1) adminRequestRow {
	return adminRequestRow{
		EventID:              event.EventID,
		CompletedAt:          event.CompletedAt,
		Client:               event.Client,
		ConfiguredRoute:      event.ConfiguredRoute,
		EffectiveProvider:    event.EffectiveProvider,
		RequestedModel:       event.RequestedModel,
		RequestedModelFamily: event.RequestedModelFamily,
		ServedModel:          event.ServedModel,
		Status:               event.Status,
		TerminationReason:    event.TerminationReason,
		DurationMS:           event.DurationMS,
		TTFTMS:               event.TTFTMS,
		Subagent:             event.Subagent,
		Fallback:             event.Fallback,
		Primary:              clonePrimaryV1(event.Primary),
	}
}

func compareAdminRequestKey(
	event telemetry.EventV1,
	cursor adminRequestsCursor,
) int {
	if event.CompletedAt.Before(cursor.CompletedAt) {
		return -1
	}
	if event.CompletedAt.After(cursor.CompletedAt) {
		return 1
	}
	switch {
	case event.EventID < cursor.EventID:
		return -1
	case event.EventID > cursor.EventID:
		return 1
	default:
		return 0
	}
}

func encodeAdminRequestsCursor(row adminRequestRow) (string, error) {
	encoded, err := json.Marshal(adminRequestsCursor{
		Version:     adminRequestsCursorV1,
		CompletedAt: row.CompletedAt.UTC(),
		EventID:     row.EventID,
	})
	if err != nil {
		return "", fmt.Errorf("marshal admin requests cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeAdminRequestsCursor(value string) (*adminRequestsCursor, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > adminRequestsCursorMaxLen {
		return nil, fmt.Errorf("admin requests cursor exceeds %d bytes", adminRequestsCursorMaxLen)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode admin requests cursor: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor adminRequestsCursor
	if err := decoder.Decode(&cursor); err != nil {
		return nil, fmt.Errorf("parse admin requests cursor: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, fmt.Errorf("parse admin requests cursor: %w", err)
	}
	if cursor.Version != adminRequestsCursorV1 {
		return nil, fmt.Errorf("unsupported admin requests cursor version")
	}
	if cursor.CompletedAt.IsZero() {
		return nil, fmt.Errorf("admin requests cursor completed_at is required")
	}
	if !validAdminRequestEventID(cursor.EventID) {
		return nil, fmt.Errorf("admin requests cursor event_id is invalid")
	}
	cursor.CompletedAt = cursor.CompletedAt.UTC()
	return &cursor, nil
}

func validAdminRequestEventID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
