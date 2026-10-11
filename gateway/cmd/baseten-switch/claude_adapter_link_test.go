package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func requireSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link test requires link privileges")
	}
}

func TestClaudeOnOffPreservesLinkedSettings(t *testing.T) {
	requireSymlinks(t)
	cases := []struct {
		name string
		link func(t *testing.T, requested, target string)
	}{
		{
			name: "absolute",
			link: func(t *testing.T, requested, target string) {
				t.Helper()
				if err := os.Symlink(target, requested); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "relative multihop",
			link: func(t *testing.T, requested, target string) {
				t.Helper()
				middle := filepath.Join(filepath.Dir(requested), "settings.shared")
				if err := os.Symlink(filepath.Base(target), middle); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(middle), requested); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, out := testAdapter(t)
			if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
			original := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://proxy.example.com","CLAUDE_CODE_ATTRIBUTION_HEADER":"custom-before","ENABLE_TOOL_SEARCH":"auto:25","OTHER":"keep"},"theme":"dark"}`)
			if err := os.WriteFile(target, original, 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0o640); err != nil {
				t.Fatal(err)
			}
			tc.link(t, a.settingsPath, target)
			linkText, err := os.Readlink(a.settingsPath)
			if err != nil {
				t.Fatal(err)
			}

			if code := a.on(); code != 0 {
				t.Fatalf("on = %d (%s)", code, out.String())
			}
			if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
				t.Fatalf("on changed link: text=%q err=%v, want %q", got, err, linkText)
			}
			assertClaudeOnValues(t, a)
			if got, _ := envValue(t, target, "OTHER"); got != "keep" {
				t.Fatalf("unrelated env after on = %q", got)
			}
			if got := readTree(t, target)["theme"]; got != "dark" {
				t.Fatalf("unrelated setting after on = %v", got)
			}
			if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o640 {
				t.Fatalf("linked target mode after on = %v, err=%v", st.Mode().Perm(), err)
			}
			bak, err := loadClaudeBackup(a.backupPath)
			if err != nil || bak == nil {
				t.Fatalf("load backup: bak=%v err=%v", bak, err)
			}
			wantResolved, err := filepath.EvalSymlinks(a.settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if bak.ResolvedPath != wantResolved || bak.WrittenIdentity == nil {
				t.Fatalf("backup target metadata = resolved %q identity %v, want %q", bak.ResolvedPath, bak.WrittenIdentity, wantResolved)
			}
			for _, key := range claudeOnEnvKeys {
				if !backupCovers(bak, key) {
					t.Errorf("backup does not cover %s", key)
				}
			}

			if code := a.off(); code != 0 {
				t.Fatalf("off = %d (%s)", code, out.String())
			}
			if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
				t.Fatalf("off changed link: text=%q err=%v, want %q", got, err, linkText)
			}
			for key, want := range map[string]string{
				claudeManagedEnvKey:     "https://proxy.example.com",
				claudeAttributionEnvKey: "custom-before",
				claudeToolSearchEnvKey:  "auto:25",
				"OTHER":                 "keep",
			} {
				if got, ok := envValue(t, target, key); !ok || got != want {
					t.Errorf("%s after off = %q, ok=%v, want %q", key, got, ok, want)
				}
			}
			if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o640 {
				t.Fatalf("linked target mode after off = %v, err=%v", st.Mode().Perm(), err)
			}
		})
	}
}

func TestClaudeLinkedSettingsRefusalsLeaveTopologyUntouched(t *testing.T) {
	t.Run("hard linked target", func(t *testing.T) {
		a, out := testAdapter(t)
		if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(filepath.Dir(a.settingsPath), "shared.json")
		original := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://proxy.example.com","CLAUDE_CODE_ATTRIBUTION_HEADER":"custom","ENABLE_TOOL_SEARCH":"auto"}}`)
		if err := os.WriteFile(target, original, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(target, a.settingsPath); err != nil {
			t.Fatal(err)
		}
		if code := a.on(); code != 1 {
			t.Fatalf("on = %d, want refusal (%s)", code, out.String())
		}
		if got := fileBytes(t, target); !bytes.Equal(got, original) {
			t.Fatalf("hard-linked target changed: %s", got)
		}
		if got := fileBytes(t, a.settingsPath); !bytes.Equal(got, original) {
			t.Fatalf("hard-linked requested path changed: %s", got)
		}
		if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
			t.Fatalf("backup survived refused mutation: %v", err)
		}
	})

	t.Run("dangling final symlink", func(t *testing.T) {
		requireSymlinks(t)
		a, out := testAdapter(t)
		if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
			t.Fatal(err)
		}
		linkText := "missing.json"
		if err := os.Symlink(linkText, a.settingsPath); err != nil {
			t.Fatal(err)
		}
		if code := a.on(); code != 1 {
			t.Fatalf("on = %d, want refusal (%s)", code, out.String())
		}
		if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
			t.Fatalf("dangling link changed: text=%q err=%v", got, err)
		}
		if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
			t.Fatalf("backup written for dangling link: %v", err)
		}
	})
}

