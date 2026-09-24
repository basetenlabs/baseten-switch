package auth

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type fileSavedAPIKeyStore struct{}

type savedAPIKeyFile struct {
	key  string
	info os.FileInfo
}

func savedAPIKeyFileOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0
}

func readSavedAPIKeyFile(configPath string) (savedAPIKeyFile, error) {
	path := configPath + ".api-key"
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return savedAPIKeyFile{}, nil
	}
	if err != nil {
		return savedAPIKeyFile{}, errors.New("could not read saved API key")
	}
	if !savedAPIKeyFileOwned(before) {
		return savedAPIKeyFile{}, errors.New("saved API key must be an owner-only regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return savedAPIKeyFile{}, errors.New("could not read saved API key")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !savedAPIKeyFileOwned(info) || !os.SameFile(before, info) {
		return savedAPIKeyFile{}, errors.New("saved API key must be an owner-only regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSavedAPIKeySize+1))
	if err != nil || len(data) > maxSavedAPIKeySize {
		return savedAPIKeyFile{}, errors.New("could not read saved API key")
	}
	key, err := ValidateSavedAPIKey(string(data))
	if err != nil {
		return savedAPIKeyFile{}, errors.New("saved API key is invalid")
	}
	return savedAPIKeyFile{key: key, info: info}, nil
}

func (fileSavedAPIKeyStore) Load(configPath string) (string, error) {
	file, err := readSavedAPIKeyFile(configPath)
	return file.key, err
}

func (fileSavedAPIKeyStore) Save(configPath, key string) error {
	path := configPath + ".api-key"
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("could not save API key")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".api-key-*")
	if err != nil {
		return errors.New("could not save API key")
	}
	defer os.Remove(f.Name())
	if _, err = io.WriteString(f, key); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("could not save API key")
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return errors.New("could not save API key")
	}
	return nil
}

func (fileSavedAPIKeyStore) Remove(configPath string) error {
	if err := os.Remove(configPath + ".api-key"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not remove saved API key")
	}
	return nil
}

// The lock serializes Switch writers. Recheck the file too, since an older
// Switch version or another program may have changed it during migration.
func removeUnchangedSavedAPIKeyFile(configPath string, previous savedAPIKeyFile) error {
	current, err := readSavedAPIKeyFile(configPath)
	if err != nil {
		return err
	}
	if current.info == nil {
		return nil
	}
	if previous.info == nil || !os.SameFile(previous.info, current.info) || previous.key != current.key {
		return errors.New("saved API key file changed; retry the operation")
	}
	return (fileSavedAPIKeyStore{}).Remove(configPath)
}

// Explicit replacement/removal may repair an invalid or insecure legacy file.
// Inspect its identity without reading through symlinks, then recheck before
// unlinking. Directories and entries owned by another user are never removed.
func snapshotSavedAPIKeyFile(configPath string) (os.FileInfo, error) {
	info, err := os.Lstat(configPath + ".api-key")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("could not inspect saved API key file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || (!info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0) {
		return nil, errors.New("saved API key file must be owned by the current user and cannot be a directory")
	}
	return info, nil
}

func removeSavedAPIKeySnapshot(configPath string, previous os.FileInfo) error {
	current, err := snapshotSavedAPIKeyFile(configPath)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if previous == nil || !os.SameFile(previous, current) || previous.Size() != current.Size() || previous.Mode() != current.Mode() || !previous.ModTime().Equal(current.ModTime()) {
		return errors.New("saved API key file changed; retry the operation")
	}
	return (fileSavedAPIKeyStore{}).Remove(configPath)
}
