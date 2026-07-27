package telemetry

import (
	"bytes"
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
	DefaultMaxSegmentBytes int64 = 32 << 20
	DefaultRetentionDays         = 90
	writerLockName               = ".writer.lock"
	recoveryReadChunkBytes int64 = 64 << 10
)

// EventWriter is the storage boundary used by request capture.
type EventWriter interface {
	Write(EventV1) error
	Health() WriterHealth
	Close() error
}

type WriterOptions struct {
	Dir             string
	RetentionDays   int
	MaxSegmentBytes int64
	Now             func() time.Time
}

type WriterHealth struct {
	ActiveSegment         string
	LastWriteAt           *time.Time
	LastWriteError        string
	DroppedEvents         uint64
	RecoveredPartialLines uint64
	RetentionDays         int
	LastRetentionError    string
}

// Writer serializes telemetry v1 events into bounded monthly JSONL segments.
type Writer struct {
	mu sync.Mutex

	dir             string
	retentionDays   int
	maxSegmentBytes int64
	now             func() time.Time

	lockFile *os.File
	active   *os.File
	segment  Segment
	closed   bool

	lastRetentionDay string
	health           WriterHealth
}

var _ EventWriter = (*Writer)(nil)

func NewWriter(options WriterOptions) (*Writer, error) {
	if options.Dir == "" {
		return nil, errors.New("telemetry directory is required")
	}
	if options.RetentionDays <= 0 {
		options.RetentionDays = DefaultRetentionDays
	}
	if options.MaxSegmentBytes <= 0 {
		options.MaxSegmentBytes = DefaultMaxSegmentBytes
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if err := ensurePrivateDirectory(options.Dir); err != nil {
		return nil, err
	}
	lockFile, err := acquireWriterLock(options.Dir)
	if err != nil {
		return nil, err
	}
	writer := &Writer{
		dir:             options.Dir,
		retentionDays:   options.RetentionDays,
		maxSegmentBytes: options.MaxSegmentBytes,
		now:             options.Now,
		lockFile:        lockFile,
	}
	writer.health.RetentionDays = options.RetentionDays
	if err := writer.openLatestSegmentLocked(); err != nil {
		_ = writer.releaseLock()
		return nil, err
	}
	writer.runRetentionLocked(options.Now())
	return writer, nil
}

func ensurePrivateDirectory(dir string) error {
	if err := validateTelemetryDirectoryPath(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create telemetry directory %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat telemetry directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("telemetry directory %s must be a non-symlink directory", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure telemetry directory %s: %w", dir, err)
	}
	return nil
}

func validateTelemetryDirectoryPath(dir string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve telemetry directory %s: %w", dir, err)
	}
	absolute = filepath.Clean(absolute)
	root := filepath.VolumeName(absolute) + string(os.PathSeparator)
	unsafe := []string{root, os.TempDir()}
	if home, err := os.UserHomeDir(); err == nil {
		unsafe = append(unsafe, home)
	}
	for _, candidate := range unsafe {
		candidateAbsolute, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		if absolute == filepath.Clean(candidateAbsolute) {
			return fmt.Errorf("refusing unsafe telemetry directory %s", absolute)
		}
	}
	return nil
}

func acquireWriterLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, writerLockName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open telemetry writer lock %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure telemetry writer lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock telemetry directory %s: %w", dir, err)
	}
	return file, nil
}

