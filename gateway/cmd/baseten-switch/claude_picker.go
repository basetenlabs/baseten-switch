package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/modelmeta"
	"github.com/basetenlabs/baseten-switch/gateway/internal/safefile"
)

const claudeModelPickerMinVersion = "2.1.243"
const claudeModelPickerVersionOutputLimit = 4096
const claudeStandardContextTokens int64 = 200_000
const claudeOneMillionContextTokens int64 = 1_000_000
const claudeOneMillionContextSuffix = "[1m]"
const claudePickerContextMinimumErrorCode = "context_window_below_claude_picker_minimum"

type claudeModelPickerBackup struct {
	OriginalMissing          bool              `json:"original_missing,omitempty"`
	Original                 json.RawMessage   `json:"original,omitempty"`
	WrittenRows              []json.RawMessage `json:"written_rows,omitempty"`
	WrittenAnchor            int               `json:"written_anchor,omitempty"`
	WrittenPickerFingerprint string            `json:"written_picker_fingerprint,omitempty"`
}

type claudePickerStatus struct {
	Enabled                   bool   `json:"enabled"`
	Configuration             string `json:"configuration"`
	UserFileSync              string `json:"user_file_sync"`
	KnownPolicy               string `json:"known_policy"`
	AllowlistPolicy           string `json:"allowlist_policy"`
	ManagedPolicy             string `json:"managed_policy"`
	ReplacementMode           string `json:"replacement_mode"`
	RuntimeVerification       string `json:"runtime_verification"`
	ConfiguredRows            int    `json:"configured_rows"`
	InstalledRows             int    `json:"installed_rows"`
	SettingsPath              string `json:"settings_path"`
	LegacyDiscoveryEnabled    bool   `json:"legacy_discovery_enabled"`
	SavedModel                string `json:"saved_model,omitempty"`
	SavedModelUnconfigured    bool   `json:"saved_model_unconfigured,omitempty"`
	SavedModelContextMismatch bool   `json:"saved_model_context_mismatch,omitempty"`
	Message                   string `json:"message,omitempty"`
}

type pickerReceiptState struct {
	previousHash  string
	previousToken string
	adminBefore   *routingAdminStatus
	activationErr error
}

type pickerConfigProjection struct {
	raw           []byte
	contextTokens map[string]int64
}

