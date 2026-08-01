package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
)

const globalRoutingFixtureYAML = `# fixture keeps comments across the global flip.
global:
  routing_enabled: true  # the one global gate

clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
    model_routes:
      fable: zai-org/GLM-5.2
      opus: zai-org/GLM-5.2
      sonnet: zai-org/GLM-5.2
      haiku: zai-org/GLM-5.2
    fallback_route: anthropic
`

func installRoutingMutationSeams(t *testing.T) {
	t.Helper()
	oldFetch := fetchRoutingAdminStatus
	oldSignal := signalRouter
	oldTimeout := routeApplyTimeout
	oldHTTPTimeout := mutationHTTPTimeout
	oldLockTimeout := mutationLockTimeout
	oldCommitHook := exactConfigCommitHook
	oldCommitLink := exactConfigCommitLink
	t.Cleanup(func() {
		fetchRoutingAdminStatus = oldFetch
		signalRouter = oldSignal
		routeApplyTimeout = oldTimeout
		mutationHTTPTimeout = oldHTTPTimeout
		mutationLockTimeout = oldLockTimeout
		exactConfigCommitHook = oldCommitHook
		exactConfigCommitLink = oldCommitLink
	})
	routeApplyTimeout = 300 * time.Millisecond
	mutationHTTPTimeout = 50 * time.Millisecond
	mutationLockTimeout = 100 * time.Millisecond
}

func routingEnabledFromFile(t *testing.T, path string) bool {
	t.Helper()
	file, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if file.Global.RoutingEnabled == nil {
		t.Fatal("global.routing_enabled is absent")
	}
	return *file.Global.RoutingEnabled
}

func exactFileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return exactConfigHash(data)
}

func liveRoutingStatus(path, hash string, enabled bool, generation uint64) routingAdminStatus {
	return routingAdminStatus{
		ConfigPath:           path,
		RouterPID:            os.Getpid(),
		RouterBootID:         "019f90a6-2c0d-7fd1-bf11-8b78cf7427c1",
		ActiveGeneration:     generation,
		ActiveConfigHash:     hash,
		DesiredConfigHash:    hash,
		GlobalRoutingEnabled: enabled,
	}
}

func decodeMutationResult(t *testing.T, raw string) mutationResult {
	t.Helper()
	var result mutationResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode mutation JSON %q: %v", raw, err)
	}
	return result
}

