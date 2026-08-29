package tracecapture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const writerLockName = ".writer.lock"

func ensurePrivateDirectory(dir string) error {
	if dir == "" {
		return errors.New("trace directory must not be empty")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve trace directory: %w", err)
	}
	absolute = filepath.Clean(absolute)
	root := filepath.VolumeName(absolute) + string(os.PathSeparator)
	if absolute == root {
		return fmt.Errorf("refusing unsafe trace directory %s", absolute)
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && absolute == filepath.Clean(home) {
		return fmt.Errorf("refusing unsafe trace directory %s", absolute)
	}
	if absolute == filepath.Clean(os.TempDir()) {
		return fmt.Errorf("refusing unsafe trace directory %s", absolute)
	}
	if err := rejectSymlinkComponents(absolute); err != nil {
		return err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return fmt.Errorf("create trace directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("stat trace directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("trace directory must be a non-symlink directory")
	}
	if err := validateCurrentOwner(info); err != nil {
		return fmt.Errorf("validate trace directory owner: %w", err)
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return fmt.Errorf("secure trace directory: %w", err)
	}
	return nil
}

func rejectSymlinkComponents(absolute string) error {
	volume := filepath.VolumeName(absolute)
	rest := stringsTrimVolumeAndSeparators(absolute, volume)
	current := volume + string(os.PathSeparator)
	for _, component := range splitPathComponents(rest) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect trace directory component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("trace directory component %s must not be a symlink", current)
		}
	}
	return nil
}

func stringsTrimVolumeAndSeparators(path, volume string) string {
	path = path[len(volume):]
	for len(path) > 0 && os.IsPathSeparator(path[0]) {
		path = path[1:]
	}
	return path
}

func splitPathComponents(path string) []string {
	var result []string
	for path != "" && path != "." {
		dir, file := filepath.Split(path)
		if file != "" {
			result = append([]string{file}, result...)
		}
		path = filepath.Clean(dir)
		for len(path) > 0 && os.IsPathSeparator(path[len(path)-1]) {
			path = path[:len(path)-1]
		}
	}
	return result
}

func acquireWriterLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, writerLockName)
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open trace writer lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open trace writer lock")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat trace writer lock: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("trace writer lock must be a regular file")
	}
	if err := validatePrivateFileInfo(info, 0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("validate trace writer lock: %w", err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock trace directory: %w", err)
	}
	return file, nil
}

func openPrivateFileNoFollow(path string, flags int, mode os.FileMode) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, errors.New("must be a regular non-symlink file")
	}
	if err := validatePrivateFileInfo(before, mode); err != nil {
		return nil, nil, err
	}
	fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, nil, errors.New("open private file")
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, nil, errors.New("private file changed while opening")
	}
	if err := validatePrivateFileInfo(after, mode); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, after, nil
}

func releaseWriterLock(file *os.File) error {
	if file == nil {
		return nil
	}
	errUnlock := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	errClose := file.Close()
	return errors.Join(errUnlock, errClose)
}

func validatePrivateFileInfo(info os.FileInfo, mode os.FileMode) error {
	if !info.Mode().IsRegular() {
		return errors.New("must be a regular file")
	}
	if info.Mode().Perm() != mode {
		return fmt.Errorf("permissions must be %04o", mode)
	}
	return validateCurrentOwner(info)
}

func validateCurrentOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine file owner")
	}
	if int(stat.Uid) != os.Getuid() {
		return errors.New("must be owned by the current user")
	}
	return nil
}
