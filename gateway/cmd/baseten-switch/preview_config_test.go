package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func TestRunConfigPreviewSnapshotHandlesQuotedAndInlineYAML(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "stable.yaml")
	output := filepath.Join(dir, "preview", "gateway.yaml")
	raw := `
global:
  routing_enabled: false
  auth: {anthropic: literal-stable-secret}
  telemetry_dir: /Users/test/.config/baseten-switch/telemetry
clients:
  - name: claude-code
    enabled: true
    "bind_addr": 127.0.0.1:8789
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
    auth_token: {header: Authorization, value: literal-client-secret}
door:
  ports: [{bind_addr: 127.0.0.1:8081, router_addr: 127.0.0.1:8789}]
`
	if err := os.WriteFile(source, []byte(strings.TrimSpace(raw)), 0o600); err != nil {
		t.Fatal(err)
	}
	args := previewConfigArgs{
		Source:     source,
		Output:     output,
		Root:       filepath.Dir(output),
		RouterAddr: "127.0.0.1:45372",
		DoorAddr:   "127.0.0.1:45371",
	}
	var stderr bytes.Buffer
	if code := runConfigPreviewSnapshot(args, &stderr); code != 0 {
		t.Fatalf("snapshot code = %d, stderr = %s", code, stderr.String())
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"literal-stable-secret",
		"literal-client-secret",
		"127.0.0.1:8789",
		"127.0.0.1:8081",
	} {
		if strings.Contains(string(got), forbidden) {
			t.Fatalf("snapshot contains forbidden %q:\n%s", forbidden, got)
		}
	}
	parsed, err := config.Load(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidatePreviewConfig(parsed, args.policy()); err != nil {
		t.Fatalf("saved snapshot is invalid: %v", err)
	}
	stat, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if mode := stat.Mode().Perm(); mode != 0o600 {
		t.Fatalf("snapshot mode = %o, want 600", mode)
	}
}

func TestRunConfigPreviewValidateRejectsQuotedForeignBind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	raw := `
global:
  routing_enabled: false
  auth: {}
  telemetry_dir: ` + dir + `/telemetry
clients:
  - name: allowed
    enabled: true
    bind_addr: 127.0.0.1:45372
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
  - name: hidden
    enabled: true
    "bind_addr": 0.0.0.0:8789
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
door:
  ports:
    - bind_addr: 127.0.0.1:45371
      router_addr: 127.0.0.1:45372
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	args := previewConfigArgs{
		Path:       path,
		Root:       dir,
		RouterAddr: "127.0.0.1:45372",
		DoorAddr:   "127.0.0.1:45371",
	}
	var stderr bytes.Buffer
	if code := runConfigPreviewValidate(args, &stderr); code != 1 {
		t.Fatalf("validate code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "bind_addr") {
		t.Fatalf("stderr = %q, want bind_addr refusal", stderr.String())
	}
}
