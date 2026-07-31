package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

// All tests operate exclusively on t.TempDir() paths; the real
// ~/.claude and ~/.config/baseten-switch are never read or written.

const testDoorPort = "8081"

func testAdapter(t *testing.T) (*claudeAdapter, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "claude", "settings.json")
	out := &bytes.Buffer{}
	return &claudeAdapter{
		settingsPath: settings,
		backupPath:   claudeBackupPath(filepath.Join(dir, "backups"), settings),
		desiredPort:  testDoorPort,
		gatewayPorts: map[string]bool{"8081": true, "18081": true},
		modelAliases: map[string]string{
			"claude-baseten-glm-5-2":   "zai-org/GLM-5.2",
			"claude-baseten-kimi-k2-7": "moonshotai/Kimi-K2-7",
		},
		out: out,
	}, out
}

func writeSettingsFile(t *testing.T, a *claudeAdapter, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.settingsPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTree(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func envValue(t *testing.T, path, key string) (string, bool) {
	t.Helper()
	root := readTree(t, path)
	env, _ := root["env"].(map[string]any)
	if env == nil {
		return "", false
	}
	s, ok := env[key].(string)
	return s, ok
}

func assertClaudeOnValues(t *testing.T, a *claudeAdapter) {
	t.Helper()
	for key, want := range map[string]string{
		claudeManagedEnvKey:     "http://127.0.0.1:8081",
		claudeAttributionEnvKey: claudeAttributionValue,
		claudeToolSearchEnvKey:  claudeToolSearchValue,
	} {
		if got, ok := envValue(t, a.settingsPath, key); !ok || got != want {
			t.Errorf("%s = %q, ok=%v, want %q", key, got, ok, want)
		}
	}
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClaudeOnOffRoundTripRestoresKeyExactly(t *testing.T) {
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{
  "env": {
    "ANTHROPIC_BASE_URL": "https://corp-proxy.example.com",
    "CLAUDE_CODE_ATTRIBUTION_HEADER": "custom-attribution",
    "ENABLE_TOOL_SEARCH": "auto:25",
    "OTHER": "keep"
  },
  "model": "claude-sonnet-4-6",
  "permissions": {"allow": ["Bash(ls:*)"]},
  "retries": 3
}`)
	if code := a.on(); code != 0 {
		t.Fatalf("on = %d", code)
	}
	if v, _ := envValue(t, a.settingsPath, claudeManagedEnvKey); v != "http://127.0.0.1:8081" {
		t.Fatalf("after on, base url = %q", v)
	}
	assertClaudeOnValues(t, a)
	if _, err := os.Stat(a.backupPath); err != nil {
		t.Fatalf("backup not written: %v", err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d", code)
	}
	root := readTree(t, a.settingsPath)
	env := root["env"].(map[string]any)
	if env[claudeManagedEnvKey] != "https://corp-proxy.example.com" {
		t.Errorf("base url not restored: %v", env[claudeManagedEnvKey])
	}
	if env[claudeAttributionEnvKey] != "custom-attribution" {
		t.Errorf("attribution header not restored: %v", env[claudeAttributionEnvKey])
	}
	if env[claudeToolSearchEnvKey] != "auto:25" {
		t.Errorf("tool search not restored: %v", env[claudeToolSearchEnvKey])
	}
	if env["OTHER"] != "keep" {
		t.Errorf("OTHER lost: %v", env["OTHER"])
	}
	if root["model"] != "claude-sonnet-4-6" {
		t.Errorf("model changed: %v", root["model"])
	}
	if root["retries"] != float64(3) {
		t.Errorf("retries changed: %v", root["retries"])
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("backup not deleted after off: %v", err)
	}
}

func TestClaudeOffWithoutOnIsNoop(t *testing.T) {
	cases := []struct {
		name    string
		content string // "" = no file
	}{
		{"no file", ""},
		{"user settings untouched", `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com"},"model":"claude-opus-4-8"}` + "\n"},
		{"no env block", `{"permissions":{}}` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, out := testAdapter(t)
			if tc.content != "" {
				writeSettingsFile(t, a, tc.content)
			}
			if code := a.off(); code != 0 {
				t.Fatalf("off = %d (%s)", code, out.String())
			}
			if tc.content == "" {
				if _, err := os.Stat(a.settingsPath); !os.IsNotExist(err) {
					t.Errorf("off created a settings file")
				}
			} else if got := string(fileBytes(t, a.settingsPath)); got != tc.content {
				t.Errorf("off modified the file:\n%s", got)
			}
			if !strings.Contains(out.String(), "off") {
				t.Errorf("expected an off report, got %q", out.String())
			}
		})
	}
}

func TestClaudeDoubleOnIsSafe(t *testing.T) {
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("first on failed")
	}
	bak1 := fileBytes(t, a.backupPath)
	settings1 := fileBytes(t, a.settingsPath)
	if code := a.on(); code != 0 {
		t.Fatal("second on failed")
	}
	if !bytes.Equal(bak1, fileBytes(t, a.backupPath)) {
		t.Errorf("second on rewrote the backup; the user's original values must be snapshotted once")
	}
	if !bytes.Equal(settings1, fileBytes(t, a.settingsPath)) {
		t.Errorf("second on rewrote an already-correct settings file")
	}
	// Round trip still restores the original.
	if code := a.off(); code != 0 {
		t.Fatal("off failed")
	}
	if v, _ := envValue(t, a.settingsPath, claudeManagedEnvKey); v != "https://corp-proxy.example.com" {
		t.Errorf("restore after double on = %q", v)
	}
}

