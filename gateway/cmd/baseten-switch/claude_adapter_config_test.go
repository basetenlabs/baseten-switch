package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func TestLoadGatewayConfigForAdapterReportsMissingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	cfg, loadedPath, err := loadGatewayConfigForAdapter()
	if cfg != nil || loadedPath != "" || err == nil || err.Error() != config.MissingConfigMessage(path) {
		t.Fatalf("missing config cfg=%v path=%q err=%v", cfg, loadedPath, err)
	}
}

func TestLoadGatewayConfigForAdapterRejectsMalformedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte("clients: ["), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	cfg, loadedPath, err := loadGatewayConfigForAdapter()
	if cfg != nil || loadedPath != "" || err == nil || !strings.Contains(err.Error(), "is malformed:") || strings.Contains(err.Error(), "no gateway config at") {
		t.Fatalf("malformed config cfg=%v path=%q err=%v", cfg, loadedPath, err)
	}
}

func TestLoadGatewayConfigForAdapterRejectsUnreadableConfig(t *testing.T) {
	path := t.TempDir()
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	cfg, loadedPath, err := loadGatewayConfigForAdapter()
	if cfg != nil || loadedPath != "" || err == nil || !strings.Contains(err.Error(), "is malformed:") {
		t.Fatalf("unreadable config cfg=%v path=%q err=%v", cfg, loadedPath, err)
	}
}
