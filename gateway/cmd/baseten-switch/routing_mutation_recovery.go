package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
)

var (
	mutationTerminalLink       = os.Link
	mutationTerminalRemove     = os.Remove
	mutationTerminalSync       = syncDirectory
	errMutationJournalConflict = errors.New("multiple active mutation journals")
)

const (
	mutationTerminalVersion    = 2
	mutationFingerprintVersion = 2

	mutationOutcomeApplied    = "applied"
	mutationOutcomeUnchanged  = "unchanged"
	mutationOutcomeNotApplied = "not_applied"
	mutationOutcomeRolledBack = "rolled_back"
	mutationOutcomeRejected   = "rejected"

	mutationIdentityExact  = "exact"
	mutationIdentityLegacy = "legacy"

	mutationStatusNone                   = "none"
	mutationStatusDesiredActive          = "desired_active"
	mutationStatusPriorActive            = "prior_active"
	mutationStatusDesiredPending         = "desired_pending"
	mutationStatusPriorPending           = "prior_pending"
	mutationStatusRouterUnavailable      = "router_unavailable"
	mutationStatusRouterUnsupported      = "router_unsupported"
	mutationStatusExternalChange         = "external_change"
	mutationStatusCommitRecoveryRequired = "commit_recovery_required"
	mutationStatusJournalInvalid         = "journal_invalid"
	mutationStatusJournalConflict        = "journal_conflict"
	mutationStatusCleanupPending         = "cleanup_pending"

	mutationConfigReadLimit   = 4 << 20
	mutationJournalReadLimit  = 6 << 20
	mutationTerminalReadLimit = 64 << 10
	mutationCompletedScanMax  = 4096
	mutationTerminalRetain    = 30 * 24 * time.Hour
)

type mutationFingerprintInput struct {
	FingerprintVersion      int    `json:"fingerprint_version"`
	CanonicalConfigIdentity string `json:"canonical_config_identity"`
	Operation               string `json:"operation"`
	Surface                 string `json:"surface"`
	Client                  string `json:"client,omitempty"`
	Key                     string `json:"key,omitempty"`
	RequestedPresent        bool   `json:"requested_present"`
	Requested               bool   `json:"requested"`
	RequestedTarget         string `json:"requested_target,omitempty"`
	IfConfigHash            string `json:"if_config_hash,omitempty"`
	IfActiveToken           string `json:"if_active_token,omitempty"`
}

type mutationTerminalRecord struct {
	Version             int       `json:"version"`
	OperationID         string    `json:"operation_id"`
	IdentityStrength    string    `json:"identity_strength"`
	ConfigIdentityHash  string    `json:"config_identity_hash"`
	RequestFingerprint  string    `json:"request_fingerprint,omitempty"`
	KeyHash             string    `json:"key_hash,omitempty"`
	RequestedTargetHash string    `json:"requested_target_hash,omitempty"`
	Operation           string    `json:"operation"`
	Surface             string    `json:"surface,omitempty"`
	Client              string    `json:"client,omitempty"`
	RequestedPresent    bool      `json:"requested_present"`
	Requested           bool      `json:"requested"`
	Outcome             string    `json:"outcome"`
	PreviousConfigHash  string    `json:"previous_config_hash"`
	DesiredConfigHash   string    `json:"desired_config_hash"`
	ActiveConfigHash    string    `json:"active_config_hash"`
	PreviousActiveToken string    `json:"previous_active_token,omitempty"`
	ActiveToken         string    `json:"active_token,omitempty"`
	ErrorCode           string    `json:"error_code,omitempty"`
	ErrorRetryable      bool      `json:"error_retryable,omitempty"`
	CompletedAt         time.Time `json:"completed_at"`
}

type routingMutationStatus struct {
	Classification      string         `json:"classification"`
	BlockingOperationID string         `json:"blocking_operation_id,omitempty"`
	Error               *mutationError `json:"error,omitempty"`
}

type terminalPublishResult struct {
	Record         mutationTerminalRecord
	CleanupPending bool
}

type mutationRecoveryResult struct {
	OK                 bool           `json:"ok"`
	Classification     string         `json:"classification"`
	OperationID        string         `json:"operation_id,omitempty"`
	Outcome            string         `json:"outcome,omitempty"`
	Applied            bool           `json:"applied"`
	IdentityStrength   string         `json:"identity_strength,omitempty"`
	RequestFingerprint string         `json:"request_fingerprint,omitempty"`
	CleanupPending     bool           `json:"cleanup_pending,omitempty"`
	Error              *mutationError `json:"error,omitempty"`
}

func requestedPresent(operation string) bool {
	return operation == "set_global_routing" || operation == "set_fallback_policy"
}

