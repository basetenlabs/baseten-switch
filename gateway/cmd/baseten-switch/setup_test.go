package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestDependencies(path string) setupDependencies {
	return setupDependencies{
		findBaseten:    func() (string, error) { return "/test/bin/baseten", nil },
		basetenVersion: func(string) string { return "baseten version v0.3.0" },
		loadCredential: func() (string, error) {
			return "current OAuth credential is available", nil
		},
		login:      func(string) error { return nil },
		configPath: func() string { return path },
		stat:       os.Stat,
		initConfig: func(string, bool, io.Writer) int { return 0 },
	}
}

func TestSetupAlreadySignedInInitializesConfigAndPrintsNextCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	deps := setupTestDependencies(path)
	loginCalls := 0
	initCalls := 0
	deps.login = func(string) error {
		loginCalls++
		return nil
	}
	deps.initConfig = func(gotPath string, force bool, out io.Writer) int {
		initCalls++
		if gotPath != path || force {
			t.Fatalf("initConfig(%q, %v), want (%q, false)", gotPath, force, path)
		}
		return 0
	}

	var stdout, stderr bytes.Buffer
	if code := runSetup(deps, &stdout, &stderr); code != 0 {
		t.Fatalf("runSetup = %d, want 0\nstderr:\n%s", code, stderr.String())
	}
	if loginCalls != 0 {
		t.Fatalf("login calls = %d, want 0", loginCalls)
	}
	if initCalls != 1 {
		t.Fatalf("init calls = %d, want 1", initCalls)
	}
	if !strings.Contains(stdout.String(), "Gateway config: created "+path) {
		t.Fatalf("stdout missing config creation:\n%s", stdout.String())
	}
	assertSetupNextCommands(t, stdout.String())
}

