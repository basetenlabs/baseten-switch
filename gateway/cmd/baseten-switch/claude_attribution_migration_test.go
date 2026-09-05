package main

import (
	"bytes"
	"encoding/json"
	"os"
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
	bak.AttributionState = ""
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

func TestClaudeAttributionMigrationAndOff(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "drifted"}[drift], func(t *testing.T) {
			a := legacyAttributionAdapter(t)
			before, _ := loadClaudeBackup(a.backupPath)
			if drift {
				editAttributionSettings(t, a, func(root map[string]any) {
					root["env"].(map[string]any)[claudeToolSearchEnvKey] = "manual"
					root["theme"] = "light"
				})
			}
			changed, err := a.migrateAttribution()
			if err != nil || !changed {
				t.Fatalf("migration = %v, %v", changed, err)
			}
			if got, _ := envValue(t, a.settingsPath, claudeAttributionEnvKey); got != "1" {
				t.Fatalf("attribution = %q, want enabled", got)
			}
			after, _ := loadClaudeBackup(a.backupPath)
			if after.AttributionState != claudeAttributionOwned || !backupCovers(after, claudeAttributionEnvKey) {
				t.Fatal("migration lost ownership state")
			}
			if drift && after.WrittenHash != before.WrittenHash {
				t.Fatal("migration incorrectly cleared existing drift")
			}
			if changed, err := a.migrateAttribution(); changed || err != nil {
				t.Fatalf("second migration = %v, %v", changed, err)
			}
			if rc := a.off(); rc != 0 {
				t.Fatalf("off = %d", rc)
			}
			if _, ok := envValue(t, a.settingsPath, claudeAttributionEnvKey); ok {
				t.Fatal("off left managed attribution")
			}
			if drift {
				if got, _ := envValue(t, a.settingsPath, claudeToolSearchEnvKey); got != "manual" {
					t.Fatal("off lost user tool-search edit")
				}
				if readTree(t, a.settingsPath)["theme"] != "light" {
					t.Fatal("off lost unrelated edit")
				}
			}
		})
	}
}

func TestClaudeAttributionMigrationPreservesLaterOptOut(t *testing.T) {
	a := legacyAttributionAdapter(t)
	if _, err := a.migrateAttribution(); err != nil {
		t.Fatal(err)
	}
	editAttributionSettings(t, a, func(root map[string]any) { root["env"].(map[string]any)[claudeAttributionEnvKey] = "0" })
	before := fileBytes(t, a.settingsPath)
	if changed, err := a.migrateAttribution(); changed || err != nil {
		t.Fatalf("migration = %v, %v", changed, err)
	}
	if !bytes.Equal(before, fileBytes(t, a.settingsPath)) {
		t.Fatal("migration overwrote user opt-out")
	}
	if rc := a.off(); rc != 0 {
		t.Fatalf("off = %d", rc)
	}
	if got, _ := envValue(t, a.settingsPath, claudeAttributionEnvKey); got != "0" {
		t.Fatal("off removed user opt-out")
	}
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

func TestClaudeAttributionMigrationMarksAlreadyEnabled(t *testing.T) {
	for _, explicitOn := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "on"}[explicitOn], func(t *testing.T) {
			a := legacyAttributionAdapter(t)
			editAttributionSettings(t, a, func(root map[string]any) { root["env"].(map[string]any)[claudeAttributionEnvKey] = "1" })
			before, _ := loadClaudeBackup(a.backupPath)
			settingsBefore := fileBytes(t, a.settingsPath)
			if explicitOn {
				if rc := a.on(); rc != 0 {
					t.Fatalf("on = %d", rc)
				}
			} else if changed, err := a.migrateAttribution(); changed || err != nil {
				t.Fatalf("migration = %v, %v", changed, err)
			}
			after, _ := loadClaudeBackup(a.backupPath)
			if after.AttributionState != claudeAttributionPreserved || before.WrittenHash != after.WrittenHash {
				t.Fatal("marker update lost drift")
			}
			if !bytes.Equal(settingsBefore, fileBytes(t, a.settingsPath)) {
				t.Fatal("marker update rewrote settings")
			}
			editAttributionSettings(t, a, func(root map[string]any) { root["env"].(map[string]any)[claudeAttributionEnvKey] = "0" })
			if changed, err := a.migrateAttribution(); changed || err != nil {
				t.Fatalf("migration after opt-out = %v, %v", changed, err)
			}
		})
	}
}

