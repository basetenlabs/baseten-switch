package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadEventsAcrossSegmentsIgnoresMalformedAndPartialLines(t *testing.T) {
	dir := t.TempDir()
	first := validEventV1(time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC))
	second := validEventV1(first.CompletedAt.Add(time.Minute))
	second.EventID = "00000000000000000000000000000002"
	writeReaderLine(
		t,
		filepath.Join(dir, "requests-2026-07-001.jsonl"),
		first,
		true,
	)
	firstPath := filepath.Join(dir, "requests-2026-07-001.jsonl")
	file, err := os.OpenFile(firstPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{malformed}\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	writeReaderLine(
		t,
		filepath.Join(dir, "requests-2026-07-002.jsonl"),
		second,
		true,
	)
	writeReaderLine(
		t,
		filepath.Join(dir, "requests-2026-07-002.jsonl"),
		validEventV1(second.CompletedAt.Add(time.Minute)),
		false,
	)

	events, err := ReadEvents(dir)
	if err == nil {
		t.Fatal("malformed complete line did not surface a reader error")
	}
	if len(events) != 2 ||
		events[0].EventID != first.EventID ||
		events[1].EventID != second.EventID {
		t.Fatalf("events = %+v", events)
	}
}

func TestReadSegmentEventsAdvancesOnlyCompleteLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	first := validEventV1(time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC))
	second := validEventV1(first.CompletedAt.Add(time.Minute))
	second.EventID = "00000000000000000000000000000002"
	writeReaderLine(t, path, first, true)
	writeReaderLine(t, path, second, false)

	events, offset, err := ReadSegmentEvents(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != first.EventID {
		t.Fatalf("first read events = %+v", events)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	events, next, err := ReadSegmentEvents(path, offset)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != second.EventID || next <= offset {
		t.Fatalf("second read events=%+v offset=%d next=%d", events, offset, next)
	}
}

func TestReadSegmentEventsRejectsReplacementBetweenStatAndOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	replacementPath := filepath.Join(dir, "replacement.jsonl")
	writeReaderLine(
		t,
		path,
		validReaderEvent(time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC), 1),
		true,
	)
	writeReaderLine(
		t,
		replacementPath,
		validReaderEvent(time.Date(2026, time.July, 25, 12, 1, 0, 0, time.UTC), 2),
		true,
	)

	originalOpen := openTelemetrySegment
	t.Cleanup(func() { openTelemetrySegment = originalOpen })
	openTelemetrySegment = func(name string) (*os.File, error) {
		if name == path {
			if err := os.Rename(replacementPath, path); err != nil {
				return nil, err
			}
		}
		return originalOpen(name)
	}

	events, offset, err := ReadSegmentEvents(path, 0)
	if err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("error = %v, want replacement rejection", err)
	}
	if len(events) != 0 || offset != 0 {
		t.Fatalf("events=%+v offset=%d, want no data from replacement", events, offset)
	}
}

func TestReadSegmentEventsSupportsLinesLargerThanBuffer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	event := validReaderEvent(
		time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC),
		1,
	)
	event.Client = strings.Repeat("c", segmentReadBufferBytes+1)
	writeReaderLine(t, path, event, true)

	events, offset, err := ReadSegmentEvents(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Client != event.Client {
		t.Fatalf("full events = %+v", events)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if offset != info.Size() {
		t.Fatalf("offset = %d, want %d", offset, info.Size())
	}

}

func TestReadSegmentEventsRejectsOversizedLineAndContinues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.Repeat("x", MaxEventLineBytes) + "\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	event := validReaderEvent(
		time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC),
		1,
	)
	line := readerLineBytes(t, event)
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	events, offset, err := ReadSegmentEvents(path, 0)
	var rejected *SegmentReadError
	if !errors.As(err, &rejected) || rejected.MalformedLines != 1 {
		t.Fatalf("error = %v, want one malformed oversized line", err)
	}
	if len(events) != 1 || events[0].EventID != event.EventID {
		t.Fatalf("events = %+v, want valid event after oversized line", events)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if offset != info.Size() {
		t.Fatalf("offset = %d, want %d", offset, info.Size())
	}
}

func TestReadTailEventsCrossesNewestSegmentsAndIgnoresPartialFinalLine(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	for index := 1; index <= 4; index++ {
		event := validEventV1(base.Add(time.Duration(index) * time.Minute))
		event.EventID = fmt.Sprintf("%032x", index)
		sequence := 1
		if index > 2 {
			sequence = 2
		}
		writeReaderLine(
			t,
			filepath.Join(dir, fmt.Sprintf("requests-2026-07-%03d.jsonl", sequence)),
			event,
			true,
		)
	}
	partial := validEventV1(base.Add(5 * time.Minute))
	partial.EventID = fmt.Sprintf("%032x", 5)
	writeReaderLine(
		t,
		filepath.Join(dir, "requests-2026-07-002.jsonl"),
		partial,
		false,
	)

	events, err := ReadTailEvents(dir, 3, DefaultTailReadMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("ReadTailEvents() returned %d events, want 3", len(events))
	}
	for index, want := range []string{
		fmt.Sprintf("%032x", 2),
		fmt.Sprintf("%032x", 3),
		fmt.Sprintf("%032x", 4),
	} {
		if events[index].EventID != want {
			t.Fatalf("event[%d] = %s, want %s", index, events[index].EventID, want)
		}
	}
}

