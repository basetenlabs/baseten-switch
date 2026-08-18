package tracecapture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRuntimePathsAndOwnershipMarker(t *testing.T) {
	root := privateTempDir(t)
	configPath := filepath.Join(root, "gateway.yaml")
	if err := os.WriteFile(configPath, []byte("global: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := ResolveRuntimePaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths.RuntimeID) != 16 || !strings.HasPrefix(filepath.Base(paths.TraceDir), "traces-") {
		t.Fatalf("unexpected runtime paths: %+v", paths)
	}
	if err := EnsureRuntimeTraceStore(paths); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRuntimeTraceStore(paths); err != nil {
		t.Fatal(err)
	}
	markerInfo, err := os.Stat(filepath.Join(paths.TraceDir, ownershipMarkerName))
	if err != nil || markerInfo.Mode().Perm() != 0o600 {
		t.Fatalf("unsafe marker: %v %v", markerInfo, err)
	}

	other := paths
	other.RuntimeID = "0123456789abcdef"
	if other.RuntimeID == paths.RuntimeID {
		other.RuntimeID = "fedcba9876543210"
	}
	if err := ValidateRuntimeTraceStore(other); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected marker mismatch, got %v", err)
	}
}

func TestPurgeRuntimeTraceStoreIsTargetedAndLockAware(t *testing.T) {
	root := privateTempDir(t)
	paths := RuntimePaths{
		RuntimeID: "0123456789abcdef",
		TraceDir:  filepath.Join(root, "traces-0123456789abcdef"),
		ExportDir: filepath.Join(root, "exports-0123456789abcdef"),
	}
	if err := EnsureRuntimeTraceStore(paths); err != nil {
		t.Fatal(err)
	}
	segment := filepath.Join(paths.TraceDir, "trace-content-2026-08-17-001.jsonl")
	unknown := filepath.Join(paths.TraceDir, "operator-note.txt")
	if err := os.WriteFile(segment, []byte("trace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCorrelationKey(paths.TraceDir); err == nil {
		t.Fatal("key creation should reject a nonempty store")
	}
	lock, err := acquireWriterLock(paths.TraceDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PurgeRuntimeTraceStore(paths); !errors.Is(err, ErrStoreLocked) {
		t.Fatalf("purge ignored active lock: %v", err)
	}
	if err := releaseWriterLock(lock); err != nil {
		t.Fatal(err)
	}
	result, err := PurgeRuntimeTraceStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedFiles != 1 || result.RemovedBytes != 6 {
		t.Fatalf("unexpected purge result: %+v", result)
	}
	if _, err := os.Stat(segment); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recognized segment retained: %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown file removed: %v", err)
	}
	if err := ValidateRuntimeTraceStore(paths); err != nil {
		t.Fatalf("purge removed ownership state: %v", err)
	}
}

func TestPurgeRefusesWhileWriterActive(t *testing.T) {
	root := privateTempDir(t)
	paths := RuntimePaths{
		RuntimeID: "0123456789abcdef",
		TraceDir:  filepath.Join(root, "traces-0123456789abcdef"),
		ExportDir: filepath.Join(root, "exports-0123456789abcdef"),
	}
	if err := EnsureRuntimeTraceStore(paths); err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(Config{Dir: paths.TraceDir, RetentionDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PurgeRuntimeTraceStore(paths); !errors.Is(err, ErrStoreLocked) {
		t.Fatalf("expected active writer refusal, got %v", err)
	}
	w.Close(context.Background())
}
