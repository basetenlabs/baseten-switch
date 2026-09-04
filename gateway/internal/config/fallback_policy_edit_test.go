package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fallbackEditFixture() []byte {
	return []byte(`# header
global:
  routing_enabled: true # keep
  auth: {}
clients:
  - name: claude-code
    enabled: false
    protocol_shape: anthropic
    fallback_route: anthropic # route
# footer
`)
}

func TestSetFallbackPolicyTriggerCreatesAndUpdatesOneField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	original := fallbackEditFixture()
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := SetFallbackPolicyTrigger(path, "429", false); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(first, []byte("  fallback_policy:\n    on_baseten_429: false\n")) {
		t.Fatalf("created policy is missing:\n%s", first)
	}
	if !bytes.Contains(first, []byte("routing_enabled: true # keep")) || !bytes.HasSuffix(first, []byte("# footer\n")) {
		t.Fatalf("unrelated bytes changed:\n%s", first)
	}
	if err := SetFallbackPolicyTrigger(path, "5xx", false); err != nil {
		t.Fatal(err)
	}
	if err := SetFallbackPolicyTrigger(path, "429", true); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved := ResolveFallbackPolicy(file.Global.FallbackPolicy)
	if !resolved.OnBaseten429 || resolved.OnBaseten5xx {
		t.Fatalf("resolved policy = %+v, want 429 on and 5xx off", resolved)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
}

func TestSetFallbackPolicyTriggerRejectsInvalidTriggerAndFlowStyle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, fallbackEditFixture(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetFallbackPolicyTrigger(path, "500", true); err == nil || !strings.Contains(err.Error(), "allowed: 429, 5xx") {
		t.Fatalf("invalid trigger error = %v", err)
	}
	flow := bytes.Replace(fallbackEditFixture(), []byte("  auth: {}"), []byte("  fallback_policy: {on_baseten_429: true}"), 1)
	if err := os.WriteFile(path, flow, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetFallbackPolicyTrigger(path, "429", false); err == nil || !strings.Contains(err.Error(), "editable block mapping") {
		t.Fatalf("flow-style error = %v", err)
	}
}

func TestSetClientNativeFallbackModelPreservesOtherBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	original := fallbackEditFixture()
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetClientNativeFallbackModel(path, "claude-code", DefaultClaudeNativeFallbackModel); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("    native_fallback_model: claude-opus-5\n")) {
		t.Fatalf("native target was not inserted:\n%s", got)
	}
	if !bytes.Contains(got, []byte("fallback_route: anthropic # route")) || !bytes.HasSuffix(got, []byte("# footer\n")) {
		t.Fatalf("unrelated bytes changed:\n%s", got)
	}
	if err := SetClientNativeFallbackModel(path, "claude-code", "opus"); err == nil || !strings.Contains(err.Error(), "full anthropic-native model ID") {
		t.Fatalf("invalid model error = %v", err)
	}
}