func TestClaudeSettingsSnapshotRejectsConcurrentReplacement(t *testing.T) {
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://proxy.example.com"}}`)
	root, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	env, err := settingsEnv(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range claudeOnEnvKeys {
		env[key] = a.desiredClaudeEnvValue(key)
	}
	concurrent := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://concurrent.example.com","CLAUDE_CODE_ATTRIBUTION_HEADER":"concurrent","ENABLE_TOOL_SEARCH":"manual"}}`)
	replacement := a.settingsPath + ".concurrent"
	if err := os.WriteFile(replacement, concurrent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, a.settingsPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeClaudeSettings(snap, root); err == nil {
		t.Fatal("write through stale snapshot succeeded")
	}
	if got := fileBytes(t, a.settingsPath); !bytes.Equal(got, concurrent) {
		t.Fatalf("stale snapshot overwrote concurrent edit: %s", got)
	}
}

func TestClaudeSettingsSnapshotRejectsLinkRetarget(t *testing.T) {
	requireSymlinks(t)
	a, _ := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(filepath.Dir(a.settingsPath), "first.json")
	second := filepath.Join(filepath.Dir(a.settingsPath), "second.json")
	firstRaw := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://first.example.com"}}`)
	secondRaw := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://second.example.com"}}`)
	if err := os.WriteFile(first, firstRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, secondRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(first), a.settingsPath); err != nil {
		t.Fatal(err)
	}
	root, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	env, err := settingsEnv(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range claudeOnEnvKeys {
		env[key] = a.desiredClaudeEnvValue(key)
	}
	if err := os.Remove(a.settingsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(second), a.settingsPath); err != nil {
		t.Fatal(err)
	}

	if _, _, err := writeClaudeSettings(snap, root); err == nil {
		t.Fatal("write through retargeted link succeeded")
	}
	if got := fileBytes(t, first); !bytes.Equal(got, firstRaw) {
		t.Fatalf("original target changed: %s", got)
	}
	if got := fileBytes(t, second); !bytes.Equal(got, secondRaw) {
		t.Fatalf("retargeted target changed: %s", got)
	}
}

func TestClaudeOffRefusesRetargetedSettingsLink(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(filepath.Dir(a.settingsPath), "first.json")
	second := filepath.Join(filepath.Dir(a.settingsPath), "second.json")
	original := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://first.example.com","CLAUDE_CODE_ATTRIBUTION_HEADER":"first","ENABLE_TOOL_SEARCH":"first"},"owner":"first"}`)
	unrelated := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","CLAUDE_CODE_ATTRIBUTION_HEADER":"0","ENABLE_TOOL_SEARCH":"true"},"owner":"second"}`)
	if err := os.WriteFile(first, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, unrelated, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(first), a.settingsPath); err != nil {
		t.Fatal(err)
	}
	if code := a.on(); code != 0 {
		t.Fatalf("on = %d (%s)", code, out.String())
	}
	firstAfterOn := fileBytes(t, first)
	if err := os.Remove(a.settingsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(second), a.settingsPath); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if code := a.off(); code != 1 {
		t.Fatalf("off = %d, want retarget refusal (%s)", code, out.String())
	}
	if got := fileBytes(t, first); !bytes.Equal(got, firstAfterOn) {
		t.Fatalf("original target changed: %s", got)
	}
	if got := fileBytes(t, second); !bytes.Equal(got, unrelated) {
		t.Fatalf("retargeted file changed: %s", got)
	}
	if _, err := os.Stat(a.backupPath); err != nil {
		t.Fatalf("retarget refusal cleared backup: %v", err)
	}
}

func TestClaudeOnRejectsLinkRetargetAfterBackup(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(filepath.Dir(a.settingsPath), "first.json")
	second := filepath.Join(filepath.Dir(a.settingsPath), "second.json")
	firstRaw := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://first.example.com","CLAUDE_CODE_ATTRIBUTION_HEADER":"first","ENABLE_TOOL_SEARCH":"first"}}`)
	secondRaw := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://second.example.com","CLAUDE_CODE_ATTRIBUTION_HEADER":"second","ENABLE_TOOL_SEARCH":"second"}}`)
	if err := os.WriteFile(first, firstRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, secondRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(first), a.settingsPath); err != nil {
		t.Fatal(err)
	}
	oldHook := claudeBeforeSettingsMutation
	claudeBeforeSettingsMutation = func() {
		if err := os.Remove(a.settingsPath); err != nil {
			panic(err)
		}
		if err := os.Symlink(filepath.Base(second), a.settingsPath); err != nil {
			panic(err)
		}
	}
	t.Cleanup(func() { claudeBeforeSettingsMutation = oldHook })

	if code := a.on(); code != 1 {
		t.Fatalf("on = %d, want retarget conflict (%s)", code, out.String())
	}
	if got := fileBytes(t, first); !bytes.Equal(got, firstRaw) {
		t.Fatalf("original target changed: %s", got)
	}
	if got := fileBytes(t, second); !bytes.Equal(got, secondRaw) {
		t.Fatalf("retargeted target changed: %s", got)
	}
	if got, err := os.Readlink(a.settingsPath); err != nil || got != filepath.Base(second) {
		t.Fatalf("retargeted link changed: text=%q err=%v", got, err)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Fatalf("staged backup survived retarget conflict: %v", err)
	}
}

func TestClaudeOnConcurrentWriteRestoresLegacyBackupBeforeRetry(t *testing.T) {
	a, out := testAdapter(t)
	original := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"}}`)
	writeSettingsFile(t, a, string(original))
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(original),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}
	concurrent := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","CLAUDE_CODE_ATTRIBUTION_HEADER":"concurrent","ENABLE_TOOL_SEARCH":"manual","OTHER":"keep"}}`)
	oldHook := claudeBeforeSettingsMutation
	claudeBeforeSettingsMutation = func() {
		if err := os.WriteFile(a.settingsPath, concurrent, 0o600); err != nil {
			panic(err)
		}
	}
	t.Cleanup(func() { claudeBeforeSettingsMutation = oldHook })

	if code := a.on(); code != 1 {
		t.Fatalf("on = %d, want conflict (%s)", code, out.String())
	}
	if got := fileBytes(t, a.settingsPath); !bytes.Equal(got, concurrent) {
		t.Fatalf("concurrent settings lost: %s", got)
	}
	rolledBack, err := loadClaudeBackup(a.backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if backupCovers(rolledBack, claudeAttributionEnvKey) || backupCovers(rolledBack, claudeToolSearchEnvKey) {
		t.Fatalf("failed mutation left stale additive backup: %+v", rolledBack)
	}

	claudeBeforeSettingsMutation = nil
	out.Reset()
	if code := a.on(); code != 0 {
		t.Fatalf("retry on = %d (%s)", code, out.String())
	}
	assertClaudeOnValues(t, a)
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	for key, want := range map[string]string{
		claudeManagedEnvKey:     "https://legacy.example.com",
		claudeAttributionEnvKey: "concurrent",
		claudeToolSearchEnvKey:  "manual",
		"OTHER":                 "keep",
	} {
		if got, ok := envValue(t, a.settingsPath, key); !ok || got != want {
			t.Errorf("%s after retry/off = %q, ok=%v, want %q", key, got, ok, want)
		}
	}
}

func TestConcurrentClaudeOnKeepsRecoveryState(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://proxy.example.com","CLAUDE_CODE_ATTRIBUTION_HEADER":"custom","ENABLE_TOOL_SEARCH":"manual"}}`)
	a2 := *a
	out2 := &bytes.Buffer{}
	a2.out = out2

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	oldHook := claudeBeforeSettingsMutation
	claudeBeforeSettingsMutation = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	t.Cleanup(func() { claudeBeforeSettingsMutation = oldHook })

	firstResult := make(chan int, 1)
	secondResult := make(chan int, 1)
	go func() { firstResult <- a.on() }()
	<-entered
	go func() { secondResult <- a2.on() }()
	close(release)
	if code := <-firstResult; code != 0 {
		t.Fatalf("first on = %d (%s)", code, out.String())
	}
	if code := <-secondResult; code != 0 {
		t.Fatalf("second on = %d (%s)", code, out2.String())
	}
	if _, err := os.Stat(a.backupPath); err != nil {
		t.Fatalf("concurrent on lost backup: %v", err)
	}
	assertClaudeOnValues(t, a)
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	for key, want := range map[string]string{
		claudeManagedEnvKey:     "https://proxy.example.com",
		claudeAttributionEnvKey: "custom",
		claudeToolSearchEnvKey:  "manual",
	} {
		if got, ok := envValue(t, a.settingsPath, key); !ok || got != want {
			t.Errorf("%s after concurrent on/off = %q, ok=%v, want %q", key, got, ok, want)
		}
	}
}

func TestClaudeOnOffCreatesAndRemovesThroughSymlinkedParent(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	configuredDir := filepath.Dir(a.settingsPath)
	realDir := configuredDir + ".real"
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configuredDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, configuredDir); err != nil {
		t.Fatal(err)
	}

	if code := a.on(); code != 0 {
		t.Fatalf("on = %d (%s)", code, out.String())
	}
	target := filepath.Join(realDir, filepath.Base(a.settingsPath))
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("linked-parent target not created: %v", err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("linked-parent target not removed: %v", err)
	}
	if info, err := os.Lstat(configuredDir); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("configured parent symlink changed: info=%v err=%v", info, err)
	}
}

func TestClaudeLegacyLinkedBackupOnOffMigration(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
	managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"},"theme":"dark"}`)
	if err := os.WriteFile(target, managed, 0o600); err != nil {
		t.Fatal(err)
	}
	linkText := filepath.Base(target)
	if err := os.Symlink(linkText, a.settingsPath); err != nil {
		t.Fatal(err)
	}
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(managed),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}

	if code := a.on(); code != 0 {
		t.Fatalf("on = %d (%s)", code, out.String())
	}
	if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
		t.Fatalf("on changed settings link: text=%q err=%v, want %q", got, err, linkText)
	}
	assertClaudeOnValues(t, a)
	if got, ok := envValue(t, target, "OTHER"); !ok || got != "keep" {
		t.Fatalf("unrelated env after on = %q, ok=%v", got, ok)
	}
	if got := readTree(t, target)["theme"]; got != "dark" {
		t.Fatalf("unrelated setting after on = %v", got)
	}
	upgraded, err := loadClaudeBackup(a.backupPath)
	if err != nil || upgraded == nil {
		t.Fatalf("load upgraded backup: bak=%v err=%v", upgraded, err)
	}
	_, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.ResolvedPath != snap.ResolvedPath || upgraded.WrittenIdentity == nil || *upgraded.WrittenIdentity != snap.Identity {
		t.Fatalf("upgraded provenance = resolved %q identity %v, want %q %+v", upgraded.ResolvedPath, upgraded.WrittenIdentity, snap.ResolvedPath, snap.Identity)
	}
	if upgraded.WrittenHash != sha256Hex(snap.Data) {
		t.Fatalf("upgraded written hash = %q, want current target hash", upgraded.WrittenHash)
	}
	if upgraded.ConfigPath != legacy.ConfigPath || upgraded.Values[claudeManagedEnvKey] != legacy.Values[claudeManagedEnvKey] || upgraded.Existed != legacy.Existed {
		t.Fatalf("migration changed legacy recovery state: got %+v want %+v", upgraded, legacy)
	}

	out.Reset()
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
		t.Fatalf("off changed settings link: text=%q err=%v, want %q", got, err, linkText)
	}
	if got, ok := envValue(t, target, claudeManagedEnvKey); !ok || got != "https://legacy.example.com" {
		t.Fatalf("legacy base URL after off = %q, ok=%v", got, ok)
	}
	for _, key := range []string{claudeAttributionEnvKey, claudeToolSearchEnvKey} {
		if _, ok := envValue(t, target, key); ok {
			t.Errorf("legacy-uncovered %s survived off", key)
		}
	}
	if got, ok := envValue(t, target, "OTHER"); !ok || got != "keep" {
		t.Fatalf("unrelated env after off = %q, ok=%v", got, ok)
	}
	if got := readTree(t, target)["theme"]; got != "dark" {
		t.Fatalf("unrelated setting after off = %v", got)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Fatalf("backup after off: %v", err)
	}
}