type claudePickerPreview struct {
	Alias       string `json:"alias"`
	Slug        string `json:"slug"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type claudePickerEnablePreview struct {
	Models []claudePickerPreview `json:"models"`
}

type claudePickerAliasAmbiguity struct {
	OK           bool                  `json:"ok"`
	Slug         string                `json:"slug"`
	AliasChoices []claudePickerPreview `json:"alias_choices"`
	Error        mutationError         `json:"error"`
}

type pickerManualResolutionError struct{ message string }

type pickerContextMinimumError struct {
	slug   string
	tokens int64
}

func (e *pickerContextMinimumError) Error() string {
	return fmt.Sprintf(
		"Baseten model %q has a known context limit of %d tokens, below Claude Code's 200000-token model picker minimum; remove it from model_picker or select a model with at least 200000 tokens",
		e.slug,
		e.tokens,
	)
}

func isPickerContextMinimum(err error) bool {
	var target *pickerContextMinimumError
	return errors.As(err, &target)
}

type claudePickerCatalogResponse struct {
	State         string                     `json:"state"`
	Models        []claudePickerCatalogModel `json:"models"`
	ContextLimits []claudePickerCatalogModel `json:"context_limits"`
}

type claudePickerCatalogModel struct {
	Slug          string `json:"slug"`
	ContextTokens int64  `json:"context_tokens"`
}

func (e *pickerManualResolutionError) Error() string { return e.message }

func manualPickerErrorf(format string, args ...any) error {
	return &pickerManualResolutionError{message: fmt.Sprintf(format, args...)}
}

func isPickerManualResolution(err error) bool {
	var target *pickerManualResolutionError
	return errors.As(err, &target)
}

var (
	claudePickerLookPath       = exec.LookPath
	claudePickerCommand        = boundedClaudePickerCommand
	claudePickerVersionTimeout = 2 * time.Second
	fetchClaudePickerCatalog   = func(adminAddr string) (claudePickerCatalogResponse, error) {
		var response claudePickerCatalogResponse
		err := getJSON(adminAddr, "/v1/admin/model-catalog", &response)
		return response, err
	}
)

type boundedOutputBuffer struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *boundedOutputBuffer) Write(p []byte) (int, error) {
	remaining := claudeModelPickerVersionOutputLimit + 1 - b.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	if b.buf.Len() > claudeModelPickerVersionOutputLimit || remaining < len(p) {
		b.overflow = true
	}
	return len(p), nil
}

func boundedClaudePickerCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	var out boundedOutputBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if out.overflow {
		return nil, fmt.Errorf("Claude Code version output exceeds %d bytes", claudeModelPickerVersionOutputLimit)
	}
	return out.buf.Bytes(), err
}

func claudeConfiguredModelPicker(f *config.File, clientName string) *config.ModelPicker {
	for i := range f.Clients {
		if f.Clients[i].Name == clientName && f.Clients[i].ModelPicker != nil {
			p := *f.Clients[i].ModelPicker
			p.Models = append([]config.ModelPickerModel(nil), p.Models...)
			return &p
		}
	}
	return nil
}

func (a *claudeAdapter) modelPickerEnabled() bool {
	return a.modelPicker != nil && a.modelPicker.Enabled
}

func (a *claudeAdapter) reloadModelPicker() error {
	f, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	a.modelPicker = claudeConfiguredModelPicker(f, a.clientName)
	_, _, aliases, err := claudeDoorPort(f)
	if err != nil {
		return err
	}
	a.modelAliases = aliases
	return nil
}

func claudePickerRows(
	picker *config.ModelPicker,
	aliases map[string]string,
	contextTokens map[string]int64,
	preservedOneMillionAliases map[string]bool,
) ([]any, error) {
	if picker == nil || !picker.Enabled {
		return nil, nil
	}
	rows := make([]any, 0, len(picker.Models))
	seenAliases := make(map[string]struct{}, len(picker.Models))
	for _, model := range picker.Models {
		if _, duplicate := seenAliases[model.Alias]; duplicate {
			return nil, fmt.Errorf(
				"model picker alias %q is duplicated",
				model.Alias,
			)
		}
		seenAliases[model.Alias] = struct{}{}
		slug := aliases[model.Alias]
		label := modelmeta.ResolveBaseten(slug).DisplayName + " via Baseten"
		modelID := model.Alias
		// A non-nil map is an authoritative projection. Missing and zero
		// exact-model values are conservative. A nil map means the trusted
		// projection is unavailable, so retain a still-owned prior suffix.
		knownTokens, known := contextTokens[slug]
		if contextTokens != nil && known && knownTokens > 0 &&
			knownTokens < claudeStandardContextTokens {
			return nil, &pickerContextMinimumError{
				slug: slug, tokens: knownTokens,
			}
		}
		useOneMillion := contextTokens != nil && known &&
			knownTokens >= claudeOneMillionContextTokens
		if contextTokens == nil && preservedOneMillionAliases[model.Alias] {
			useOneMillion = true
		}
		if useOneMillion {
			modelID += claudeOneMillionContextSuffix
		}
		row := map[string]any{
			"model": modelID, "label": label, "description": "Served by Baseten.",
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func copyPickerAliases(aliases map[string]string) map[string]string {
	copy := make(map[string]string, len(aliases))
	for alias, slug := range aliases {
		copy[alias] = slug
	}
	return copy
}

func (a *claudeAdapter) preflightPickerContext(
	picker *config.ModelPicker,
	aliases map[string]string,
) error {
	response, err := fetchClaudePickerCatalog(
		envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr),
	)
	if err != nil {
		// Without a trusted local projection, new rows remain in Claude Code's
		// conservative 200K context bucket. Sync preserves any still-owned
		// [1m] row separately when the whole projection is unavailable.
		return nil
	}
	models := response.ContextLimits
	if models == nil {
		if response.State != "ready" {
			return nil
		}
		// Compatibility with a running gateway that predates the complete
		// exact-context projection. Its live account rows are still trusted,
		// but cannot prove anything about configured models absent from them.
		models = response.Models
	}
	contextTokens := make(map[string]int64, len(models))
	for _, model := range models {
		if model.Slug != "" {
			contextTokens[model.Slug] = model.ContextTokens
		}
	}
	_, err = claudePickerRows(picker, aliases, contextTokens, nil)
	return err
}

func normalizeClaudePickerModelID(modelID string) (string, bool) {
	if len(modelID) >= len(claudeOneMillionContextSuffix) &&
		strings.EqualFold(
			modelID[len(modelID)-len(claudeOneMillionContextSuffix):],
			claudeOneMillionContextSuffix,
		) {
		return modelID[:len(modelID)-len(claudeOneMillionContextSuffix)], true
	}
	return modelID, false
}

func desiredPickerHasOneMillionAlias(rows []any, alias string) bool {
	for _, row := range rows {
		base, oneMillion := normalizeClaudePickerModelID(rowModel(row))
		if oneMillion && base == alias {
			return true
		}
	}
	return false
}

func ownedOneMillionPickerAliases(
	root map[string]any,
	backup *claudeModelPickerBackup,
) map[string]bool {
	if backup == nil || backup.WrittenPickerFingerprint == "" {
		return nil
	}
	obj, exists, err := modelPickerObject(root)
	if err != nil || !exists {
		return nil
	}
	rows, err := modelPickerOptions(obj)
	if err != nil ||
		!anchoredRowsEqual(rows, backup.WrittenRows, backup.WrittenAnchor) {
		return nil
	}
	preserved := map[string]bool{}
	for _, raw := range backup.WrittenRows {
		var row map[string]any
		if json.Unmarshal(raw, &row) != nil {
			return nil
		}
		modelID, _ := row["model"].(string)
		if alias, oneMillion := normalizeClaudePickerModelID(modelID); oneMillion {
			if alias != "" {
				preserved[alias] = true
			}
		}
	}
	return preserved
}

func pickerContextTokensFromAdmin(
	status routingAdminStatus,
	clientName string,
) map[string]int64 {
	for _, client := range status.Clients {
		if client.Name != clientName || client.ModelPicker == nil {
			continue
		}
		contextTokens := make(map[string]int64, len(client.ModelPicker.Models))
		for _, model := range client.ModelPicker.Models {
			if model.Slug != "" {
				contextTokens[model.Slug] = model.ContextTokens
			}
		}
		return contextTokens
	}
	return nil
}

func (a *claudeAdapter) activePickerContextTokens() map[string]int64 {
	if state, _ := classifyPidfile(gatewayPidfilePath()); state != pidfileAlive {
		return nil
	}
	raw, _, err := readExactConfig(a.configPath)
	if err != nil {
		return nil
	}
	status, err := fetchRoutingAdminStatus(
		envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr),
	)
	if err != nil ||
		validateMutationAdminStatus(
			status,
			a.configPath,
			exactConfigHash(raw),
		) != nil {
		return nil
	}
	return pickerContextTokensFromAdmin(status, a.clientName)
}

func canonicalJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

func canonicalRows(rows []any) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, len(rows))
	for i := range rows {
		row, err := canonicalJSON(rows[i])
		if err != nil {
			return nil, err
		}
		out[i] = row
	}
	return out, nil
}

func modelPickerObject(root map[string]any) (map[string]any, bool, error) {
	v, ok := root["modelPicker"]
	if !ok {
		return nil, false, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, true, manualPickerErrorf("settings modelPicker is not a JSON object; fix it by hand")
	}
	return obj, true, nil
}

func modelPickerOptions(obj map[string]any) ([]any, error) {
	v, ok := obj["options"]
	if !ok {
		return []any{}, nil
	}
	rows, ok := v.([]any)
	if !ok {
		return nil, manualPickerErrorf("settings modelPicker.options is not an array; fix it by hand")
	}
	for i, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			return nil, manualPickerErrorf("settings modelPicker.options[%d] is not an object", i)
		}
		if _, ok := m["model"].(string); !ok {
			return nil, manualPickerErrorf("settings modelPicker.options[%d].model is not a string", i)
		}
	}
	return rows, nil
}

func rawEqual(a json.RawMessage, v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, a); err != nil {
		return false
	}
	return bytes.Equal(compact.Bytes(), b)
}

func rawRowsEqual(a, b []json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		var decoded any
		dec := json.NewDecoder(bytes.NewReader(b[i]))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err != nil || !rawEqual(a[i], decoded) {
			return false
		}
	}
	return true
}

func anchoredRowsEqual(rows []any, recorded []json.RawMessage, anchor int) bool {
	if anchor < 0 || anchor+len(recorded) > len(rows) {
		return false
	}
	for i := range recorded {
		if !rawEqual(recorded[i], rows[anchor+i]) {
			return false
		}
	}
	return true
}

func (a *claudeAdapter) modelPickerModels() []config.ModelPickerModel {
	if a.modelPicker == nil {
		return nil
	}
	return a.modelPicker.Models
}

func removeRecordedRowsAtAnchor(rows []any, recorded []json.RawMessage, anchor int) ([]any, error) {
	if anchor < 0 || anchor+len(recorded) > len(rows) {
		return nil, manualPickerErrorf("a Switch-managed modelPicker row was moved or removed; resolve settings manually")
	}
	for i, want := range recorded {
		if !rawEqual(want, rows[anchor+i]) {
			return nil, manualPickerErrorf("a Switch-managed modelPicker row was edited, moved, or removed; resolve settings manually")
		}
	}
	result := append([]any(nil), rows[:anchor]...)
	result = append(result, rows[anchor+len(recorded):]...)
	return result, nil
}

func rowModel(row any) string {
	m, _ := row.(map[string]any)
	s, _ := m["model"].(string)
	return s
}

func modelPickerFingerprint(obj map[string]any) (string, error) {
	raw, err := canonicalJSON(obj)
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

func installModelPicker(root map[string]any, desired []any, prior *claudeModelPickerBackup) (*claudeModelPickerBackup, bool, error) {
	return installModelPickerWithOptions(root, desired, prior, false)
}

func installModelPickerWithOptions(root map[string]any, desired []any, prior *claudeModelPickerBackup, allowReplacementConversion bool) (*claudeModelPickerBackup, bool, error) {
	obj, existed, err := modelPickerObject(root)
	if err != nil {
		return nil, false, err
	}
	if !existed {
		obj = map[string]any{}
	}
	if replaceRaw, present := obj["replaceBuiltInOptions"]; present {
		replace, ok := replaceRaw.(bool)
		if !ok {
			return nil, false, manualPickerErrorf("settings modelPicker.replaceBuiltInOptions is not a boolean; fix it by hand")
		}
		if replace && !allowReplacementConversion {
			return nil, false, fmt.Errorf("existing modelPicker uses replaceBuiltInOptions=true; rerun with --convert-replacement-mode to confirm conversion to append mode")
		}
	}
	rows, err := modelPickerOptions(obj)
	if err != nil {
		return nil, false, err
	}
	backup := prior
	anchor := len(rows)
	if backup == nil {
		backup = &claudeModelPickerBackup{OriginalMissing: !existed}
		if existed {
			backup.Original, err = canonicalJSON(obj)
			if err != nil {
				return nil, false, err
			}
		}
	} else {
		if backup.WrittenPickerFingerprint == "" {
			return nil, false, manualPickerErrorf("modelPicker backup predates anchored ownership; resolve settings manually before syncing")
		}
		anchor = backup.WrittenAnchor
		rows, err = removeRecordedRowsAtAnchor(rows, backup.WrittenRows, anchor)
		if err != nil {
			return nil, false, err
		}
	}
	for _, want := range desired {
		id := rowModel(want)
		for _, external := range rows {
			if rowModel(external) == id {
				return nil, false, manualPickerErrorf("modelPicker already has an external row for %q; explicit adoption is required", id)
			}
		}
	}
	updated := make([]any, 0, len(rows)+len(desired))
	updated = append(updated, rows[:anchor]...)
	updated = append(updated, desired...)
	updated = append(updated, rows[anchor:]...)
	rows = updated
	obj["options"] = rows
	obj["replaceBuiltInOptions"] = false
	root["modelPicker"] = obj
	written, err := canonicalRows(desired)
	if err != nil {
		return nil, false, err
	}
	backup.WrittenRows = written
	backup.WrittenAnchor = anchor
	backup.WrittenPickerFingerprint, err = modelPickerFingerprint(obj)
	if err != nil {
		return nil, false, err
	}
	return backup, true, nil
}

func cleanupModelPicker(root map[string]any, picker *claudeModelPickerBackup, _ bool) (bool, error) {
	if picker == nil {
		return false, nil
	}
	obj, existed, err := modelPickerObject(root)
	if err != nil || !existed {
		return false, err
	}
	if picker.WrittenPickerFingerprint == "" {
		return false, manualPickerErrorf("modelPicker backup predates anchored ownership; resolve settings manually")
	}
	currentFingerprint, err := modelPickerFingerprint(obj)
	if err != nil {
		return false, err
	}
	if currentFingerprint == picker.WrittenPickerFingerprint {
		if picker.OriginalMissing {
			delete(root, "modelPicker")
			return true, nil
		}
		var original any
		dec := json.NewDecoder(bytes.NewReader(picker.Original))
		dec.UseNumber()
		if err := dec.Decode(&original); err != nil {
			return false, fmt.Errorf("parse backed-up modelPicker: %w", err)
		}
		root["modelPicker"] = original
		return true, nil
	}
	rows, err := modelPickerOptions(obj)
	if err != nil {
		return false, err
	}
	rows, err = removeRecordedRowsAtAnchor(rows, picker.WrittenRows, picker.WrittenAnchor)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 && picker.OriginalMissing && len(obj) == 2 && obj["replaceBuiltInOptions"] == false {
		delete(root, "modelPicker")
	} else {
		obj["options"] = rows
	}
	return true, nil
}

type pickerBackupPreimage struct {
	existed bool
	raw     []byte
	mode    os.FileMode
}

func capturePickerBackupPreimage(path string) (pickerBackupPreimage, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pickerBackupPreimage{}, nil
	}
	if err != nil {
		return pickerBackupPreimage{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return pickerBackupPreimage{}, err
	}
	return pickerBackupPreimage{existed: true, raw: raw, mode: info.Mode()}, nil
}

func restorePickerBackupPreimage(path string, pre pickerBackupPreimage) error {
	if !pre.existed {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return atomicWriteFile(path, pre.raw, pre.mode)
}

func (a *claudeAdapter) syncModelPicker(requireVersion bool, allowReplacementConversion ...bool) error {
	projection, err := func() (pickerConfigProjection, error) {
		configLock, lockErr := acquireConfigMutationLock(a.configPath)
		if lockErr != nil {
			return pickerConfigProjection{}, fmt.Errorf("lock picker config projection: %w", lockErr)
		}
		defer configLock.close()
		if recoverErr := recoverInterruptedExactConfigCommit(a.configPath); recoverErr != nil {
			return pickerConfigProjection{}, fmt.Errorf("recover picker config projection: %w", recoverErr)
		}
		raw, _, readErr := readExactConfig(a.configPath)
		if readErr != nil {
			return pickerConfigProjection{}, readErr
		}
		if reloadErr := a.reloadModelPicker(); reloadErr != nil {
			return pickerConfigProjection{}, reloadErr
		}
		if current, _, readErr := readExactConfig(a.configPath); readErr != nil || !bytes.Equal(current, raw) {
			return pickerConfigProjection{}, fmt.Errorf("picker config changed while loading its settings projection; retry sync")
		}
		result := pickerConfigProjection{raw: raw}
		state, _ := classifyPidfile(gatewayPidfilePath())
		admin, adminErr := fetchRoutingAdminStatus(
			envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr),
		)
		if adminErr != nil && state == pidfileAlive {
			return pickerConfigProjection{}, fmt.Errorf("verify active picker config: %w", adminErr)
		}
		if adminErr == nil {
			activeErr := validateMutationAdminStatus(
				admin,
				a.configPath,
				exactConfigHash(raw),
			)
			if activeErr != nil && state == pidfileAlive {
				return pickerConfigProjection{}, fmt.Errorf("picker config is not active: %w", activeErr)
			}
			if activeErr == nil {
				result.contextTokens = pickerContextTokensFromAdmin(
					admin,
					a.clientName,
				)
			}
		}
		return result, nil
	}()
	if err != nil {
		return err
	}
	lock, err := a.acquireSettingsMutationLock()
	if err != nil {
		return err
	}
	defer lock.close()
	if current, _, readErr := readExactConfig(a.configPath); readErr != nil || !bytes.Equal(current, projection.raw) {
		return fmt.Errorf("picker config changed before settings lock acquisition; retry sync")
	}
	if a.modelPickerEnabled() && requireVersion {
		if err := checkClaudeModelPickerVersion(); err != nil {
			return err
		}
	}
	root, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		return err
	}
	if !a.modelPickerEnabled() {
		return a.removeModelPickerRowsLocked(root, snap)
	}
	env, err := settingsEnv(root)
	if err != nil {
		return err
	}
	baseManaged, _ := a.claudeOnState(env)
	if !baseManaged {
		return fmt.Errorf("Claude wiring is off; run 'baseten-switch claude on'")
	}
	backupPreimage, err := capturePickerBackupPreimage(a.backupPath)
	if err != nil {
		return fmt.Errorf("capture modelPicker backup: %w", err)
	}
	bak, err := loadClaudeBackup(a.backupPath)
	if err != nil {
		return err
	}
	if bak != nil && !claudeBackupTargetSafe(bak, snap) {
		return fmt.Errorf("settings target changed since backup; refusing modelPicker write")
	}
	if bak == nil {
		bak = &claudeBackup{
			ConfigPath: a.settingsPath, Values: map[string]string{}, EnvExisted: env != nil,
			Existed: snap.Exists, WrittenHash: sha256Hex(snap.Data),
		}
		recordClaudeBackupFile(bak, snap)
	}
	desired, err := claudePickerRows(
		a.modelPicker,
		a.modelAliases,
		projection.contextTokens,
		ownedOneMillionPickerAliases(root, bak.ModelPicker),
	)
	if err != nil {
		return fmt.Errorf("project Claude model picker: %w", err)
	}
	allowConversion := len(allowReplacementConversion) > 0 && allowReplacementConversion[0]
	pickerBackup, changed, err := installModelPickerWithOptions(root, desired, bak.ModelPicker, allowConversion)
	if err != nil {
		return err
	}
	bak.ModelPicker = pickerBackup
	projectionCurrent := func() bool {
		current, _, readErr := readExactConfig(a.configPath)
		return readErr == nil && bytes.Equal(current, projection.raw)
	}
	if !projectionCurrent() {
		return fmt.Errorf("picker config changed before staging its settings projection; retry sync")
	}
	if err := saveClaudeBackup(a.backupPath, bak); err != nil {
		return fmt.Errorf("stage modelPicker backup: %w", err)
	}
	if !changed {
		return nil
	}
	if claudeBeforeSettingsMutation != nil {
		claudeBeforeSettingsMutation()
	}
	if !projectionCurrent() {
		if restoreErr := restorePickerBackupPreimage(a.backupPath, backupPreimage); restoreErr != nil {
			return fmt.Errorf("picker config changed before settings commit; restore exact prior modelPicker backup: %w", restoreErr)
		}
		return fmt.Errorf("picker config changed before settings commit; backup restored, retry sync")
	}
	raw, committed, err := writeClaudeSettings(snap, root)
	if err != nil {
		if !safefile.CommitApplied(err) {
			if restoreErr := restorePickerBackupPreimage(a.backupPath, backupPreimage); restoreErr != nil {
				return fmt.Errorf("write Claude settings: %v; restore exact prior modelPicker backup: %w", err, restoreErr)
			}
		}
		return err
	}
	bak.WrittenHash = sha256Hex(raw)
	recordClaudeBackupFile(bak, committed)
	if err := saveClaudeBackup(a.backupPath, bak); err != nil {
		return fmt.Errorf("refresh modelPicker backup: %w", err)
	}
	return nil
}

func checkClaudeModelPickerVersion() error {
	path, err := claudePickerLookPath("claude")
	if err != nil {
		return fmt.Errorf("Claude Code %s+ is required: %w", claudeModelPickerMinVersion, err)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("Claude Code version probe resolved a relative executable %q", path)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve Claude Code executable %q: %w", path, err)
	}
	info, err := os.Stat(resolvedPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("Claude Code executable %q is not an executable regular file", resolvedPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudePickerVersionTimeout)
	defer cancel()
	out, err := claudePickerCommand(ctx, resolvedPath, "--version")
	if err != nil {
		return fmt.Errorf("probe Claude Code version: %w", err)
	}
	if len(out) > claudeModelPickerVersionOutputLimit {
		return fmt.Errorf("Claude Code version output exceeds %d bytes", claudeModelPickerVersionOutputLimit)
	}
	re := regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`)
	m := re.FindSubmatch(out)
	if len(m) != 4 {
		return fmt.Errorf("could not parse Claude Code version from bounded output")
	}
	want := []int{2, 1, 243}
	got := make([]int, 3)
	for i := range got {
		got[i], _ = strconv.Atoi(string(m[i+1]))
	}
	for i := range got {
		if got[i] > want[i] {
			return nil
		}
		if got[i] < want[i] {
			return fmt.Errorf("Claude Code %s+ is required; found %d.%d.%d", claudeModelPickerMinVersion, got[0], got[1], got[2])
		}
	}
	return nil
}

