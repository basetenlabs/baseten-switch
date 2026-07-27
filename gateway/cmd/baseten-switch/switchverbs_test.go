package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func writeSwitchFixture(t *testing.T, yamlContent string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(dir, "gateway.pid"))
	return path
}

func recordSignals(t *testing.T) *[]int {
	t.Helper()
	var got []int
	old := signalRouter
	signalRouter = func(pid int) error {
		got = append(got, pid)
		return nil
	}
	t.Cleanup(func() { signalRouter = old })
	return &got
}

func TestRunSwitchEditsOnlyGlobalRoutingEnabled(t *testing.T) {
	fixture := `# comments and model mappings survive
global:
  routing_enabled: true  # one global gate
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
    model_routes:
      opus: zai-org/GLM-5.2
`
	path := writeSwitchFixture(t, fixture)
	recordSignals(t)
	var out strings.Builder
	stderr := captureStderr(t, func() {
		if code := runSwitch("off", nil, &out); code != 0 {
			t.Fatalf("code = %d, stdout:\n%s", code, out.String())
		}
	})
	file, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if file.Global.RoutingEnabled == nil || *file.Global.RoutingEnabled {
		t.Fatalf("routing_enabled = %#v, want false", file.Global.RoutingEnabled)
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(bytes)
	for _, preserved := range []string{
		"# comments and model mappings survive",
		"routing_enabled: false  # one global gate",
		"opus: zai-org/GLM-5.2",
	} {
		if !strings.Contains(got, preserved) {
			t.Fatalf("edited config missing %q:\n%s", preserved, got)
		}
	}
	if !strings.Contains(out.String(), "global routing OFF") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", out.String(), stderr)
	}
}

func TestRunSwitchRejectsClientArgument(t *testing.T) {
	path := writeSwitchFixture(t, "global:\n  routing_enabled: true\nclients: []\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	stderr := captureStderr(t, func() {
		if code := runSwitch("off", []string{"claude-code"}, &out); code != 2 {
			t.Fatalf("code = %d, output:\n%s", code, out.String())
		}
	})
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("client-scoped invocation changed config")
	}
	if !strings.Contains(stderr, "takes no client argument") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", out.String(), stderr)
	}
}

func TestRunSwitchRequiresGlobalRoutingEnabled(t *testing.T) {
	writeSwitchFixture(t, "global: {}\nclients: []\n")
	var out strings.Builder
	stderr := captureStderr(t, func() {
		if code := runSwitch("on", nil, &out); code != 1 {
			t.Fatalf("code = %d, output:\n%s", code, out.String())
		}
	})
	if !strings.Contains(stderr, "global.routing_enabled") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", out.String(), stderr)
	}
}

func TestRecordSignalsHelperDoesNotSignalRealProcess(t *testing.T) {
	got := recordSignals(t)
	if err := signalRouter(42); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(*got) != "[42]" {
		t.Fatalf("signals = %v", *got)
	}
}
