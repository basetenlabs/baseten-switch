package auth

import (
	"errors"
	"strings"
	"unicode"
)

const maxSavedAPIKeySize = 4096

type savedAPIKeyStore interface {
	Load(configPath string) (string, error)
	Save(configPath, key string) error
	Remove(configPath string) error
}

var savedAPIKeys = platformSavedAPIKeyStore()

// UseFileSavedAPIKeyStoreForTesting isolates callers from the system Keychain.
// Call it before starting test goroutines and restore after they have stopped.
func UseFileSavedAPIKeyStoreForTesting() func() {
	previous := savedAPIKeys
	savedAPIKeys = fileSavedAPIKeyStore{}
	return func() { savedAPIKeys = previous }
}

// ValidateSavedAPIKey normalizes pasted keys and rejects invalid header values.
func ValidateSavedAPIKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > maxSavedAPIKeySize || strings.ContainsFunc(key, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return "", errors.New("API key must contain 1 to 4096 characters without whitespace or control characters")
	}
	return key, nil
}

// LoadSavedAPIKey reads the Switch credential belonging to an exact config.
// Only an absent credential (or an unspecified config) permits another source.
func LoadSavedAPIKey(configPath string) (string, error) {
	if configPath == "" {
		return "", nil
	}
	return savedAPIKeys.Load(configPath)
}

// SaveSavedAPIKey replaces the credential in macOS Keychain, or an owner-only
// file on other platforms. It never falls back from Keychain to plaintext.
func SaveSavedAPIKey(configPath, key string) error {
	if configPath == "" {
		return errors.New("a config path is required to save an API key")
	}
	key, err := ValidateSavedAPIKey(key)
	if err != nil {
		return err
	}
	return savedAPIKeys.Save(configPath, key)
}

func RemoveSavedAPIKey(configPath string) error {
	if configPath == "" {
		return errors.New("a config path is required to remove an API key")
	}
	return savedAPIKeys.Remove(configPath)
}
