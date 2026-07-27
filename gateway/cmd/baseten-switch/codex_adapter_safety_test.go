package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestCodexOnBackupBeforeModify pins the safety ordering the adapter
// contract requires: the backup must be durable BEFORE the overlay is
// modified. When the backup cannot be written, on fails and the
// pre-existing overlay is left byte-untouched.
func TestCodexOnBackupBeforeModify(t *testing.T) {
	a, out := testCodexAdapter(t)
	writeOverlayFile(t, a, codexTestStaleOverlay)

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
	if got := string(fileBytes(t, a.overlayPath)); got != codexTestStaleOverlay {
		t.Fatalf("overlay modified despite failed backup:\n%s", got)
	}
}

// TestCodexOnStagedBackupCrashEquivalence: the staged backup (written
// before the overlay write) carries the pre-on file hash, so an off
// that runs against an unmodified file (the crash-between-writes case)
// takes the clean-restore path and lands byte-identical to the
// original.
func TestCodexOnStagedBackupCrashEquivalence(t *testing.T) {
	a, _ := testCodexAdapter(t)
	writeOverlayFile(t, a, codexTestStaleOverlay)
	if err := os.MkdirAll(filepath.Dir(a.backupPath), 0o700); err != nil {
		t.Fatal(err)
	}

	// Run a normal on to produce the backup, then simulate the crash
	// window by restoring the pre-on file content while resetting the
	// drift hash to the pre-on state, as the staged save would have
	// left it.
	if rc := a.on(); rc != 0 {
		t.Fatalf("on rc = %d", rc)
	}
	bak, err := loadCodexBackup(a.backupPath)
	if err != nil || bak == nil {
		t.Fatalf("backup missing after on: %v", err)
	}
	writeOverlayFile(t, a, codexTestStaleOverlay)
	bak.WrittenHash = sha256Hex([]byte(codexTestStaleOverlay))
	if err := saveCodexBackup(a.backupPath, bak); err != nil {
		t.Fatal(err)
	}

	if rc := a.off(); rc != 0 {
		t.Fatalf("off rc = %d", rc)
	}
	// Whole-file restore: the contract is byte-exact.
	if got := string(fileBytes(t, a.overlayPath)); got != codexTestStaleOverlay {
		t.Fatalf("off after simulated crash not byte-identical:\n%s", got)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Fatalf("backup not cleared after off: %v", err)
	}
}

// TestCodexOffAfterCrashBeforeCreateIsNoop covers the crash window for
// the created-file case: the staged backup exists (existed=false), the
// overlay was never written. off must clear the backup, create
// nothing, and warn about nothing.
func TestCodexOffAfterCrashBeforeCreateIsNoop(t *testing.T) {
	a, out := testCodexAdapter(t)
	nb := &codexBackup{
		ConfigPath:  a.overlayPath,
		Existed:     false,
		WrittenHash: sha256Hex(nil),
	}
	if err := saveCodexBackup(a.backupPath, nb); err != nil {
		t.Fatal(err)
	}
	if rc := a.off(); rc != 0 {
		t.Fatalf("off rc = %d (%s)", rc, out.String())
	}
	if _, err := os.Stat(a.overlayPath); !os.IsNotExist(err) {
		t.Errorf("off created an overlay file")
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("backup not cleared")
	}
	if bytes.Contains(out.Bytes(), []byte("warning")) {
		t.Errorf("crash-window off must be a quiet no-op, got %q", out.String())
	}
}
