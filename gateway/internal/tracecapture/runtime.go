package tracecapture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const ownershipMarkerName = ".trace-store-owner"

type RuntimePaths struct {
	RuntimeID string
	TraceDir  string
	ExportDir string
}

type ownershipMarker struct {
	SchemaVersion int    `json:"schema_version"`
	RuntimeID     string `json:"runtime_id"`
}

// ResolveRuntimePaths derives the private trace and export roots from the
// canonical active gateway configuration identity.
func ResolveRuntimePaths(activeConfigPath string) (RuntimePaths, error) {
	if activeConfigPath == "" {
		return RuntimePaths{}, errors.New("active config path must not be empty")
	}
	canonical, err := canonicalPath(activeConfigPath)
	if err != nil {
		return RuntimePaths{}, err
	}
	digest := sha256.Sum256([]byte(canonical))
	runtimeID := hex.EncodeToString(digest[:8])
	home, err := os.UserHomeDir()
	if err != nil {
		return RuntimePaths{}, fmt.Errorf("resolve user home: %w", err)
	}
	defaultRoot := filepath.Join(home, ".config", "baseten-switch")
	defaultConfig := filepath.Join(defaultRoot, "gateway.yaml")
	defaultCanonical, defaultErr := canonicalPath(defaultConfig)
	if defaultErr != nil {
		defaultCanonical = filepath.Clean(defaultConfig)
	}
	if canonical == defaultCanonical {
		return RuntimePaths{
			RuntimeID: runtimeID,
			TraceDir:  filepath.Join(defaultRoot, "traces"),
			ExportDir: filepath.Join(defaultRoot, "exports"),
		}, nil
	}
	parent := filepath.Dir(canonical)
	return RuntimePaths{
		RuntimeID: runtimeID,
		TraceDir:  filepath.Join(parent, "traces-"+runtimeID),
		ExportDir: filepath.Join(parent, "exports-"+runtimeID),
	}, nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve active config path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		if parentErr != nil {
			return "", fmt.Errorf("resolve active config parent: %w", parentErr)
		}
		return filepath.Join(parent, filepath.Base(absolute)), nil
	}
	return "", fmt.Errorf("canonicalize active config path: %w", err)
}

// EnsureRuntimeTraceStore creates or validates the runtime-owned trace root
// and its ownership marker. It never creates the export directory.
func EnsureRuntimeTraceStore(paths RuntimePaths) error {
	if err := validateRuntimePaths(paths); err != nil {
		return err
	}
	if err := validateRuntimeParent(filepath.Dir(paths.TraceDir)); err != nil {
		return err
	}
	_, statErr := os.Lstat(paths.TraceDir)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect trace store: %w", statErr)
	}
	if err := ensurePrivateDirectory(paths.TraceDir); err != nil {
		return err
	}
	markerPath := filepath.Join(paths.TraceDir, ownershipMarkerName)
	err := validateOwnershipMarker(markerPath, paths.RuntimeID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if existed {
		return errors.New("existing trace store is missing its ownership marker")
	}
	segments, discoverErr := DiscoverSegments(paths.TraceDir)
	if discoverErr != nil {
		return discoverErr
	}
	if len(segments) > 0 {
		return errors.New("trace store ownership marker is missing from a nonempty store")
	}
	marker := ownershipMarker{SchemaVersion: SchemaVersionV1, RuntimeID: paths.RuntimeID}
	encoded, marshalErr := json.Marshal(marker)
	if marshalErr != nil {
		return marshalErr
	}
	encoded = append(encoded, '\n')
	file, openErr := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if openErr != nil {
		if errors.Is(openErr, os.ErrExist) {
			return validateOwnershipMarker(markerPath, paths.RuntimeID)
		}
		return fmt.Errorf("create trace ownership marker: %w", openErr)
	}
	if _, writeErr := file.Write(encoded); writeErr != nil {
		_ = file.Close()
		_ = os.Remove(markerPath)
		return fmt.Errorf("write trace ownership marker: %w", writeErr)
	}
	if syncErr := file.Sync(); syncErr != nil {
		_ = file.Close()
		_ = os.Remove(markerPath)
		return fmt.Errorf("sync trace ownership marker: %w", syncErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("close trace ownership marker: %w", closeErr)
	}
	return syncDirectory(paths.TraceDir)
}

func ValidateRuntimeTraceStore(paths RuntimePaths) error {
	if err := validateRuntimePaths(paths); err != nil {
		return err
	}
	if err := validateRuntimeParent(filepath.Dir(paths.TraceDir)); err != nil {
		return err
	}
	info, err := os.Lstat(paths.TraceDir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("trace store must be a private non-symlink directory")
	}
	if err := validateCurrentOwner(info); err != nil {
		return err
	}
	return validateOwnershipMarker(filepath.Join(paths.TraceDir, ownershipMarkerName), paths.RuntimeID)
}

func validateOwnershipMarker(path, runtimeID string) error {
	file, _, err := openPrivateFileNoFollow(path, syscall.O_RDONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, 4<<10))
	if err != nil {
		return fmt.Errorf("read trace ownership marker: %w", err)
	}
	var marker ownershipMarker
	if err := json.Unmarshal(encoded, &marker); err != nil {
		return errors.New("trace ownership marker is malformed")
	}
	if marker.SchemaVersion != SchemaVersionV1 || marker.RuntimeID != runtimeID {
		return errors.New("trace ownership marker does not match the active runtime")
	}
	return nil
}

