package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
)

const customizedConfig = `# exact user bytes must survive in the backup
global:
  routing_enabled: false
clients:
  - name: claude-code
    enabled: true
    protocol_shape: anthropic
    bind_addr: 127.0.0.1:18081
    native_route: anthropic
`

func resetFixture(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(dir, "gateway.pid"))
	return path
}

func TestRunConfigResetReplacesWholeFileAndMakesUniqueExactBackup(t *testing.T) {
	path := resetFixture(t, customizedConfig)
	var out strings.Builder
	if code := runConfigReset(path, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, config.InitTemplate) {
		t.Fatal("reset did not install the canonical template byte for byte")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("installed mode = %04o, want 0600", info.Mode().Perm())
	}

	backups, err := filepath.Glob(path + ".pre-reset-*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != customizedConfig {
		t.Fatal("backup did not preserve the prior exact bytes")
	}
	backupInfo, err := os.Stat(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if backupInfo.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %04o, want 0600", backupInfo.Mode().Perm())
	}

	// A second reset creates a distinct backup rather than clobbering the
	// first one.
	if code := runConfigReset(path, &out); code != 0 {
		t.Fatalf("second reset exit %d:\n%s", code, out.String())
	}
	backups, err = filepath.Glob(path + ".pre-reset-*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 2 {
		t.Fatalf("backups after second reset = %v, want two unique files", backups)
	}
}

