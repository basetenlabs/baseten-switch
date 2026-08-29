package tracecapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	DefaultRetentionDays         = 7
	DefaultMaxSegmentBytes int64 = 128 << 20
	DefaultMaxStoreBytes   int64 = 1 << 30
	DefaultMaxQueueBytes   int64 = 384 << 20
	DefaultMaxQueueRecords       = 64
	retentionSweepInterval       = 6 * time.Hour
)

type Config struct {
	Dir           string
	RetentionDays int
}

type EnqueueResult struct {
	Accepted bool
	Reason   string
}

type CloseResult struct {
	Drained        bool
	DroppedRecords uint64
	Error          string
}

type Status struct {
	State                 string
	StoreBytes            int64
	StoreLimitBytes       int64
	RetentionDays         int
	ActiveSegment         string
	LastSuccessfulWrite   *time.Time
	QueuedRecords         int
	QueuedBytesEstimate   int64
	DroppedRecords        map[string]uint64
	RecoveredPartialLines uint64
	LastError             string
}

type queuedTrace struct {
	trace    TraceV1
	estimate int64
	release  func()
}

type writerLimits struct {
	maxSegmentBytes    int64
	maxStoreBytes      int64
	maxQueueBytes      int64
	maxQueueRecords    int
	maxEncodedRowBytes int
}

func defaultWriterLimits() writerLimits {
	return writerLimits{
		maxSegmentBytes:    DefaultMaxSegmentBytes,
		maxStoreBytes:      DefaultMaxStoreBytes,
		maxQueueBytes:      DefaultMaxQueueBytes,
		maxQueueRecords:    DefaultMaxQueueRecords,
		maxEncodedRowBytes: MaxEncodedRowBytes,
	}
}

type Writer struct {
	mu            sync.Mutex
	dir           string
	retentionDays int
	now           func() time.Time
	limits        writerLimits

	lockFile      *os.File
	active        *os.File
	segment       Segment
	storeBytes    int64
	lastRetention time.Time

	queue         chan queuedTrace
	done          chan struct{}
	abort         chan struct{}
	abortOnce     sync.Once
	closeOnce     sync.Once
	accepting     bool
	queuedRecords int
	queuedBytes   int64
	health        Status
}

func NewWriter(config Config) (*Writer, error) {
	return newWriter(config, defaultWriterLimits(), time.Now)
}

func newWriter(config Config, limits writerLimits, now func() time.Time) (*Writer, error) {
	if config.RetentionDays <= 0 {
		config.RetentionDays = DefaultRetentionDays
	}
	if config.RetentionDays > 3650 {
		return nil, errors.New("trace retention days exceeds safety bound")
	}
	if limits.maxSegmentBytes <= 0 || limits.maxStoreBytes <= 0 ||
		limits.maxQueueBytes <= 0 || limits.maxQueueRecords <= 0 || limits.maxEncodedRowBytes <= 0 {
		return nil, errors.New("trace writer limits must be positive")
	}
	if limits.maxStoreBytes < limits.maxSegmentBytes+int64(limits.maxEncodedRowBytes) {
		return nil, errors.New("trace store limit must accommodate one segment and one maximum row")
	}
	if now == nil {
		now = time.Now
	}
	if err := ensurePrivateDirectory(config.Dir); err != nil {
		return nil, err
	}
	lockFile, err := acquireWriterLock(config.Dir)
	if err != nil {
		return nil, err
	}
	w := &Writer{
		dir: config.Dir, retentionDays: config.RetentionDays, now: now, limits: limits,
		lockFile: lockFile, queue: make(chan queuedTrace, limits.maxQueueRecords),
		done: make(chan struct{}), abort: make(chan struct{}), accepting: true,
	}
	w.health = Status{
		State: "enabled", StoreLimitBytes: limits.maxStoreBytes,
		RetentionDays: config.RetentionDays, DroppedRecords: make(map[string]uint64),
	}
	if err := w.initializeStore(); err != nil {
		_ = releaseWriterLock(lockFile)
		return nil, err
	}
	go w.run()
	return w, nil
}