func TestClaudeLegacyBackupMigratesThroughSymlinkedParent(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	configuredDir := filepath.Dir(a.settingsPath)
	realDir := configuredDir + ".real"
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configuredDir), 0o700); err != nil {
		t.Fatal(err)
	}
	managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"},"theme":"dark"}`)
	target := filepath.Join(realDir, filepath.Base(a.settingsPath))
	if err := os.WriteFile(target, managed, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, configuredDir); err != nil {
		t.Fatal(err)
	}
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(managed),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}

	if code := a.on(); code != 0 {
		t.Fatalf("on = %d (%s)", code, out.String())
	}
	upgraded, err := loadClaudeBackup(a.backupPath)
	if err != nil || upgraded == nil {
		t.Fatalf("load upgraded backup: bak=%v err=%v", upgraded, err)
	}
	_, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.ResolvedPath != target || upgraded.ResolvedPath != snap.ResolvedPath || upgraded.WrittenIdentity == nil || *upgraded.WrittenIdentity != snap.Identity {
		t.Fatalf("upgraded provenance = resolved %q identity %v, want %q %+v", upgraded.ResolvedPath, upgraded.WrittenIdentity, target, snap.Identity)
	}
	if info, err := os.Lstat(configuredDir); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("on changed configured parent symlink: info=%v err=%v", info, err)
	}

	out.Reset()
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if got, ok := envValue(t, target, claudeManagedEnvKey); !ok || got != "https://legacy.example.com" {
		t.Fatalf("legacy base URL after off = %q, ok=%v", got, ok)
	}
	for _, key := range []string{claudeAttributionEnvKey, claudeToolSearchEnvKey} {
		if _, ok := envValue(t, target, key); ok {
			t.Errorf("legacy-uncovered %s survived off", key)
		}
	}
	if got, ok := envValue(t, target, "OTHER"); !ok || got != "keep" {
		t.Fatalf("unrelated env after off = %q, ok=%v", got, ok)
	}
	if info, err := os.Lstat(configuredDir); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("off changed configured parent symlink: info=%v err=%v", info, err)
	}
	if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("target mode after off = %v, err=%v", st.Mode().Perm(), err)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Fatalf("backup after off: %v", err)
	}
}

