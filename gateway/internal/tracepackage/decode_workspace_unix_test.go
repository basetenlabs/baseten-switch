//go:build darwin || linux

package tracepackage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDecodeWorkspacePublishDoesNotReplaceDestination(t *testing.T) {
	parent := resolvedTempDir(t)
	output := filepath.Join(parent, "decoded")
	workspace, err := createDecodeWorkspace(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := workspace.cleanup(); err != nil {
			t.Fatal(err)
		}
	}()
	if err := decodeWritePrivateFile(workspace.root, "payload.txt", []byte("decoded")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(output, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(output, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.publish(); !errors.Is(err, ErrDecodeDestinationExists) {
		t.Fatalf("publish error = %v", err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "keep" {
		t.Fatalf("destination was replaced: %q, %v", data, err)
	}
}

func TestDecodeWorkspaceCleanupDoesNotDeleteReplacementPath(t *testing.T) {
	parent := resolvedTempDir(t)
	output := filepath.Join(parent, "decoded")
	workspace, err := createDecodeWorkspace(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeWritePrivateFile(workspace.root, "sensitive.txt", []byte("remove")); err != nil {
		t.Fatal(err)
	}
	movedStage := filepath.Join(parent, "moved-stage")
	if err := os.Rename(workspace.stagePath, movedStage); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace.stagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(workspace.stagePath, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = workspace.cleanup()
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("cleanup error = %v", err)
	}
	data, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(data) != "keep" {
		t.Fatalf("replacement path was deleted: %q, %v", data, readErr)
	}
	entries, readDirErr := os.ReadDir(movedStage)
	if readDirErr != nil {
		t.Fatal(readDirErr)
	}
	if len(entries) != 0 {
		t.Fatalf("original staging contents were not scrubbed: %v", entries)
	}
}

func TestDecodeWorkspacePublishRejectsReplacedParentPath(t *testing.T) {
	root := resolvedTempDir(t)
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(parent, "decoded")
	workspace, err := createDecodeWorkspace(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := workspace.cleanup(); err != nil {
			t.Fatal(err)
		}
	}()
	if err := decodeWritePrivateFile(workspace.root, "payload.txt", []byte("decoded")); err != nil {
		t.Fatal(err)
	}
	movedParent := filepath.Join(root, "moved-parent")
	if err := os.Rename(parent, movedParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := workspace.publish(); err == nil || !strings.Contains(err.Error(), "directory path changed") {
		t.Fatalf("publish error = %v", err)
	}
	for _, candidate := range []string{filepath.Join(parent, "decoded"), filepath.Join(movedParent, "decoded")} {
		if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
			t.Fatalf("output was published at %s: %v", candidate, err)
		}
	}
}

func TestMaterializeDecodeRejectsSameContentSourceReplacement(t *testing.T) {
	source := createDecodeFixture(t)
	plan, err := InspectDecode(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, source+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(resolvedTempDir(t), "decoded")
	if _, err := MaterializeDecode(context.Background(), plan, output); err == nil || !strings.Contains(err.Error(), "changed after inspection") {
		t.Fatalf("materialize error = %v", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output was created: %v", err)
	}
}

func TestDecodeRejectsSymlinkedSourceAndOutputParent(t *testing.T) {
	source := createDecodeFixture(t)
	sourceLink := filepath.Join(filepath.Dir(source), "source-link.zip")
	if err := os.Symlink(source, sourceLink); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectDecode(context.Background(), sourceLink); err == nil {
		t.Fatal("symlinked source package was accepted")
	}

	root := resolvedTempDir(t)
	realParent := filepath.Join(root, "real-parent")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDecodeOutputPath(filepath.Join(linkedParent, "decoded")); err == nil {
		t.Fatal("symlinked output parent was accepted")
	}
}
