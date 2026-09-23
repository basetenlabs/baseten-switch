package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
)

func TestSavedAPIKeyCommandsPreserveCLIStoreAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	const cliStore = `{"version":1,"profiles":{}}`
	writeAuthJSON(t, cliStore)
	pf := filepath.Join(t.TempDir(), "gateway.pid")
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", pf)
	if err := pidfile.WriteAt(pf, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	pids, _ := setAuthLoginSeams(t)
	var out, errOut bytes.Buffer
	const key = "synthetic-saved-key"
	if code := cmdAuthAPIKey([]string{"set"}, strings.NewReader(key+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("set = %d: %s", code, errOut.String())
	}
	if got, err := auth.LoadSavedAPIKey(path); err != nil || got != key {
		t.Fatal("saved key did not round-trip")
	}
	if code := cmdAuthAPIKey([]string{"remove"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("remove = %d: %s", code, errOut.String())
	}
	if got, err := auth.LoadSavedAPIKey(path); err != nil || got != "" {
		t.Fatal("saved key was not removed")
	}
	if len(*pids) != 2 || (*pids)[0] != os.Getpid() || (*pids)[1] != os.Getpid() {
		t.Fatalf("reload signals = %v", *pids)
	}
	if strings.Contains(out.String()+errOut.String(), key) {
		t.Fatal("command output exposed key")
	}
	store, err := os.ReadFile(os.Getenv("BASETEN_SWITCH_AUTH_FILE"))
	if err != nil || string(store) != cliStore {
		t.Fatal("CLI credential store changed")
	}
}

func TestSavedAPIKeyCommandRejectsArgumentsAndInvalidInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "gateway.pid"))
	for _, tc := range []struct {
		name  string
		args  []string
		input string
		code  int
	}{
		{"argument", []string{"set", "synthetic-argument-key"}, "", 2},
		{"empty", []string{"set"}, "\n", 1},
		{"oversized", []string{"set"}, strings.Repeat("x", 4097), 1},
		{"multiple lines", []string{"set"}, "synthetic-one\nsynthetic-two", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := cmdAuthAPIKey(tc.args, strings.NewReader(tc.input), &out, &errOut); got != tc.code {
				t.Fatalf("code = %d, want %d", got, tc.code)
			}
			if strings.Contains(out.String()+errOut.String(), "synthetic-") {
				t.Fatal("invalid key appeared in output")
			}
			if got, err := auth.LoadSavedAPIKey(path); err != nil || got != "" {
				t.Fatal("invalid input saved a credential")
			}
		})
	}
}

func TestSavedAPIKeyCommandReloadsAfterStorageFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	pf := filepath.Join(t.TempDir(), "gateway.pid")
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", pf)
	if err := pidfile.WriteAt(pf, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".api-key", 0700); err != nil {
		t.Fatal(err)
	}
	pids, _ := setAuthLoginSeams(t)
	var out, errOut bytes.Buffer
	if code := cmdAuthAPIKey([]string{"set"}, strings.NewReader("synthetic-new-key"), &out, &errOut); code != 1 {
		t.Fatalf("set = %d, want storage failure", code)
	}
	if len(*pids) != 1 || (*pids)[0] != os.Getpid() {
		t.Fatal("failed mutation did not reconcile router auth")
	}
	if strings.Contains(out.String(), "Saved API key") || strings.Contains(out.String()+errOut.String(), "synthetic-new-key") {
		t.Fatal("failed mutation reported success or exposed key")
	}
}

func TestWhoamiSavedAPIKeyTakesPriorityWithoutIdentityLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	writeAuthJSON(t, "malformed CLI store")
	if err := auth.SaveSavedAPIKey(path, "synthetic-key"); err != nil {
		t.Fatal(err)
	}
	out, code := captureStdout(t, func() int { return cmdWhoami([]string{"--refresh", "--profile", "other"}) })
	if code != 0 || !strings.Contains(out, "saved Switch API key") || strings.Contains(out, "synthetic-key") {
		t.Fatalf("whoami = %d: %s", code, out)
	}
}

func TestSavedAPIKeyUnreadableFailsWhoamiAndSetup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	if err := os.Mkdir(path+".api-key", 0o700); err != nil {
		t.Fatal(err)
	}
	var code int
	captureStderr(t, func() { code = cmdWhoami(nil) })
	if code != 1 {
		t.Fatalf("whoami = %d, want 1", code)
	}
	deps := setupTestDependencies(path)
	deps.findBaseten = func() (string, error) { t.Fatal("must not fall back to CLI"); return "", nil }
	var out, errOut bytes.Buffer
	if code := runSetup(deps, &out, &errOut); code != 1 {
		t.Fatalf("setup = %d, want 1", code)
	}
}

func TestSetupSavedAPIKeyDoesNotRequireCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := auth.SaveSavedAPIKey(path, "synthetic-key"); err != nil {
		t.Fatal(err)
	}
	deps := setupTestDependencies(path)
	deps.findBaseten = func() (string, error) { t.Fatal("saved key must skip CLI prerequisite"); return "", nil }
	deps.loadCredential = func() (string, error) { t.Fatal("saved key must skip CLI store"); return "", nil }
	var out, errOut bytes.Buffer
	if code := runSetup(deps, &out, &errOut); code != 0 {
		t.Fatalf("setup = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "saved Switch API key") {
		t.Fatalf("missing saved key source: %s", out.String())
	}
}

func TestDoctorSavedKeyPrecedesMalformedCLIStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	writeAuthJSON(t, "malformed CLI store")
	if err := auth.SaveSavedAPIKey(path, "synthetic-key"); err != nil {
		t.Fatal(err)
	}
	var status, finding string
	add := func(_, _, gotStatus, gotFinding, _ string, _ ...string) { status, finding = gotStatus, gotFinding }
	store := doctorAuthCheck(add, nil, nil, nil, "", doctorRouterAuth{}, false)
	if status != docOK || !store.ready || !strings.Contains(finding, "saved Switch API key") {
		t.Fatalf("saved auth check = %s %s, store %+v", status, finding, store)
	}
	doctorAuthHealthCheck(add, true, doctorRouterAuth{SignedIn: true, AuthType: "oauth", Health: "ok"}, true, store)
	if status != docFail || !strings.Contains(finding, "another credential source") {
		t.Fatalf("stale auth health = %s %s", status, finding)
	}
	if err := os.WriteFile(path+".api-key", nil, 0600); err != nil {
		t.Fatal(err)
	}
	store = doctorAuthCheck(add, nil, nil, nil, "", doctorRouterAuth{}, false)
	if status != docFail || store.ready || !strings.Contains(finding, "saved API key") {
		t.Fatalf("malformed saved auth check = %s %s, store %+v", status, finding, store)
	}
}

func TestRemovingSavedAPIKeyRestoresCLIProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "gateway.pid"))
	writeAuthJSON(t, `{"version":1,"current":"example-profile","profiles":{"example-profile":{"remote_url":"https://api.baseten.co","auth_type":"api_key","api_key":"synthetic-cli-key"}}}`)
	if err := auth.SaveSavedAPIKey(path, "synthetic-saved-key"); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := cmdAuthAPIKey([]string{"remove"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("remove = %d: %s", code, errOut.String())
	}
	identity, code := captureStdout(t, func() int { return cmdWhoami(nil) })
	if code != 0 || !strings.Contains(identity, "example-profile") || strings.Contains(identity, "synthetic-") {
		t.Fatalf("CLI identity = %d: %s", code, identity)
	}
}
