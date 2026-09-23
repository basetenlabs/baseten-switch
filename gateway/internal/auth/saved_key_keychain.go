package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const savedAPIKeyService = "com.baseten.switch.saved-api-key"

var errSavedAPIKeyNotFound = errors.New("saved API key not found")
var errSavedAPIKeyTooLong = errors.New("API key exceeds the macOS Keychain storage limit")

type savedAPIKeyKeychain interface {
	Get(account string) (string, error)
	Set(account, key string) error
	Delete(account string) error
}

type keychainSavedAPIKeyStore struct {
	keychain savedAPIKeyKeychain
	lockDir  string
}

func savedAPIKeyAccount(configPath string) string {
	digest := sha256.Sum256([]byte(configPath))
	return "config-" + hex.EncodeToString(digest[:])
}

func (s keychainSavedAPIKeyStore) Load(configPath string) (string, error) {
	path, unlock, err := lockSavedAPIKey(configPath, s.lockDir)
	if err != nil {
		return "", err
	}
	defer unlock()
	account := savedAPIKeyAccount(path)
	key, err := s.keychain.Get(account)
	if err != nil && !errors.Is(err, errSavedAPIKeyNotFound) {
		return "", errors.New("could not read saved API key from macOS Keychain; unlock Keychain and allow access, then retry")
	}
	legacy, legacyErr := readSavedAPIKeyFile(path)
	if legacyErr != nil {
		return "", legacyErr
	}
	if errors.Is(err, errSavedAPIKeyNotFound) {
		if legacy.info == nil {
			return "", nil
		}
		key = legacy.key
		if err := s.writeVerified(account, key); err != nil {
			return "", err
		}
	} else {
		validated, validationErr := ValidateSavedAPIKey(key)
		if validationErr != nil || validated != key {
			return "", errors.New("saved API key in macOS Keychain is invalid")
		}
		if legacy.info != nil && legacy.key != key {
			return "", errors.New("Keychain and legacy saved API key differ; save or remove the key to resolve")
		}
	}
	if err := removeUnchangedSavedAPIKeyFile(path, legacy); err != nil {
		return "", err
	}
	return key, nil
}

func (s keychainSavedAPIKeyStore) Save(configPath, key string) error {
	path, unlock, err := lockSavedAPIKey(configPath, s.lockDir)
	if err != nil {
		return err
	}
	defer unlock()
	legacy, err := snapshotSavedAPIKeyFile(path)
	if err != nil {
		return err
	}
	if err := s.writeVerified(savedAPIKeyAccount(path), key); err != nil {
		return err
	}
	return removeSavedAPIKeySnapshot(path, legacy)
}

func (s keychainSavedAPIKeyStore) writeVerified(account, key string) error {
	if err := s.keychain.Set(account, key); err != nil {
		if errors.Is(err, errSavedAPIKeyTooLong) {
			return errSavedAPIKeyTooLong
		}
		return errors.New("could not save API key in macOS Keychain; unlock Keychain and allow access, then retry")
	}
	stored, err := s.keychain.Get(account)
	if err != nil || stored != key {
		return errors.New("could not verify saved API key in macOS Keychain; retry before changing credentials")
	}
	return nil
}

func (s keychainSavedAPIKeyStore) Remove(configPath string) error {
	path, unlock, err := lockSavedAPIKey(configPath, s.lockDir)
	if err != nil {
		return err
	}
	defer unlock()
	// Delete the migration source first so a successful Keychain deletion cannot
	// be undone by the gateway's next load, including after a process crash.
	legacy, err := snapshotSavedAPIKeyFile(path)
	if err != nil {
		return err
	}
	if err := removeSavedAPIKeySnapshot(path, legacy); err != nil {
		return err
	}
	account := savedAPIKeyAccount(path)
	if err := s.keychain.Delete(account); err != nil && !errors.Is(err, errSavedAPIKeyNotFound) {
		return errors.New("could not remove saved API key from macOS Keychain; unlock Keychain and allow access, then retry")
	}
	if _, err := s.keychain.Get(account); !errors.Is(err, errSavedAPIKeyNotFound) {
		return errors.New("could not verify API key removal from macOS Keychain; retry before changing credentials")
	}
	return nil
}

// A stable lock file coordinates the app gateway and separate CLI processes.
// The lock contains no credential and is never removed while a process runs.
func lockSavedAPIKey(configPath, lockDir string) (string, func(), error) {
	path, err := filepath.Abs(configPath)
	if err != nil {
		return "", nil, errors.New("could not resolve API key config path")
	}
	if lockDir == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return "", nil, errors.New("could not locate saved API key lock directory")
		}
		lockDir = filepath.Join(cacheDir, "baseten-switch", "auth-locks")
	}
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		return "", nil, errors.New("could not lock saved API key")
	}
	directory, err := os.Lstat(lockDir)
	if err != nil {
		return "", nil, errors.New("could not inspect saved API key lock directory")
	}
	stat, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !directory.IsDir() || directory.Mode().Perm()&0077 != 0 {
		return "", nil, errors.New("saved API key lock directory must be owned by the current user and owner-only")
	}
	lockPath := filepath.Join(lockDir, savedAPIKeyAccount(path)+".lock")
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", nil, errors.New("could not lock saved API key")
	}
	file := os.NewFile(uintptr(fd), lockPath)
	info, err := file.Stat()
	if err != nil || !savedAPIKeyFileOwned(info) {
		file.Close()
		return "", nil, errors.New("saved API key lock must be an owner-only regular file")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return path, func() { file.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) || !time.Now().Before(deadline) {
			file.Close()
			return "", nil, errors.New("saved API key is busy; retry the operation")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
