package telemetry

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWriterConcurrentSubmissionsProduceCompleteUniqueLines(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "telemetry")
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	writer, err := NewWriter(WriterOptions{Dir: dir, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	const count = 100
	var wait sync.WaitGroup
	errorsByEvent := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			event := testEvent(now, index)
			errorsByEvent <- writer.Write(event)
		}(index)
	}
	wait.Wait()
	close(errorsByEvent)
	for err := range errorsByEvent {
		if err != nil {
			t.Error(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	events := readAllEvents(t, dir)
	if len(events) != count {
		t.Fatalf("read %d events, want %d", len(events), count)
	}
	ids := make(map[string]bool, count)
	for _, event := range events {
		if ids[event.EventID] {
			t.Fatalf("duplicate event id %q", event.EventID)
		}
		ids[event.EventID] = true
	}
	if health := writer.Health(); health.DroppedEvents != 0 || health.LastWriteAt == nil {
		t.Fatalf("writer health = %+v", health)
	}
}

func TestWriterAndReaderShareEventLineLimit(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	exact := eventWithEncodedLineSize(t, now, MaxEventLineBytes)
	exactDir := filepath.Join(t.TempDir(), "exact")
	writer, err := NewWriter(WriterOptions{Dir: exactDir, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(exact); err != nil {
		t.Fatalf("exact-limit event rejected: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := ReadEvents(exactDir)
	if err != nil || len(events) != 1 {
		t.Fatalf("exact-limit read events=%d error=%v", len(events), err)
	}

	oversized := eventWithEncodedLineSize(t, now, MaxEventLineBytes+1)
	oversizedDir := filepath.Join(t.TempDir(), "oversized")
	writer, err = NewWriter(WriterOptions{Dir: oversizedDir, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(oversized); err == nil {
		t.Fatal("oversized event was accepted")
	}
	health := writer.Health()
	if health.DroppedEvents != 1 ||
		!strings.Contains(health.LastWriteError, "maximum") {
		t.Fatalf("oversized writer health = %+v", health)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriterCreatesAndRepairsPrivateModes(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "telemetry")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	writeEventLine(t, path, testEvent(time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC), 1), 0o666)

	writer, err := NewWriter(WriterOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("telemetry directory mode = %o, want 700", got)
	}
	for _, name := range []string{"requests-2026-07-001.jsonl", writerLockName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, got)
		}
	}
}

func TestValidateTelemetryDirectoryPathRefusesBroadDirectories(t *testing.T) {
	paths := []string{string(os.PathSeparator), os.TempDir()}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, home)
	}
	seen := make(map[string]bool)
	for _, path := range paths {
		path, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		t.Run(strings.ReplaceAll(path, string(os.PathSeparator), "_"), func(t *testing.T) {
			err := validateTelemetryDirectoryPath(path)
			if err == nil || !strings.Contains(err.Error(), "refusing unsafe telemetry directory") {
				t.Fatalf("validateTelemetryDirectoryPath(%q) error = %v", path, err)
			}
		})
	}
}

func TestWriterRotatesBeforeSizeBoundary(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC)
	event := testEvent(now, 1)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewWriter(WriterOptions{
		Dir:             dir,
		MaxSegmentBytes: int64(len(encoded) + 1),
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(event); err != nil {
		t.Fatal(err)
	}
	event.EventID = fmt.Sprintf("%032x", 2)
	if err := writer.Write(event); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	segments, err := DiscoverSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("found %d segments, want 2", len(segments))
	}
	if segments[0].Name != "requests-2026-07-001.jsonl" ||
		segments[1].Name != "requests-2026-07-002.jsonl" {
		t.Fatalf("segments = %+v", segments)
	}
	for _, segment := range segments {
		if got := len(readEvents(t, segment.Path)); got != 1 {
			t.Fatalf("%s contains %d events, want 1", segment.Name, got)
		}
	}
}

func TestWriterRotatesWhenEventUTCMonthChanges(t *testing.T) {
	dir := t.TempDir()
	july := time.Date(2026, time.July, 31, 23, 59, 0, 0, time.UTC)
	august := july.Add(2 * time.Minute)
	writer, err := NewWriter(WriterOptions{Dir: dir, Now: func() time.Time { return august }})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(testEvent(july, 1)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(testEvent(august, 2)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	segments, err := DiscoverSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 ||
		segments[0].Name != "requests-2026-07-001.jsonl" ||
		segments[1].Name != "requests-2026-08-001.jsonl" {
		t.Fatalf("segments = %+v", segments)
	}
}

func TestWriterRecoversInvalidFinalSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	first := testEvent(time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC), 1)
	writeEventLine(t, path, first, 0o600)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"schema_version":1,"event":"request"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	writer, err := NewWriter(WriterOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	health := writer.Health()
	if health.RecoveredPartialLines != 1 {
		t.Fatalf("recovered partial lines = %d, want 1", health.RecoveredPartialLines)
	}
	second := testEvent(first.CompletedAt.Add(time.Minute), 2)
	if err := writer.Write(second); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, path)
	if len(events) != 2 || events[0].EventID != first.EventID || events[1].EventID != second.EventID {
		t.Fatalf("recovered events = %+v", events)
	}
}

func TestWriterRecoveryRejectsSparseOversizedSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	first := testEvent(
		time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC),
		1,
	)
	writeEventLine(t, path, first, 0o600)
	file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(int64(MaxEventLineBytes*2), io.SeekEnd); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteString("invalid-tail"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(WriterOptions{Dir: dir}); err == nil ||
		!strings.Contains(err.Error(), "final line exceeds") {
		t.Fatalf("NewWriter() error = %v, want oversized final-line rejection", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("oversized segment size changed from %d to %d", before.Size(), after.Size())
	}
}

func TestWriterRepairsValidFinalJSONMissingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	event := testEvent(time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC), 1)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	writer, err := NewWriter(WriterOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("repaired segment does not end in newline: %q", data)
	}
	if got := len(readEvents(t, path)); got != 1 {
		t.Fatalf("read %d events, want 1", got)
	}
}

func TestWriterStartupRetentionKeepsActiveSegment(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC)
	writeEventLine(t, filepath.Join(dir, "requests-2026-01-001.jsonl"), testEvent(old, 1), 0o600)
	writeEventLine(t, filepath.Join(dir, "requests-2026-01-002.jsonl"), testEvent(old, 2), 0o600)

	writer, err := NewWriter(WriterOptions{
		Dir:           dir,
		RetentionDays: 90,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	assertFileExistence(t, filepath.Join(dir, "requests-2026-01-001.jsonl"), false)
	assertFileExistence(t, filepath.Join(dir, "requests-2026-01-002.jsonl"), true)
	if got := writer.Health().ActiveSegment; got != "requests-2026-01-002.jsonl" {
		t.Fatalf("active segment = %q", got)
	}
}

func TestWriterRunsRetentionAfterRotation(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC)
	oldPath := filepath.Join(dir, "requests-2026-01-001.jsonl")
	writeEventLine(t, oldPath, testEvent(old, 1), 0o600)

	writer, err := NewWriter(WriterOptions{
		Dir:           dir,
		RetentionDays: 90,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	assertFileExistence(t, oldPath, true)

	if err := writer.Write(testEvent(now, 2)); err != nil {
		t.Fatal(err)
	}
	assertFileExistence(t, oldPath, false)
}

func TestWriterRunsRetentionOnceOnNextUTCDay(t *testing.T) {
	dir := t.TempDir()
	current := time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC)
	activePath := filepath.Join(dir, "requests-2026-07-001.jsonl")
	writeEventLine(t, activePath, testEvent(current, 1), 0o600)

	writer, err := NewWriter(WriterOptions{
		Dir:           dir,
		RetentionDays: 90,
		Now:           func() time.Time { return current },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	oldPath := filepath.Join(dir, "requests-2026-01-001.jsonl")
	writeEventLine(
		t,
		oldPath,
		testEvent(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), 2),
		0o600,
	)

	if err := writer.Write(testEvent(current.Add(time.Hour), 3)); err != nil {
		t.Fatal(err)
	}
	assertFileExistence(t, oldPath, true)

	current = current.Add(24 * time.Hour)
	if err := writer.Write(testEvent(current, 4)); err != nil {
		t.Fatal(err)
	}
	assertFileExistence(t, oldPath, false)
}

func TestWriterRejectsSecondOwner(t *testing.T) {
	dir := t.TempDir()
	first, err := NewWriter(WriterOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewWriter(WriterOptions{Dir: dir})
	if err == nil {
		_ = second.Close()
		t.Fatal("second writer unexpectedly acquired telemetry directory")
	}
}

func TestWriterRecordsDroppedEventsAndClosesIdempotently(t *testing.T) {
	writer, err := NewWriter(WriterOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	invalid := EventV1{}
	if err := writer.Write(invalid); err == nil {
		t.Fatal("invalid event write succeeded")
	}
	health := writer.Health()
	if health.DroppedEvents != 1 || !strings.Contains(health.LastWriteError, "schema_version") {
		t.Fatalf("writer health = %+v", health)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(testEvent(time.Now(), 1)); err == nil {
		t.Fatal("write after close succeeded")
	}
	if got := writer.Health().DroppedEvents; got != 2 {
		t.Fatalf("dropped events = %d, want 2", got)
	}
}

func testEvent(startedAt time.Time, index int) EventV1 {
	event := validEventV1(startedAt)
	event.EventID = fmt.Sprintf("%032x", index+1)
	return event
}

func eventWithEncodedLineSize(
	t *testing.T,
	completedAt time.Time,
	size int,
) EventV1 {
	t.Helper()
	event := testEvent(completedAt, 1)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	currentSize := len(encoded) + 1
	if currentSize > size {
		t.Fatalf("base event is %d bytes, requested %d", currentSize, size)
	}
	event.Client += strings.Repeat("c", size-currentSize)
	encoded, err = json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(encoded) + 1; got != size {
		t.Fatalf("encoded line size = %d, want %d", got, size)
	}
	return event
}

func writeEventLine(t *testing.T, path string, event EventV1, mode os.FileMode) {
	t.Helper()
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, mode); err != nil {
		t.Fatal(err)
	}
}

func readAllEvents(t *testing.T, dir string) []EventV1 {
	t.Helper()
	segments, err := DiscoverSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	var events []EventV1
	for _, segment := range segments {
		events = append(events, readEvents(t, segment.Path)...)
	}
	return events
}

func readEvents(t *testing.T, path string) []EventV1 {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []EventV1
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), MaxEventLineBytes)
	for scanner.Scan() {
		var event EventV1
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
	return events
}
