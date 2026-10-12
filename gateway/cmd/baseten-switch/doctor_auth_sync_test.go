package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestDoctorStoredAndActiveAuthSelection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		storedAPIKey bool
		activeType   string
		activeSource string
		wantStatus   string
		storedLabel  string
		activeLabel  string
	}{
		{"API key pending over OAuth", true, "oauth", "profile_oauth", docFail, "CLI API key", "CLI OAuth"},
		{"OAuth pending over API key", false, "api_key", "profile_api_key", docFail, "CLI OAuth", "CLI API key"},
		{"matching API key", true, "api_key", "profile_api_key", docOK, "", ""},
		{"matching OAuth", false, "oauth", "profile_oauth", docOK, "", ""},
		{"profile key pending over environment fallback", true, "api_key", "environment_fallback", docFail, "CLI API key", "environment API-key fallback"},
		{"OAuth type mismatch without source", false, "api_key", "", docFail, "CLI OAuth", "API key"},
		{"profile source mismatch without type", false, "", "profile_api_key", docFail, "CLI OAuth", "CLI API key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDoctorFixture(t, func(c *doctorFixtureCfg) { c.apiKeyProfile = tc.storedAPIKey })
			if !tc.storedAPIKey {
				writeAuthJSON(t, `{
  "version": 1,
  "current": "doc@example.com",
  "profiles": {
    "doc@example.com": {
      "remote_url": "https://app.example.com",
      "auth_type": "oauth",
      "oauth_credential": {
        "access_token": "synthetic-doctor-access-token",
        "refresh_token": "synthetic-doctor-refresh-token",
        "expiry": "2100-01-01T00:00:00Z"
      }
    }
  }
}`)
			}
			// Replace only the fixture's auth projection with a stale or matching
			// selection. All other checks retain the healthy fixture contract.
			adminURL := "http://" + os.Getenv("BASETEN_SWITCH_ADMIN_ADDR")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || (r.URL.Path != "/healthz" && r.URL.Path != "/v1/admin/status") {
					t.Errorf("doctor made unexpected request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				resp, err := http.Get(adminURL + r.URL.Path)
				if err != nil {
					t.Errorf("read fixture status: %v", err)
					http.Error(w, "fixture unavailable", http.StatusBadGateway)
					return
				}
				defer resp.Body.Close()
				var payload map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
					t.Errorf("decode fixture status: %v", err)
					http.Error(w, "invalid fixture", http.StatusBadGateway)
					return
				}
				if r.URL.Path == "/v1/admin/status" {
					auth := payload["auth"].(map[string]any)
					auth["auth_type"], auth["source"] = tc.activeType, tc.activeSource
				}
				_ = json.NewEncoder(w).Encode(payload)
			}))
			t.Cleanup(srv.Close)
			t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", hostPort(t, srv.URL))

			rep := runDoctor(doctorOpts{})
			check := findCheck(t, rep, "auth", "health")
			if check.Status != tc.wantStatus {
				t.Fatalf("auth/health = %+v, want %s", check, tc.wantStatus)
			}
			if tc.wantStatus == docFail {
				for _, want := range []string{"credential change pending", "stored: " + tc.storedLabel, "active router: " + tc.activeLabel} {
					if !strings.Contains(check.Finding, want) {
						t.Errorf("finding %q missing %q", check.Finding, want)
					}
				}
				if check.Fix != "baseten-switch auth reload" {
					t.Errorf("fix = %q, want auth reload", check.Fix)
				}
				if rep.ExitCode != 1 || rep.FirstFailure != "auth/health" {
					t.Errorf("report exit = %d, first failure = %q, want auth/health failure", rep.ExitCode, rep.FirstFailure)
				}
			} else if rep.ExitCode != 0 {
				t.Errorf("matching selection failed doctor: %+v", rep)
			}
			if fx.authStatusHits.Load() != 0 {
				t.Error("doctor performed an upstream-capable auth status check")
			}
			for _, secret := range []string{"synthetic-doctor-key", "synthetic-doctor-access-token", "synthetic-doctor-refresh-token"} {
				if strings.Contains(fmt.Sprintf("%+v", rep), secret) {
					t.Errorf("doctor report contains credential material %q", secret)
				}
			}
		})
	}
}

func TestDoctorAuthSelectionUnavailableOrUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name        string
		routerUp    bool
		routerKnown bool
		store       doctorStoreAuth
		wantStatus  string
	}{
		{"router stopped", false, false, doctorStoreAuth{authType: "OAuth", ready: true}, docSkip},
		{"router projection unavailable", true, false, doctorStoreAuth{authType: "OAuth", ready: true}, docSkip},
		{"unreadable store does not invent mismatch", true, true, doctorStoreAuth{authType: "OAuth"}, docOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var check doctorCheck
			doctorAuthHealthCheck(func(section, name, status, finding, fix string, _ ...string) {
				check = doctorCheck{Section: section, Name: name, Status: status, Finding: finding, Fix: fix}
			}, tc.routerUp, doctorRouterAuth{SignedIn: true, AuthType: "api_key", Source: "profile_api_key", Health: "ok"}, tc.routerKnown, tc.store)
			if check.Status != tc.wantStatus {
				t.Fatalf("check = %+v, want %s", check, tc.wantStatus)
			}
		})
	}
}
