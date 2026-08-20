package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseTracePackageOptionsAnchorsRelativeBoundsOnce(t *testing.T) {
	anchor := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)
	options, err := parseTracePackageOptions([]string{
		"--since", "24h",
		"--until", "30m",
		"--client", "claude-code",
		"--client", "codex",
		"--include-native",
		"--require-native",
		"--yes",
	}, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if want := anchor.Add(-24 * time.Hour); !options.selection.Since.Equal(want) {
		t.Fatalf("since = %s, want %s", options.selection.Since, want)
	}
	if want := anchor.Add(-30 * time.Minute); !options.selection.Until.Equal(want) {
		t.Fatalf("until = %s, want %s", options.selection.Until, want)
	}
	if len(options.selection.Clients) != 2 || !options.includeNative || !options.requireNative || !options.yes {
		t.Fatalf("options = %#v", options)
	}
}

func TestParseTracePackageOptionsRFC3339AndDefaultUntil(t *testing.T) {
	anchor := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)
	options, err := parseTracePackageOptions([]string{
		"--since", "2026-08-17T18:00:00-02:00",
		"--client", "claude-code",
	}, anchor.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !options.selection.Since.Equal(anchor) || !options.selection.Until.Equal(anchor.Add(time.Hour)) {
		t.Fatalf("selection = %#v", options.selection)
	}
}

func TestParseTracePackageOptionsRejectsUnsafeCombinations(t *testing.T) {
	anchor := time.Now().UTC()
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--client", "codex"}, "--since is required"},
		{[]string{"--since", "1h"}, "at least one --client"},
		{[]string{"--since", "1h", "--client", "codex", "--include-codex-archived"}, "requires --include-native"},
		{[]string{"--since", "1h", "--client", "codex", "--require-native"}, "requires --include-native"},
		{[]string{"--since", "1h", "--client", "codex", "--yes", "--dry-run"}, "not valid with --dry-run"},
		{[]string{"--since", "0s", "--client", "codex"}, "duration must not be zero"},
		{[]string{"--since", "31d", "--client", "codex"}, "expected RFC 3339"},
	}
	for _, test := range tests {
		_, err := parseTracePackageOptions(test.args, anchor)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("parse(%q) error = %v, want %q", test.args, err, test.want)
		}
	}
}

func TestParseTracePolicyOptions(t *testing.T) {
	options, err := parseTracePolicyOptions([]string{
		"--client", "claude-code",
		"--client", "codex",
		"--retention-days", "14",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.retentionDays != 14 || len(options.clients) != 2 {
		t.Fatalf("options = %#v", options)
	}
}