func TestClaudeDoubleOffIsSafe(t *testing.T) {
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	if code := a.off(); code != 0 {
		t.Fatal("first off failed")
	}
	after1 := fileBytes(t, a.settingsPath)
	if code := a.off(); code != 0 {
		t.Fatal("second off failed")
	}
	if !bytes.Equal(after1, fileBytes(t, a.settingsPath)) {
		t.Errorf("second off modified the file")
	}
}

func TestClaudeUserEditsSurviveOffViaDriftDowngrade(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	// User edits another key between on and off: hash drifts.
	root := readTree(t, a.settingsPath)
	root["theme"] = "dark"
	b, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(a.settingsPath, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	// Drift while still gateway-managed is the NORMAL path (Claude Code
	// rewrites settings.json as it runs): the message explains what
	// happened and that edits are kept, and must not read as a warning.
	if !strings.Contains(out.String(), "your edits are kept") {
		t.Errorf("expected normal-drift explanation, got %q", out.String())
	}
	if !strings.Contains(out.String(), "normal: Claude Code rewrites it as you work") {
		t.Errorf("drift message does not say the drift is normal: %q", out.String())
	}
	if strings.Contains(out.String(), "warning:") {
		t.Errorf("normal drift path must not print a warning: %q", out.String())
	}
	// Strip-only deletes the key without restoring the pre-'on' value;
	// the output must say so (naming the value) and give the manual
	// re-add instruction, or "your edits are kept" reads as if the
	// corp proxy came back while Claude Code silently routes to
	// api.anthropic.com.
	if !strings.Contains(out.String(), "NOT restored") ||
		!strings.Contains(out.String(), `"https://corp-proxy.example.com"`) {
		t.Errorf("missing not-restored note naming the pre-'on' value: %q", out.String())
	}
	if !strings.Contains(out.String(), "re-add it to") {
		t.Errorf("not-restored note gives no manual re-add instruction: %q", out.String())
	}
	after := readTree(t, a.settingsPath)
	if after["theme"] != "dark" {
		t.Errorf("user edit lost: %v", after["theme"])
	}
	if _, ok := envValue(t, a.settingsPath, claudeManagedEnvKey); ok {
		t.Errorf("gateway-owned base url not stripped under drift")
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("backup not cleared after drift off")
	}
}

// TestClaudeOffDriftRedirectedStaysAWarning: the OTHER drift branch,
// where the file no longer points at the gateway at all (something
// else redirected the harness between on and off), is genuinely
// surprising and keeps its warning prefix.
func TestClaudeOffDriftRedirectedStaysAWarning(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	// Someone (not us) points the harness elsewhere after on.
	root := readTree(t, a.settingsPath)
	env := root["env"].(map[string]any)
	env[claudeManagedEnvKey] = "https://other-proxy.example.com"
	b, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(a.settingsPath, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if !strings.Contains(out.String(), "warning:") || !strings.Contains(out.String(), "no longer points at the gateway") {
		t.Errorf("expected redirected-drift warning, got %q", out.String())
	}
	// The foreign value is not ours to touch.
	if v, _ := envValue(t, a.settingsPath, claudeManagedEnvKey); v != "https://other-proxy.example.com" {
		t.Errorf("foreign base url touched: %q", v)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("backup not cleared after redirected drift off")
	}
}

func TestClaudeOffRedirectedDriftReportsUnrestoredKnobs(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com","CLAUDE_CODE_ATTRIBUTION_HEADER":"custom-before","ENABLE_TOOL_SEARCH":"auto:25"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	root := readTree(t, a.settingsPath)
	root["env"].(map[string]any)[claudeManagedEnvKey] = "https://other-proxy.example.com"
	b, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(a.settingsPath, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if got, _ := envValue(t, a.settingsPath, claudeManagedEnvKey); got != "https://other-proxy.example.com" {
		t.Errorf("redirected base URL changed: %q", got)
	}
	for _, key := range []string{claudeAttributionEnvKey, claudeToolSearchEnvKey} {
		if _, ok := envValue(t, a.settingsPath, key); ok {
			t.Errorf("covered Switch value %s not stripped", key)
		}
	}
	for _, want := range []string{
		"CLAUDE_CODE_ATTRIBUTION_HEADER \"custom-before\" was NOT restored",
		"ENABLE_TOOL_SEARCH \"auto:25\" was NOT restored",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing unrestored-value note %q in %q", want, out.String())
		}
	}
}

func TestClaudeStripOnlyOwnedNeverTouchesUserValues(t *testing.T) {
	cases := []struct {
		name      string
		baseURL   string
		wantKept  bool
		wantWrite bool
	}{
		{"user proxy kept", "https://corp-proxy.example.com", true, false},
		{"user localhost non-gateway port kept", "http://127.0.0.1:9999", true, false},
		{"gateway door port stripped", "http://127.0.0.1:8081", false, true},
		{"gateway router port stripped", "http://localhost:18081", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := testAdapter(t)
			content := `{"env":{"ANTHROPIC_BASE_URL":"` + tc.baseURL + `","ANTHROPIC_AUTH_TOKEN":"user-token"},"other":true}` + "\n"
			writeSettingsFile(t, a, content)
			before := fileBytes(t, a.settingsPath)
			if code := a.off(); code != 0 {
				t.Fatalf("off = %d", code)
			}
			v, ok := envValue(t, a.settingsPath, claudeManagedEnvKey)
			if tc.wantKept && (!ok || v != tc.baseURL) {
				t.Errorf("user value touched: %q ok=%v", v, ok)
			}
			if !tc.wantKept && ok {
				t.Errorf("gateway value not stripped: %q", v)
			}
			if tok, _ := envValue(t, a.settingsPath, "ANTHROPIC_AUTH_TOKEN"); tok != "user-token" {
				t.Errorf("unmanaged env key touched: %q", tok)
			}
			if !tc.wantWrite && !bytes.Equal(before, fileBytes(t, a.settingsPath)) {
				t.Errorf("noop off rewrote the file")
			}
		})
	}
}

func TestClaudePoisonedBackupDiscardedNotRestored(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081"}}`)
	// Hand-craft a poisoned backup: its "original" value is itself a
	// gateway URL, so restoring it would re-manage the harness.
	bak := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "http://127.0.0.1:18081"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(fileBytes(t, a.settingsPath)),
	}
	if err := saveClaudeBackup(a.backupPath, bak); err != nil {
		t.Fatal(err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if !strings.Contains(out.String(), "poisoned") {
		t.Errorf("expected poisoned-backup warning, got %q", out.String())
	}
	if v, ok := envValue(t, a.settingsPath, claudeManagedEnvKey); ok {
		t.Errorf("poisoned value restored or kept: %q", v)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("poisoned backup not discarded")
	}
}

func TestClaudeOnCreatesFileAndOffDeletesIt(t *testing.T) {
	a, _ := testAdapter(t)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	if v, _ := envValue(t, a.settingsPath, claudeManagedEnvKey); v != "http://127.0.0.1:8081" {
		t.Fatalf("created settings wrong: %q", v)
	}
	var bak claudeBackup
	b := fileBytes(t, a.backupPath)
	if err := json.Unmarshal(b, &bak); err != nil {
		t.Fatal(err)
	}
	if bak.Existed {
		t.Errorf("backup existed=true for a created file")
	}
	if len(bak.Missing) != len(claudeOnEnvKeys) {
		t.Errorf("backup missing list = %v", bak.Missing)
	}
	for _, key := range claudeOnEnvKeys {
		if !backupCovers(&bak, key) {
			t.Errorf("backup does not cover missing %s", key)
		}
	}
	if code := a.off(); code != 0 {
		t.Fatal("off failed")
	}
	if _, err := os.Stat(a.settingsPath); !os.IsNotExist(err) {
		t.Errorf("off left behind a settings file we created")
	}
}

func TestClaudeOnUpgradesAlreadyManagedBaseWithoutBackingUpBase(t *testing.T) {
	// Manually gateway-managed (old port), no backup: on must not
	// snapshot our own base URL as the user's original state, but must
	// record the two new keys so the upgrade remains reversible.
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:18081"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	bak, err := loadClaudeBackup(a.backupPath)
	if err != nil || bak == nil {
		t.Fatalf("load upgrade backup: bak=%v err=%v", bak, err)
	}
	if backupCovers(bak, claudeManagedEnvKey) {
		t.Errorf("on snapshotted an already gateway-managed base URL")
	}
	for _, key := range []string{claudeAttributionEnvKey, claudeToolSearchEnvKey} {
		if !backupCovers(bak, key) {
			t.Errorf("upgrade backup does not cover %s", key)
		}
	}
	assertClaudeOnValues(t, a)
	if code := a.off(); code != 0 {
		t.Fatal("off failed")
	}
	for _, key := range claudeOnEnvKeys {
		if _, ok := envValue(t, a.settingsPath, key); ok {
			t.Errorf("off did not strip %s", key)
		}
	}
}

func TestClaudeOffResetsPersistedModelAlias(t *testing.T) {
	t.Run("restored from backup", func(t *testing.T) {
		a, out := testAdapter(t)
		writeSettingsFile(t, a, `{"model":"claude-sonnet-4-6"}`)
		if code := a.on(); code != 0 {
			t.Fatal("on failed")
		}
		// User picks a gateway alias via the /v1/models picker; it
		// persists to settings (the model-discovery contract trap).
		root := readTree(t, a.settingsPath)
		root["model"] = "claude-baseten-glm-5-2"
		b, _ := json.MarshalIndent(root, "", "  ")
		if err := os.WriteFile(a.settingsPath, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := a.off(); code != 0 {
			t.Fatalf("off = %d", code)
		}
		after := readTree(t, a.settingsPath)
		if after["model"] != "claude-sonnet-4-6" {
			t.Errorf("alias not reset to backed-up model: %v", after["model"])
		}
		if !strings.Contains(out.String(), "alias") {
			t.Errorf("expected alias note, got %q", out.String())
		}
	})
	t.Run("deleted without backup", func(t *testing.T) {
		a, out := testAdapter(t)
		writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081"},"model":"claude-baseten-kimi-k2-7"}`)
		if code := a.off(); code != 0 {
			t.Fatalf("off = %d", code)
		}
		after := readTree(t, a.settingsPath)
		if _, ok := after["model"]; ok {
			t.Errorf("alias model not deleted: %v", after["model"])
		}
		if !strings.Contains(out.String(), "alias") {
			t.Errorf("expected alias note, got %q", out.String())
		}
	})
}

func TestClaudeOnPreservesOtherSettingsContent(t *testing.T) {
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{
  "env": {"MAX_THINKING_TOKENS": "31999"},
  "hooks": {"PostToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "echo hi"}]}]},
  "big": 12345678901234567890,
  "float": 0.5
}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	raw := string(fileBytes(t, a.settingsPath))
	for _, want := range []string{`"MAX_THINKING_TOKENS": "31999"`, `"echo hi"`, `12345678901234567890`, `0.5`} {
		if !strings.Contains(raw, want) {
			t.Errorf("settings after on lost %s:\n%s", want, raw)
		}
	}
}

func TestClaudeOnOnlyWritesWhenChanged(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","CLAUDE_CODE_ATTRIBUTION_HEADER":"0","ENABLE_TOOL_SEARCH":"true"}}`)
	before := fileBytes(t, a.settingsPath)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	if !bytes.Equal(before, fileBytes(t, a.settingsPath)) {
		t.Errorf("on rewrote a file that already had the desired value")
	}
	if !strings.Contains(out.String(), "already on") {
		t.Errorf("expected already-on report, got %q", out.String())
	}
}

func TestClaudeStatusExitCodes(t *testing.T) {
	a, _ := testAdapter(t)
	var stdout bytes.Buffer
	if code := a.status(&stdout); code != statusExitOff {
		t.Errorf("status with no file = %d, want %d", code, statusExitOff)
	}
	if !strings.Contains(stdout.String(), testDoorPort) {
		t.Errorf("status missing door port: %q", stdout.String())
	}
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	stdout.Reset()
	if code := a.status(&stdout); code != 0 {
		t.Errorf("status when on = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "on (gateway-managed)") {
		t.Errorf("status output: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:8081") {
		t.Errorf("status missing base url: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), claudeAttributionEnvKey) ||
		!strings.Contains(stdout.String(), claudeToolSearchEnvKey) {
		t.Errorf("status missing managed env values: %q", stdout.String())
	}
}

func TestClaudeStatusBaseOnlyIsIncomplete(t *testing.T) {
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081"}}`)
	var stdout bytes.Buffer
	if code := a.status(&stdout); code != statusExitOff {
		t.Fatalf("status = %d, want %d", code, statusExitOff)
	}
	if !strings.Contains(stdout.String(), "incomplete") ||
		!strings.Contains(stdout.String(), "claude on") {
		t.Errorf("partial status does not identify repair: %q", stdout.String())
	}
}

func TestClaudeOnRepairsLegacyBaseOnlyBackup(t *testing.T) {
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"},"theme":"dark"}`)
	legacy := &claudeBackup{
		ConfigPath:  a.settingsPath,
		Values:      map[string]string{claudeManagedEnvKey: "https://corp-proxy.example.com"},
		EnvExisted:  true,
		Existed:     true,
		WrittenHash: sha256Hex(fileBytes(t, a.settingsPath)),
	}
	if err := saveClaudeBackup(a.backupPath, legacy); err != nil {
		t.Fatal(err)
	}

	if code := a.on(); code != 0 {
		t.Fatal("legacy upgrade on failed")
	}
	assertClaudeOnValues(t, a)
	upgraded, err := loadClaudeBackup(a.backupPath)
	if err != nil || upgraded == nil {
		t.Fatalf("load upgraded backup: bak=%v err=%v", upgraded, err)
	}
	for _, key := range claudeOnEnvKeys {
		if !backupCovers(upgraded, key) {
			t.Errorf("upgraded backup does not cover %s", key)
		}
	}
	if code := a.off(); code != 0 {
		t.Fatal("off after legacy upgrade failed")
	}
	if got, _ := envValue(t, a.settingsPath, claudeManagedEnvKey); got != "https://corp-proxy.example.com" {
		t.Errorf("legacy base URL restore = %q", got)
	}
	for _, key := range []string{claudeAttributionEnvKey, claudeToolSearchEnvKey} {
		if _, ok := envValue(t, a.settingsPath, key); ok {
			t.Errorf("legacy-missing %s was not removed", key)
		}
	}
	if got, _ := envValue(t, a.settingsPath, "OTHER"); got != "keep" {
		t.Errorf("unrelated env lost: %q", got)
	}
}

func TestClaudeOnRepairsCurrentBaseWithMissingKnobs(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8081","OTHER":"keep"}}`)
	if code := a.on(); code != 0 {
		t.Fatalf("on = %d (%s)", code, out.String())
	}
	assertClaudeOnValues(t, a)
	if strings.Contains(out.String(), "already on") {
		t.Errorf("base-only state incorrectly reported already on: %q", out.String())
	}
	if got, _ := envValue(t, a.settingsPath, "OTHER"); got != "keep" {
		t.Errorf("unrelated env lost: %q", got)
	}
}

func TestClaudeOffWithoutBackupPreservesMatchingUserKnobs(t *testing.T) {
	a, _ := testAdapter(t)
	content := `{"env":{"CLAUDE_CODE_ATTRIBUTION_HEADER":"0","ENABLE_TOOL_SEARCH":"true","OTHER":"keep"}}` + "\n"
	writeSettingsFile(t, a, content)
	if code := a.off(); code != 0 {
		t.Fatal("off failed")
	}
	if got := string(fileBytes(t, a.settingsPath)); got != content {
		t.Errorf("off without ownership proof modified user settings:\n%s", got)
	}
}

func TestClaudeDriftReportSkipsValueAlreadyRestoredByUser(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com","CLAUDE_CODE_ATTRIBUTION_HEADER":"custom-before","ENABLE_TOOL_SEARCH":"auto:25"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	root := readTree(t, a.settingsPath)
	root["env"].(map[string]any)[claudeAttributionEnvKey] = "custom-before"
	root["theme"] = "dark"
	b, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(a.settingsPath, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if strings.Contains(out.String(), `CLAUDE_CODE_ATTRIBUTION_HEADER "custom-before" was NOT restored`) {
		t.Errorf("reported a false unrestored warning: %q", out.String())
	}
	if got, _ := envValue(t, a.settingsPath, claudeAttributionEnvKey); got != "custom-before" {
		t.Errorf("user-restored value lost: %q", got)
	}
}

func TestClaudeDriftPreservesPreexistingMatchingKnob(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com","ENABLE_TOOL_SEARCH":"true"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	root := readTree(t, a.settingsPath)
	root["theme"] = "dark"
	b, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(a.settingsPath, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if got, ok := envValue(t, a.settingsPath, claudeToolSearchEnvKey); !ok || got != claudeToolSearchValue {
		t.Errorf("preexisting matching tool-search value lost: %q ok=%v", got, ok)
	}
	if _, ok := envValue(t, a.settingsPath, claudeAttributionEnvKey); ok {
		t.Errorf("attribution value added by on was not stripped")
	}
	if _, ok := envValue(t, a.settingsPath, claudeManagedEnvKey); ok {
		t.Errorf("gateway base URL was not stripped")
	}
	if strings.Contains(out.String(), `ENABLE_TOOL_SEARCH "true" was NOT restored`) {
		t.Errorf("preexisting matching value incorrectly reported unrestored: %q", out.String())
	}
}

func TestClaudeDriftedManagedKnobsPreserveUserValues(t *testing.T) {
	a, out := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com","CLAUDE_CODE_ATTRIBUTION_HEADER":"custom-before","ENABLE_TOOL_SEARCH":"auto:25"}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	root := readTree(t, a.settingsPath)
	env := root["env"].(map[string]any)
	env[claudeAttributionEnvKey] = "custom-after"
	env[claudeToolSearchEnvKey] = "auto"
	root["theme"] = "dark"
	b, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(a.settingsPath, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if _, ok := envValue(t, a.settingsPath, claudeManagedEnvKey); ok {
		t.Errorf("gateway base URL not stripped")
	}
	if got, _ := envValue(t, a.settingsPath, claudeAttributionEnvKey); got != "custom-after" {
		t.Errorf("drifted attribution value lost: %q", got)
	}
	if got, _ := envValue(t, a.settingsPath, claudeToolSearchEnvKey); got != "auto" {
		t.Errorf("drifted tool-search value lost: %q", got)
	}
	for _, want := range []string{
		"CLAUDE_CODE_ATTRIBUTION_HEADER \"custom-before\" was NOT restored",
		"ENABLE_TOOL_SEARCH \"auto:25\" was NOT restored",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing unrestored-value note %q in %q", want, out.String())
		}
	}
}

func TestClaudeDoorPortResolution(t *testing.T) {
	claudeClient := config.Client{Name: "claude-code", BindAddr: "127.0.0.1:18081", ProtocolShape: "anthropic"}
	codexClient := config.Client{Name: "codex", BindAddr: "127.0.0.1:18083", ProtocolShape: "openai"}
	door := &config.Door{Ports: []config.DoorPort{{BindAddr: "127.0.0.1:8081", RouterAddr: "127.0.0.1:18081"}}}

	cases := []struct {
		name      string
		file      *config.File
		wantPort  string
		wantErr   string
		wantPorts []string
	}{
		{
			name:      "door port resolved for claude-code",
			file:      &config.File{Clients: []config.Client{claudeClient, codexClient}, Door: door},
			wantPort:  "8081",
			wantPorts: []string{"8081", "18081", "18083"},
		},
		{
			name:     "empty shape defaults to anthropic",
			file:     &config.File{Clients: []config.Client{{Name: "cc", BindAddr: "127.0.0.1:18081"}}, Door: door},
			wantPort: "8081",
		},
		{
			name:    "no door section",
			file:    &config.File{Clients: []config.Client{claudeClient}},
			wantErr: "no door: section",
		},
		{
			name:    "door does not route to the claude listener",
			file:    &config.File{Clients: []config.Client{{Name: "claude-code", BindAddr: "127.0.0.1:19999", ProtocolShape: "anthropic"}}, Door: door},
			wantErr: "no door port routes",
		},
		{
			name:    "no anthropic client",
			file:    &config.File{Clients: []config.Client{codexClient}, Door: door},
			wantErr: "no anthropic-shape client",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port, ports, _, err := claudeDoorPort(tc.file)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want contains %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if port != tc.wantPort {
				t.Errorf("port = %q, want %q", port, tc.wantPort)
			}
			for _, p := range tc.wantPorts {
				if !ports[p] {
					t.Errorf("gateway ports missing %s: %v", p, ports)
				}
			}
		})
	}
}

// TestCmdClaudeEnvPlumbing exercises the cmdClaude entry point end to
// end against temp paths via the documented env overrides.
func TestCmdClaudeEnvPlumbing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.yaml")
	cfg := `clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
door:
  ports:
    - bind_addr: 127.0.0.1:8081
      router_addr: 127.0.0.1:18081
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://corp-proxy.example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", cfgPath)
	t.Setenv("BASETEN_SWITCH_CLAUDE_SETTINGS", settings)
	t.Setenv("BASETEN_SWITCH_BACKUP_DIR", filepath.Join(dir, "backups"))

	if code := cmdClaude([]string{"on"}); code != 0 {
		t.Fatalf("cmdClaude on = %d", code)
	}
	if v, _ := envValue(t, settings, claudeManagedEnvKey); v != "http://127.0.0.1:8081" {
		t.Fatalf("on via cmdClaude: %q", v)
	}
	if code := cmdClaude([]string{"status"}); code != 0 {
		t.Errorf("status while on = %d", code)
	}
	// stop alias == off
	if code := cmdClaude([]string{"stop"}); code != 0 {
		t.Fatalf("cmdClaude stop = %d", code)
	}
	if v, _ := envValue(t, settings, claudeManagedEnvKey); v != "https://corp-proxy.example.com" {
		t.Errorf("stop did not restore: %q", v)
	}
	if code := cmdClaude([]string{"status"}); code != statusExitOff {
		t.Errorf("status while off = %d, want %d", code, statusExitOff)
	}
	if code := cmdClaude([]string{"bogus"}); code != 2 {
		t.Errorf("bogus subcommand = %d, want 2", code)
	}
	if code := cmdClaude(nil); code != 2 {
		t.Errorf("no subcommand = %d, want 2", code)
	}
}

func TestClaudeOnRemovesEnvBlockItCreated(t *testing.T) {
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{"permissions":{"allow":[]}}`)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	if code := a.off(); code != 0 {
		t.Fatal("off failed")
	}
	root := readTree(t, a.settingsPath)
	if _, ok := root["env"]; ok {
		t.Errorf("off left behind the env block on created: %v", root["env"])
	}
	if _, ok := root["permissions"]; !ok {
		t.Errorf("permissions lost")
	}
}
