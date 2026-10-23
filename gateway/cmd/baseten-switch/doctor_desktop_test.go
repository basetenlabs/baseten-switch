package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func desktopDoctorConfig(address string) *config.File {
	routing, native := false, false
	return &config.File{
		Global: config.Global{RoutingEnabled: &routing},
		Clients: []config.Client{{
			Name: config.ClaudeDesktopClientName, Enabled: true, BindAddr: address,
			ProtocolShape: "anthropic", AllowNative: &native,
			ModelAliases: map[string]string{"claude-baseten-alpha": "example/alpha"},
			ModelPicker:  &config.ModelPicker{Enabled: true, Models: []config.ModelPickerModel{{Alias: "claude-baseten-alpha"}}},
		}},
	}
}

func desktopDoctorChecks() (*doctorReport, addCheck) {
	report := &doctorReport{}
	return report, func(section, name, status, finding, fix string, fixArgv ...string) {
		report.Checks = append(report.Checks, doctorCheck{Section: section, Name: name, Status: status, Finding: finding, Fix: fix})
	}
}

func TestDoctorDesktopMissingAuthFailsWhileTraditionalOverridesOff(t *testing.T) {
	fixture := newDoctorFixture(t, func(c *doctorFixtureCfg) {
		c.signedIn, c.routerSignedOut = false, true
		c.authHealth = "signed_out"
	})
	if err := config.SetupClaudeDesktop(fixture.cfgPath, "127.0.0.1:45274"); err != nil {
		t.Fatal(err)
	}
	if err := config.SetGlobalRoutingEnabled(fixture.cfgPath, false); err != nil {
		t.Fatal(err)
	}
	report := runDoctor(doctorOpts{})
	check := findCheck(t, report, "auth", "signin")
	if check.Status != docFail || !strings.Contains(check.Finding, "claude-desktop") || strings.Contains(check.Finding, "claude-code") {
		t.Fatalf("Desktop auth requirement hidden while Off: %#v", check)
	}
}

func TestDoctorDesktopOnlyConfigDoesNotRequireDoor(t *testing.T) {
	report, add := desktopDoctorChecks()
	dir := t.TempDir()
	doctorConfigChecks(add, filepath.Join(dir, "gateway.yaml"), nil, desktopDoctorConfig("127.0.0.1:45274"), nil, filepath.Join(dir, "env"), nil, 0)
	check := findCheck(t, *report, "config", "door")
	if check.Status != docSkip || !strings.Contains(check.Finding, "no door is required") || check.Fix != "" {
		t.Fatalf("direct-only config was told to add a door: %#v", check)
	}
}

func TestDoctorDesktopProbeUsesSelectedAliasDirectlyWhileOverridesOff(t *testing.T) {
	for _, provider := range []string{"baseten", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			telDir := t.TempDir()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
					t.Errorf("wrong probe endpoint %s %s", r.Method, r.URL.Path)
				}
				var request struct {
					Model     string `json:"model"`
					MaxTokens int    `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Model != "claude-baseten-alpha" || request.MaxTokens != 2 {
					t.Errorf("native/default model or wrong budget used: %#v", request)
				}
				event := doctorTestTelemetryEvent(time.Now(), 1, "claude-desktop", "baseten", provider, request.Model, http.StatusOK, false)
				if err := writeDoctorTelemetryEvents(telDir, event); err != nil {
					t.Error(err)
				}
				fmt.Fprint(w, `{"model":"served-model"}`)
			}))
			defer server.Close()
			address := hostPort(t, server.URL)
			report, add := desktopDoctorChecks()
			doctorE2EChecks(add, doctorOpts{probe: true, yes: true, timeoutSec: 1}, nil, nil, desktopDoctorConfig(address), telDir)
			probe := findCheck(t, *report, "e2e", "probe:"+portOf(address))
			if probe.Status != docOK || !strings.Contains(probe.Finding, "direct listener") || calls.Load() != 1 {
				t.Fatalf("direct endpoint not exercised: %#v, calls=%d", probe, calls.Load())
			}
			route := findCheck(t, *report, "e2e", "route:"+portOf(address))
			want := docOK
			if provider != "baseten" {
				want = docFail
			}
			if route.Status != want || strings.Contains(route.Finding, "routing stamp") {
				t.Fatalf("direct route evidence = %#v, want %s without a door stamp", route, want)
			}
		})
	}
}

func TestDoctorDesktopProbeSkipsEmptySelectionWithoutSendingNativeProbe(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	for _, picker := range []*config.ModelPicker{nil, {Enabled: true}, {Enabled: false, Models: []config.ModelPickerModel{{Alias: "claude-baseten-alpha"}}}} {
		file := desktopDoctorConfig(hostPort(t, server.URL))
		file.Clients[0].DefaultModel = "example/not-selected"
		file.Clients[0].ModelPicker = picker
		report, add := desktopDoctorChecks()
		doctorE2EChecks(add, doctorOpts{probe: true, yes: true, timeoutSec: 1}, nil, nil, file, t.TempDir())
		check := findCheck(t, *report, "e2e", "probe:"+portOf(file.Clients[0].BindAddr))
		if check.Status != docSkip || !strings.Contains(check.Finding, "no selected model") {
			t.Fatalf("empty picker not explained: %#v", check)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("sent %d requests for empty/disabled Desktop selection", calls.Load())
	}
}

func TestStatusDesktopIsIndependentWhileTraditionalSwitchOff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"clients":[{"name":"claude-code","enabled":true,"bind_addr":"127.0.0.1:45272","effective_route":"anthropic","allow_native":true},{"name":"claude-desktop","enabled":true,"bind_addr":"127.0.0.1:45274","effective_route":"baseten","allow_native":false}]}`)
	}))
	defer server.Close()
	rows := routerClientRows(hostPort(t, server.URL))
	if len(rows) != 2 || !strings.Contains(rows[0], "switch OFF") || !strings.Contains(rows[1], "independent gateway") || strings.Contains(rows[1], "switch ON") {
		t.Fatalf("status conflates independent Desktop with override switch: %v", rows)
	}
}