func (a *claudeAdapter) picker(args []string, stdout io.Writer) int {
	if len(args) == 0 {
		args = []string{"status"}
	}
	opts, cleanArgs, err := parsePickerOptions(args)
	if err != nil {
		fmt.Fprintf(a.out, "claude picker: %v\n", err)
		return 2
	}
	args = cleanArgs
	switch args[0] {
	case "status":
		return a.pickerStatus(stdout, false)
	case "list":
		return a.pickerStatus(stdout, true)
	case "sync":
		receiptState, casErr := a.checkPickerCAS(opts)
		if casErr != nil {
			if opts.JSON {
				emitPickerMutation(stdout, opts, "sync_claude_picker", "", a, receiptState, false, false, "config_conflict", casErr.Error())
				return 1
			}
			fmt.Fprintf(a.out, "claude picker sync: %v\n", casErr)
			return 1
		}
		if err := a.syncModelPicker(true, opts.ConvertPickerReplacement); err != nil {
			if opts.JSON {
				outcome := "settings_sync_failed"
				if isPickerManualResolution(err) {
					outcome = "manual_resolution_required"
				}
				emitPickerMutation(stdout, opts, "sync_claude_picker", "", a, receiptState, false, true, outcome, err.Error())
				return 1
			}
			fmt.Fprintf(a.out, "claude picker sync: %v\n", err)
			return 1
		}
		if opts.JSON {
			emitPickerMutation(stdout, opts, "sync_claude_picker", "", a, receiptState, true, false, "applied", "")
			return 0
		}
		fmt.Fprintln(stdout, "claude picker: synced")
		return 0
	case "enable", "disable", "add", "remove", "move":
		return a.mutatePickerConfig(args, opts, stdout)
	default:
		fmt.Fprintf(a.out, "unknown claude picker subcommand %q\n", args[0])
		return 2
	}
}