func TestGlobalSwitchMutatesOnlyGlobalGateAndConfirmsExactHash(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	priorHash := exactConfigHash(prior)
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var signalCount atomic.Int32
	signalRouter = func(pid int) error {
		if pid != os.Getpid() {
			t.Fatalf("signal pid = %d want %d", pid, os.Getpid())
		}
		signalCount.Add(1)
		return nil
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		if signalCount.Load() == 0 {
			return liveRoutingStatus(path, priorHash, true, 41), nil
		}
		return liveRoutingStatus(path, exactFileHash(t, path), false, 42), nil
	}

	var out strings.Builder
	args := []string{
		"--json",
		"--operation-id", "global-off-1",
		"--if-active-token", "019f90a6-2c0d-7fd1-bf11-8b78cf7427c1:41",
		"--if-config-hash", priorHash,
	}
	var rc int
	stderr := captureStderr(t, func() { rc = runSwitch("off", args, &out) })
	if rc != 0 {
		t.Fatalf("rc = %d\nstdout:\n%s\nstderr:\n%s", rc, out.String(), stderr)
	}
	if stderr != "" {
		t.Fatalf("JSON mutation wrote stderr: %s", stderr)
	}
	if signalCount.Load() != 1 {
		t.Fatalf("signals = %d want 1", signalCount.Load())
	}
	result := decodeMutationResult(t, out.String())
	if !result.OK || !result.Applied || result.Error != nil {
		t.Fatalf("result = %+v", result)
	}
	if result.PreviousActiveToken != "019f90a6-2c0d-7fd1-bf11-8b78cf7427c1:41" ||
		result.ActiveToken != "019f90a6-2c0d-7fd1-bf11-8b78cf7427c1:42" {
		t.Fatalf("unexpected ordering tokens: %+v", result)
	}
	if result.PreviousDesiredConfigHash != priorHash ||
		result.DesiredConfigHash == priorHash ||
		result.ActiveConfigHash != result.DesiredConfigHash {
		t.Fatalf("unexpected hashes: %+v", result)
	}
	if routingEnabledFromFile(t, path) {
		t.Fatal("global routing still enabled")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(globalRoutingFixtureYAML,
		"routing_enabled: true  # the one global gate",
		"routing_enabled: false  # the one global gate", 1)
	if string(after) != want {
		t.Fatalf("routing edit changed bytes outside the gate\n--- got ---\n%s\n--- want ---\n%s", after, want)
	}
	if _, err := os.Stat(mutationJournalPath(path, "global-off-1")); !os.IsNotExist(err) {
		t.Fatalf("completed journal still exists: %v", err)
	}
}

func TestRoutingMutationRejectsNamedClientBeforeWriting(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	var rc int
	stderr := captureStderr(t, func() { rc = runSwitch("off", []string{"claude-code"}, &out) })
	if rc != 2 || !strings.Contains(stderr, "one global switch") {
		t.Fatalf("rc = %d stdout=%q stderr=%q", rc, out.String(), stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("named-client refusal changed the config")
	}
}

func TestRoutingMutationCASAndStatusPreconditionsFailClosed(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*routingAdminStatus)
		args       func(string) []string
		wantCode   string
		wantPhrase string
	}{
		{
			name: "stale exact config hash",
			args: func(string) []string {
				return []string{"--json", "--operation-id", "stale-hash", "--if-config-hash", "sha256:" + strings.Repeat("0", 64)}
			},
			wantCode: "stale_config_hash",
		},
		{
			name: "stale active token",
			args: func(hash string) []string {
				return []string{"--json", "--operation-id", "stale-token", "--if-config-hash", hash, "--if-active-token", "other:9"}
			},
			wantCode: "stale_active_token",
		},
		{
			name: "missing active config path",
			mutate: func(status *routingAdminStatus) {
				status.ConfigPath = ""
			},
			args: func(hash string) []string {
				return []string{"--json", "--operation-id", "missing-path", "--if-config-hash", hash}
			},
			wantCode:   "router_state_mismatch",
			wantPhrase: "managed routing config",
		},
		{
			name: "wrong active config path",
			mutate: func(status *routingAdminStatus) {
				status.ConfigPath += ".other"
			},
			args: func(hash string) []string {
				return []string{"--json", "--operation-id", "wrong-path", "--if-config-hash", hash}
			},
			wantCode:   "router_state_mismatch",
			wantPhrase: "managed routing config",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			installRoutingMutationSeams(t)
			path := writeSwitchFixture(t, globalRoutingFixtureYAML)
			hash := exactFileHash(t, path)
			if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
				t.Fatal(err)
			}
			status := liveRoutingStatus(path, hash, true, 7)
			if testCase.mutate != nil {
				testCase.mutate(&status)
			}
			fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
				return status, nil
			}
			var signaled bool
			signalRouter = func(int) error {
				signaled = true
				return nil
			}
			var out strings.Builder
			var rc int
			captureStderr(t, func() {
				rc = runSwitch("off", testCase.args(hash), &out)
			})
			if rc != 1 {
				t.Fatalf("rc = %d want 1; %s", rc, out.String())
			}
			result := decodeMutationResult(t, out.String())
			if result.Error == nil || result.Error.Code != testCase.wantCode {
				t.Fatalf("error = %+v want code %s", result.Error, testCase.wantCode)
			}
			if testCase.wantPhrase != "" && !strings.Contains(result.Error.Message, testCase.wantPhrase) {
				t.Fatalf("error %q missing %q", result.Error.Message, testCase.wantPhrase)
			}
			if signaled {
				t.Fatal("failed precondition signaled the router")
			}
			if !routingEnabledFromFile(t, path) {
				t.Fatal("failed precondition changed the global gate")
			}
		})
	}
}