func TestClaudeLegacyLinkedBackupDirectOffMigration(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
	managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"},"theme":"dark"}`)
	if err := os.WriteFile(target, managed, 0o600); err != nil {
		t.Fatal(err)
	}
	linkText := filepath.Base(target)
	if err := os.Symlink(linkText, a.settingsPath); err != nil {
		t.Fatal(err)
	}
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(managed),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}

	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
		t.Fatalf("off changed settings link: text=%q err=%v, want %q", got, err, linkText)
	}
	if got, ok := envValue(t, target, claudeManagedEnvKey); !ok || got != "https://legacy.example.com" {
		t.Fatalf("legacy base URL after off = %q, ok=%v", got, ok)
	}
	if got, ok := envValue(t, target, "OTHER"); !ok || got != "keep" {
		t.Fatalf("unrelated env after off = %q, ok=%v", got, ok)
	}
	if got := readTree(t, target)["theme"]; got != "dark" {
		t.Fatalf("unrelated setting after off = %v", got)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Fatalf("backup after off: %v", err)
	}
}

func TestClaudeLegacyLinkedBackupHashMismatchRefuses(t *testing.T) {
	requireSymlinks(t)
	for _, command := range []string{"on", "off"} {
		t.Run(command, func(t *testing.T) {
			a, out := testAdapter(t)
			if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
			recorded := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"before"}}`)
			drifted := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"after"},"theme":"dark"}`)
			if err := os.WriteFile(target, drifted, 0o600); err != nil {
				t.Fatal(err)
			}
			linkText := filepath.Base(target)
			if err := os.Symlink(linkText, a.settingsPath); err != nil {
				t.Fatal(err)
			}
			legacy := &claudeBackup{
				ConfigPath:  a.settingsPath,
				Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
				EnvExisted:  true,
				Existed:     true,
				WrittenHash: sha256Hex(recorded),
			}
			if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
				t.Fatal(err)
			}
			backupBefore := fileBytes(t, a.backupPath)

			var code int
			if command == "on" {
				code = a.on()
			} else {
				code = a.off()
			}
			if code != 1 {
				t.Fatalf("%s = %d, want refusal (%s)", command, code, out.String())
			}
			for _, want := range []string{"legacy backup", "predates linked-settings support", "does not exactly match", "backup left intact", "restore the link", "do not delete the backup"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("%s diagnostic missing %q: %q", command, want, out.String())
				}
			}
			if got := fileBytes(t, target); !bytes.Equal(got, drifted) {
				t.Fatalf("%s changed drifted target: %s", command, got)
			}
			if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
				t.Fatalf("%s changed settings link: text=%q err=%v, want %q", command, got, err, linkText)
			}
			if got := fileBytes(t, a.backupPath); !bytes.Equal(got, backupBefore) {
				t.Fatalf("%s changed non-adoptable legacy backup", command)
			}
		})
	}
}

