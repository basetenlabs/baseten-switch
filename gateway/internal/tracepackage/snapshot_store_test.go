package tracepackage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTraceStoreSnapshotUsesCompletePerSegmentBoundaries(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "trace-content-2026-08-13-001.jsonl")
	second := filepath.Join(directory, "trace-content-2026-08-13-002.jsonl")
	writePrivateFile(t, first, []byte("one\npartial"))
	writePrivateFile(t, second, []byte("two\n"))

	snapshot, err := TraceStoreSnapshot(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.EstimatedBytes != int64(len("one\ntwo\n")) {
		t.Fatalf("estimated bytes = %d", snapshot.EstimatedBytes)
	}
	reader, err := snapshot.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatal(errors.Join(readErr, closeErr))
	}
	if got := string(data); got != "one\ntwo\n" {
		t.Fatalf("snapshot data = %q", got)
	}
	if err := snapshot.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTraceStoreSnapshotAllowsAppendBeyondBoundary(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "trace-content-2026-08-13-001.jsonl")
	writePrivateFile(t, path, []byte("one\npartial"))
	snapshot, err := TraceStoreSnapshot(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("-tail\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := snapshot.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatal(errors.Join(readErr, closeErr))
	}
	if got := string(data); got != "one\n" {
		t.Fatalf("snapshot data = %q", got)
	}
	if err := snapshot.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTraceStoreSnapshotDetectsMutationWithinBoundary(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "trace-content-2026-08-13-001.jsonl")
	writePrivateFile(t, path, []byte("one\n"))
	snapshot, err := TraceStoreSnapshot(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := snapshot.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("X"), 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Verify(context.Background()); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatalf("Verify error = %v, want ErrSnapshotChanged", err)
	}
}

func TestTraceStoreSnapshotDetectsNewSegment(t *testing.T) {
	directory := t.TempDir()
	writePrivateFile(t,
		filepath.Join(directory, "trace-content-2026-08-13-001.jsonl"),
		[]byte("one\n"),
	)
	snapshot, err := TraceStoreSnapshot(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := snapshot.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t,
		filepath.Join(directory, "trace-content-2026-08-13-002.jsonl"),
		[]byte("two\n"),
	)
	if err := snapshot.Verify(context.Background()); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatalf("Verify error = %v, want ErrSnapshotChanged", err)
	}
}

func TestTraceStoreSnapshotRejectsRecognizedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link test requires privileges")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	writePrivateFile(t, target, []byte("one\n"))
	if err := os.Symlink(target,
		filepath.Join(directory, "trace-content-2026-08-13-001.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := TraceStoreSnapshot(context.Background(), directory); err == nil {
		t.Fatal("TraceStoreSnapshot accepted recognized symlink")
	}
}

func TestTelemetryStoreSnapshotRecognizesTelemetrySegmentsOnly(t *testing.T) {
	directory := t.TempDir()
	writePrivateFile(t,
		filepath.Join(directory, "requests-2026-08-001.jsonl"),
		[]byte("telemetry\n"),
	)
	writePrivateFile(t, filepath.Join(directory, "unrecognized.jsonl"), []byte("ignore\n"))
	snapshot, err := TelemetryStoreSnapshot(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := snapshot.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatal(errors.Join(readErr, closeErr))
	}
	if got := string(data); got != "telemetry\n" {
		t.Fatalf("snapshot data = %q", got)
	}
	if err := snapshot.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func writePrivateFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
