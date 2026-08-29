package tracecapture

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const (
	exportMarkerName = ".export-store-owner"
	exportLockName   = ".export.lock"
	quarantineName   = "quarantine"
)

var (
	exportPackageNamePattern = regexp.MustCompile(`^baseten-switch-traces-[0-9]{8}T[0-9]{6}Z(?:-[0-9a-f]{8})?\.zip$`)
	quarantineIDPattern      = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type ExportStatus struct {
	PackageCount    int
	PackageBytes    int64
	QuarantineCount int
	QuarantineBytes int64
}

type ExportPurgeResult struct {
	RemovedPackages   int
	RemovedQuarantine int
	RemovedBytes      int64
}

func EnsureRuntimeExportStore(paths RuntimePaths) error {
	if err := validateRuntimePaths(paths); err != nil {
		return err
	}
	if err := validateRuntimeParent(filepath.Dir(paths.ExportDir)); err != nil {
		return err
	}
	_, statErr := os.Lstat(paths.ExportDir)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect export store: %w", statErr)
	}
	if err := ensurePrivateDirectory(paths.ExportDir); err != nil {
		return err
	}
	if existed {
		if err := validateRuntimeMarker(filepath.Join(paths.ExportDir, exportMarkerName), paths.RuntimeID); err != nil {
			return errors.New("existing export store is missing or has an invalid ownership marker")
		}
	}
	if err := ensureRuntimeMarker(filepath.Join(paths.ExportDir, exportMarkerName), paths.RuntimeID); err != nil {
		return err
	}
	quarantine := filepath.Join(paths.ExportDir, quarantineName)
	if err := ensurePrivateDirectory(quarantine); err != nil {
		return err
	}
	return syncDirectory(paths.ExportDir)
}

func ValidateRuntimeExportStore(paths RuntimePaths) error {
	if err := validateRuntimePaths(paths); err != nil {
		return err
	}
	if err := validateRuntimeParent(filepath.Dir(paths.ExportDir)); err != nil {
		return err
	}
	if err := validatePrivateDirectory(paths.ExportDir); err != nil {
		return err
	}
	if err := validateRuntimeMarker(filepath.Join(paths.ExportDir, exportMarkerName), paths.RuntimeID); err != nil {
		return err
	}
	return validatePrivateDirectory(filepath.Join(paths.ExportDir, quarantineName))
}

func ensureRuntimeMarker(path, runtimeID string) error {
	if err := validateRuntimeMarker(path, runtimeID); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	encoded, err := json.Marshal(ownershipMarker{SchemaVersion: SchemaVersionV1, RuntimeID: runtimeID})
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return validateRuntimeMarker(path, runtimeID)
		}
		return fmt.Errorf("create runtime ownership marker: %w", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return fmt.Errorf("write runtime ownership marker: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync runtime ownership marker: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close runtime ownership marker: %w", err)
	}
	remove = false
	return syncDirectory(filepath.Dir(path))
}

func validateRuntimeMarker(path, runtimeID string) error {
	file, _, err := openPrivateFileNoFollow(path, syscall.O_RDONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, 4<<10))
	if err != nil {
		return fmt.Errorf("read runtime ownership marker: %w", err)
	}
	var marker ownershipMarker
	if json.Unmarshal(encoded, &marker) != nil || marker.SchemaVersion != SchemaVersionV1 || marker.RuntimeID != runtimeID {
		return errors.New("runtime ownership marker does not match the active runtime")
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("runtime directory must be a private non-symlink directory")
	}
	return validateCurrentOwner(info)
}