func TestClaudeLinkedBackupPartialMetadataRefuses(t *testing.T) {
	requireSymlinks(t)
	for _, metadata := range []string{"resolved path only", "identity only"} {
		for _, command := range []string{"on", "off"} {
			t.Run(metadata+"/"+command, func(t *testing.T) {
				a, out := testAdapter(t)
				if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
				managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"}}`)
				if err := os.WriteFile(target, managed, 0o600); err != nil {
					t.Fatal(err)
				}
				linkText := filepath.Base(target)
				if err := os.Symlink(linkText, a.settingsPath); err != nil {
					t.Fatal(err)
				}
				_, snap, err := readClaudeSettings(a.settingsPath)
				if err != nil {
					t.Fatal(err)
				}
				bak := &claudeBackup{
					ConfigPath:  a.settingsPath,
					Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
					EnvExisted:  true,
					Existed:     true,
					WrittenHash: sha256Hex(managed),
				}
				if metadata == "resolved path only" {
					bak.ResolvedPath = snap.ResolvedPath
				} else {
					identity := snap.Identity
					bak.WrittenIdentity = &identity
				}
				if err := saveClaudeBackup(a.backupPath, bak); err != nil {
					t.Fatal(err)
				}
				backupBefore := fileBytes(t, a.backupPath)

				var code int
				if command == "on" {
					code = a.on()
				} else {
					code = a.off()
				}
				if code != 1 {
					t.Fatalf("%s = %d, want refusal (%s)", command, code, out.String())
				}
				if !strings.Contains(out.String(), "incomplete") || !strings.Contains(out.String(), "metadata") {
					t.Errorf("%s diagnostic does not identify incomplete metadata: %q", command, out.String())
				}
				if got := fileBytes(t, target); !bytes.Equal(got, managed) {
					t.Fatalf("%s changed target: %s", command, got)
				}
				if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
					t.Fatalf("%s changed settings link: text=%q err=%v, want %q", command, got, err, linkText)
				}
				if got := fileBytes(t, a.backupPath); !bytes.Equal(got, backupBefore) {
					t.Fatalf("%s changed partial-metadata backup", command)
				}
			})
		}
	}
}