func (a *claudeAdapter) checkPickerCAS(opts mutationOptions) (*pickerReceiptState, error) {
	raw, _, err := readExactConfig(a.configPath)
	if err != nil {
		return nil, err
	}
	state := &pickerReceiptState{previousHash: exactConfigHash(raw)}
	if admin, adminErr := fetchRoutingAdminStatus(envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)); adminErr == nil {
		state.previousToken = admin.activeToken()
		state.adminBefore = &admin
	}
	if opts.hasConfigHash && opts.IfConfigHash != state.previousHash {
		return state, fmt.Errorf("desired config changed: expected %s, found %s", opts.IfConfigHash, state.previousHash)
	}
	if opts.hasActiveToken && opts.IfActiveToken != state.previousToken {
		return state, fmt.Errorf("active router changed: expected %s, found %s", opts.IfActiveToken, state.previousToken)
	}
	return state, nil
}

func (a *claudeAdapter) currentPickerStatus() (claudePickerStatus, error) {
	if err := a.reloadModelPicker(); err != nil {
		return claudePickerStatus{}, err
	}
	status := claudePickerStatus{
		Enabled: a.modelPickerEnabled(), UserFileSync: "out_of_sync", KnownPolicy: "no_known_conflict",
		AllowlistPolicy: "no_known_conflict", ManagedPolicy: "unverified", ReplacementMode: "append",
		RuntimeVerification: "unverified", SettingsPath: a.settingsPath,
	}
	if status.Enabled {
		status.Configuration = "enabled"
	} else {
		status.Configuration = "disabled"
	}
	if a.modelPicker != nil {
		status.ConfiguredRows = len(a.modelPicker.Models)
	}
	root, _, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		return status, err
	}
	if saved, ok := root["model"].(string); ok {
		status.SavedModel = saved
		savedAlias, _ := normalizeClaudePickerModelID(saved)
		if _, aliasExists := a.modelAliases[savedAlias]; aliasExists && pickerModelIndex(a.modelPickerModels(), savedAlias) < 0 {
			status.SavedModelUnconfigured = true
		}
	}
	status.LegacyDiscoveryEnabled = effectiveLegacyDiscovery(root)
	if rawAllowlist, present := root["availableModels"]; present && status.Enabled {
		values, ok := rawAllowlist.([]any)
		if !ok {
			status.AllowlistPolicy = "blocked"
			status.KnownPolicy = status.AllowlistPolicy
			status.Message = "availableModels is not an array"
		} else {
			allowed := make(map[string]bool, len(values))
			for _, entry := range values {
				if id, ok := entry.(string); ok {
					allowed[id] = true
				}
			}
			for _, row := range a.modelPicker.Models {
				if !allowed[row.Alias] {
					status.AllowlistPolicy = "possible_conflict"
					status.KnownPolicy = "possible_allowlist_conflict"
					break
				}
			}
		}
	}
	obj, exists, err := modelPickerObject(root)
	if err != nil {
		status.Message = err.Error()
		return status, nil
	}
	if exists {
		if raw, present := obj["replaceBuiltInOptions"]; present {
			replace, ok := raw.(bool)
			if !ok {
				status.ReplacementMode = "blocked"
				status.UserFileSync = "blocked"
				status.Message = "modelPicker.replaceBuiltInOptions is not a boolean"
				return status, nil
			}
			if replace {
				status.ReplacementMode = "replace"
				if status.Enabled {
					status.UserFileSync = "blocked"
					status.Message = "modelPicker replacement mode requires explicit conversion"
				}
			}
		}
		rows, rowErr := modelPickerOptions(obj)
		if rowErr != nil {
			status.Message = rowErr.Error()
			return status, nil
		}
		status.InstalledRows = len(rows)
		bak, backupErr := loadClaudeBackup(a.backupPath)
		if backupErr != nil {
			status.UserFileSync = "blocked"
			status.Message = backupErr.Error()
			return status, nil
		}
		if status.Enabled && status.UserFileSync != "blocked" && bak != nil && bak.ModelPicker != nil && bak.ModelPicker.WrittenPickerFingerprint != "" {
			contextTokens := a.activePickerContextTokens()
			desired, projectionErr := claudePickerRows(
				a.modelPicker,
				a.modelAliases,
				contextTokens,
				ownedOneMillionPickerAliases(root, bak.ModelPicker),
			)
			if projectionErr != nil {
				status.UserFileSync = "blocked"
				status.Message = projectionErr.Error()
				return status, nil
			}
			savedAlias, savedOneMillion := normalizeClaudePickerModelID(
				status.SavedModel,
			)
			if savedOneMillion &&
				pickerModelIndex(a.modelPickerModels(), savedAlias) >= 0 &&
				!desiredPickerHasOneMillionAlias(desired, savedAlias) {
				status.SavedModelContextMismatch = true
			}
			want, _ := canonicalRows(desired)
			if anchoredRowsEqual(rows, want, bak.ModelPicker.WrittenAnchor) && rawRowsEqual(want, bak.ModelPicker.WrittenRows) {
				status.UserFileSync = "synced"
			}
		}
		if !status.Enabled {
			switch {
			case bak == nil || bak.ModelPicker == nil:
				status.UserFileSync = "synced"
			case bak.ModelPicker.WrittenPickerFingerprint == "":
				status.UserFileSync = "blocked"
				status.Message = "modelPicker backup predates anchored ownership; manual resolution is required"
			case anchoredRowsEqual(rows, bak.ModelPicker.WrittenRows, bak.ModelPicker.WrittenAnchor):
				status.UserFileSync = "out_of_sync"
			default:
				status.UserFileSync = "blocked"
				status.Message = "Switch-managed modelPicker rows were edited, moved, or removed; manual resolution is required"
			}
		}
	}
	if !status.Enabled && !exists {
		status.UserFileSync = "synced"
	}
	return status, nil
}

