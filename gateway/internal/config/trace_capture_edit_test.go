package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetTraceCaptureInsertsAndPreservesUnrelatedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	before := "global:\n  # preserve me\n  routing_enabled: false # gate\nclients:\n  - name: claude-code\n    enabled: true\n    bind_addr: 127.0.0.1:18081\n    protocol_shape: anthropic\n    default_model: example/model\n"
	if err := os.WriteFile(path, []byte(before), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := SetTraceCapture(path, TraceCapture{Enabled: true, Clients: []string{"claude-code"}}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "  # preserve me\n  routing_enabled: false # gate\n  trace_capture:\n") ||
		!strings.Contains(got, "      - \"claude-code\"\n    retention_days: 7\nclients:\n") {
		t.Fatalf("edited config:\n%s", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestSetTraceCaptureReplacesOnlySubtree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	before := "global:\n  routing_enabled: false\n  trace_capture:\n    # old capture comment\n    enabled: false\n    clients: []\n    retention_days: 3\n  retry_max: 3 # preserve\nclients:\n  - name: claude-code\n    enabled: true\n    bind_addr: 127.0.0.1:18081\n    protocol_shape: anthropic\n    default_model: example/model\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetTraceCapture(path, TraceCapture{Enabled: true, Clients: []string{"claude-code"}, RetentionDays: 14}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	got := string(body)
	if strings.Contains(got, "old capture comment") || !strings.Contains(got, "retention_days: 14\n  retry_max: 3 # preserve") {
		t.Fatalf("edited config:\n%s", got)
	}
}

func TestSetTraceCapturePreservesFourSpaceIndentation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	before := "global:\n    routing_enabled: false\n    retry_max: 3\nclients:\n    - name: claude-code\n      enabled: true\n      bind_addr: 127.0.0.1:18081\n      protocol_shape: anthropic\n      default_model: example/model\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetTraceCapture(path, TraceCapture{
		Enabled: true, Clients: []string{"claude-code"}, RetentionDays: 7,
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	want := "    trace_capture:\n        enabled: true\n        clients:\n            - \"claude-code\"\n        retention_days: 7\nclients:\n"
	if !strings.Contains(got, want) {
		t.Fatalf("edited config does not preserve four-space indentation:\n%s", got)
	}
}

func TestSetTraceCaptureRejectsUnsafePolicyWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	before := "global:\n  routing_enabled: false\nclients:\n  - name: claude-code\n    enabled: true\n    bind_addr: 0.0.0.0:18081\n    protocol_shape: anthropic\n    default_model: example/model\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SetTraceCapture(path, TraceCapture{Enabled: true, Clients: []string{"claude-code"}})
	if err == nil {
		t.Fatal("expected unsafe bind rejection")
	}
	body, _ := os.ReadFile(path)
	if string(body) != before {
		t.Fatalf("rejected edit changed file:\n%s", body)
	}
}