func TestRoutingMutationNeverSignalsPIDZeroWhenRouterDisappears(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	priorHash := exactFileHash(t, path)
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		_ = os.Remove(pidfile.Path())
		return liveRoutingStatus(path, priorHash, true, 1), nil
	}
	var signaled []int
	signalRouter = func(pid int) error {
		signaled = append(signaled, pid)
		return nil
	}
	var out strings.Builder
	var rc int
	captureStderr(t, func() {
		rc = runSwitch("off", []string{"--json", "--operation-id", "router-disappeared"}, &out)
	})
	if rc != 1 {
		t.Fatalf("rc = %d want 1; %s", rc, out.String())
	}
	for _, pid := range signaled {
		if pid <= 1 {
			t.Fatalf("unsafe pid was signaled: %v", signaled)
		}
	}
	if len(signaled) != 0 {
		t.Fatalf("router disappearance should not signal any pid: %v", signaled)
	}
	if !routingEnabledFromFile(t, path) {
		t.Fatal("failed activation did not restore the prior global gate")
	}
}

func TestRoutingMutationOfflineJournalAndReconcile(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	priorHash := exactFileHash(t, path)
	var out strings.Builder
	var rc int
	captureStderr(t, func() {
		rc = runSwitch("off", []string{"--json", "--operation-id", "offline-op"}, &out)
	})
	if rc != 0 {
		t.Fatalf("offline mutation rc = %d: %s", rc, out.String())
	}
	result := decodeMutationResult(t, out.String())
	if !result.OK || result.Applied || !result.ReconciliationRequired {
		t.Fatalf("offline result = %+v", result)
	}
	journalPath := mutationJournalPath(path, "offline-op")
	journalInfo, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if journalInfo.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %v want 0600", journalInfo.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(journalPath))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("journal dir mode = %v want 0700", dirInfo.Mode().Perm())
	}
	desiredHash := exactFileHash(t, path)
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var signals atomic.Int32
	signalRouter = func(pid int) error {
		signals.Add(1)
		return nil
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		if signals.Load() == 0 {
			return liveRoutingStatus(path, priorHash, true, 10), nil
		}
		return liveRoutingStatus(path, desiredHash, false, 11), nil
	}
	stdout, reconcileRC := captureStdout(t, func() int {
		return cmdMutation([]string{"reconcile", "offline-op", "--json"})
	})
	if reconcileRC != 0 {
		t.Fatalf("reconcile rc = %d: %s", reconcileRC, stdout)
	}
	reconciled := decodeMutationResult(t, stdout)
	if !reconciled.OK || !reconciled.Applied || reconciled.ActiveConfigHash != desiredHash {
		t.Fatalf("reconcile result = %+v", reconciled)
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("reconciled journal still exists: %v", err)
	}
}

func TestRoutingMutationUnfinishedJournalBlocksNextMutation(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	var first strings.Builder
	captureStderr(t, func() {
		if rc := runSwitch("off", []string{"--json", "--operation-id", "first-offline"}, &first); rc != 0 {
			t.Fatalf("first mutation rc = %d: %s", rc, first.String())
		}
	})
	if routingEnabledFromFile(t, path) {
		t.Fatal("first offline mutation did not disable routing")
	}

	var second strings.Builder
	var rc int
	captureStderr(t, func() {
		rc = runSwitch("on", []string{"--json", "--operation-id", "second-offline"}, &second)
	})
	if rc != 1 {
		t.Fatalf("second mutation rc = %d want 1: %s", rc, second.String())
	}
	result := decodeMutationResult(t, second.String())
	if result.Error == nil || result.Error.Code != "unfinished_mutation" ||
		result.BlockingOperationID != "first-offline" || result.ReconciliationRequired {
		t.Fatalf("result = %+v", result)
	}
	if routingEnabledFromFile(t, path) {
		t.Fatal("blocked second mutation changed the global gate")
	}
}

func TestRoutingMutationLockRejectsConcurrentWriter(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.close()
	mutationLockTimeout = 25 * time.Millisecond
	var out strings.Builder
	var rc int
	captureStderr(t, func() {
		rc = runSwitch("off", []string{"--json", "--operation-id", "locked-op"}, &out)
	})
	if rc != 1 {
		t.Fatalf("rc = %d want 1; %s", rc, out.String())
	}
	result := decodeMutationResult(t, out.String())
	if result.Error == nil || result.Error.Code != "mutation_locked" || !result.Error.Retryable {
		t.Fatalf("result = %+v", result)
	}
	if !routingEnabledFromFile(t, path) {
		t.Fatal("lock refusal changed global routing")
	}
}