func TestClaudeLegacyLinkedBackupRetargetAfterMigrationKeepsProvenance(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(filepath.Dir(a.settingsPath), "first.json")
	second := filepath.Join(filepath.Dir(a.settingsPath), "second.json")
	firstRaw := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"}}`)
	secondRaw := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://second.example.com","OTHER":"second"}}`)
	if err := os.WriteFile(first, firstRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, secondRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(first), a.settingsPath); err != nil {
		t.Fatal(err)
	}
	_, firstSnap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(firstRaw),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}

	var staged *claudeBackup
	var stagedErr error
	oldHook := claudeBeforeSettingsMutation
	claudeBeforeSettingsMutation = func() {
		staged, stagedErr = loadClaudeBackup(a.backupPath)
		if err := os.Remove(a.settingsPath); err != nil {
			panic(err)
		}
		if err := os.Symlink(filepath.Base(second), a.settingsPath); err != nil {
			panic(err)
		}
	}
	t.Cleanup(func() { claudeBeforeSettingsMutation = oldHook })

	if code := a.on(); code != 1 {
		t.Fatalf("on = %d, want retarget conflict (%s)", code, out.String())
	}
	if stagedErr != nil || staged == nil {
		t.Fatalf("load staged backup: bak=%v err=%v", staged, stagedErr)
	}
	if staged.ResolvedPath != firstSnap.ResolvedPath || staged.WrittenIdentity == nil || *staged.WrittenIdentity != firstSnap.Identity {
		t.Fatalf("staged provenance = resolved %q identity %v, want %q %+v", staged.ResolvedPath, staged.WrittenIdentity, firstSnap.ResolvedPath, firstSnap.Identity)
	}
	if !backupCovers(staged, claudeAttributionEnvKey) || !backupCovers(staged, claudeToolSearchEnvKey) {
		t.Fatalf("staged backup does not cover additive keys: %+v", staged)
	}
	if got := fileBytes(t, first); !bytes.Equal(got, firstRaw) {
		t.Fatalf("original target changed: %s", got)
	}
	if got := fileBytes(t, second); !bytes.Equal(got, secondRaw) {
		t.Fatalf("retargeted target changed: %s", got)
	}
	if got, err := os.Readlink(a.settingsPath); err != nil || got != filepath.Base(second) {
		t.Fatalf("retargeted link changed: text=%q err=%v", got, err)
	}
	retained, err := loadClaudeBackup(a.backupPath)
	if err != nil || retained == nil {
		t.Fatalf("load retained backup: bak=%v err=%v", retained, err)
	}
	if retained.ResolvedPath != firstSnap.ResolvedPath || retained.WrittenIdentity == nil || *retained.WrittenIdentity != firstSnap.Identity {
		t.Fatalf("retained provenance = resolved %q identity %v, want %q %+v", retained.ResolvedPath, retained.WrittenIdentity, firstSnap.ResolvedPath, firstSnap.Identity)
	}
	if retained.WrittenHash != legacy.WrittenHash || retained.Values[claudeManagedEnvKey] != legacy.Values[claudeManagedEnvKey] {
		t.Fatalf("retained backup changed legacy recovery state: got %+v want %+v", retained, legacy)
	}
	if backupCovers(retained, claudeAttributionEnvKey) || backupCovers(retained, claudeToolSearchEnvKey) {
		t.Fatalf("failed mutation retained stale additive backup entries: %+v", retained)
	}
}

func TestClaudeLegacyLinkedBackupMigrationWriteFailurePreservesOriginal(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
	managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"}}`)
	if err := os.WriteFile(target, managed, 0o600); err != nil {
		t.Fatal(err)
	}
	linkText := filepath.Base(target)
	if err := os.Symlink(linkText, a.settingsPath); err != nil {
		t.Fatal(err)
	}
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(managed),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}
	// Pre-create the lock so making the backup directory read-only blocks only
	// the migration's atomic replacement, not mutation-lock acquisition.
	if err := os.WriteFile(a.backupPath+".mutation.lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	backupBefore := fileBytes(t, a.backupPath)
	backupDir := filepath.Dir(a.backupPath)
	if err := os.Chmod(backupDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(backupDir, 0o700) })

	if code := a.on(); code != 1 {
		t.Fatalf("on = %d, want migrated-backup replacement failure (%s)", code, out.String())
	}
	for _, want := range []string{"backup", "settings untouched", "not applied"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("on diagnostic missing %q for migrated-backup write failure: %q", want, out.String())
		}
	}
	if got := fileBytes(t, target); !bytes.Equal(got, managed) {
		t.Fatalf("failed migration changed settings target: %s", got)
	}
	if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
		t.Fatalf("failed migration changed settings link: text=%q err=%v, want %q", got, err, linkText)
	}
	if got := fileBytes(t, a.backupPath); !bytes.Equal(got, backupBefore) {
		t.Fatalf("failed migration changed original legacy backup bytes")
	}
}

func TestClaudeLegacyLinkedBackupMigrationRejectsConcurrentBackupChange(t *testing.T) {
	requireSymlinks(t)
	a, _ := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
	managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"}}`)
	if err := os.WriteFile(target, managed, 0o600); err != nil {
		t.Fatal(err)
	}
	linkText := filepath.Base(target)
	if err := os.Symlink(linkText, a.settingsPath); err != nil {
		t.Fatal(err)
	}
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(managed),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}
	bak, backupSnap, err := loadClaudeBackupSnapshot(a.backupPath)
	if err != nil {
		t.Fatal(err)
	}
	_, settingsSnap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := cloneClaudeBackup(legacy)
	concurrent.Values[claudeManagedEnvKey] = "https://concurrent.example.com"
	if err := saveClaudeBackup(a.backupPath, concurrent); err != nil {
		t.Fatal(err)
	}
	concurrentBytes := fileBytes(t, a.backupPath)

	got, migrated, err := a.migrateLegacyLinkedBackup(bak, backupSnap, settingsSnap)
	if err == nil || migrated {
		t.Fatalf("migration = bak=%+v migrated=%v err=%v, want concurrent-backup refusal", got, migrated, err)
	}
	if !strings.Contains(err.Error(), "changed concurrently") || !strings.Contains(err.Error(), "not overwritten") {
		t.Fatalf("concurrent-backup diagnostic = %q", err)
	}
	if got == nil || got.ResolvedPath != "" || got.WrittenIdentity != nil || got.Values[claudeManagedEnvKey] != legacy.Values[claudeManagedEnvKey] {
		t.Fatalf("failed migration returned changed legacy state: %+v", got)
	}
	if actual := fileBytes(t, a.backupPath); !bytes.Equal(actual, concurrentBytes) {
		t.Fatalf("migration overwrote concurrent backup")
	}
	if actual := fileBytes(t, target); !bytes.Equal(actual, managed) {
		t.Fatalf("concurrent-backup refusal changed settings target: %s", actual)
	}
	if actual, err := os.Readlink(a.settingsPath); err != nil || actual != linkText {
		t.Fatalf("concurrent-backup refusal changed settings link: text=%q err=%v, want %q", actual, err, linkText)
	}
}

