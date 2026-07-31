package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallDryRunIsReadOnlyAndEnumeratesActions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	called := 0
	steps := []uninstallStep{
		{description: "first action", run: func() error { called++; return nil }},
		{description: "second action", run: func() error { called++; return nil }},
	}
	var out bytes.Buffer
	if code := runUninstall(uninstallOptions{dryRun: true, purge: true, yes: true}, steps, &out); code != 0 {
		t.Fatalf("runUninstall = %d", code)
	}
	if called != 0 {
		t.Fatalf("dry run invoked %d actions", called)
	}
	for _, want := range []string{
		"would first action",
		"would second action",
		"would permanently remove current product data root",
		"no changes made",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out.String())
		}
	}
}

func TestUninstallHarnessStepsRestoreOnlyManagedState(t *testing.T) {
	claude, claudeOut := testAdapter(t)
	writeSettingsFile(t, claude, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com","OTHER":"keep"}}`)
	if code := claude.on(); code != 0 {
		t.Fatalf("claude on = %d (%s)", code, claudeOut.String())
	}

	codex, codexOut := testCodexAdapter(t)
	writeOverlayFile(t, codex, codexTestStaleOverlay)
	if code := codex.on(); code != 0 {
		t.Fatalf("codex on = %d (%s)", code, codexOut.String())
	}

	steps := []uninstallStep{
		{
			description: "restore Claude",
			run: func() error {
				return restoreWithRetainedBackup(claude.backupPath, claude.off)
			},
		},
		{
			description: "restore Codex",
			run: func() error {
				return restoreWithRetainedBackup(codex.backupPath, codex.off)
			},
		},
	}
	if code := runUninstall(uninstallOptions{}, steps, &bytes.Buffer{}); code != 0 {
		t.Fatalf("runUninstall = %d", code)
	}
	if got, _ := envValue(t, claude.settingsPath, claudeManagedEnvKey); got != "https://corp-proxy.example.com" {
		t.Errorf("Claude original value = %q", got)
	}
	if got := string(fileBytes(t, codex.overlayPath)); got != codexTestStaleOverlay {
		t.Errorf("Codex original overlay not restored exactly:\n%s", got)
	}
	for _, path := range []string{
		claude.backupPath + ".uninstall-retained",
		codex.backupPath + ".uninstall-retained",
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("consumed backup was not retained at %s: %v", path, err)
		}
	}

	foreignClaude, _ := testAdapter(t)
	foreignClaudeRaw := `{"env":{"ANTHROPIC_BASE_URL":"https://another.example.com"}}` + "\n"
	writeSettingsFile(t, foreignClaude, foreignClaudeRaw)
	if code := foreignClaude.off(); code != 0 {
		t.Fatalf("foreign claude off = %d", code)
	}
	if got := string(fileBytes(t, foreignClaude.settingsPath)); got != foreignClaudeRaw {
		t.Errorf("unowned Claude settings changed:\n%s", got)
	}

	foreignCodex, _ := testCodexAdapter(t)
	writeOverlayFile(t, foreignCodex, codexTestForeignOverlay)
	if code := foreignCodex.off(); code != 0 {
		t.Fatalf("foreign codex off = %d", code)
	}
	if got := string(fileBytes(t, foreignCodex.overlayPath)); got != codexTestForeignOverlay {
		t.Errorf("unowned Codex overlay changed:\n%s", got)
	}
}

func TestUninstallDefaultRetainsDataAndPurgeRemovesOnlyCurrentRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := basetenSwitchDataRoot()
	for path, content := range map[string]string{
		"gateway.yaml":            "version: 1\n",
		"env":                     "secret\n",
		"telemetry/segment.jsonl": "{}\n",
		"logs/router.log":         "log\n",
		"backups/claude.json":     "{}\n",
		"gateway.pid":             "123\n",
		"door.pid":                "456\n",
		"gateway.config-path":     "/tmp/gateway.yaml\n",
	} {
		writeTestFile(t, filepath.Join(root, path), content)
	}
	outside := filepath.Join(home, ".config", "another-product", "keep")
	writeTestFile(t, outside, "keep\n")

	if err := removeRuntimeResidue(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"gateway.yaml", "env", "telemetry/segment.jsonl", "logs/router.log", "backups/claude.json"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("retained path %s: %v", path, err)
		}
	}
	for _, path := range []string{"gateway.pid", "door.pid", "gateway.config-path"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Errorf("runtime path %s still exists: %v", path, err)
		}
	}

	// A second pass is a no-op.
	if err := removeRuntimeResidue(root); err != nil {
		t.Fatalf("idempotent cleanup: %v", err)
	}
	if err := purgeBasetenSwitchDataRoot(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("purged root still exists: %v", err)
	}
	if got := string(fileBytes(t, outside)); got != "keep\n" {
		t.Errorf("purge changed unrelated data: %q", got)
	}
	if err := purgeBasetenSwitchDataRoot(root); err != nil {
		t.Fatalf("idempotent purge: %v", err)
	}
}