func TestNormalizeMutationInvocation(t *testing.T) {
	got, err := normalizeMutationInvocation([]string{
		"--json",
		"--operation-id", "abc",
		"--if-active-token=boot:3",
		"--if-config-hash", "sha256:" + strings.Repeat("a", 64),
		"off",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"off",
		"--json",
		"--operation-id", "abc",
		"--if-active-token=boot:3",
		"--if-config-hash", "sha256:" + strings.Repeat("a", 64),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("normalized args = %#v want %#v", got, want)
	}

	got, err = normalizeMutationInvocation([]string{
		"--json",
		"--operation-id", "route-abc",
		"claude", "route", "opus", "native",
	})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		"claude", "route", "opus", "native",
		"--json",
		"--operation-id", "route-abc",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("normalized claude args = %#v want %#v", got, want)
	}
}

func TestRoutingMutationRejectsAdminPIDMismatchWithoutSignalOrWrite(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	status := liveRoutingStatus(path, exactConfigHash(prior), true, 9)
	status.RouterPID = os.Getpid() + 1000
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		return status, nil
	}
	signaled := false
	signalRouter = func(int) error {
		signaled = true
		return nil
	}
	var out strings.Builder
	var rc int
	captureStderr(t, func() {
		rc = runSwitch("off", []string{"--json", "--operation-id", "pid-mismatch"}, &out)
	})
	if rc != 1 {
		t.Fatalf("rc = %d want 1: %s", rc, out.String())
	}
	result := decodeMutationResult(t, out.String())
	if result.Error == nil || result.Error.Code != "router_identity_mismatch" {
		t.Fatalf("result = %+v", result)
	}
	if signaled {
		t.Fatal("pid identity mismatch signaled a process")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(prior) {
		t.Fatal("pid identity mismatch changed the config")
	}
}

func TestRoutingMutationClaudeRouteUsesJournaledReceipt(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	priorHash := exactFileHash(t, path)
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var signals atomic.Int32
	signalRouter = func(pid int) error {
		if pid != os.Getpid() {
			t.Fatalf("signal pid = %d want %d", pid, os.Getpid())
		}
		signals.Add(1)
		return nil
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		if signals.Load() == 0 {
			return liveRoutingStatus(path, priorHash, true, 21), nil
		}
		return liveRoutingStatus(path, exactFileHash(t, path), true, 22), nil
	}
	adapter := &claudeAdapter{
		configPath: path,
		clientName: "claude-code",
		modelAliases: map[string]string{
			"claude-baseten-glm-5-2": "zai-org/GLM-5.2",
		},
		out: io.Discard,
	}
	var out strings.Builder
	rc := adapter.route([]string{
		"opus", "native",
		"--json",
		"--operation-id", "route-opus-native",
		"--if-active-token", "019f90a6-2c0d-7fd1-bf11-8b78cf7427c1:21",
		"--if-config-hash", priorHash,
	}, &out)
	if rc != 0 {
		t.Fatalf("rc = %d: %s", rc, out.String())
	}
	result := decodeMutationResult(t, out.String())
	if !result.OK || !result.Applied ||
		result.Operation != "set_claude_route" ||
		result.Client != "claude-code" ||
		result.Key != "opus" ||
		result.RequestedTarget != "native" {
		t.Fatalf("result = %+v", result)
	}
	file, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Clients[0].ModelRoutes["opus"]; got != "native" {
		t.Fatalf("opus route = %q want native", got)
	}
}