func TestReadTailEventsStopsAfterRequestedRowsWithinOneChunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	base := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	const eventCount = 200
	for index := 1; index <= eventCount; index++ {
		event := validEventV1(base.Add(time.Duration(index) * time.Second))
		event.EventID = fmt.Sprintf("%032x", index)
		writeReaderLine(t, path, event, true)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= tailReadChunkBytes {
		t.Fatalf("fixture size = %d, want more than one tail chunk", info.Size())
	}

	events, bytesRead, err := readTailEvents(dir, 2, DefaultTailReadMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if bytesRead != tailReadChunkBytes {
		t.Fatalf("bytes read = %d, want one %d-byte chunk", bytesRead, tailReadChunkBytes)
	}
	if len(events) != 2 ||
		events[0].EventID != fmt.Sprintf("%032x", eventCount-1) ||
		events[1].EventID != fmt.Sprintf("%032x", eventCount) {
		t.Fatalf("events = %+v", events)
	}
}

func TestReadTailEventsReportsByteLimitTruncationWithValidEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	base := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	for index := 1; index <= 3; index++ {
		event := validEventV1(base.Add(time.Duration(index) * time.Minute))
		event.EventID = fmt.Sprintf("%032x", index)
		writeReaderLine(t, path, event, true)
	}
	lastLineBytes := readerLineBytes(
		t,
		validReaderEvent(base.Add(3*time.Minute), 3),
	)
	byteLimit := int64(len(lastLineBytes) + 1)

	events, bytesRead, err := readTailEvents(dir, 2, byteLimit)
	if !errors.Is(err, ErrTailHistoryTruncated) {
		t.Fatalf("error = %v, want ErrTailHistoryTruncated", err)
	}
	if bytesRead != byteLimit {
		t.Fatalf("bytes read = %d, want %d", bytesRead, byteLimit)
	}
	if len(events) != 1 ||
		events[0].EventID != fmt.Sprintf("%032x", 3) {
		t.Fatalf("events = %+v, want newest valid event", events)
	}
}

func TestReadTailEventsExactLimitDoesNotReportTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	base := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	for index := 1; index <= 2; index++ {
		writeReaderLine(t, path, validReaderEvent(base.Add(time.Duration(index)*time.Minute), index), true)
	}
	lastLineBytes := readerLineBytes(
		t,
		validReaderEvent(base.Add(2*time.Minute), 2),
	)
	byteLimit := int64(len(lastLineBytes) + 1)

	events, bytesRead, err := readTailEvents(dir, 1, byteLimit)
	if err != nil {
		t.Fatalf("error = %v, want nil after satisfying requested limit", err)
	}
	if bytesRead != byteLimit {
		t.Fatalf("bytes read = %d, want %d", bytesRead, byteLimit)
	}
	if len(events) != 1 ||
		events[0].EventID != fmt.Sprintf("%032x", 2) {
		t.Fatalf("events = %+v, want newest valid event", events)
	}
}

func TestReadTailEventsReturnsCorruptLineErrorWithValidEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	event := validReaderEvent(
		time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC),
		1,
	)
	writeReaderLine(t, path, event, true)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{malformed}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := ReadTailEvents(dir, 1, DefaultTailReadMaxBytes)
	var segmentErr *SegmentReadError
	if !errors.As(err, &segmentErr) {
		t.Fatalf("error = %v, want SegmentReadError", err)
	}
	if errors.Is(err, ErrTailHistoryTruncated) {
		t.Fatalf("error unexpectedly reports byte truncation: %v", err)
	}
	if len(events) != 1 || events[0].EventID != event.EventID {
		t.Fatalf("events = %+v, want valid event before corrupt line", events)
	}
}

func validReaderEvent(completedAt time.Time, id int) EventV1 {
	event := validEventV1(completedAt)
	event.EventID = fmt.Sprintf("%032x", id)
	return event
}

func readerLineBytes(t *testing.T, value any) []byte {
	t.Helper()
	line, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

func writeReaderLine(t *testing.T, path string, value any, newline bool) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	line := readerLineBytes(t, value)
	if !newline {
		line = line[:len(line)-1]
	}
	if _, err := file.Write(line); err != nil {
		t.Fatal(err)
	}
}
