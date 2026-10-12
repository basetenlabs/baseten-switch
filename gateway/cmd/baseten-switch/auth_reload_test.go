package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
)

func authReloadFixture(t *testing.T, fault string) (string, *string, *int) {
	t.Helper()
	restore := auth.UseFileSavedAPIKeyStoreForTesting()
	t.Cleanup(restore)
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	pf := filepath.Join(t.TempDir(), "gateway.pid")
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", pf)
	if err := pidfile.WriteAt(pf, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var active string
	posts, snapshots := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/admin/status":
			snapshots++
			cfg, pid, boot := path, os.Getpid(), "synthetic-boot"
			if fault == "foreign config" {
				cfg += ".other"
			}
			if fault == "foreign pid" {
				pid++
			}
			if fault == "restarted" && snapshots > 1 {
				boot = "replacement-boot"
			}
			json.NewEncoder(w).Encode(map[string]any{"config_path": cfg, "router_pid": pid, "router_boot_id": boot})
		case "/v1/admin/auth/reload":
			posts++
			raw, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPost || r.Header.Get("X-Baseten-Switch-Admin") != "1" || len(raw) != 0 {
				t.Errorf("unexpected reload request: method %s, body bytes %d", r.Method, len(raw))
			}
			if fault == "timeout" {
				time.Sleep(70 * time.Millisecond)
			}
			if fault == "unsupported" {
				w.WriteHeader(404)
				return
			}
			if fault == "rejected" {
				w.WriteHeader(500)
				w.Write([]byte("synthetic-upstream-secret"))
				return
			}
			if fault == "malformed" {
				w.Write([]byte("not JSON"))
				return
			}
			if fault == "incomplete" {
				w.Write([]byte(`{"source":"none"}`))
				return
			}
			active, _ = auth.LoadSavedAPIKey(path)
			source, kind := "none", "none"
			if active != "" {
				source, kind = "saved_api_key", "api_key"
			}
			if fault == "preserved CLI" {
				source, kind = "profile_oauth", "oauth"
			}
			if fault == "stale selection" {
				source, kind = "none", "none"
			}
			json.NewEncoder(w).Encode(map[string]any{"source": source, "auth_type": kind, "health": "ok", "signed_in": active != "" || fault == "preserved CLI", "saved_api_key": active != "" && fault != "stale selection", "revision": 0})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	return path, &active, &posts
}

func TestAuthCommandsConfirmStoredKeyBeforeReturning(t *testing.T) {
	path, active, posts := authReloadFixture(t, "")
	writeAuthJSON(t, `{"version":1,"profiles":{}}`)
	for _, key := range []string{"synthetic-key-a", "synthetic-key-b", "synthetic-key-b"} {
		var out, errOut bytes.Buffer
		if rc := cmdAuthAPIKey([]string{"set"}, strings.NewReader(key+"\n"), &out, &errOut); rc != 0 {
			t.Fatalf("set = %d: %s", rc, errOut.String())
		}
		if *active != key {
			t.Fatal("command returned before router loaded the selected key")
		}
		if !strings.Contains(out.String(), "reloaded and confirmed") || strings.Contains(out.String()+errOut.String(), key) {
			t.Fatal("missing confirmation or key leaked")
		}
	}
	var out, errOut bytes.Buffer
	if rc := cmdAuthAPIKey([]string{"remove"}, strings.NewReader(""), &out, &errOut); rc != 0 {
		t.Fatalf("remove = %d: %s", rc, errOut.String())
	}
	if *active != "" || *posts != 4 {
		t.Fatalf("removal not confirmed: posts %d", *posts)
	}
	if key, _ := auth.LoadSavedAPIKey(path); key != "" {
		t.Fatal("saved key remained")
	}
}

func TestAuthCommandUnconfirmedRuntimePreservesSavedCredential(t *testing.T) {
	for _, fault := range []string{"foreign config", "foreign pid", "restarted", "unsupported", "rejected", "malformed", "incomplete", "stale selection", "timeout"} {
		t.Run(fault, func(t *testing.T) {
			path, _, posts := authReloadFixture(t, fault)
			if fault == "timeout" {
				old := authReloadHTTPTimeout
				authReloadHTTPTimeout = 20 * time.Millisecond
				t.Cleanup(func() { authReloadHTTPTimeout = old })
			}
			const key = "synthetic-persisted-key"
			var out, errOut bytes.Buffer
			if rc := cmdAuthAPIKey([]string{"set"}, strings.NewReader(key), &out, &errOut); rc != 1 {
				t.Fatalf("unconfirmed set = %d", rc)
			}
			if got, _ := auth.LoadSavedAPIKey(path); got != key {
				t.Fatal("saved credential was rolled back")
			}
			if strings.Contains(out.String(), "reloaded and confirmed") || !strings.Contains(errOut.String(), "unconfirmed") {
				t.Fatal("unconfirmed application was reported as complete")
			}
			if strings.Contains(out.String()+errOut.String(), key) || strings.Contains(errOut.String(), "synthetic-upstream-secret") {
				t.Fatal("credential or remote body leaked")
			}
			if (fault == "foreign config" || fault == "foreign pid") && *posts != 0 {
				t.Fatal("foreign router was mutated")
			}
		})
	}
}

func TestAuthReloadSubcommandConfirmsWithoutWritingStore(t *testing.T) {
	path, active, posts := authReloadFixture(t, "")
	const key = "synthetic-existing-key"
	if err := auth.SaveSavedAPIKey(path, key); err != nil {
		t.Fatal(err)
	}
	output := captureStderr(t, func() {
		if rc := cmdAuth([]string{"reload"}); rc != 0 {
			t.Fatalf("reload=%d", rc)
		}
	})
	if *active != key || *posts != 1 || !strings.Contains(output, "confirmed") || strings.Contains(output, key) {
		t.Fatal("reload did not confirm existing credential")
	}
	if got, _ := auth.LoadSavedAPIKey(path); got != key {
		t.Fatal("reload changed credential store")
	}
}

func TestAuthReloadWithoutRouter(t *testing.T) {
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "missing.pid"))
	captureStderr(t, func() {
		if rc := cmdAuth([]string{"reload"}); rc != 1 {
			t.Fatalf("offline reload=%d", rc)
		}
	})
	captureStderr(t, func() {
		if rc := cmdAuth([]string{"reload", "extra"}); rc != 2 {
			t.Fatalf("invalid reload=%d", rc)
		}
	})
}

func TestAuthReloadCannotConfirmPreservedCredentialAfterStoreFailure(t *testing.T) {
	authReloadFixture(t, "preserved CLI")
	writeAuthJSON(t, "{synthetic-malformed-store")
	var rc int
	text := captureStderr(t, func() { rc = cmdAuth([]string{"reload"}) })
	if rc != 1 || !strings.Contains(text, "store could not be verified") || strings.Contains(text, "reloaded and confirmed") || strings.Contains(text, "synthetic-malformed-store") {
		t.Fatalf("reload=%d output=%s", rc, text)
	}
}
