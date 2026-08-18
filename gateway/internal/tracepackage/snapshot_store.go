package tracepackage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"slices"
	"sync"

	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
)

const snapshotTailChunkBytes int64 = 64 << 10

type discoveredSegment struct {
	Name string
	Path string
}

type capturedSegment struct {
	Name         string
	Path         string
	Identity     os.FileInfo
	CapturedSize int64
	ReadSize     int64
}

type discoverStoreFunc func(string) ([]discoveredSegment, error)

// TraceStoreSnapshot captures a stable fixed-offset view of recognized trace
// segments. It retries discovery once when rotation or retention races the
// initial capture.
func TraceStoreSnapshot(ctx context.Context, dir string) (Snapshot, error) {
	return snapshotStore(ctx, dir, discoverTraceSegments)
}

// TelemetryStoreSnapshot captures a stable fixed-offset view of recognized
// telemetry segments. The packager performs exact event-ID filtering later.
func TelemetryStoreSnapshot(ctx context.Context, dir string) (Snapshot, error) {
	return snapshotStore(ctx, dir, discoverTelemetrySegments)
}

// TraceStoreSource adapts a runtime trace directory to Sources.Traces.
func TraceStoreSource(dir string) TraceSnapshotFunc {
	return func(ctx context.Context, _ Selection) (Snapshot, error) {
		return TraceStoreSnapshot(ctx, dir)
	}
}

// TelemetryStoreSource adapts a runtime telemetry directory to
// Sources.Telemetry. The exact join remains enforced inside Create.
func TelemetryStoreSource(dir string) TelemetrySnapshotFunc {
	return func(
		ctx context.Context,
		_ Selection,
		_ []string,
	) (Snapshot, error) {
		return TelemetryStoreSnapshot(ctx, dir)
	}
}

func discoverTraceSegments(dir string) ([]discoveredSegment, error) {
	segments, err := tracecapture.DiscoverSegments(dir)
	if err != nil {
		return nil, err
	}
	result := make([]discoveredSegment, 0, len(segments))
	for _, segment := range segments {
		result = append(result, discoveredSegment{Name: segment.Name, Path: segment.Path})
	}
	return result, nil
}

func discoverTelemetrySegments(dir string) ([]discoveredSegment, error) {
	segments, err := telemetry.DiscoverSegments(dir)
	if err != nil {
		return nil, err
	}
	result := make([]discoveredSegment, 0, len(segments))
	for _, segment := range segments {
		result = append(result, discoveredSegment{Name: segment.Name, Path: segment.Path})
	}
	return result, nil
}

func snapshotStore(
	ctx context.Context,
	dir string,
	discover discoverStoreFunc,
) (Snapshot, error) {
	if err := validateSnapshotRoot(dir); err != nil {
		return Snapshot{}, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		discovered, err := discover(dir)
		if err != nil {
			return Snapshot{}, fmt.Errorf("discover snapshot segments: %w", err)
		}
		captured, total, err := captureSegments(ctx, discovered)
		if err != nil {
			if errors.Is(err, ErrSnapshotChanged) {
				lastErr = err
				continue
			}
			return Snapshot{}, err
		}
		if err := verifySegmentSet(ctx, dir, discover, captured, nil); err != nil {
			if errors.Is(err, ErrSnapshotChanged) {
				lastErr = err
				continue
			}
			return Snapshot{}, err
		}
		return buildStoreSnapshot(dir, discover, captured, total), nil
	}
	return Snapshot{}, fmt.Errorf("%w after retry: %v", ErrSnapshotChanged, lastErr)
}

func validateSnapshotRoot(dir string) error {
	if dir == "" {
		return errors.New("trace package: snapshot store path is empty")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect snapshot store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("trace package: snapshot store must be a non-symlink directory")
	}
	return nil
}

func captureSegments(
	ctx context.Context,
	discovered []discoveredSegment,
) ([]capturedSegment, int64, error) {
	captured := make([]capturedSegment, 0, len(discovered))
	var total int64
	for _, segment := range discovered {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		file, info, err := openCapturedSegment(segment.Path)
		if err != nil {
			return nil, 0, err
		}
		readSize, boundaryErr := lastCompleteLineOffset(file, info.Size())
		closeErr := file.Close()
		if boundaryErr != nil || closeErr != nil {
			return nil, 0, fmt.Errorf(
				"capture segment boundary: %w",
				errors.Join(boundaryErr, closeErr),
			)
		}
		if readSize > MaxSourceBytes-total {
			return nil, 0, fmt.Errorf("%w: snapshot source bytes", ErrLimitExceeded)
		}
		total += readSize
		captured = append(captured, capturedSegment{
			Name:         segment.Name,
			Path:         segment.Path,
			Identity:     info,
			CapturedSize: info.Size(),
			ReadSize:     readSize,
		})
	}
	return captured, total, nil
}

func openCapturedSegment(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect snapshot segment: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, errors.New("trace package: snapshot segment is not a regular non-symlink file")
	}
	file, err := openNoFollow(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open snapshot segment without following links: %w", err)
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: segment changed while opening", ErrSnapshotChanged)
	}
	return file, after, nil
}

