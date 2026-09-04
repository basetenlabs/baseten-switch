package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
)

func installTerminalPublicationSeams(t *testing.T) {
	t.Helper()
	oldLink := mutationTerminalLink
	oldRemove := mutationTerminalRemove
	oldSync := mutationTerminalSync
	t.Cleanup(func() {
		mutationTerminalLink = oldLink
		mutationTerminalRemove = oldRemove
		mutationTerminalSync = oldSync
	})
}

func legacyJournalForTest(t *testing.T, path, operationID string) routingMutationJournal {
	t.Helper()
	prior, mode, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := previewExactConfigEdit(path, prior, mode, func(editPath string) error {
		return config.SetGlobalRoutingEnabled(editPath, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	return routingMutationJournal{
		Version:             mutationJournalVersion,
		OperationID:         operationID,
		Operation:           "set_global_routing",
		ConfigPath:          path,
		Requested:           false,
		PreviousRouting:     true,
		PreviousConfig:      prior,
		PreviousMode:        uint32(mode),
		PreviousConfigHash:  exactConfigHash(prior),
		DesiredConfigHash:   exactConfigHash(desired),
		PreviousActiveToken: "boot:4",
		CreatedAt:           time.Now().UTC(),
	}
}

func TestRoutingMutationPublishesTerminalAndReplaysExactRequest(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	priorHash := exactFileHash(t, path)
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var signals atomic.Int32
	signalRouter = func(int) error {
		signals.Add(1)
		return nil
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		if signals.Load() == 0 {
			return liveRoutingStatus(path, priorHash, true, 4), nil
		}
		return liveRoutingStatus(path, exactFileHash(t, path), false, 5), nil
	}
	args := []string{"--json", "--operation-id", "terminal-replay", "--if-config-hash", priorHash}
	var first strings.Builder
	if rc := runSwitch("off", args, &first); rc != 0 {
		t.Fatalf("first rc = %d: %s", rc, first.String())
	}
	record, err := readMutationTerminal(path, "terminal-replay")
	if err != nil {
		t.Fatal(err)
	}
	if record.Outcome != mutationOutcomeApplied || record.Surface != mutationSurfaceSwitch ||
		record.IdentityStrength != mutationIdentityExact || record.RequestFingerprint == "" {
		t.Fatalf("terminal = %+v", record)
	}
	if _, err := os.Stat(mutationJournalPath(path, "terminal-replay")); !os.IsNotExist(err) {
		t.Fatalf("active journal remains: %v", err)
	}
	terminalBytes, err := os.ReadFile(mutationTerminalPath(path, "terminal-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(terminalBytes), path) || strings.Contains(string(terminalBytes), globalRoutingFixtureYAML) {
		t.Fatalf("terminal retained config path or bytes: %s", terminalBytes)
	}
	firstSignals := signals.Load()
	var replay strings.Builder
	if rc := runSwitch("off", args, &replay); rc != 0 {
		t.Fatalf("replay rc = %d: %s", rc, replay.String())
	}
	if signals.Load() != firstSignals {
		t.Fatal("terminal replay signaled the router")
	}
	replayed := decodeMutationResult(t, replay.String())
	if !replayed.OK || replayed.Outcome != mutationOutcomeApplied || replayed.RequestFingerprint != record.RequestFingerprint {
		t.Fatalf("replay = %+v", replayed)
	}

	var conflict strings.Builder
	conflictArgs := []string{"--json", "--operation-id", "terminal-replay"}
	if rc := runSwitch("off", conflictArgs, &conflict); rc != 1 {
		t.Fatalf("conflict rc = %d: %s", rc, conflict.String())
	}
	conflicted := decodeMutationResult(t, conflict.String())
	if conflicted.Error == nil || conflicted.Error.Code != "operation_id_conflict" {
		t.Fatalf("conflict = %+v", conflicted)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	var missingConfigReplay strings.Builder
	if rc := runSwitch("off", args, &missingConfigReplay); rc != 0 {
		t.Fatalf("pre-config replay rc = %d: %s", rc, missingConfigReplay.String())
	}
}

func publishExactUnchangedTerminalForTest(t *testing.T, path, client string, opts mutationOptions, spec journaledMutationSpec) routingMutationJournal {
	t.Helper()
	prior, _, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := mutationRequestFingerprint(path, opts, spec)
	if err != nil {
		t.Fatal(err)
	}
	journal := routingMutationJournal{
		Version: mutationJournalVersion, OperationID: opts.OperationID, Operation: spec.Operation,
		Surface: spec.Surface, ConfigPath: path, Requested: spec.Requested, RequestedTarget: spec.RequestedTarget,
		Client: client, Key: spec.Key, PreviousConfig: prior, PreviousMode: 0o600,
		PreviousConfigHash: exactConfigHash(prior), DesiredConfigHash: exactConfigHash(prior),
		RequestFingerprint: fingerprint, CreatedAt: time.Now().UTC(),
	}
	record := terminalFromJournal(journal, mutationOutcomeUnchanged, routingAdminStatus{}, "", false)
	if _, err := publishMutationTerminal(path, record, nil); err != nil {
		t.Fatal(err)
	}
	return journal
}

func TestTerminalReplayPreservesCleanupPendingContract(t *testing.T) {
	installRoutingMutationSeams(t)
	setup := func(t *testing.T, operationID string) (string, mutationOptions, journaledMutationSpec) {
		t.Helper()
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		opts := mutationOptions{JSON: true, OperationID: operationID, hasOperationID: true}
		spec := journaledMutationSpec{Operation: "set_global_routing", Surface: mutationSurfaceSwitch, Requested: true}
		journal := publishExactUnchangedTerminalForTest(t, path, "", opts, spec)
		if err := writeMutationJournal(journal); err != nil {
			t.Fatal(err)
		}
		return path, opts, spec
	}

	t.Run("original command", func(t *testing.T) {
		path, opts, spec := setup(t, "cleanup-pending-original")
		var out strings.Builder
		replayed, rc := preflightTerminalReplay(path, opts, spec, &out)
		if !replayed || rc != 0 {
			t.Fatalf("replayed=%v rc=%d out=%s", replayed, rc, out.String())
		}
		result := decodeMutationResult(t, out.String())
		if !result.OK || !result.CleanupPending || result.Outcome != mutationOutcomeUnchanged {
			t.Fatalf("result=%+v", result)
		}
		if _, err := os.Stat(mutationJournalPath(path, opts.OperationID)); err != nil {
			t.Fatalf("active journal was not preserved: %v", err)
		}
	})

	t.Run("reconcile", func(t *testing.T) {
		path, opts, _ := setup(t, "cleanup-pending-reconcile")
		stdout, rc := captureStdout(t, func() int {
			return cmdMutation([]string{"reconcile", opts.OperationID, "--json"})
		})
		if rc != 0 {
			t.Fatalf("reconcile rc=%d out=%s", rc, stdout)
		}
		result := decodeMutationResult(t, stdout)
		if !result.OK || !result.CleanupPending || result.Outcome != mutationOutcomeUnchanged {
			t.Fatalf("result=%+v", result)
		}
		if _, err := os.Stat(mutationJournalPath(path, opts.OperationID)); err != nil {
			t.Fatalf("active journal was not preserved: %v", err)
		}
	})
}

func TestAdapterTerminalReplayPrecedesConfigLoading(t *testing.T) {
	installRoutingMutationSeams(t)
	t.Run("claude route", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		opts, _, err := parseMutationOptions([]string{"--json", "--operation-id", "claude-preload"})
		if err != nil {
			t.Fatal(err)
		}
		spec := journaledMutationSpec{Operation: "set_claude_route", Surface: mutationSurfaceClaude, Client: "claude-code", Key: "sonnet", RequestedTarget: "native"}
		publishExactUnchangedTerminalForTest(t, path, spec.Client, opts, spec)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		stdout, rc := captureStdout(t, func() int {
			return cmdClaude([]string{"route", "sonnet", "native", "--json", "--operation-id", "claude-preload"})
		})
		if rc != 0 {
			t.Fatalf("rc = %d: %s", rc, stdout)
		}
		result := decodeMutationResult(t, stdout)
		if !result.OK || result.Outcome != mutationOutcomeUnchanged || result.Client != "claude-code" {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("codex route", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		opts, _, err := parseMutationOptions([]string{"--json", "--operation-id", "codex-preload"})
		if err != nil {
			t.Fatal(err)
		}
		spec := journaledMutationSpec{Operation: "set_codex_route", Surface: mutationSurfaceCodex, Client: "codex", Key: "default_model", RequestedTarget: "example/model"}
		publishExactUnchangedTerminalForTest(t, path, spec.Client, opts, spec)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		stdout, rc := captureStdout(t, func() int {
			return cmdCodex([]string{"route", "example/model", "--json", "--operation-id", "codex-preload"})
		})
		if rc != 0 {
			t.Fatalf("rc = %d: %s", rc, stdout)
		}
		result := decodeMutationResult(t, stdout)
		if !result.OK || result.Outcome != mutationOutcomeUnchanged || result.Client != "codex" {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestAdapterTerminalReplayUsesSelectedFallbackClientIdentity(t *testing.T) {
	installRoutingMutationSeams(t)
	fixture := strings.ReplaceAll(globalRoutingFixtureYAML, "claude-code", "anthropic-fallback") + `
  - name: openai-fallback
    enabled: true
    bind_addr: 127.0.0.1:18082
    protocol_shape: openai
    default_model: example/model
`
	path := writeSwitchFixture(t, fixture)

	t.Run("claude reasoning", func(t *testing.T) {
		opts := mutationOptions{JSON: true, OperationID: "fallback-claude-reasoning", hasOperationID: true}
		spec := journaledMutationSpec{
			Operation: "set_model_reasoning", Surface: mutationSurfaceClaude, Client: "anthropic-fallback",
			Key: "example/model", RequestedTarget: "default",
		}
		publishExactUnchangedTerminalForTest(t, path, spec.Client, opts, spec)
		stdout, rc := captureStdout(t, func() int {
			return cmdClaude([]string{
				"reasoning", "baseten", "example/model", "default",
				"--json", "--operation-id", opts.OperationID,
			})
		})
		if rc != 0 {
			t.Fatalf("rc=%d out=%s", rc, stdout)
		}
		result := decodeMutationResult(t, stdout)
		if result.Client != spec.Client || result.Outcome != mutationOutcomeUnchanged {
			t.Fatalf("result=%+v", result)
		}

		conflict, conflictRC := captureStdout(t, func() int {
			return cmdCodex([]string{
				"reasoning", "baseten", "example/model", "default",
				"--json", "--operation-id", opts.OperationID,
			})
		})
		if conflictRC != 1 || decodeMutationResult(t, conflict).Error.Code != "operation_id_conflict" {
			t.Fatalf("cross-adapter rc=%d out=%s", conflictRC, conflict)
		}
	})

	t.Run("codex reasoning", func(t *testing.T) {
		opts := mutationOptions{JSON: true, OperationID: "fallback-codex-reasoning", hasOperationID: true}
		spec := journaledMutationSpec{
			Operation: "set_model_reasoning", Surface: mutationSurfaceCodex, Client: "openai-fallback",
			Key: "example/model", RequestedTarget: "default",
		}
		publishExactUnchangedTerminalForTest(t, path, spec.Client, opts, spec)
		stdout, rc := captureStdout(t, func() int {
			return cmdCodex([]string{
				"reasoning", "baseten", "example/model", "default",
				"--json", "--operation-id", opts.OperationID,
			})
		})
		if rc != 0 {
			t.Fatalf("rc=%d out=%s", rc, stdout)
		}
		result := decodeMutationResult(t, stdout)
		if result.Client != spec.Client || result.Outcome != mutationOutcomeUnchanged {
			t.Fatalf("result=%+v", result)
		}
	})
}

func TestCustomFallbackTerminalReplayIsConfigIndependentAndSurfaceBound(t *testing.T) {
	installRoutingMutationSeams(t)
	reasoningArgs := func(operationID string) []string {
		return []string{
			"reasoning", "baseten", "example/model", "default",
			"--json", "--operation-id", operationID,
		}
	}
	setup := func(t *testing.T, operationID, surface, client string) string {
		t.Helper()
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		opts := mutationOptions{JSON: true, OperationID: operationID, hasOperationID: true}
		spec := journaledMutationSpec{
			Operation: "set_model_reasoning", Surface: surface, Client: client,
			Key: "example/model", RequestedTarget: "default",
		}
		publishExactUnchangedTerminalForTest(t, path, client, opts, spec)
		return path
	}

	t.Run("claude missing config", func(t *testing.T) {
		const operationID = "fallback-claude-missing"
		path := setup(t, operationID, mutationSurfaceClaude, "anthropic-fallback")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		stdout, rc := captureStdout(t, func() int { return cmdClaude(reasoningArgs(operationID)) })
		result := decodeMutationResult(t, stdout)
		if rc != 0 || !result.OK || result.Client != "anthropic-fallback" || result.Outcome != mutationOutcomeUnchanged {
			t.Fatalf("rc=%d result=%+v", rc, result)
		}
	})

	t.Run("codex malformed config", func(t *testing.T) {
		const operationID = "fallback-codex-malformed"
		path := setup(t, operationID, mutationSurfaceCodex, "openai-fallback")
		if err := os.WriteFile(path, []byte("global: [\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, rc := captureStdout(t, func() int { return cmdCodex(reasoningArgs(operationID)) })
		result := decodeMutationResult(t, stdout)
		if rc != 0 || !result.OK || result.Client != "openai-fallback" || result.Outcome != mutationOutcomeUnchanged {
			t.Fatalf("rc=%d result=%+v", rc, result)
		}
	})

	t.Run("cross surface conflict without config", func(t *testing.T) {
		const operationID = "fallback-cross-surface"
		path := setup(t, operationID, mutationSurfaceClaude, "shared-fallback")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		stdout, rc := captureStdout(t, func() int { return cmdCodex(reasoningArgs(operationID)) })
		result := decodeMutationResult(t, stdout)
		if rc != 1 || result.Error == nil || result.Error.Code != "operation_id_conflict" {
			t.Fatalf("rc=%d result=%+v", rc, result)
		}
	})
}

func TestMutationStatusAndCleanupRecoverDesiredActive(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	var mutationOut strings.Builder
	if rc := runSwitch("off", []string{"--json", "--operation-id", "cleanup-applied"}, &mutationOut); rc != 0 {
		t.Fatalf("offline mutation rc = %d: %s", rc, mutationOut.String())
	}
	desiredHash := exactFileHash(t, path)
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		return liveRoutingStatus(path, desiredHash, false, 8), nil
	}
	status, err := inspectRoutingMutationStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if status.Classification != mutationStatusDesiredActive || status.BlockingOperationID != "cleanup-applied" {
		t.Fatalf("status = %+v", status)
	}
	stdout, rc := captureStdout(t, func() int { return cmdMutation([]string{"recover", "--json"}) })
	if rc != 0 {
		t.Fatalf("recover rc = %d: %s", rc, stdout)
	}
	var recovered mutationRecoveryResult
	if err := json.Unmarshal([]byte(stdout), &recovered); err != nil {
		t.Fatal(err)
	}
	if !recovered.OK || recovered.Outcome != mutationOutcomeApplied || !recovered.Applied {
		t.Fatalf("recover = %+v", recovered)
	}
	if _, err := os.Stat(mutationJournalPath(path, "cleanup-applied")); !os.IsNotExist(err) {
		t.Fatalf("active journal remains: %v", err)
	}
	status, err = inspectRoutingMutationStatus(path)
	if err != nil || status.Classification != mutationStatusNone {
		t.Fatalf("post-cleanup status = %+v err=%v", status, err)
	}
}

func TestCleanupPriorActiveRecordsNotAppliedNotRollback(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	journal := legacyJournalForTest(t, path, "pre-cas-crash")
	if err := writeMutationJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		return liveRoutingStatus(path, journal.PreviousConfigHash, true, 4), nil
	}
	status, err := inspectRoutingMutationStatus(path)
	if err != nil || status.Classification != mutationStatusPriorActive {
		t.Fatalf("status = %+v err=%v", status, err)
	}
	stdout, rc := captureStdout(t, func() int { return cmdMutation([]string{"recover", "--json"}) })
	if rc != 0 {
		t.Fatalf("recover rc = %d: %s", rc, stdout)
	}
	record, err := readMutationTerminal(path, journal.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Outcome != mutationOutcomeNotApplied || record.ErrorCode == "activation_failed_rolled_back" || record.IdentityStrength != mutationIdentityLegacy {
		t.Fatalf("terminal = %+v", record)
	}
}

func TestCleanupPendingUsesTerminalWithoutRouter(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	journal := legacyJournalForTest(t, path, "terminal-residue")
	if err := writeMutationJournal(journal); err != nil {
		t.Fatal(err)
	}
	active := liveRoutingStatus(path, journal.PreviousConfigHash, true, 5)
	record := terminalFromJournal(journal, mutationOutcomeNotApplied, active, "", false)
	if _, err := publishMutationTerminal(path, record, nil); err != nil {
		t.Fatal(err)
	}
	status, err := inspectRoutingMutationStatus(path)
	if err != nil || status.Classification != mutationStatusCleanupPending {
		t.Fatalf("status = %+v err=%v", status, err)
	}
	stdout, rc := captureStdout(t, func() int { return cmdMutation([]string{"recover", "--json"}) })
	if rc != 0 {
		t.Fatalf("recover rc = %d: %s", rc, stdout)
	}
	if _, err := os.Stat(mutationJournalPath(path, journal.OperationID)); !os.IsNotExist(err) {
		t.Fatalf("cleanup residue remains: %v", err)
	}
}

func TestMutationStatusClassifiesPriorAndDesiredPending(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	journal := legacyJournalForTest(t, path, "pending-matrix")
	if err := writeMutationJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		return liveRoutingStatus(path, journal.DesiredConfigHash, false, 5), nil
	}
	status, err := inspectRoutingMutationStatus(path)
	if err != nil || status.Classification != mutationStatusPriorPending {
		t.Fatalf("prior pending = %+v err=%v", status, err)
	}
	prior, mode, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := previewExactConfigEdit(path, prior, mode, func(editPath string) error {
		return config.SetGlobalRoutingEnabled(editPath, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, desired, mode); err != nil {
		t.Fatal(err)
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		return liveRoutingStatus(path, journal.PreviousConfigHash, true, 6), nil
	}
	status, err = inspectRoutingMutationStatus(path)
	if err != nil || status.Classification != mutationStatusDesiredPending {
		t.Fatalf("desired pending = %+v err=%v", status, err)
	}
}

func TestMutationStatusAuthorityPrecedesExternalChange(t *testing.T) {
	t.Run("router unavailable", func(t *testing.T) {
		installRoutingMutationSeams(t)
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		journal := legacyJournalForTest(t, path, "external-without-router")
		if err := writeMutationJournal(journal); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(journal.PreviousConfig, []byte("# external\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		status, err := inspectRoutingMutationStatus(path)
		if err != nil || status.Classification != mutationStatusRouterUnavailable {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	})

	t.Run("router unsupported", func(t *testing.T) {
		installRoutingMutationSeams(t)
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		journal := legacyJournalForTest(t, path, "external-unsupported-router")
		if err := writeMutationJournal(journal); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(journal.PreviousConfig, []byte("# external\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
			t.Fatal(err)
		}
		fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
			status := liveRoutingStatus(path, journal.PreviousConfigHash, true, 4)
			status.RouterBootID = ""
			return status, nil
		}
		status, err := inspectRoutingMutationStatus(path)
		if err != nil || status.Classification != mutationStatusRouterUnsupported {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	})
}

func TestMutationStatusClassifiesMultipleActiveJournalsAsConflict(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	first := legacyJournalForTest(t, path, "active-one")
	if err := writeMutationJournal(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.OperationID = "active-two"
	data, err := json.MarshalIndent(second, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mutationJournalPath(path, second.OperationID), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := inspectRoutingMutationStatus(path)
	if err != nil || status.Classification != mutationStatusJournalConflict || status.Error == nil || status.Error.Code != mutationStatusJournalConflict {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestMutationTerminalGCRetainsYoungAndFutureRecords(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	now := time.Now().UTC()
	for _, item := range []struct {
		id   string
		when time.Time
	}{
		{id: "old-terminal", when: now.Add(-31 * 24 * time.Hour)},
		{id: "young-terminal", when: now.Add(-29 * 24 * time.Hour)},
		{id: "future-terminal", when: now.Add(time.Hour)},
	} {
		journal := legacyJournalForTest(t, path, item.id)
		record := terminalFromJournal(journal, mutationOutcomeUnchanged, routingAdminStatus{}, "", false)
		record.DesiredConfigHash = record.PreviousConfigHash
		record.PreviousActiveToken = ""
		future := item.when.After(now)
		if future {
			record.CompletedAt = now
		} else {
			record.CompletedAt = item.when
		}
		if _, err := publishMutationTerminal(path, record, nil); err != nil {
			t.Fatal(err)
		}
		if future {
			record.CompletedAt = item.when
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mutationTerminalPath(path, item.id), append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := gcMutationTerminals(path, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mutationTerminalPath(path, "old-terminal")); !os.IsNotExist(err) {
		t.Fatalf("old terminal was not collected: %v", err)
	}
	for _, id := range []string{"young-terminal", "future-terminal"} {
		if _, err := os.Stat(mutationTerminalPath(path, id)); err != nil {
			t.Fatalf("%s was removed: %v", id, err)
		}
	}
	status, err := inspectRoutingMutationStatus(path)
	if err != nil || status.Classification != mutationStatusJournalInvalid {
		t.Fatalf("future terminal status = %+v err=%v", status, err)
	}
}

func TestMutationTerminalPublicationCrashSeams(t *testing.T) {
	installRoutingMutationSeams(t)
	t.Run("before link preserves active", func(t *testing.T) {
		installTerminalPublicationSeams(t)
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		journal := legacyJournalForTest(t, path, "before-link")
		if err := writeMutationJournal(journal); err != nil {
			t.Fatal(err)
		}
		record := terminalFromJournal(journal, mutationOutcomeNotApplied,
			liveRoutingStatus(path, journal.PreviousConfigHash, true, 5), "", false)
		mutationTerminalLink = func(string, string) error { return errors.New("injected pre-publication stop") }
		if _, err := publishMutationTerminal(path, record, &journal); err == nil {
			t.Fatal("publication unexpectedly succeeded")
		}
		if _, err := os.Stat(mutationJournalPath(path, journal.OperationID)); err != nil {
			t.Fatalf("active journal was not preserved: %v", err)
		}
		if _, err := os.Stat(mutationTerminalPath(path, journal.OperationID)); !os.IsNotExist(err) {
			t.Fatalf("terminal unexpectedly exists: %v", err)
		}
	})

	t.Run("after link before directory sync preserves active", func(t *testing.T) {
		installTerminalPublicationSeams(t)
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		journal := legacyJournalForTest(t, path, "before-completed-sync")
		if err := writeMutationJournal(journal); err != nil {
			t.Fatal(err)
		}
		record := terminalFromJournal(journal, mutationOutcomeNotApplied,
			liveRoutingStatus(path, journal.PreviousConfigHash, true, 5), "", false)
		realSync := mutationTerminalSync
		mutationTerminalSync = func(dir string) error {
			if dir == mutationCompletedDir(path) {
				return errors.New("injected completed sync stop")
			}
			return realSync(dir)
		}
		if _, err := publishMutationTerminal(path, record, &journal); err == nil {
			t.Fatal("publication unexpectedly succeeded")
		}
		if _, err := os.Stat(mutationJournalPath(path, journal.OperationID)); err != nil {
			t.Fatalf("active journal was not preserved: %v", err)
		}
		if _, err := os.Stat(mutationTerminalPath(path, journal.OperationID)); err != nil {
			t.Fatalf("published link should remain for idempotent retry: %v", err)
		}
		mutationTerminalSync = realSync
		lock, err := acquireConfigMutationLock(path)
		if err != nil {
			t.Fatal(err)
		}
		_, recovered, err := recoverRoutingMutationLocked(path)
		lock.close()
		if err != nil || recovered.Record.OperationID != journal.OperationID {
			t.Fatalf("idempotent recovery record=%+v err=%v", recovered, err)
		}
	})

	t.Run("after terminal durability reports cleanup pending", func(t *testing.T) {
		installTerminalPublicationSeams(t)
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		journal := legacyJournalForTest(t, path, "active-unlink-stop")
		if err := writeMutationJournal(journal); err != nil {
			t.Fatal(err)
		}
		record := terminalFromJournal(journal, mutationOutcomeNotApplied,
			liveRoutingStatus(path, journal.PreviousConfigHash, true, 5), "", false)
		realRemove := mutationTerminalRemove
		mutationTerminalRemove = func(target string) error {
			if target == mutationJournalPath(path, journal.OperationID) {
				return errors.New("injected active unlink stop")
			}
			return realRemove(target)
		}
		published, err := publishMutationTerminal(path, record, &journal)
		if err != nil || !published.CleanupPending {
			t.Fatalf("publication=%+v err=%v", published, err)
		}
		if _, err := os.Stat(mutationTerminalPath(path, journal.OperationID)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(mutationJournalPath(path, journal.OperationID)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMutationTerminalSameIDIdempotenceAndConflict(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	journal := legacyJournalForTest(t, path, "same-terminal-id")
	record := terminalFromJournal(journal, mutationOutcomeNotApplied,
		liveRoutingStatus(path, journal.PreviousConfigHash, true, 5), "", false)
	first, err := publishMutationTerminal(path, record, nil)
	if err != nil {
		t.Fatal(err)
	}
	retry := record
	retry.CompletedAt = time.Now().UTC()
	second, err := publishMutationTerminal(path, retry, nil)
	if err != nil || !second.Record.CompletedAt.Equal(first.Record.CompletedAt) {
		t.Fatalf("idempotent retry=%+v err=%v", second, err)
	}
	conflict := record
	conflict.Outcome = mutationOutcomeApplied
	conflict.ActiveConfigHash = conflict.DesiredConfigHash
	if _, err := publishMutationTerminal(path, conflict, nil); err == nil || !strings.Contains(err.Error(), "terminal_conflict") {
		t.Fatalf("divergent same-ID publication error = %v", err)
	}
}

func TestMutationTerminalConcurrentSameIDPublication(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	journal := legacyJournalForTest(t, path, "concurrent-terminal-id")
	record := terminalFromJournal(journal, mutationOutcomeNotApplied,
		liveRoutingStatus(path, journal.PreviousConfigHash, true, 5), "", false)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			ready.Done()
			<-start
			_, err := publishMutationTerminal(path, record, nil)
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent idempotent publication failed: %v", err)
		}
	}
	stored, err := readMutationTerminal(path, journal.OperationID)
	if err != nil || !terminalRecordsEqual(stored, record) {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}

func TestMutationRecoverySizeAndScanLimits(t *testing.T) {
	installRoutingMutationSeams(t)
	t.Run("oversize active journal", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		if err := os.Mkdir(mutationJournalDir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		activePath := mutationJournalPath(path, "oversize-active")
		file, err := os.OpenFile(activePath, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(mutationJournalReadLimit + 1); err != nil {
			t.Fatal(err)
		}
		file.Close()
		status, err := inspectRoutingMutationStatus(path)
		if err != nil || status.Classification != mutationStatusJournalInvalid {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	})

	t.Run("oversize terminal", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		if err := os.MkdirAll(mutationCompletedDir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(mutationJournalDir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		terminalPath := mutationTerminalPath(path, "oversize-terminal")
		file, err := os.OpenFile(terminalPath, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(mutationTerminalReadLimit + 1); err != nil {
			t.Fatal(err)
		}
		file.Close()
		status, err := inspectRoutingMutationStatus(path)
		if err != nil || status.Classification != mutationStatusJournalInvalid {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	})

	t.Run("completed scan bound", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		if err := os.MkdirAll(mutationCompletedDir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(mutationJournalDir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= mutationCompletedScanMax; i++ {
			name := filepath.Join(mutationCompletedDir(path), newOperationID()+".json")
			if err := os.WriteFile(name, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		status, err := inspectRoutingMutationStatus(path)
		if err != nil || status.Classification != mutationStatusJournalInvalid {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	})
}

func TestLegacyActiveV1AndCompletedDirectoryCompatibility(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	journal := legacyJournalForTest(t, path, "legacy-v1")
	if journal.Version != 1 {
		t.Fatalf("legacy journal version=%d, want 1", journal.Version)
	}
	if err := writeMutationJournal(journal); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mutationJournalPath(path, journal.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "request_fingerprint") || strings.Contains(string(data), "if_config_hash") ||
		strings.Contains(string(data), `"surface"`) {
		t.Fatalf("legacy fixture unexpectedly contains new fields: %s", data)
	}
	readBack, err := readMutationJournal(path, journal.OperationID)
	if err != nil || readBack.RequestFingerprint != "" {
		t.Fatalf("legacy read=%+v err=%v", readBack, err)
	}
	if err := os.Mkdir(mutationCompletedDir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if operationID, err := unfinishedMutationOperation(path); err != nil || operationID != journal.OperationID {
		t.Fatalf("old scanner operation=%q err=%v", operationID, err)
	}
}

func TestMutationHelpDocumentsStatusRecoverAndReconcile(t *testing.T) {
	var out strings.Builder
	if !printCommandHelp(&out, "mutation") {
		t.Fatal("mutation help unavailable")
	}
	for _, command := range []string{"mutation status", "mutation recover", "mutation reconcile"} {
		if !strings.Contains(out.String(), command) {
			t.Fatalf("mutation help missing %q:\n%s", command, out.String())
		}
	}
	if filepath.Base(mutationCompletedDir("/tmp/example.yaml")) != "completed" {
		t.Fatal("completed directory path changed unexpectedly")
	}
}

func TestRecoveryRevalidatesAuthoritativeCleanupSnapshot(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	journal := legacyJournalForTest(t, path, "cleanup-snapshot-change")
	if err := writeMutationJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var calls int
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		calls++
		status := liveRoutingStatus(path, journal.PreviousConfigHash, true, 4)
		if calls > 1 {
			status.ActiveConfigHash = domainHash("changed-active", "config")
		}
		return status, nil
	}
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, recoverErr := recoverRoutingMutationLocked(path)
	lock.close()
	if recoverErr == nil || !strings.Contains(recoverErr.Error(), "cleanup predicate changed") {
		t.Fatalf("recovery error = %v", recoverErr)
	}
	if _, err := os.Stat(mutationJournalPath(path, journal.OperationID)); err != nil {
		t.Fatalf("active journal was removed: %v", err)
	}
	if _, err := os.Stat(mutationTerminalPath(path, journal.OperationID)); !os.IsNotExist(err) {
		t.Fatalf("terminal unexpectedly published: %v", err)
	}
}

func TestRecoveryPublishesFromFinalValidatedSnapshot(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	journal := legacyJournalForTest(t, path, "cleanup-final-snapshot")
	if err := writeMutationJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var calls int
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		calls++
		generation := uint64(4 + calls - 1)
		if calls > 2 {
			generation = 99
		}
		return liveRoutingStatus(path, journal.PreviousConfigHash, true, generation), nil
	}
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		t.Fatal(err)
	}
	_, published, recoverErr := recoverRoutingMutationLocked(path)
	lock.close()
	if recoverErr != nil {
		t.Fatal(recoverErr)
	}
	if calls != 2 || !strings.HasSuffix(published.Record.ActiveToken, ":5") {
		t.Fatalf("admin calls=%d terminal=%+v", calls, published.Record)
	}
}

func TestDirectMutationReadersRejectSymlinkedStateDirectories(t *testing.T) {
	installRoutingMutationSeams(t)
	t.Run("journal", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		journal := legacyJournalForTest(t, path, "symlink-journal-dir")
		if err := writeMutationJournal(journal); err != nil {
			t.Fatal(err)
		}
		dir := mutationJournalDir(path)
		realDir := dir + ".real"
		if err := os.Rename(dir, realDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDir, dir); err != nil {
			t.Fatal(err)
		}
		if _, err := readMutationJournal(path, journal.OperationID); err == nil {
			t.Fatal("journal reader accepted a symlinked parent directory")
		}
	})
	t.Run("terminal", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		journal := legacyJournalForTest(t, path, "symlink-terminal-dir")
		record := terminalFromJournal(journal, mutationOutcomeUnchanged, routingAdminStatus{}, "", false)
		record.DesiredConfigHash = record.PreviousConfigHash
		record.PreviousActiveToken = ""
		if _, err := publishMutationTerminal(path, record, nil); err != nil {
			t.Fatal(err)
		}
		dir := mutationCompletedDir(path)
		realDir := dir + ".real"
		if err := os.Rename(dir, realDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDir, dir); err != nil {
			t.Fatal(err)
		}
		if _, err := readMutationTerminal(path, journal.OperationID); err == nil {
			t.Fatal("terminal reader accepted a symlinked parent directory")
		}
	})
}

func TestExactJournalFingerprintIsRecomputed(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	prior, mode, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := mutationOptions{OperationID: "fingerprint-tamper", hasOperationID: true, IfConfigHash: exactConfigHash(prior), hasConfigHash: true}
	spec := journaledMutationSpec{Operation: "set_claude_route", Surface: mutationSurfaceClaude, Client: "claude-code", Key: "opus", RequestedTarget: "native"}
	fingerprint, err := mutationRequestFingerprint(path, opts, spec)
	if err != nil {
		t.Fatal(err)
	}
	journal := routingMutationJournal{
		Version: mutationJournalVersion, OperationID: opts.OperationID, Operation: spec.Operation,
		Surface: spec.Surface, ConfigPath: path, Client: spec.Client, Key: spec.Key, RequestedTarget: spec.RequestedTarget,
		PreviousConfig: prior, PreviousMode: uint32(mode), PreviousConfigHash: exactConfigHash(prior),
		DesiredConfigHash: exactConfigHash(prior), IfConfigHash: opts.IfConfigHash,
		RequestFingerprint: fingerprint, CreatedAt: time.Now().UTC(),
	}
	if err := writeMutationJournal(journal); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mutationJournalPath(path, opts.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"client": "claude-code"`), []byte(`"client": "codex"`), 1)
	if err := os.WriteFile(mutationJournalPath(path, opts.OperationID), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMutationJournal(path, opts.OperationID); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("tampered exact journal error = %v", err)
	}
}

func TestReplayDoesNotAdoptTerminalClient(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	opts := mutationOptions{JSON: true, OperationID: "cross-client", hasOperationID: true}
	codex := journaledMutationSpec{Operation: "set_model_reasoning", Surface: mutationSurfaceCodex, Client: "codex", Key: "example/model", RequestedTarget: "default"}
	publishExactUnchangedTerminalForTest(t, path, codex.Client, opts, codex)
	claude := codex
	claude.Client = "claude-code"
	result, found, rc, err := replayTerminalForRequest(path, opts, claude)
	if !found || rc != 1 || err == nil || err.Error() != "operation_id_conflict" || result.Client != "codex" {
		t.Fatalf("cross-client replay result=%+v found=%v rc=%d err=%v", result, found, rc, err)
	}
}

func TestMutationStatusFindsCommitArtifactWithoutActiveJournal(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	dir := filepath.Join(filepath.Dir(path), exactCommitDirPrefix(path)+"orphan")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := inspectRoutingMutationStatus(path)
	if err != nil || status.Classification != mutationStatusCommitRecoveryRequired || status.BlockingOperationID != "" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestTerminalReaderRejectsTrailingAndInconsistentJSON(t *testing.T) {
	installRoutingMutationSeams(t)
	for _, testCase := range []struct {
		name   string
		mutate func(*mutationTerminalRecord)
		extra  []byte
	}{
		{name: "trailing", extra: []byte("{}\n")},
		{name: "future completion", mutate: func(record *mutationTerminalRecord) { record.CompletedAt = time.Now().UTC().Add(time.Hour) }},
		{name: "requested presence", mutate: func(record *mutationTerminalRecord) { record.RequestedPresent = true }},
		{name: "requested value", mutate: func(record *mutationTerminalRecord) { record.Requested = true }},
		{name: "command surface", mutate: func(record *mutationTerminalRecord) { record.Surface = mutationSurfaceClaude }},
		{name: "unchanged hashes", mutate: func(record *mutationTerminalRecord) { record.DesiredConfigHash = domainHash("other", "value") }},
		{name: "malformed previous token", mutate: func(record *mutationTerminalRecord) { record.PreviousActiveToken = "not-a-token" }},
		{name: "token without hash", mutate: func(record *mutationTerminalRecord) {
			record.PreviousActiveToken = "boot:1"
			record.ActiveToken = "boot:1"
		}},
		{name: "unchanged token mismatch", mutate: func(record *mutationTerminalRecord) {
			record.ActiveConfigHash = record.DesiredConfigHash
			record.PreviousActiveToken = "boot:1"
			record.ActiveToken = "boot:2"
		}},
		{name: "rejected error code", mutate: func(record *mutationTerminalRecord) {
			record.Outcome = mutationOutcomeRejected
			record.DesiredConfigHash = domainHash("desired", "different")
			record.ErrorCode = "terminal_write_failed"
			record.ErrorRetryable = true
		}},
		{name: "rejected retryability", mutate: func(record *mutationTerminalRecord) {
			record.Outcome = mutationOutcomeRejected
			record.DesiredConfigHash = domainHash("desired", "different")
			record.ErrorCode = "stale_config_hash"
			record.ErrorRetryable = false
		}},
		{name: "rejected unchanged hash", mutate: func(record *mutationTerminalRecord) {
			record.Outcome = mutationOutcomeRejected
			record.ErrorCode = "stale_config_hash"
			record.ErrorRetryable = true
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeSwitchFixture(t, globalRoutingFixtureYAML)
			opts := mutationOptions{OperationID: "terminal-shape", hasOperationID: true}
			spec := journaledMutationSpec{Operation: "set_codex_route", Surface: mutationSurfaceCodex, Client: "codex", Key: "default_model", RequestedTarget: "example/model"}
			publishExactUnchangedTerminalForTest(t, path, spec.Client, opts, spec)
			record, err := readMutationTerminal(path, opts.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.mutate != nil {
				testCase.mutate(&record)
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			data = append(append(data, '\n'), testCase.extra...)
			if err := os.WriteFile(mutationTerminalPath(path, opts.OperationID), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readMutationTerminal(path, opts.OperationID); err == nil {
				t.Fatal("terminal reader accepted inconsistent content")
			}
		})
	}
}

func TestTerminalPickerToggleOperationShapeRequiresNoTargetHash(t *testing.T) {
	installRoutingMutationSeams(t)
	for _, operation := range []string{"enable_claude_picker", "disable_claude_picker"} {
		t.Run(operation, func(t *testing.T) {
			path := writeSwitchFixture(t, globalRoutingFixtureYAML)
			journal := legacyJournalForTest(t, path, operation+"-terminal-shape")
			journal.Operation = operation
			journal.Surface = mutationSurfaceClaude
			journal.Client = "claude-code"
			journal.Key = "model_picker"
			journal.RequestedTarget = ""
			journal.RequestFingerprint = domainHash("request", operation)
			record := terminalFromJournal(
				journal,
				mutationOutcomeUnchanged,
				routingAdminStatus{},
				"",
				false,
			)
			record.DesiredConfigHash = record.PreviousConfigHash
			record.PreviousActiveToken = ""
			if err := validateTerminalRecord(record, path, journal.OperationID); err != nil {
				t.Fatalf("valid picker toggle terminal rejected: %v", err)
			}
			record.RequestedTargetHash = domainHash("requested-target-v1", "unexpected")
			if err := validateTerminalRecord(record, path, journal.OperationID); err == nil {
				t.Fatal("picker toggle terminal with a target hash was accepted")
			}
		})
	}
}

func TestMutationWritersEnforceReaderBoundsBeforeCommit(t *testing.T) {
	installRoutingMutationSeams(t)
	t.Run("journal", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		journal := legacyJournalForTest(t, path, "oversize-writer")
		journal.PreviousConfig = bytes.Repeat([]byte("x"), mutationJournalReadLimit)
		journal.PreviousConfigHash = exactConfigHash(journal.PreviousConfig)
		if err := writeMutationJournal(journal); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("oversize journal error = %v", err)
		}
		if _, err := os.Stat(mutationJournalPath(path, journal.OperationID)); !os.IsNotExist(err) {
			t.Fatalf("oversize journal was written: %v", err)
		}
	})
	t.Run("desired config", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		prior, mode, err := readExactConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		opts := mutationOptions{JSON: true, OperationID: "oversize-desired", hasOperationID: true}
		rc := runJournaledMutationLocked(path, prior, mode, opts, &out, journaledMutationSpec{
			Operation: "set_global_routing", Surface: mutationSurfaceSwitch, Apply: func(editPath string) error {
				return os.WriteFile(editPath, bytes.Repeat([]byte("x"), mutationConfigReadLimit+1), mode)
			},
		})
		result := decodeMutationResult(t, out.String())
		if rc != 1 || result.Error == nil || result.Error.Code != "config_size_limit" || exactFileHash(t, path) != exactConfigHash(prior) {
			t.Fatalf("oversize desired rc=%d result=%+v", rc, result)
		}
	})
}

func writeOldTerminalForReplayTest(t *testing.T, path, operationID string) {
	t.Helper()
	if err := secureDirectory(mutationJournalDir(path), true); err != nil {
		t.Fatal(err)
	}
	if err := secureDirectory(mutationCompletedDir(path), true); err != nil {
		t.Fatal(err)
	}
	journal := legacyJournalForTest(t, path, operationID)
	record := terminalFromJournal(journal, mutationOutcomeUnchanged, routingAdminStatus{}, "", false)
	record.DesiredConfigHash = record.PreviousConfigHash
	record.PreviousActiveToken = ""
	record.CompletedAt = time.Now().UTC().Add(-mutationTerminalRetain - time.Hour)
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mutationTerminalPath(path, operationID), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func ageTerminalForReplayTest(t *testing.T, path, operationID string) {
	t.Helper()
	record, err := readMutationTerminal(path, operationID)
	if err != nil {
		t.Fatal(err)
	}
	record.CompletedAt = time.Now().UTC().Add(-mutationTerminalRetain - time.Hour)
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mutationTerminalPath(path, operationID), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgedCleanupPendingTerminalSurvivesReplayGC(t *testing.T) {
	installRoutingMutationSeams(t)
	setup := func(t *testing.T, operationID string) (string, mutationOptions, journaledMutationSpec) {
		t.Helper()
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		opts := mutationOptions{JSON: true, OperationID: operationID, hasOperationID: true}
		spec := journaledMutationSpec{Operation: "set_global_routing", Surface: mutationSurfaceSwitch, Requested: true}
		journal := publishExactUnchangedTerminalForTest(t, path, "", opts, spec)
		if err := writeMutationJournal(journal); err != nil {
			t.Fatal(err)
		}
		ageTerminalForReplayTest(t, path, operationID)
		return path, opts, spec
	}
	assertResidue := func(t *testing.T, path, operationID string) {
		t.Helper()
		if _, err := os.Stat(mutationTerminalPath(path, operationID)); err != nil {
			t.Fatalf("aged cleanup-pending terminal was removed: %v", err)
		}
		if _, err := os.Stat(mutationJournalPath(path, operationID)); err != nil {
			t.Fatalf("active cleanup journal was removed: %v", err)
		}
	}

	t.Run("original command", func(t *testing.T) {
		path, opts, spec := setup(t, "aged-cleanup-original")
		var out strings.Builder
		replayed, rc := preflightTerminalReplay(path, opts, spec, &out)
		result := decodeMutationResult(t, out.String())
		if !replayed || rc != 0 || !result.OK || !result.CleanupPending {
			t.Fatalf("replayed=%v rc=%d result=%+v", replayed, rc, result)
		}
		assertResidue(t, path, opts.OperationID)
	})

	t.Run("reconcile", func(t *testing.T) {
		path, opts, _ := setup(t, "aged-cleanup-reconcile")
		stdout, rc := captureStdout(t, func() int {
			return cmdMutation([]string{"reconcile", opts.OperationID, "--json"})
		})
		result := decodeMutationResult(t, stdout)
		if rc != 0 || !result.OK || !result.CleanupPending {
			t.Fatalf("rc=%d result=%+v", rc, result)
		}
		assertResidue(t, path, opts.OperationID)
	})
}

func TestTerminalGCFailsClosedOnInvalidOrAmbiguousActiveState(t *testing.T) {
	installRoutingMutationSeams(t)
	assertProtected := func(t *testing.T, path, operationID string) {
		t.Helper()
		if err := gcMutationTerminals(path, time.Now().UTC()); err == nil {
			t.Fatal("GC accepted unsafe active mutation state")
		}
		if _, err := os.Stat(mutationTerminalPath(path, operationID)); err != nil {
			t.Fatalf("GC deleted a terminal after unsafe active state: %v", err)
		}
	}

	t.Run("invalid", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		const terminalID = "gc-protected-by-invalid-state"
		writeOldTerminalForReplayTest(t, path, terminalID)
		if err := os.WriteFile(mutationJournalPath(path, "invalid-active"), []byte("{\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertProtected(t, path, terminalID)
	})

	t.Run("multiple", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		const terminalID = "gc-protected-by-ambiguous-state"
		writeOldTerminalForReplayTest(t, path, terminalID)
		for _, operationID := range []string{"active-one", "active-two"} {
			journal := legacyJournalForTest(t, path, operationID)
			data, err := json.Marshal(journal)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mutationJournalPath(path, operationID), append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		assertProtected(t, path, terminalID)
	})
}

func TestTerminalReplayRunsBestEffortGC(t *testing.T) {
	installRoutingMutationSeams(t)
	t.Run("original command", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		opts := mutationOptions{JSON: true, OperationID: "gc-original", hasOperationID: true}
		spec := journaledMutationSpec{Operation: "set_global_routing", Surface: mutationSurfaceSwitch, Requested: true}
		publishExactUnchangedTerminalForTest(t, path, "", opts, spec)
		writeOldTerminalForReplayTest(t, path, "gc-old-original")
		var out strings.Builder
		replayed, rc := preflightTerminalReplay(path, opts, spec, &out)
		if !replayed || rc != 0 {
			t.Fatalf("replay=%v rc=%d out=%s", replayed, rc, out.String())
		}
		if _, err := os.Stat(mutationTerminalPath(path, "gc-old-original")); !os.IsNotExist(err) {
			t.Fatalf("old terminal was not collected: %v", err)
		}
	})
	t.Run("reconcile", func(t *testing.T) {
		path := writeSwitchFixture(t, globalRoutingFixtureYAML)
		opts := mutationOptions{JSON: true, OperationID: "gc-reconcile", hasOperationID: true}
		spec := journaledMutationSpec{Operation: "set_global_routing", Surface: mutationSurfaceSwitch, Requested: true}
		publishExactUnchangedTerminalForTest(t, path, "", opts, spec)
		writeOldTerminalForReplayTest(t, path, "gc-old-reconcile")
		stdout, rc := captureStdout(t, func() int {
			return cmdMutation([]string{"reconcile", opts.OperationID, "--json"})
		})
		if rc != 0 {
			t.Fatalf("reconcile rc=%d out=%s", rc, stdout)
		}
		if _, err := os.Stat(mutationTerminalPath(path, "gc-old-reconcile")); !os.IsNotExist(err) {
			t.Fatalf("old terminal was not collected: %v", err)
		}
	})
}

func TestJSONMutationErrorsUseReviewedMessages(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	hidden := path + ".private-detail"
	var out strings.Builder
	opts := mutationOptions{JSON: true, OperationID: "reviewed-error", hasOperationID: true}
	result := mutationResult{Operation: "reconcile_routing_mutation", ConfigPath: path}
	if rc := failMutation(opts, &out, result, "config_read_failed", "read "+hidden+": permission denied", true, 1); rc != 1 {
		t.Fatalf("rc = %d", rc)
	}
	decoded := decodeMutationResult(t, out.String())
	if decoded.Error == nil || decoded.Error.Message != reviewedMutationMessage("config_read_failed") ||
		strings.Contains(decoded.Error.Message, hidden) || strings.Contains(decoded.Error.Message, "permission denied") {
		t.Fatalf("reviewed error = %+v", decoded.Error)
	}
}

func TestHumanMutationFailuresUseReviewedMessages(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	hidden := path + ".private-detail"
	opts := mutationOptions{OperationID: "reviewed-human-error", hasOperationID: true}
	result := mutationResult{Operation: "set_global_routing", ConfigPath: path}
	stderr := captureStderr(t, func() {
		if rc := failMutation(opts, io.Discard, result, "config_read_failed", "read "+hidden+": backend target example/private", true, 1); rc != 1 {
			t.Fatalf("rc = %d", rc)
		}
	})
	if stderr != reviewedMutationMessage("config_read_failed")+"\n" || strings.Contains(stderr, hidden) || strings.Contains(stderr, "example/private") {
		t.Fatalf("human error = %q", stderr)
	}
}
