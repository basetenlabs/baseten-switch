package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func legacyAttributionAdapter(t *testing.T) *claudeAdapter {
	t.Helper()
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"OTHER":"keep"},"theme":"dark"}`)
	if rc := a.on(); rc != 0 {
		t.Fatal("on failed")
	}
	root, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	root["env"].(map[string]any)[claudeAttributionEnvKey] = "0"
	raw, snap, err := writeClaudeSettings(snap, root)
	if err != nil {
		t.Fatal(err)
	}
	bak, err := loadClaudeBackup(a.backupPath)
	if err != nil {
		t.Fatal(err)
	}
	bak.WrittenValues = nil
	bak.WrittenHash = sha256Hex(raw)
	recordClaudeBackupFile(bak, snap)
	if err := saveClaudeBackup(a.backupPath, bak); err != nil {
		t.Fatal(err)
	}
	return a
}

func editAttributionSettings(t *testing.T, a *claudeAdapter, edit func(map[string]any)) {
	t.Helper()
	root := readTree(t, a.settingsPath)
	edit(root)
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	writeSettingsFile(t, a, string(raw))
}

func TestClaudeLegacyAttributionOffUsesPreviousWrittenValue(t *testing.T) {
	for _, value := range []string{"0", "1"} {
		t.Run(value, func(t *testing.T) {
			a := legacyAttributionAdapter(t)
			editAttributionSettings(t, a, func(root map[string]any) {
				root["theme"] = "light"
				root["env"].(map[string]any)[claudeAttributionEnvKey] = value
			})
			if rc := a.off(); rc != 0 {
				t.Fatalf("off = %d", rc)
			}
			got, exists := envValue(t, a.settingsPath, claudeAttributionEnvKey)
			if value == "0" && exists {
				t.Fatal("off left legacy owned value")
			}
			if value == "1" && (!exists || got != "1") {
				t.Fatal("off removed user's changed value")
			}
		})
	}
}

func TestClaudeObservedAttributionRemainsUserOwnedOnOff(t *testing.T) {
	for _, operation := range []string{"already_on", "repair_other_setting"} {
		t.Run(operation, func(t *testing.T) {
			a := legacyAttributionAdapter(t)
			editAttributionSettings(t, a, func(root map[string]any) {
				env := root["env"].(map[string]any)
				env[claudeAttributionEnvKey] = "1"
				if operation == "repair_other_setting" {
					env[claudeToolSearchEnvKey] = "manual"
				}
			})
			before, _ := loadClaudeBackup(a.backupPath)
			settingsBefore := fileBytes(t, a.settingsPath)
			if rc := a.on(); rc != 0 {
				t.Fatalf("on = %d", rc)
			}
			after, _ := loadClaudeBackup(a.backupPath)
			if after.WrittenValues == nil || after.WrittenValues[claudeAttributionEnvKey] != "" {
				t.Fatal("observed value was claimed as written")
			}
			if before.Values[claudeAttributionEnvKey] != after.Values[claudeAttributionEnvKey] || backupCovers(before, claudeAttributionEnvKey) != backupCovers(after, claudeAttributionEnvKey) {
				t.Fatal("original attribution backup changed")
			}
			if operation == "already_on" && (before.WrittenHash != after.WrittenHash || !bytes.Equal(settingsBefore, fileBytes(t, a.settingsPath))) {
				t.Fatal("receipt update rewrote settings or cleared drift")
			}
			if rc := a.off(); rc != 0 {
				t.Fatalf("off = %d", rc)
			}
			if got, ok := envValue(t, a.settingsPath, claudeAttributionEnvKey); !ok || got != "1" {
				t.Fatal("off removed user-enabled attribution")
			}
		})
	}
}

func TestClaudeOnUpgradesLegacyAttributionAndRestoresOriginal(t *testing.T) {
	a := legacyAttributionAdapter(t)
	bak, _ := loadClaudeBackup(a.backupPath)
	bak.Values[claudeAttributionEnvKey] = "0"
	if err := saveClaudeBackup(a.backupPath, bak); err != nil {
		t.Fatal(err)
	}
	if rc := a.on(); rc != 0 {
		t.Fatalf("on = %d", rc)
	}
	assertClaudeOnValues(t, a)
	bak, _ = loadClaudeBackup(a.backupPath)
	if bak.WrittenValues[claudeAttributionEnvKey] != "1" {
		t.Fatal("on did not record its written value")
	}
	if rc := a.off(); rc != 0 {
		t.Fatalf("off = %d", rc)
	}
	if got, _ := envValue(t, a.settingsPath, claudeAttributionEnvKey); got != "0" {
		t.Fatal("original opt-out not restored")
	}
}

func TestClaudeOnRecordsOnlyChangedEnvValues(t *testing.T) {
	a, _ := testAdapter(t)
	writeSettingsFile(t, a, `{"env":{"CLAUDE_CODE_ATTRIBUTION_HEADER":"1","ENABLE_TOOL_SEARCH":"manual"}}`)
	if rc := a.on(); rc != 0 {
		t.Fatalf("on = %d", rc)
	}
	bak, err := loadClaudeBackup(a.backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(bak.WrittenValues) != 2 || bak.WrittenValues[claudeManagedEnvKey] != a.desiredURL() || bak.WrittenValues[claudeToolSearchEnvKey] != claudeToolSearchValue {
		t.Fatalf("receipt does not describe the actual writes: %v", bak.WrittenValues)
	}
	if _, owned := bak.WrittenValues[claudeAttributionEnvKey]; owned {
		t.Fatal("receipt claimed the preexisting attribution value")
	}
	if rc := a.off(); rc != 0 {
		t.Fatalf("off = %d", rc)
	}
	if got, _ := envValue(t, a.settingsPath, claudeAttributionEnvKey); got != "1" {
		t.Fatal("off removed preexisting attribution")
	}
	if got, _ := envValue(t, a.settingsPath, claudeToolSearchEnvKey); got != "manual" {
		t.Fatal("off failed to restore the original tool-search value")
	}
}