func TestUninstallContinuesAfterPartialFailure(t *testing.T) {
	ranAfterFailure := false
	steps := []uninstallStep{
		{description: "failing action", run: func() error { return errors.New("boom") }},
		{description: "later action", run: func() error { ranAfterFailure = true; return nil }},
	}
	var out bytes.Buffer
	if code := runUninstall(uninstallOptions{}, steps, &out); code != 1 {
		t.Fatalf("runUninstall = %d", code)
	}
	if !ranAfterFailure {
		t.Fatal("later action did not run after failure")
	}
	if !strings.Contains(out.String(), "failing action: boom") ||
		!strings.Contains(out.String(), "1 incomplete step") {
		t.Errorf("partial failure not reported:\n%s", out.String())
	}
}

func TestUninstallPurgeRequiresExplicitNoninteractiveConfirmation(t *testing.T) {
	if _, err := parseUninstallOptions([]string{"--purge"}); err == nil ||
		!strings.Contains(err.Error(), "requires explicit confirmation") {
		t.Fatalf("parse --purge error = %v", err)
	}
	opts, err := parseUninstallOptions([]string{"--purge", "--yes"})
	if err != nil || !opts.purge || !opts.yes {
		t.Fatalf("parse --purge --yes = %#v, %v", opts, err)
	}
	opts, err = parseUninstallOptions([]string{"--dry-run", "--purge"})
	if err != nil || !opts.purge || !opts.dryRun {
		t.Fatalf("parse --dry-run --purge = %#v, %v", opts, err)
	}
}

// The app directory below has no Info.plist at all, so the managed
// identity check fails and removal stays manual (the same outcome a
// foreign bundle at the managed path must get).
func TestUninstallLeavesAppWhenLoginItemCannotBeSafelyUnregistered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldGOOS := menubarGOOS
	menubarGOOS = "darwin"
	t.Cleanup(func() { menubarGOOS = oldGOOS })
	app := filepath.Join(home, "Applications", menubarAppName)
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	err := uninstallManagedApp()
	if err == nil || !strings.Contains(err.Error(), "manual action required") ||
		!strings.Contains(err.Error(), "Start at Login") {
		t.Fatalf("app cleanup error = %v", err)
	}
	if _, err := os.Stat(app); err != nil {
		t.Fatalf("app was removed despite unresolved login item: %v", err)
	}
}

// stubManagedAppCommands points the runCmd seam at a plutil answer
// table, a code-signature result, and a pgrep result that finds no
// running menubar process.
func stubManagedAppCommands(t *testing.T, plist map[string]string, codesignErr error) {
	t.Helper()
	fields := map[string]string{
		"CFBundleIdentifier":         menubarBundleID,
		"CFBundleDisplayName":        "Baseten Switch",
		"CFBundleExecutable":         menubarProcName,
		"CFBundleShortVersionString": "0.2.0",
	}
	for key, value := range plist {
		fields[key] = value
	}
	old := runCmd
	runCmd = func(name string, args ...string) (string, error) {
		switch {
		case strings.HasSuffix(name, "plutil"):
			// plutil -extract <key> raw <path>
			if len(args) >= 4 {
				if v, ok := fields[args[1]]; ok {
					return v, nil
				}
			}
			return "", errors.New("plist key not found")
		case strings.HasSuffix(name, "codesign"):
			return "", codesignErr
		case strings.HasSuffix(name, "pgrep"):
			return "", errors.New("no processes matched")
		}
		return "", fmt.Errorf("unexpected command %s %v", name, args)
	}
	t.Cleanup(func() { runCmd = old })
}

// stubManagedAppPlist installs the valid command results shared by
// the managed-app removal tests.
func stubManagedAppPlist(t *testing.T, plist map[string]string) {
	t.Helper()
	stubManagedAppCommands(t, plist, nil)
}

func managedAppFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldGOOS := menubarGOOS
	menubarGOOS = "darwin"
	t.Cleanup(func() { menubarGOOS = oldGOOS })
	app := filepath.Join(home, "Applications", menubarAppName)
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(app, "Contents", "Info.plist"), "plist\n")
	executable := filepath.Join(app, "Contents", "MacOS", menubarProcName)
	writeTestFile(t, executable, "executable\n")
	if err := os.Chmod(executable, 0o700); err != nil {
		t.Fatal(err)
	}
	return app
}

// The managed bundle unregisters its own login item via the headless
// CLI mode and is then removed: the safe-removal path the step's
// description promises.
func TestUninstallRemovesManagedAppAfterLoginItemUnregister(t *testing.T) {
	app := managedAppFixture(t)
	stubManagedAppPlist(t, map[string]string{
		"CFBundleIdentifier":        menubarBundleID,
		"CFBundleExecutable":        menubarProcName,
		"BasetenSwitchLoginItemCLI": "true",
	})
	driven := false
	oldCLI := runManagedAppCLI
	runManagedAppCLI = func(appPath string, args ...string) (string, error) {
		driven = true
		if appPath != app || len(args) != 1 || args[0] != menubarLoginItemCLIFlag {
			t.Errorf("runManagedAppCLI(%q, %v), want (%q, [%s])", appPath, args, app, menubarLoginItemCLIFlag)
		}
		return "Start at Login enabled -> notRegistered", nil
	}
	t.Cleanup(func() { runManagedAppCLI = oldCLI })
	if err := uninstallManagedApp(); err != nil {
		t.Fatalf("uninstallManagedApp = %v", err)
	}
	if !driven {
		t.Fatal("the bundle's login-item CLI mode was not driven before removal")
	}
	if _, err := os.Lstat(app); !os.IsNotExist(err) {
		t.Fatalf("app still present after safe removal: %v", err)
	}
}

// A bundle with the managed identity but no login-item CLI marker
// predates headless control: keep the manual-removal requirement
// rather than launching an unknown UI mid-uninstall.
func TestUninstallAppManualRemovalWhenBundlePredatesCLIControl(t *testing.T) {
	app := managedAppFixture(t)
	stubManagedAppPlist(t, map[string]string{
		"CFBundleIdentifier": menubarBundleID,
		"CFBundleExecutable": menubarProcName,
	})
	driven := false
	oldCLI := runManagedAppCLI
	runManagedAppCLI = func(appPath string, args ...string) (string, error) {
		driven = true
		return "", nil
	}
	t.Cleanup(func() { runManagedAppCLI = oldCLI })
	err := uninstallManagedApp()
	if err == nil || !strings.Contains(err.Error(), "manual action required") ||
		!strings.Contains(err.Error(), "Start at Login") {
		t.Fatalf("app cleanup error = %v", err)
	}
	if driven {
		t.Error("a bundle without the CLI marker must never be exec'd")
	}
	if _, statErr := os.Stat(app); statErr != nil {
		t.Fatalf("app was removed despite missing CLI control: %v", statErr)
	}
}

// A failed unregister leaves the bundle in place with the manual
// instructions; removing it anyway would strand a live login item
// pointing at a deleted bundle.
func TestUninstallAppManualRemovalWhenUnregisterFails(t *testing.T) {
	app := managedAppFixture(t)
	stubManagedAppPlist(t, map[string]string{
		"CFBundleIdentifier":        menubarBundleID,
		"CFBundleExecutable":        menubarProcName,
		"BasetenSwitchLoginItemCLI": "true",
	})
	oldCLI := runManagedAppCLI
	runManagedAppCLI = func(appPath string, args ...string) (string, error) {
		return "", errors.New("exit status 1")
	}
	t.Cleanup(func() { runManagedAppCLI = oldCLI })
	err := uninstallManagedApp()
	if err == nil || !strings.Contains(err.Error(), "manual action required") ||
		!strings.Contains(err.Error(), "Start at Login") {
		t.Fatalf("app cleanup error = %v", err)
	}
	if _, statErr := os.Stat(app); statErr != nil {
		t.Fatalf("app was removed despite the failed unregister: %v", statErr)
	}
}

