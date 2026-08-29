package tracecapture

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

const MaxSelectedTraceRows = 10_000

var (
	ErrSelectedTraceRowLimit  = errors.New("selected trace row limit exceeded")
	ErrSelectedTraceByteLimit = errors.New("selected trace retained byte limit exceeded")
	ErrTraceSnapshotChanged   = errors.New("trace store changed while taking snapshot")
)

type TraceSelection struct {
	Since                   time.Time
	Until                   time.Time
	Clients                 []string
	MaxRetainedEncodedBytes int64
}

// TraceSelectionStats contains no captured content, source paths, or native
// identifiers. Exclusion counters are mutually exclusive: time is evaluated
// before client.
type TraceSelectionStats struct {
	SegmentsSnapshot       int
	CompleteRows           int
	SelectedRows           int
	SelectedEncodedBytes   int64
	OutsideWindowRows      int
	OtherClientRows        int
	MalformedRows          int
	InvalidRows            int
	IncompleteTailSegments int
}

type traceSegmentSnapshot struct {
	segment Segment
	info    os.FileInfo
	end     int64
}

// ReadSelectedTraces streams a fixed, complete-line snapshot of the trace
// store. It retains only rows whose StartedAt is in [Since, Until) and whose
// client is selected. At most MaxSelectedTraceRows and
// MaxRetainedEncodedBytes are retained in memory.
func ReadSelectedTraces(
	dir string,
	selection TraceSelection,
) ([]TraceV1, TraceSelectionStats, error) {
	clients, err := validateTraceSelection(selection)
	if err != nil {
		return nil, TraceSelectionStats{}, err
	}
	snapshots, err := snapshotTraceSegments(dir)
	if err != nil {
		return nil, TraceSelectionStats{}, err
	}
	stats := TraceSelectionStats{SegmentsSnapshot: len(snapshots)}
	selected := make([]TraceV1, 0, min(len(snapshots)*16, MaxSelectedTraceRows))
	var readErrors []error
	for _, snapshot := range snapshots {
		err := streamSelectedSegment(
			snapshot,
			selection,
			clients,
			&selected,
			&stats,
		)
		if errors.Is(err, ErrSelectedTraceRowLimit) ||
			errors.Is(err, ErrSelectedTraceByteLimit) ||
			errors.Is(err, ErrTraceSnapshotChanged) {
			return selected, stats, err
		}
		if err != nil {
			readErrors = append(readErrors, fmt.Errorf("%s: %w", snapshot.segment.Name, err))
		}
	}
	return selected, stats, errors.Join(readErrors...)
}

func validateTraceSelection(selection TraceSelection) (map[string]struct{}, error) {
	if selection.Since.IsZero() || selection.Until.IsZero() ||
		!selection.Since.Before(selection.Until) {
		return nil, errors.New("trace selection must define a nonempty [since, until) interval")
	}
	if selection.MaxRetainedEncodedBytes <= 0 {
		return nil, errors.New("trace selection retained byte limit must be positive")
	}
	clients := make(map[string]struct{}, len(selection.Clients))
	for _, client := range selection.Clients {
		if client == "" {
			return nil, errors.New("trace selection client must not be empty")
		}
		clients[client] = struct{}{}
	}
	if len(clients) == 0 {
		return nil, errors.New("trace selection requires at least one client")
	}
	return clients, nil
}

