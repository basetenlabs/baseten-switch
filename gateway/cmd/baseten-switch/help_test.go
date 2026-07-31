package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var expectedAdvertisedCommands = []string{
	"up",
	"down",
	"uninstall",
	"status",
	"restart",
	"on",
	"off",
	"mutation",
	"menubar",
	"door",
	"gateway",
	"config",
	"setup",
	"spend",
	"whoami",
	"auth",
	"doctor",
	"claude",
	"codex",
	"pi",
}

func TestRootHelpUsesOneLinePerAdvertisedCommand(t *testing.T) {
	out := captureStderr(t, usage)
	lines := strings.Split(out, "\n")

	start := -1
	end := -1
	for i, line := range lines {
		switch line {
		case "Commands:":
			start = i + 1
		case "":
			if start >= 0 && end < 0 && i >= start {
				end = i
			}
		}
	}
	if start < 0 || end < start {
		t.Fatalf("command section not found:\n%s", out)
	}
	commandLines := lines[start:end]
	if len(commandLines) != len(expectedAdvertisedCommands) {
		t.Fatalf("command lines = %d, expected commands = %d:\n%s",
			len(commandLines), len(expectedAdvertisedCommands), out)
	}
	if len(commandHelpEntries) != len(expectedAdvertisedCommands) {
		t.Fatalf("help registry entries = %d, expected commands = %d",
			len(commandHelpEntries), len(expectedAdvertisedCommands))
	}
	for i, name := range expectedAdvertisedCommands {
		if commandHelpEntries[i].name != name {
			t.Errorf("help registry command %d = %q, want %q",
				i, commandHelpEntries[i].name, name)
		}
		wantPrefix := fmt.Sprintf("  %-14s ", name)
		if !strings.HasPrefix(commandLines[i], wantPrefix) {
			t.Errorf("command line %d = %q, want prefix %q", i, commandLines[i], wantPrefix)
		}
		if strings.Contains(commandHelpEntries[i].summary, "\n") {
			t.Errorf("%s summary contains a newline", name)
		}
	}
}

func TestEveryAdvertisedCommandHasDetailedHelp(t *testing.T) {
	for _, entry := range commandHelpEntries {
		t.Run(entry.name, func(t *testing.T) {
			var out bytes.Buffer
			if !printCommandHelp(&out, entry.name) {
				t.Fatalf("printCommandHelp(%q) = false", entry.name)
			}
			if !strings.HasPrefix(out.String(), "Usage:") {
				t.Fatalf("detailed help does not start with Usage:\n%s", out.String())
			}
		})
	}
}

func TestHiddenHealthzHasDetailedHelp(t *testing.T) {
	root := captureStderr(t, usage)
	if strings.Contains(root, "\n  healthz") {
		t.Fatalf("hidden healthz command was advertised:\n%s", root)
	}
	detail := captureStderr(t, func() {
		if code := dispatch([]string{"healthz", "--help"}); code != 0 {
			t.Errorf("dispatch(healthz --help) = %d, want 0", code)
		}
	})
	if !strings.HasPrefix(detail, "Usage: baseten-switch healthz") ||
		!strings.Contains(detail, "BASETEN_SWITCH_GATEWAY_PORT") {
		t.Fatalf("healthz detailed help is incomplete:\n%s", detail)
	}
}

func TestDoctorDetailedHelpIncludesTimeout(t *testing.T) {
	var out bytes.Buffer
	if !printCommandHelp(&out, "doctor") {
		t.Fatal("doctor detailed help is unavailable")
	}
	if !strings.Contains(out.String(), "--timeout SEC") ||
		!strings.Contains(out.String(), "--timeout  Per-probe HTTP timeout") {
		t.Fatalf("doctor help does not document --timeout:\n%s", out.String())
	}
}

func TestDetailedHelpRetainsEnvironmentGuidance(t *testing.T) {
	tests := []struct {
		command string
		terms   []string
	}{
		{"up", []string{"BASETEN_SWITCH_LAUNCHD=off", "BASETEN_SWITCH_MENUBAR=off", "BASETEN_SWITCH_CONFIG_PATH"}},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			var out bytes.Buffer
			if !printCommandHelp(&out, test.command) {
				t.Fatalf("%s detailed help is unavailable", test.command)
			}
			for _, term := range test.terms {
				if !strings.Contains(out.String(), term) {
					t.Errorf("%s help is missing %s:\n%s", test.command, term, out.String())
				}
			}
		})
	}
}