func (a *claudeAdapter) pickerStatus(stdout io.Writer, list bool) int {
	status, err := a.currentPickerStatus()
	if err != nil {
		fmt.Fprintf(a.out, "claude picker status: %v\n", err)
		return 1
	}
	if list {
		if a.modelPicker == nil || len(a.modelPicker.Models) == 0 {
			fmt.Fprintln(stdout, "no configured Claude picker rows")
			return 0
		}
		for i, row := range a.modelPicker.Models {
			slug := a.modelAliases[row.Alias]
			fmt.Fprintf(stdout, "%d\t%s\t%s\tServed by Baseten.\n", i+1, row.Alias, modelmeta.ResolveBaseten(slug).DisplayName+" via Baseten")
		}
		return 0
	}
	b, _ := json.Marshal(status)
	fmt.Fprintln(stdout, string(b))
	if status.Enabled && status.UserFileSync != "synced" {
		return statusExitOff
	}
	if !status.Enabled && status.UserFileSync != "synced" {
		return statusExitOff
	}
	return 0
}

func effectiveLegacyDiscovery(root map[string]any) bool {
	if processDiscovery, set := os.LookupEnv("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"); set {
		return processDiscovery == "1"
	}
	if env, err := settingsEnv(root); err == nil {
		if value, ok := envString(env, "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"); ok {
			return value == "1"
		}
	}
	return false
}

func (a *claudeAdapter) pickerRemovalWarnings(alias string) []string {
	root, _, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		return nil
	}
	var warnings []string
	if saved, ok := root["model"].(string); ok {
		savedAlias, _ := normalizeClaudePickerModelID(saved)
		if savedAlias == alias {
			warnings = append(warnings, fmt.Sprintf("saved Claude default model %q still points at the removed alias; select a new default in /model", alias))
		}
	}
	if effectiveLegacyDiscovery(root) {
		warnings = append(warnings, fmt.Sprintf("legacy gateway model discovery is enabled, so removing %q from modelPicker may not hide it from /model", alias))
	}
	return warnings
}

