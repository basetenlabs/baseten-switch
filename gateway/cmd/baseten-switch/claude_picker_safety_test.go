package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func stubPickerVersion(t *testing.T, version string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("stub"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldLook, oldCommand, oldTimeout := claudePickerLookPath, claudePickerCommand, claudePickerVersionTimeout
	t.Cleanup(func() {
		claudePickerLookPath, claudePickerCommand, claudePickerVersionTimeout = oldLook, oldCommand, oldTimeout
	})
	claudePickerLookPath = func(string) (string, error) { return bin, nil }
	claudePickerCommand = func(context.Context, string, ...string) ([]byte, error) { return []byte(version), nil }
}

func prepareManagedPicker(t *testing.T, adminDown bool) (*subagentTestEnv, *claudeAdapter) {
	t.Helper()
	env := newSubagentTestEnv(t, adminDown)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{
		Enabled: true, Models: []config.ModelPickerModel{{Alias: "claude-baseten-glm-5-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if rc := a.on(); rc != 0 {
		t.Fatalf("claude on rc=%d", rc)
	}
	if err := a.syncModelPicker(false); err != nil {
		t.Fatalf("initial picker sync: %v", err)
	}
	return env, a
}

func TestPickerSyncSettingsCASFailureRestoresExactPriorBackup(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{
		Enabled: true, Models: []config.ModelPickerModel{{Alias: "claude-baseten-glm-5-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if rc := a.on(); rc != 0 {
		t.Fatalf("claude on rc=%d", rc)
	}
	backupBefore, err := os.ReadFile(a.backupPath)
	if err != nil {
		t.Fatal(err)
	}
	oldHook := claudeBeforeSettingsMutation
	t.Cleanup(func() { claudeBeforeSettingsMutation = oldHook })
	claudeBeforeSettingsMutation = func() {
		root := readTree(t, env.settings)
		root["concurrent"] = true
		raw, _ := json.Marshal(root)
		if err := os.WriteFile(env.settings, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.syncModelPicker(false); err == nil {
		t.Fatal("expected stale settings CAS failure")
	}
	backupAfter, err := os.ReadFile(a.backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupBefore, backupAfter) {
		t.Fatal("staged picker backup was not rolled back byte-for-byte")
	}
	if readTree(t, env.settings)["concurrent"] != true {
		t.Fatal("concurrent settings edit was not preserved")
	}
}

func TestPickerSyncSettingsCASFailureRemovesNewlyStagedBackup(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{
		Enabled: true, Models: []config.ModelPickerModel{{Alias: "claude-baseten-glm-5-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	root := readTree(t, env.settings)
	envBlock, _ := settingsEnv(root)
	envBlock[claudeAttributionEnvKey] = claudeAttributionValue
	envBlock[claudeToolSearchEnvKey] = claudeToolSearchValue
	raw, _ := json.Marshal(root)
	if err := os.WriteFile(env.settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Fatalf("unexpected initial backup: %v", err)
	}
	oldHook := claudeBeforeSettingsMutation
	t.Cleanup(func() { claudeBeforeSettingsMutation = oldHook })
	claudeBeforeSettingsMutation = func() {
		concurrent := readTree(t, env.settings)
		concurrent["concurrent"] = true
		updated, _ := json.Marshal(concurrent)
		if err := os.WriteFile(env.settings, updated, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.syncModelPicker(false); err == nil {
		t.Fatal("expected stale settings CAS failure")
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Fatalf("newly staged backup was not removed: %v", err)
	}
}

func TestPickerSyncRefusesConfigChangeAfterProjectionAndRestoresBackup(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{
		Enabled: true, Models: []config.ModelPickerModel{{Alias: "claude-baseten-glm-5-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if rc := a.on(); rc != 0 {
		t.Fatalf("claude on rc=%d", rc)
	}
	backupBefore, _ := os.ReadFile(a.backupPath)
	settingsBefore, _ := os.ReadFile(env.settings)
	oldHook := claudeBeforeSettingsMutation
	t.Cleanup(func() { claudeBeforeSettingsMutation = oldHook })
	claudeBeforeSettingsMutation = func() {
		if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{Enabled: false}); err != nil {
			t.Fatal(err)
		}
	}
	err = a.syncModelPicker(false)
	if err == nil || !strings.Contains(err.Error(), "config changed") {
		t.Fatalf("sync error = %v", err)
	}
	backupAfter, _ := os.ReadFile(a.backupPath)
	settingsAfter, _ := os.ReadFile(env.settings)
	if !bytes.Equal(backupBefore, backupAfter) || !bytes.Equal(settingsBefore, settingsAfter) {
		t.Fatal("stale projection changed settings or backup")
	}
}

func TestPickerSyncNeverHoldsConfigAndSettingsLocksTogether(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{
		Enabled: true, Models: []config.ModelPickerModel{{Alias: "claude-baseten-glm-5-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if rc := a.on(); rc != 0 {
		t.Fatalf("claude on rc=%d", rc)
	}
	oldHook := claudeBeforeSettingsMutation
	t.Cleanup(func() { claudeBeforeSettingsMutation = oldHook })
	claudeBeforeSettingsMutation = func() {
		lock, lockErr := acquireConfigMutationLock(env.cfgPath)
		if lockErr != nil {
			t.Fatalf("config lock remained held during settings mutation: %v", lockErr)
		}
		lock.close()
	}
	if err := a.syncModelPicker(false); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeOffRemovesBaseWiringWhenMovedPickerNeedsManualResolution(t *testing.T) {
	env, a := prepareManagedPicker(t, true)
	root := readTree(t, env.settings)
	obj := root["modelPicker"].(map[string]any)
	rows := obj["options"].([]any)
	obj["options"] = append([]any{map[string]any{"model": "external", "label": "keep"}}, rows...)
	raw, _ := json.Marshal(root)
	if err := os.WriteFile(env.settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := a.off(); rc != 1 {
		t.Fatalf("claude off rc=%d, want manual-resolution failure", rc)
	}
	after := readTree(t, env.settings)
	envBlock, _ := settingsEnv(after)
	if _, ok := envString(envBlock, claudeManagedEnvKey); ok {
		t.Fatal("claude off left base URL wiring installed")
	}
	if _, ok := after["modelPicker"]; !ok {
		t.Fatal("moved picker rows should be preserved")
	}
	bak, err := loadClaudeBackup(a.backupPath)
	if err != nil || bak == nil || bak.ModelPicker == nil {
		t.Fatalf("picker recovery backup = %+v, err=%v", bak, err)
	}
}

func TestReplacementModeRequiresConfirmationAndRestoresOriginal(t *testing.T) {
	root := map[string]any{"modelPicker": map[string]any{
		"replaceBuiltInOptions": true,
		"options":               pickerTestRows("external"),
	}}
	if _, _, err := installModelPicker(root, pickerTestRows("claude-baseten-a"), nil); err == nil {
		t.Fatal("expected replacement-mode confirmation error")
	}
	bak, _, err := installModelPickerWithOptions(root, pickerTestRows("claude-baseten-a"), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if root["modelPicker"].(map[string]any)["replaceBuiltInOptions"] != false {
		t.Fatal("confirmed conversion did not select append mode")
	}
	if _, err := cleanupModelPicker(root, bak, true); err != nil {
		t.Fatal(err)
	}
	obj := root["modelPicker"].(map[string]any)
	if obj["replaceBuiltInOptions"] != true || len(obj["options"].([]any)) != 1 {
		t.Fatalf("replacement object was not restored: %#v", obj)
	}
}

func TestReplacementModeRejectsNonBoolean(t *testing.T) {
	root := map[string]any{"modelPicker": map[string]any{
		"replaceBuiltInOptions": "false", "options": []any{},
	}}
	if _, _, err := installModelPickerWithOptions(root, pickerTestRows("a"), nil, true); err == nil {
		t.Fatal("expected non-boolean replacement mode error")
	}
}

func TestPickerManualConflictsAreTypedButConversionConfirmationIsNot(t *testing.T) {
	tests := []struct {
		name string
		root map[string]any
	}{
		{name: "non-object", root: map[string]any{"modelPicker": "bad"}},
		{name: "non-array options", root: map[string]any{"modelPicker": map[string]any{"options": "bad"}}},
		{name: "malformed row", root: map[string]any{"modelPicker": map[string]any{"options": []any{"bad"}}}},
		{name: "non-bool replacement", root: map[string]any{"modelPicker": map[string]any{"replaceBuiltInOptions": "false", "options": []any{}}}},
		{name: "duplicate desired id", root: map[string]any{"modelPicker": map[string]any{"options": pickerTestRows("claude-baseten-a")}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := installModelPicker(tc.root, pickerTestRows("claude-baseten-a"), nil)
			if err == nil || !isPickerManualResolution(err) {
				t.Fatalf("error = %v, want typed manual resolution", err)
			}
		})
	}
	root := map[string]any{"modelPicker": map[string]any{"replaceBuiltInOptions": true, "options": []any{}}}
	_, _, err := installModelPicker(root, pickerTestRows("claude-baseten-a"), nil)
	if err == nil || isPickerManualResolution(err) {
		t.Fatalf("replacement confirmation error = %v, want non-manual retry with explicit flag", err)
	}
}

func TestDisabledPickerSyncRemovesOwnedRowsAndPreservesExternalRows(t *testing.T) {
	env, a := prepareManagedPicker(t, true)
	root := readTree(t, env.settings)
	obj := root["modelPicker"].(map[string]any)
	obj["options"] = append(obj["options"].([]any), map[string]any{"model": "external", "label": "keep"})
	raw, _ := json.Marshal(root)
	if err := os.WriteFile(env.settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := a.syncModelPicker(false); err != nil {
		t.Fatal(err)
	}
	obj = readTree(t, env.settings)["modelPicker"].(map[string]any)
	rows := obj["options"].([]any)
	if len(rows) != 1 || rowModel(rows[0]) != "external" {
		t.Fatalf("rows after disabled sync = %#v", rows)
	}
	status, err := a.currentPickerStatus()
	if err != nil || status.UserFileSync != "synced" {
		t.Fatalf("disabled status = %+v, err=%v", status, err)
	}
}

func TestPickerDisableMovedRowReturnsManualResolutionAction(t *testing.T) {
	env, a := prepareManagedPicker(t, true)
	root := readTree(t, env.settings)
	obj := root["modelPicker"].(map[string]any)
	rows := obj["options"].([]any)
	obj["options"] = append([]any{map[string]any{"model": "external", "label": "keep"}}, rows...)
	raw, _ := json.Marshal(root)
	if err := os.WriteFile(env.settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if rc := a.mutatePickerConfig(
		[]string{"disable"},
		mutationOptions{JSON: true, OperationID: "disable-moved"},
		&out,
	); rc != 1 {
		t.Fatalf("disable rc=%d output=%s", rc, out.String())
	}
	var receipt mutationResult
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ReconciliationAction != "mutation_reconcile_then_manual_claude_settings_resolution" || receipt.Outcome != "manual_resolution_required" ||
		receipt.Error == nil || receipt.Error.Retryable {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestDisabledPickerStatusSyncedWithExternalRowsAndNoOwnership(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.settings, []byte(`{"modelPicker":{"replaceBuiltInOptions":false,"options":[{"model":"external"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := newClaudeAdapterFromEnv()
	status, err := a.currentPickerStatus()
	if err != nil || status.UserFileSync != "synced" {
		t.Fatalf("status = %+v, err=%v", status, err)
	}
}

func TestPickerEnableDryRunForAbsentPickerReturnsNoModels(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelAliases(env.cfgPath, "claude-code", map[string]string{
		"claude-baseten-another-glm": "zai-org/GLM-5.2",
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := newClaudeAdapterFromEnv()
	var out bytes.Buffer
	if rc := a.picker([]string{"enable", "--dry-run", "--json"}, &out); rc != 0 {
		t.Fatalf("enable preview rc=%d, output=%s", rc, out.String())
	}
	var preview claudePickerEnablePreview
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Models) != 0 {
		t.Fatalf("preview models = %+v, want empty", preview.Models)
	}
}

func TestPickerEnableDryRunReflectsDisabledSavedRows(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{
		Enabled: false,
		Models: []config.ModelPickerModel{
			{Alias: "claude-baseten-kimi-k2-7"},
			{Alias: "claude-baseten-glm-5-2"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := newClaudeAdapterFromEnv()
	var out bytes.Buffer
	if rc := a.picker([]string{"enable", "--dry-run", "--json"}, &out); rc != 0 {
		t.Fatalf("enable preview rc=%d, output=%s", rc, out.String())
	}
	var preview claudePickerEnablePreview
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Models) != 2 ||
		preview.Models[0].Alias != "claude-baseten-kimi-k2-7" ||
		preview.Models[1].Alias != "claude-baseten-glm-5-2" {
		t.Fatalf("preview models = %+v, want saved row order", preview.Models)
	}
}

func TestPickerEnablePreservesExplicitEmptySelection(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{Enabled: false, Models: []config.ModelPickerModel{}}); err != nil {
		t.Fatal(err)
	}
	stubPickerVersion(t, "2.1.243")
	a, _ := newClaudeAdapterFromEnv()
	if rc := a.on(); rc != 0 {
		t.Fatalf("claude on rc=%d", rc)
	}
	var out bytes.Buffer
	if rc := a.picker([]string{"enable", "--json"}, &out); rc != 0 {
		t.Fatalf("enable rc=%d, output=%s", rc, out.String())
	}
	f, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if f.Clients[0].ModelPicker == nil || !f.Clients[0].ModelPicker.Enabled || len(f.Clients[0].ModelPicker.Models) != 0 {
		t.Fatalf("explicit empty picker was bootstrapped: %+v", f.Clients[0].ModelPicker)
	}
}

func TestPickerEnablePreservesDisabledSavedRows(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{
		Enabled: false,
		Models: []config.ModelPickerModel{{
			Alias: "claude-baseten-kimi-k2-7",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	stubPickerVersion(t, "2.1.243")
	a, _ := newClaudeAdapterFromEnv()
	if rc := a.on(); rc != 0 {
		t.Fatalf("claude on rc=%d", rc)
	}
	var out bytes.Buffer
	if rc := a.picker([]string{"enable", "--json"}, &out); rc != 0 {
		t.Fatalf("enable rc=%d, output=%s", rc, out.String())
	}
	f, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	picker := f.Clients[0].ModelPicker
	if picker == nil || !picker.Enabled || len(picker.Models) != 1 ||
		picker.Models[0].Alias != "claude-baseten-kimi-k2-7" {
		t.Fatalf("re-enabled picker = %+v, want saved row", picker)
	}
	obj, _, err := modelPickerObject(readTree(t, env.settings))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := modelPickerOptions(obj)
	if err != nil || len(rows) != 1 ||
		rowModel(rows[0]) != "claude-baseten-kimi-k2-7" {
		t.Fatalf("installed rows = %#v, err=%v", rows, err)
	}
}

func TestPickerEnableAbsentCreatesEmptySelectionWithoutCopyingAliases(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	stubPickerVersion(t, "2.1.243")
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if rc := a.on(); rc != 0 {
		t.Fatalf("claude on rc=%d", rc)
	}
	var out bytes.Buffer
	if rc := a.mutatePickerConfig(
		[]string{"enable"},
		mutationOptions{JSON: true, OperationID: "empty-enable"},
		&out,
	); rc != 0 {
		t.Fatalf("enable rc=%d output=%s", rc, out.String())
	}
	f, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	client := f.Clients[0]
	if client.ModelPicker == nil || !client.ModelPicker.Enabled ||
		len(client.ModelPicker.Models) != 0 {
		t.Fatalf("enabled picker = %+v, want no models", client.ModelPicker)
	}
	if len(client.ModelAliases) == 0 {
		t.Fatal("fixture unexpectedly has no model aliases")
	}
	obj, exists, err := modelPickerObject(readTree(t, env.settings))
	if err != nil || !exists {
		t.Fatalf("Claude settings modelPicker exists=%t err=%v", exists, err)
	}
	rows, err := modelPickerOptions(obj)
	if err != nil || len(rows) != 0 || obj["replaceBuiltInOptions"] != false {
		t.Fatalf("Claude settings modelPicker = %#v, rows=%#v, err=%v", obj, rows, err)
	}
}

func TestPickerEnableAbsentSucceedsWithoutAliases(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	raw, err := os.ReadFile(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	withoutAliases := strings.Replace(string(raw), `    model_aliases:
      claude-baseten-glm-5-2: zai-org/GLM-5.2
      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2-7
`, "", 1)
	if withoutAliases == string(raw) {
		t.Fatal("test fixture model_aliases block changed")
	}
	if err := os.WriteFile(env.cfgPath, []byte(withoutAliases), 0o600); err != nil {
		t.Fatal(err)
	}
	stubPickerVersion(t, "2.1.243")
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if rc := a.on(); rc != 0 {
		t.Fatalf("claude on rc=%d", rc)
	}
	var out bytes.Buffer
	if rc := a.mutatePickerConfig([]string{"enable"}, mutationOptions{JSON: true, OperationID: "no-aliases"}, &out); rc != 0 {
		t.Fatalf("enable rc=%d output=%s", rc, out.String())
	}
	f, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if f.Clients[0].ModelPicker == nil || !f.Clients[0].ModelPicker.Enabled ||
		len(f.Clients[0].ModelPicker.Models) != 0 {
		t.Fatalf("enabled picker = %+v, want no models", f.Clients[0].ModelPicker)
	}
}

func TestPickerAddDryRunReturnsSortedAliasChoices(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	const slug = "zai-org/GLM-5.2"
	if err := config.SetClientModelAliases(env.cfgPath, "claude-code", map[string]string{
		"claude-baseten-a-glm": slug,
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := newClaudeAdapterFromEnv()
	var out bytes.Buffer
	if rc := a.picker([]string{"add", slug, "--dry-run", "--json"}, &out); rc != 1 {
		t.Fatalf("ambiguous preview rc=%d, output=%s", rc, out.String())
	}
	var got claudePickerAliasAmbiguity
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Code != "ambiguous_alias" || len(got.AliasChoices) != 2 ||
		got.AliasChoices[0].Alias != "claude-baseten-a-glm" || got.AliasChoices[1].Alias != "claude-baseten-glm-5-2" {
		t.Fatalf("ambiguity = %+v", got)
	}
	if _, err := os.Stat(mutationJournalDir(env.cfgPath)); !os.IsNotExist(err) {
		t.Fatalf("dry-run created mutation state: %v", err)
	}
}

func TestPickerAddExplicitAliasSelectsOneOfSeveralRoutes(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	const slug = "zai-org/GLM-5.2"
	const selected = "claude-baseten-a-glm"
	if err := config.SetClientModelAliases(env.cfgPath, "claude-code", map[string]string{selected: slug}); err != nil {
		t.Fatal(err)
	}
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	stubPickerVersion(t, "2.1.243")
	a, _ := newClaudeAdapterFromEnv()
	if rc := a.on(); rc != 0 {
		t.Fatalf("claude on rc=%d", rc)
	}
	var out bytes.Buffer
	if rc := a.picker([]string{"add", slug, "--alias", selected, "--json"}, &out); rc != 0 {
		t.Fatalf("explicit alias add rc=%d, output=%s", rc, out.String())
	}
	f, _ := config.Load(env.cfgPath)
	if got := f.Clients[0].ModelPicker.Models; len(got) != 1 || got[0].Alias != selected {
		t.Fatalf("configured rows = %+v", got)
	}
}

func TestPickerRemovalReceiptWarnsForSavedDefaultAndLegacyDiscovery(t *testing.T) {
	env, a := prepareManagedPicker(t, true)
	stubPickerVersion(t, "2.1.243")
	t.Setenv("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "1")
	root := readTree(t, env.settings)
	root["model"] = "claude-baseten-glm-5-2"
	raw, _ := json.Marshal(root)
	if err := os.WriteFile(env.settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if rc := a.mutatePickerConfig(
		[]string{"remove", "claude-baseten-glm-5-2"},
		mutationOptions{JSON: true, OperationID: "remove-saved"},
		&out,
	); rc != 0 {
		t.Fatalf("remove rc=%d output=%s", rc, out.String())
	}
	var receipt mutationResult
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Warnings) != 2 || !strings.Contains(receipt.Warnings[0], "saved Claude default") ||
		!strings.Contains(receipt.Warnings[1], "legacy gateway model discovery") {
		t.Fatalf("warnings = %#v", receipt.Warnings)
	}
}

func TestPickerStatusReportsReplacementAllowlistManagedAndRuntimeAxes(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{
		Enabled: true, Models: []config.ModelPickerModel{{Alias: "claude-baseten-glm-5-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	root := readTree(t, env.settings)
	root["availableModels"] = []any{"claude-native"}
	root["modelPicker"] = map[string]any{"replaceBuiltInOptions": true, "options": []any{}}
	envBlock, _ := settingsEnv(root)
	envBlock["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] = "1"
	raw, _ := json.Marshal(root)
	if err := os.WriteFile(env.settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "0")
	a, _ := newClaudeAdapterFromEnv()
	status, err := a.currentPickerStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Configuration != "enabled" || status.UserFileSync != "blocked" ||
		status.ReplacementMode != "replace" || status.AllowlistPolicy != "possible_conflict" ||
		status.ManagedPolicy != "unverified" || status.RuntimeVerification != "unverified" || status.LegacyDiscoveryEnabled {
		t.Fatalf("status axes = %+v", status)
	}
}

func TestPickerStatusAndDoctorWarnForAllowlistAndSavedUnconfiguredModel(t *testing.T) {
	env, a := prepareManagedPicker(t, true)
	root := readTree(t, env.settings)
	root["availableModels"] = []any{"claude-native"}
	root["model"] = "claude-baseten-kimi-k2-7"
	raw, _ := json.Marshal(root)
	if err := os.WriteFile(env.settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := a.currentPickerStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.UserFileSync != "synced" || status.AllowlistPolicy != "possible_conflict" || !status.SavedModelUnconfigured {
		t.Fatalf("status = %+v", status)
	}
	stubPickerVersion(t, "2.1.243")
	var checks []doctorCheck
	doctorModelPickerCheck(func(section, name, status, finding, fix string, fixArgv ...string) {
		checks = append(checks, doctorCheck{Section: section, Name: name, Status: status, Finding: finding, Fix: fix})
	}, a)
	if len(checks) != 1 || checks[0].Status != docWarn || !strings.Contains(checks[0].Finding, "possible Claude Code allowlist conflict") ||
		!strings.Contains(checks[0].Finding, "managed_policy=unverified") || !strings.Contains(checks[0].Finding, "saved default") {
		t.Fatalf("doctor checks = %+v", checks)
	}
}

func TestPickerDoctorFailsReplacementModeWithExplicitConversionFix(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{
		Enabled: true, Models: []config.ModelPickerModel{{Alias: "claude-baseten-glm-5-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	root := readTree(t, env.settings)
	root["modelPicker"] = map[string]any{"replaceBuiltInOptions": true, "options": []any{}}
	raw, _ := json.Marshal(root)
	if err := os.WriteFile(env.settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stubPickerVersion(t, "2.1.243")
	a, _ := newClaudeAdapterFromEnv()
	var checks []doctorCheck
	doctorModelPickerCheck(func(section, name, status, finding, fix string, fixArgv ...string) {
		checks = append(checks, doctorCheck{Section: section, Name: name, Status: status, Finding: finding, Fix: fix})
	}, a)
	if len(checks) != 1 || checks[0].Status != docFail || !strings.Contains(checks[0].Finding, "replacement mode") ||
		!strings.Contains(checks[0].Fix, "--convert-replacement-mode") {
		t.Fatalf("doctor checks = %+v", checks)
	}
}

func TestPickerDoctorFailsDisabledOutOfSyncWithValidSyncFix(t *testing.T) {
	env, a := prepareManagedPicker(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	var checks []doctorCheck
	doctorModelPickerCheck(func(section, name, status, finding, fix string, fixArgv ...string) {
		checks = append(checks, doctorCheck{Section: section, Name: name, Status: status, Finding: finding, Fix: fix})
	}, a)
	if len(checks) != 1 || checks[0].Status != docFail || !strings.Contains(checks[0].Finding, "not synchronized") ||
		checks[0].Fix != "baseten-switch claude picker sync" {
		t.Fatalf("doctor checks = %+v", checks)
	}
}

func TestPickerVersionProbeResolvesSymlinkAndRejectsUnsafeOutput(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "claude-real")
	link := filepath.Join(dir, "claude")
	if err := os.WriteFile(target, []byte("stub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	oldLook, oldCommand := claudePickerLookPath, claudePickerCommand
	t.Cleanup(func() { claudePickerLookPath, claudePickerCommand = oldLook, oldCommand })
	claudePickerLookPath = func(string) (string, error) { return link, nil }
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	claudePickerCommand = func(_ context.Context, path string, _ ...string) ([]byte, error) {
		if path != resolvedTarget {
			t.Fatalf("version command path=%q, want resolved %q", path, resolvedTarget)
		}
		return []byte("2.1.243"), nil
	}
	if err := checkClaudeModelPickerVersion(); err != nil {
		t.Fatal(err)
	}
	claudePickerLookPath = func(string) (string, error) { return "relative/claude", nil }
	if err := checkClaudeModelPickerVersion(); err == nil {
		t.Fatal("expected relative executable rejection")
	}
	claudePickerLookPath = func(string) (string, error) { return target, nil }
	claudePickerCommand = func(context.Context, string, ...string) ([]byte, error) { return []byte("not-a-version"), nil }
	if err := checkClaudeModelPickerVersion(); err == nil {
		t.Fatal("expected malformed version rejection")
	}
	claudePickerCommand = func(context.Context, string, ...string) ([]byte, error) {
		return bytes.Repeat([]byte("x"), claudeModelPickerVersionOutputLimit+1), nil
	}
	if err := checkClaudeModelPickerVersion(); err == nil {
		t.Fatal("expected oversized version output rejection")
	}
}

func TestPickerVersionProbeReportsMissingExecutable(t *testing.T) {
	oldLook := claudePickerLookPath
	t.Cleanup(func() { claudePickerLookPath = oldLook })
	claudePickerLookPath = func(string) (string, error) { return "", os.ErrNotExist }
	if err := checkClaudeModelPickerVersion(); err == nil || !strings.Contains(err.Error(), claudeModelPickerMinVersion) {
		t.Fatalf("missing executable error = %v", err)
	}
}

func TestPickerVersionProbeTimeout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldLook, oldCommand, oldTimeout := claudePickerLookPath, claudePickerCommand, claudePickerVersionTimeout
	t.Cleanup(func() {
		claudePickerLookPath, claudePickerCommand, claudePickerVersionTimeout = oldLook, oldCommand, oldTimeout
	})
	claudePickerLookPath = func(string) (string, error) { return bin, nil }
	claudePickerCommand = boundedClaudePickerCommand
	claudePickerVersionTimeout = 20 * time.Millisecond
	if err := checkClaudeModelPickerVersion(); err == nil {
		t.Fatal("expected bounded version timeout")
	}
}
