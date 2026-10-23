package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupClaudeDesktopPreservesOtherClientsAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	prior := []byte("# preserved\nglobal:\n  routing_enabled: false\nclients:\n  - name: claude-code\n    enabled: true\n    bind_addr: 127.0.0.1:45272\n    protocol_shape: anthropic\n    default_model: example/model-a # preserved too\n# door comment\ndoor:\n  ports: [{bind_addr: '127.0.0.1:45271', router_addr: '127.0.0.1:45272'}]\n")
	if err := os.WriteFile(path, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetupClaudeDesktop(path, "127.0.0.1:45274"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(got, []byte("  - name: claude-desktop"))
	end := bytes.Index(got, []byte("# door comment"))
	if start < 0 || end < start || !bytes.Equal(append(append([]byte{}, got[:start]...), got[end:]...), prior) {
		t.Fatalf("unrelated bytes changed:\n%s", got)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	desktop := f.Clients[1]
	if desktop.Name != ClaudeDesktopClientName || !desktop.Enabled || IsNativeAllowed(desktop) || desktop.DefaultModel != "" || desktop.AuthToken != nil || desktop.ModelPicker == nil || !desktop.ModelPicker.Enabled || len(desktop.ModelPicker.Models) != 0 {
		t.Fatalf("unexpected Desktop config: %#v", desktop)
	}
	if err := SetupClaudeDesktop(path, "127.0.0.1:45374"); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if !bytes.Equal(got, again) {
		t.Fatal("repeated setup changed saved config or custom address")
	}
}

func TestSetupClaudeDesktopRejectsUnsafeAddressesAndCollisions(t *testing.T) {
	for _, address := range []string{"0.0.0.0:45274", "example.com:45274", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:45272"} {
		t.Run(address, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gateway.yaml")
			prior := []byte("global:\n  routing_enabled: true\nclients:\n  - name: claude-code\n    enabled: true\n    bind_addr: 127.0.0.1:45272\n    protocol_shape: anthropic\n    default_model: example/model\n")
			if err := os.WriteFile(path, prior, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := SetupClaudeDesktop(path, address); err == nil {
				t.Fatal("unsafe setup accepted")
			}
			got, _ := os.ReadFile(path)
			if !bytes.Equal(prior, got) {
				t.Fatal("rejected setup wrote config")
			}
		})
	}
}

func TestSetupClaudeDesktopEnablesSavedModelsWithoutRetargeting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	prior := "global:\n  routing_enabled: true\nclients:\n  - name: claude-desktop\n    enabled: false\n    bind_addr: '[::1]:45274'\n    protocol_shape: anthropic\n    allow_native: false\n    model_aliases:\n      claude-baseten-alpha: example/alpha\n    model_picker:\n      enabled: false\n      models:\n        - alias: claude-baseten-alpha\n"
	if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetupClaudeDesktop(path, "127.0.0.1:45274"); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := f.Clients[0]
	if !c.Enabled || c.BindAddr != "[::1]:45274" || !c.ModelPicker.Enabled || len(c.ModelPicker.Models) != 1 || c.ModelAliases["claude-baseten-alpha"] != "example/alpha" {
		t.Fatalf("saved configuration changed: %#v", c)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "auth_token") {
		t.Fatal("setup stored a provider credential")
	}
}