func domainHash(domain, value string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func configIdentityHash(path string) string {
	return domainHash("config-identity-v1", canonicalPath(path))
}

func mutationRequestFingerprint(path string, opts mutationOptions, spec journaledMutationSpec) (string, error) {
	if !validMutationSurface(spec.Operation, spec.Surface) {
		return "", fmt.Errorf("invalid mutation surface")
	}
	input := mutationFingerprintInput{
		FingerprintVersion:      mutationFingerprintVersion,
		CanonicalConfigIdentity: canonicalPath(path),
		Operation:               spec.Operation,
		Surface:                 spec.Surface,
		Client:                  spec.Client,
		Key:                     spec.Key,
		RequestedPresent:        requestedPresent(spec.Operation),
		Requested:               spec.Requested,
		RequestedTarget:         spec.RequestedTarget,
	}
	if opts.hasConfigHash {
		input.IfConfigHash = opts.IfConfigHash
	}
	if opts.hasActiveToken {
		input.IfActiveToken = opts.IfActiveToken
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func mutationCompletedDir(configPath string) string {
	return filepath.Join(mutationJournalDir(configPath), "completed")
}

func mutationTerminalPath(configPath, operationID string) string {
	return filepath.Join(mutationCompletedDir(configPath), operationID+".json")
}

func secureDirectory(path string, create bool) error {
	if create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("mutation state directory is not a secure regular directory")
	}
	return nil
}

func readBoundedRegularFile(path string, limit int64, requireMode os.FileMode) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("mutation state entry is not a regular file")
	}
	if requireMode != 0 && info.Mode().Perm() != requireMode.Perm() {
		return nil, fmt.Errorf("mutation state entry has unsafe permissions")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("mutation state entry exceeds the size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("mutation state entry changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("mutation state entry exceeds the size limit")
	}
	return data, nil
}

func validateMutationStateDirectories(configPath string, completed bool) error {
	if err := secureDirectory(mutationJournalDir(configPath), false); err != nil {
		return err
	}
	if completed {
		return secureDirectory(mutationCompletedDir(configPath), false)
	}
	return nil
}

func validTerminalOperationShape(record mutationTerminalRecord) bool {
	if record.RequestedPresent != requestedPresent(record.Operation) {
		return false
	}
	switch record.Operation {
	case "set_global_routing":
		return validMutationSurface(record.Operation, record.Surface) && record.Client == "" && record.KeyHash == "" && record.RequestedTargetHash == ""
	case "set_fallback_policy":
		return validMutationSurface(record.Operation, record.Surface) && record.Client == "" && validConfigHash(record.KeyHash) && validConfigHash(record.RequestedTargetHash)
	case "set_native_fallback_model":
		return validMutationSurface(record.Operation, record.Surface) && !record.Requested && record.Client != "" && validConfigHash(record.KeyHash) && validConfigHash(record.RequestedTargetHash)
	case "set_claude_route", "set_claude_subagents",
		"enable_claude_picker", "disable_claude_picker",
		"add_claude_picker_model", "remove_claude_picker_model", "move_claude_picker_model":
		return validMutationSurface(record.Operation, record.Surface) && !record.Requested && record.Client != "" && validConfigHash(record.KeyHash) && validConfigHash(record.RequestedTargetHash)
	case "set_codex_route":
		return validMutationSurface(record.Operation, record.Surface) && !record.Requested && record.Client != "" && validConfigHash(record.KeyHash) && validConfigHash(record.RequestedTargetHash)
	case "set_model_reasoning":
		return validMutationSurface(record.Operation, record.Surface) &&
			!record.Requested && record.Client != "" && validConfigHash(record.KeyHash) && validConfigHash(record.RequestedTargetHash)
	default:
		return false
	}
}

func validMutationSurface(operation, surface string) bool {
	switch operation {
	case "set_global_routing":
		return surface == mutationSurfaceSwitch
	case "set_fallback_policy", "set_native_fallback_model":
		return surface == mutationSurfaceConfig
	case "set_claude_route", "set_claude_subagents",
		"enable_claude_picker", "disable_claude_picker",
		"add_claude_picker_model", "remove_claude_picker_model", "move_claude_picker_model":
		return surface == mutationSurfaceClaude
	case "set_codex_route":
		return surface == mutationSurfaceCodex
	case "set_model_reasoning":
		return surface == mutationSurfaceClaude || surface == mutationSurfaceCodex
	default:
		return false
	}
}

func validLegacyTerminalOperationShape(record mutationTerminalRecord) bool {
	if record.Surface != "" || record.RequestedPresent != requestedPresent(record.Operation) {
		return false
	}
	switch record.Operation {
	case "set_global_routing":
		return record.Client == "" && record.KeyHash == "" && record.RequestedTargetHash == ""
	case "set_claude_route", "set_claude_subagents", "set_codex_route", "set_model_reasoning":
		return !record.Requested && record.Client != "" && validConfigHash(record.KeyHash) && validConfigHash(record.RequestedTargetHash)
	default:
		return false
	}
}

func validMutationActiveToken(token string) bool {
	separator := strings.LastIndexByte(token, ':')
	if separator <= 0 || separator == len(token)-1 || strings.Contains(token[:separator], ":") {
		return false
	}
	generation, err := strconv.ParseUint(token[separator+1:], 10, 64)
	return err == nil && generation > 0
}

func validateTerminalRecord(record mutationTerminalRecord, path, operationID string) error {
	if record.Version != mutationTerminalVersion || record.OperationID != operationID ||
		record.ConfigIdentityHash != configIdentityHash(path) || !validConfigHash(record.ConfigIdentityHash) ||
		record.CompletedAt.IsZero() || record.CompletedAt.After(time.Now().UTC()) {
		return fmt.Errorf("terminal record identity is invalid")
	}
	if record.IdentityStrength != mutationIdentityExact && record.IdentityStrength != mutationIdentityLegacy {
		return fmt.Errorf("terminal record identity strength is invalid")
	}
	if record.IdentityStrength == mutationIdentityExact {
		if !validConfigHash(record.RequestFingerprint) {
			return fmt.Errorf("terminal record fingerprint is invalid")
		}
		if !validTerminalOperationShape(record) {
			return fmt.Errorf("terminal record surface is invalid")
		}
	} else {
		if record.RequestFingerprint != "" {
			return fmt.Errorf("legacy terminal record cannot contain an exact fingerprint")
		}
		if !validLegacyTerminalOperationShape(record) {
			return fmt.Errorf("legacy terminal record surface is invalid")
		}
	}
	switch record.Outcome {
	case mutationOutcomeApplied, mutationOutcomeUnchanged, mutationOutcomeNotApplied, mutationOutcomeRolledBack, mutationOutcomeRejected:
	default:
		return fmt.Errorf("terminal record outcome is invalid")
	}
	if !validConfigHash(record.PreviousConfigHash) || !validConfigHash(record.DesiredConfigHash) {
		return fmt.Errorf("terminal record hashes are invalid")
	}
	if record.ActiveConfigHash != "" && !validConfigHash(record.ActiveConfigHash) {
		return fmt.Errorf("terminal record active hash is invalid")
	}
	if (record.PreviousActiveToken != "" && !validMutationActiveToken(record.PreviousActiveToken)) ||
		(record.ActiveToken != "" && !validMutationActiveToken(record.ActiveToken)) {
		return fmt.Errorf("terminal record active token is invalid")
	}
	if (record.ActiveToken == "") != (record.ActiveConfigHash == "") {
		return fmt.Errorf("terminal record active token and hash are inconsistent")
	}
	if record.ErrorRetryable && record.ErrorCode == "" {
		return fmt.Errorf("terminal record error state is inconsistent")
	}
	switch record.Outcome {
	case mutationOutcomeApplied:
		if record.ActiveConfigHash != record.DesiredConfigHash || record.ActiveToken == "" || record.ErrorCode != "" ||
			record.ErrorRetryable || record.PreviousConfigHash == record.DesiredConfigHash || record.ActiveToken == record.PreviousActiveToken {
			return fmt.Errorf("applied terminal record is inconsistent")
		}
	case mutationOutcomeUnchanged:
		if record.PreviousConfigHash != record.DesiredConfigHash || record.ErrorCode != "" || record.ErrorRetryable ||
			(record.ActiveConfigHash != "" && record.ActiveConfigHash != record.DesiredConfigHash) || record.ActiveToken != record.PreviousActiveToken {
			return fmt.Errorf("unchanged terminal record is inconsistent")
		}
	case mutationOutcomeNotApplied:
		if record.ActiveConfigHash != record.PreviousConfigHash || record.ActiveToken == "" || record.ErrorCode != "" || record.ErrorRetryable {
			return fmt.Errorf("not-applied terminal record is inconsistent")
		}
	case mutationOutcomeRolledBack:
		if record.ActiveConfigHash != record.PreviousConfigHash || record.ActiveToken == "" ||
			record.ErrorCode != "activation_failed_rolled_back" || record.ErrorRetryable {
			return fmt.Errorf("rolled-back terminal record is inconsistent")
		}
	case mutationOutcomeRejected:
		if record.ErrorCode != "stale_config_hash" || !record.ErrorRetryable ||
			record.PreviousConfigHash == record.DesiredConfigHash ||
			(record.ActiveConfigHash != "" && record.ActiveConfigHash != record.PreviousConfigHash) ||
			record.ActiveToken != record.PreviousActiveToken {
			return fmt.Errorf("rejected terminal record is inconsistent")
		}
	}
	return nil
}

func readMutationTerminal(configPath, operationID string) (mutationTerminalRecord, error) {
	var record mutationTerminalRecord
	if err := validateMutationStateDirectories(configPath, true); err != nil {
		return record, err
	}
	data, err := readBoundedRegularFile(mutationTerminalPath(configPath, operationID), mutationTerminalReadLimit, 0o600)
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&record); err != nil {
		return record, fmt.Errorf("decode terminal record: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return record, fmt.Errorf("decode terminal record: trailing JSON content")
	}
	if err := validateTerminalRecord(record, configPath, operationID); err != nil {
		return record, err
	}
	return record, nil
}

func terminalRecordsEqual(a, b mutationTerminalRecord) bool {
	// completed_at belongs to the first durable publication. A retry must
	// validate every replay-affecting field and reuse that original timestamp.
	b.CompletedAt = a.CompletedAt
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func terminalMatchesJournal(record mutationTerminalRecord, journal routingMutationJournal) bool {
	if record.OperationID != journal.OperationID ||
		record.ConfigIdentityHash != configIdentityHash(journal.ConfigPath) ||
		record.Operation != journal.Operation || record.Surface != journal.Surface || record.Client != journal.Client ||
		record.RequestedPresent != requestedPresent(journal.Operation) || record.Requested != journal.Requested ||
		record.PreviousConfigHash != journal.PreviousConfigHash || record.DesiredConfigHash != journal.DesiredConfigHash ||
		record.PreviousActiveToken != journal.PreviousActiveToken {
		return false
	}
	if journal.Key != "" && record.KeyHash != domainHash("mutation-key-v1", journal.Key) {
		return false
	}
	if journal.Key == "" && record.KeyHash != "" {
		return false
	}
	if journal.RequestedTarget != "" && record.RequestedTargetHash != domainHash("requested-target-v1", journal.RequestedTarget) {
		return false
	}
	if journal.RequestedTarget == "" && record.RequestedTargetHash != "" {
		return false
	}
	if journal.RequestFingerprint == "" {
		return record.IdentityStrength == mutationIdentityLegacy && record.RequestFingerprint == ""
	}
	return record.IdentityStrength == mutationIdentityExact && record.RequestFingerprint == journal.RequestFingerprint
}

func terminalFromJournal(journal routingMutationJournal, outcome string, active routingAdminStatus, errorCode string, retryable bool) mutationTerminalRecord {
	strength := mutationIdentityLegacy
	if journal.RequestFingerprint != "" {
		strength = mutationIdentityExact
	}
	record := mutationTerminalRecord{
		Version:             mutationTerminalVersion,
		OperationID:         journal.OperationID,
		IdentityStrength:    strength,
		ConfigIdentityHash:  configIdentityHash(journal.ConfigPath),
		RequestFingerprint:  journal.RequestFingerprint,
		Operation:           journal.Operation,
		Surface:             journal.Surface,
		Client:              journal.Client,
		RequestedPresent:    requestedPresent(journal.Operation),
		Requested:           journal.Requested,
		Outcome:             outcome,
		PreviousConfigHash:  journal.PreviousConfigHash,
		DesiredConfigHash:   journal.DesiredConfigHash,
		ActiveConfigHash:    active.ActiveConfigHash,
		PreviousActiveToken: journal.PreviousActiveToken,
		ActiveToken:         active.activeToken(),
		ErrorCode:           errorCode,
		ErrorRetryable:      retryable,
		CompletedAt:         time.Now().UTC(),
	}
	if journal.Key != "" {
		record.KeyHash = domainHash("mutation-key-v1", journal.Key)
	}
	if journal.RequestedTarget != "" {
		record.RequestedTargetHash = domainHash("requested-target-v1", journal.RequestedTarget)
	}
	return record
}

func publishMutationTerminal(configPath string, record mutationTerminalRecord, active *routingMutationJournal) (terminalPublishResult, error) {
	result := terminalPublishResult{Record: record}
	if err := validateTerminalRecord(record, configPath, record.OperationID); err != nil {
		return result, err
	}
	journalDir := mutationJournalDir(configPath)
	if err := secureDirectory(journalDir, true); err != nil {
		return result, fmt.Errorf("prepare mutation journal directory: %w", err)
	}
	completedDir := mutationCompletedDir(configPath)
	created := false
	if _, err := os.Lstat(completedDir); errors.Is(err, os.ErrNotExist) {
		created = true
	}
	if err := secureDirectory(completedDir, true); err != nil {
		return result, fmt.Errorf("prepare completed mutation directory: %w", err)
	}
	if created {
		if err := mutationTerminalSync(journalDir); err != nil {
			return result, fmt.Errorf("sync mutation journal directory: %w", err)
		}
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return result, err
	}
	data = append(data, '\n')
	if len(data) > mutationTerminalReadLimit {
		return result, fmt.Errorf("terminal record exceeds the size limit")
	}
	temp, err := os.CreateTemp(completedDir, ".terminal-*")
	if err != nil {
		return result, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return result, err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return result, err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return result, err
	}
	if err := temp.Close(); err != nil {
		return result, err
	}
	finalPath := mutationTerminalPath(configPath, record.OperationID)
	if err := mutationTerminalLink(tempPath, finalPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return result, err
		}
		existing, readErr := readMutationTerminal(configPath, record.OperationID)
		if readErr != nil || !terminalRecordsEqual(existing, record) {
			return result, fmt.Errorf("terminal_conflict")
		}
		result.Record = existing
	}
	if err := mutationTerminalRemove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if err := mutationTerminalSync(completedDir); err != nil {
		return result, err
	}
	if active != nil {
		current, err := readMutationJournal(configPath, active.OperationID)
		if err != nil || !terminalMatchesJournal(result.Record, current) {
			result.CleanupPending = true
			return result, nil
		}
		if err := mutationTerminalRemove(mutationJournalPath(configPath, active.OperationID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			result.CleanupPending = true
			return result, nil
		}
		if err := mutationTerminalSync(journalDir); err != nil {
			result.CleanupPending = true
			return result, nil
		}
	}
	_ = gcMutationTerminals(configPath, time.Now().UTC())
	return result, nil
}

func gcMutationTerminals(configPath string, now time.Time) error {
	dir := mutationCompletedDir(configPath)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > mutationCompletedScanMax {
		return fmt.Errorf("completed mutation directory exceeds the scan limit")
	}
	activeOperationIDs, err := activeMutationOperationIDsForGC(configPath)
	if err != nil {
		return err
	}
	type candidate struct {
		name string
		when time.Time
	}
	var old []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		operationID := strings.TrimSuffix(entry.Name(), ".json")
		if _, active := activeOperationIDs[operationID]; active {
			continue
		}
		record, readErr := readMutationTerminal(configPath, operationID)
		if readErr != nil || record.CompletedAt.After(now) || now.Sub(record.CompletedAt) < mutationTerminalRetain {
			continue
		}
		old = append(old, candidate{name: entry.Name(), when: record.CompletedAt})
	}
	sort.Slice(old, func(i, j int) bool { return old[i].when.Before(old[j].when) })
	if len(old) == 0 {
		return nil
	}
	for _, candidate := range old {
		if err := os.Remove(filepath.Join(dir, candidate.name)); err != nil {
			return err
		}
	}
	return syncDirectory(dir)
}

// activeMutationOperationIDsForGC identifies every terminal that must be
// retained because top-level recovery state still exists. It validates all
// active entries before GC can delete anything, so malformed or ambiguous
// recovery state makes retention fail closed.
func activeMutationOperationIDsForGC(configPath string) (map[string]struct{}, error) {
	dir := mutationJournalDir(configPath)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := secureDirectory(dir, false); err != nil {
		return nil, err
	}
	operationIDs := make(map[string]struct{})
	for _, entry := range entries {
		if entry.Name() == "completed" && entry.IsDir() {
			if err := secureDirectory(filepath.Join(dir, entry.Name()), false); err != nil {
				return nil, err
			}
			continue
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, fmt.Errorf("unexpected mutation journal entry")
		}
		operationID := strings.TrimSuffix(entry.Name(), ".json")
		if err := validateOperationID(operationID); err != nil {
			return nil, fmt.Errorf("invalid mutation journal operation ID")
		}
		if _, err := readMutationJournal(configPath, operationID); err != nil {
			return nil, err
		}
		operationIDs[operationID] = struct{}{}
	}
	if len(operationIDs) > 1 {
		return nil, errMutationJournalConflict
	}
	return operationIDs, nil
}

func resultFromTerminal(path string, record mutationTerminalRecord, includeRequest bool, spec journaledMutationSpec) mutationResult {
	result := mutationResult{
		OperationID:               record.OperationID,
		Operation:                 record.Operation,
		Client:                    record.Client,
		ConfigPath:                path,
		Requested:                 record.Requested,
		PreviousActiveToken:       record.PreviousActiveToken,
		PreviousDesiredConfigHash: record.PreviousConfigHash,
		DesiredConfigHash:         record.DesiredConfigHash,
		ActiveToken:               record.ActiveToken,
		ActiveConfigHash:          record.ActiveConfigHash,
		Outcome:                   record.Outcome,
		RequestFingerprint:        record.RequestFingerprint,
		IdentityStrength:          record.IdentityStrength,
	}
	if includeRequest {
		result.Key = spec.Key
		result.RequestedTarget = spec.RequestedTarget
		result.StructuredWarnings = spec.StructuredWarnings
	}
	switch record.Outcome {
	case mutationOutcomeApplied:
		result.OK, result.Applied = true, true
	case mutationOutcomeUnchanged:
		result.OK = true
		result.Applied = record.ActiveConfigHash == record.DesiredConfigHash && record.ActiveConfigHash != ""
	case mutationOutcomeNotApplied, mutationOutcomeRolledBack:
		result.Applied = false
	case mutationOutcomeRejected:
		result.Applied = false
	}
	return result
}

// terminalCleanupPending preserves the recovery state represented by a
// terminal record whose matching active journal could not be removed. A
// terminal record is authoritative for the mutation outcome, but it is not
// proof that the active journal cleanup completed.
func terminalCleanupPending(path string, record mutationTerminalRecord) (bool, error) {
	journal, err := readMutationJournal(path, record.OperationID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !terminalMatchesJournal(record, journal) {
		return false, fmt.Errorf("terminal_conflict")
	}
	return true, nil
}

func replayTerminalForRequest(path string, opts mutationOptions, spec journaledMutationSpec) (mutationResult, bool, int, error) {
	record, err := readMutationTerminal(path, opts.OperationID)
	if errors.Is(err, os.ErrNotExist) {
		return mutationResult{}, false, 0, nil
	}
	if err != nil {
		return mutationResult{}, true, 1, err
	}
	baseResult := resultFromTerminal(path, record, true, spec)
	if record.IdentityStrength != mutationIdentityExact {
		return baseResult, true, 1, fmt.Errorf("operation_id_legacy")
	}
	if record.Surface != spec.Surface {
		return baseResult, true, 1, fmt.Errorf("operation_id_conflict")
	}
	if spec.Client == "" && spec.Surface != mutationSurfaceSwitch {
		spec.Client = record.Client
		baseResult.Client = record.Client
	}
	fingerprint, err := mutationRequestFingerprint(path, opts, spec)
	if err != nil {
		return mutationResult{}, true, 1, err
	}
	if record.RequestFingerprint != fingerprint {
		return baseResult, true, 1, fmt.Errorf("operation_id_conflict")
	}
	result := baseResult
	cleanupPending, err := terminalCleanupPending(path, record)
	if err != nil {
		return result, true, 1, err
	}
	result.CleanupPending = cleanupPending
	switch record.Outcome {
	case mutationOutcomeApplied, mutationOutcomeUnchanged:
		return result, true, 0, nil
	case mutationOutcomeNotApplied:
		return result, true, 1, fmt.Errorf("mutation_not_applied")
	case mutationOutcomeRolledBack:
		return result, true, 1, fmt.Errorf("activation_failed_rolled_back")
	default:
		code := record.ErrorCode
		if code == "" {
			code = "mutation_rejected"
		}
		return result, true, 1, fmt.Errorf("%s", code)
	}
}

// preflightTerminalReplay is the small adapter-entry hook that enforces the
// replay-before-config-loading contract. A missing terminal is not authority:
// the caller continues through its normal config load and locked mutation.
func preflightTerminalReplay(path string, opts mutationOptions, spec journaledMutationSpec, out io.Writer) (bool, int) {
	if !opts.hasOperationID {
		return false, 0
	}
	if _, err := os.Lstat(mutationTerminalPath(path, opts.OperationID)); errors.Is(err, os.ErrNotExist) {
		return false, 0
	} else if err != nil {
		result := mutationResultForSpec(spec, opts.OperationID, path, "")
		return true, failMutation(opts, out, result, "terminal_conflict", reviewedMutationMessage("terminal_conflict"), false, 1)
	}
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		result := mutationResultForSpec(spec, opts.OperationID, path, "")
		return true, failMutation(opts, out, result, "mutation_locked", "routing mutation replay is temporarily unavailable", true, 1)
	}
	defer lock.close()
	replay, found, rc, replayErr := replayTerminalForRequest(path, opts, spec)
	if !found {
		return false, 0
	}
	_ = gcMutationTerminals(path, time.Now().UTC())
	if replayErr != nil && replayErr.Error() != "operation_id_conflict" && replayErr.Error() != "operation_id_legacy" &&
		replayErr.Error() != "mutation_not_applied" && replayErr.Error() != "activation_failed_rolled_back" {
		replayErr = fmt.Errorf("terminal_conflict")
	}
	return true, emitTerminalReplay(opts, out, replay, rc, replayErr)
}

func emitTerminalReplay(opts mutationOptions, out io.Writer, result mutationResult, rc int, replayErr error) int {
	if replayErr != nil {
		code := replayErr.Error()
		message := reviewedMutationMessage(code)
		result.Error = &mutationError{Code: code, Message: message, Retryable: false}
		result.OK = false
	}
	if opts.JSON {
		emitMutationResult(out, result)
	} else if replayErr != nil {
		fmt.Fprintln(os.Stderr, reviewedMutationMessage(replayErr.Error()))
	} else {
		fmt.Fprintf(out, "mutation %s replayed (%s)\n", result.OperationID, result.Outcome)
	}
	return rc
}

func reviewedMutationMessage(code string) string {
	switch code {
	case "operation_id_conflict":
		return "that operation ID belongs to a different routing mutation"
	case "operation_id_legacy":
		return "that operation ID has legacy recovery state and cannot replay an original command"
	case "mutation_not_applied":
		return "the routing mutation was not applied; the prior routing state is active"
	case "activation_failed_rolled_back":
		return "routing activation failed and the prior routing state was restored"
	case "journal_not_found":
		return "no routing mutation recovery state exists for that operation ID"
	case "terminal_conflict":
		return "routing mutation completion state conflicts with active recovery state"
	case "terminal_write_failed":
		return "routing changed, but durable completion could not be recorded"
	case "router_unavailable":
		return "the router is unavailable; routing recovery state was preserved"
	case "router_unsupported":
		return "the running router does not support the state required for this routing operation"
	case "mutation_locked":
		return "another routing mutation is in progress; retry this operation"
	case "stale_config_hash", "stale_active_token":
		return "routing state changed before this mutation could be applied"
	case "journal_read_failed", "journal_write_failed":
		return "routing mutation recovery state could not be safely accessed"
	case "config_read_failed", "config_load_failed", "config_edit_failed", "config_validation_failed", "invalid_routing_policy":
		return "the routing config could not be safely prepared"
	case "router_state_mismatch", "router_identity_mismatch":
		return "the running router does not match the managed routing config"
	case "commit_recovery_failed":
		return "an interrupted config commit could not be safely recovered"
	case "reconcile_failed", "activation_indeterminate":
		return "the routing result could not be confirmed; recovery state was preserved"
	case "reasoning_preflight_failed":
		return "the reasoning policy could not be validated against the running router"
	case "fingerprint_failed":
		return "the routing request could not be identified safely"
	case "cleanup_predicate_changed":
		return "routing state changed during cleanup; retry the cleanup operation"
	case "unfinished_mutation":
		return "another routing mutation must be reconciled before starting this operation"
	case "config_size_limit":
		return "the edited routing config exceeds the mutation size limit"
	case "invalid_subagent_state":
		return "set a subagent model before enabling subagent routing"
	case "external_config_change":
		return "the config changed outside this routing mutation; recovery state was preserved"
	case "commit_recovery_required":
		return "an interrupted config commit requires explicit reconciliation"
	case "journal_invalid":
		return "routing mutation recovery state is invalid and was preserved"
	case "journal_conflict":
		return "multiple or conflicting routing mutation records were preserved"
	default:
		return "the routing mutation could not be completed"
	}
}

func hasInterruptedExactCommit(path string) (bool, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	prefix := exactCommitDirPrefix(path)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && entry.IsDir() {
			if _, err := os.Lstat(filepath.Join(filepath.Dir(path), entry.Name(), "state.json")); err == nil {
				return true, nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		}
	}
	return false, nil
}

func singleActiveMutation(configPath string) (routingMutationJournal, bool, error) {
	var empty routingMutationJournal
	dir := mutationJournalDir(configPath)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, err
	}
	if err := secureDirectory(dir, false); err != nil {
		return empty, false, err
	}
	var operationIDs []string
	for _, entry := range entries {
		if entry.Name() == "completed" && entry.IsDir() {
			if err := secureDirectory(filepath.Join(dir, entry.Name()), false); err != nil {
				return empty, false, err
			}
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") {
			return empty, false, fmt.Errorf("unexpected mutation journal entry")
		}
		operationIDs = append(operationIDs, strings.TrimSuffix(entry.Name(), ".json"))
	}
	if len(operationIDs) == 0 {
		return empty, false, nil
	}
	if len(operationIDs) != 1 {
		return empty, false, errMutationJournalConflict
	}
	journal, err := readMutationJournal(configPath, operationIDs[0])
	if err != nil {
		return empty, false, err
	}
	return journal, true, nil
}