func TestClaudeObservedAttributionRemainsUserOwnedOnOff(t *testing.T) {
	for _, operation := range []string{"startup", "already_on", "repair_other_setting"} {
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
			if operation == "startup" {
				if changed, err := a.migrateAttribution(); changed || err != nil {
					t.Fatalf("migration = %v, %v", changed, err)
				}
			} else if rc := a.on(); rc != 0 {
				t.Fatalf("on = %d", rc)
			}
			after, _ := loadClaudeBackup(a.backupPath)
			if after.AttributionState != claudeAttributionPreserved {
				t.Fatal("observed value was claimed as written")
			}
			if before.Values[claudeAttributionEnvKey] != after.Values[claudeAttributionEnvKey] || backupCovers(before, claudeAttributionEnvKey) != backupCovers(after, claudeAttributionEnvKey) {
				t.Fatal("original attribution backup changed")
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

func TestClaudeAttributionMigrationRequiresOwnership(t *testing.T) {
	for _, scenario := range []string{"no_backup", "prior_zero", "uncovered", "foreign_base", "other_gateway", "user_value", "retargeted", "other_config"} {
		t.Run(scenario, func(t *testing.T) {
			a := legacyAttributionAdapter(t)
			bak, _ := loadClaudeBackup(a.backupPath)
			switch scenario {
			case "no_backup":
				if err := os.Remove(a.backupPath); err != nil {
					t.Fatal(err)
				}
			case "prior_zero":
				bak.Values[claudeAttributionEnvKey] = "0"
			case "uncovered":
				bak.Missing = nil
			case "retargeted":
				bak.ResolvedPath += ".other"
			case "other_config":
				bak.ConfigPath += ".other"
			case "foreign_base", "other_gateway", "user_value":
				editAttributionSettings(t, a, func(root map[string]any) {
					env := root["env"].(map[string]any)
					if scenario == "user_value" {
						env[claudeAttributionEnvKey] = "custom"
					} else if scenario == "foreign_base" {
						env[claudeManagedEnvKey] = "https://example.com"
					} else {
						env[claudeManagedEnvKey] = "http://127.0.0.1:18081"
					}
				})
			}
			if scenario != "no_backup" {
				if err := saveClaudeBackup(a.backupPath, bak); err != nil {
					t.Fatal(err)
				}
			}
			before := fileBytes(t, a.settingsPath)
			changed, err := a.migrateAttribution()
			wantErr := scenario == "prior_zero" || scenario == "uncovered" || scenario == "retargeted" || scenario == "other_config"
			if changed || (err != nil) != wantErr {
				t.Fatalf("migration = %v, %v", changed, err)
			}
			if !bytes.Equal(before, fileBytes(t, a.settingsPath)) {
				t.Fatal("migration changed unowned settings")
			}
		})
	}
}

func TestClaudeAttributionMigrationRejectsConcurrentWrite(t *testing.T) {
	a := legacyAttributionAdapter(t)
	old := claudeBeforeSettingsMutation
	t.Cleanup(func() { claudeBeforeSettingsMutation = old })
	claudeBeforeSettingsMutation = func() { writeSettingsFile(t, a, `{"theme":"user edit"}`) }
	if changed, err := a.migrateAttribution(); changed || err == nil {
		t.Fatalf("migration = %v, %v", changed, err)
	}
	if readTree(t, a.settingsPath)["theme"] != "user edit" {
		t.Fatal("lost concurrent edit")
	}
	bak, _ := loadClaudeBackup(a.backupPath)
	if bak.AttributionState != "" {
		t.Fatal("failed migration marked complete")
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
	if bak.AttributionState != claudeAttributionOwned {
		t.Fatal("on did not mark migration")
	}
	if rc := a.off(); rc != 0 {
		t.Fatalf("off = %d", rc)
	}
	if got, _ := envValue(t, a.settingsPath, claudeAttributionEnvKey); got != "0" {
		t.Fatal("original opt-out not restored")
	}
}
