package tracecapture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriterWritesReadsAndClosesGracefully(t *testing.T) {
	dir := privateTempDir(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	w, err := newWriter(Config{Dir: dir, RetentionDays: 7}, defaultWriterLimits(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	trace := validTrace(t, now, "10")
	if result := w.Enqueue(trace); !result.Accepted {
		t.Fatalf("enqueue rejected: %+v", result)
	}
	closeResult := w.Close(context.Background())
	if !closeResult.Drained || closeResult.Error != "" {
		t.Fatalf("unexpected close result: %+v", closeResult)
	}
	traces, err := ReadTraces(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 || traces[0].EventID != trace.EventID {
		t.Fatalf("unexpected traces: %+v", traces)
	}
	status := w.Status()
	if status.State != "disabled" || status.ActiveSegment != "" || status.LastSuccessfulWrite == nil {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestWriterReleasesTransferredBodyMemoryAfterWrite(t *testing.T) {
	dir := privateTempDir(t)
	w, err := NewWriter(Config{Dir: dir, RetentionDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	trace := validTrace(t, time.Now().UTC(), "14")
	request := []byte(`{"deferred":true}`)
	trace.Request.BodyBase64 = ""
	trace.Request.RawBody = request
	trace.Request.ObservedBytes = int64(len(request))
	released := make(chan struct{})
	if result := w.EnqueueWithRelease(trace, func() { close(released) }); !result.Accepted {
		t.Fatalf("enqueue rejected: %+v", result)
	}
	if result := w.Close(context.Background()); !result.Drained {
		t.Fatalf("close = %+v", result)
	}
	select {
	case <-released:
	default:
		t.Fatal("writer did not release transferred body memory")
	}
}

func TestWriterDailyAndSizeRotation(t *testing.T) {
	dir := privateTempDir(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	trace := validTrace(t, now, "11")
	encoded, _ := json.Marshal(trace)
	limits := writerLimits{
		maxSegmentBytes: int64(len(encoded) + 2),
		maxStoreBytes:   int64(len(encoded))*5 + 4096,
		maxQueueBytes:   1 << 20, maxQueueRecords: 8,
		maxEncodedRowBytes: len(encoded) + 1024,
	}
	w, err := newWriter(Config{Dir: dir, RetentionDays: 7}, limits, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for index, value := range []TraceV1{
		trace,
		validTrace(t, now.Add(time.Minute), "12"),
		validTrace(t, now.AddDate(0, 0, 1), "13"),
	} {
		w.writeTrace(value)
		if status := w.Status(); status.DroppedRecords["storage"] != 0 {
			t.Fatalf("write %d failed: %+v", index, status)
		}
	}
	if result := w.Close(context.Background()); !result.Drained {
		t.Fatalf("close did not drain: %+v", result)
	}
	segments, err := DiscoverSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 3 {
		t.Fatalf("expected three rotated segments, got %+v", segments)
	}
	if segments[0].Day.Equal(segments[2].Day) {
		t.Fatalf("daily rotation did not occur: %+v", segments)
	}
}

func TestWriterEvictsOldestClosedSegmentForQuota(t *testing.T) {
	dir := privateTempDir(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	trace := validTrace(t, now, "21")
	encoded, _ := json.Marshal(trace)
	lineBytes := int64(len(encoded) + 1)
	limits := writerLimits{
		maxSegmentBytes:    lineBytes + 1,
		maxEncodedRowBytes: len(encoded) + 32,
		maxStoreBytes:      2*lineBytes + 128,
		maxQueueBytes:      1 << 20, maxQueueRecords: 8,
	}
	w, err := newWriter(Config{Dir: dir, RetentionDays: 30}, limits, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []TraceV1{
		trace,
		validTrace(t, now.Add(time.Minute), "22"),
		validTrace(t, now.Add(2*time.Minute), "23"),
	} {
		w.writeTrace(value)
	}
	w.Close(context.Background())
	segments, err := DiscoverSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || strings.HasSuffix(segments[0].Name, "001.jsonl") {
		t.Fatalf("oldest closed segment was not evicted: %+v", segments)
	}
	traces, err := ReadTraces(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 2 || traces[0].EventID == trace.EventID {
		t.Fatalf("unexpected retained traces: %+v", traces)
	}
}

func TestWriterRecoversPartialFinalLine(t *testing.T) {
	dir := privateTempDir(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	trace := validTrace(t, now, "31")
	encoded, _ := json.Marshal(trace)
	path := filepath.Join(dir, "trace-content-2026-08-17-001.jsonl")
	content := append(append(encoded, '\n'), []byte("{partial")...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := newWriter(
		Config{Dir: dir, RetentionDays: 7},
		defaultWriterLimits(),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	if status := w.Status(); status.RecoveredPartialLines != 1 {
		t.Fatalf("expected recovery, got %+v", status)
	}
	w.Close(context.Background())
	traces, err := ReadTraces(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 || traces[0].EventID != trace.EventID {
		t.Fatalf("unexpected recovered traces: %+v", traces)
	}
}

func TestWriterLockAndSymlinkSafety(t *testing.T) {
	dir := privateTempDir(t)
	first, err := NewWriter(Config{Dir: dir, RetentionDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(Config{Dir: dir, RetentionDays: 7}); err == nil {
		t.Fatal("second writer acquired the store lock")
	}
	first.Close(context.Background())

	dir = privateTempDir(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "trace-content-2026-08-17-001.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(Config{Dir: dir, RetentionDays: 7}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected segment symlink rejection, got %v", err)
	}
}

func TestSweepRetentionDoesNotCreateAndHonorsLock(t *testing.T) {
	parent := privateTempDir(t)
	missing := filepath.Join(parent, "missing")
	result, err := SweepRetention(missing, 7, time.Now())
	if err != nil || result.RemovedBytes != 0 {
		t.Fatalf("unexpected missing-store sweep result: %+v %v", result, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sweep created missing directory: %v", err)
	}

	dir := privateTempDir(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	oldPath := filepath.Join(dir, "trace-content-2026-08-01-001.jsonl")
	newPath := filepath.Join(dir, "trace-content-2026-08-16-001.jsonl")
	if err := os.WriteFile(oldPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockFile, err := acquireWriterLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := SweepRetention(dir, 7, now); !errors.Is(err, ErrStoreLocked) || !result.Skipped {
		t.Fatalf("expected locked sweep, got %+v %v", result, err)
	}
	if err := releaseWriterLock(lockFile); err != nil {
		t.Fatal(err)
	}
	result, err = SweepRetention(dir, 7, now)
	if err != nil || result.RemovedBytes != 4 {
		t.Fatalf("unexpected sweep: %+v %v", result, err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old segment retained: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new segment removed: %v", err)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(working, ".tracecapture-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
