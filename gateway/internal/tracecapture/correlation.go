package tracecapture

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"
)

const (
	correlationKeyName = ".correlation-key"
	correlationDomain  = "baseten-switch/native-correlation/v1"
	correlationKeySize = 32
)

type CorrelationKey struct {
	bytes [correlationKeySize]byte
	id    string
}

func (k *CorrelationKey) ID() string {
	if k == nil {
		return ""
	}
	return k.id
}

func (k *CorrelationKey) Hash(client, fieldName, normalizedValue string) (string, error) {
	if k == nil {
		return "", errors.New("correlation key is unavailable")
	}
	if client == "" || fieldName == "" || normalizedValue == "" {
		return "", errors.New("correlation client, field, and value must not be empty")
	}
	if !utf8.ValidString(normalizedValue) || len(normalizedValue) > 4<<10 {
		return "", errors.New("correlation value must be valid UTF-8 and at most 4 KiB")
	}
	mac := hmac.New(sha256.New, k.bytes[:])
	_, _ = mac.Write([]byte(correlationDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(client))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(fieldName))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(normalizedValue))
	return hex.EncodeToString(mac.Sum(nil)[:16]), nil
}

// LoadOrCreateCorrelationKey returns the installation-local join key. A
// missing key is created only while the store contains no trace rows.
func LoadOrCreateCorrelationKey(dir string) (*CorrelationKey, error) {
	if err := ensurePrivateDirectory(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, correlationKeyName)
	key, err := loadCorrelationKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	segments, discoverErr := DiscoverSegments(dir)
	if discoverErr != nil {
		return nil, discoverErr
	}
	for _, segment := range segments {
		if segment.Size > 0 {
			return nil, errors.New("correlation key is missing from a nonempty trace store")
		}
	}
	var raw [correlationKeySize]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return nil, fmt.Errorf("generate correlation key: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".correlation-key.tmp-")
	if err != nil {
		return nil, fmt.Errorf("create temporary correlation key: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("secure temporary correlation key: %w", err)
	}
	if _, err := temporary.Write(raw[:]); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("write temporary correlation key: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("sync temporary correlation key: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close temporary correlation key: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return loadCorrelationKey(path)
		}
		return nil, fmt.Errorf("publish correlation key: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return nil, err
	}
	return newCorrelationKey(raw[:]), nil
}

// LoadCorrelationKey opens an existing local join key without creating one.
// Read-only consumers such as the package command use this so inspection can
// never create or replace capture state.
func LoadCorrelationKey(dir string) (*CorrelationKey, error) {
	return loadCorrelationKey(filepath.Join(dir, correlationKeyName))
}

func loadCorrelationKey(path string) (*CorrelationKey, error) {
	file, openedInfo, err := openPrivateFileNoFollow(path, syscall.O_RDONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open correlation key: %w", err)
	}
	defer file.Close()
	if openedInfo.Size() != correlationKeySize {
		return nil, fmt.Errorf("correlation key must be exactly %d bytes", correlationKeySize)
	}
	raw := make([]byte, correlationKeySize)
	if _, err := io.ReadFull(file, raw); err != nil {
		return nil, fmt.Errorf("read correlation key: %w", err)
	}
	return newCorrelationKey(raw), nil
}

func newCorrelationKey(raw []byte) *CorrelationKey {
	key := &CorrelationKey{}
	copy(key.bytes[:], raw)
	digest := sha256.Sum256(raw)
	key.id = hex.EncodeToString(digest[:8])
	return key
}

func syncDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open trace directory for sync: %w", err)
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync trace directory: %w", err)
	}
	return nil
}
