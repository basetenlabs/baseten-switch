package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func TestFallbackPolicyJSONReceiptUsesStructuredWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, config.InitTemplate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(dir, "router.pid"))
	opts := mutationOptions{JSON: true, OperationID: "fallback-warning-test", hasOperationID: true}
	var out bytes.Buffer
	if rc := mutateFallbackPolicy("429", true, opts, &out); rc != 0 {
		t.Fatalf("rc=%d receipt=%s", rc, out.String())
	}
	var receipt map[string]any
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	warnings, ok := receipt["warnings"].([]any)
	if !ok || len(warnings) != 2 {
		t.Fatalf("warnings=%#v", receipt["warnings"])
	}
	wantWarnings := []string{"cross_provider_history_may_be_incompatible", "capable_router_activation_required"}
	for i, want := range wantWarnings {
		warning, ok := warnings[i].(map[string]any)
		if !ok || warning["code"] != want {
			t.Fatalf("warning[%d]=%#v want=%q", i, warnings[i], want)
		}
	}
	if receipt["operation"] != "set_fallback_policy" || receipt["key"] != "429" || receipt["requested_target"] != "on" {
		t.Fatalf("receipt identity=%#v", receipt)
	}
}

func TestOfflineFallbackTargetReceiptRequiresCapableRouterActivation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, config.InitTemplate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(dir, "router.pid"))
	opts := mutationOptions{JSON: true, OperationID: "fallback-target-offline", hasOperationID: true}
	var out bytes.Buffer
	if rc := mutateNativeFallbackModel("claude-code", "claude-sonnet-5", opts, &out); rc != 0 {
		t.Fatalf("rc=%d receipt=%s", rc, out.String())
	}
	result := decodeMutationResult(t, out.String())
	if len(result.StructuredWarnings) != 1 || result.StructuredWarnings[0].Code != "capable_router_activation_required" {
		t.Fatalf("warnings=%#v", result.StructuredWarnings)
	}
}

func TestFallbackMutationRejectsReachableRouterWithoutCapabilityBeforeWrite(t *testing.T) {
	installRoutingMutationSeams(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, config.InitTemplate, 0o600); err != nil {
		t.Fatal(err)
	}
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	priorHash := exactConfigHash(prior)
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(dir, "router.pid"))
	if err := os.WriteFile(gatewayPidfilePath(), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		return liveRoutingStatus(path, priorHash, true, 1), nil
	}
	opts := mutationOptions{JSON: true, OperationID: "unsupported-fallback-router", hasOperationID: true}
	var out bytes.Buffer
	if rc := mutateFallbackPolicy("429", false, opts, &out); rc != 1 {
		t.Fatalf("rc=%d receipt=%s", rc, out.String())
	}
	result := decodeMutationResult(t, out.String())
	if result.Error == nil || result.Error.Code != "router_unsupported" {
		t.Fatalf("result=%+v", result)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, prior) {
		t.Fatal("unsupported router rejection changed the config")
	}
}

func TestFallbackActivationRequiresOperationSpecificProjection(t *testing.T) {
	installRoutingMutationSeams(t)
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	hash := exactConfigHash(config.InitTemplate)
	base := liveRoutingStatus(path, hash, true, 2)
	base.Capabilities = []string{"fallback_policy"}

	t.Run("policy", func(t *testing.T) {
		status := base
		status.FallbackPolicy.OnBaseten429 = false
		fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) { return status, nil }
		if _, ok := waitConfigMutationActivation("unused", path, hash, "", false, 5*time.Millisecond,
			"set_fallback_policy", "429", "", "on", true); ok {
			t.Fatal("hash-only match accepted the wrong active policy")
		}
		status.FallbackPolicy.OnBaseten429 = true
		if _, ok := waitConfigMutationActivation("unused", path, hash, "", false, 5*time.Millisecond,
			"set_fallback_policy", "429", "", "on", true); !ok {
			t.Fatal("matching active policy was not accepted")
		}
	})

	t.Run("target", func(t *testing.T) {
		status := base
		client := fallbackAdminClient{Name: "claude-code"}
		client.BasetenModelFallback.ResolvedModel = "claude-opus-5"
		status.Clients = []fallbackAdminClient{client}
		fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) { return status, nil }
		if _, ok := waitConfigMutationActivation("unused", path, hash, "", false, 5*time.Millisecond,
			"set_native_fallback_model", "native_fallback_model", "claude-code", "claude-sonnet-5", false); ok {
			t.Fatal("hash-only match accepted the wrong active target")
		}
		status.Clients[0].BasetenModelFallback.ResolvedModel = "claude-sonnet-5"
		if _, ok := waitConfigMutationActivation("unused", path, hash, "", false, 5*time.Millisecond,
			"set_native_fallback_model", "native_fallback_model", "claude-code", "claude-sonnet-5", false); !ok {
			t.Fatal("matching active target was not accepted")
		}
	})
}

