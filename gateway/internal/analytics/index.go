package analytics

import (
	"bytes"
	"errors"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

const (
	DefaultMaxAge            = 30 * 24 * time.Hour
	DefaultMaxRows           = 100_000
	DefaultBootstrapMaxBytes = int64(128 << 20)
	indexCheckpointBytes     = int64(64)
	indexBootstrapWorkers    = 8
	indexReadBufferBytes     = 256 << 10
)

var (
	indexDiscoverSegments         = telemetry.DiscoverSegments
	indexReadSegmentEvents        = telemetry.ReadSegmentEvents
	indexReadSegmentEventsThrough = telemetry.ReadSegmentEventsThrough
	indexStat                     = os.Lstat
)

type IndexOptions struct {
	MaxAge            time.Duration
	MaxRows           int
	BootstrapMaxBytes int64
}

type Snapshot struct {
	// Generation advances whenever indexed events or coverage state changes.
	// Consumers may use it as a cache validator for this Index instance.
	Generation uint64
	// Events is immutable index-owned data. Consumers must not modify the
	// slice or its elements.
	Events   []telemetry.EventV1
	Complete bool
	Reason   string
	Earliest time.Time
	Latest   time.Time
}

type segmentCursor struct {
	segment          telemetry.Segment
	pos              int64
	fileInfo         os.FileInfo
	checkpointOffset int64
	checkpoint       []byte
	events           []telemetry.EventV1
	durableGap       string
	readError        string
	coldEnd          *int64
}

// Index incrementally follows every recognized telemetry v1 segment. Closed
// segments remain indexed while the active segment is consumed from its last
// complete newline. Filesystem work is lazy and serialized by mu.
type Index struct {
	mu sync.Mutex

	opts IndexOptions
	dir  string

	cursors map[string]*segmentCursor
	started bool

	byteCapped          bool
	rowDiscardedThrough time.Time
	ageDiscardedThrough time.Time
	discoveryError      string
	generation          uint64
	combinedGeneration  uint64
	combined            []telemetry.EventV1
}

func NewIndex(opts IndexOptions) *Index {
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultMaxAge
	}
	if opts.MaxRows <= 0 {
		opts.MaxRows = DefaultMaxRows
	}
	if opts.BootstrapMaxBytes <= 0 {
		opts.BootstrapMaxBytes = DefaultBootstrapMaxBytes
	}
	return &Index{opts: opts, cursors: map[string]*segmentCursor{}}
}

func (i *Index) Snapshot(dir string, now time.Time, since int64) Snapshot {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.dir != dir {
		i.reset(dir)
	}
	i.ensure(now.UTC())

	events := i.snapshotEvents()
	reasons := make([]string, 0, 3)
	if i.byteCapped && !eventsCoverSince(events, since) {
		reasons = append(reasons, "bootstrap byte cap")
	}
	if !i.rowDiscardedThrough.IsZero() &&
		i.rowDiscardedThrough.Unix() >= since {
		reasons = append(reasons, "row cap")
	}
	if since < now.Add(-i.opts.MaxAge).Unix() ||
		(!i.ageDiscardedThrough.IsZero() && i.ageDiscardedThrough.Unix() >= since) {
		reasons = append(reasons, "age cap")
	}
	hasCorruption := false
	hasReadError := i.discoveryError != ""
	for _, cursor := range i.cursors {
		hasCorruption = hasCorruption || cursor.durableGap != ""
		hasReadError = hasReadError || cursor.readError != ""
	}
	if hasCorruption {
		reasons = append(reasons, "segment corruption")
	}
	if hasReadError {
		reasons = append(reasons, "telemetry read error")
	}

	snapshot := Snapshot{
		Generation: i.generation,
		Events:     events,
		Complete:   len(reasons) == 0,
		Reason:     strings.Join(reasons, ", "),
	}
	if len(events) > 0 {
		snapshot.Earliest = events[0].CompletedAt
		snapshot.Latest = events[len(events)-1].CompletedAt
	}
	return snapshot
}

