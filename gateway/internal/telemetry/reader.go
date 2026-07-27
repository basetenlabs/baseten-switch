package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	// MaxEventLineBytes is the largest encoded telemetry event, including its
	// trailing newline, accepted by both the writer and production readers.
	MaxEventLineBytes = 4 << 20
	// DefaultTailReadMaxBytes bounds diagnostic tail reads independently of
	// total retained telemetry size.
	DefaultTailReadMaxBytes int64 = 32 << 20
	tailReadChunkBytes      int64 = 64 << 10
	segmentReadBufferBytes        = 256 << 10
	segmentTypicalRowBytes  int64 = 1 << 10
	segmentPreallocMaxRows        = 100_000
)

// ErrTailHistoryTruncated reports that a bounded tail read exhausted its byte
// budget before it found the requested number of events, while older telemetry
// remained unread. Callers still receive every valid event found within the
// bounded window.
var ErrTailHistoryTruncated = errors.New(
	"telemetry tail history truncated by byte limit",
)

var openTelemetrySegment = os.Open

// SegmentReadError reports complete lines that could not be accepted as
// telemetry v1 events. Valid events from the same segment are still returned.
// The error is a durable coverage gap because the reader advances past each
// rejected newline-terminated line.
type SegmentReadError struct {
	MalformedLines int
	InvalidLines   int
}

func (e *SegmentReadError) Error() string {
	return fmt.Sprintf(
		"telemetry segment contains %d malformed and %d invalid complete lines",
		e.MalformedLines,
		e.InvalidLines,
	)
}

// ReadEvents reads every complete, valid telemetry v1 request event from the
// recognized segments in lexical order. A partial final line is ignored until
// a later read observes its terminating newline.
func ReadEvents(dir string) ([]EventV1, error) {
	segments, err := DiscoverSegments(dir)
	if err != nil {
		return nil, err
	}
	events := make([]EventV1, 0)
	var readErrors []error
	for _, segment := range segments {
		values, _, err := ReadSegmentEvents(segment.Path, 0)
		if err != nil {
			readErrors = append(readErrors, fmt.Errorf("%s: %w", segment.Name, err))
		}
		events = append(events, values...)
	}
	return events, errors.Join(readErrors...)
}

// ReadTailEvents reads at most limit complete, valid events from the newest
// recognized segments. It scans backward in bounded chunks, stops once limit
// events are found or maxBytes have been read, and returns events in their
// original chronological order. A partial final line is ignored.
func ReadTailEvents(dir string, limit int, maxBytes int64) ([]EventV1, error) {
	events, _, err := readTailEvents(dir, limit, maxBytes)
	return events, err
}

func readTailEvents(
	dir string,
	limit int,
	maxBytes int64,
) (events []EventV1, bytesRead int64, err error) {
	if limit <= 0 {
		return nil, 0, fmt.Errorf("telemetry tail limit must be positive")
	}
	if maxBytes <= 0 {
		return nil, 0, fmt.Errorf("telemetry tail byte limit must be positive")
	}
	segments, err := DiscoverSegments(dir)
	if err != nil {
		return nil, 0, err
	}

	newestFirst := make([]EventV1, 0, limit)
	for index := len(segments) - 1; index >= 0 &&
		len(newestFirst) < limit && bytesRead < maxBytes; index-- {
		values, segmentBytes, segmentTruncated, readErr := readSegmentTail(
			segments[index].Path,
			limit-len(newestFirst),
			maxBytes-bytesRead,
		)
		bytesRead += segmentBytes
		newestFirst = append(newestFirst, values...)
		byteCapTruncated := len(newestFirst) < limit &&
			bytesRead >= maxBytes &&
			(segmentTruncated || index > 0)
		if readErr != nil {
			reverseEvents(newestFirst)
			segmentErr := fmt.Errorf("%s: %w", segments[index].Name, readErr)
			if byteCapTruncated {
				return newestFirst, bytesRead,
					errors.Join(segmentErr, ErrTailHistoryTruncated)
			}
			return newestFirst, bytesRead, segmentErr
		}
		if byteCapTruncated {
			reverseEvents(newestFirst)
			return newestFirst, bytesRead, ErrTailHistoryTruncated
		}
	}
	reverseEvents(newestFirst)
	return newestFirst, bytesRead, nil
}

