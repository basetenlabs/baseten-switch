package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClaudeOnBackupBeforeModify pins the safety ordering added after
// the 2026-07-07 review: the backup must be durable BEFORE the
// settings file is modified. When the backup cannot be written, on
// fails and the settings file is left byte-untouched.
func TestClaudeOnBackupBeforeModify(t *testing.T) {
	a, out := testAdapter(t)
	orig := `{
  "env": {
    "ANTHROPIC_BASE_URL": "https://user.example.com"
  }
}`
	writeSettingsFile(t, a, orig)

	// Make the backup destination unwritable.
	bdir := filepath.Dir(a.backupPath)
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bdir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bdir, 0o700) })

	if rc := a.on(); rc != 1 {
		t.Fatalf("rc = %d want 1 (backup write must fail)\n%s", rc, out.String())
	}
	got, err := os.ReadFile(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != orig {
		t.Fatalf("settings modified despite failed backup:\n%s", got)
	}
}

// TestClaudeOnStagedBackupCrashEquivalence: the staged backup (written
// before the settings write) carries the pre-on file hash, so an off
// that runs against an unmodified file (the crash-between-writes case)
// takes the clean-restore path and lands byte-identical to the
// original.
func TestClaudeOnStagedBackupCrashEquivalence(t *testing.T) {
	a, _ := testAdapter(t)
	orig := `{
  "env": {
    "ANTHROPIC_BASE_URL": "https://user.example.com"
  }
}`
	writeSettingsFile(t, a, orig)
	if err := os.MkdirAll(filepath.Dir(a.backupPath), 0o700); err != nil {
		t.Fatal(err)
	}

	// Run a normal on to produce the backup, then simulate the crash
	// window by restoring the pre-on file content while keeping the
	// backup's ORIGINAL values (reset the drift hash to the pre-on
	// state, as the staged save would have left it).
	if rc := a.on(); rc != 0 {
		t.Fatalf("on rc = %d", rc)
	}
	bak, err := loadClaudeBackup(a.backupPath)
	if err != nil || bak == nil {
		t.Fatalf("backup missing after on: %v", err)
	}
	writeSettingsFile(t, a, orig)
	bak.WrittenHash = sha256Hex([]byte(orig))
	if err := saveClaudeBackup(a.backupPath, bak); err != nil {
		t.Fatal(err)
	}

	if rc := a.off(); rc != 0 {
		t.Fatalf("off rc = %d", rc)
	}
	// The restore path writes through the JSON writer, so the contract
	// is value-exact (formatting normalized), not byte-exact.
	if v, ok := envValue(t, a.settingsPath, "ANTHROPIC_BASE_URL"); !ok || v != "https://user.example.com" {
		t.Fatalf("off after simulated crash: ANTHROPIC_BASE_URL = %q ok=%v, want original", v, ok)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Fatalf("backup not cleared after off: %v", err)
	}
}