func InspectRuntimeExports(paths RuntimePaths) (ExportStatus, error) {
	if err := ValidateRuntimeExportStore(paths); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ExportStatus{}, nil
		}
		return ExportStatus{}, err
	}
	var result ExportStatus
	entries, err := os.ReadDir(paths.ExportDir)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if !exportPackageNamePattern.MatchString(entry.Name()) {
			continue
		}
		info, err := validateExportEntry(entry)
		if err != nil {
			return result, err
		}
		result.PackageCount++
		result.PackageBytes += info.Size()
	}
	quarantine, err := os.ReadDir(filepath.Join(paths.ExportDir, quarantineName))
	if err != nil {
		return result, err
	}
	for _, entry := range quarantine {
		if !quarantineIDPattern.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || entry.Type()&os.ModeSymlink != 0 || !info.IsDir() {
			return result, fmt.Errorf("unsafe quarantine entry %s", entry.Name())
		}
		result.QuarantineCount++
		bytes, err := directoryRegularBytes(filepath.Join(paths.ExportDir, quarantineName, entry.Name()))
		if err != nil {
			return result, err
		}
		result.QuarantineBytes += bytes
	}
	return result, nil
}

func QuarantineExportStaging(paths RuntimePaths, stagingPath string) (string, error) {
	if err := ValidateRuntimeExportStore(paths); err != nil {
		return "", err
	}
	stageAbs, err := filepath.Abs(stagingPath)
	if err != nil {
		return "", err
	}
	exportAbs, err := filepath.Abs(paths.ExportDir)
	if err != nil {
		return "", err
	}
	if filepath.Dir(stageAbs) != exportAbs {
		return "", errors.New("refusing to quarantine package staging outside the managed export store")
	}
	return quarantineValidatedExportStaging(paths, stageAbs)
}

// QuarantineExternalExportStaging moves a packager-owned staging directory
// beside an explicitly named custom destination into the managed quarantine.
func QuarantineExternalExportStaging(paths RuntimePaths, stagingPath, destination string) (string, error) {
	if err := ValidateRuntimeExportStore(paths); err != nil {
		return "", err
	}
	stageAbs, err := filepath.Abs(stagingPath)
	if err != nil {
		return "", err
	}
	destinationAbs, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	if filepath.Dir(stageAbs) != filepath.Dir(destinationAbs) {
		return "", errors.New("refusing to quarantine staging outside the custom destination directory")
	}
	return quarantineValidatedExportStaging(paths, stageAbs)
}

func quarantineValidatedExportStaging(paths RuntimePaths, stageAbs string) (string, error) {
	if !strings.HasPrefix(filepath.Base(stageAbs), ".baseten-switch-package-") {
		return "", errors.New("refusing to quarantine an unrecognized staging path")
	}
	stageInfo, err := os.Lstat(stageAbs)
	if err != nil || stageInfo.Mode()&os.ModeSymlink != 0 || !stageInfo.IsDir() || stageInfo.Mode().Perm() != 0o700 {
		return "", errors.New("refusing to quarantine unsafe package staging")
	}
	if err := validateCurrentOwner(stageInfo); err != nil {
		return "", errors.New("refusing to quarantine package staging with an unexpected owner")
	}
	// Custom destinations may be on another filesystem. os.Rename below then
	// fails closed and the caller reports the still-private recovery directory.
	for attempts := 0; attempts < 8; attempts++ {
		var raw [16]byte
		if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
			return "", err
		}
		id := hex.EncodeToString(raw[:])
		reservation := filepath.Join(paths.ExportDir, quarantineName, id)
		if err := os.Mkdir(reservation, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", fmt.Errorf("reserve package quarantine: %w", err)
		}
		destination := filepath.Join(reservation, "staging")
		if err := os.Rename(stageAbs, destination); err != nil {
			_ = os.Remove(reservation)
			return "", fmt.Errorf("quarantine package staging: %w", err)
		}
		if err := syncDirectory(reservation); err != nil {
			return "", err
		}
		if err := syncDirectory(filepath.Join(paths.ExportDir, quarantineName)); err != nil {
			return "", err
		}
		return id, nil
	}
	return "", errors.New("could not allocate a quarantine identifier")
}