func (w *Writer) initializeStore() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	segments, err := DiscoverSegments(w.dir)
	if err != nil {
		return err
	}
	for _, segment := range segments {
		w.storeBytes += segment.Size
	}
	if len(segments) > 0 {
		latest := segments[len(segments)-1]
		if err := w.openSegmentLocked(latest); err != nil {
			return err
		}
		if !latest.Day.Equal(utcDay(w.now())) {
			if err := w.closeActiveLocked(); err != nil {
				return err
			}
		}
	}
	w.runRetentionLocked(w.now())
	w.health.StoreBytes = w.storeBytes
	return nil
}

func (w *Writer) Enqueue(trace TraceV1) EnqueueResult {
	return w.EnqueueWithRelease(trace, nil)
}

// EnqueueWithRelease transfers trace-owned body memory to the writer. release
// runs exactly once after the record is written or dropped. A rejected enqueue
// does not take ownership and does not invoke release.
func (w *Writer) EnqueueWithRelease(trace TraceV1, release func()) EnqueueResult {
	estimate := estimateTraceBytes(trace)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.accepting {
		w.dropLocked("writer_closed", 1)
		return EnqueueResult{Reason: "writer_closed"}
	}
	if estimate > int64(w.limits.maxEncodedRowBytes) {
		w.dropLocked("row_too_large", 1)
		return EnqueueResult{Reason: "row_too_large"}
	}
	if w.queuedRecords >= w.limits.maxQueueRecords || w.queuedBytes+estimate > w.limits.maxQueueBytes {
		w.dropLocked("queue_full", 1)
		return EnqueueResult{Reason: "queue_full"}
	}
	item := queuedTrace{trace: trace, estimate: estimate, release: release}
	select {
	case w.queue <- item:
		w.queuedRecords++
		w.queuedBytes += estimate
		w.updateQueueHealthLocked()
		return EnqueueResult{Accepted: true}
	default:
		w.dropLocked("queue_full", 1)
		return EnqueueResult{Reason: "queue_full"}
	}
}

func estimateTraceBytes(trace TraceV1) int64 {
	const fixedEstimate = 64 << 10
	rawBytes := len(trace.Request.RawBody) + len(trace.Response.RawBody)
	encodedBytes := len(trace.Request.BodyBase64) + len(trace.Response.BodyBase64)
	return int64(encodedBytes+(rawBytes+2)/3*4) + fixedEstimate
}

func (w *Writer) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	status := w.health
	status.DroppedRecords = cloneCounts(w.health.DroppedRecords)
	return status
}

func (w *Writer) Close(ctx context.Context) CloseResult {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.accepting = false
		w.health.State = "disabling"
		close(w.queue)
		w.mu.Unlock()
	})
	select {
	case <-w.done:
		status := w.Status()
		return CloseResult{Drained: true, Error: status.LastError}
	case <-ctx.Done():
		w.abortOnce.Do(func() { close(w.abort) })
		select {
		case <-w.done:
			status := w.Status()
			return CloseResult{Drained: true, DroppedRecords: status.DroppedRecords["shutdown_deadline"], Error: status.LastError}
		default:
			return CloseResult{Error: ctx.Err().Error()}
		}
	}
}

func (w *Writer) run() {
	ticker := time.NewTicker(retentionSweepInterval)
	defer ticker.Stop()
	defer close(w.done)
	defer func() {
		w.mu.Lock()
		if err := w.closeActiveLocked(); err != nil {
			w.health.LastError = sanitizedError(err)
		}
		if err := releaseWriterLock(w.lockFile); err != nil && w.health.LastError == "" {
			w.health.LastError = sanitizedError(err)
		}
		w.lockFile = nil
		w.health.State = "disabled"
		w.mu.Unlock()
	}()
	for {
		select {
		case <-w.abort:
			w.dropQueuedForShutdown()
			return
		default:
		}
		select {
		case <-w.abort:
			w.dropQueuedForShutdown()
			return
		case <-ticker.C:
			w.mu.Lock()
			w.runRetentionLocked(w.now())
			w.mu.Unlock()
		case item, ok := <-w.queue:
			if !ok {
				return
			}
			w.mu.Lock()
			w.queuedRecords--
			w.queuedBytes -= item.estimate
			w.updateQueueHealthLocked()
			w.mu.Unlock()
			w.writeTrace(item.trace)
			if item.release != nil {
				item.release()
			}
		}
	}
}