func snapshotTraceSegments(dir string) ([]traceSegmentSnapshot, error) {
	segments, err := DiscoverSegments(dir)
	if err != nil {
		return nil, err
	}
	snapshots := make([]traceSegmentSnapshot, 0, len(segments))
	for _, segment := range segments {
		info, err := os.Lstat(segment.Path)
		if err != nil {
			return nil, errors.Join(ErrTraceSnapshotChanged, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errors.Join(ErrTraceSnapshotChanged, errors.New("recognized trace segment is unsafe"))
		}
		if err := validatePrivateFileInfo(info, 0o600); err != nil {
			return nil, err
		}
		snapshots = append(snapshots, traceSegmentSnapshot{
			segment: segment,
			info:    info,
			end:     info.Size(),
		})
	}
	verified, err := DiscoverSegments(dir)
	if err != nil {
		return nil, err
	}
	if len(verified) != len(snapshots) {
		return nil, ErrTraceSnapshotChanged
	}
	for index, segment := range verified {
		if segment.Name != snapshots[index].segment.Name {
			return nil, ErrTraceSnapshotChanged
		}
		info, err := os.Lstat(segment.Path)
		if err != nil || !os.SameFile(snapshots[index].info, info) || info.Size() < snapshots[index].end {
			return nil, ErrTraceSnapshotChanged
		}
	}
	return snapshots, nil
}

func streamSelectedSegment(
	snapshot traceSegmentSnapshot,
	selection TraceSelection,
	clients map[string]struct{},
	selected *[]TraceV1,
	stats *TraceSelectionStats,
) error {
	file, openedInfo, err := openPrivateFileNoFollow(
		snapshot.segment.Path,
		syscall.O_RDONLY,
		0o600,
	)
	if err != nil {
		return errors.Join(ErrTraceSnapshotChanged, err)
	}
	defer file.Close()
	if !os.SameFile(snapshot.info, openedInfo) || openedInfo.Size() < snapshot.end {
		return ErrTraceSnapshotChanged
	}
	if snapshot.end > 0 {
		var final [1]byte
		if _, err := file.ReadAt(final[:], snapshot.end-1); err != nil {
			return errors.Join(ErrTraceSnapshotChanged, err)
		}
		if final[0] != '\n' {
			stats.IncompleteTailSegments++
		}
	}
	readHash := sha256.New()
	counting := &countingReader{reader: io.TeeReader(io.LimitReader(file, snapshot.end), readHash)}
	scanner := bufio.NewScanner(counting)
	scanner.Buffer(make([]byte, 256<<10), MaxEncodedRowBytes+1)
	scanner.Split(scanCompleteLines)
	for scanner.Scan() {
		stats.CompleteRows++
		line := scanner.Bytes()
		if !json.Valid(line) {
			stats.MalformedRows++
			continue
		}
		trace, decodeErr := DecodeTraceV1Strict(line)
		if decodeErr != nil {
			stats.InvalidRows++
			continue
		}
		if trace.StartedAt.Before(selection.Since) || !trace.StartedAt.Before(selection.Until) {
			stats.OutsideWindowRows++
			continue
		}
		if _, ok := clients[trace.Client]; !ok {
			stats.OtherClientRows++
			continue
		}
		encodedBytes := int64(len(line) + 1)
		if len(*selected) >= MaxSelectedTraceRows {
			return fmt.Errorf("%w: maximum is %d", ErrSelectedTraceRowLimit, MaxSelectedTraceRows)
		}
		if stats.SelectedEncodedBytes+encodedBytes > selection.MaxRetainedEncodedBytes {
			return fmt.Errorf(
				"%w: maximum is %d bytes",
				ErrSelectedTraceByteLimit,
				selection.MaxRetainedEncodedBytes,
			)
		}
		*selected = append(*selected, trace)
		stats.SelectedRows++
		stats.SelectedEncodedBytes += encodedBytes
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan trace snapshot: %w", err)
	}
	if counting.count != snapshot.end {
		return ErrTraceSnapshotChanged
	}
	verify, verifyInfo, err := openPrivateFileNoFollow(snapshot.segment.Path, syscall.O_RDONLY, 0o600)
	if err != nil {
		return errors.Join(ErrTraceSnapshotChanged, err)
	}
	defer verify.Close()
	if !os.SameFile(snapshot.info, verifyInfo) || verifyInfo.Size() < snapshot.end {
		return ErrTraceSnapshotChanged
	}
	verifyHash := sha256.New()
	if copied, err := io.CopyN(verifyHash, verify, snapshot.end); err != nil || copied != snapshot.end {
		return errors.Join(ErrTraceSnapshotChanged, err)
	}
	if !bytes.Equal(readHash.Sum(nil), verifyHash.Sum(nil)) {
		return ErrTraceSnapshotChanged
	}
	return nil
}