func TestFallbackRecoveryDoesNotClassifyHashOnlyMatchAsDesiredActive(t *testing.T) {
	installRoutingMutationSeams(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	desired := append([]byte(nil), config.InitTemplate...)
	prior := bytes.Replace(desired, []byte("on_baseten_429: true"), []byte("on_baseten_429: false"), 1)
	if bytes.Equal(prior, desired) {
		t.Fatal("fixture did not change fallback policy")
	}
	if err := os.WriteFile(path, desired, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(dir, "router.pid"))
	if err := os.WriteFile(gatewayPidfilePath(), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := mutationOptions{OperationID: "fallback-recovery-projection", hasOperationID: true}
	spec := journaledMutationSpec{
		Operation: "set_fallback_policy", Surface: mutationSurfaceConfig,
		Requested: true, RequestedTarget: "on", Key: "429",
	}
	fingerprint, err := mutationRequestFingerprint(path, opts, spec)
	if err != nil {
		t.Fatal(err)
	}
	desiredHash := exactConfigHash(desired)
	journal := routingMutationJournal{
		Version: mutationJournalVersion, OperationID: opts.OperationID, Operation: spec.Operation,
		Surface: spec.Surface, ConfigPath: path, Requested: true, RequestedTarget: spec.RequestedTarget,
		Key: spec.Key, PreviousConfig: prior, PreviousMode: 0o600,
		PreviousConfigHash: exactConfigHash(prior), DesiredConfigHash: desiredHash,
		PreviousActiveToken: "boot:1", RequestFingerprint: fingerprint, CreatedAt: time.Now().UTC(),
	}
	if err := writeMutationJournal(journal); err != nil {
		t.Fatal(err)
	}
	active := liveRoutingStatus(path, desiredHash, true, 2)
	active.Capabilities = []string{"fallback_policy"}
	active.FallbackPolicy.OnBaseten429 = false
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) { return active, nil }
	status, err := inspectRoutingMutationStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if status.Classification != mutationStatusDesiredPending {
		t.Fatalf("wrong projection classification=%s", status.Classification)
	}
	active.FallbackPolicy.OnBaseten429 = true
	status, err = inspectRoutingMutationStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if status.Classification != mutationStatusDesiredActive {
		t.Fatalf("matching projection classification=%s", status.Classification)
	}
}

func TestFallbackHumanStatusDistinguishesConfiguredAndActiveValues(t *testing.T) {
	active := config.ResolvedFallbackPolicy{OnBaseten429: false, OnBaseten5xx: true}
	status := fallbackReadStatus{
		Configured: config.ResolvedFallbackPolicy{OnBaseten429: true, OnBaseten5xx: true},
		Active:     &active,
		Clients: []fallbackReadClient{{
			Name:            "claude-code",
			ConfiguredModel: "claude-opus-5",
			ActiveModel:     "claude-sonnet-5",
			activeKnown:     true,
		}},
	}
	var out bytes.Buffer
	printFallbackPolicyStatus(&out, status, true)
	text := out.String()
	for _, want := range []string{
		"429   configured on    active off",
		"configured   Opus",
		"active       Sonnet",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("status output missing %q:\n%s", want, text)
		}
	}

	out.Reset()
	printFallbackModelStatus(&out, status.Clients[0], true)
	if !strings.Contains(out.String(), "configured   Opus") || !strings.Contains(out.String(), "active       Sonnet") {
		t.Fatalf("model output did not distinguish configured and active:\n%s", out.String())
	}
}

func TestMutationResultWarningWireRoundTripsBothShapes(t *testing.T) {
	structured := mutationResult{
		OK: true, Operation: "set_fallback_policy",
		StructuredWarnings: []mutationWarning{{Code: "cross_provider_history_may_be_incompatible"}},
	}
	data, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	var decoded mutationResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.StructuredWarnings) != 1 || decoded.StructuredWarnings[0].Code != "cross_provider_history_may_be_incompatible" {
		t.Fatalf("structured warnings=%#v data=%s", decoded.StructuredWarnings, data)
	}
	if err := json.Unmarshal([]byte(`{"ok":true,"warnings":["legacy warning"]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Warnings) != 1 || decoded.Warnings[0] != "legacy warning" {
		t.Fatalf("string warnings=%#v", decoded.Warnings)
	}
}

func TestFallbackPolicyTerminalReplayRetainsStructuredWarning(t *testing.T) {
	spec := journaledMutationSpec{
		Operation: "set_fallback_policy", Surface: mutationSurfaceConfig,
		Requested: true, RequestedTarget: "on", Key: "429",
		StructuredWarnings: []mutationWarning{{Code: "cross_provider_history_may_be_incompatible"}},
	}
	result := resultFromTerminal("/synthetic/gateway.yaml", mutationTerminalRecord{
		OperationID: "policy-replay", Operation: spec.Operation, Requested: true,
		Outcome: mutationOutcomeApplied,
	}, true, spec)
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	warnings := wire["warnings"].([]any)
	if len(warnings) != 1 || warnings[0].(map[string]any)["code"] != "cross_provider_history_may_be_incompatible" {
		t.Fatalf("replay warnings=%#v", wire["warnings"])
	}
}

func TestNormalizeMutationInvocationAcceptsConfigFallback(t *testing.T) {
	got, err := normalizeMutationInvocation([]string{"--json", "--operation-id", "policy-1", "config", "fallback", "5xx", "off"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"config", "fallback", "5xx", "off", "--json", "--operation-id", "policy-1"}
	if !equalStringSlices(got, want) {
		t.Fatalf("normalized=%q want=%q", got, want)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
