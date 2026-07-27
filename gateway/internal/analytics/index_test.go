package analytics

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

func appendEventLine(
	t *testing.T,
	path string,
	value any,
	newline bool,
) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	line, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if newline {
		line = append(line, '\n')
	}
	if _, err := file.Write(line); err != nil {
		t.Fatal(err)
	}
}

func TestIndexDefaultBounds(t *testing.T) {
	index := NewIndex(IndexOptions{})
	if index.opts.MaxAge != 30*24*time.Hour ||
		index.opts.MaxRows != 100_000 ||
		index.opts.BootstrapMaxBytes != 128<<20 {
		t.Fatalf("default bounds = %+v", index.opts)
	}
}

func TestIndexFollowsMultipleSegmentsAndCompleteLines(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "requests-2026-07-001.jsonl")
	secondPath := filepath.Join(dir, "requests-2026-07-002.jsonl")
	first := analyticsEvent(now.Add(-3*time.Minute).Unix(), "anthropic", "claude-opus-4-8", "claude-opus-4-8")
	second := analyticsEvent(now.Add(-2*time.Minute).Unix(), "baseten", "claude-opus-4-8", "zai-org/GLM-5.2")
	third := analyticsEvent(now.Add(-time.Minute).Unix(), "baseten", "claude-sonnet-4-6", "zai-org/GLM-5.2")

	appendEventLine(t, firstPath, first, true)
	appendEventLine(t, firstPath, map[string]string{"bad": "line"}, true)
	appendEventLine(t, secondPath, second, false)

	index := NewIndex(IndexOptions{})
	snapshot := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if len(snapshot.Events) != 1 || snapshot.Events[0].EventID != first.EventID {
		t.Fatalf("initial events = %+v", snapshot.Events)
	}
	if snapshot.Complete || snapshot.Reason != "segment corruption" {
		t.Fatalf("corrupt coverage = complete %v reason %q",
			snapshot.Complete, snapshot.Reason)
	}

	file, err := os.OpenFile(secondPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	snapshot = index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if len(snapshot.Events) != 2 || snapshot.Events[1].EventID != second.EventID {
		t.Fatalf("completed partial events = %+v", snapshot.Events)
	}
	if snapshot.Complete || snapshot.Reason != "segment corruption" {
		t.Fatalf("corruption gap was cleared: %+v", snapshot)
	}

	thirdPath := filepath.Join(dir, "requests-2026-07-003.jsonl")
	appendEventLine(t, thirdPath, third, true)
	snapshot = index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if len(snapshot.Events) != 3 || snapshot.Events[2].EventID != third.EventID {
		t.Fatalf("rotated events = %+v", snapshot.Events)
	}
}

func TestIndexDiscoveryFailureRetainsRowsAndMarksCoveragePartial(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	parent := t.TempDir()
	dir := filepath.Join(parent, "telemetry")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	event := analyticsEvent(now.Add(-time.Minute).Unix(), "anthropic", "model", "model")
	appendEventLine(t, path, event, true)
	index := NewIndex(IndexOptions{})
	first := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if !first.Complete || len(first.Events) != 1 {
		t.Fatalf("initial snapshot = %+v", first)
	}

	moved := filepath.Join(parent, "telemetry-moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if second.Complete || second.Reason != "telemetry read error" {
		t.Fatalf("discovery failure coverage = %+v", second)
	}
	if len(second.Events) != 1 || second.Events[0].EventID != event.EventID {
		t.Fatalf("discovery failure discarded retained events: %+v", second.Events)
	}
}

func TestIndexStatAndReadFailuresRetainRowsAndMarkCoveragePartial(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	event := analyticsEvent(now.Add(-time.Minute).Unix(), "anthropic", "model", "model")
	appendEventLine(t, path, event, true)
	index := NewIndex(IndexOptions{})
	if first := index.Snapshot(dir, now, now.Add(-time.Hour).Unix()); !first.Complete || len(first.Events) != 1 {
		t.Fatalf("initial snapshot = %+v", first)
	}

	originalStat := indexStat
	originalRead := indexReadSegmentEvents
	t.Cleanup(func() {
		indexStat = originalStat
		indexReadSegmentEvents = originalRead
	})
	indexStat = func(string) (os.FileInfo, error) {
		return nil, errors.New("injected stat failure")
	}
	statFailure := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if statFailure.Complete || statFailure.Reason != "telemetry read error" ||
		len(statFailure.Events) != 1 {
		t.Fatalf("stat failure snapshot = %+v", statFailure)
	}

	indexStat = originalStat
	indexReadSegmentEvents = func(
		_ string,
		offset int64,
	) ([]telemetry.EventV1, int64, error) {
		return nil, offset, errors.New("injected read failure")
	}
	readFailure := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if readFailure.Complete || readFailure.Reason != "telemetry read error" ||
		len(readFailure.Events) != 1 {
		t.Fatalf("read failure snapshot = %+v", readFailure)
	}

	indexReadSegmentEvents = func(
		_ string,
		offset int64,
	) ([]telemetry.EventV1, int64, error) {
		return nil, offset, errors.Join(
			&telemetry.SegmentReadError{MalformedLines: 1},
			errors.New("injected read failure"),
		)
	}
	combinedFailure := index.Snapshot(
		dir,
		now,
		now.Add(-time.Hour).Unix(),
	)
	if combinedFailure.Complete ||
		combinedFailure.Reason !=
			"segment corruption, telemetry read error" ||
		len(combinedFailure.Events) != 1 {
		t.Fatalf("combined read failure snapshot = %+v", combinedFailure)
	}
}