func TestUninstallAppRefusesInvalidBundleBeforeExecution(t *testing.T) {
	tests := []struct {
		name        string
		codesignErr error
		breakBundle func(*testing.T, string)
	}{
		{
			name:        "invalid code signature",
			codesignErr: errors.New("code object is not signed at all"),
		},
		{
			name: "symlink executable",
			breakBundle: func(t *testing.T, app string) {
				t.Helper()
				executable := filepath.Join(app, "Contents", "MacOS", menubarProcName)
				if err := os.Remove(executable); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "replacement")
				writeTestFile(t, target, "replacement\n")
				if err := os.Symlink(target, executable); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := managedAppFixture(t)
			if tt.breakBundle != nil {
				tt.breakBundle(t, app)
			}
			stubManagedAppCommands(t, map[string]string{
				"BasetenSwitchLoginItemCLI": "true",
			}, tt.codesignErr)
			driven := false
			oldCLI := runManagedAppCLI
			runManagedAppCLI = func(appPath string, args ...string) (string, error) {
				driven = true
				return "", nil
			}
			t.Cleanup(func() { runManagedAppCLI = oldCLI })

			err := uninstallManagedApp()
			if err == nil || !strings.Contains(err.Error(), "manual action required") ||
				!strings.Contains(err.Error(), "managed app validation") {
				t.Fatalf("app cleanup error = %v", err)
			}
			if driven {
				t.Error("an invalid bundle must never be exec'd")
			}
			if _, statErr := os.Lstat(app); statErr != nil {
				t.Fatalf("invalid bundle was removed: %v", statErr)
			}
		})
	}
}

func TestUninstallAppRevalidatesBundleBeforeRemoval(t *testing.T) {
	app := managedAppFixture(t)
	fields := map[string]string{
		"CFBundleIdentifier":         menubarBundleID,
		"CFBundleDisplayName":        "Baseten Switch",
		"CFBundleExecutable":         menubarProcName,
		"CFBundleShortVersionString": "0.2.0",
		"BasetenSwitchLoginItemCLI":  "true",
	}
	codesignCalls := 0
	oldRunCmd := runCmd
	runCmd = func(name string, args ...string) (string, error) {
		switch {
		case strings.HasSuffix(name, "plutil"):
			if len(args) >= 4 {
				if value, ok := fields[args[1]]; ok {
					return value, nil
				}
			}
			return "", errors.New("plist key not found")
		case strings.HasSuffix(name, "codesign"):
			codesignCalls++
			if codesignCalls == 2 {
				return "", errors.New("bundle changed after execution")
			}
			return "", nil
		case strings.HasSuffix(name, "pgrep"):
			return "", errors.New("no processes matched")
		}
		return "", fmt.Errorf("unexpected command %s %v", name, args)
	}
	t.Cleanup(func() { runCmd = oldRunCmd })
	driven := false
	oldCLI := runManagedAppCLI
	runManagedAppCLI = func(appPath string, args ...string) (string, error) {
		driven = true
		return "Start at Login enabled -> notRegistered", nil
	}
	t.Cleanup(func() { runManagedAppCLI = oldCLI })

	err := uninstallManagedApp()
	if err == nil || !strings.Contains(err.Error(), "manual action required") ||
		!strings.Contains(err.Error(), "code signature is invalid") {
		t.Fatalf("app cleanup error = %v", err)
	}
	if !driven {
		t.Fatal("the login-item CLI was not driven")
	}
	if codesignCalls != 2 {
		t.Fatalf("codesign validation calls = %d, want 2", codesignCalls)
	}
	if _, statErr := os.Lstat(app); statErr != nil {
		t.Fatalf("bundle was removed after failing revalidation: %v", statErr)
	}
}

// A regular file at the managed path is not a bundle and must not be
// removed by this step.
func TestUninstallAppRefusesNonBundle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldGOOS := menubarGOOS
	menubarGOOS = "darwin"
	t.Cleanup(func() { menubarGOOS = oldGOOS })
	app := filepath.Join(home, "Applications", menubarAppName)
	writeTestFile(t, app, "not a bundle\n")
	err := uninstallManagedApp()
	if err == nil || !strings.Contains(err.Error(), "not an app bundle") {
		t.Fatalf("non-bundle error = %v", err)
	}
	if _, statErr := os.Stat(app); statErr != nil {
		t.Fatalf("non-bundle path was removed: %v", statErr)
	}
}

func TestUninstallRejectsSymlinkCleanupTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := basetenSwitchDataRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	writeTestFile(t, outside, "keep\n")
	if err := os.Symlink(outside, filepath.Join(root, "gateway.pid")); err != nil {
		t.Fatal(err)
	}
	if err := removeRuntimeResidue(root); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("cleanup symlink error = %v", err)
	}
	if got := string(fileBytes(t, outside)); got != "keep\n" {
		t.Errorf("symlink target changed: %q", got)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