func (i *Index) reset(dir string) {
	i.generation++
	i.dir = dir
	i.cursors = map[string]*segmentCursor{}
	i.started = false
	i.byteCapped = false
	i.rowDiscardedThrough = time.Time{}
	i.ageDiscardedThrough = time.Time{}
	i.discoveryError = ""
	i.combinedGeneration = 0
	i.combined = nil
}

func (i *Index) ensure(now time.Time) {
	segments, err := indexDiscoverSegments(i.dir)
	if err != nil {
		value := err.Error()
		if i.discoveryError != value {
			i.discoveryError = value
			i.generation++
		}
		return
	}
	if i.discoveryError != "" {
		i.discoveryError = ""
		i.generation++
	}
	cutoff := now.Add(-i.opts.MaxAge)
	filtered := segments[:0]
	for _, segment := range segments {
		monthEnd := time.Date(
			segment.Year,
			segment.Month+1,
			1,
			0,
			0,
			0,
			0,
			time.UTC,
		)
		if monthEnd.After(cutoff) {
			filtered = append(filtered, segment)
		}
	}
	segments = filtered

	live := make(map[string]bool, len(segments))
	for _, segment := range segments {
		live[segment.Name] = true
	}
	for name := range i.cursors {
		if !live[name] {
			delete(i.cursors, name)
			i.generation++
		}
	}

	starts := map[string]int64{}
	coldBootstrap := !i.started
	if coldBootstrap {
		remaining := i.opts.BootstrapMaxBytes
		for index := len(segments) - 1; index >= 0; index-- {
			segment := segments[index]
			switch {
			case remaining <= 0:
				starts[segment.Name] = segment.Size
				if !i.byteCapped {
					i.byteCapped = true
					i.generation++
				}
			case segment.Size > remaining:
				starts[segment.Name] = segment.Size - remaining
				remaining = 0
				if !i.byteCapped {
					i.byteCapped = true
					i.generation++
				}
			default:
				remaining -= segment.Size
			}
		}
		i.started = true
	}

	cursors := make([]*segmentCursor, 0, len(segments))
	for _, segment := range segments {
		cursor := i.cursors[segment.Name]
		if cursor == nil {
			cursor = &segmentCursor{segment: segment}
			if coldBootstrap {
				end := segment.Size
				cursor.coldEnd = &end
			}
			i.cursors[segment.Name] = cursor
			i.generation++
			if start := starts[segment.Name]; start > 0 {
				cursor.pos = discardPartialPrefix(
					segment.Path,
					start,
					segment.Size,
				)
			}
		} else {
			cursor.segment = segment
		}
		cursors = append(cursors, cursor)
	}
	refreshed := i.refreshCursors(cursors, coldBootstrap)
	for index, cursor := range cursors {
		if refreshed[index] {
			i.generation++
		}
		if i.pruneCursor(cursor, cutoff) {
			i.generation++
		}
	}
	if i.applyRowCap() {
		i.generation++
	}
}

func (i *Index) refreshCursors(
	cursors []*segmentCursor,
	parallel bool,
) []bool {
	changed := make([]bool, len(cursors))
	workers := min(len(cursors), runtime.GOMAXPROCS(0), indexBootstrapWorkers)
	if !parallel || workers <= 1 {
		for index, cursor := range cursors {
			changed[index] = i.refreshCursor(cursor)
		}
		return changed
	}

	jobs := make(chan int)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for index := range jobs {
				changed[index] = i.refreshCursor(cursors[index])
			}
		}()
	}
	for index := range cursors {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	return changed
}

