package tracecapture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCorrelationKeyPersistsAndSeparatesDomains(t *testing.T) {
	dir := privateTempDir(t)
	first, err := LoadOrCreateCorrelationKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateCorrelationKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != second.ID() || len(first.ID()) != 16 {
		t.Fatalf("key IDs differ or have wrong length: %q %q", first.ID(), second.ID())
	}
	session, err := first.Hash("claude-code", "session", "same")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := first.Hash("claude-code", "turn", "same")
	if err != nil {
		t.Fatal(err)
	}
	if session == turn || len(session) != 32 {
		t.Fatalf("domain separation failed: %q %q", session, turn)
	}
	info, err := os.Stat(filepath.Join(dir, correlationKeyName))
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != correlationKeySize {
		t.Fatalf("unsafe key file: info=%v err=%v", info, err)
	}
}

func TestCorrelationKeyRejectsSymlinkAndMissingKeyInNonemptyStore(t *testing.T) {
	dir := privateTempDir(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, make([]byte, correlationKeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, correlationKeyName)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCorrelationKey(dir); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}

	dir = privateTempDir(t)
	segment := filepath.Join(dir, "trace-content-2026-08-17-001.jsonl")
	if err := os.WriteFile(segment, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCorrelationKey(dir); err == nil || !strings.Contains(err.Error(), "nonempty") {
		t.Fatalf("expected nonempty store rejection, got %v", err)
	}
}
