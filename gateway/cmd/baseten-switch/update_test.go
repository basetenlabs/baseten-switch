package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/updates"
)

func TestUpdateJSONAndUsageContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	c := updates.Checker{Root: t.TempDir(), CurrentVersion: "v0.9.0", APIBase: server.URL}
	var out, errOut bytes.Buffer
	if code := runUpdate([]string{"check", "--json", "--refresh"}, c, &out, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	var result updates.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("JSON contaminated: %s", out.String())
	}
	if result.Error == "" || result.Status != "unknown" || errOut.Len() != 0 {
		t.Fatalf("error contract: %+v stderr %s", result, errOut.String())
	}
	out.Reset()
	if code := runUpdate([]string{"check", "--unknown"}, c, &out, &errOut); code != 2 || out.Len() != 0 {
		t.Fatal("usage contract")
	}
	out.Reset()
	errOut.Reset()
	if code := runUpdate([]string{"automatic", "on", "--json"}, c, &out, &errOut); code != 0 {
		t.Fatal("JSON preference command failed")
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.AutomaticCheck {
		t.Fatalf("JSON preference result: %s", out.String())
	}
	out.Reset()
	if code := runUpdate([]string{"automatic", "off"}, c, &out, &errOut); code != 0 {
		t.Fatal("preference command failed")
	}
	out.Reset()
	if code := runUpdate([]string{"check", "--json"}, c, &out, &errOut); code != 0 {
		t.Fatal("disabled cache read failed")
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.AutomaticCheck || result.Status != "disabled" {
		t.Fatalf("disabled result: %s", out.String())
	}
}

func TestUpdatePassiveOutputBoundaries(t *testing.T) {
	for _, command := range []string{"status", "up", "doctor"} {
		if !updateNoticeEligible([]string{command}, true) {
			t.Errorf("missing %s", command)
		}
	}
	for _, args := range [][]string{{"doctor", "--json"}, {"doctor", "--json=true"}, {"status", "--help"}, {"up", "--uninstall"}, {"gateway", "status"}, {"door"}, {"--version"}, {"help"}, {"update", "check", "--json"}} {
		if updateNoticeEligible(args, true) {
			t.Errorf("eligible machine/side effect path: %v", args)
		}
	}
	if updateNoticeEligible([]string{"status"}, false) {
		t.Fatal("piped invocation eligible")
	}
}

func TestUpdateInstallSourceAndInstructions(t *testing.T) {
	for path, want := range map[string]string{"/opt/homebrew/Cellar/baseten-switch/0.10.0/bin/baseten-switch": "homebrew", "/nix/store/fictional-baseten-switch/bin/baseten-switch": "nix", "/usr/local/bin/baseten-switch": "unknown", "/tmp/Cellar/another/0.10.0/bin/baseten-switch": "unknown"} {
		if got := updateInstallSource(path); got != want {
			t.Errorf("%s: %s", path, got)
		}
	}
	var out bytes.Buffer
	printUpdateResult(&out, updates.Result{CurrentVersion: "v0.9.0", AvailableVersion: "v0.10.0", InstallSource: "unknown", ReleaseURL: "https://example.invalid/release", Status: "available"})
	if strings.Contains(out.String(), "brew") || !strings.Contains(out.String(), "may interrupt") {
		t.Fatal("unknown install guidance assumed Homebrew or omitted adoption boundary")
	}
}