func (w *Writer) Write(event EventV1) error {
	if err := event.Validate(); err != nil {
		return w.recordDropped(fmt.Errorf("validate telemetry event: %w", err))
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return w.recordDropped(fmt.Errorf("marshal telemetry event: %w", err))
	}
	encoded = append(encoded, '\n')
	if len(encoded) > MaxEventLineBytes {
		return w.recordDropped(fmt.Errorf(
			"encoded telemetry event is %d bytes; maximum is %d",
			len(encoded),
			MaxEventLineBytes,
		))
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.recordDroppedLocked(errors.New("telemetry writer is closed"))
	}
	eventMonth := event.CompletedAt.UTC()
	rotated, err := w.ensureSegmentLocked(eventMonth, int64(len(encoded)))
	if err != nil {
		return w.recordDroppedLocked(err)
	}
	written, err := w.active.Write(encoded)
	if err == nil && written != len(encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.closeActiveAfterErrorLocked()
		return w.recordDroppedLocked(fmt.Errorf("append telemetry event: %w", err))
	}
	w.segment.Size += int64(written)
	now := w.now().UTC()
	w.health.LastWriteAt = &now
	w.health.LastWriteError = ""
	if rotated || w.lastRetentionDay != utcDay(now) {
		w.runRetentionLocked(now)
	}
	return nil
}

func (w *Writer) ensureSegmentLocked(month time.Time, lineBytes int64) (bool, error) {
	if w.active == nil {
		if err := w.openLatestSegmentLocked(); err != nil {
			return false, err
		}
	}
	rotate := w.active == nil ||
		w.segment.monthKey() != month.Format("2006-01") ||
		(w.segment.Size > 0 && w.segment.Size+lineBytes > w.maxSegmentBytes)
	if !rotate {
		return false, nil
	}
	if err := w.closeActiveLocked(); err != nil {
		return false, err
	}
	if err := w.createSegmentLocked(month); err != nil {
		return false, err
	}
	return true, nil
}

func (w *Writer) openLatestSegmentLocked() error {
	segments, err := DiscoverSegments(w.dir)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		w.health.ActiveSegment = ""
		return nil
	}
	latest := segments[len(segments)-1]
	info, err := os.Lstat(latest.Path)
	if err != nil {
		return fmt.Errorf("stat active telemetry segment %s: %w", latest.Name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("active telemetry segment %s must be a regular non-symlink file", latest.Name)
	}
	if err := os.Chmod(latest.Path, 0o600); err != nil {
		return fmt.Errorf("secure active telemetry segment %s: %w", latest.Name, err)
	}
	file, err := os.OpenFile(latest.Path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open active telemetry segment %s: %w", latest.Name, err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat open active telemetry segment %s: %w", latest.Name, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("active telemetry segment %s changed while opening", latest.Name)
	}
	recovered, err := recoverFinalLine(file)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("recover active telemetry segment %s: %w", latest.Name, err)
	}
	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat active telemetry segment %s: %w", latest.Name, err)
	}
	latest.Size = stat.Size()
	w.active = file
	w.segment = latest
	w.health.ActiveSegment = latest.Name
	if recovered {
		w.health.RecoveredPartialLines++
	}
	return nil
}

func recoverFinalLine(file *os.File) (bool, error) {
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}

	var finalByte [1]byte
	if _, err := file.ReadAt(finalByte[:], info.Size()-1); err != nil {
		return false, err
	}
	hasFinalNewline := finalByte[0] == '\n'
	contentEnd := info.Size()
	if hasFinalNewline {
		contentEnd--
	}
	start, oversized, err := findFinalLineStart(file, contentEnd)
	if err != nil {
		return false, err
	}
	if oversized {
		return false, fmt.Errorf(
			"active telemetry segment final line exceeds %d bytes",
			MaxEventLineBytes,
		)
	}
	lineBytes := contentEnd - start
	encodedBytes := lineBytes + 1
	if encodedBytes > MaxEventLineBytes {
		return false, fmt.Errorf(
			"active telemetry segment final line exceeds %d bytes",
			MaxEventLineBytes,
		)
	}
	valid := lineBytes > 0
	if valid {
		line := make([]byte, int(lineBytes))
		if _, err := file.ReadAt(line, start); err != nil {
			return false, err
		}
		valid = json.Valid(line)
	}
	if !valid {
		if err := file.Truncate(start); err != nil {
			return false, err
		}
		return true, nil
	}
	if hasFinalNewline {
		return false, nil
	}
	written, err := file.Write([]byte{'\n'})
	if err != nil {
		return false, err
	}
	if written != 1 {
		return false, io.ErrShortWrite
	}
	return true, nil
}