func validateCompletedMutationRecords(configPath string, now time.Time) error {
	dir := mutationCompletedDir(configPath)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := secureDirectory(dir, false); err != nil {
		return err
	}
	if len(entries) > mutationCompletedScanMax {
		return fmt.Errorf("completed mutation directory exceeds the scan limit")
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") {
			return fmt.Errorf("unexpected completed mutation entry")
		}
		operationID := strings.TrimSuffix(entry.Name(), ".json")
		record, err := readMutationTerminal(configPath, operationID)
		if err != nil {
			return err
		}
		if record.CompletedAt.After(now) {
			return fmt.Errorf("completed mutation record is future-dated")
		}
	}
	return nil
}

func inspectRoutingMutationStatus(configPath string) (routingMutationStatus, error) {
	lock, err := acquireConfigMutationLock(configPath)
	if err != nil {
		return routingMutationStatus{}, err
	}
	defer lock.close()
	return inspectRoutingMutationStatusLocked(configPath)
}

func inspectRoutingMutationStatusLocked(configPath string) (routingMutationStatus, error) {
	status := routingMutationStatus{Classification: mutationStatusNone}
	if err := validateCompletedMutationRecords(configPath, time.Now().UTC()); err != nil {
		status.Classification = mutationStatusJournalInvalid
		status.Error = &mutationError{Code: mutationStatusJournalInvalid, Message: reviewedMutationMessage(mutationStatusJournalInvalid)}
		return status, nil
	}
	journal, found, err := singleActiveMutation(configPath)
	if err != nil {
		if errors.Is(err, errMutationJournalConflict) {
			status.Classification = mutationStatusJournalConflict
			status.Error = &mutationError{Code: mutationStatusJournalConflict, Message: reviewedMutationMessage(mutationStatusJournalConflict)}
			return status, nil
		}
		status.Classification = mutationStatusJournalInvalid
		status.Error = &mutationError{Code: mutationStatusJournalInvalid, Message: reviewedMutationMessage(mutationStatusJournalInvalid)}
		return status, nil
	}
	if found {
		status.BlockingOperationID = journal.OperationID
	}
	interrupted, err := hasInterruptedExactCommit(configPath)
	if err != nil {
		return routingMutationStatus{}, err
	}
	if interrupted {
		status.Classification = mutationStatusCommitRecoveryRequired
		return status, nil
	}
	if !found {
		return status, nil
	}
	terminal, terminalErr := readMutationTerminal(configPath, journal.OperationID)
	if terminalErr == nil {
		if terminalMatchesJournal(terminal, journal) {
			status.Classification = mutationStatusCleanupPending
			return status, nil
		}
		status.Classification = mutationStatusJournalConflict
		status.Error = &mutationError{Code: mutationStatusJournalConflict, Message: reviewedMutationMessage(mutationStatusJournalConflict)}
		return status, nil
	}
	if !errors.Is(terminalErr, os.ErrNotExist) {
		status.Classification = mutationStatusJournalConflict
		status.Error = &mutationError{Code: mutationStatusJournalConflict, Message: reviewedMutationMessage(mutationStatusJournalConflict)}
		return status, nil
	}
	data, _, err := readExactConfig(configPath)
	if err != nil {
		status.Classification = mutationStatusJournalInvalid
		status.Error = &mutationError{Code: mutationStatusJournalInvalid, Message: reviewedMutationMessage(mutationStatusJournalInvalid)}
		return status, nil
	}
	diskHash := exactConfigHash(data)
	state, managedPID := classifyPidfile(gatewayPidfilePath())
	if state != pidfileAlive {
		status.Classification = mutationStatusRouterUnavailable
		status.Error = &mutationError{Code: mutationStatusRouterUnavailable, Message: reviewedMutationMessage(mutationStatusRouterUnavailable), Retryable: true}
		return status, nil
	}
	admin, err := fetchRoutingAdminStatus(envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr))
	if err != nil {
		status.Classification = mutationStatusRouterUnavailable
		status.Error = &mutationError{Code: mutationStatusRouterUnavailable, Message: reviewedMutationMessage(mutationStatusRouterUnavailable), Retryable: true}
		return status, nil
	}
	if admin.RouterBootID == "" || admin.ActiveGeneration == 0 || admin.ActiveConfigHash == "" || admin.DesiredConfigHash == "" ||
		admin.ConfigPath == "" || canonicalPath(admin.ConfigPath) != canonicalPath(configPath) || validateManagedRouterIdentity(admin, managedPID) != nil {
		status.Classification = mutationStatusRouterUnsupported
		status.Error = &mutationError{Code: mutationStatusRouterUnsupported, Message: reviewedMutationMessage(mutationStatusRouterUnsupported)}
		return status, nil
	}
	if capability := requiredMutationCapability(journal.Operation); capability != "" && !containsString(admin.Capabilities, capability) {
		status.Classification = mutationStatusRouterUnsupported
		status.Error = &mutationError{Code: mutationStatusRouterUnsupported, Message: reviewedMutationMessage(mutationStatusRouterUnsupported)}
		return status, nil
	}
	if diskHash != journal.PreviousConfigHash && diskHash != journal.DesiredConfigHash {
		status.Classification = mutationStatusExternalChange
		status.Error = &mutationError{Code: mutationStatusExternalChange, Message: reviewedMutationMessage(mutationStatusExternalChange)}
		return status, nil
	}
	if diskHash == journal.DesiredConfigHash {
		if admin.DesiredConfigHash == diskHash && admin.ActiveConfigHash == diskHash && mutationJournalProjectionMatches(admin, journal) {
			status.Classification = mutationStatusDesiredActive
		} else {
			status.Classification = mutationStatusDesiredPending
		}
		return status, nil
	}
	if admin.DesiredConfigHash == diskHash && admin.ActiveConfigHash == diskHash {
		status.Classification = mutationStatusPriorActive
	} else {
		status.Classification = mutationStatusPriorPending
	}
	return status, nil
}