func TestIndexAgeRowAndBootstrapCoverage(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	for offset := 5; offset >= 1; offset-- {
		event := analyticsEvent(
			now.Add(-time.Duration(offset)*time.Minute).Unix(),
			"anthropic",
			"claude-model-with-padding",
			"claude-model-with-padding",
		)
		appendEventLine(t, path, event, true)
	}

	index := NewIndex(IndexOptions{
		MaxAge: 4 * time.Minute, MaxRows: 2, BootstrapMaxBytes: 1 << 20,
	})
	snapshot := index.Snapshot(dir, now, now.Add(-10*time.Minute).Unix())
	if len(snapshot.Events) != 2 {
		t.Fatalf("bounded events = %d, want 2", len(snapshot.Events))
	}
	if snapshot.Complete || snapshot.Reason != "row cap, age cap" {
		t.Fatalf("coverage = complete %v reason %q", snapshot.Complete, snapshot.Reason)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	byteIndex := NewIndex(IndexOptions{
		MaxAge: time.Hour, MaxRows: 100, BootstrapMaxBytes: info.Size() / 2,
	})
	byteSnapshot := byteIndex.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if byteSnapshot.Complete ||
		byteSnapshot.Reason != "bootstrap byte cap" ||
		len(byteSnapshot.Events) == 0 {
		t.Fatalf("byte coverage = %+v", byteSnapshot)
	}
}

func TestColdBootstrapStopsAtDiscoveredSegmentSize(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	first := analyticsEvent(now.Add(-time.Minute).Unix(), "anthropic", "first", "first")
	second := analyticsEvent(now.Unix(), "baseten", "second", "second")
	appendEventLine(t, path, first, true)

	originalReadThrough := indexReadSegmentEventsThrough
	t.Cleanup(func() { indexReadSegmentEventsThrough = originalReadThrough })
	appended := false
	indexReadSegmentEventsThrough = func(
		readPath string,
		offset int64,
		end int64,
	) ([]telemetry.EventV1, int64, error) {
		if !appended {
			appended = true
			appendEventLine(t, path, second, true)
		}
		return originalReadThrough(readPath, offset, end)
	}

	index := NewIndex(IndexOptions{})
	cold := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if len(cold.Events) != 1 || cold.Events[0].EventID != first.EventID {
		t.Fatalf("cold snapshot crossed discovered end: %+v", cold.Events)
	}
	warm := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if len(warm.Events) != 2 || warm.Events[1].EventID != second.EventID {
		t.Fatalf("warm snapshot did not consume append: %+v", warm.Events)
	}
}

func TestColdBootstrapWorkerConcurrencyIsBounded(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	const segments = 12
	for sequence := 1; sequence <= segments; sequence++ {
		path := filepath.Join(
			dir,
			fmt.Sprintf("requests-2026-07-%03d.jsonl", sequence),
		)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	originalReadThrough := indexReadSegmentEventsThrough
	t.Cleanup(func() { indexReadSegmentEventsThrough = originalReadThrough })
	var active atomic.Int32
	var highest atomic.Int32
	indexReadSegmentEventsThrough = func(
		path string,
		offset int64,
		end int64,
	) ([]telemetry.EventV1, int64, error) {
		current := active.Add(1)
		for {
			seen := highest.Load()
			if current <= seen || highest.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return originalReadThrough(path, offset, end)
	}

	index := NewIndex(IndexOptions{})
	_ = index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	wantCeiling := min(segments, runtime.GOMAXPROCS(0), indexBootstrapWorkers)
	if got := int(highest.Load()); got < 1 || got > wantCeiling || got > 8 {
		t.Fatalf("maximum cold workers = %d, want 1..%d", got, wantCeiling)
	}
}

func TestIndexDetectsSegmentReplacement(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	old := analyticsEvent(now.Add(-time.Minute).Unix(), "anthropic", "old", "old")
	appendEventLine(t, path, old, true)
	index := NewIndex(IndexOptions{})
	if got := index.Snapshot(dir, now, now.Add(-time.Hour).Unix()); len(got.Events) != 1 {
		t.Fatalf("initial snapshot = %+v", got.Events)
	}

	replacement := analyticsEvent(now.Unix(), "baseten", "new", "new")
	line, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	if err := os.WriteFile(path, line, 0o600); err != nil {
		t.Fatal(err)
	}
	got := index.Snapshot(dir, now.Add(time.Second), now.Add(-time.Hour).Unix())
	if len(got.Events) != 1 || got.Events[0].EventID != replacement.EventID {
		t.Fatalf("replacement snapshot = %+v", got.Events)
	}
}

func TestIndexGenerationTracksDataAndCoverageChanges(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	first := analyticsEvent(
		now.Add(-time.Minute).Unix(),
		"anthropic",
		"first",
		"first",
	)
	appendEventLine(t, path, first, true)

	index := NewIndex(IndexOptions{})
	initial := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if initial.Generation == 0 || len(initial.Events) != 1 {
		t.Fatalf("initial snapshot = %+v", initial)
	}
	unchanged := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if unchanged.Generation != initial.Generation {
		t.Fatalf(
			"unchanged generation = %d, want %d",
			unchanged.Generation,
			initial.Generation,
		)
	}

	second := analyticsEvent(
		now.Unix(),
		"baseten",
		"second",
		"second",
	)
	appendEventLine(t, path, second, false)
	incomplete := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if incomplete.Generation != initial.Generation ||
		len(incomplete.Events) != 1 {
		t.Fatalf("partial append changed generation: %+v", incomplete)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	appended := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if appended.Generation <= initial.Generation ||
		len(appended.Events) != 2 {
		t.Fatalf("completed append did not invalidate: %+v", appended)
	}

	replacement := analyticsEvent(
		now.Add(time.Second).Unix(),
		"baseten",
		"replacement",
		"replacement",
	)
	line, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	replaced := index.Snapshot(
		dir,
		now.Add(time.Second),
		now.Add(-time.Hour).Unix(),
	)
	if replaced.Generation <= appended.Generation ||
		len(replaced.Events) != 1 ||
		replaced.Events[0].EventID != replacement.EventID {
		t.Fatalf("replacement did not invalidate: %+v", replaced)
	}
}

func TestIndexReusesCombinedSnapshotUntilGenerationChanges(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	firstEvent := analyticsEvent(
		now.Add(-time.Minute).Unix(),
		"anthropic",
		"first",
		"first",
	)
	appendEventLine(t, path, firstEvent, true)

	index := NewIndex(IndexOptions{})
	first := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	second := index.Snapshot(dir, now, now.Add(-time.Hour).Unix())
	if first.Generation != second.Generation {
		t.Fatalf("unchanged generation = %d then %d",
			first.Generation, second.Generation)
	}
	if len(first.Events) != 1 || len(second.Events) != 1 ||
		&first.Events[0] != &second.Events[0] {
		t.Fatal("unchanged snapshot did not reuse combined event storage")
	}

	secondEvent := analyticsEvent(
		now.Unix(),
		"baseten",
		"second",
		"second",
	)
	appendEventLine(t, path, secondEvent, true)
	third := index.Snapshot(dir, now.Add(time.Second), now.Add(-time.Hour).Unix())
	if third.Generation == second.Generation {
		t.Fatalf("appended event did not advance generation %d", third.Generation)
	}
	if len(third.Events) != 2 || &third.Events[0] == &second.Events[0] {
		t.Fatal("changed snapshot reused stale combined event storage")
	}
}

func TestIndexGenerationAdvancesWhenAgePruningRemovesData(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-07-001.jsonl")
	event := analyticsEvent(
		now.Add(-30*time.Second).Unix(),
		"anthropic",
		"model",
		"model",
	)
	appendEventLine(t, path, event, true)

	index := NewIndex(IndexOptions{MaxAge: time.Minute})
	initial := index.Snapshot(dir, now, now.Add(-time.Minute).Unix())
	if len(initial.Events) != 1 {
		t.Fatalf("initial events = %+v", initial.Events)
	}
	pruned := index.Snapshot(
		dir,
		now.Add(2*time.Minute),
		now.Add(-time.Minute).Unix(),
	)
	if len(pruned.Events) != 0 ||
		pruned.Generation <= initial.Generation {
		t.Fatalf("pruned snapshot did not invalidate: %+v", pruned)
	}
}