func discardPartialPrefix(path string, offset, end int64) int64 {
	info, err := os.Lstat(path)
	if err != nil ||
		info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return offset
	}
	file, err := os.Open(path)
	if err != nil {
		return offset
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil ||
		!openedInfo.Mode().IsRegular() ||
		!os.SameFile(info, openedInfo) {
		return offset
	}
	end = min(end, info.Size(), openedInfo.Size())
	if offset >= end {
		return end
	}
	if offset > 0 {
		var previous [1]byte
		if _, err := file.ReadAt(previous[:], offset-1); err == nil && previous[0] == '\n' {
			return offset
		}
	}
	reader := io.NewSectionReader(file, offset, end-offset)
	buffer := make([]byte, indexReadBufferBytes)
	for offset < end {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if newline := bytes.IndexByte(buffer[:n], '\n'); newline >= 0 {
				return offset + int64(newline) + 1
			}
			offset += int64(n)
		}
		if readErr != nil {
			return offset
		}
	}
	return offset
}

func (i *Index) refreshCursor(cursor *segmentCursor) bool {
	originalEventCount := len(cursor.events)
	originalDurableGap := cursor.durableGap
	originalReadError := cursor.readError
	changed := false

	info, err := indexStat(cursor.segment.Path)
	if err != nil {
		cursor.readError = err.Error()
		return cursor.readError != originalReadError
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		cursor.readError = "telemetry segment is not a regular non-symlink file"
		return cursor.readError != originalReadError
	}
	replaced := cursor.fileInfo != nil &&
		(!os.SameFile(cursor.fileInfo, info) || info.Size() < cursor.pos)
	if !replaced && cursor.fileInfo != nil {
		var checkpointErr error
		replaced, checkpointErr = cursorCheckpointChanged(cursor)
		if checkpointErr != nil {
			cursor.readError = checkpointErr.Error()
			return cursor.readError != originalReadError
		}
	}
	if replaced {
		changed = true
		cursor.pos = 0
		cursor.fileInfo = nil
		cursor.checkpoint = nil
		cursor.checkpointOffset = 0
		cursor.events = nil
		cursor.durableGap = ""
		cursor.readError = ""
	}

	var events []telemetry.EventV1
	var nextOffset int64
	if cursor.coldEnd != nil {
		events, nextOffset, err = indexReadSegmentEventsThrough(
			cursor.segment.Path,
			cursor.pos,
			*cursor.coldEnd,
		)
		if err == nil || onlySegmentReadErrors(err) {
			cursor.coldEnd = nil
		}
	} else {
		events, nextOffset, err = indexReadSegmentEvents(
			cursor.segment.Path,
			cursor.pos,
		)
	}
	if len(cursor.events) == 0 {
		cursor.events = events
	} else {
		cursor.events = append(cursor.events, events...)
	}
	cursor.pos = nextOffset
	cursor.fileInfo = info
	updateCursorCheckpoint(cursor)
	if err == nil {
		cursor.readError = ""
		return changed ||
			len(cursor.events) != originalEventCount ||
			cursor.durableGap != originalDurableGap ||
			cursor.readError != originalReadError
	}
	var gap *telemetry.SegmentReadError
	if errors.As(err, &gap) {
		cursor.durableGap = gap.Error()
		if onlySegmentReadErrors(err) {
			cursor.readError = ""
			return changed ||
				len(cursor.events) != originalEventCount ||
				cursor.durableGap != originalDurableGap ||
				cursor.readError != originalReadError
		}
	}
	cursor.readError = err.Error()
	return changed ||
		len(cursor.events) != originalEventCount ||
		cursor.durableGap != originalDurableGap ||
		cursor.readError != originalReadError
}

