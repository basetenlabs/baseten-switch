package tracecapture

import (
	"errors"
	"fmt"
	"os"
	"time"
)

var ErrStoreLocked = errors.New("trace store is locked by another writer")

type SweepResult struct {
	RemovedBytes int64
	Skipped      bool
}

// SweepRetention prunes a pre-existing disabled store without creating a
// missing directory. It uses the same nonblocking lock as the active writer.
func SweepRetention(dir string, retentionDays int, now time.Time) (SweepResult, error) {
	if retentionDays <= 0 {
		return SweepResult{}, errors.New("trace retention days must be positive")
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return SweepResult{}, nil
	} else if err != nil {
		return SweepResult{}, fmt.Errorf("stat trace directory: %w", err)
	}
	if err := ensurePrivateDirectory(dir); err != nil {
		return SweepResult{}, err
	}
	lockFile, err := acquireWriterLock(dir)
	if err != nil {
		return SweepResult{Skipped: true}, errors.Join(ErrStoreLocked, err)
	}
	defer releaseWriterLock(lockFile)
	removed, err := deleteExpiredSegments(dir, "", now.AddDate(0, 0, -retentionDays))
	return SweepResult{RemovedBytes: removed}, err
}
