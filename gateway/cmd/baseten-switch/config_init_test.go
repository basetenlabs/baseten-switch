package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

// TestRunConfigInitCreatesValidConfig covers the happy path: parent
// dirs 0755, file 0600, contents byte-identical to the embedded
// template, the result loads via config.Load, and the claude adapter's
// door resolution works against it.
func TestRunConfigInitCreatesValidConfig(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "nested", "baseten-switch", "gateway.yaml")
	var out strings.Builder
	if code := runConfigInit(path, false, &out); code != 0 {
		t.Fatalf("exit %d, output:\n%s", code, out.String())
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, config.InitTemplate) {
		t.Error("written config differs from the embedded template")
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("config file mode %o, want 0600", st.Mode().Perm())
	}
	for _, d := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		dst, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if dst.Mode().Perm() != 0o755 {
			t.Errorf("created dir %s mode %o, want 0755", d, dst.Mode().Perm())
		}
	}

	// The written config loads and the claude adapter resolves the
	// door port from it (the same resolution `baseten-switch claude on`
	// performs).
	f, err := config.Load(path)
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	port, ports, _, err := claudeDoorPort(f)
	if err != nil {
		t.Fatalf("claude adapter door resolution failed: %v", err)
	}
	if port != "45271" {
		t.Errorf("claude door port %q, want 45271", port)
	}
	for _, p := range []string{"45271", "45272"} {
		if !ports[p] {
			t.Errorf("gateway-owned port set missing %s: %v", p, ports)
		}
	}
	claude := &claudeAdapter{desiredPort: port}
	if got := claude.desiredURL(); got != "http://127.0.0.1:45271" {
		t.Errorf("managed Claude endpoint %q, want http://127.0.0.1:45271", got)
	}

	_, codexPort, codexPorts, err := codexDoorPort(f)
	if err != nil {
		t.Fatalf("codex adapter door resolution failed: %v", err)
	}
	if codexPort != "45271" {
		t.Errorf("codex door port %q, want 45271", codexPort)
	}
	for _, p := range []string{"45271", "45272"} {
		if !codexPorts[p] {
			t.Errorf("codex gateway-owned port set missing %s: %v", p, codexPorts)
		}
	}
	codex := &codexAdapter{desiredPort: codexPort}
	if got := codex.desiredURL(); got != "http://127.0.0.1:45271/v1" {
		t.Errorf("managed Codex endpoint %q, want http://127.0.0.1:45271/v1", got)
	}

	// Next-steps output names the command flow.
	for _, want := range []string{
		"baseten auth login",
		"BASETEN_API_KEY",
		"baseten-switch up --install",
		"baseten-switch claude on",
		"baseten-switch status",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// TestCmdConfigInitHonorsConfigPathEnv proves the subcommand writes to
// BASETEN_SWITCH_CONFIG_PATH when set (the same resolution every other command
// uses).
func TestCmdConfigInitHonorsConfigPathEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom", "gw.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	if code := cmdConfigInit(nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config not written at BASETEN_SWITCH_CONFIG_PATH: %v", err)
	}
	if !bytes.Equal(b, config.InitTemplate) {
		t.Error("written config differs from the embedded template")
	}
}

// TestRunConfigInitRefusesExisting: an existing file is never touched
// without --force, and no backup is created on refusal.
func TestRunConfigInitRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	writeFile(t, path, "original: true\n")
	var out strings.Builder
	if code := runConfigInit(path, false, &out); code != 1 {
		t.Fatalf("want exit 1, got %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "--force") {
		t.Errorf("refusal does not name --force:\n%s", out.String())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "original: true\n" {
		t.Error("refusal modified the existing file")
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Error("refusal created a backup file")
	}
}

// TestRunConfigInitForceBacksUp: --force replaces the file after
// backing the original up to gateway.yaml.bak.
func TestRunConfigInitForceBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	writeFile(t, path, "original: true\n")
	var out strings.Builder
	if code := runConfigInit(path, true, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if string(bak) != "original: true\n" {
		t.Error("backup does not hold the original bytes")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, config.InitTemplate) {
		t.Error("forced write does not match the embedded template")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("config file mode %o after --force, want 0600", st.Mode().Perm())
	}
	if !strings.Contains(out.String(), path+".bak") {
		t.Errorf("output does not name the backup path:\n%s", out.String())
	}
}

// TestCmdConfigInitUnknownFlag: usage error, exit 2.
func TestCmdConfigInitUnknownFlag(t *testing.T) {
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", filepath.Join(t.TempDir(), "gateway.yaml"))
	if code := cmdConfigInit([]string{"--bogus"}); code != 2 {
		t.Fatalf("want exit 2 for unknown flag, got %d", code)
	}
}