func onlySegmentReadErrors(err error) bool {
	switch value := err.(type) {
	case *telemetry.SegmentReadError:
		return true
	case interface{ Unwrap() []error }:
		children := value.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlySegmentReadErrors(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		child := value.Unwrap()
		return child != nil && onlySegmentReadErrors(child)
	default:
		return false
	}
}

func cursorCheckpointChanged(cursor *segmentCursor) (bool, error) {
	if len(cursor.checkpoint) == 0 {
		return false, nil
	}
	file, err := openCursorFile(cursor.segment.Path, cursor.fileInfo)
	if err != nil {
		return false, err
	}
	defer file.Close()
	current := make([]byte, len(cursor.checkpoint))
	if _, err := file.ReadAt(current, cursor.checkpointOffset); err != nil {
		return false, err
	}
	return !bytes.Equal(current, cursor.checkpoint), nil
}

func updateCursorCheckpoint(cursor *segmentCursor) {
	size := indexCheckpointBytes
	if cursor.pos < size {
		size = cursor.pos
	}
	if size <= 0 {
		cursor.checkpointOffset = 0
		cursor.checkpoint = nil
		return
	}
	file, err := openCursorFile(cursor.segment.Path, cursor.fileInfo)
	if err != nil {
		return
	}
	defer file.Close()
	offset := cursor.pos - size
	checkpoint := make([]byte, int(size))
	if _, err := file.ReadAt(checkpoint, offset); err != nil {
		return
	}
	cursor.checkpointOffset = offset
	cursor.checkpoint = checkpoint
}

func openCursorFile(path string, expected os.FileInfo) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("telemetry segment is not a regular non-symlink file")
	}
	if expected != nil && !os.SameFile(expected, info) {
		return nil, errors.New("telemetry segment changed before opening")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, errors.New("telemetry segment changed while opening")
	}
	return file, nil
}

func (i *Index) pruneCursor(cursor *segmentCursor, cutoff time.Time) bool {
	originalLength := len(cursor.events)
	originalDiscardedThrough := i.ageDiscardedThrough
	retained := cursor.events[:0]
	for _, event := range cursor.events {
		if event.CompletedAt.Before(cutoff) {
			if event.CompletedAt.After(i.ageDiscardedThrough) {
				i.ageDiscardedThrough = event.CompletedAt
			}
			continue
		}
		retained = append(retained, event)
	}
	cursor.events = retained
	return len(cursor.events) != originalLength ||
		i.ageDiscardedThrough != originalDiscardedThrough
}

func (i *Index) combinedEvents() []telemetry.EventV1 {
	eventCount := 0
	for _, cursor := range i.cursors {
		eventCount += len(cursor.events)
	}
	events := make([]telemetry.EventV1, 0, eventCount)
	for _, cursor := range i.cursors {
		events = append(events, cursor.events...)
	}
	sort.SliceStable(events, func(left, right int) bool {
		if events[left].CompletedAt.Equal(events[right].CompletedAt) {
			return events[left].EventID < events[right].EventID
		}
		return events[left].CompletedAt.Before(events[right].CompletedAt)
	})
	return events
}

func (i *Index) snapshotEvents() []telemetry.EventV1 {
	if i.combinedGeneration == i.generation && i.combined != nil {
		return i.combined
	}
	i.combined = i.combinedEvents()
	i.combinedGeneration = i.generation
	return i.combined
}

func (i *Index) applyRowCap() bool {
	eventCount := 0
	for _, cursor := range i.cursors {
		eventCount += len(cursor.events)
	}
	if eventCount <= i.opts.MaxRows {
		return false
	}
	events := i.combinedEvents()
	excess := eventCount - i.opts.MaxRows
	discard := make(map[string]bool, excess)
	for _, event := range events[:excess] {
		discard[event.EventID] = true
		if event.CompletedAt.After(i.rowDiscardedThrough) {
			i.rowDiscardedThrough = event.CompletedAt
		}
	}
	for _, cursor := range i.cursors {
		retained := cursor.events[:0]
		for _, event := range cursor.events {
			if !discard[event.EventID] {
				retained = append(retained, event)
			}
		}
		cursor.events = retained
	}
	return true
}

func eventsCoverSince(events []telemetry.EventV1, since int64) bool {
	for _, event := range events {
		if event.CompletedAt.Unix() <= since {
			return true
		}
	}
	return false
}