func TestDetailedHelpDoesNotClaimTelemetryDirOverride(t *testing.T) {
	for _, command := range []string{"gateway", "spend"} {
		t.Run(command, func(t *testing.T) {
			var out bytes.Buffer
			if !printCommandHelp(&out, command) {
				t.Fatalf("%s detailed help is unavailable", command)
			}
			if strings.Contains(out.String(), "BASETEN_SWITCH_TELEMETRY_DIR") {
				t.Fatalf("%s help claims unsupported BASETEN_SWITCH_TELEMETRY_DIR override:\n%s",
					command, out.String())
			}
		})
	}
}

func TestDispatchInterceptsLongAndShortCommandHelp(t *testing.T) {
	for _, helpFlag := range []string{"--help", "-h"} {
		t.Run(helpFlag, func(t *testing.T) {
			out := captureStderr(t, func() {
				if code := dispatch([]string{"up", helpFlag}); code != 0 {
					t.Errorf("dispatch(up %s) = %d, want 0", helpFlag, code)
				}
			})
			if !strings.HasPrefix(out, "Usage: baseten-switch up") {
				t.Fatalf("help output = %q", out)
			}
		})
	}
}

func TestCommandHelpRecognizesUnsafeNestedPaths(t *testing.T) {
	for _, args := range [][]string{
		{"gateway", "stop", "--help"},
		{"gateway", "stop", "--operation-id", "--help"},
		{"config", "reset", "--help"},
		{"config", "reset", "--preview-root", "--help"},
		{"claude", "on", "--help"},
		{"codex", "on", "-h"},
		{"pi", "install", "--help"},
		{"healthz", "--help"},
		{"mutation", "reconcile", "--operation-id", "--help"},
	} {
		if !commandHelpRequested(args) {
			t.Errorf("commandHelpRequested(%v) = false", args)
		}
	}
}

func TestDoorOptionValuesAreNotHelp(t *testing.T) {
	for _, name := range []string{
		"config",
		"port",
		"cooldown",
		"probe-interval",
		"anthropic-url",
		"openai-url",
	} {
		for _, prefix := range []string{"-", "--"} {
			args := []string{"door", prefix + name, "--help"}
			if commandHelpRequested(args) {
				t.Errorf("consumed %s%s value was treated as help: %v",
					prefix, name, args)
			}
		}
	}
}

func TestCommandHelpDoesNotConsumeMutationOptionValue(t *testing.T) {
	normalized, err := normalizeMutationInvocation([]string{
		"--operation-id", "--help", "off",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"off", "--operation-id", "--help"}
	if strings.Join(normalized, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalized args = %v, want %v", normalized, want)
	}
	if commandHelpRequested(normalized) {
		t.Fatalf("consumed --operation-id value was treated as help: %v", normalized)
	}
}

func TestDispatchPreservesRootExitBehavior(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"long help", []string{"--help"}, 0},
		{"short help", []string{"-h"}, 0},
		{"help command", []string{"help"}, 0},
		{"unknown", []string{"not-a-command"}, 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			captureStderr(t, func() {
				if got := dispatch(test.args); got != test.want {
					t.Errorf("dispatch(%v) = %d, want %d", test.args, got, test.want)
				}
			})
		})
	}
}

func TestHelpIsInterceptedBeforeMutatingGatewayStop(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "gateway.pid")
	if err := os.WriteFile(pidPath, []byte("9999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", pidPath)

	out := captureStderr(t, func() {
		if code := dispatch([]string{"gateway", "stop", "--help"}); code != 0 {
			t.Errorf("dispatch(gateway stop --help) = %d, want 0", code)
		}
	})
	if !strings.HasPrefix(out, "Usage: baseten-switch gateway") {
		t.Fatalf("help output = %q", out)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("gateway stop handler acted on the pidfile: %v", err)
	}
}

func TestUnknownCommandHelpKeepsUnknownCommandBehavior(t *testing.T) {
	out := captureStderr(t, func() {
		if code := dispatch([]string{"not-a-command", "--help"}); code != 2 {
			t.Errorf("dispatch(unknown --help) = %d, want 2", code)
		}
	})
	if !strings.HasPrefix(out, "unknown command: not-a-command\n") {
		t.Fatalf("unknown-command output = %q", out)
	}
}

func TestRemovedCommandsAreUnknown(t *testing.T) {
	for _, args := range [][]string{
		{"tier"},
		{"tier", "--help"},
		{"session-check"},
		{"session-check", "--help"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			out := captureStderr(t, func() {
				if code := dispatch(args); code != 2 {
					t.Errorf("dispatch(%v) = %d, want 2", args, code)
				}
			})
			if want := "unknown command: " + args[0] + "\n"; !strings.HasPrefix(out, want) {
				t.Fatalf("dispatch(%v) output = %q, want prefix %q", args, out, want)
			}
		})
	}
}
