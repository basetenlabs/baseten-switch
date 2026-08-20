package tracecapture

import (
	"os"
	"path/filepath"
	"testing"
)

func testRuntimePaths(t *testing.T) RuntimePaths {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "gateway.yaml")
	if err := os.WriteFile(configPath, []byte("global: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := ResolveRuntimePaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestRuntimeExportStoreStatusCleanupAndPurge(t *testing.T) {
	paths := testRuntimePaths(t)
	if err := EnsureRuntimeExportStore(paths); err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(paths.ExportDir, "baseten-switch-traces-20260817T120000Z.zip")
	if err := os.WriteFile(packagePath, []byte("zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage, err := os.MkdirTemp(paths.ExportDir, ".baseten-switch-package-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "member"), []byte("sensitive"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := QuarantineExportStaging(paths, stage)
	if err != nil {
		t.Fatal(err)
	}
	status, err := InspectRuntimeExports(paths)
	if err != nil {
		t.Fatal(err)
	}
	if status.PackageCount != 1 || status.PackageBytes != 3 || status.QuarantineCount != 1 {
		t.Fatalf("status = %#v", status)
	}
	if err := CleanupExportQuarantine(paths, id); err != nil {
		t.Fatal(err)
	}
	result, err := PurgeRuntimeExports(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedPackages != 1 || result.RemovedQuarantine != 0 || result.RemovedBytes != 3 {
		t.Fatalf("purge = %#v", result)
	}
}

func TestRuntimeExportStoreRejectsSymlinkedPackage(t *testing.T) {
	paths := testRuntimePaths(t)
	if err := EnsureRuntimeExportStore(paths); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(paths.ExportDir, "baseten-switch-traces-20260817T120000Z.zip")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRuntimeExports(paths); err == nil {
		t.Fatal("InspectRuntimeExports accepted a recognized symlink")
	}
}

func TestQuarantineRejectsUnownedStagingPath(t *testing.T) {
	paths := testRuntimePaths(t)
	if err := EnsureRuntimeExportStore(paths); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), ".baseten-switch-package-outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := QuarantineExportStaging(paths, outside); err == nil {
		t.Fatal("QuarantineExportStaging accepted an outside path")
	}
}