func (a *claudeAdapter) mutatePickerConfig(args []string, opts mutationOptions, stdout io.Writer) int {
	if err := a.reloadModelPicker(); err != nil {
		fmt.Fprintf(a.out, "claude picker %s: %v\n", args[0], err)
		return 1
	}
	picker := a.modelPicker
	if picker == nil {
		picker = &config.ModelPicker{}
	}
	p := *picker
	p.Models = append([]config.ModelPickerModel(nil), picker.Models...)
	verb := args[0]
	operation := map[string]string{
		"enable": "enable_claude_picker", "disable": "disable_claude_picker",
		"add": "add_claude_picker_model", "remove": "remove_claude_picker_model",
		"move": "move_claude_picker_model",
	}[verb]
	requestedTarget := ""
	aliasesToAdd := map[string]string{}
	if len(args) > 1 {
		requestedTarget = args[1]
	}
	if opts.DryRun && verb == "enable" {
		if len(args) != 1 || !opts.JSON {
			fmt.Fprintln(a.out, "claude picker enable --dry-run requires --json")
			return 2
		}
		proposed := p
		proposed.Enabled = true
		if err := a.preflightPickerContext(
			&proposed,
			copyPickerAliases(a.modelAliases),
		); err != nil {
			code := "config_validation_failed"
			if isPickerContextMinimum(err) {
				code = claudePickerContextMinimumErrorCode
			}
			return failMutation(opts, stdout, mutationResult{
				Operation: operation, Client: a.clientName,
				Key: "model_picker", ConfigPath: a.configPath,
				Requested: true,
			}, code, err.Error(), false, 1)
		}
		preview := claudePickerEnablePreview{Models: []claudePickerPreview{}}
		for _, model := range proposed.Models {
			slug := a.modelAliases[model.Alias]
			preview.Models = append(
				preview.Models,
				pickerPreview(model.Alias, slug),
			)
		}
		_ = json.NewEncoder(stdout).Encode(preview)
		return 0
	}
	if verb == "add" && opts.DryRun {
		if len(args) != 2 || !opts.JSON {
			fmt.Fprintln(a.out, "claude picker add --dry-run requires one slug and --json")
			return 2
		}
		if opts.PickerAlias == "" {
			aliases := aliasesForSlug(args[1], a.modelAliases)
			if len(aliases) > 1 {
				ambiguity := claudePickerAliasAmbiguity{
					OK: false, Slug: args[1], AliasChoices: make([]claudePickerPreview, 0, len(aliases)),
					Error: mutationError{Code: "ambiguous_alias", Message: "multiple configured aliases route to this slug; select one", Retryable: false},
				}
				for _, alias := range aliases {
					ambiguity.AliasChoices = append(ambiguity.AliasChoices, pickerPreview(alias, args[1]))
				}
				_ = json.NewEncoder(stdout).Encode(ambiguity)
				return 1
			}
		}
		preview, err := buildPickerPreview(args[1], a.modelAliases, opts.PickerAlias)
		if err != nil {
			fmt.Fprintf(a.out, "claude picker add preview: %v\n", err)
			return 1
		}
		proposed := p
		proposed.Enabled = true
		proposed.Models = append(
			proposed.Models,
			config.ModelPickerModel{Alias: preview.Alias},
		)
		proposedAliases := copyPickerAliases(a.modelAliases)
		proposedAliases[preview.Alias] = preview.Slug
		if err := a.preflightPickerContext(
			&proposed,
			proposedAliases,
		); err != nil {
			code := "config_validation_failed"
			if isPickerContextMinimum(err) {
				code = claudePickerContextMinimumErrorCode
			}
			return failMutation(opts, stdout, mutationResult{
				Operation: operation, RequestedTarget: args[1],
				Client: a.clientName, Key: "model_picker",
				ConfigPath: a.configPath, Requested: true,
			}, code, err.Error(), false, 1)
		}
		_ = json.NewEncoder(stdout).Encode(preview)
		return 0
	}
	if opts.DryRun {
		fmt.Fprintln(a.out, "--dry-run is supported only by picker enable and add")
		return 2
	}
	switch verb {
	case "enable":
		if len(args) != 1 {
			return pickerUsage(a.out)
		}
		p.Enabled = true
	case "disable":
		if len(args) != 1 {
			return pickerUsage(a.out)
		}
		p.Enabled = false
	case "add":
		if len(args) != 2 || !p.Enabled {
			return pickerUsage(a.out)
		}
		preview, err := buildPickerPreview(args[1], a.modelAliases, opts.PickerAlias)
		if err != nil {
			fmt.Fprintf(a.out, "claude picker add: %v\n", err)
			return 1
		}
		alias := preview.Alias
		for _, row := range p.Models {
			if row.Alias == alias {
				fmt.Fprintf(a.out, "claude picker add: %s is already configured\n", alias)
				return 1
			}
		}
		if _, exists := a.modelAliases[alias]; !exists {
			aliasesToAdd[alias] = preview.Slug
		}
		p.Models = append(p.Models, config.ModelPickerModel{Alias: alias})
	case "remove":
		if len(args) != 2 || !p.Enabled {
			return pickerUsage(a.out)
		}
		idx := pickerModelIndex(p.Models, args[1])
		if idx < 0 {
			fmt.Fprintf(a.out, "claude picker remove: alias %q is not configured\n", args[1])
			return 1
		}
		p.Models = append(p.Models[:idx], p.Models[idx+1:]...)
	case "move":
		if len(args) != 4 || args[2] != "--before" || !p.Enabled {
			return pickerUsage(a.out)
		}
		from, before := pickerModelIndex(p.Models, args[1]), pickerModelIndex(p.Models, args[3])
		if from < 0 || before < 0 {
			fmt.Fprintln(a.out, "claude picker move: both aliases must be configured")
			return 1
		}
		row := p.Models[from]
		p.Models = append(p.Models[:from], p.Models[from+1:]...)
		before = pickerModelIndex(p.Models, args[3])
		p.Models = append(p.Models, config.ModelPickerModel{})
		copy(p.Models[before+1:], p.Models[before:])
		p.Models[before] = row
		requestedTarget = args[1] + " before " + args[3]
	}
	if verb == "add" && opts.PickerAlias != "" {
		requestedTarget += " via " + opts.PickerAlias
	}
	if (verb == "enable" || verb == "add" || verb == "move") && p.Enabled {
		proposedAliases := copyPickerAliases(a.modelAliases)
		for alias, slug := range aliasesToAdd {
			proposedAliases[alias] = slug
		}
		if err := a.preflightPickerContext(&p, proposedAliases); err != nil {
			code := "config_validation_failed"
			if isPickerContextMinimum(err) {
				code = claudePickerContextMinimumErrorCode
			}
			return failMutation(opts, stdout, mutationResult{
				Operation: operation, RequestedTarget: requestedTarget,
				Client: a.clientName, Key: "model_picker",
				ConfigPath: a.configPath, Requested: true,
			}, code, err.Error(), false, 1)
		}
	}

	lock, err := acquireConfigMutationLock(a.configPath)
	if err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation: operation, RequestedTarget: requestedTarget, Client: a.clientName,
			Key: "model_picker", ConfigPath: a.configPath,
		}, "mutation_locked", err.Error(), true, 1)
	}
	if err := recoverInterruptedExactConfigCommit(a.configPath); err != nil {
		lock.close()
		return failMutation(opts, stdout, mutationResult{
			Operation: operation, RequestedTarget: requestedTarget, Client: a.clientName,
			Key: "model_picker", ConfigPath: a.configPath,
		}, "commit_recovery_failed", err.Error(), true, 1)
	}
	journalOpts := opts
	journalOpts.JSON = true
	var journalOutput bytes.Buffer
	rc := a.runClaudeJournaledMutationLocked(journalOpts, &journalOutput, journaledMutationSpec{
		Operation:       operation,
		RequestedTarget: requestedTarget,
		Client:          a.clientName,
		Key:             "model_picker",
		Apply: func(path string) error {
			if len(aliasesToAdd) > 0 {
				if err := config.SetClientModelAliases(path, a.clientName, aliasesToAdd); err != nil {
					return err
				}
			}
			return config.SetClientModelPicker(path, a.clientName, &p)
		},
	})
	lock.close()
	var result mutationResult
	if err := json.Unmarshal(bytes.TrimSpace(journalOutput.Bytes()), &result); err != nil {
		result = mutationResult{
			OperationID: opts.OperationID, Operation: operation,
			RequestedTarget: requestedTarget, Client: a.clientName, Key: "model_picker",
			ConfigPath: a.configPath,
		}
		result.Error = &mutationError{Code: "journal_receipt_invalid", Message: "could not decode the config mutation receipt", Retryable: true}
		result.Outcome = "journal_receipt_invalid"
		rc = 1
	}
	if rc != 0 {
		return emitPickerJournalResult(opts, stdout, a.out, result, rc)
	}
	if verb == "remove" {
		result.Warnings = a.pickerRemovalWarnings(args[1])
	}
	if err := a.reloadModelPicker(); err != nil {
		return emitPickerSettingsFailure(opts, stdout, a.out, result, verb, err)
	}
	if verb == "disable" {
		if err := a.syncModelPicker(false); err != nil {
			return emitPickerSettingsFailure(opts, stdout, a.out, result, verb, err)
		}
	} else if err := a.syncModelPicker(true, opts.ConvertPickerReplacement); err != nil {
		return emitPickerSettingsFailure(opts, stdout, a.out, result, verb, err)
	}
	return emitPickerJournalResult(opts, stdout, a.out, result, 0)
}

