package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeSavedAPIKeyKeychain struct {
	mu                        sync.Mutex
	values                    map[string]string
	getErr, setErr, deleteErr error
	afterSet                  func()
	readback                  *string
	setCalls                  int
}

func newFakeSavedAPIKeyKeychain() *fakeSavedAPIKeyKeychain {
	return &fakeSavedAPIKeyKeychain{values: make(map[string]string)}
}
func (f *fakeSavedAPIKeyKeychain) Get(account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return "", f.getErr
	}
	if f.readback != nil {
		return *f.readback, nil
	}
	key, ok := f.values[account]
	if !ok {
		return "", errSavedAPIKeyNotFound
	}
	return key, nil
}
func (f *fakeSavedAPIKeyKeychain) Set(account, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}
	f.values[account] = key
	if f.afterSet != nil {
		f.afterSet()
	}
	return nil
}
func (f *fakeSavedAPIKeyKeychain) Delete(account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.values[account]; !ok {
		return errSavedAPIKeyNotFound
	}
	delete(f.values, account)
	return nil
}

func TestKeychainSavedAPIKeyConfigIsolationAndRemoval(t *testing.T) {
	backend := newFakeSavedAPIKeyKeychain()
	store := keychainSavedAPIKeyStore{keychain: backend, lockDir: filepath.Join(t.TempDir(), "locks")}
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	other := path + "-other"
	if key, err := store.Load(path); key != "" || err != nil {
		t.Fatalf("missing key returned credential or error: %v", err)
	}
	if err := store.Save(path, "synthetic-one"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(other, "synthetic-two"); err != nil {
		t.Fatal(err)
	}
	for config, want := range map[string]string{path: "synthetic-one", other: "synthetic-two"} {
		got, err := store.Load(config)
		if got != want || err != nil {
			t.Fatalf("config did not use its own key: %v", err)
		}
		if _, err := os.Lstat(config + ".api-key"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("Keychain save wrote a plaintext credential")
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(relative); err != nil || got != "synthetic-one" {
		t.Fatal("relative path used a different entry")
	}
	if err := os.WriteFile(path+".api-key", []byte("synthetic-legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(path); got != "" || err != nil {
		t.Fatal("removed credential was migrated back")
	}
	if got, err := store.Load(other); got != "synthetic-two" || err != nil {
		t.Fatal("removal crossed config boundary")
	}
}

func TestKeychainSavedAPIKeyMigratesAfterVerifiedWrite(t *testing.T) {
	backend := newFakeSavedAPIKeyKeychain()
	store := keychainSavedAPIKeyStore{keychain: backend, lockDir: filepath.Join(t.TempDir(), "locks")}
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path+".api-key", []byte("synthetic-legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	backend.afterSet = func() {
		if _, err := os.Stat(path + ".api-key"); err != nil {
			t.Fatal("file deleted before verification")
		}
	}
	if got, err := store.Load(path); got != "synthetic-legacy" || err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	if _, err := os.Lstat(path + ".api-key"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verified legacy credential remained on disk")
	}
	if got, err := store.Load(path); got != "synthetic-legacy" || err != nil {
		t.Fatal("migration was not durable")
	}
	if backend.setCalls != 1 {
		t.Fatal("loading migrated credential rewrote Keychain")
	}
}

func TestKeychainSavedAPIKeyMigrationFailurePreservesFile(t *testing.T) {
	for _, failure := range []string{"read", "write", "readback", "mismatch", "file-replaced", "file-rewritten"} {
		t.Run(failure, func(t *testing.T) {
			backend := newFakeSavedAPIKeyKeychain()
			store := keychainSavedAPIKeyStore{keychain: backend, lockDir: filepath.Join(t.TempDir(), "locks")}
			path := filepath.Join(t.TempDir(), "gateway.yaml")
			file := path + ".api-key"
			if err := os.WriteFile(file, []byte("synthetic-legacy"), 0600); err != nil {
				t.Fatal(err)
			}
			secretError := errors.New("synthetic-sensitive-backend-error")
			switch failure {
			case "read":
				backend.getErr = secretError
			case "write":
				backend.setErr = secretError
			case "readback":
				backend.afterSet = func() { backend.getErr = secretError }
			case "mismatch":
				backend.afterSet = func() { wrong := "synthetic-wrong-value"; backend.readback = &wrong }
			case "file-replaced", "file-rewritten":
				backend.afterSet = func() {
					if failure == "file-replaced" {
						if err := os.Remove(file); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(file, []byte("synthetic-new-value"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if key, err := store.Load(path); err == nil || key != "" || strings.Contains(err.Error(), secretError.Error()) {
				t.Fatal("migration did not fail closed with safe error")
			}
			contents, err := os.ReadFile(file)
			if err != nil || len(contents) == 0 {
				t.Fatal("failed migration removed legacy key")
			}
			backend.getErr, backend.setErr, backend.afterSet, backend.readback = nil, nil, nil, nil
			if failure == "file-replaced" || failure == "file-rewritten" {
				if _, err := store.Load(path); err == nil {
					t.Fatal("conflicting keys silently reconciled")
				}
				if err := store.Save(path, "synthetic-reconciled"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.Load(path); err != nil {
				t.Fatalf("migration could not be retried: %v", err)
			}
		})
	}
}

func TestKeychainSavedAPIKeyRejectsUnsafeLegacyFiles(t *testing.T) {
	for _, kind := range []string{"empty", "malformed", "oversized", "public", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			backend := newFakeSavedAPIKeyKeychain()
			store := keychainSavedAPIKeyStore{keychain: backend, lockDir: filepath.Join(t.TempDir(), "locks")}
			path := filepath.Join(t.TempDir(), "gateway.yaml")
			file := path + ".api-key"
			var err error
			switch kind {
			case "empty":
				err = os.WriteFile(file, nil, 0600)
			case "malformed":
				err = os.WriteFile(file, []byte("invalid key"), 0600)
			case "oversized":
				err = os.WriteFile(file, []byte(strings.Repeat("x", 4097)), 0600)
			case "public":
				err = os.WriteFile(file, []byte("synthetic-key"), 0644)
			case "symlink":
				err = os.Symlink(path+".target", file)
			case "directory":
				err = os.Mkdir(file, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if key, err := store.Load(path); key != "" || err == nil {
				t.Fatal("unsafe file used or treated as absent")
			}
			if backend.setCalls != 0 {
				t.Fatal("unsafe file migrated into Keychain")
			}
		})
	}
}

func TestKeychainSavedAPIKeyRemovalFailureCannotRemigrate(t *testing.T) {
	backend := newFakeSavedAPIKeyKeychain()
	store := keychainSavedAPIKeyStore{keychain: backend, lockDir: filepath.Join(t.TempDir(), "locks")}
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := store.Save(path, "synthetic-key"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".api-key", []byte("synthetic-legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	backend.deleteErr = errors.New("synthetic-secret-error")
	if err := store.Remove(path); err == nil || strings.Contains(err.Error(), backend.deleteErr.Error()) {
		t.Fatal("delete failure hidden or leaked detail")
	}
	if _, err := os.Lstat(path + ".api-key"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed deletion retained migration source")
	}
	backend.deleteErr = nil
	if err := store.Remove(path); err != nil {
		t.Fatal(err)
	}
	if key, err := store.Load(path); key != "" || err != nil {
		t.Fatal("retry resurrected removed key")
	}
}

func TestKeychainSavedAPIKeyOperationsSerialize(t *testing.T) {
	backend := newFakeSavedAPIKeyKeychain()
	store := keychainSavedAPIKeyStore{keychain: backend, lockDir: filepath.Join(t.TempDir(), "locks")}
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if err := store.Save(path, "synthetic-key"); err != nil {
				t.Error(err)
			}
			if _, err := store.Load(path); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if backend.setCalls != 8 {
		t.Fatal("concurrent writes were dropped")
	}
}

func TestKeychainSavedAPIKeyReplacementRepairsLegacyFile(t *testing.T) {
	for _, kind := range []string{"empty", "malformed", "oversized", "public", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			backend := newFakeSavedAPIKeyKeychain()
			store := keychainSavedAPIKeyStore{keychain: backend, lockDir: filepath.Join(t.TempDir(), "locks")}
			path := filepath.Join(t.TempDir(), "gateway.yaml")
			file := path + ".api-key"
			target := path + ".target"
			var err error
			switch kind {
			case "empty":
				err = os.WriteFile(file, nil, 0600)
			case "malformed":
				err = os.WriteFile(file, []byte("invalid key"), 0600)
			case "oversized":
				err = os.WriteFile(file, []byte(strings.Repeat("x", 4097)), 0600)
			case "public":
				err = os.WriteFile(file, []byte("synthetic-key"), 0644)
			case "symlink":
				if err := os.WriteFile(target, []byte("unrelated-synthetic-file"), 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(target, file)
			}
			if err != nil {
				t.Fatal(err)
			}
			backend.setErr = errors.New("synthetic-write-denied")
			if err := store.Save(path, "synthetic-replacement"); err == nil {
				t.Fatal("failed Keychain write was treated as saved")
			}
			if _, err := os.Lstat(file); err != nil {
				t.Fatal("failed write removed existing file")
			}
			backend.setErr = nil
			if err := store.Save(path, "synthetic-replacement"); err != nil {
				t.Fatal(err)
			}
			if key, err := store.Load(path); key != "synthetic-replacement" || err != nil {
				t.Fatal("replacement could not repair invalid legacy storage")
			}
			if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("verified replacement retained invalid legacy file")
			}
			if kind == "symlink" {
				if data, err := os.ReadFile(target); err != nil || string(data) != "unrelated-synthetic-file" {
					t.Fatal("replacement changed the symlink target")
				}
			}
		})
	}
}

func TestKeychainSavedAPIKeyRefusesUnsafeLock(t *testing.T) {
	for _, kind := range []string{"symlink", "public"} {
		t.Run(kind, func(t *testing.T) {
			backend := newFakeSavedAPIKeyKeychain()
			store := keychainSavedAPIKeyStore{keychain: backend, lockDir: filepath.Join(t.TempDir(), "locks")}
			if err := os.Mkdir(store.lockDir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "gateway.yaml")
			var err error
			if kind == "symlink" {
				err = os.Symlink(path+".target", filepath.Join(store.lockDir, savedAPIKeyAccount(path)+".lock"))
			} else {
				err = os.WriteFile(filepath.Join(store.lockDir, savedAPIKeyAccount(path)+".lock"), nil, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if key, err := store.Load(path); key != "" || err == nil {
				t.Fatal("unsafe lock permitted fallback")
			}
			if err := store.Save(path, "synthetic-key"); err == nil {
				t.Fatal("unsafe lock permitted key mutation")
			}
			if backend.setCalls != 0 {
				t.Fatal("unsafe lock reached Keychain")
			}
		})
	}
}

func TestKeychainSavedAPIKeyReadOnlyConfigDirectory(t *testing.T) {
	backend := newFakeSavedAPIKeyKeychain()
	store := keychainSavedAPIKeyStore{keychain: backend, lockDir: filepath.Join(t.TempDir(), "locks")}
	configDir := t.TempDir()
	path := filepath.Join(configDir, "gateway.yaml")
	if err := os.WriteFile(path, []byte("version: 1\n"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(configDir, 0700) })
	if key, err := store.Load(path); key != "" || err != nil {
		t.Fatalf("read-only config blocked normal CLI authentication: %v", err)
	}
	if err := store.Save(path, "synthetic-key"); err != nil {
		t.Fatalf("read-only config blocked Keychain storage: %v", err)
	}
	if key, err := store.Load(path); key != "synthetic-key" || err != nil {
		t.Fatal("read-only config blocked the saved Keychain credential")
	}
	if err := store.Remove(path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil || len(entries) != 1 {
		t.Fatal("Keychain operations wrote into the config directory")
	}
}

func TestKeychainSavedAPIKeyRefusesUnsafeLockDirectory(t *testing.T) {
	for _, kind := range []string{"public", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			backend := newFakeSavedAPIKeyKeychain()
			lockDir := filepath.Join(t.TempDir(), "locks")
			var err error
			if kind == "public" {
				err = os.Mkdir(lockDir, 0755)
			} else {
				err = os.Symlink(t.TempDir(), lockDir)
			}
			if err != nil {
				t.Fatal(err)
			}
			store := keychainSavedAPIKeyStore{keychain: backend, lockDir: lockDir}
			if _, err := store.Load(filepath.Join(t.TempDir(), "gateway.yaml")); err == nil {
				t.Fatal("unsafe lock directory was accepted")
			}
		})
	}
}