func cleanupPendingMutation(configPath string, status routingMutationStatus) (terminalPublishResult, error) {
	journal, found, err := singleActiveMutation(configPath)
	if err != nil || !found || journal.OperationID != status.BlockingOperationID {
		return terminalPublishResult{}, fmt.Errorf("cleanup predicate changed")
	}
	terminal, err := readMutationTerminal(configPath, journal.OperationID)
	if err != nil || !terminalMatchesJournal(terminal, journal) {
		return terminalPublishResult{}, fmt.Errorf("cleanup predicate changed")
	}
	interrupted, err := hasInterruptedExactCommit(configPath)
	if err != nil || interrupted {
		return terminalPublishResult{}, fmt.Errorf("cleanup predicate changed")
	}
	published, err := publishMutationTerminal(configPath, terminal, &journal)
	if err != nil {
		return terminalPublishResult{}, err
	}
	return published, nil
}

// finalCleanupSnapshot rechecks every cleanup predicate and returns the exact
// router observation used to build the terminal record. Callers must not fetch
// router state again between this snapshot and durable publication.
func finalCleanupSnapshot(configPath string, expected routingMutationStatus) (routingMutationJournal, routingAdminStatus, error) {
	journal, found, err := singleActiveMutation(configPath)
	if err != nil || !found || journal.OperationID != expected.BlockingOperationID {
		return routingMutationJournal{}, routingAdminStatus{}, fmt.Errorf("cleanup predicate changed")
	}
	if _, err := readMutationTerminal(configPath, journal.OperationID); !errors.Is(err, os.ErrNotExist) {
		return routingMutationJournal{}, routingAdminStatus{}, fmt.Errorf("cleanup predicate changed")
	}
	interrupted, err := hasInterruptedExactCommit(configPath)
	if err != nil || interrupted {
		return routingMutationJournal{}, routingAdminStatus{}, fmt.Errorf("cleanup predicate changed")
	}
	data, _, err := readExactConfig(configPath)
	if err != nil {
		return routingMutationJournal{}, routingAdminStatus{}, fmt.Errorf("cleanup predicate changed")
	}
	diskHash := exactConfigHash(data)
	state, managedPID := classifyPidfile(gatewayPidfilePath())
	if state != pidfileAlive {
		return routingMutationJournal{}, routingAdminStatus{}, fmt.Errorf("cleanup predicate changed")
	}
	admin, err := fetchRoutingAdminStatus(envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr))
	if err != nil || admin.RouterBootID == "" || admin.ActiveGeneration == 0 ||
		admin.ActiveConfigHash == "" || admin.DesiredConfigHash == "" || admin.ConfigPath == "" ||
		canonicalPath(admin.ConfigPath) != canonicalPath(configPath) || validateManagedRouterIdentity(admin, managedPID) != nil {
		return routingMutationJournal{}, routingAdminStatus{}, fmt.Errorf("cleanup predicate changed")
	}
	if capability := requiredMutationCapability(journal.Operation); capability != "" && !containsString(admin.Capabilities, capability) {
		return routingMutationJournal{}, routingAdminStatus{}, fmt.Errorf("cleanup predicate changed")
	}
	classification := mutationStatusPriorPending
	if diskHash == journal.DesiredConfigHash {
		classification = mutationStatusDesiredPending
		if admin.DesiredConfigHash == diskHash && admin.ActiveConfigHash == diskHash && mutationJournalProjectionMatches(admin, journal) {
			classification = mutationStatusDesiredActive
		}
	} else if diskHash == journal.PreviousConfigHash {
		if admin.DesiredConfigHash == diskHash && admin.ActiveConfigHash == diskHash {
			classification = mutationStatusPriorActive
		}
	} else {
		classification = mutationStatusExternalChange
	}
	if classification != expected.Classification {
		return routingMutationJournal{}, routingAdminStatus{}, fmt.Errorf("cleanup predicate changed")
	}
	return journal, admin, nil
}