func TestSetupDelegatesMissingCredentialToBasetenLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	deps := setupTestDependencies(path)
	loadCalls := 0
	deps.loadCredential = func() (string, error) {
		loadCalls++
		if loadCalls == 1 {
			return "", errSetupCredentialUnavailable
		}
		return "current OAuth credential is available", nil
	}
	loginCalls := 0
	deps.login = func(bin string) error {
		loginCalls++
		if bin != "/test/bin/baseten" {
			t.Fatalf("login binary = %q", bin)
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	if code := runSetup(deps, &stdout, &stderr); code != 0 {
		t.Fatalf("runSetup = %d, want 0\nstderr:\n%s", code, stderr.String())
	}
	if loadCalls != 2 || loginCalls != 1 {
		t.Fatalf("load calls = %d, login calls = %d; want 2, 1", loadCalls, loginCalls)
	}
	if !strings.Contains(stdout.String(), "starting 'baseten auth login'") {
		t.Fatalf("stdout missing delegated-login notice:\n%s", stdout.String())
	}
}

func TestSetupRejectsOldBasetenCLI(t *testing.T) {
	deps := setupTestDependencies(filepath.Join(t.TempDir(), "gateway.yaml"))
	deps.basetenVersion = func(string) string { return "baseten 0.2.99" }
	credentialCalls := 0
	deps.loadCredential = func() (string, error) {
		credentialCalls++
		return "credential", nil
	}

	var stdout, stderr bytes.Buffer
	if code := runSetup(deps, &stdout, &stderr); code != 1 {
		t.Fatalf("runSetup = %d, want 1", code)
	}
	if credentialCalls != 0 {
		t.Fatalf("credential calls = %d, want 0", credentialCalls)
	}
	if !strings.Contains(stderr.String(), "v0.3.0 or newer is required") {
		t.Fatalf("stderr missing minimum version diagnosis:\n%s", stderr.String())
	}
}

func TestSetupKeepsExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	original := []byte("existing: config\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	deps := setupTestDependencies(path)
	initCalls := 0
	deps.initConfig = func(string, bool, io.Writer) int {
		initCalls++
		return 0
	}

	var stdout, stderr bytes.Buffer
	if code := runSetup(deps, &stdout, &stderr); code != 0 {
		t.Fatalf("runSetup = %d, want 0\nstderr:\n%s", code, stderr.String())
	}
	if initCalls != 0 {
		t.Fatalf("init calls = %d, want 0", initCalls)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("existing config changed: got %q, want %q", after, original)
	}
	if !strings.Contains(stdout.String(), "Gateway config: using existing "+path) {
		t.Fatalf("stdout missing existing-config notice:\n%s", stdout.String())
	}
}

func TestSetupReportsDelegatedLoginFailure(t *testing.T) {
	deps := setupTestDependencies(filepath.Join(t.TempDir(), "gateway.yaml"))
	deps.loadCredential = func() (string, error) {
		return "", errSetupCredentialUnavailable
	}
	deps.login = func(string) error { return errors.New("device flow failed") }
	initCalls := 0
	deps.initConfig = func(string, bool, io.Writer) int {
		initCalls++
		return 0
	}

	var stdout, stderr bytes.Buffer
	if code := runSetup(deps, &stdout, &stderr); code != 1 {
		t.Fatalf("runSetup = %d, want 1", code)
	}
	if initCalls != 0 {
		t.Fatalf("init calls = %d, want 0", initCalls)
	}
	if !strings.Contains(stderr.String(), "baseten auth login failed: device flow failed") {
		t.Fatalf("stderr missing login failure:\n%s", stderr.String())
	}
}

func TestSetupReportsConfigInitializationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	deps := setupTestDependencies(path)
	deps.initConfig = func(string, bool, io.Writer) int { return 1 }

	var stdout, stderr bytes.Buffer
	if code := runSetup(deps, &stdout, &stderr); code != 1 {
		t.Fatalf("runSetup = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "could not initialize gateway config at "+path) {
		t.Fatalf("stderr missing config failure:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "Next commands:") {
		t.Fatalf("stdout printed next commands after failure:\n%s", stdout.String())
	}
}

func TestLoadCurrentSetupCredentialUsesCurrentStore(t *testing.T) {
	writeAuthJSON(t, `{
  "version": 1,
  "current": "developer@example.com",
  "profiles": {
    "developer@example.com": {
      "auth_type": "oauth",
      "oauth_credential": {
        "access_token": "current-access-token",
        "refresh_token": "current-refresh-token"
      }
    }
  }
}`)

	got, err := loadCurrentSetupCredential()
	if err != nil {
		t.Fatalf("loadCurrentSetupCredential: %v", err)
	}
	if got != "current OAuth credential is available" {
		t.Fatalf("credential description = %q", got)
	}
}

func TestSetupRejectsFlags(t *testing.T) {
	var code int
	stderr := captureStderr(t, func() { code = cmdSetup([]string{"--force"}) })
	if code != 2 {
		t.Fatalf("cmdSetup(--force) = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: baseten-switch setup") {
		t.Fatalf("stderr missing usage:\n%s", stderr)
	}
}

func TestParseSemanticVersion(t *testing.T) {
	tests := []struct {
		output string
		want   string
		ok     bool
	}{
		{"baseten 0.3.0", "0.3.0", true},
		{"baseten version v1.24.3", "1.24.3", true},
		{"baseten v0.4.0-rc.1+darwin.arm64", "0.4.0-rc.1", true},
		{"version 0.3", "", false},
		{"version 00.3.0", "", false},
		{"version 0.3.0-01", "", false},
		{"unknown", "", false},
	}
	for _, test := range tests {
		t.Run(test.output, func(t *testing.T) {
			got, err := parseSemanticVersion(test.output)
			if (err == nil) != test.ok {
				t.Fatalf("parseSemanticVersion(%q) error = %v, want ok %v", test.output, err, test.ok)
			}
			if test.ok && got.String() != test.want {
				t.Fatalf("parseSemanticVersion(%q) = %q, want %q", test.output, got, test.want)
			}
		})
	}
}

func TestMinimumBasetenCLIVersionRejectsPrerelease(t *testing.T) {
	minimum, err := parseSemanticVersion(minimumBasetenCLIVersion)
	if err != nil {
		t.Fatal(err)
	}
	prerelease, err := parseSemanticVersion("v0.3.0-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	if !prerelease.less(minimum) {
		t.Fatalf("%s should be older than %s", prerelease, minimum)
	}
}

func assertSetupNextCommands(t *testing.T, output string) {
	t.Helper()
	want := "Next commands:\n" +
		"baseten-switch up --install\n" +
		"baseten-switch claude on\n" +
		"baseten-switch doctor --probe\n"
	if !strings.HasSuffix(output, want) {
		t.Fatalf("setup next commands differ\nwant suffix:\n%s\ngot:\n%s", want, output)
	}
}
