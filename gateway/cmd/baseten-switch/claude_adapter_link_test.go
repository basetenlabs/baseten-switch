package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
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

func TestClaudeLegacyBackupDoesNotRestoreThroughFinalLink(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(a.settingsPath), "cloud-settings.json")
	managed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","CLAUDE_CODE_ATTRIBUTION_HEADER":"0","ENABLE_TOOL_SEARCH":"true","OTHER":"keep"}}`)
	if err := os.WriteFile(target, managed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, a.settingsPath); err != nil {
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

	if code := a.off(); code != 1 {
		t.Fatalf("off = %d, want legacy linked-target refusal (%s)", code, out.String())
	}
	if got := fileBytes(t, target); !bytes.Equal(got, managed) {
		t.Fatalf("legacy refusal changed linked target: %s", got)
	}
	if _, err := os.Readlink(a.settingsPath); err != nil {
		t.Fatalf("off replaced settings link: %v", err)
	}
	if _, err := os.Stat(a.backupPath); err != nil {
		t.Fatalf("legacy refusal cleared backup: %v", err)
	}
}

func TestClaudeLegacyBackupDoesNotRestoreThroughSymlinkedParent(t *testing.T) {
	requireSymlinks(t)
	a, out := testAdapter(t)
	configuredDir := filepath.Dir(a.settingsPath)
	firstDir := configuredDir + ".first"
	secondDir := configuredDir + ".second"
	for _, dir := range []string{firstDir, secondDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	firstRaw := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://first.example.com"}}`)
	secondRaw := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","CLAUDE_CODE_ATTRIBUTION_HEADER":"0","ENABLE_TOOL_SEARCH":"true"},"owner":"second"}`)
	if err := os.WriteFile(filepath.Join(firstDir, filepath.Base(a.settingsPath)), firstRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	secondTarget := filepath.Join(secondDir, filepath.Base(a.settingsPath))
	if err := os.WriteFile(secondTarget, secondRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondDir, configuredDir); err != nil {
		t.Fatal(err)
	}
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://legacy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(secondRaw),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}

	if code := a.off(); code != 1 {
		t.Fatalf("off = %d, want legacy parent-link refusal (%s)", code, out.String())
	}
	if got := fileBytes(t, secondTarget); !bytes.Equal(got, secondRaw) {
		t.Fatalf("legacy refusal changed retargeted parent file: %s", got)
	}
	if _, err := os.Stat(a.backupPath); err != nil {
		t.Fatalf("legacy refusal cleared backup: %v", err)
	}
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
