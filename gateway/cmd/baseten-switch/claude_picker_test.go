package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func prepareMillionContextPicker(
	t *testing.T,
) (*subagentTestEnv, *claudeAdapter, func(bool, int64)) {
	t.Helper()
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(
		env.cfgPath,
		"claude-code",
		&config.ModelPicker{
			Enabled: true,
			Models: []config.ModelPickerModel{{
				Alias: "claude-baseten-glm-5-2",
			}},
		},
	); err != nil {
		t.Fatal(err)
	}
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if rc := a.on(); rc != 0 {
		t.Fatalf("claude on rc=%d", rc)
	}
	online := true
	contextTokens := int64(1_048_576)
	oldFetch := fetchRoutingAdminStatus
	t.Cleanup(func() { fetchRoutingAdminStatus = oldFetch })
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		if !online {
			return routingAdminStatus{}, errors.New("router unavailable")
		}
		raw, _, err := readExactConfig(env.cfgPath)
		if err != nil {
			return routingAdminStatus{}, err
		}
		return routingAdminStatus{
			ConfigPath:        env.cfgPath,
			RouterPID:         os.Getpid(),
			RouterBootID:      "picker-context-test",
			ActiveGeneration:  1,
			ActiveConfigHash:  exactConfigHash(raw),
			DesiredConfigHash: exactConfigHash(raw),
			Clients: []fallbackAdminClient{{
				Name: "claude-code",
				ModelPicker: &modelPickerAdminStatus{
					Enabled: true,
					Models: []modelPickerAdminStatusModel{{
						Alias:         "claude-baseten-glm-5-2",
						Slug:          "zai-org/GLM-5.2",
						ContextTokens: contextTokens,
					}},
				},
			}},
		}, nil
	}
	setProjection := func(wantOnline bool, tokens int64) {
		online = wantOnline
		contextTokens = tokens
		if wantOnline {
			if err := os.WriteFile(
				gatewayPidfilePath(),
				[]byte(fmt.Sprintf("%d\n", os.Getpid())),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := os.Remove(gatewayPidfilePath()); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	setProjection(true, contextTokens)
	if err := a.syncModelPicker(false); err != nil {
		t.Fatalf("initial one-million picker sync: %v", err)
	}
	return env, a, setProjection
}

func pickerTestRows(ids ...string) []any {
	rows := make([]any, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, map[string]any{"model": id, "label": id + " label"})
	}
	return rows
}

func stubPickerCatalog(
	t *testing.T,
	models ...claudePickerCatalogModel,
) {
	stubPickerCatalogProjection(t, models, models)
}

func stubPickerCatalogProjection(
	t *testing.T,
	liveModels []claudePickerCatalogModel,
	contextLimits []claudePickerCatalogModel,
) {
	stubPickerCatalogResult(t, "ready", liveModels, contextLimits)
}

func stubPickerCatalogResult(
	t *testing.T,
	state string,
	liveModels []claudePickerCatalogModel,
	contextLimits []claudePickerCatalogModel,
) {
	t.Helper()
	oldFetch := fetchClaudePickerCatalog
	t.Cleanup(func() { fetchClaudePickerCatalog = oldFetch })
	fetchClaudePickerCatalog = func(string) (
		claudePickerCatalogResponse,
		error,
	) {
		return claudePickerCatalogResponse{
			State:         state,
			Models:        liveModels,
			ContextLimits: contextLimits,
		}, nil
	}
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

func TestPickerAddRejectsAbsentLiveKnownSubMinimumContextBeforeAnyWrite(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetClientModelPicker(
		env.cfgPath,
		"claude-code",
		&config.ModelPicker{Enabled: true},
	); err != nil {
		t.Fatal(err)
	}
	stubPickerCatalogProjection(
		t,
		[]claudePickerCatalogModel{},
		[]claudePickerCatalogModel{{
			Slug: "example-org/Small-Model", ContextTokens: 199_999,
		}},
	)
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
	var dryRun bytes.Buffer
	dryRunRC := a.mutatePickerConfig(
		[]string{"add", "example-org/Small-Model"},
		mutationOptions{
			JSON: true, DryRun: true,
			OperationID: "preview-reject-small-picker-model",
		},
		&dryRun,
	)
	var dryRunResult mutationResult
	if dryRunRC != 1 || json.Unmarshal(
		dryRun.Bytes(),
		&dryRunResult,
	) != nil || dryRunResult.Error == nil ||
		dryRunResult.Error.Code != claudePickerContextMinimumErrorCode ||
		dryRunResult.Error.Retryable {
		t.Fatalf("dry-run rc=%d result=%+v output=%s",
			dryRunRC, dryRunResult, dryRun.String())
	}
	var out bytes.Buffer
	rc := a.mutatePickerConfig(
		[]string{"add", "example-org/Small-Model"},
		mutationOptions{
			JSON: true, OperationID: "reject-small-picker-model",
		},
		&out,
	)
	if rc != 1 {
		t.Fatalf("add rc=%d output=%s", rc, out.String())
	}
	var result mutationResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("parse mutation result %q: %v", out.String(), err)
	}
	if result.OK || result.Operation != "add_claude_picker_model" ||
		result.Error == nil ||
		result.Error.Code != claudePickerContextMinimumErrorCode ||
		result.Error.Retryable {
		t.Fatalf("mutation result = %+v", result)
	}
	afterConfig, err := os.ReadFile(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	afterSettings, err := os.ReadFile(env.settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, afterConfig) ||
		!bytes.Equal(beforeSettings, afterSettings) {
		t.Fatal("rejected add changed config or Claude settings")
	}
}

func TestPickerEnableRejectsLastKnownSubMinimumWhenLiveCatalogUnavailableBeforeAnyWrite(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	stubPickerCatalogResult(
		t,
		"error",
		[]claudePickerCatalogModel{},
		[]claudePickerCatalogModel{{
			Slug: "zai-org/GLM-5.2", ContextTokens: 1,
		}},
	)
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
		[]string{"enable"},
		mutationOptions{
			JSON: true, OperationID: "reject-small-picker-enable",
		},
		&out,
	)
	if rc != 1 {
		t.Fatalf("enable rc=%d output=%s", rc, out.String())
	}
	var result mutationResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("parse mutation result %q: %v", out.String(), err)
	}
	if result.OK || result.Operation != "enable_claude_picker" ||
		result.Error == nil ||
		result.Error.Code != claudePickerContextMinimumErrorCode ||
		result.Error.Retryable {
		t.Fatalf("mutation result = %+v", result)
	}
	afterConfig, err := os.ReadFile(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	afterSettings, err := os.ReadFile(env.settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, afterConfig) ||
		!bytes.Equal(beforeSettings, afterSettings) {
		t.Fatal("rejected enable changed config or Claude settings")
	}
}

func TestPickerAddAllowsGenuinelyUnknownContextAndInstallsConservativeRow(t *testing.T) {
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
	stubPickerCatalogProjection(
		t,
		[]claudePickerCatalogModel{{Slug: "example-org/New_Model-v1"}},
		[]claudePickerCatalogModel{},
	)
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
	rows, err := claudePickerRows(
		p,
		map[string]string{"claude-baseten-a": "org/GLM-5.2"},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	row := rows[0].(map[string]any)
	if row["model"] != "claude-baseten-a" || row["label"] != "GLM 5.2 via Baseten" || row["description"] != "Served by Baseten." {
		t.Fatalf("row = %#v", row)
	}
}

func TestClaudePickerRowsProjectExactContextBuckets(t *testing.T) {
	p := &config.ModelPicker{Enabled: true, Models: []config.ModelPickerModel{
		{Alias: "claude-baseten-million"},
		{Alias: "claude-baseten-standard"},
		{Alias: "claude-baseten-unknown"},
	}}
	aliases := map[string]string{
		"claude-baseten-million":  "org/model-million",
		"claude-baseten-standard": "org/model-standard",
		"claude-baseten-unknown":  "org/model-name-claims-1m",
	}
	rows, err := claudePickerRows(p, aliases, map[string]int64{
		"org/model-million":  1_048_576,
		"org/model-standard": 200_000,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	models := make([]string, 0, len(rows))
	for _, raw := range rows {
		row := raw.(map[string]any)
		if _, exists := row["behavesAs"]; exists {
			t.Fatalf("generated row contains behavesAs: %#v", row)
		}
		models = append(models, row["model"].(string))
	}
	want := []string{
		"claude-baseten-million[1m]",
		"claude-baseten-standard",
		"claude-baseten-unknown",
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
}

func TestClaudePickerRowsContextBoundaries(t *testing.T) {
	const alias = "claude-baseten-boundary"
	const slug = "org/model"
	picker := &config.ModelPicker{
		Enabled: true,
		Models:  []config.ModelPickerModel{{Alias: alias}},
	}
	aliases := map[string]string{alias: slug}
	tests := []struct {
		name      string
		context   map[string]int64
		wantModel string
		wantErr   bool
	}{
		{name: "missing", context: map[string]int64{}, wantModel: alias},
		{name: "zero", context: map[string]int64{slug: 0}, wantModel: alias},
		{name: "one", context: map[string]int64{slug: 1}, wantErr: true},
		{name: "199999", context: map[string]int64{slug: 199_999}, wantErr: true},
		{name: "200000", context: map[string]int64{slug: 200_000}, wantModel: alias},
		{name: "999999", context: map[string]int64{slug: 999_999}, wantModel: alias},
		{name: "1000000", context: map[string]int64{slug: 1_000_000}, wantModel: alias + "[1m]"},
		{name: "1048576", context: map[string]int64{slug: 1_048_576}, wantModel: alias + "[1m]"},
		{name: "above one million", context: map[string]int64{slug: 2_000_000}, wantModel: alias + "[1m]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := claudePickerRows(picker, aliases, tc.context, nil)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "below Claude Code's 200000-token model picker minimum") {
					t.Fatalf("error = %v, want actionable unsupported-limit error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			row := rows[0].(map[string]any)
			if got := rowModel(row); got != tc.wantModel {
				t.Fatalf("model = %q, want %q", got, tc.wantModel)
			}
			if _, exists := row["behavesAs"]; exists {
				t.Fatalf("generated row contains behavesAs: %#v", row)
			}
		})
	}
}

func TestClaudePickerRowsRejectDuplicateAliases(t *testing.T) {
	picker := &config.ModelPicker{
		Enabled: true,
		Models: []config.ModelPickerModel{
			{Alias: "claude-baseten-duplicate"},
			{Alias: "claude-baseten-duplicate"},
		},
	}
	_, err := claudePickerRows(
		picker,
		map[string]string{"claude-baseten-duplicate": "org/model"},
		map[string]int64{"org/model": 200_000},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "is duplicated") {
		t.Fatalf("error = %v, want duplicate alias error", err)
	}
}

func TestPickerContextTokensFromAdminUsesExactClientRows(t *testing.T) {
	status := routingAdminStatus{Clients: []fallbackAdminClient{
		{
			Name: "other-client",
			ModelPicker: &modelPickerAdminStatus{Models: []modelPickerAdminStatusModel{{
				Slug: "org/shared", ContextTokens: 200_000,
			}}},
		},
		{
			Name: "claude-code",
			ModelPicker: &modelPickerAdminStatus{Models: []modelPickerAdminStatusModel{
				{Slug: "org/shared", ContextTokens: 1_048_576},
				{Slug: "org/unknown", ContextTokens: 0},
			}},
		},
	}}
	got := pickerContextTokensFromAdmin(status, "claude-code")
	want := map[string]int64{
		"org/shared":  1_048_576,
		"org/unknown": 0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("context tokens = %v, want %v", got, want)
	}
}

func TestOfflinePickerStatusAndSyncPreserveOwnedOneMillionSuffix(t *testing.T) {
	env, a, setProjection := prepareMillionContextPicker(t)
	setProjection(false, 0)

	status, err := a.currentPickerStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.UserFileSync != "synced" {
		t.Fatalf("offline user_file_sync = %q, want synced", status.UserFileSync)
	}
	if err := a.syncModelPicker(false); err != nil {
		t.Fatalf("offline picker sync: %v", err)
	}
	root := readTree(t, env.settings)
	obj, _, err := modelPickerObject(root)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := modelPickerOptions(obj)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowModel(rows[0]); got != "claude-baseten-glm-5-2[1m]" {
		t.Fatalf("offline picker model = %q, want preserved [1m] suffix", got)
	}
}

func TestTrustedUnknownContextDowngradesOwnedOneMillionSuffix(t *testing.T) {
	env, a, setProjection := prepareMillionContextPicker(t)
	root := readTree(t, env.settings)
	root["model"] = "claude-baseten-glm-5-2[1M]"
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	warnings := a.pickerRemovalWarnings("claude-baseten-glm-5-2")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "saved Claude default model") {
		t.Fatalf("decorated saved-default removal warnings = %v", warnings)
	}
	setProjection(true, 0)

	if err := a.syncModelPicker(false); err != nil {
		t.Fatalf("known-unknown picker sync: %v", err)
	}
	root = readTree(t, env.settings)
	obj, _, err := modelPickerObject(root)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := modelPickerOptions(obj)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowModel(rows[0]); got != "claude-baseten-glm-5-2" {
		t.Fatalf("known-unknown picker model = %q, want conservative bare alias", got)
	}
	if got := root["model"]; got != "claude-baseten-glm-5-2[1M]" {
		t.Fatalf("saved default was mutated: %v", got)
	}
	status, err := a.currentPickerStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.UserFileSync != "synced" {
		t.Fatalf("known-unknown user_file_sync = %q, want synced", status.UserFileSync)
	}
	if status.SavedModelUnconfigured || !status.SavedModelContextMismatch {
		t.Fatalf(
			"saved default diagnostics = unconfigured:%t context_mismatch:%t",
			status.SavedModelUnconfigured,
			status.SavedModelContextMismatch,
		)
	}
}

func TestKnownUnsupportedContextBlocksPickerSync(t *testing.T) {
	env, a, setProjection := prepareMillionContextPicker(t)
	setProjection(true, 199_999)

	err := a.syncModelPicker(false)
	if err == nil || !strings.Contains(
		err.Error(),
		"below Claude Code's 200000-token model picker minimum",
	) {
		t.Fatalf("sync error = %v, want actionable unsupported-limit error", err)
	}
	root := readTree(t, env.settings)
	obj, _, objectErr := modelPickerObject(root)
	if objectErr != nil {
		t.Fatal(objectErr)
	}
	rows, rowsErr := modelPickerOptions(obj)
	if rowsErr != nil {
		t.Fatal(rowsErr)
	}
	if got := rowModel(rows[0]); got != "claude-baseten-glm-5-2[1m]" {
		t.Fatalf("failed sync changed installed picker row to %q", got)
	}
	status, statusErr := a.currentPickerStatus()
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if status.UserFileSync != "blocked" || !strings.Contains(
		status.Message,
		"below Claude Code's 200000-token model picker minimum",
	) {
		t.Fatalf("status = %+v, want blocked actionable diagnostic", status)
	}
}