func findFinalLineStart(
	file *os.File,
	contentEnd int64,
) (start int64, oversized bool, err error) {
	position := contentEnd
	lowerBound := max(int64(0), contentEnd-int64(MaxEventLineBytes))
	buffer := make([]byte, recoveryReadChunkBytes)
	for position > lowerBound {
		readSize := min(int64(len(buffer)), position-lowerBound)
		start := position - readSize
		n, err := file.ReadAt(buffer[:readSize], start)
		if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
			return 0, false, err
		}
		if newline := bytes.LastIndexByte(buffer[:n], '\n'); newline >= 0 {
			return start + int64(newline) + 1, false, nil
		}
		position = start
	}
	if lowerBound > 0 {
		return 0, true, nil
	}
	return 0, false, nil
}

func (w *Writer) createSegmentLocked(month time.Time) error {
	segments, err := DiscoverSegments(w.dir)
	if err != nil {
		return err
	}
	name, err := nextSegmentName(segments, month)
	if err != nil {
		return err
	}
	path := filepath.Join(w.dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("create telemetry segment %s: %w", name, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure telemetry segment %s: %w", name, err)
	}
	year, segmentMonth, sequence, _ := parseSegmentName(name)
	w.active = file
	w.segment = Segment{
		Name: name, Path: path, Year: year, Month: segmentMonth, Sequence: sequence,
	}
	w.health.ActiveSegment = name
	return nil
}

func (w *Writer) runRetentionLocked(now time.Time) {
	w.lastRetentionDay = utcDay(now)
	cutoff := now.UTC().Add(-time.Duration(w.retentionDays) * 24 * time.Hour)
	err := DeleteExpiredSegments(w.dir, w.health.ActiveSegment, cutoff)
	if err != nil {
		w.health.LastRetentionError = err.Error()
		return
	}
	w.health.LastRetentionError = ""
}

func utcDay(value time.Time) string {
	return value.UTC().Format("2006-01-02")
}

func (w *Writer) recordDropped(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recordDroppedLocked(err)
}

func (w *Writer) recordDroppedLocked(err error) error {
	w.health.DroppedEvents++
	w.health.LastWriteError = err.Error()
	return err
}

func (w *Writer) closeActiveAfterErrorLocked() {
	if w.active != nil {
		_ = w.active.Close()
		w.active = nil
	}
}

func (w *Writer) closeActiveLocked() error {
	if w.active == nil {
		return nil
	}
	if err := w.active.Sync(); err != nil {
		_ = w.active.Close()
		w.active = nil
		return fmt.Errorf("flush telemetry segment %s: %w", w.segment.Name, err)
	}
	if err := w.active.Close(); err != nil {
		w.active = nil
		return fmt.Errorf("close telemetry segment %s: %w", w.segment.Name, err)
	}
	w.active = nil
	return nil
}

func (w *Writer) Health() WriterHealth {
	w.mu.Lock()
	defer w.mu.Unlock()
	health := w.health
	if w.health.LastWriteAt != nil {
		value := *w.health.LastWriteAt
		health.LastWriteAt = &value
	}
	return health
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	closeErr := w.closeActiveLocked()
	lockErr := w.releaseLock()
	return errors.Join(closeErr, lockErr)
}

func (w *Writer) releaseLock() error {
	if w.lockFile == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(w.lockFile.Fd()), syscall.LOCK_UN)
	closeErr := w.lockFile.Close()
	w.lockFile = nil
	if unlockErr != nil {
		unlockErr = fmt.Errorf("unlock telemetry writer: %w", unlockErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close telemetry writer lock: %w", closeErr)
	}
	return errors.Join(unlockErr, closeErr)
}