func validateRuntimePaths(paths RuntimePaths) error {
	if !isLowerHex(paths.RuntimeID, 16) {
		return errors.New("runtime ID must be 16 lowercase hexadecimal characters")
	}
	if paths.TraceDir == "" || paths.ExportDir == "" {
		return errors.New("runtime trace and export directories must not be empty")
	}
	trace, err := filepath.Abs(paths.TraceDir)
	if err != nil {
		return err
	}
	export, err := filepath.Abs(paths.ExportDir)
	if err != nil {
		return err
	}
	if filepath.Clean(trace) == filepath.Clean(export) {
		return errors.New("trace and export directories must be distinct")
	}
	return nil
}

func validateRuntimeParent(parent string) error {
	absolute, err := filepath.Abs(parent)
	if err != nil {
		return err
	}
	if err := rejectSymlinkComponents(absolute); err != nil {
		return err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("stat trace runtime parent: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("trace runtime parent must be a non-symlink directory")
	}
	if err := validateCurrentOwner(info); err != nil {
		return fmt.Errorf("validate trace runtime parent: %w", err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("trace runtime parent must not be group-writable or world-writable")
	}
	return nil
}

type PurgeResult struct {
	RemovedFiles int
	RemovedBytes int64
}

// PurgeRuntimeTraceStore removes only recognized trace segments, the local
// correlation key, and abandoned correlation-key temporary files. The marker
// and lock remain so runtime ownership is preserved.
func PurgeRuntimeTraceStore(paths RuntimePaths) (PurgeResult, error) {
	if err := ValidateRuntimeTraceStore(paths); err != nil {
		return PurgeResult{}, err
	}
	lockFile, err := acquireWriterLock(paths.TraceDir)
	if err != nil {
		return PurgeResult{}, errors.Join(ErrStoreLocked, err)
	}
	defer releaseWriterLock(lockFile)
	entries, err := os.ReadDir(paths.TraceDir)
	if err != nil {
		return PurgeResult{}, fmt.Errorf("read trace store for purge: %w", err)
	}
	var result PurgeResult
	for _, entry := range entries {
		name := entry.Name()
		_, _, segment := parseSegmentName(name)
		recognized := segment || name == correlationKeyName || strings.HasPrefix(name, ".correlation-key.tmp-")
		if !recognized {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return result, fmt.Errorf("refusing symlinked trace artifact %s", name)
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return result, fmt.Errorf("refusing non-regular trace artifact %s", name)
		}
		if err := validatePrivateFileInfo(info, 0o600); err != nil {
			return result, fmt.Errorf("refusing unsafe trace artifact %s: %w", name, err)
		}
		if removeErr := os.Remove(filepath.Join(paths.TraceDir, name)); removeErr != nil {
			return result, fmt.Errorf("remove trace artifact %s: %w", name, removeErr)
		}
		result.RemovedFiles++
		result.RemovedBytes += info.Size()
	}
	if err := syncDirectory(paths.TraceDir); err != nil {
		return result, err
	}
	return result, nil
}

// RemoveRuntimeTraceStore removes a validated, inactive trace store after its
// recognized content has been purged. Unknown entries make removal fail.
func RemoveRuntimeTraceStore(paths RuntimePaths) error {
	if err := ValidateRuntimeTraceStore(paths); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if _, err := PurgeRuntimeTraceStore(paths); err != nil {
		return err
	}
	lock, err := acquireWriterLock(paths.TraceDir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(paths.TraceDir)
	if err != nil {
		_ = releaseWriterLock(lock)
		return err
	}
	for _, entry := range entries {
		if entry.Name() != ownershipMarkerName && entry.Name() != writerLockName {
			_ = releaseWriterLock(lock)
			return fmt.Errorf("refusing to remove trace store containing unrecognized entry %s", entry.Name())
		}
	}
	if err := os.Remove(filepath.Join(paths.TraceDir, ownershipMarkerName)); err != nil {
		_ = releaseWriterLock(lock)
		return err
	}
	if err := os.Remove(filepath.Join(paths.TraceDir, writerLockName)); err != nil {
		_ = releaseWriterLock(lock)
		return err
	}
	if err := releaseWriterLock(lock); err != nil {
		return err
	}
	if err := os.Remove(paths.TraceDir); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(paths.TraceDir))
}