func TestClaudeLegacyLinkedBackupAdditiveSaveFailureKeepsAdoptedProvenance(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
	managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"}}`)
	if err := os.WriteFile(target, managed, 0o600); err != nil {
		t.Fatal(err)
	}
	linkText := filepath.Base(target)
	if err := os.Symlink(linkText, a.settingsPath); err != nil {
		t.Fatal(err)
	}
	_, settingsSnap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(managed),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}
	expected := cloneClaudeBackup(legacy)
	recordClaudeBackupFile(expected, settingsSnap)
	expectedBytes, err := marshalClaudeBackup(expected)
	if err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Dir(a.backupPath)
	hookCalled := false
	oldHook := claudeBeforeMigratedBackupReplace
	claudeBeforeMigratedBackupReplace = func(action string) {
		if action != "migrated backup additive update" {
			return
		}
		hookCalled = true
		if err := os.Chmod(backupDir, 0o500); err != nil {
			panic(err)
		}
	}
	t.Cleanup(func() {
		claudeBeforeMigratedBackupReplace = oldHook
		_ = os.Chmod(backupDir, 0o700)
	})

	if code := a.on(); code != 1 {
		t.Fatalf("on = %d, want additive-backup replacement failure (%s)", code, out.String())
	}
	if !hookCalled {
		t.Fatal("on did not route the additive update through the migrated-backup CAS seam")
	}
	for _, want := range []string{"write backup", "failed before backup replacement", "not applied", "settings untouched"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("additive-backup failure diagnostic missing %q: %q", want, out.String())
		}
	}
	if got := fileBytes(t, target); !bytes.Equal(got, managed) {
		t.Fatalf("additive-backup failure changed settings target: %s", got)
	}
	if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
		t.Fatalf("additive-backup failure changed settings link: text=%q err=%v, want %q", got, err, linkText)
	}
	retainedBytes := fileBytes(t, a.backupPath)
	if len(retainedBytes) == 0 {
		t.Fatal("additive-backup failure truncated the sole recovery backup")
	}
	if !bytes.Equal(retainedBytes, expectedBytes) {
		t.Fatal("additive-backup failure changed the adopted recovery backup")
	}
	retained, err := loadClaudeBackup(a.backupPath)
	if err != nil || retained == nil {
		t.Fatalf("retained backup is not valid JSON recovery state: bak=%v err=%v bytes=%q", retained, err, retainedBytes)
	}
	if retained.ResolvedPath != settingsSnap.ResolvedPath || retained.WrittenIdentity == nil || *retained.WrittenIdentity != settingsSnap.Identity {
		t.Fatalf("retained provenance = resolved %q identity %v, want %q %+v", retained.ResolvedPath, retained.WrittenIdentity, settingsSnap.ResolvedPath, settingsSnap.Identity)
	}
	if retained.WrittenHash != legacy.WrittenHash || retained.Values[claudeManagedEnvKey] != legacy.Values[claudeManagedEnvKey] {
		t.Fatalf("retained backup changed legacy recovery state: got %+v want %+v", retained, legacy)
	}
	if backupCovers(retained, claudeAttributionEnvKey) || backupCovers(retained, claudeToolSearchEnvKey) {
		t.Fatalf("failed additive save falsely committed managed-key snapshots: %+v", retained)
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(a.backupPath), "config-backup.*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 || backups[0] != a.backupPath {
		t.Fatalf("recovery backup files = %v, want only %s", backups, a.backupPath)
	}
}

func TestClaudeLegacyLinkedBackupAdditiveSaveRejectsConcurrentBackupChange(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
	managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"}}`)
	if err := os.WriteFile(target, managed, 0o600); err != nil {
		t.Fatal(err)
	}
	linkText := filepath.Base(target)
	if err := os.Symlink(linkText, a.settingsPath); err != nil {
		t.Fatal(err)
	}
	_, settingsSnap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(managed),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}
	concurrent := cloneClaudeBackup(legacy)
	recordClaudeBackupFile(concurrent, settingsSnap)
	concurrent.Model = "concurrent-model"
	concurrentBytes, err := marshalClaudeBackup(concurrent)
	if err != nil {
		t.Fatal(err)
	}
	concurrentTemp := a.backupPath + ".concurrent"
	if err := os.WriteFile(concurrentTemp, concurrentBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(concurrentTemp) })

	hookCalled := false
	oldHook := claudeBeforeMigratedBackupReplace
	claudeBeforeMigratedBackupReplace = func(action string) {
		if action != "migrated backup additive update" {
			return
		}
		hookCalled = true
		if err := os.Rename(concurrentTemp, a.backupPath); err != nil {
			panic(err)
		}
	}
	t.Cleanup(func() { claudeBeforeMigratedBackupReplace = oldHook })

	if code := a.on(); code != 1 {
		t.Fatalf("on = %d, want concurrent additive-backup refusal (%s)", code, out.String())
	}
	if !hookCalled {
		t.Fatal("on did not route the additive update through the migrated-backup CAS seam")
	}
	if !strings.Contains(out.String(), "changed concurrently") || !strings.Contains(out.String(), "not overwritten") {
		t.Fatalf("concurrent additive-backup diagnostic = %q", out.String())
	}
	if got := fileBytes(t, a.backupPath); !bytes.Equal(got, concurrentBytes) {
		t.Fatalf("additive save overwrote concurrent backup")
	}
	if got := fileBytes(t, target); !bytes.Equal(got, managed) {
		t.Fatalf("concurrent additive-backup refusal changed settings target: %s", got)
	}
	if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
		t.Fatalf("concurrent additive-backup refusal changed settings link: text=%q err=%v, want %q", got, err, linkText)
	}
}

