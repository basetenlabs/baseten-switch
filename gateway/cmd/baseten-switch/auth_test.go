package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeAuthJSON(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_AUTH_FILE", path)
	t.Setenv("BASETEN_SWITCH_AUTH_NO_KEYRING", "1")
}

func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := fn()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b), code
}

// TestWhoamiDefaultsToCurrentProfile verifies whoami with no --profile flag
// resolves via auth.json's current-profile pointer (profile default is "",
// not "default"), matching the gateway and the baseten CLI's email-derived
// profile names.
func TestWhoamiDefaultsToCurrentProfile(t *testing.T) {
	sawAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/me" {
			http.NotFound(w, r)
			return
		}
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"email":          "user@example.com",
			"workspace_name": "baseten",
		})
	}))
	defer srv.Close()

	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	writeAuthJSON(t, fmt.Sprintf(`{
  "version": 1,
  "current": "user@example.com",
  "profiles": {
    "user@example.com": {
      "remote_url": %q,
      "auth_type": "oauth",
      "oauth_credential": {"access_token": "at-live", "refresh_token": "rt", "expiry": %q}
    }
  }
}`, srv.URL, expiry))

	out, code := captureStdout(t, func() int {
		return cmdWhoami([]string{"--host=" + srv.URL})
	})
	if code != 0 {
		t.Fatalf("cmdWhoami = %d, want 0; output:\n%s", code, out)
	}
	if sawAuth != "Bearer at-live" {
		t.Fatalf("server saw Authorization %q; current-profile token not used", sawAuth)
	}
	if !strings.Contains(out, "user@example.com") {
		t.Fatalf("output missing email:\n%s", out)
	}
	if !strings.Contains(out, "(current)") {
		t.Fatalf("output should label the profile as (current):\n%s", out)
	}
}

// TestWhoamiExplicitProfileFlagStillWorks confirms --profile continues to
// select a named profile.
func TestWhoamiExplicitProfileFlagStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"email": "other@example.com", "workspace_name": "w"})
	}))
	defer srv.Close()
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	writeAuthJSON(t, fmt.Sprintf(`{
  "version": 1,
  "current": "user@example.com",
  "profiles": {
    "other@example.com": {
      "remote_url": %q,
      "auth_type": "oauth",
      "oauth_credential": {"access_token": "at-other", "refresh_token": "rt", "expiry": %q}
    }
  }
}`, srv.URL, expiry))
	out, code := captureStdout(t, func() int {
		return cmdWhoami([]string{"--profile", "other@example.com", "--host=" + srv.URL})
	})
	if code != 0 {
		t.Fatalf("cmdWhoami = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "other@example.com") {
		t.Fatalf("output missing profile email:\n%s", out)
	}
}

// TestWhoamiAPIKeyProfile verifies an api_key-type profile reports "signed
// in with API key" (exit 0) instead of "not signed in" (exit 3).
func TestWhoamiAPIKeyProfile(t *testing.T) {
	writeAuthJSON(t, `{
  "version": 1,
  "current": "svc",
  "profiles": {
    "svc": {"remote_url": "https://api.baseten.co", "auth_type": "api_key", "api_key": "sk-live-1"}
  }
}`)
	out, code := captureStdout(t, func() int {
		return cmdWhoami(nil)
	})
	if code != 0 {
		t.Fatalf("cmdWhoami = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "Signed in with API key") {
		t.Fatalf("output should report API key sign-in:\n%s", out)
	}
	if !strings.Contains(out, `"svc"`) {
		t.Fatalf("output should name the profile:\n%s", out)
	}
	if !strings.Contains(out, "baseten-switch status") || !strings.Contains(out, "baseten-switch doctor --probe") {
		t.Fatalf("output should point API-key users at current routing diagnostics:\n%s", out)
	}
	if strings.Contains(out, "session-check") {
		t.Fatalf("output should not reference the removed session-check command:\n%s", out)
	}
}

// TestWhoamiAPIKeyProfileUnreadableKey verifies an api_key profile whose key
// cannot be read reports an actionable error (exit 1), not "not signed in".
func TestWhoamiAPIKeyProfileUnreadableKey(t *testing.T) {
	writeAuthJSON(t, `{
  "version": 1,
  "current": "svc",
  "profiles": {
    "svc": {"remote_url": "https://api.baseten.co", "auth_type": "api_key"}
  }
}`)
	out, code := captureStdout(t, func() int {
		return cmdWhoami(nil)
	})
	if code != 1 {
		t.Fatalf("cmdWhoami = %d, want 1; output:\n%s", code, out)
	}
}