func emitPickerSettingsFailure(opts mutationOptions, stdout, humanOut io.Writer, result mutationResult, verb string, err error) int {
	result.OK = false
	result.ReconciliationRequired = true
	if isPickerManualResolution(err) {
		result.ReconciliationAction = "manual_claude_settings_resolution"
		if result.Outcome != mutationOutcomeApplied && result.Outcome != mutationOutcomeUnchanged {
			result.ReconciliationAction = "mutation_reconcile_then_manual_claude_settings_resolution"
		}
		result.Outcome = "manual_resolution_required"
		result.Error = &mutationError{Code: "manual_resolution_required", Message: err.Error(), Retryable: false}
		if opts.JSON {
			emitMutationResult(stdout, result)
		} else if result.ReconciliationAction == "mutation_reconcile_then_manual_claude_settings_resolution" {
			fmt.Fprintf(humanOut, "claude picker %s: config activation and manual Claude settings resolution are pending: %v; after the router starts, run 'baseten-switch mutation reconcile %s'\n", verb, err, result.OperationID)
		} else {
			fmt.Fprintf(humanOut, "claude picker %s: config updated, but Claude settings need manual resolution: %v\n", verb, err)
		}
		return 1
	}
	result.ReconciliationAction = "claude_picker_sync"
	if result.Outcome != mutationOutcomeApplied && result.Outcome != mutationOutcomeUnchanged {
		result.ReconciliationAction = "mutation_reconcile_then_claude_picker_sync"
	}
	result.Outcome = "settings_sync_pending"
	result.Error = &mutationError{Code: "settings_sync_failed", Message: err.Error(), Retryable: true}
	if opts.JSON {
		emitMutationResult(stdout, result)
	} else if result.ReconciliationAction == "mutation_reconcile_then_claude_picker_sync" {
		fmt.Fprintf(humanOut, "claude picker %s: config updated but router activation and Claude settings reconciliation are pending: %v; after the router starts, run 'baseten-switch mutation reconcile %s', then 'baseten-switch claude picker sync'\n", verb, err, result.OperationID)
	} else {
		fmt.Fprintf(humanOut, "claude picker %s: config updated; Claude settings reconciliation pending: %v; run 'baseten-switch claude picker sync'\n", verb, err)
	}
	return 1
}

func emitPickerJournalResult(opts mutationOptions, stdout, humanOut io.Writer, result mutationResult, rc int) int {
	if opts.JSON {
		emitMutationResult(stdout, result)
		return rc
	}
	if rc != 0 {
		if result.Error != nil {
			fmt.Fprintln(humanOut, result.Error.Message)
		} else {
			fmt.Fprintln(humanOut, "Claude picker config mutation failed")
		}
		return rc
	}
	if result.ReconciliationRequired && !result.Applied {
		fmt.Fprintf(humanOut, "Claude picker config and settings updated; router activation is pending. After the router starts, run 'baseten-switch mutation reconcile %s'.\n", result.OperationID)
		return 0
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(humanOut, "warning: %s\n", warning)
	}
	action := map[string]string{
		"enable_claude_picker":       "enable",
		"disable_claude_picker":      "disable",
		"add_claude_picker_model":    "add",
		"remove_claude_picker_model": "remove",
		"move_claude_picker_model":   "move",
	}[result.Operation]
	if action == "" {
		action = result.Operation
	}
	fmt.Fprintf(stdout, "claude picker: %s complete\n", action)
	return 0
}

func parsePickerOptions(args []string) (mutationOptions, []string, error) {
	opts := mutationOptions{OperationID: newOperationID()}
	clean := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			opts.JSON = true
		case "--dry-run":
			opts.DryRun = true
		case "--alias":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--alias requires a value")
			}
			i++
			opts.PickerAlias = args[i]
		case "--convert-replacement-mode":
			opts.ConvertPickerReplacement = true
		case "--operation-id":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--operation-id requires a value")
			}
			i++
			opts.OperationID = args[i]
			opts.hasOperationID = true
		case "--if-active-token":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--if-active-token requires a value")
			}
			i++
			opts.IfActiveToken = args[i]
			opts.hasActiveToken = true
		case "--if-config-hash":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--if-config-hash requires a value")
			}
			i++
			opts.IfConfigHash = args[i]
			opts.hasConfigHash = true
		default:
			clean = append(clean, args[i])
		}
	}
	if len(clean) == 0 {
		return opts, nil, fmt.Errorf("missing picker subcommand")
	}
	return opts, clean, nil
}

func pickerTerminalReplayRequest(args []string) (mutationOptions, journaledMutationSpec, error) {
	opts, positional, err := parsePickerOptions(args)
	if err != nil {
		return opts, journaledMutationSpec{}, err
	}
	if opts.DryRun {
		return opts, journaledMutationSpec{}, fmt.Errorf("dry-run has no mutation terminal")
	}
	spec := journaledMutationSpec{Key: "model_picker"}
	switch positional[0] {
	case "enable":
		if len(positional) != 1 {
			return opts, spec, fmt.Errorf("invalid enable request")
		}
		spec.Operation = "enable_claude_picker"
	case "disable":
		if len(positional) != 1 {
			return opts, spec, fmt.Errorf("invalid disable request")
		}
		spec.Operation = "disable_claude_picker"
	case "add":
		if len(positional) != 2 {
			return opts, spec, fmt.Errorf("invalid add request")
		}
		spec.Operation = "add_claude_picker_model"
		spec.RequestedTarget = positional[1]
		if opts.PickerAlias != "" {
			spec.RequestedTarget += " via " + opts.PickerAlias
		}
	case "remove":
		if len(positional) != 2 {
			return opts, spec, fmt.Errorf("invalid remove request")
		}
		spec.Operation = "remove_claude_picker_model"
		spec.RequestedTarget = positional[1]
	case "move":
		if len(positional) != 4 || positional[2] != "--before" {
			return opts, spec, fmt.Errorf("invalid move request")
		}
		spec.Operation = "move_claude_picker_model"
		spec.RequestedTarget = positional[1] + " before " + positional[3]
	default:
		return opts, spec, fmt.Errorf("not a picker config mutation")
	}
	return opts, spec, nil
}

func emitPickerMutation(out io.Writer, opts mutationOptions, operation, target string, a *claudeAdapter, before *pickerReceiptState, applied, reconciliation bool, outcome, message string) {
	result := mutationResult{
		OK: message == "", OperationID: opts.OperationID, Operation: operation, Requested: true,
		RequestedTarget: target, Client: a.clientName, Key: "model_picker",
		ConfigPath: a.configPath, Applied: applied, ReconciliationRequired: reconciliation,
		Outcome: outcome,
	}
	if before != nil {
		result.PreviousDesiredConfigHash = before.previousHash
		result.PreviousActiveToken = before.previousToken
	}
	if raw, _, err := readExactConfig(a.configPath); err == nil {
		result.DesiredConfigHash = exactConfigHash(raw)
	}
	if current, err := fetchRoutingAdminStatus(envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)); err == nil {
		result.ActiveToken = current.activeToken()
		result.ActiveConfigHash = current.ActiveConfigHash
	}
	if message != "" {
		retryable := reconciliation
		if outcome == "manual_resolution_required" {
			result.ReconciliationAction = "manual_claude_settings_resolution"
			retryable = false
		}
		result.Error = &mutationError{Code: outcome, Message: message, Retryable: retryable}
	}
	emitMutationResult(out, result)
}

