package safefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReplaceCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Exists {
		t.Fatal("missing path reported as existing")
	}

	committed, err := snapshot.Replace([]byte("created\n"), 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if !committed.Exists || string(committed.Data) != "created\n" {
		t.Fatalf("committed snapshot = %+v", committed)
	}
	if got := committed.Mode.Perm(); got != 0o640 {
		t.Fatalf("created mode = %o, want 640", got)
	}
}

func TestReplacePreservesExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, path, []byte("before\n"), 0o644)

	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := snapshot.Replace([]byte("after\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if got := committed.Mode.Perm(); got != 0o644 {
		t.Fatalf("replacement mode = %o, want 644", got)
	}
}

func TestReplacePreservesRelativeMultihopSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link test requires link privileges")
	}
	root := t.TempDir()
	target := filepath.Join(root, "store", "settings.json")
	writeFile(t, target, []byte("before\n"), 0o600)
	if err := os.Symlink(filepath.Join("store", "settings.json"), filepath.Join(root, "middle")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("middle", filepath.Join(root, "configured")); err != nil {
		t.Fatal(err)
	}

	configured := filepath.Join(root, "configured")
	snapshot, err := Read(configured)
	if err != nil {
		t.Fatal(err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Linked || !snapshot.FinalLinked || snapshot.ResolvedPath != resolvedTarget {
		t.Fatalf("snapshot path = %+v", snapshot)
	}
	if _, err := snapshot.Replace([]byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(configured); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("configured symlink was replaced: info=%v err=%v", info, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "after\n" {
		t.Fatalf("target = %q, err=%v", got, err)
	}
}

func TestReplaceCreatesThroughSymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link test requires link privileges")
	}
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "configured")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "configured", "nested", "settings.json")

	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Exists || !snapshot.Linked || snapshot.FinalLinked {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if _, err := snapshot.Replace([]byte("created\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(realDir, "nested", "settings.json"))
	if err != nil || string(got) != "created\n" {
		t.Fatalf("resolved target = %q, err=%v", got, err)
	}
}

func TestReplaceRejectsSymlinkRetargetRaceAndCleansStage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link test requires link privileges")
	}
	root := t.TempDir()
	first := filepath.Join(root, "first.json")
	second := filepath.Join(root, "second.json")
	writeFile(t, first, []byte("first\n"), 0o600)
	writeFile(t, second, []byte("second\n"), 0o600)
	configured := filepath.Join(root, "configured")
	if err := os.Symlink("first.json", configured); err != nil {
		t.Fatal(err)
	}

	snapshot, err := Read(configured)
	if err != nil {
		t.Fatal(err)
	}
	beforeCommitHook = func() {
		if err := os.Remove(configured); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("second.json", configured); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { beforeCommitHook = nil }()

	if _, err := snapshot.Replace([]byte("replacement\n"), 0o600); !errors.Is(err, ErrConflict) {
		t.Fatalf("Replace error = %v, want ErrConflict", err)
	}
	assertFile(t, first, "first\n")
	assertFile(t, second, "second\n")
	assertNoStages(t, root)
}

func TestReplaceRejectsPreimageRaceAndCleansStage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "settings.json")
	writeFile(t, path, []byte("before\n"), 0o600)
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeCommitHook = func() {
		writeFile(t, path, []byte("concurrent\n"), 0o600)
	}
	defer func() { beforeCommitHook = nil }()

	if _, err := snapshot.Replace([]byte("replacement\n"), 0o600); !errors.Is(err, ErrConflict) {
		t.Fatalf("Replace error = %v, want ErrConflict", err)
	}
	assertFile(t, path, "concurrent\n")
	assertNoStages(t, root)
}

func TestReadRejectsMultiplyHardLinkedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "settings.json")
	alias := filepath.Join(root, "alias.json")
	writeFile(t, path, []byte("value\n"), 0o600)
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}

	if _, err := Read(path); !errors.Is(err, ErrHardLinked) {
		t.Fatalf("Read error = %v, want ErrHardLinked", err)
	}
}

func TestReadRejectsDanglingAndCyclicLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link test requires link privileges")
	}
	t.Run("dangling", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "configured")
		if err := os.Symlink("missing.json", path); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(path); !errors.Is(err, ErrUnsafeTarget) {
			t.Fatalf("Read error = %v, want ErrUnsafeTarget", err)
		}
	})
	t.Run("cyclic", func(t *testing.T) {
		root := t.TempDir()
		first := filepath.Join(root, "first")
		second := filepath.Join(root, "second")
		if err := os.Symlink("second", first); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("first", second); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(first); !errors.Is(err, ErrUnsafeTarget) {
			t.Fatalf("Read error = %v, want ErrUnsafeTarget", err)
		}
	})
}

func TestReadRejectsNonRegularTarget(t *testing.T) {
	if _, err := Read(t.TempDir()); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("Read error = %v, want ErrUnsafeTarget", err)
	}
}

func TestMissingTargetDoesNotClobberConcurrentCreation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "settings.json")
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeCommitHook = func() {
		writeFile(t, path, []byte("concurrent\n"), 0o600)
	}
	defer func() { beforeCommitHook = nil }()

	if _, err := snapshot.Replace([]byte("replacement\n"), 0o600); !errors.Is(err, ErrConflict) {
		t.Fatalf("Replace error = %v, want ErrConflict", err)
	}
	assertFile(t, path, "concurrent\n")
	assertNoStages(t, root)
}

func TestRemoveCreatedRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	missing, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := missing.Replace([]byte("created\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed path still exists or returned wrong error: %v", err)
	}
}

func TestRemoveThroughSymlinkPreservesConfiguredLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link test requires link privileges")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	configured := filepath.Join(root, "configured")
	writeFile(t, target, []byte("value\n"), 0o600)
	if err := os.Symlink("target.json", configured); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Read(configured)
	if err != nil {
		t.Fatal(err)
	}

	if err := snapshot.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("resolved target still exists or returned wrong error: %v", err)
	}
	info, err := os.Lstat(configured)
	if err != nil {
		t.Fatalf("configured symlink was removed: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("configured path mode = %v, want symbolic link", info.Mode())
	}
}

func TestRemoveRefusesPreimageConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, path, []byte("before\n"), 0o600)
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, []byte("concurrent\n"), 0o600)

	if err := snapshot.Remove(); !errors.Is(err, ErrConflict) {
		t.Fatalf("Remove error = %v, want ErrConflict", err)
	}
	assertFile(t, path, "concurrent\n")
}

func TestRemoveRefusesHardLinkAddedAfterSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "settings.json")
	alias := filepath.Join(root, "alias.json")
	writeFile(t, path, []byte("value\n"), 0o600)
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}

	err = snapshot.Remove()
	if !errors.Is(err, ErrConflict) || !errors.Is(err, ErrHardLinked) {
		t.Fatalf("Remove error = %v, want ErrConflict and ErrHardLinked", err)
	}
	assertFile(t, path, "value\n")
	assertFile(t, alias, "value\n")
}

func TestVerifyRefusesPreimageConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, path, []byte("before\n"), 0o600)
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, []byte("concurrent\n"), 0o600)

	if err := snapshot.Verify(); !errors.Is(err, ErrConflict) {
		t.Fatalf("Verify error = %v, want ErrConflict", err)
	}
	assertFile(t, path, "concurrent\n")
}

func writeFile(t *testing.T, path string, data []byte, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertNoStages(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".safefile-") {
			t.Fatalf("staged file was not removed: %s", entry.Name())
		}
	}
}
