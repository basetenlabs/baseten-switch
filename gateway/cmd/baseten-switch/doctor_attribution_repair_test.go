package main

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Exercise the real adapter against an older receipt while the existing doctor
// fixture supplies healthy local mock services. No external process or account
// is needed for the repair loop.
func doctorLegacyAttributionFixture(t *testing.T) (*claudeAdapter, map[string]any) {
	t.Helper()
	newDoctorFixture(t, nil)
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.backupPath); err != nil {
		t.Fatal(err)
	}
	writeSettingsFile(t, a, `{"env":{"ANTHROPIC_BASE_URL":"https://example.invalid","CLAUDE_CODE_ATTRIBUTION_HEADER":"original-attribution","ENABLE_TOOL_SEARCH":"original-search","UNRELATED":"keep"},"permissions":{"allow":["Read"]},"theme":"dark"}`)
	original := readTree(t, a.settingsPath)
	if rc := a.on(); rc != 0 {
		t.Fatalf("initial on = %d", rc)
	}
	root, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	root["env"].(map[string]any)[claudeAttributionEnvKey] = "0"
	raw, committed, err := writeClaudeSettings(snap, root)
	if err != nil {
		t.Fatal(err)
	}
	bak, err := loadClaudeBackup(a.backupPath)
	if err != nil {
		t.Fatal(err)
	}
	bak.WrittenValues = nil
	bak.WrittenHash = sha256Hex(raw)
	recordClaudeBackupFile(bak, committed)
	if err := saveClaudeBackup(a.backupPath, bak); err != nil {
		t.Fatal(err)
	}
	return a, original
}

func TestDoctorAttributionConfirmedRepairAndRestore(t *testing.T) {
	a, original := doctorLegacyAttributionFixture(t)
	settingsBefore := fileBytes(t, a.settingsPath)
	backupBefore := fileBytes(t, a.backupPath)
	rep := runDoctor(doctorOpts{})
	if rep.FirstFailure != "claude/attribution_header" {
		t.Fatalf("first failure = %q, want attribution", rep.FirstFailure)
	}
	check := findCheck(t, rep, "claude", "attribution_header")
	if check.Status != docFail || strings.Join(check.fixArgv, " ") != "claude on" {
		t.Fatalf("attribution check = %+v", check)
	}
	calls := setDoctorFixSeams(t, true, "y\n", func(argv []string) error {
		if strings.Join(argv, " ") != "claude on" {
			return fmt.Errorf("unexpected fix command: %v", argv)
		}
		if rc := cmdClaude(argv[1:]); rc != 0 {
			return fmt.Errorf("actual claude on failed: %d", rc)
		}
		return nil
	})
	out, rc := captureStdout(t, func() int { return cmdDoctor(nil) })
	if rc != 1 || len(*calls) != 0 {
		t.Fatalf("plain doctor = %d, fixes = %v\n%s", rc, *calls, out)
	}
	if !bytes.Equal(settingsBefore, fileBytes(t, a.settingsPath)) || !bytes.Equal(backupBefore, fileBytes(t, a.backupPath)) {
		t.Fatal("plain doctor changed settings or receipt")
	}
	out, rc = captureStdout(t, func() int { return cmdDoctor([]string{"--fix"}) })
	if rc != 0 || len(*calls) != 1 || !strings.Contains(out, "apply? [y/N]") {
		t.Fatalf("confirmed repair = %d, fixes = %v\n%s", rc, *calls, out)
	}
	if got, _ := envValue(t, a.settingsPath, claudeAttributionEnvKey); got != "1" {
		t.Fatalf("repaired attribution = %q", got)
	}
	if got := findCheck(t, runDoctor(doctorOpts{}), "claude", "attribution_header"); got.Status != docOK {
		t.Fatalf("repaired attribution check = %+v", got)
	}
	settingsAfter := fileBytes(t, a.settingsPath)
	backupAfter := fileBytes(t, a.backupPath)
	out, rc = captureStdout(t, func() int { return cmdDoctor([]string{"--fix"}) })
	if rc != 0 || len(*calls) != 1 || strings.Contains(out, "apply? [y/N]") {
		t.Fatalf("second repair = %d, fixes = %v\n%s", rc, *calls, out)
	}
	if !bytes.Equal(settingsAfter, fileBytes(t, a.settingsPath)) || !bytes.Equal(backupAfter, fileBytes(t, a.backupPath)) {
		t.Fatal("second doctor repair changed settings or receipt")
	}
	if rc := a.off(); rc != 0 {
		t.Fatalf("off = %d", rc)
	}
	if got := readTree(t, a.settingsPath); !reflect.DeepEqual(got, original) {
		t.Fatalf("original values or unrelated fields were not restored: %+v", got)
	}
}

func TestDoctorAttributionDeclinedRepairPreservesSettings(t *testing.T) {
	a, _ := doctorLegacyAttributionFixture(t)
	settingsBefore := fileBytes(t, a.settingsPath)
	backupBefore := fileBytes(t, a.backupPath)
	calls := setDoctorFixSeams(t, true, "n\n", nil)
	out, rc := captureStdout(t, func() int { return cmdDoctor([]string{"--fix"}) })
	if rc != 1 || len(*calls) != 0 || !strings.Contains(out, "not applied") {
		t.Fatalf("declined repair = %d, fixes = %v\n%s", rc, *calls, out)
	}
	if got, _ := envValue(t, a.settingsPath, claudeAttributionEnvKey); got != "0" {
		t.Fatalf("declined repair changed attribution: %q", got)
	}
	if !bytes.Equal(settingsBefore, fileBytes(t, a.settingsPath)) || !bytes.Equal(backupBefore, fileBytes(t, a.backupPath)) {
		t.Fatal("declined repair changed settings or receipt")
	}
}
