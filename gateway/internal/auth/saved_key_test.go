package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSavedAPIKeyStore(t *testing.T) {
	t.Cleanup(UseFileSavedAPIKeyStoreForTesting())
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if key, err := LoadSavedAPIKey(path); key != "" || err != nil {
		t.Fatalf("missing key = %q, %v", key, err)
	}
	if err := SaveSavedAPIKey(path, "  synthetic-key  "); err != nil {
		t.Fatal(err)
	}
	if key, err := LoadSavedAPIKey(path); key != "synthetic-key" || err != nil {
		t.Fatalf("saved key = %q, %v", key, err)
	}
	info, err := os.Stat(path + ".api-key")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("saved mode: %v, %v", info, err)
	}
	for _, invalid := range []string{"", "  ", "bad key", "bad\nkey", "bad\x00key", strings.Repeat("x", 4097)} {
		if err := SaveSavedAPIKey(path, invalid); err == nil {
			t.Fatal("invalid key accepted")
		}
		if key, err := LoadSavedAPIKey(path); key != "synthetic-key" || err != nil {
			t.Fatal("invalid save changed previous key")
		}
	}
	if key, err := LoadSavedAPIKey(path + "-other"); key != "" || err != nil {
		t.Fatal("saved key crossed config boundary")
	}
	if err := SaveSavedAPIKey(path, "replacement"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSavedAPIKey(path); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSavedAPIKey(path); err != nil {
		t.Fatal(err)
	}
	if key, err := LoadSavedAPIKey(path); key != "" || err != nil {
		t.Fatal("removed key still available")
	}
	if err := SaveSavedAPIKey("", "synthetic-key"); err == nil {
		t.Fatal("saved with unspecified config")
	}
}

func TestSavedAPIKeyReadFailsClosed(t *testing.T) {
	t.Cleanup(UseFileSavedAPIKeyStoreForTesting())
	for _, body := range []string{"", "bad key", strings.Repeat("x", 4097)} {
		path := filepath.Join(t.TempDir(), "gateway.yaml")
		if err := os.WriteFile(path+".api-key", []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if key, err := LoadSavedAPIKey(path); key != "" || err == nil {
			t.Fatal("malformed saved key treated as absent")
		}
	}
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path+".api-key", []byte("synthetic-key"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSavedAPIKey(path); err == nil {
		t.Fatal("world-readable saved key accepted")
	}
	for _, targetExists := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "gateway.yaml")
		target := path + ".target"
		if targetExists {
			if err := os.WriteFile(target, []byte("synthetic-key"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(target, path+".api-key"); err != nil {
			t.Fatal(err)
		}
		if key, err := LoadSavedAPIKey(path); key != "" || err == nil {
			t.Fatal("saved-key symlink allowed credential fallback or use")
		}
		if err := SaveSavedAPIKey(path, "replacement"); err != nil {
			t.Fatal(err)
		}
		if key, err := LoadSavedAPIKey(path); key != "replacement" || err != nil {
			t.Fatal("could not repair saved-key symlink by replacing it")
		}
	}
}
