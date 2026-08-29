package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func pickerTestRows(ids ...string) []any {
	rows := make([]any, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, map[string]any{"model": id, "label": id + " label"})
	}
	return rows
}

func TestPickerAddDryRunJSONDoesNotWrite(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeSettings, err := os.ReadFile(env.settings)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rc := a.mutatePickerConfig(
		[]string{"add", "example-org/New_Model-v1"},
		mutationOptions{JSON: true, DryRun: true, OperationID: "preview"},
		&out,
	)
	if rc != 0 {
		t.Fatalf("dry-run rc=%d stderr=%v", rc, a.out)
	}
	var preview claudePickerPreview
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatalf("parse preview %q: %v", out.String(), err)
	}
	if preview.Alias != "claude-baseten-new-model-v1" || preview.Slug != "example-org/New_Model-v1" ||
		preview.Label != "New Model v1 via Baseten" || preview.Description != "Served by Baseten." {
		t.Fatalf("preview = %+v", preview)
	}
	afterConfig, _ := os.ReadFile(env.cfgPath)
	afterSettings, _ := os.ReadFile(env.settings)
	if !bytes.Equal(beforeConfig, afterConfig) || !bytes.Equal(beforeSettings, afterSettings) {
		t.Fatal("dry-run changed config or Claude settings")
	}
}

func TestPickerAddGeneratesAliasAndInstallsRow(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("stub"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldLook, oldCommand := claudePickerLookPath, claudePickerCommand
	t.Cleanup(func() { claudePickerLookPath, claudePickerCommand = oldLook, oldCommand })
	claudePickerLookPath = func(string) (string, error) { return bin, nil }
	claudePickerCommand = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("2.1.243"), nil
	}
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rc := a.mutatePickerConfig(
		[]string{"add", "example-org/New_Model-v1"},
		mutationOptions{JSON: true, OperationID: "add-generated"},
		&out,
	)
	if rc != 0 {
		t.Fatalf("add rc=%d output=%s", rc, out.String())
	}
	var receipt mutationResult
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatalf("parse mutation receipt %q: %v", out.String(), err)
	}
	if !receipt.OK || receipt.Operation != "add_claude_picker_model" || receipt.OperationID != "add-generated" ||
		receipt.DesiredConfigHash == "" || receipt.PreviousDesiredConfigHash == "" || !receipt.ReconciliationRequired {
		t.Fatalf("mutation receipt = %+v", receipt)
	}
	if _, err := readMutationJournal(env.cfgPath, "add-generated"); err != nil {
		t.Fatalf("picker config mutation was not journaled: %v", err)
	}
	f, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	client := f.Clients[0]
	const alias = "claude-baseten-new-model-v1"
	if client.ModelAliases[alias] != "example-org/New_Model-v1" ||
		client.ModelPicker == nil || len(client.ModelPicker.Models) != 1 || client.ModelPicker.Models[0].Alias != alias {
		t.Fatalf("client config = %+v", client)
	}
	settings := readTree(t, env.settings)
	picker := settings["modelPicker"].(map[string]any)
	rows := picker["options"].([]any)
	row := rows[0].(map[string]any)
	if row["model"] != alias || row["label"] != "New Model v1 via Baseten" || row["description"] != "Served by Baseten." {
		t.Fatalf("installed row = %#v", row)
	}
}

