package tracecapture

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadSelectedTracesFiltersAndReportsContentFreeCounts(t *testing.T) {
	dir := privateTempDir(t)
	since := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	before := validTrace(t, since.Add(-time.Second), "51")
	selectedTrace := validTrace(t, since.Add(time.Minute), "52")
	otherClient := validTrace(t, since.Add(2*time.Minute), "53")
	otherClient.Client = "codex"
	atUntil := validTrace(t, until.Add(time.Second), "54")
	invalid := validTrace(t, since.Add(3*time.Minute), "55")
	invalid.SchemaVersion = 9

	path := filepath.Join(dir, "trace-content-2026-08-17-001.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, trace := range []TraceV1{before, selectedTrace, otherClient, atUntil, invalid} {
		encoded, marshalErr := json.Marshal(trace)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := file.Write([]byte("not-json\n{partial")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	traces, stats, err := ReadSelectedTraces(dir, TraceSelection{
		Since: since, Until: until, Clients: []string{"claude-code"},
		MaxRetainedEncodedBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 || traces[0].EventID != selectedTrace.EventID {
		t.Fatalf("unexpected selected traces: %+v", traces)
	}
	if stats.SegmentsSnapshot != 1 || stats.CompleteRows != 6 ||
		stats.SelectedRows != 1 || stats.OutsideWindowRows != 2 ||
		stats.OtherClientRows != 1 || stats.MalformedRows != 1 ||
		stats.InvalidRows != 1 || stats.IncompleteTailSegments != 1 ||
		stats.SelectedEncodedBytes <= 0 {
		t.Fatalf("unexpected selection stats: %+v", stats)
	}
}

func TestReadSelectedTracesEnforcesRetainedByteLimit(t *testing.T) {
	dir := privateTempDir(t)
	now := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	trace := validTrace(t, now, "61")
	encoded, _ := json.Marshal(trace)
	path := filepath.Join(dir, "trace-content-2026-08-17-001.jsonl")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	traces, stats, err := ReadSelectedTraces(dir, TraceSelection{
		Since: now.Add(-time.Minute), Until: now.Add(time.Minute),
		Clients:                 []string{"claude-code"},
		MaxRetainedEncodedBytes: int64(len(encoded)),
	})
	if !errors.Is(err, ErrSelectedTraceByteLimit) {
		t.Fatalf("expected byte limit error, got %v", err)
	}
	if len(traces) != 0 || stats.SelectedRows != 0 || stats.CompleteRows != 1 {
		t.Fatalf("limit retained content: traces=%d stats=%+v", len(traces), stats)
	}
}

func TestSelectedSegmentEnforcesFixedRowLimit(t *testing.T) {
	dir := privateTempDir(t)
	now := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	trace := validTrace(t, now, "71")
	encoded, _ := json.Marshal(trace)
	path := filepath.Join(dir, "trace-content-2026-08-17-001.jsonl")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := snapshotTraceSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	selected := make([]TraceV1, MaxSelectedTraceRows)
	stats := TraceSelectionStats{SelectedRows: MaxSelectedTraceRows}
	err = streamSelectedSegment(
		snapshots[0],
		TraceSelection{
			Since: now.Add(-time.Minute), Until: now.Add(time.Minute),
			Clients: []string{"claude-code"}, MaxRetainedEncodedBytes: 1 << 20,
		},
		map[string]struct{}{"claude-code": {}},
		&selected,
		&stats,
	)
	if !errors.Is(err, ErrSelectedTraceRowLimit) || len(selected) != MaxSelectedTraceRows {
		t.Fatalf("expected row limit without retention, got len=%d err=%v", len(selected), err)
	}
}

func TestReadSelectedTracesValidatesSelectionAndRejectsSymlink(t *testing.T) {
	dir := privateTempDir(t)
	if _, _, err := ReadSelectedTraces(dir, TraceSelection{}); err == nil {
		t.Fatal("invalid empty selection accepted")
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "trace-content-2026-08-17-001.jsonl")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, _, err := ReadSelectedTraces(dir, TraceSelection{
		Since: now.Add(-time.Hour), Until: now,
		Clients: []string{"claude-code"}, MaxRetainedEncodedBytes: 1 << 20,
	}); err == nil {
		t.Fatal("symlinked recognized segment accepted")
	}
}