func (w *Writer) dropQueuedForShutdown() {
	var dropped uint64
	for item := range w.queue {
		if item.release != nil {
			item.release()
		}
		dropped++
	}
	w.mu.Lock()
	w.queuedRecords = 0
	w.queuedBytes = 0
	w.updateQueueHealthLocked()
	w.dropLocked("shutdown_deadline", dropped)
	w.mu.Unlock()
}

func (w *Writer) writeTrace(trace TraceV1) {
	if err := trace.Validate(); err != nil {
		w.recordWriteFailure("invalid_record", err)
		return
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		w.recordWriteFailure("serialization", err)
		return
	}
	encoded = append(encoded, '\n')
	if len(encoded) > w.limits.maxEncodedRowBytes {
		w.recordWriteFailure("row_too_large", fmt.Errorf("encoded trace row exceeds limit"))
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.prepareAppendLocked(trace.CompletedAt, int64(len(encoded))); err != nil {
		w.dropLocked("storage", 1)
		w.health.LastError = sanitizedError(err)
		return
	}
	written, err := w.active.Write(encoded)
	if err == nil && written != len(encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = w.closeActiveLocked()
		w.dropLocked("storage", 1)
		w.health.LastError = sanitizedError(err)
		return
	}
	w.segment.Size += int64(written)
	w.storeBytes += int64(written)
	now := w.now().UTC()
	w.health.LastSuccessfulWrite = &now
	w.health.LastError = ""
	w.health.StoreBytes = w.storeBytes
	if w.lastRetention.IsZero() || now.Sub(w.lastRetention) >= retentionSweepInterval {
		w.runRetentionLocked(now)
	}
}

func (w *Writer) prepareAppendLocked(day time.Time, lineBytes int64) error {
	if lineBytes <= 0 || lineBytes > int64(w.limits.maxEncodedRowBytes) || lineBytes > w.limits.maxSegmentBytes {
		return errors.New("trace row exceeds storage bounds")
	}
	day = utcDay(day)
	rotate := w.active == nil || !w.segment.Day.Equal(day) ||
		(w.segment.Size > 0 && w.segment.Size+lineBytes > w.limits.maxSegmentBytes)
	if rotate {
		if err := w.closeActiveLocked(); err != nil {
			return err
		}
		if err := w.createSegmentLocked(day); err != nil {
			return err
		}
	}
	if err := w.evictForLocked(lineBytes); err != nil {
		return err
	}
	if w.storeBytes+lineBytes > w.limits.maxStoreBytes {
		return errors.New("trace store quota exceeded by active segment")
	}
	return nil
}

func (w *Writer) evictForLocked(lineBytes int64) error {
	if w.storeBytes+lineBytes <= w.limits.maxStoreBytes {
		return nil
	}
	segments, err := DiscoverSegments(w.dir)
	if err != nil {
		return err
	}
	for _, segment := range segments {
		if w.storeBytes+lineBytes <= w.limits.maxStoreBytes {
			break
		}
		if segment.Name == w.segment.Name {
			continue
		}
		if err := os.Remove(segment.Path); err != nil {
			return fmt.Errorf("evict closed trace segment: %w", err)
		}
		w.storeBytes -= segment.Size
	}
	w.health.StoreBytes = w.storeBytes
	return nil
}

func (w *Writer) createSegmentLocked(day time.Time) error {
	segments, err := DiscoverSegments(w.dir)
	if err != nil {
		return err
	}
	name, err := nextSegmentName(segments, day)
	if err != nil {
		return err
	}
	path := filepath.Join(w.dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("create trace segment: %w", err)
	}
	info, err := file.Stat()
	if err != nil || validatePrivateFileInfo(info, 0o600) != nil {
		_ = file.Close()
		_ = os.Remove(path)
		if err != nil {
			return fmt.Errorf("stat new trace segment: %w", err)
		}
		return errors.New("new trace segment is not private")
	}
	w.active = file
	w.segment = Segment{Name: name, Path: path, Day: utcDay(day), Sequence: parseSequence(name), Size: 0}
	w.health.ActiveSegment = name
	return nil
}

func parseSequence(name string) int {
	_, sequence, _ := parseSegmentName(name)
	return sequence
}

func (w *Writer) openSegmentLocked(segment Segment) error {
	file, openedInfo, err := openPrivateFileNoFollow(
		segment.Path,
		syscall.O_RDWR|syscall.O_APPEND,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("open active trace segment: %w", err)
	}
	before := openedInfo.Size()
	recovered, err := recoverFinalLine(file, w.limits.maxEncodedRowBytes)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("recover active trace segment: %w", err)
	}
	afterInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	segment.Size = afterInfo.Size()
	w.storeBytes += afterInfo.Size() - before
	w.active = file
	w.segment = segment
	w.health.ActiveSegment = segment.Name
	if recovered {
		w.health.RecoveredPartialLines++
	}
	return nil
}