func TestRoutingMutationClaudeSubagentsUsesJournaledReceipt(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	if err := config.SetClientScalars(path, "claude-code", map[string]string{
		"subagent_model":   "zai-org/GLM-5.2",
		"subagent_routing": "on",
	}); err != nil {
		t.Fatal(err)
	}
	priorHash := exactFileHash(t, path)
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var signals atomic.Int32
	signalRouter = func(pid int) error {
		if pid != os.Getpid() {
			t.Fatalf("signal pid = %d want %d", pid, os.Getpid())
		}
		signals.Add(1)
		return nil
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		if signals.Load() == 0 {
			return liveRoutingStatus(path, priorHash, true, 31), nil
		}
		return liveRoutingStatus(path, exactFileHash(t, path), true, 32), nil
	}
	adapter := &claudeAdapter{
		configPath: path,
		clientName: "claude-code",
		out:        io.Discard,
	}
	var out strings.Builder
	rc := adapter.subagents([]string{
		"off",
		"--json",
		"--operation-id", "subagents-inherit",
		"--if-active-token", "019f90a6-2c0d-7fd1-bf11-8b78cf7427c1:31",
		"--if-config-hash", priorHash,
	}, &out)
	if rc != 0 {
		t.Fatalf("rc = %d: %s", rc, out.String())
	}
	result := decodeMutationResult(t, out.String())
	if !result.OK || !result.Applied ||
		result.Operation != "set_claude_subagents" ||
		result.Client != "claude-code" ||
		result.Key != "subagents" ||
		result.RequestedTarget != "inherit" {
		t.Fatalf("result = %+v", result)
	}
	file, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Clients[0].SubagentRouting; got != "off" {
		t.Fatalf("subagent_routing = %q want off", got)
	}
}

func TestRoutingMutationClaudeRouteOfflineReconcilePreservesReceipt(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	priorHash := exactFileHash(t, path)
	adapter := &claudeAdapter{
		configPath: path,
		clientName: "claude-code",
		out:        io.Discard,
	}
	var out strings.Builder
	rc := adapter.route([]string{
		"sonnet", "native",
		"--json",
		"--operation-id", "route-offline",
	}, &out)
	if rc != 0 {
		t.Fatalf("offline route rc = %d: %s", rc, out.String())
	}
	offline := decodeMutationResult(t, out.String())
	if !offline.OK || offline.Applied || !offline.ReconciliationRequired {
		t.Fatalf("offline result = %+v", offline)
	}
	desiredHash := exactFileHash(t, path)
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
			return liveRoutingStatus(path, priorHash, true, 41), nil
		}
		return liveRoutingStatus(path, desiredHash, true, 42), nil
	}
	stdout, reconcileRC := captureStdout(t, func() int {
		return cmdMutation([]string{"reconcile", "route-offline", "--json"})
	})
	if reconcileRC != 0 {
		t.Fatalf("reconcile rc = %d: %s", reconcileRC, stdout)
	}
	result := decodeMutationResult(t, stdout)
	if !result.OK || !result.Applied ||
		result.Operation != "set_claude_route" ||
		result.Client != "claude-code" ||
		result.Key != "sonnet" ||
		result.RequestedTarget != "native" ||
		result.IdentityStrength != mutationIdentityExact ||
		result.RequestFingerprint == "" {
		t.Fatalf("reconciled result = %+v", result)
	}
	replayStdout, replayRC := captureStdout(t, func() int {
		return cmdMutation([]string{"reconcile", "route-offline", "--json"})
	})
	if replayRC != 0 {
		t.Fatalf("terminal reconcile rc = %d: %s", replayRC, replayStdout)
	}
	replay := decodeMutationResult(t, replayStdout)
	if !replay.OK || replay.Operation != result.Operation || replay.Client != result.Client ||
		replay.Key != "" || replay.RequestedTarget != "" ||
		replay.RequestFingerprint != result.RequestFingerprint || replay.IdentityStrength != mutationIdentityExact {
		t.Fatalf("terminal reconcile replay = %+v", replay)
	}
}

