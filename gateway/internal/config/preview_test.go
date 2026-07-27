package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func previewPolicyForTest(root string) PreviewPolicy {
	return PreviewPolicy{
		Root:       root,
		RouterAddr: "127.0.0.1:45372",
		DoorAddr:   "127.0.0.1:45371",
	}
}

func TestBuildPreviewConfigSanitizesInlineCredentialsAndQuotedBindKeys(t *testing.T) {
	const raw = `
global:
  routing_enabled: false
  auth: {baseten: literal-baseten-secret, anthropic: literal-anthropic-secret}
  telemetry_dir: /Users/test/.config/baseten-switch/telemetry
clients:
  - name: claude-code
    enabled: true
    "bind_addr": 0.0.0.0:8789
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
    auth_token: {header: Authorization, value: literal-client-secret}
door:
  ports: [{bind_addr: 0.0.0.0:8081, router_addr: 127.0.0.1:8789}]
  cooldown: 19s
  probe_interval: 7s
	`
	var source File
	if err := yaml.Unmarshal([]byte(strings.TrimSpace(raw)), &source); err != nil {
		t.Fatal(err)
	}
	policy := previewPolicyForTest(t.TempDir())
	got, err := BuildPreviewConfig(&source, policy)
	if err != nil {
		t.Fatalf("BuildPreviewConfig: %v", err)
	}
	if got.Global.Auth["baseten"] != "${BASETEN_API_KEY}" ||
		got.Global.Auth["anthropic"] != "${ANTHROPIC_API_KEY}" {
		t.Fatalf("global auth was not sanitized: %#v", got.Global.Auth)
	}
	client := got.Clients[0]
	if client.BindAddr != policy.RouterAddr {
		t.Fatalf("bind_addr = %q, want %q", client.BindAddr, policy.RouterAddr)
	}
	if client.AuthToken == nil || client.AuthToken.Value != "${ANTHROPIC_AUTH_TOKEN}" {
		t.Fatalf("client auth was not sanitized: %#v", client.AuthToken)
	}
	if got.Door.Cooldown != "19s" || got.Door.ProbeInterval != "7s" {
		t.Fatalf("door policy fields were not preserved: %#v", got.Door)
	}
	if len(got.Door.Ports) != 1 ||
		got.Door.Ports[0].BindAddr != policy.DoorAddr ||
		got.Door.Ports[0].RouterAddr != policy.RouterAddr {
		t.Fatalf("door ports were not isolated: %#v", got.Door.Ports)
	}
}

func TestValidatePreviewConfigRejectsHiddenListenerAndLiteralCredential(t *testing.T) {
	policy := previewPolicyForTest(t.TempDir())
	enabled := false
	source := &File{
		Global: Global{
			RoutingEnabled: &enabled,
			Auth:           map[string]string{"anthropic": "${ANTHROPIC_API_KEY}"},
			TelemetryDir:   policy.Root + "/telemetry",
		},
		Clients: []Client{{
			Name:          "claude-code",
			Enabled:       true,
			BindAddr:      policy.RouterAddr,
			ProtocolShape: "anthropic",
			DefaultModel:  "zai-org/GLM-5.2",
			AuthToken: &AuthToken{
				Header: "Authorization",
				Value:  "${ANTHROPIC_AUTH_TOKEN}",
			},
		}},
		Door: &Door{Ports: []DoorPort{{
			BindAddr:   policy.DoorAddr,
			RouterAddr: policy.RouterAddr,
		}}},
	}
	if err := ValidatePreviewConfig(source, policy); err != nil {
		t.Fatalf("valid Preview config: %v", err)
	}
	source.Clients[0].BindAddr = "0.0.0.0:8789"
	if err := ValidatePreviewConfig(source, policy); err == nil ||
		!strings.Contains(err.Error(), "bind_addr") {
		t.Fatalf("hidden listener error = %v", err)
	}
	source.Clients[0].BindAddr = policy.RouterAddr
	source.Global.Auth["anthropic"] = "literal-secret"
	if err := ValidatePreviewConfig(source, policy); err == nil ||
		!strings.Contains(err.Error(), "global auth") {
		t.Fatalf("literal credential error = %v", err)
	}
}

func TestEnvFilePathHonorsExplicitOverride(t *testing.T) {
	want := t.TempDir() + "/preview-env"
	t.Setenv("BASETEN_SWITCH_ENV_FILE", want)
	if got := EnvFilePath(); got != want {
		t.Fatalf("EnvFilePath() = %q, want %q", got, want)
	}
}