func recoverRoutingMutationLocked(configPath string) (routingMutationStatus, terminalPublishResult, error) {
	status, err := inspectRoutingMutationStatusLocked(configPath)
	if err != nil {
		return status, terminalPublishResult{}, err
	}
	switch status.Classification {
	case mutationStatusNone:
		return status, terminalPublishResult{}, nil
	case mutationStatusCleanupPending:
		published, err := cleanupPendingMutation(configPath, status)
		return status, published, err
	case mutationStatusDesiredActive, mutationStatusPriorActive:
		journal, admin, err := finalCleanupSnapshot(configPath, status)
		if err != nil {
			return status, terminalPublishResult{}, err
		}
		outcome := mutationOutcomeApplied
		if status.Classification == mutationStatusPriorActive {
			outcome = mutationOutcomeNotApplied
		}
		record := terminalFromJournal(journal, outcome, admin, "", false)
		published, err := publishMutationTerminal(configPath, record, &journal)
		if err != nil {
			return status, terminalPublishResult{}, err
		}
		return status, published, nil
	default:
		return status, terminalPublishResult{}, fmt.Errorf("%s", status.Classification)
	}
}

func parseReadOnlyMutationArgs(command string, args []string) (bool, error) {
	jsonOutput := false
	for _, arg := range args {
		if arg != "--json" || jsonOutput {
			return false, fmt.Errorf("usage: baseten-switch mutation %s [--json]", command)
		}
		jsonOutput = true
	}
	return jsonOutput, nil
}

