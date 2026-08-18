package tracecapture

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"syscall"
)

type SegmentReadError struct {
	MalformedLines int
	InvalidLines   int
}

func (e *SegmentReadError) Error() string {
	return fmt.Sprintf("trace segment contains %d malformed and %d invalid complete lines", e.MalformedLines, e.InvalidLines)
}

func ReadTraces(dir string) ([]TraceV1, error) {
	segments, err := DiscoverSegments(dir)
	if err != nil {
		return nil, err
	}
	var traces []TraceV1
	var readErrors []error
	for _, segment := range segments {
		values, _, readErr := ReadSegmentTraces(segment.Path, 0)
		traces = append(traces, values...)
		if readErr != nil {
			readErrors = append(readErrors, fmt.Errorf("%s: %w", segment.Name, readErr))
		}
	}
	return traces, errors.Join(readErrors...)
}

// ReadSegmentTraces reads complete rows up to maxBytes. A non-positive
// maxBytes snapshots the complete current file size. A final partial line is
// ignored rather than exposed as a trace.
func ReadSegmentTraces(path string, maxBytes int64) ([]TraceV1, int64, error) {
	file, openedInfo, err := openPrivateFileNoFollow(path, syscall.O_RDONLY, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("open trace segment: %w", err)
	}
	defer file.Close()
	readLimit := openedInfo.Size()
	if maxBytes > 0 && maxBytes < readLimit {
		readLimit = maxBytes
	}
	counting := &countingReader{reader: io.LimitReader(file, readLimit)}
	scanner := bufio.NewScanner(counting)
	scanner.Buffer(make([]byte, 256<<10), MaxEncodedRowBytes+1)
	scanner.Split(scanCompleteLines)
	var traces []TraceV1
	var rejected SegmentReadError
	for scanner.Scan() {
		line := scanner.Bytes()
		var trace TraceV1
		if json.Unmarshal(line, &trace) != nil {
			rejected.MalformedLines++
			continue
		}
		if err := trace.Validate(); err != nil {
			rejected.InvalidLines++
			continue
		}
		traces = append(traces, trace)
	}
	var resultErr error
	if err := scanner.Err(); err != nil {
		resultErr = fmt.Errorf("scan trace segment: %w", err)
	}
	if rejected.MalformedLines > 0 || rejected.InvalidLines > 0 {
		resultErr = errors.Join(resultErr, &rejected)
	}
	return traces, counting.count, resultErr
}

func scanCompleteLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for index, value := range data {
		if value == '\n' {
			if index == 0 {
				return index + 1, nil, nil
			}
			return index + 1, data[:index], nil
		}
	}
	if atEOF {
		return len(data), nil, nil
	}
	return 0, nil, nil
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}