func CleanupExportQuarantine(paths RuntimePaths, id string) error {
	if !quarantineIDPattern.MatchString(id) {
		return errors.New("invalid quarantine identifier")
	}
	if err := ValidateRuntimeExportStore(paths); err != nil {
		return err
	}
	target := filepath.Join(paths.ExportDir, quarantineName, id)
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("quarantine entry must be a non-symlink directory")
	}
	if _, err := directoryRegularBytes(target); err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(target))
}

func PurgeRuntimeExports(paths RuntimePaths) (ExportPurgeResult, error) {
	if err := ValidateRuntimeExportStore(paths); err != nil {
		return ExportPurgeResult{}, err
	}
	lock, err := acquireExportLock(paths.ExportDir)
	if err != nil {
		return ExportPurgeResult{}, err
	}
	defer releaseWriterLock(lock)
	var result ExportPurgeResult
	entries, err := os.ReadDir(paths.ExportDir)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if !exportPackageNamePattern.MatchString(entry.Name()) {
			continue
		}
		info, err := validateExportEntry(entry)
		if err != nil {
			return result, err
		}
		if err := os.Remove(filepath.Join(paths.ExportDir, entry.Name())); err != nil {
			return result, err
		}
		result.RemovedPackages++
		result.RemovedBytes += info.Size()
	}
	quarantineRoot := filepath.Join(paths.ExportDir, quarantineName)
	quarantine, err := os.ReadDir(quarantineRoot)
	if err != nil {
		return result, err
	}
	for _, entry := range quarantine {
		if !quarantineIDPattern.MatchString(entry.Name()) {
			continue
		}
		target := filepath.Join(quarantineRoot, entry.Name())
		bytes, err := directoryRegularBytes(target)
		if err != nil {
			return result, err
		}
		if err := os.RemoveAll(target); err != nil {
			return result, err
		}
		result.RemovedQuarantine++
		result.RemovedBytes += bytes
	}
	if err := syncDirectory(paths.ExportDir); err != nil {
		return result, err
	}
	return result, nil
}

func RemoveRuntimeExportStore(paths RuntimePaths) error {
	if err := ValidateRuntimeExportStore(paths); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if _, err := PurgeRuntimeExports(paths); err != nil {
		return err
	}
	lock, err := acquireExportLock(paths.ExportDir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(paths.ExportDir)
	if err != nil {
		_ = releaseWriterLock(lock)
		return err
	}
	for _, entry := range entries {
		if entry.Name() != exportMarkerName && entry.Name() != exportLockName && entry.Name() != quarantineName {
			_ = releaseWriterLock(lock)
			return fmt.Errorf("refusing to remove export store containing unrecognized entry %s", entry.Name())
		}
	}
	if quarantine, err := os.ReadDir(filepath.Join(paths.ExportDir, quarantineName)); err != nil || len(quarantine) != 0 {
		_ = releaseWriterLock(lock)
		return errors.New("refusing to remove a nonempty export quarantine")
	}
	for _, target := range []string{quarantineName, exportMarkerName, exportLockName} {
		if err := os.Remove(filepath.Join(paths.ExportDir, target)); err != nil {
			_ = releaseWriterLock(lock)
			return err
		}
	}
	if err := releaseWriterLock(lock); err != nil {
		return err
	}
	if err := os.Remove(paths.ExportDir); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(paths.ExportDir))
}

func validateExportEntry(entry os.DirEntry) (os.FileInfo, error) {
	if entry.Type()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("recognized export %s must not be a symlink", entry.Name())
	}
	info, err := entry.Info()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("recognized export %s must be a private regular file", entry.Name())
	}
	if err := validateCurrentOwner(info); err != nil {
		return nil, err
	}
	return info, nil
}

func directoryRegularBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("quarantine contains a symlink")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("quarantine contains a non-regular file")
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func acquireExportLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, exportLockName)
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open export lock")
	}
	info, err := file.Stat()
	if err != nil || validatePrivateFileInfo(info, 0o600) != nil {
		_ = file.Close()
		return nil, errors.New("unsafe export lock")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock export store: %w", err)
	}
	return file, nil
}