func cmdMutationStatus(args []string) int {
	jsonOutput, err := parseReadOnlyMutationArgs("status", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	path, _ := resolveConfigPath()
	status, err := inspectRoutingMutationStatus(path)
	if err != nil {
		status = routingMutationStatus{
			Classification: mutationStatusJournalInvalid,
			Error:          &mutationError{Code: "mutation_locked", Message: "routing mutation status is temporarily unavailable", Retryable: true},
		}
	}
	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(status)
	} else if status.Classification == mutationStatusNone {
		fmt.Fprintln(os.Stdout, "no active routing mutation")
	} else {
		fmt.Fprintf(os.Stdout, "routing mutation status: %s\n", status.Classification)
	}
	if err != nil {
		return 1
	}
	return 0
}

func cmdMutationRecover(args []string) int {
	jsonOutput, err := parseReadOnlyMutationArgs("recover", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	path, _ := resolveConfigPath()
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		result := mutationRecoveryResult{Classification: mutationStatusJournalInvalid, Error: &mutationError{Code: "mutation_locked", Message: "routing mutation cleanup is temporarily unavailable", Retryable: true}}
		if jsonOutput {
			_ = json.NewEncoder(os.Stdout).Encode(result)
		} else {
			fmt.Fprintln(os.Stderr, result.Error.Message)
		}
		return 1
	}
	defer lock.close()
	status, published, recoverErr := recoverRoutingMutationLocked(path)
	terminal := published.Record
	result := mutationRecoveryResult{
		OK:                 recoverErr == nil,
		Classification:     status.Classification,
		OperationID:        status.BlockingOperationID,
		Outcome:            terminal.Outcome,
		Applied:            terminal.Outcome == mutationOutcomeApplied,
		IdentityStrength:   terminal.IdentityStrength,
		RequestFingerprint: terminal.RequestFingerprint,
		CleanupPending:     published.CleanupPending,
	}
	if recoverErr != nil {
		code := recoverErr.Error()
		switch code {
		case mutationStatusRouterUnavailable, mutationStatusRouterUnsupported, mutationStatusExternalChange,
			mutationStatusCommitRecoveryRequired, mutationStatusJournalInvalid, mutationStatusJournalConflict,
			mutationStatusDesiredPending, mutationStatusPriorPending:
		default:
			code = "cleanup_predicate_changed"
		}
		result.Error = &mutationError{Code: code, Message: reviewedMutationMessage(code), Retryable: code == mutationStatusRouterUnavailable || code == "cleanup_predicate_changed"}
	}
	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(result)
	} else if result.OK {
		if result.OperationID == "" {
			fmt.Fprintln(os.Stdout, "no active routing mutation")
		} else {
			fmt.Fprintf(os.Stdout, "routing mutation %s cleanup complete\n", result.OperationID)
		}
	} else {
		fmt.Fprintln(os.Stderr, result.Error.Message)
	}
	if recoverErr != nil {
		return 1
	}
	return 0
}