func TestCmdConfigResetRequiresYesWithoutTouchingConfig(t *testing.T) {
	path := resetFixture(t, customizedConfig)
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	var code int
	stderr := captureStderr(t, func() {
		code = cmdConfigReset(nil)
	})
	if code != 2 {
		t.Fatalf("exit = %d, want usage exit 2", code)
	}
	if !strings.Contains(stderr, "--yes") || !strings.Contains(stderr, "discards custom routing settings") {
		t.Fatalf("refusal does not explain destructive confirmation:\n%s", stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != customizedConfig {
		t.Fatal("unconfirmed reset changed the config")
	}
	if backups, _ := filepath.Glob(path + ".pre-reset-*.bak"); len(backups) != 0 {
		t.Fatalf("unconfirmed reset created backups: %v", backups)
	}
}

func TestCmdConfigResetUsesStickyActivePath(t *testing.T) {
	path := resetFixture(t, customizedConfig)
	pidPath := filepath.Join(filepath.Dir(path), "state", "gateway.pid")
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", pidPath)
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", "")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidfile.ConfigStatePath(pidPath), []byte(path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := cmdConfigReset([]string{"--yes"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, config.InitTemplate) {
		t.Fatal("sticky active config was not reset")
	}
}

func TestCmdConfigResetBuildsIsolatedPreviewConfig(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gateway.yaml")
	if err := os.WriteFile(path, []byte(customizedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(root, "gateway.pid"))
	routerAddr := "127.0.0.1:28789"
	doorAddr := "127.0.0.1:28790"

	var code int
	stderr := captureStderr(t, func() {
		code = cmdConfigReset([]string{
			"--yes",
			"--preview-root", root,
			"--router-addr", routerAddr,
			"--door-addr", doorAddr,
		})
	})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}

	file, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := config.PreviewPolicy{
		Root:       root,
		RouterAddr: routerAddr,
		DoorAddr:   doorAddr,
	}
	if err := config.ValidatePreviewConfig(file, policy); err != nil {
		t.Fatalf("reset config violates Preview isolation: %v", err)
	}
	if file.Global.TelemetryDir != filepath.Join(root, "telemetry") {
		t.Fatalf("telemetry_dir = %q", file.Global.TelemetryDir)
	}
	if file.Global.Auth["baseten"] != "${BASETEN_API_KEY}" ||
		file.Global.Auth["anthropic"] != "${ANTHROPIC_API_KEY}" ||
		file.Global.Auth["monitor"] != "" {
		t.Fatalf("unexpected Preview auth placeholders: %v", file.Global.Auth)
	}
	if len(file.Clients) != 2 {
		t.Fatalf("unexpected Preview client transformation: %+v", file.Clients)
	}
	clients := make(map[string]config.Client, len(file.Clients))
	for _, client := range file.Clients {
		clients[client.Name] = client
	}
	claude, ok := clients["claude-code"]
	if !ok ||
		!claude.Enabled ||
		claude.BindAddr != routerAddr ||
		claude.AuthToken == nil ||
		claude.AuthToken.Value != "${ANTHROPIC_AUTH_TOKEN}" {
		t.Fatalf("unexpected Preview Claude client transformation: %+v", claude)
	}
	codex, ok := clients["codex"]
	if !ok ||
		codex.Enabled ||
		codex.BindAddr != routerAddr ||
		codex.ProtocolShape != "openai" ||
		codex.AuthToken == nil ||
		codex.AuthToken.Value != "${CODEX_AUTH_TOKEN}" ||
		codex.DefaultModel != "zai-org/GLM-5.2" {
		t.Fatalf("unexpected Preview parked Codex transformation: %+v", codex)
	}
	if file.Door == nil || len(file.Door.Ports) != 1 ||
		file.Door.Ports[0].BindAddr != doorAddr ||
		file.Door.Ports[0].RouterAddr != routerAddr {
		t.Fatalf("unexpected Preview door transformation: %+v", file.Door)
	}
}

func TestCmdConfigResetRequiresCompletePreviewFlags(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gateway.yaml")
	if err := os.WriteFile(path, []byte(customizedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(root, "gateway.pid"))

	cases := [][]string{
		{"--yes", "--preview-root", root},
		{"--yes", "--preview-root", root, "--router-addr", "127.0.0.1:28789"},
		{"--yes", "--router-addr", "127.0.0.1:28789", "--door-addr", "127.0.0.1:28790"},
	}
	for _, args := range cases {
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var code int
		stderr := captureStderr(t, func() {
			code = cmdConfigReset(args)
		})
		if code != 2 {
			t.Fatalf("args %v exit %d, want 2:\n%s", args, code, stderr)
		}
		if !strings.Contains(stderr, "must be supplied together") {
			t.Fatalf("args %v missing complete-set error:\n%s", args, stderr)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatalf("args %v changed config despite incomplete Preview flags", args)
		}
	}
}

func TestRunConfigResetConfirmsLiveActivation(t *testing.T) {
	installRoutingMutationSeams(t)
	path := resetFixture(t, globalRoutingFixtureYAML)
	if err := os.WriteFile(pidfile.Path(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	priorHash := exactFileHash(t, path)
	signals := 0
	signalRouter = func(pid int) error {
		if pid != os.Getpid() {
			t.Fatalf("signal pid = %d, want %d", pid, os.Getpid())
		}
		signals++
		return nil
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		if signals == 0 {
			return liveRoutingStatus(path, priorHash, true, 20), nil
		}
		return liveRoutingStatus(path, exactFileHash(t, path), true, 21), nil
	}

	var out strings.Builder
	if code := runConfigReset(path, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if signals != 1 {
		t.Fatalf("signals = %d, want 1", signals)
	}
	if !strings.Contains(out.String(), "active router confirmed") {
		t.Fatalf("output missing live confirmation:\n%s", out.String())
	}
}

func TestRunConfigResetRollsBackExactBytesWhenActivationFails(t *testing.T) {
	installRoutingMutationSeams(t)
	path := resetFixture(t, globalRoutingFixtureYAML)
	if err := os.WriteFile(pidfile.Path(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	priorHash := exactConfigHash(original)
	signals := 0
	signalRouter = func(int) error {
		signals++
		return nil
	}
	fetchRoutingAdminStatus = func(string) (routingAdminStatus, error) {
		// The router never advances from the prior active config.
		return liveRoutingStatus(path, priorHash, true, 20), nil
	}

	var out strings.Builder
	if code := runConfigReset(path, &out); code != 1 {
		t.Fatalf("exit %d, want failure after rollback:\n%s", code, out.String())
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatal("activation failure did not restore the prior exact bytes")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("restored mode = %04o, want original 0640", info.Mode().Perm())
	}
	if signals != 1 {
		t.Fatalf("signals = %d, want one attempted canonical reload", signals)
	}
	if !strings.Contains(out.String(), "restored and reactivated prior exact config") {
		t.Fatalf("output missing verified rollback:\n%s", out.String())
	}
}
