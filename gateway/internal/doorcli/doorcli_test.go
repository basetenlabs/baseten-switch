package doorcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/door"
)

// TestParseFlagsSurface pins the flag surface of `baseten-switch door`,
// which defines the public `baseten-switch door` interface: --config,
// --port, --cooldown, --probe-interval, --anthropic-url, --openai-url.
func TestParseFlagsSurface(t *testing.T) {
	var errBuf bytes.Buffer
	opts, err := ParseFlags([]string{
		"--port", "28081=anthropic:127.0.0.1:28181",
		"--port", "28082=openai:127.0.0.1:28182",
		"--config", "/tmp/x.yaml",
		"--cooldown", "9s",
		"--probe-interval", "2s",
		"--anthropic-url", "https://a.example",
		"--openai-url", "https://o.example",
	}, &errBuf)
	if err != nil {
		t.Fatalf("ParseFlags: %v (stderr: %s)", err, errBuf.String())
	}
	if len(opts.Ports) != 2 {
		t.Fatalf("ports = %d want 2", len(opts.Ports))
	}
	if opts.Ports[0].Shape != door.ShapeAnthropic || opts.Ports[0].Port != 28081 {
		t.Fatalf("port[0] = %+v", opts.Ports[0])
	}
	if opts.ConfigPath != "/tmp/x.yaml" {
		t.Fatalf("config = %q", opts.ConfigPath)
	}
	if opts.Cooldown != 9*time.Second || opts.ProbeInterval != 2*time.Second {
		t.Fatalf("cooldown=%s probe=%s", opts.Cooldown, opts.ProbeInterval)
	}
	if opts.AnthropicURL != "https://a.example" || opts.OpenAIURL != "https://o.example" {
		t.Fatalf("urls = %q %q", opts.AnthropicURL, opts.OpenAIURL)
	}
	for _, name := range []string{"port", "config", "cooldown", "probe-interval", "anthropic-url", "openai-url"} {
		if !opts.Explicit[name] {
			t.Fatalf("flag %q not recorded as explicit", name)
		}
	}
}

func TestParseFlagsErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"--bogus"}},
		{"bad port spec", []string{"--port", "notaport=anthropic:127.0.0.1:1"}},
		{"bad shape", []string{"--port", "28081=grpc:127.0.0.1:1"}},
		{"bad duration", []string{"--cooldown", "banana"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errBuf bytes.Buffer
			if _, err := ParseFlags(tc.args, &errBuf); err == nil {
				t.Fatalf("ParseFlags(%v) expected error", tc.args)
			}
		})
	}
}

func TestBuildSpecsFlagsMode(t *testing.T) {
	var errBuf bytes.Buffer
	opts, err := ParseFlags([]string{
		"--port", "28081=anthropic:127.0.0.1:28181",
		"--port", "28082=openai:127.0.0.1:28182",
		"--cooldown", "7s",
	}, &errBuf)
	if err != nil {
		t.Fatal(err)
	}
	specs, cfgPath, err := BuildSpecs(opts, &errBuf)
	if err != nil {
		t.Fatal(err)
	}
	if cfgPath != "" {
		t.Fatalf("flags mode should not report a config path, got %q", cfgPath)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %d want 2", len(specs))
	}
	if specs[0].ListenAddr != "127.0.0.1:28081" || specs[0].Shape != door.ShapeAnthropic {
		t.Fatalf("spec[0] = %+v", specs[0])
	}
	if specs[0].Cooldown != 7*time.Second {
		t.Fatalf("cooldown = %s want 7s", specs[0].Cooldown)
	}
	if specs[1].FallbackBase != door.DefaultOpenAIBase {
		t.Fatalf("openai fallback = %q", specs[1].FallbackBase)
	}
	if !strings.Contains(errBuf.String(), "config ignored") {
		t.Fatalf("missing flags-mode notice in %q", errBuf.String())
	}
}

func TestBuildSpecsFlagsModeErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"duplicate port", []string{"--port", "28081=anthropic:127.0.0.1:1", "--port", "28081=openai:127.0.0.1:2"}, "duplicate"},
		{"zero cooldown", []string{"--port", "28081=anthropic:127.0.0.1:1", "--cooldown", "0s"}, "--cooldown must be positive"},
		{"zero probe", []string{"--port", "28081=anthropic:127.0.0.1:1", "--probe-interval", "0s"}, "--probe-interval must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errBuf bytes.Buffer
			opts, err := ParseFlags(tc.args, &errBuf)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = BuildSpecs(opts, &errBuf)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildSpecs error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestBuildSpecsConfigMode(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gateway.yaml")
	yaml := `
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:28181
    protocol_shape: anthropic
door:
  cooldown: 10s
  probe_interval: 2s
  ports:
    - bind_addr: 127.0.0.1:28081
      router_addr: 127.0.0.1:28181
`
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	var errBuf bytes.Buffer
	opts, err := ParseFlags([]string{"--config", cfg}, &errBuf)
	if err != nil {
		t.Fatal(err)
	}
	specs, cfgPath, err := BuildSpecs(opts, &errBuf)
	if err != nil {
		t.Fatal(err)
	}
	if cfgPath != cfg {
		t.Fatalf("cfgPath = %q want %q", cfgPath, cfg)
	}
	if len(specs) != 1 || specs[0].ListenAddr != "127.0.0.1:28081" {
		t.Fatalf("specs = %+v", specs)
	}
	if specs[0].Cooldown != 10*time.Second {
		t.Fatalf("cooldown = %s want 10s (from door.cooldown)", specs[0].Cooldown)
	}
}

func TestBuildSpecsConfigModeMissingFile(t *testing.T) {
	var errBuf bytes.Buffer
	opts, err := ParseFlags([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml")}, &errBuf)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := BuildSpecs(opts, &errBuf); err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestBuildSpecsConfigModeNoDoorSection(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(cfg, []byte("clients: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var errBuf bytes.Buffer
	opts, err := ParseFlags([]string{"--config", cfg}, &errBuf)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = BuildSpecs(opts, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "door") {
		t.Fatalf("expected no-door-section error, got %v", err)
	}
}
