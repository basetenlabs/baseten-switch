package auth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMacOSSavedAPIKeyRejectsOversizedCommandBeforeKeychainAccess(t *testing.T) {
	if err := (macOSKeychain{}).Set(savedAPIKeyAccount("synthetic-config"), strings.Repeat("x", maxSavedAPIKeySize)); !errors.Is(err, errSavedAPIKeyTooLong) {
		t.Fatal("oversized key did not fail before native access")
	}
}

// This test creates and deletes only a disposable Keychain in an explicitly
// approved guest. Ordinary test runs never read or change the system Keychain.
func TestNativeSavedAPIKeyKeychain(t *testing.T) {
	if os.Getenv("BASETEN_SWITCH_NATIVE_KEYCHAIN_TEST") != "isolated-tart-guest" {
		t.Skip("requires an explicitly approved isolated Tart guest")
	}
	security := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "/usr/bin/security", args...).Output()
		if err != nil {
			t.Fatalf("disposable Keychain setup or cleanup failed: %s", args[0])
		}
		return strings.TrimSpace(string(output))
	}
	originalSearchList := security("list-keychains", "-d", "user")
	keychainPath := filepath.Join(t.TempDir(), "synthetic-test.keychain-db")
	security("create-keychain", "-p", "", keychainPath)
	t.Cleanup(func() {
		security("delete-keychain", keychainPath)
		if actual := security("list-keychains", "-d", "user"); actual != originalSearchList {
			t.Error("disposable Keychain cleanup did not restore the original search list")
		} else {
			t.Log("original Keychain search list restored")
		}
	})
	keychain := macOSKeychain{path: keychainPath, timeout: 2 * time.Second}
	store := keychainSavedAPIKeyStore{keychain: keychain, lockDir: filepath.Join(t.TempDir(), "locks")}
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if key, err := store.Load(path); key != "" || err != nil {
		t.Fatal("unlocked missing item did not permit normal authentication")
	}
	if err := store.Save(path, "synthetic-native-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".api-key"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native save created a plaintext key file")
	}
	// A new backend instance represents another process reading the same item.
	reopened := keychainSavedAPIKeyStore{keychain: macOSKeychain{path: keychainPath, timeout: 2 * time.Second}, lockDir: store.lockDir}
	if key, err := reopened.Load(path); key != "synthetic-native-one" || err != nil {
		t.Fatal("fresh backend could not read the saved item")
	}
	if key, err := store.Load(path + "-other"); key != "" || err != nil {
		t.Fatal("native Keychain entries crossed config boundaries")
	}
	if err := store.Save(path, "synthetic-native-two"); err != nil {
		t.Fatal(err)
	}
	security("lock-keychain", keychainPath)
	started := time.Now()
	if key, err := store.Load(path); key != "" || err == nil {
		t.Fatal("locked saved item permitted credential fallback")
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("locked Keychain read exceeded its bounded deadline")
	}
	// macOS can inspect item metadata while locked. Record whether an absent
	// item is distinguishable; it must never return a credential.
	if key, err := store.Load(path + "-missing"); key != "" {
		t.Fatal("locked missing item returned a credential")
	} else if err == nil {
		t.Log("locked Keychain distinguishes an absent item")
	} else {
		t.Log("locked Keychain denies lookup of an absent item")
	}
	security("unlock-keychain", "-p", "", keychainPath)
	if key, err := store.Load(path); key != "synthetic-native-two" || err != nil {
		t.Fatal("unlock did not recover the saved credential")
	}
	if err := store.Remove(path); err != nil {
		t.Fatal(err)
	}
	if key, err := store.Load(path); key != "" || err != nil {
		t.Fatal("native remove did not restore normal authentication")
	}
	if err := os.WriteFile(path+".api-key", []byte("synthetic-migrated"), 0600); err != nil {
		t.Fatal(err)
	}
	if key, err := store.Load(path); key != "synthetic-migrated" || err != nil {
		t.Fatal("native migration failed")
	}
	if _, err := os.Stat(path + ".api-key"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native migration retained the plaintext file")
	}
	if err := store.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Log("native create, read, replace, config isolation, locked denial, unlock, remove and migration passed")
}
