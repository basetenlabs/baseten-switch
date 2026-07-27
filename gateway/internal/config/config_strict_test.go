package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsUnknownTopLevelField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte("global:\n  routing_enabled: true\nmystery: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "field mystery not found") {
		t.Fatalf("Load error = %v, want unknown top-level field", err)
	}
}

func TestValidateRoutingPolicyRequiresProtocolShape(t *testing.T) {
	enabled := false
	err := ValidateRoutingPolicy(&File{
		Global: Global{RoutingEnabled: &enabled},
		Clients: []Client{{
			Name: "claude-code",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "protocol_shape must be explicitly") {
		t.Fatalf("ValidateRoutingPolicy() error = %v", err)
	}
}