func TestJournaledMutationCASPreservesConcurrentExternalEdit(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	prior, mode, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	external := append(append([]byte(nil), prior...), []byte("# external edit\n")...)
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.close()
	var out strings.Builder
	opts := mutationOptions{JSON: true, OperationID: "external-race", hasOperationID: true}
	rc := runJournaledMutationLocked(path, prior, mode, opts, &out, journaledMutationSpec{
		Operation:       "set_claude_route",
		Surface:         mutationSurfaceClaude,
		RequestedTarget: "native",
		Client:          "claude-code",
		Key:             "opus",
		Apply: func(previewPath string) error {
			if err := config.SetClientMapEntries(previewPath, "claude-code", "model_routes", map[string]string{"opus": "native"}); err != nil {
				return err
			}
			return os.WriteFile(path, external, mode)
		},
	})
	if rc != 1 {
		t.Fatalf("rc = %d want 1: %s", rc, out.String())
	}
	result := decodeMutationResult(t, out.String())
	if result.Error == nil || result.Error.Code != "stale_config_hash" {
		t.Fatalf("result = %+v", result)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(external) {
		t.Fatal("CAS failure overwrote the concurrent external edit")
	}
	if _, err := os.Stat(mutationJournalPath(path, "external-race")); !os.IsNotExist(err) {
		t.Fatalf("CAS refusal left a journal: %v", err)
	}
}

func TestRejectedMutationReceiptPreservesCleanupPending(t *testing.T) {
	installRoutingMutationSeams(t)
	installTerminalPublicationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	prior, mode, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	external := append(append([]byte(nil), prior...), []byte("# external edit\n")...)
	realRemove := mutationTerminalRemove
	mutationTerminalRemove = func(target string) error {
		if target == mutationJournalPath(path, "rejected-cleanup") {
			return errors.New("injected active journal cleanup failure")
		}
		return realRemove(target)
	}
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.close()
	var out strings.Builder
	opts := mutationOptions{JSON: true, OperationID: "rejected-cleanup", hasOperationID: true}
	rc := runJournaledMutationLocked(path, prior, mode, opts, &out, journaledMutationSpec{
		Operation: "set_claude_route", Surface: mutationSurfaceClaude, Client: "claude-code", Key: "opus", RequestedTarget: "native",
		Apply: func(previewPath string) error {
			if err := config.SetClientMapEntries(previewPath, "claude-code", "model_routes", map[string]string{"opus": "native"}); err != nil {
				return err
			}
			return os.WriteFile(path, external, mode)
		},
	})
	result := decodeMutationResult(t, out.String())
	if rc != 1 || result.Error == nil || result.Error.Code != "stale_config_hash" ||
		result.Outcome != mutationOutcomeRejected || !result.CleanupPending {
		t.Fatalf("rejected result rc=%d result=%+v", rc, result)
	}
	if _, err := os.Stat(mutationJournalPath(path, opts.OperationID)); err != nil {
		t.Fatalf("cleanup-pending journal missing: %v", err)
	}
	record, err := readMutationTerminal(path, opts.OperationID)
	if err != nil || record.Outcome != mutationOutcomeRejected {
		t.Fatalf("rejected terminal=%+v err=%v", record, err)
	}
}

func atomicExternalConfigReplace(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	file, err := os.CreateTemp(filepath.Dir(path), ".external-config-*")
	if err != nil {
		t.Fatal(err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		t.Fatal(err)
	}
}

func assertNoExactCommitArtifacts(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(
		filepath.Dir(path), exactCommitDirPrefix(path)+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("exact-config transaction artifacts remain: %v", matches)
	}
}

func TestExactConfigCommitPreservesAtomicEditImmediatelyBeforeDisplacement(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	prior, mode, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	desired := []byte(strings.Replace(
		string(prior), "routing_enabled: true", "routing_enabled: false", 1))
	external := append(append([]byte(nil), prior...), []byte("# external before displacement\n")...)
	exactConfigCommitHook = func(stage, configPath, _ string) error {
		if stage == "before_displacement" {
			atomicExternalConfigReplace(t, configPath, external, mode)
		}
		return nil
	}

	if err := compareAndSwapExactConfig(path, exactConfigHash(prior), desired, mode); err == nil {
		t.Fatal("commit succeeded after an external edit")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(external) {
		t.Fatal("external edit immediately before displacement was overwritten")
	}
	assertNoExactCommitArtifacts(t, path)
}

func TestExactConfigCommitPreservesAtomicEditBetweenDisplacementAndInstall(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	prior, mode, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	desired := []byte(strings.Replace(
		string(prior), "routing_enabled: true", "routing_enabled: false", 1))
	external := append(append([]byte(nil), prior...), []byte("# external during displacement\n")...)
	exactConfigCommitHook = func(stage, configPath, _ string) error {
		if stage == "after_displacement" {
			atomicExternalConfigReplace(t, configPath, external, mode)
		}
		return nil
	}

	if err := compareAndSwapExactConfig(path, exactConfigHash(prior), desired, mode); err == nil {
		t.Fatal("commit succeeded after an external editor reclaimed the path")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(external) {
		t.Fatal("external edit between displacement and install was overwritten")
	}
	assertNoExactCommitArtifacts(t, path)
}

func TestExactConfigCommitInstallFailureRestoresPriorWithoutRenameClobber(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	prior, mode, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	desired := []byte(strings.Replace(
		string(prior), "routing_enabled: true", "routing_enabled: false", 1))
	exactConfigCommitLink = func(oldPath, newPath string) error {
		if filepath.Base(oldPath) == "desired" && newPath == path {
			return errors.New("injected exclusive install failure")
		}
		return os.Link(oldPath, newPath)
	}

	if err := compareAndSwapExactConfig(path, exactConfigHash(prior), desired, mode); err == nil {
		t.Fatal("commit succeeded despite injected install failure")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(prior) {
		t.Fatal("install failure did not restore the prior exact bytes")
	}
	assertNoExactCommitArtifacts(t, path)
}

func TestExactConfigCommitRejectsSymlinkAndPreservesPrivateMode(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	prior, mode, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	desired := []byte(strings.Replace(
		string(prior), "routing_enabled: true", "routing_enabled: false", 1))
	if err := compareAndSwapExactConfig(path, exactConfigHash(prior), desired, mode); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode.Perm() {
		t.Fatalf("installed mode = %v want %v", info.Mode().Perm(), mode.Perm())
	}

	linkPath := filepath.Join(filepath.Dir(path), "gateway-link.yaml")
	if err := os.Symlink(path, linkPath); err != nil {
		t.Fatal(err)
	}
	if err := compareAndSwapExactConfig(
		linkPath, exactConfigHash(desired), prior, mode); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink commit error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(desired) {
		t.Fatal("symlink refusal changed its target")
	}
}

func TestMutationReconcileRecoversCommitInterruptedAfterDisplacement(t *testing.T) {
	installRoutingMutationSeams(t)
	path := writeSwitchFixture(t, globalRoutingFixtureYAML)
	prior, _, err := readExactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	priorHash := exactConfigHash(prior)
	if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		return liveRoutingStatus(path, priorHash, true, 51), nil
	}
	exactConfigCommitHook = func(stage, _, _ string) error {
		if stage == "after_displacement" {
			return errExactConfigCommitInterrupted
		}
		return nil
	}

	var first strings.Builder
	rc := runSwitch("off", []string{
		"--json", "--operation-id", "interrupted-displacement",
	}, &first)
	if rc != 1 {
		t.Fatalf("interrupted mutation rc = %d want 1: %s", rc, first.String())
	}
	firstResult := decodeMutationResult(t, first.String())
	if firstResult.Error == nil ||
		firstResult.Error.Code != "activation_indeterminate" ||
		!firstResult.ReconciliationRequired {
		t.Fatalf("interrupted mutation result = %+v", firstResult)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("interrupted commit unexpectedly left config path present: %v", err)
	}
	if _, err := os.Stat(mutationJournalPath(path, "interrupted-displacement")); err != nil {
		t.Fatalf("interrupted commit lost its journal: %v", err)
	}

	exactConfigCommitHook = nil
	stdout, reconcileRC := captureStdout(t, func() int {
		return cmdMutation([]string{"reconcile", "interrupted-displacement", "--json"})
	})
	if reconcileRC != 0 {
		t.Fatalf("reconcile rc = %d: %s", reconcileRC, stdout)
	}
	result := decodeMutationResult(t, stdout)
	if !result.OK || result.Applied {
		t.Fatalf("reconciled result = %+v", result)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(prior) {
		t.Fatal("reconciliation did not restore the prior exact config")
	}
	if _, err := os.Stat(mutationJournalPath(path, "interrupted-displacement")); !os.IsNotExist(err) {
		t.Fatalf("reconciled journal still exists: %v", err)
	}
	assertNoExactCommitArtifacts(t, path)
}