func reverseEvents(events []EventV1) {
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
}

// readSegmentTail returns newest-first events so callers can continue into
// older segments without buffering or sorting the complete store.
func readSegmentTail(
	path string,
	limit int,
	maxBytes int64,
) (events []EventV1, bytesRead int64, historyOmitted bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, false,
			fmt.Errorf("stat telemetry segment %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf(
			"telemetry segment %s is not a regular non-symlink file",
			path,
		)
	}
	file, err := openTelemetrySegment(path)
	if err != nil {
		return nil, 0, false,
			fmt.Errorf("open telemetry segment %s: %w", path, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, 0, false,
			fmt.Errorf("stat open telemetry segment %s: %w", path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, 0, false,
			fmt.Errorf("telemetry segment %s changed while opening", path)
	}

	position := openedInfo.Size()
	var carry []byte
	var rejected SegmentReadError
	discardPartialTail := false
	atFileEnd := true
	for position > 0 && bytesRead < maxBytes && len(events) < limit {
		readSize := min(tailReadChunkBytes, position, maxBytes-bytesRead)
		start := position - readSize
		chunk := make([]byte, readSize)
		n, readErr := file.ReadAt(chunk, start)
		bytesRead += int64(n)
		if readErr != nil && !(errors.Is(readErr, io.EOF) && n > 0) {
			return nil, bytesRead, position > 0,
				fmt.Errorf("read telemetry segment %s: %w", path, readErr)
		}
		chunk = chunk[:n]
		position = start

		if atFileEnd {
			atFileEnd = false
			if len(chunk) > 0 && chunk[len(chunk)-1] != '\n' {
				lastNewline := bytes.LastIndexByte(chunk, '\n')
				if lastNewline < 0 {
					discardPartialTail = true
					continue
				}
				chunk = chunk[:lastNewline+1]
			}
		} else if discardPartialTail {
			lastNewline := bytes.LastIndexByte(chunk, '\n')
			if lastNewline < 0 {
				continue
			}
			chunk = chunk[:lastNewline+1]
			discardPartialTail = false
		}
		if len(chunk) == 0 {
			continue
		}

		combined := make([]byte, 0, len(chunk)+len(carry))
		combined = append(combined, chunk...)
		combined = append(combined, carry...)
		var completeLines []byte
		if start == 0 {
			completeLines = combined
		} else {
			firstNewline := bytes.IndexByte(combined, '\n')
			if firstNewline < 0 {
				carry = combined
				continue
			}
			completeLines = combined[firstNewline+1:]
			carry = append(carry[:0], combined[:firstNewline+1]...)
		}
		events = appendReverseEvents(events, completeLines, limit, &rejected)
	}
	if rejected.MalformedLines > 0 || rejected.InvalidLines > 0 {
		return events, bytesRead, position > 0, &rejected
	}
	return events, bytesRead, position > 0, nil
}

func appendReverseEvents(
	events []EventV1,
	completeLines []byte,
	limit int,
	rejected *SegmentReadError,
) []EventV1 {
	lines := bytes.Split(completeLines, []byte{'\n'})
	for index := len(lines) - 1; index >= 0 && len(events) < limit; index-- {
		if len(lines[index]) == 0 {
			continue
		}
		if len(lines[index])+1 > MaxEventLineBytes {
			rejected.MalformedLines++
			continue
		}
		var event EventV1
		if json.Unmarshal(lines[index], &event) != nil {
			rejected.MalformedLines++
		} else if event.SchemaVersion != SchemaVersionV1 ||
			event.Event != EventRequest ||
			event.Validate() != nil {
			rejected.InvalidLines++
		} else {
			events = append(events, event)
		}
	}
	return events
}

// ReadSegmentEvents reads complete telemetry v1 lines beginning at offset.
// nextOffset advances only across newline-terminated lines, including
// malformed or unsupported lines. This lets incremental readers retry an
// incomplete final append without duplicating earlier events.
func ReadSegmentEvents(path string, offset int64) (events []EventV1, nextOffset int64, err error) {
	return readSegmentEvents(path, offset, -1)
}

// ReadSegmentEventsThrough is ReadSegmentEvents with an immutable exclusive
// end offset. It is used by cold bootstrap readers so appends after discovery
// cannot expand the accepted byte budget.
func ReadSegmentEventsThrough(
	path string,
	offset int64,
	end int64,
) (events []EventV1, nextOffset int64, err error) {
	if end < offset {
		return nil, offset, fmt.Errorf(
			"telemetry segment end %d precedes offset %d",
			end,
			offset,
		)
	}
	return readSegmentEvents(path, offset, end)
}

func readSegmentEvents(
	path string,
	offset int64,
	exclusiveEnd int64,
) (events []EventV1, nextOffset int64, err error) {
	if offset < 0 {
		return nil, offset, fmt.Errorf("telemetry segment offset must be nonnegative")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, offset, fmt.Errorf("stat telemetry segment %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, offset, fmt.Errorf("telemetry segment %s is not a regular non-symlink file", path)
	}
	file, err := openTelemetrySegment(path)
	if err != nil {
		return nil, offset, fmt.Errorf("open telemetry segment %s: %w", path, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, offset, fmt.Errorf("stat open telemetry segment %s: %w", path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, offset, fmt.Errorf("telemetry segment %s changed while opening", path)
	}
	end := min(info.Size(), openedInfo.Size())
	if exclusiveEnd >= 0 {
		end = min(end, exclusiveEnd)
	}
	if offset > end {
		return nil, offset, fmt.Errorf(
			"telemetry segment offset %d exceeds size %d",
			offset,
			end,
		)
	}
	remainingBytes := end - offset
	if remainingBytes > 0 {
		estimatedRows := min(
			remainingBytes/segmentTypicalRowBytes+1,
			int64(segmentPreallocMaxRows),
		)
		events = make([]EventV1, 0, int(estimatedRows))
	}

	nextOffset = offset
	reader := bufio.NewReaderSize(
		io.NewSectionReader(file, offset, remainingBytes),
		segmentReadBufferBytes,
	)
	var rejected SegmentReadError
	var longLine []byte
	lineBytes := 0
	oversized := false
	for {
		fragment, readErr := reader.ReadSlice('\n')
		lineBytes += len(fragment)
		if errors.Is(readErr, bufio.ErrBufferFull) {
			if !oversized && lineBytes > MaxEventLineBytes {
				oversized = true
				longLine = nil
			} else if !oversized {
				longLine = append(longLine, fragment...)
			}
			continue
		}

		line := fragment
		if !oversized && len(longLine) > 0 {
			longLine = append(longLine, fragment...)
			line = longLine
		}
		complete := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if complete {
			nextOffset += int64(lineBytes)
			if oversized || lineBytes > MaxEventLineBytes {
				rejected.MalformedLines++
			} else {
				var event EventV1
				if json.Unmarshal(line[:len(line)-1], &event) != nil {
					rejected.MalformedLines++
				} else if event.SchemaVersion != SchemaVersionV1 ||
					event.Event != EventRequest ||
					event.Validate() != nil {
					rejected.InvalidLines++
				} else {
					events = append(events, event)
				}
			}
		}
		longLine = longLine[:0]
		lineBytes = 0
		oversized = false
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				fatal := fmt.Errorf("read telemetry segment %s: %w", path, readErr)
				if rejected.MalformedLines > 0 || rejected.InvalidLines > 0 {
					return events, nextOffset, errors.Join(&rejected, fatal)
				}
				return events, nextOffset, fatal
			}
			if rejected.MalformedLines > 0 || rejected.InvalidLines > 0 {
				return events, nextOffset, &rejected
			}
			return events, nextOffset, nil
		}
	}
}
