// Package safefile replaces user-owned configuration files without severing
// symbolic links or overwriting a file that changed after it was read.
package safefile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrConflict means the configured path or its target changed after Read.
	ErrConflict = errors.New("safefile: preimage changed")
	// ErrUnsafeTarget means the configured path cannot be replaced safely.
	ErrUnsafeTarget = errors.New("safefile: unsafe target")
	// ErrHardLinked means replacement would detach another hard link.
	ErrHardLinked = errors.New("safefile: target has multiple hard links")
)

// CommitError reports an error discovered after the requested replacement was
// made. Callers can use Applied to decide whether recovery state must be kept.
type CommitError struct {
	Err     error
	Applied bool
}

func (e *CommitError) Error() string { return e.Err.Error() }
func (e *CommitError) Unwrap() error { return e.Err }

// CommitApplied reports whether err followed a successful filesystem commit.
func CommitApplied(err error) bool {
	var commitErr *CommitError
	return errors.As(err, &commitErr) && commitErr.Applied
}

// Identity describes the file generation captured by a Snapshot.
type Identity struct {
	Device          uint64 `json:"device"`
	Inode           uint64 `json:"inode"`
	Links           uint64 `json:"links"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
}

// Snapshot binds file bytes to the configured path, resolved target, mode, and
// identity observed by Read.
type Snapshot struct {
	RequestedPath string
	ResolvedPath  string
	Exists        bool
	Linked        bool
	FinalLinked   bool
	Data          []byte
	Mode          fs.FileMode
	Identity      Identity

	preimageData     []byte
	preimageMode     fs.FileMode
	preimageIdentity Identity
	preimageExists   bool
	preimageLinked   bool
	preimageFinal    bool
	preimageResolved string
}

// beforeCommitHook is a test seam for changes between staging and validation.
var beforeCommitHook func()

// Read resolves path and returns a stable snapshot of its regular-file target.
// An ordinary missing path is a valid snapshot. Dangling or cyclic links,
// non-regular targets, and multiply hard-linked files are rejected.
func Read(path string) (*Snapshot, error) {
	if !platformSupportsIdentity {
		return nil, fmt.Errorf("%w: file identity checks are unsupported on this platform", ErrUnsafeTarget)
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: configured path is empty", ErrUnsafeTarget)
	}

	requested, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: make %q absolute: %v", ErrUnsafeTarget, path, err)
	}
	requested = filepath.Clean(requested)

	resolved, exists, linked, err := resolveConfiguredPath(requested)
	if err != nil {
		return nil, err
	}
	finalLinked, err := finalComponentLinked(requested)
	if err != nil {
		return nil, err
	}
	if !exists {
		return &Snapshot{
			RequestedPath:    requested,
			ResolvedPath:     resolved,
			Linked:           linked,
			FinalLinked:      finalLinked,
			preimageLinked:   linked,
			preimageFinal:    finalLinked,
			preimageResolved: resolved,
		}, nil
	}

	file, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("open resolved target %s: %w", resolved, err)
	}
	defer file.Close()

	before, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat resolved target %s: %w", resolved, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s resolves to a non-regular file", ErrUnsafeTarget, requested)
	}
	beforeIdentity, err := identityFromInfo(before)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect %s: %v", ErrUnsafeTarget, requested, err)
	}
	if beforeIdentity.Links > 1 {
		return nil, fmt.Errorf("%w: %s resolves to %s with %d links", ErrHardLinked, requested, resolved, beforeIdentity.Links)
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read resolved target %s: %w", resolved, err)
	}
	afterOpen, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: restat opened target %s: %v", ErrConflict, requested, err)
	}
	afterOpenIdentity, err := identityFromInfo(afterOpen)
	if err != nil {
		return nil, fmt.Errorf("%w: reinspect %s: %v", ErrUnsafeTarget, requested, err)
	}
	if beforeIdentity != afterOpenIdentity || before.Mode().Perm() != afterOpen.Mode().Perm() {
		return nil, fmt.Errorf("%w: %s changed while it was read", ErrConflict, requested)
	}
	afterPath, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("%w: target disappeared while reading %s: %v", ErrConflict, requested, err)
	}
	afterPathIdentity, err := identityFromInfo(afterPath)
	if err != nil {
		return nil, fmt.Errorf("%w: reinspect path %s: %v", ErrUnsafeTarget, requested, err)
	}
	if afterOpenIdentity != afterPathIdentity || afterOpen.Mode().Perm() != afterPath.Mode().Perm() {
		return nil, fmt.Errorf("%w: %s was replaced while it was read", ErrConflict, requested)
	}

	checkResolved, checkExists, checkLinked, err := resolveConfiguredPath(requested)
	if err != nil {
		return nil, err
	}
	checkFinalLinked, err := finalComponentLinked(requested)
	if err != nil {
		return nil, err
	}
	if !checkExists || checkResolved != resolved || checkLinked != linked || checkFinalLinked != finalLinked {
		return nil, fmt.Errorf("%w: %s changed while it was resolved", ErrConflict, requested)
	}

	return &Snapshot{
		RequestedPath:    requested,
		ResolvedPath:     resolved,
		Exists:           true,
		Linked:           linked,
		FinalLinked:      finalLinked,
		Data:             bytes.Clone(data),
		Mode:             afterPath.Mode().Perm(),
		Identity:         afterPathIdentity,
		preimageData:     bytes.Clone(data),
		preimageMode:     afterPath.Mode().Perm(),
		preimageIdentity: afterPathIdentity,
		preimageExists:   true,
		preimageLinked:   linked,
		preimageFinal:    finalLinked,
		preimageResolved: resolved,
	}, nil
}

// Replace atomically installs data beside the resolved target. Existing files
// retain their mode. Missing files use createMode.
func (s *Snapshot) Replace(data []byte, createMode fs.FileMode) (*Snapshot, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil snapshot", ErrUnsafeTarget)
	}
	if s.preimageExists && s.preimageIdentity.Links > 1 {
		return nil, fmt.Errorf("%w: %s", ErrHardLinked, s.RequestedPath)
	}

	targetDir := filepath.Dir(s.preimageResolved)
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return nil, fmt.Errorf("create target directory %s: %w", targetDir, err)
	}
	mode := createMode.Perm()
	if s.preimageExists {
		mode = s.preimageMode.Perm()
	}
	if mode == 0 {
		mode = 0o600
	}

	temp, err := os.CreateTemp(targetDir, "."+filepath.Base(s.preimageResolved)+".safefile-*")
	if err != nil {
		return nil, fmt.Errorf("create staged file beside %s: %w", s.preimageResolved, err)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(mode); err != nil {
		return nil, fmt.Errorf("set staged file mode: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return nil, fmt.Errorf("write staged file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return nil, fmt.Errorf("sync staged file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return nil, fmt.Errorf("close staged file: %w", err)
	}

	if beforeCommitHook != nil {
		beforeCommitHook()
	}
	if err := s.verifyCurrent(); err != nil {
		return nil, err
	}

	if s.preimageExists {
		if err := os.Rename(tempPath, s.preimageResolved); err != nil {
			return nil, fmt.Errorf("replace %s: %w", s.preimageResolved, err)
		}
		removeTemp = false
	} else {
		// Linking is an atomic, no-clobber creation because the staged file is
		// on the same filesystem as the destination.
		if err := os.Link(tempPath, s.preimageResolved); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return nil, fmt.Errorf("%w: %s appeared before creation", ErrConflict, s.RequestedPath)
			}
			return nil, fmt.Errorf("create %s: %w", s.preimageResolved, err)
		}
		if err := os.Remove(tempPath); err != nil {
			return nil, &CommitError{
				Err:     fmt.Errorf("created %s but could not remove staged link: %w", s.preimageResolved, err),
				Applied: true,
			}
		}
		removeTemp = false
	}

	if err := syncDirectory(targetDir); err != nil {
		return nil, &CommitError{
			Err:     fmt.Errorf("replacement committed but directory sync failed for %s: %w", targetDir, err),
			Applied: true,
		}
	}
	committed, err := Read(s.RequestedPath)
	if err != nil {
		return nil, &CommitError{
			Err:     fmt.Errorf("replacement committed but could not be verified: %w", err),
			Applied: true,
		}
	}
	if !committed.Exists || !bytes.Equal(committed.Data, data) {
		return nil, &CommitError{
			Err:     fmt.Errorf("%w: committed bytes at %s do not match", ErrConflict, s.RequestedPath),
			Applied: true,
		}
	}
	return committed, nil
}

// Remove deletes the resolved regular target after confirming that the
// configured path still identifies this snapshot. A configured symbolic link
// is left in place. Removing an already-missing snapshot is a verified no-op.
func (s *Snapshot) Remove() error {
	if s == nil {
		return fmt.Errorf("%w: nil snapshot", ErrUnsafeTarget)
	}
	if s.preimageExists && s.preimageIdentity.Links > 1 {
		return fmt.Errorf("%w: %s", ErrHardLinked, s.RequestedPath)
	}

	if beforeCommitHook != nil {
		beforeCommitHook()
	}
	if err := s.verifyCurrent(); err != nil {
		return err
	}
	if !s.preimageExists {
		return nil
	}

	targetDir := filepath.Dir(s.preimageResolved)
	if err := os.Remove(s.preimageResolved); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s disappeared before removal", ErrConflict, s.RequestedPath)
		}
		return fmt.Errorf("remove resolved target %s: %w", s.preimageResolved, err)
	}
	if err := syncDirectory(targetDir); err != nil {
		return &CommitError{
			Err:     fmt.Errorf("removal committed but directory sync failed for %s: %w", targetDir, err),
			Applied: true,
		}
	}
	if _, err := os.Lstat(s.preimageResolved); err == nil {
		return &CommitError{
			Err:     fmt.Errorf("%w: removed target %s is still present", ErrConflict, s.preimageResolved),
			Applied: true,
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return &CommitError{
			Err:     fmt.Errorf("removal committed but target absence could not be verified: %w", err),
			Applied: true,
		}
	}
	return nil
}

// Verify confirms that the configured path still identifies the exact
// preimage captured by Read without changing it.
func (s *Snapshot) Verify() error {
	if s == nil {
		return fmt.Errorf("%w: nil snapshot", ErrUnsafeTarget)
	}
	if beforeCommitHook != nil {
		beforeCommitHook()
	}
	return s.verifyCurrent()
}

func (s *Snapshot) verifyCurrent() error {
	current, err := Read(s.RequestedPath)
	if err != nil {
		if errors.Is(err, ErrHardLinked) || errors.Is(err, ErrUnsafeTarget) {
			return fmt.Errorf("%w: %s is no longer the snapshotted target: %w", ErrConflict, s.RequestedPath, err)
		}
		return err
	}
	if current.ResolvedPath != s.preimageResolved ||
		current.Exists != s.preimageExists ||
		current.Linked != s.preimageLinked ||
		current.FinalLinked != s.preimageFinal {
		return fmt.Errorf("%w: %s resolves differently", ErrConflict, s.RequestedPath)
	}
	if !s.preimageExists {
		return nil
	}
	if current.Identity != s.preimageIdentity ||
		current.Mode.Perm() != s.preimageMode.Perm() ||
		!bytes.Equal(current.Data, s.preimageData) {
		return fmt.Errorf("%w: %s no longer matches its snapshot", ErrConflict, s.RequestedPath)
	}
	return nil
}

func finalComponentLinked(requested string) (bool, error) {
	info, err := os.Lstat(requested)
	if err == nil {
		return info.Mode()&os.ModeSymlink != 0, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("%w: inspect %s: %v", ErrUnsafeTarget, requested, err)
}

func resolveConfiguredPath(requested string) (resolved string, exists, linked bool, err error) {
	if _, lstatErr := os.Lstat(requested); lstatErr == nil {
		target, evalErr := filepath.EvalSymlinks(requested)
		if evalErr != nil {
			return "", false, false, fmt.Errorf("%w: resolve %s: %v", ErrUnsafeTarget, requested, evalErr)
		}
		target, evalErr = filepath.Abs(target)
		if evalErr != nil {
			return "", false, false, fmt.Errorf("%w: make resolved target absolute: %v", ErrUnsafeTarget, evalErr)
		}
		return filepath.Clean(target), true, filepath.Clean(target) != requested, nil
	} else if !errors.Is(lstatErr, fs.ErrNotExist) {
		return "", false, false, fmt.Errorf("%w: inspect %s: %v", ErrUnsafeTarget, requested, lstatErr)
	}

	probe := requested
	var missing []string
	for {
		if _, lstatErr := os.Lstat(probe); lstatErr == nil {
			break
		} else if !errors.Is(lstatErr, fs.ErrNotExist) {
			return "", false, false, fmt.Errorf("%w: inspect ancestor %s: %v", ErrUnsafeTarget, probe, lstatErr)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", false, false, fmt.Errorf("%w: no existing ancestor for %s", ErrUnsafeTarget, requested)
		}
		missing = append([]string{filepath.Base(probe)}, missing...)
		probe = parent
	}

	ancestor, evalErr := filepath.EvalSymlinks(probe)
	if evalErr != nil {
		return "", false, false, fmt.Errorf("%w: resolve ancestor %s: %v", ErrUnsafeTarget, probe, evalErr)
	}
	ancestor, evalErr = filepath.Abs(ancestor)
	if evalErr != nil {
		return "", false, false, fmt.Errorf("%w: make resolved ancestor absolute: %v", ErrUnsafeTarget, evalErr)
	}
	info, statErr := os.Stat(ancestor)
	if statErr != nil {
		return "", false, false, fmt.Errorf("%w: inspect resolved ancestor %s: %v", ErrUnsafeTarget, ancestor, statErr)
	}
	if !info.IsDir() {
		return "", false, false, fmt.Errorf("%w: ancestor %s is not a directory", ErrUnsafeTarget, ancestor)
	}
	target := filepath.Join(append([]string{ancestor}, missing...)...)
	target = filepath.Clean(target)
	return target, false, filepath.Clean(probe) != filepath.Clean(ancestor), nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
