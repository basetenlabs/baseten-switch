package main

import (
	"encoding/json"
	"testing"
)

func TestClaudeWrittenValuesJSONAndClone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
	}{
		{name: "legacy"},
		{name: "unowned", values: map[string]string{}},
		{name: "owned", values: map[string]string{claudeToolSearchEnvKey: "recorded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := &claudeBackup{WrittenValues: tc.values}
			raw, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			var restored claudeBackup
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if (restored.WrittenValues == nil) != (tc.values == nil) {
				t.Fatalf("round trip lost legacy versus unowned distinction: %s", raw)
			}
			cloned := cloneClaudeBackup(&restored)
			if (cloned.WrittenValues == nil) != (restored.WrittenValues == nil) {
				t.Fatal("clone lost legacy versus unowned distinction")
			}
			if cloned.WrittenValues != nil {
				cloned.WrittenValues[claudeToolSearchEnvKey] = "changed"
				if restored.WrittenValues[claudeToolSearchEnvKey] == "changed" {
					t.Fatal("clone shares ownership metadata")
				}
			}
		})
	}
}

func TestClaudeWrittenValuesPickerRefreshPreservesManualEdits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
		key    string
		value  string
	}{
		{name: "legacy_attribution", legacy: true, key: claudeAttributionEnvKey, value: "1"},
		{name: "current_attribution", key: claudeAttributionEnvKey, value: "0"},
		{name: "tool_search", key: claudeToolSearchEnvKey, value: "manual"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, a, setProjection := prepareMillionContextPicker(t)
			if tc.legacy {
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
			}
			editAttributionSettings(t, a, func(root map[string]any) {
				root["env"].(map[string]any)[tc.key] = tc.value
			})
			// A changed picker row refreshes the whole-file restore hash.
			// That must not silently reclaim an independently edited env key.
			setProjection(true, 200_000)
			if err := a.syncModelPicker(false); err != nil {
				t.Fatal(err)
			}
			bak, err := loadClaudeBackup(a.backupPath)
			if err != nil {
				t.Fatal(err)
			}
			if bak.WrittenHash != sha256Hex(fileBytes(t, a.settingsPath)) {
				t.Fatal("test did not reach a refreshed clean-restore hash")
			}
			if rc := a.off(); rc != 0 {
				t.Fatalf("off = %d", rc)
			}
			if got, ok := envValue(t, a.settingsPath, tc.key); !ok || got != tc.value {
				t.Fatalf("off changed manual value: got %q, exists %v", got, ok)
			}
		})
	}
}

func TestClaudeWrittenValuesOffUsesRecordedValue(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "drifted"}[drift], func(t *testing.T) {
			a, _ := testAdapter(t)
			writeSettingsFile(t, a, `{"theme":"dark"}`)
			if rc := a.on(); rc != 0 {
				t.Fatalf("on = %d", rc)
			}
			root, snap, err := readClaudeSettings(a.settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			const recorded = "a-different-written-value"
			root["env"].(map[string]any)[claudeToolSearchEnvKey] = recorded
			raw, committed, err := writeClaudeSettings(snap, root)
			if err != nil {
				t.Fatal(err)
			}
			bak, err := loadClaudeBackup(a.backupPath)
			if err != nil {
				t.Fatal(err)
			}
			bak.WrittenValues[claudeToolSearchEnvKey] = recorded
			bak.WrittenHash = sha256Hex(raw)
			recordClaudeBackupFile(bak, committed)
			if err := saveClaudeBackup(a.backupPath, bak); err != nil {
				t.Fatal(err)
			}
			if drift {
				editAttributionSettings(t, a, func(root map[string]any) { root["theme"] = "light" })
			}
			if rc := a.off(); rc != 0 {
				t.Fatalf("off = %d", rc)
			}
			if value, ok := envValue(t, a.settingsPath, claudeToolSearchEnvKey); ok {
				t.Fatalf("off used today's desired value instead of its receipt: %q", value)
			}
		})
	}
}