func (a *claudeAdapter) removeModelPickerRowsLocked(root map[string]any, snap *safefile.Snapshot) error {
	bak, err := loadClaudeBackup(a.backupPath)
	if err != nil || bak == nil || bak.ModelPicker == nil {
		return err
	}
	clean := sha256Hex(snap.Data) == bak.WrittenHash && claudeBackupMatchesFile(bak, snap)
	changed, err := cleanupModelPicker(root, bak.ModelPicker, clean)
	if err != nil {
		return err
	}
	if changed {
		raw, committed, err := writeClaudeSettings(snap, root)
		if err != nil {
			return err
		}
		bak.WrittenHash = sha256Hex(raw)
		recordClaudeBackupFile(bak, committed)
	}
	bak.ModelPicker = nil
	return saveClaudeBackup(a.backupPath, bak)
}

func resolvePickerAlias(value string, aliases map[string]string) (string, error) {
	if _, ok := aliases[value]; ok {
		return value, nil
	}
	var matches []string
	for alias, slug := range aliases {
		if slug == value {
			matches = append(matches, alias)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		sort.Strings(matches)
		return "", fmt.Errorf("slug %q has multiple aliases: %s", value, strings.Join(matches, ", "))
	}
	return "", fmt.Errorf("%q has no configured model_aliases entry; add the route alias first", value)
}

func buildPickerPreview(slug string, aliases map[string]string, explicitAlias ...string) (claudePickerPreview, error) {
	if slug == "" || strings.TrimSpace(slug) != slug || strings.ContainsAny(slug, "\x00\r\n") {
		return claudePickerPreview{}, fmt.Errorf("invalid model slug")
	}
	requestedAlias := ""
	if len(explicitAlias) > 0 {
		requestedAlias = explicitAlias[0]
	}
	alias, err := generatedPickerAlias(slug, aliases)
	if requestedAlias != "" {
		alias, err = resolvePickerAlias(requestedAlias, aliases)
		if err == nil && aliases[alias] != slug {
			err = fmt.Errorf("alias %q routes to %q, not %q", alias, aliases[alias], slug)
		}
	}
	if err != nil {
		return claudePickerPreview{}, err
	}
	return pickerPreview(alias, slug), nil
}

func pickerPreview(alias, slug string) claudePickerPreview {
	return claudePickerPreview{
		Alias: alias, Slug: slug,
		Label:       modelmeta.ResolveBaseten(slug).DisplayName + " via Baseten",
		Description: "Served by Baseten.",
	}
}

func generatedPickerAlias(slug string, aliases map[string]string) (string, error) {
	existing := aliasesForSlug(slug, aliases)
	if len(existing) == 1 {
		return existing[0], nil
	}
	if len(existing) > 1 {
		sort.Strings(existing)
		return "", fmt.Errorf("slug %q has multiple aliases: %s; choose one with --alias", slug, strings.Join(existing, ", "))
	}
	leaf := slug
	if slash := strings.LastIndex(leaf, "/"); slash >= 0 {
		leaf = leaf[slash+1:]
	}
	var normalized strings.Builder
	pendingHyphen := false
	for _, char := range leaf {
		if char >= 'A' && char <= 'Z' {
			char += 'a' - 'A'
		}
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			if pendingHyphen && normalized.Len() > 0 {
				normalized.WriteByte('-')
			}
			normalized.WriteRune(char)
			pendingHyphen = false
		} else {
			pendingHyphen = normalized.Len() > 0
		}
	}
	component := strings.Trim(normalized.String(), "-")
	if component == "" {
		return "", fmt.Errorf("slug %q has no characters usable in a Claude alias", slug)
	}
	const prefix = "claude-baseten-"
	base := prefix + component
	if len(base) > 80 {
		base = base[:80]
		base = strings.TrimRight(base, "-")
	}
	if target, collision := aliases[base]; !collision || target == slug {
		return base, nil
	}
	sum := sha256.Sum256([]byte(slug))
	hexsum := hex.EncodeToString(sum[:])
	for _, width := range []int{8, 16} {
		suffix := "-" + hexsum[:width]
		stem := base
		if len(stem)+len(suffix) > 80 {
			stem = strings.TrimRight(stem[:80-len(suffix)], "-")
		}
		candidate := stem + suffix
		if target, collision := aliases[candidate]; !collision || target == slug {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not generate a stable alias for %q", slug)
}

func aliasesForSlug(slug string, aliases map[string]string) []string {
	var matches []string
	for alias, target := range aliases {
		if target == slug {
			matches = append(matches, alias)
		}
	}
	sort.Strings(matches)
	return matches
}

func pickerModelIndex(models []config.ModelPickerModel, alias string) int {
	alias, _ = normalizeClaudePickerModelID(alias)
	for i := range models {
		if models[i].Alias == alias {
			return i
		}
	}
	return -1
}

func pickerUsage(w io.Writer) int {
	fmt.Fprintln(w, "usage: baseten-switch claude picker enable [--convert-replacement-mode]|add <slug> [--alias <alias>]|remove <alias>|move <alias> --before <alias>|sync [--convert-replacement-mode]|disable|status|list")
	return 2
}

func shortPickerFingerprint(rows []json.RawMessage) string {
	h := sha256.New()
	for _, row := range rows {
		_, _ = h.Write(row)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func doctorModelPickerCheck(add addCheck, a *claudeAdapter) {
	status, err := a.currentPickerStatus()
	if err != nil {
		add("claude", "model_picker", docFail, err.Error(), "baseten-switch claude picker sync")
		return
	}
	if status.Enabled {
		if err := checkClaudeModelPickerVersion(); err != nil {
			add("claude", "model_picker", docFail, err.Error(), "upgrade Claude Code, then run 'baseten-switch claude picker sync'")
			return
		}
	}
	if status.UserFileSync == "blocked" {
		fix := "resolve the Claude user settings conflict, then run 'baseten-switch claude picker sync'"
		if status.ReplacementMode == "replace" {
			fix = "baseten-switch claude picker sync --convert-replacement-mode"
		}
		add("claude", "model_picker", docFail, status.Message, fix)
		return
	}
	if status.UserFileSync != "synced" {
		add("claude", "model_picker", docFail, "configured model_picker state is not synchronized to Claude user settings", "baseten-switch claude picker sync", "claude", "picker", "sync")
		return
	}
	baseFinding := fmt.Sprintf("configuration=%s, user_file_sync=%s, managed_policy=%s, runtime=%s", status.Configuration, status.UserFileSync, status.ManagedPolicy, status.RuntimeVerification)
	if status.AllowlistPolicy == "blocked" {
		add("claude", "model_picker", docFail, baseFinding+"; "+status.Message, "fix availableModels in Claude user settings")
		return
	}
	var warnings []string
	if status.AllowlistPolicy == "possible_conflict" {
		warnings = append(warnings, "possible Claude Code allowlist conflict")
	}
	if status.LegacyDiscoveryEnabled {
		warnings = append(warnings, "legacy gateway discovery is also enabled")
	}
	if status.SavedModelUnconfigured {
		warnings = append(warnings, fmt.Sprintf("saved default %q is no longer configured in model_picker", status.SavedModel))
	}
	if status.SavedModelContextMismatch {
		warnings = append(warnings, fmt.Sprintf("saved default %q still requests 1M context, but its configured picker row now uses the 200K context bucket", status.SavedModel))
	}
	if len(warnings) > 0 {
		add("claude", "model_picker", docWarn, baseFinding+"; "+strings.Join(warnings, "; "), "review Claude user settings and reopen /model")
		return
	}
	switch status.UserFileSync {
	case "synced":
		add("claude", "model_picker", docOK, baseFinding, "")
	default:
		add("claude", "model_picker", docFail, baseFinding, "baseten-switch claude picker sync", "claude", "picker", "sync")
	}
}