func lastCompleteLineOffset(file *os.File, size int64) (int64, error) {
	if size <= 0 {
		return 0, nil
	}
	buffer := make([]byte, snapshotTailChunkBytes)
	for end := size; end > 0; {
		start := max(int64(0), end-int64(len(buffer)))
		chunk := buffer[:end-start]
		n, err := file.ReadAt(chunk, start)
		if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
			return 0, err
		}
		for index := n - 1; index >= 0; index-- {
			if chunk[index] == '\n' {
				return start + int64(index) + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}

type snapshotState struct {
	mu      sync.Mutex
	readers []*multiSegmentReader
}

func buildStoreSnapshot(
	dir string,
	discover discoverStoreFunc,
	segments []capturedSegment,
	total int64,
) Snapshot {
	state := &snapshotState{}
	return Snapshot{
		EstimatedBytes: total,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			reader := &multiSegmentReader{
				ctx:      ctx,
				segments: slices.Clone(segments),
				hashes:   make(map[string][sha256.Size]byte, len(segments)),
			}
			state.mu.Lock()
			state.readers = append(state.readers, reader)
			state.mu.Unlock()
			return reader, nil
		},
		Verify: func(ctx context.Context) error {
			state.mu.Lock()
			readers := slices.Clone(state.readers)
			state.mu.Unlock()
			if len(readers) == 0 {
				return errors.New("snapshot was not opened")
			}
			for _, reader := range readers {
				hashes, complete := reader.readHashes()
				if !complete {
					return errors.New("snapshot was not read to its fixed boundary")
				}
				if err := verifySegmentSet(ctx, dir, discover, segments, hashes); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

type multiSegmentReader struct {
	ctx      context.Context
	segments []capturedSegment

	mu        sync.Mutex
	index     int
	file      *os.File
	remaining int64
	hasher    hash.Hash
	hashes    map[string][sha256.Size]byte
	closed    bool
	failed    bool
}

func (r *multiSegmentReader) Read(destination []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	if len(destination) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			r.failed = true
			return 0, err
		}
		if r.index >= len(r.segments) {
			return 0, io.EOF
		}
		if r.file == nil {
			if err := r.openCurrent(); err != nil {
				r.failed = true
				return 0, err
			}
			if r.remaining == 0 {
				if err := r.finishCurrent(); err != nil {
					r.failed = true
					return 0, err
				}
				continue
			}
		}
		limit := len(destination)
		if int64(limit) > r.remaining {
			limit = int(r.remaining)
		}
		n, err := r.file.Read(destination[:limit])
		if n > 0 {
			_, _ = r.hasher.Write(destination[:n])
			r.remaining -= int64(n)
			if r.remaining == 0 {
				if finishErr := r.finishCurrent(); finishErr != nil {
					r.failed = true
					return n, finishErr
				}
			}
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			r.failed = true
			return 0, io.ErrUnexpectedEOF
		}
		if err != nil {
			r.failed = true
			return 0, err
		}
	}
}

func (r *multiSegmentReader) openCurrent() error {
	segment := r.segments[r.index]
	file, info, err := openCapturedSegment(segment.Path)
	if err != nil {
		return err
	}
	if !os.SameFile(segment.Identity, info) || info.Size() < segment.CapturedSize {
		_ = file.Close()
		return fmt.Errorf("%w: segment identity or size changed", ErrSnapshotChanged)
	}
	r.file = file
	r.remaining = segment.ReadSize
	r.hasher = sha256.New()
	return nil
}

func (r *multiSegmentReader) finishCurrent() error {
	segment := r.segments[r.index]
	var digest [sha256.Size]byte
	copy(digest[:], r.hasher.Sum(nil))
	r.hashes[segment.Name] = digest
	err := r.file.Close()
	r.file = nil
	r.hasher = nil
	r.remaining = 0
	r.index++
	return err
}

func (r *multiSegmentReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

func (r *multiSegmentReader) readHashes() (map[string][sha256.Size]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string][sha256.Size]byte, len(r.hashes))
	for name, digest := range r.hashes {
		result[name] = digest
	}
	return result, !r.failed && r.index == len(r.segments)
}

func verifySegmentSet(
	ctx context.Context,
	dir string,
	discover discoverStoreFunc,
	captured []capturedSegment,
	readHashes map[string][sha256.Size]byte,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSnapshotRoot(dir); err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotChanged, err)
	}
	discovered, err := discover(dir)
	if err != nil {
		return fmt.Errorf("verify snapshot discovery: %w", err)
	}
	if len(discovered) != len(captured) {
		return fmt.Errorf("%w: segment set changed", ErrSnapshotChanged)
	}
	for index, segment := range captured {
		if discovered[index].Name != segment.Name {
			return fmt.Errorf("%w: segment set changed", ErrSnapshotChanged)
		}
		file, info, err := openCapturedSegment(discovered[index].Path)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrSnapshotChanged, err)
		}
		if !os.SameFile(segment.Identity, info) || info.Size() < segment.CapturedSize {
			_ = file.Close()
			return fmt.Errorf("%w: segment identity or size changed", ErrSnapshotChanged)
		}
		if readHashes != nil {
			expected, present := readHashes[segment.Name]
			if !present {
				_ = file.Close()
				return fmt.Errorf("%w: segment was not completely read", ErrSnapshotChanged)
			}
			actual, hashErr := hashPrefix(file, segment.ReadSize)
			if hashErr != nil || actual != expected {
				_ = file.Close()
				return fmt.Errorf("%w: segment content changed within captured boundary", ErrSnapshotChanged)
			}
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close verified snapshot segment: %w", err)
		}
	}
	return nil
}

func hashPrefix(file *os.File, size int64) ([sha256.Size]byte, error) {
	hash := sha256.New()
	if size > 0 {
		if _, err := io.CopyN(hash, file, size); err != nil {
			return [sha256.Size]byte{}, err
		}
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}