func TestClaudeOnConfigPathMismatchRefusesBeforeMutation(t *testing.T) {
	t.Run("direct legacy backup", func(t *testing.T) {
		a, out := testAdapter(t)
		managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"}}`)
		writeSettingsFile(t, a, string(managed))
		bak := &claudeBackup{
			ConfigPath:  a.settingsPath + ".other",
			Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
			EnvExisted:  true,
			Existed:     true,
			WrittenHash: sha256Hex(managed),
		}
		if err := saveClaudeBackup(a.backupPath, bak); err != nil {
			t.Fatal(err)
		}
		backupBefore := fileBytes(t, a.backupPath)

		if code := a.on(); code != 1 {
			t.Fatalf("on = %d, want config-path refusal (%s)", code, out.String())
		}
		for _, want := range []string{"backup", "is for", "left intact"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("config-path diagnostic missing %q: %q", want, out.String())
			}
		}
		if got := fileBytes(t, a.settingsPath); !bytes.Equal(got, managed) {
			t.Fatalf("config-path refusal changed direct settings: %s", got)
		}
		if got := fileBytes(t, a.backupPath); !bytes.Equal(got, backupBefore) {
			t.Fatalf("config-path refusal changed direct legacy backup")
		}
	})

	t.Run("linked current backup", func(t *testing.T) {
		requireSymlinks(t)
		a, out := testAdapter(t)
		if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
		managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"}}`)
		if err := os.WriteFile(target, managed, 0o600); err != nil {
			t.Fatal(err)
		}
		linkText := filepath.Base(target)
		if err := os.Symlink(linkText, a.settingsPath); err != nil {
			t.Fatal(err)
		}
		_, snap, err := readClaudeSettings(a.settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		bak := &claudeBackup{
			ConfigPath:  a.settingsPath + ".other",
			Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
			EnvExisted:  true,
			Existed:     true,
			WrittenHash: sha256Hex(managed),
		}
		recordClaudeBackupFile(bak, snap)
		if err := saveClaudeBackup(a.backupPath, bak); err != nil {
			t.Fatal(err)
		}
		backupBefore := fileBytes(t, a.backupPath)

		if code := a.on(); code != 1 {
			t.Fatalf("on = %d, want config-path refusal (%s)", code, out.String())
		}
		for _, want := range []string{"backup", "is for", "left intact"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("config-path diagnostic missing %q: %q", want, out.String())
			}
		}
		if got := fileBytes(t, target); !bytes.Equal(got, managed) {
			t.Fatalf("config-path refusal changed linked settings: %s", got)
		}
		if got, err := os.Readlink(a.settingsPath); err != nil || got != linkText {
			t.Fatalf("config-path refusal changed settings link: text=%q err=%v, want %q", got, err, linkText)
		}
		if got := fileBytes(t, a.backupPath); !bytes.Equal(got, backupBefore) {
			t.Fatalf("config-path refusal changed current backup")
		}
	})
}

func TestClaudeOffNeverRemovesFinalSymlinkTarget(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(a.settingsPath), "shared-empty.json")
	raw := []byte("{}")
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, a.settingsPath); err != nil {
		t.Fatal(err)
	}
	_, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	bak := &claudeBackup{
		ConfigPath:   a.settingsPath,
		Values:       map[string]string{},
		Missing:      append([]string(nil), claudeOnEnvKeys...),
		ModelMissing: true,
		Existed:      false,
		WrittenHash:  sha256Hex(raw),
	}
	recordClaudeBackupFile(bak, snap)
	if err := saveClaudeBackup(a.backupPath, bak); err != nil {
		t.Fatal(err)
	}

	if code := a.off(); code != 1 {
		t.Fatalf("off = %d, want linked removal refusal (%s)", code, out.String())
	}
	if got := fileBytes(t, target); !bytes.Equal(got, raw) {
		t.Fatalf("linked target changed: %s", got)
	}
	if _, err := os.Readlink(a.settingsPath); err != nil {
		t.Fatalf("settings symlink changed: %v", err)
	}
	if _, err := os.Stat(a.backupPath); err != nil {
		t.Fatalf("refusal cleared backup: %v", err)
	}
}