func recoverFinalLine(file *os.File, maxLineBytes int) (bool, error) {
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return false, err
	}
	var final [1]byte
	if _, err := file.ReadAt(final[:], info.Size()-1); err != nil {
		return false, err
	}
	if final[0] == '\n' {
		return false, nil
	}
	window := int64(maxLineBytes)
	if info.Size() < window {
		window = info.Size()
	}
	buffer := make([]byte, window)
	if _, err := file.ReadAt(buffer, info.Size()-window); err != nil {
		return false, err
	}
	index := bytes.LastIndexByte(buffer, '\n')
	start := info.Size() - window + int64(index+1)
	if index < 0 && info.Size() > int64(maxLineBytes) {
		return false, errors.New("active trace segment final line exceeds maximum")
	}
	line := buffer[index+1:]
	var trace TraceV1
	valid := json.Unmarshal(line, &trace) == nil && trace.Validate() == nil
	if !valid {
		if err := file.Truncate(start); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := file.Write([]byte{'\n'}); err != nil {
		return false, err
	}
	return true, nil
}

func (w *Writer) closeActiveLocked() error {
	if w.active == nil {
		w.health.ActiveSegment = ""
		return nil
	}
	errSync := w.active.Sync()
	errClose := w.active.Close()
	w.active = nil
	w.segment = Segment{}
	w.health.ActiveSegment = ""
	return errors.Join(errSync, errClose)
}

func (w *Writer) runRetentionLocked(now time.Time) {
	removed, err := deleteExpiredSegments(w.dir, w.segment.Name, now.AddDate(0, 0, -w.retentionDays))
	if removed > 0 {
		w.storeBytes -= removed
	}
	w.lastRetention = now.UTC()
	w.health.StoreBytes = w.storeBytes
	if err != nil {
		w.health.LastError = sanitizedError(err)
	}
}

func (w *Writer) recordWriteFailure(reason string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropLocked(reason, 1)
	w.health.LastError = sanitizedError(err)
}

func (w *Writer) dropLocked(reason string, count uint64) {
	w.health.DroppedRecords[reason] += count
}

func (w *Writer) updateQueueHealthLocked() {
	w.health.QueuedRecords = w.queuedRecords
	w.health.QueuedBytesEstimate = w.queuedBytes
}

func cloneCounts(source map[string]uint64) map[string]uint64 {
	result := make(map[string]uint64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func sanitizedError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return "storage_full"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, ErrStoreLocked):
		return "store_locked"
	default:
		return "storage_error"
	}
}
