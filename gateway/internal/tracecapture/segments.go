package tracecapture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	segmentPrefix = "trace-content-"
	segmentSuffix = ".jsonl"
)

type Segment struct {
	Name     string
	Path     string
	Day      time.Time
	Sequence int
	Size     int64
}

func DiscoverSegments(dir string) ([]Segment, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read trace directory: %w", err)
	}
	segments := make([]Segment, 0, len(entries))
	for _, entry := range entries {
		day, sequence, ok := parseSegmentName(entry.Name())
		if !ok {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("recognized trace segment %s must not be a symlink", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat trace segment %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("recognized trace segment %s must be a regular file", entry.Name())
		}
		if err := validatePrivateFileInfo(info, 0o600); err != nil {
			return nil, fmt.Errorf("validate trace segment %s: %w", entry.Name(), err)
		}
		segments = append(segments, Segment{
			Name: entry.Name(), Path: filepath.Join(dir, entry.Name()),
			Day: day, Sequence: sequence, Size: info.Size(),
		})
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Name < segments[j].Name })
	return segments, nil
}

func parseSegmentName(name string) (time.Time, int, bool) {
	if !strings.HasPrefix(name, segmentPrefix) || !strings.HasSuffix(name, segmentSuffix) {
		return time.Time{}, 0, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, segmentPrefix), segmentSuffix)
	if len(body) != len("2006-01-02-001") || body[10] != '-' {
		return time.Time{}, 0, false
	}
	day, err := time.Parse("2006-01-02", body[:10])
	if err != nil {
		return time.Time{}, 0, false
	}
	sequence, err := strconv.Atoi(body[11:])
	if err != nil || sequence < 1 || sequence > 999 {
		return time.Time{}, 0, false
	}
	return day.UTC(), sequence, true
}

func nextSegmentName(segments []Segment, day time.Time) (string, error) {
	day = utcDay(day)
	highest := 0
	for _, segment := range segments {
		if segment.Day.Equal(day) && segment.Sequence > highest {
			highest = segment.Sequence
		}
	}
	if highest >= 999 {
		return "", fmt.Errorf("trace segment sequence exhausted for %s", day.Format("2006-01-02"))
	}
	return fmt.Sprintf("%s%s-%03d%s", segmentPrefix, day.Format("2006-01-02"), highest+1, segmentSuffix), nil
}

func utcDay(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func deleteExpiredSegments(dir, activeName string, cutoff time.Time) (int64, error) {
	segments, err := DiscoverSegments(dir)
	if err != nil {
		return 0, err
	}
	cutoff = cutoff.UTC()
	var removedBytes int64
	var deleteErrors []error
	for _, segment := range segments {
		dayEnd := segment.Day.AddDate(0, 0, 1)
		if segment.Name == activeName || dayEnd.After(cutoff) {
			continue
		}
		if err := os.Remove(segment.Path); err != nil {
			deleteErrors = append(deleteErrors, fmt.Errorf("delete expired trace segment %s: %w", segment.Name, err))
			continue
		}
		removedBytes += segment.Size
	}
	return removedBytes, errors.Join(deleteErrors...)
}
