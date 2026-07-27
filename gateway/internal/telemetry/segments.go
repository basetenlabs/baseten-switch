package telemetry

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
	segmentPrefix = "requests-"
	segmentSuffix = ".jsonl"
)

// Segment describes one telemetry v1 JSONL segment discovered on disk.
type Segment struct {
	Name     string
	Path     string
	Year     int
	Month    time.Month
	Sequence int
	Size     int64
}

func (s Segment) monthKey() string {
	return fmt.Sprintf("%04d-%02d", s.Year, s.Month)
}

// DiscoverSegments returns recognized regular telemetry segments in lexical
// order. Unknown files, directories, and symlinks are ignored. A missing
// telemetry directory is an empty store.
func DiscoverSegments(dir string) ([]Segment, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read telemetry directory %s: %w", dir, err)
	}

	segments := make([]Segment, 0, len(entries))
	for _, entry := range entries {
		year, month, sequence, ok := parseSegmentName(entry.Name())
		if !ok || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat telemetry segment %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		segments = append(segments, Segment{
			Name:     entry.Name(),
			Path:     filepath.Join(dir, entry.Name()),
			Year:     year,
			Month:    month,
			Sequence: sequence,
			Size:     info.Size(),
		})
	}
	sort.Slice(segments, func(i, j int) bool {
		return segments[i].Name < segments[j].Name
	})
	return segments, nil
}

func parseSegmentName(name string) (year int, month time.Month, sequence int, ok bool) {
	if !strings.HasPrefix(name, segmentPrefix) || !strings.HasSuffix(name, segmentSuffix) {
		return 0, 0, 0, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, segmentPrefix), segmentSuffix)
	parts := strings.Split(body, "-")
	if len(parts) != 3 || len(parts[0]) != 4 || len(parts[1]) != 2 || len(parts[2]) != 3 {
		return 0, 0, 0, false
	}
	year, err := strconv.Atoi(parts[0])
	if err != nil || year < 1 || year > 9999 {
		return 0, 0, 0, false
	}
	monthValue, err := strconv.Atoi(parts[1])
	if err != nil || monthValue < 1 || monthValue > 12 {
		return 0, 0, 0, false
	}
	sequence, err = strconv.Atoi(parts[2])
	if err != nil || sequence < 1 || sequence > 999 {
		return 0, 0, 0, false
	}
	return year, time.Month(monthValue), sequence, true
}

func nextSegmentName(segments []Segment, month time.Time) (string, error) {
	month = month.UTC()
	highest := 0
	for _, segment := range segments {
		if segment.Year == month.Year() && segment.Month == month.Month() &&
			segment.Sequence > highest {
			highest = segment.Sequence
		}
	}
	if highest >= 999 {
		return "", fmt.Errorf("telemetry segment sequence exhausted for %04d-%02d",
			month.Year(), month.Month())
	}
	return fmt.Sprintf("requests-%04d-%02d-%03d.jsonl",
		month.Year(), month.Month(), highest+1), nil
}

// DeleteExpiredSegments removes recognized closed segments whose encoded UTC
// month ends at or before cutoff. Writers put an event only in the segment
// named for its CompletedAt month, so a month boundary proves every event in
// the segment is expired without reading request rows under the writer lock.
//
// The active segment and the segment containing cutoff are retained. Keeping
// the cutoff month avoids deleting events near a partial-month boundary and
// bounds retention imprecision to less than one calendar month.
func DeleteExpiredSegments(dir, activeName string, cutoff time.Time) error {
	segments, err := DiscoverSegments(dir)
	if err != nil {
		return err
	}
	cutoff = cutoff.UTC()
	var deleteErrors []error
	for _, segment := range segments {
		if segment.Name == activeName || !segmentMonthEndsBy(segment, cutoff) {
			continue
		}
		if err := os.Remove(segment.Path); err != nil {
			deleteErrors = append(deleteErrors,
				fmt.Errorf("delete expired telemetry segment %s: %w", segment.Name, err))
		}
	}
	return errors.Join(deleteErrors...)
}

func segmentMonthEndsBy(segment Segment, cutoff time.Time) bool {
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
	return !monthEnd.After(cutoff)
}