func TestPickerSettingsFailureAfterAppliedConfigHasPickerSyncAction(t *testing.T) {
	env := newSubagentTestEnv(t, false)
	if err := config.SetClientModelPicker(env.cfgPath, "claude-code", &config.ModelPicker{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("stub"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldLook, oldCommand := claudePickerLookPath, claudePickerCommand
	t.Cleanup(func() { claudePickerLookPath, claudePickerCommand = oldLook, oldCommand })
	claudePickerLookPath = func(string) (string, error) { return bin, nil }
	claudePickerCommand = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("2.1.242"), nil
	}
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rc := a.mutatePickerConfig(
		[]string{"add", "example-org/New_Model-v1"},
		mutationOptions{JSON: true, OperationID: "settings-failure"},
		&out,
	)
	if rc != 1 {
		t.Fatalf("add rc=%d output=%s", rc, out.String())
	}
	var receipt mutationResult
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatalf("parse mutation receipt %q: %v", out.String(), err)
	}
	if receipt.OK || !receipt.Applied || !receipt.ReconciliationRequired ||
		receipt.ReconciliationAction != "claude_picker_sync" || receipt.Outcome != "settings_sync_pending" ||
		receipt.Error == nil || receipt.Error.Code != "settings_sync_failed" {
		t.Fatalf("mutation receipt = %+v", receipt)
	}
	if _, err := readMutationTerminal(env.cfgPath, "settings-failure"); err != nil {
		t.Fatalf("applied picker config mutation has no terminal receipt: %v", err)
	}
}

func TestCheckClaudeModelPickerVersion(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("stub"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldLook, oldCommand := claudePickerLookPath, claudePickerCommand
	t.Cleanup(func() { claudePickerLookPath, claudePickerCommand = oldLook, oldCommand })
	claudePickerLookPath = func(string) (string, error) { return bin, nil }
	claudePickerCommand = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("2.1.243 (Claude Code)"), nil
	}
	if err := checkClaudeModelPickerVersion(); err != nil {
		t.Fatal(err)
	}
	claudePickerCommand = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("2.1.242 (Claude Code)"), nil
	}
	if err := checkClaudeModelPickerVersion(); err == nil {
		t.Fatal("expected old version error")
	}
}

