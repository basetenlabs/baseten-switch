package main

import (
	"bytes"
	"errors"
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