func TestParsePickerOptions(t *testing.T) {
	opts, args, err := parsePickerOptions([]string{
		"move", "a", "--before", "b", "--json", "--operation-id", "op-1",
		"--if-active-token", "boot:2", "--if-config-hash", "sha256:abc", "--alias", "chosen", "--convert-replacement-mode",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.JSON || opts.OperationID != "op-1" || !opts.hasOperationID || opts.PickerAlias != "chosen" || !opts.ConvertPickerReplacement {
		t.Fatalf("opts = %+v", opts)
	}
	if !opts.hasActiveToken || opts.IfActiveToken != "boot:2" || !opts.hasConfigHash || opts.IfConfigHash != "sha256:abc" {
		t.Fatalf("CAS opts = %+v", opts)
	}
	want := []string{"move", "a", "--before", "b"}
	if len(args) != len(want) {
		t.Fatalf("args = %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v", args)
		}
	}
}

func TestInstallModelPickerPreservesExternalRows(t *testing.T) {
	root := map[string]any{
		"theme": "dark",
		"modelPicker": map[string]any{
			"options": pickerTestRows("external"),
			"future":  "keep",
		},
	}
	bak, changed, err := installModelPicker(root, pickerTestRows("claude-baseten-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || bak == nil || bak.OriginalMissing {
		t.Fatalf("changed=%t backup=%+v", changed, bak)
	}
	obj, _, err := modelPickerObject(root)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := modelPickerOptions(obj)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rowModel(rows[0]) != "external" || rowModel(rows[1]) != "claude-baseten-a" {
		t.Fatalf("rows = %#v", rows)
	}
	if obj["future"] != "keep" || obj["replaceBuiltInOptions"] != false {
		t.Fatalf("object = %#v", obj)
	}
	if _, err := cleanupModelPicker(root, bak, true); err != nil {
		t.Fatal(err)
	}
	obj, _, _ = modelPickerObject(root)
	rows, _ = modelPickerOptions(obj)
	if len(rows) != 1 || rowModel(rows[0]) != "external" || obj["future"] != "keep" {
		t.Fatalf("restored object = %#v", obj)
	}
}

func TestInstallModelPickerRefusesSameIDExternalRow(t *testing.T) {
	root := map[string]any{"modelPicker": map[string]any{"options": pickerTestRows("claude-baseten-a")}}
	if _, _, err := installModelPicker(root, pickerTestRows("claude-baseten-a"), nil); err == nil {
		t.Fatal("expected duplicate identity error")
	}
}

func TestCleanupModelPickerRefusesMovedExactOwnedRow(t *testing.T) {
	root := map[string]any{}
	bak, _, err := installModelPicker(root, pickerTestRows("claude-baseten-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	obj, _, _ := modelPickerObject(root)
	rows, _ := modelPickerOptions(obj)
	rows = append([]any{map[string]any{"model": "external", "label": "keep"}}, rows...)
	obj["options"] = rows
	changed, err := cleanupModelPicker(root, bak, false)
	if err == nil || changed {
		t.Fatalf("cleanup changed=%t err=%v, want moved-row refusal", changed, err)
	}
	obj, exists, _ := modelPickerObject(root)
	if !exists {
		t.Fatal("external picker object was removed")
	}
	rows, _ = modelPickerOptions(obj)
	if len(rows) != 2 || rowModel(rows[0]) != "external" || rowModel(rows[1]) != "claude-baseten-a" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestCleanupModelPickerDriftRemovesAnchoredRowAndPreservesLaterExternalRow(t *testing.T) {
	root := map[string]any{}
	bak, _, err := installModelPicker(root, pickerTestRows("claude-baseten-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	obj, _, _ := modelPickerObject(root)
	rows, _ := modelPickerOptions(obj)
	obj["options"] = append(rows, map[string]any{"model": "external", "label": "keep"})
	changed, err := cleanupModelPicker(root, bak, false)
	if err != nil || !changed {
		t.Fatalf("cleanup changed=%t err=%v", changed, err)
	}
	obj, exists, _ := modelPickerObject(root)
	if !exists {
		t.Fatal("external picker object was removed")
	}
	rows, _ = modelPickerOptions(obj)
	if len(rows) != 1 || rowModel(rows[0]) != "external" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestCleanupModelPickerRefusesEditedOwnedRow(t *testing.T) {
	root := map[string]any{}
	bak, _, err := installModelPicker(root, pickerTestRows("claude-baseten-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	obj, _, _ := modelPickerObject(root)
	obj["options"] = []any{map[string]any{"model": "claude-baseten-a", "label": "user edited"}}
	if _, err := cleanupModelPicker(root, bak, false); err == nil {
		t.Fatal("expected edited row ownership error")
	}
}

func TestClaudePickerBackupRoundTrip(t *testing.T) {
	bak := claudeBackup{
		ConfigPath: "/tmp/settings.json", Values: map[string]string{},
		ModelPicker: &claudeModelPickerBackup{
			Original:                 json.RawMessage(`{"options":[{"model":"external"}]}`),
			WrittenRows:              []json.RawMessage{json.RawMessage(`{"label":"A","model":"claude-baseten-a"}`)},
			WrittenAnchor:            1,
			WrittenPickerFingerprint: "fingerprint",
		},
	}
	raw, err := json.Marshal(bak)
	if err != nil {
		t.Fatal(err)
	}
	var got claudeBackup
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	clone := cloneClaudeBackup(&got)
	clone.ModelPicker.WrittenRows[0][0] = '['
	if got.ModelPicker.WrittenRows[0][0] == '[' {
		t.Fatal("clone shares picker row backing array")
	}
}

func TestClaudePickerRowsProjectSavedPresentation(t *testing.T) {
	p := &config.ModelPicker{Enabled: true, Models: []config.ModelPickerModel{{
		Alias: "claude-baseten-a",
	}}}
	rows := claudePickerRows(p, map[string]string{"claude-baseten-a": "org/GLM-5.2"})
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	row := rows[0].(map[string]any)
	if row["model"] != "claude-baseten-a" || row["label"] != "GLM 5.2 via Baseten" || row["description"] != "Served by Baseten." {
		t.Fatalf("row = %#v", row)
	}
}
