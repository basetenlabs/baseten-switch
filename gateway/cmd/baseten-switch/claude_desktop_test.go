package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func desktopCommand(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	rc := runClaudeDesktop(args, &out, &errOut)
	if rc != 0 {
		t.Logf("desktop command error: %s %s", out.String(), errOut.String())
	}
	return rc, out.String()
}

func TestClaudeDesktopAddUsesLocalContextForPreviewAndSave(t *testing.T) {
	for _, tokens := range []int64{199_999, 200_000, 0} {
		t.Run(fmt.Sprint(tokens), func(t *testing.T) {
			env := newSubagentTestEnv(t, true)
			if err := config.SetupClaudeDesktop(env.cfgPath, "127.0.0.1:45274"); err != nil {
				t.Fatal(err)
			}
			var localReads, discoveryReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/admin/model-catalog" {
					http.NotFound(w, r)
					return
				}
				if r.URL.Query().Get("local_only") != "true" {
					discoveryReads.Add(1)
					http.Error(w, "unexpected account discovery", http.StatusInternalServerError)
					return
				}
				localReads.Add(1)
				fmt.Fprintf(w, `{"state":"ready","context_limits":[{"slug":"example/alpha","context_tokens":%d}]}`, tokens)
			}))
			defer server.Close()
			t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", hostPort(t, server.URL))
			before, _ := os.ReadFile(env.cfgPath)
			for _, dryRun := range []bool{true, false} {
				args := []string{"models", "add", "example/alpha", "--json"}
				if dryRun {
					args = append(args, "--dry-run")
				}
				rc, body := desktopCommand(t, args...)
				if tokens > 0 && tokens < 200_000 {
					if rc != 1 || !strings.Contains(body, claudePickerContextMinimumErrorCode) {
						t.Fatalf("low context accepted: %d %s", rc, body)
					}
				} else if rc != 0 {
					t.Fatalf("add failed: %d %s", rc, body)
				}
			}
			if localReads.Load() != 2 || discoveryReads.Load() != 0 {
				t.Fatalf("local=%d discovery=%d", localReads.Load(), discoveryReads.Load())
			}
			after, _ := os.ReadFile(env.cfgPath)
			if tokens == 199_999 && !bytes.Equal(before, after) {
				t.Fatal("rejected add changed config")
			}
			if tokens != 199_999 && bytes.Equal(before, after) {
				t.Fatal("accepted add did not save")
			}
		})
	}
}

func desktopSuccess(t *testing.T, args ...string) mutationResult {
	t.Helper()
	rc, body := desktopCommand(t, append(args, "--json")...)
	var result mutationResult
	if rc != 0 || json.Unmarshal([]byte(body), &result) != nil || !result.OK {
		t.Fatalf("command failed (%d): %s", rc, body)
	}
	return result
}

func TestClaudeDesktopModelsApplyOrderedChangesAndKeepCodeUntouched(t *testing.T) {
	env := newSubagentTestEnv(t, false)
	settings, _ := os.ReadFile(env.settings)
	setup := desktopSuccess(t, "setup", "--operation-id", "desktop-setup")
	if setup.Client != "claude-desktop" || setup.Operation != "setup_claude_desktop" || setup.Key != "connection" || !setup.Applied {
		t.Fatalf("wrong setup receipt: %#v", setup)
	}
	before, _ := os.ReadFile(env.cfgPath)
	rc, body := desktopCommand(t, "models", "add", "example/alpha", "--dry-run", "--json")
	var preview claudePickerPreview
	if rc != 0 || json.Unmarshal([]byte(body), &preview) != nil || preview.Alias != "claude-baseten-alpha" {
		t.Fatalf("preview failed: %d %s", rc, body)
	}
	after, _ := os.ReadFile(env.cfgPath)
	if !bytes.Equal(before, after) {
		t.Fatal("preview wrote saved config")
	}
	first := desktopSuccess(t, "models", "add", "example/alpha", "--operation-id", "desktop-alpha")
	if first.Operation != "add_claude_desktop_model" || first.RequestedTarget != "example/alpha" || first.IdentityStrength != mutationIdentityExact || !first.Applied {
		t.Fatalf("wrong add receipt: %#v", first)
	}
	desktopSuccess(t, "models", "add", "example/beta")
	desktopSuccess(t, "models", "move", "claude-baseten-beta", "--before", "claude-baseten-alpha")
	f, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	desktop := desktopConfigClient(f)
	if got := desktop.ModelPicker.Models; len(got) != 2 || got[0].Alias != "claude-baseten-beta" || got[1].Alias != "claude-baseten-alpha" {
		t.Fatalf("wrong saved order: %#v", got)
	}
	desktopSuccess(t, "models", "remove", "claude-baseten-beta")
	f, _ = config.Load(env.cfgPath)
	desktop = desktopConfigClient(f)
	if len(desktop.ModelPicker.Models) != 1 || desktop.ModelAliases["claude-baseten-beta"] != "example/beta" {
		t.Fatal("remove must retain historical alias while removing picker membership")
	}
	status, err := readClaudeDesktopStatus(env.cfgPath)
	if err != nil || len(status.ModelPicker.Models) != 1 || status.ModelPicker.Models[0].Alias != "claude-baseten-alpha" || status.ModelPicker.Models[0].Label == "" {
		t.Fatalf("saved model projection missing: %#v, %v", status, err)
	}
	currentSettings, _ := os.ReadFile(env.settings)
	if !bytes.Equal(settings, currentSettings) {
		t.Fatal("Desktop operation changed Claude Code settings")
	}
	if len(f.Clients[0].ModelAliases) != 2 || f.Clients[0].Name != "claude-code" {
		t.Fatal("Desktop operation changed the Code client")
	}
	if err := os.Remove(env.cfgPath); err != nil {
		t.Fatal(err)
	}
	replayed := desktopSuccess(t, "models", "add", "example/alpha", "--operation-id", "desktop-alpha")
	if replayed.RequestFingerprint != first.RequestFingerprint {
		t.Fatal("terminal replay changed request identity")
	}
}

func TestClaudeDesktopAddRejectsKnownSubMinimumContextWithoutWriting(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	desktopSuccess(t, "setup")
	stubPickerCatalogProjection(t, []claudePickerCatalogModel{}, []claudePickerCatalogModel{{
		Slug: "example/small", ContextTokens: 199_999,
	}})
	beforeConfig, err := os.ReadFile(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeSettings, err := os.ReadFile(env.settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, dryRun := range []bool{true, false} {
		args := []string{"models", "add", "example/small", "--json"}
		if dryRun {
			args = append(args, "--dry-run")
		}
		rc, body := desktopCommand(t, args...)
		var result mutationResult
		if rc != 1 || json.Unmarshal([]byte(body), &result) != nil || result.OK ||
			result.Client != "claude-desktop" || result.Operation != "add_claude_desktop_model" ||
			result.Error == nil || result.Error.Code != claudePickerContextMinimumErrorCode ||
			result.Error.Retryable || !strings.Contains(result.Error.Message, "200000") {
			t.Fatalf("dry-run=%t: wrong context failure: %d %s", dryRun, rc, body)
		}
		afterConfig, err := os.ReadFile(env.cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		afterSettings, err := os.ReadFile(env.settings)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(beforeConfig, afterConfig) || !bytes.Equal(beforeSettings, afterSettings) {
			t.Fatalf("dry-run=%t: rejected add changed configuration or Claude Code settings", dryRun)
		}
	}
}

func TestClaudeDesktopAddAllowsMinimumOrUnknownContextWithoutCertification(t *testing.T) {
	for _, test := range []struct {
		name, state string
		models      []claudePickerCatalogModel
	}{
		{name: "known minimum", state: "ready", models: []claudePickerCatalogModel{{Slug: "example/alpha", ContextTokens: 200_000}}},
		{name: "unknown context", state: "ready", models: []claudePickerCatalogModel{{Slug: "example/alpha"}}},
		{name: "unavailable context catalog", state: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newSubagentTestEnv(t, false)
			desktopSuccess(t, "setup")
			stubPickerCatalogResult(t, test.state, test.models, test.models)
			settings, err := os.ReadFile(env.settings)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(env.cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			rc, body := desktopCommand(t, "models", "add", "example/alpha", "--dry-run", "--json")
			var preview claudePickerPreview
			if rc != 0 || json.Unmarshal([]byte(body), &preview) != nil || preview.Slug != "example/alpha" {
				t.Fatalf("allowed preview failed: %d %s", rc, body)
			}
			after, err := os.ReadFile(env.cfgPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("allowed preview changed configuration: %v", err)
			}
			desktopSuccess(t, "models", "add", "example/alpha")
			file, err := config.Load(env.cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			client := desktopConfigClient(file)
			if len(client.ModelPicker.Models) != 1 || client.ModelAliases[client.ModelPicker.Models[0].Alias] != "example/alpha" {
				t.Fatalf("allowed model was not added: %#v", client)
			}
			afterSettings, err := os.ReadFile(env.settings)
			if err != nil || !bytes.Equal(settings, afterSettings) {
				t.Fatalf("Desktop add changed Claude Code settings: %v", err)
			}
		})
	}
}

func TestClaudeDesktopSetupSavedWhileDownAndStatusDoesNotMutate(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	setup := desktopSuccess(t, "setup")
	if setup.Applied || !setup.ReconciliationRequired {
		t.Fatalf("offline setup claimed runtime activation: %#v", setup)
	}
	before, _ := os.ReadFile(env.cfgPath)
	status, err := readClaudeDesktopStatus(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Configured || !status.Enabled || status.Runtime.State != "unavailable" || status.Auth.State != "unknown" || status.RequestEvidence.State != "unavailable" || status.Connection.BaseURL != "http://127.0.0.1:45274" || status.Connection.APIKey != "baseten-switch-local" || len(status.ModelPicker.Models) != 0 {
		t.Fatalf("wrong offline status: %#v", status)
	}
	after, _ := os.ReadFile(env.cfgPath)
	if !bytes.Equal(before, after) {
		t.Fatal("status changed config")
	}
}

func TestClaudeDesktopStatusSeparatesRuntimeAuthAndRequestEvidence(t *testing.T) {
	env := newSubagentTestEnv(t, true)
	if err := config.SetupClaudeDesktop(env.cfgPath, "127.0.0.1:45274"); err != nil {
		t.Fatal(err)
	}
	current := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/admin/requests" {
			fmt.Fprint(w, `{"items":[{"client":"claude-code","completed_at":"2026-01-02T00:00:00Z","requested_model":"other"},{"client":"claude-desktop","completed_at":"2026-01-01T00:00:00Z","requested_model":"claude-baseten-alpha","served_model":"example/alpha","status":200,"termination_reason":"completed"}]}`)
			return
		}
		raw, _, _ := readExactConfig(env.cfgPath)
		hash := exactConfigHash(raw)
		active := hash
		if !current {
			active = "old-config"
		}
		fmt.Fprintf(w, `{"config_path":%q,"active_config_hash":%q,"desired_config_hash":%q,"claude_desktop_model_discovery":{"last_served_at":"2026-01-02T00:00:00.123456789Z","model_count":4},"auth":{"signed_in":false,"health":"signed_out","email":"must-not-appear","token":"must-not-appear"},"clients":[{"name":"claude-desktop","enabled":true,"bind_addr":"127.0.0.1:45274","currently_bound":true}]}`, env.cfgPath, active, hash)
	}))
	defer server.Close()
	t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", hostPort(t, server.URL))
	status, err := readClaudeDesktopStatus(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if status.Runtime.State != "ready" || status.Auth.State != "signed_out" || status.RequestEvidence.State != "observed" || status.RequestEvidence.LastRequest.RequestedModel != "claude-baseten-alpha" {
		t.Fatalf("independent status axes missing: %#v", status)
	}
	if status.ModelListRevision != config.DesktopModelRevision(nil, nil) {
		t.Fatalf("missing desired revision: %q", status.ModelListRevision)
	}
	if status.ModelDiscovery == nil || status.ModelDiscovery.ModelCount != 4 || status.ModelDiscovery.LastServedAt != "2026-01-02T00:00:00.123456789Z" {
		t.Fatalf("model discovery missing: %#v", status.ModelDiscovery)
	}
	body, _ := json.Marshal(status)
	if strings.Contains(string(body), "must-not-appear") {
		t.Fatal("status exposed provider identity or credential")
	}
	current = false
	status, err = readClaudeDesktopStatus(env.cfgPath)
	if err != nil || status.Runtime.State != "pending" || status.Runtime.DesiredMatchesActive {
		t.Fatalf("config gap hidden: %#v, %v", status, err)
	}
	if status.ModelDiscovery != nil {
		t.Fatal("pending configuration retained model discovery")
	}
}

func TestClaudeDesktopStatusPreservesIncompleteRequestEvidence(t *testing.T) {
	for _, test := range []struct {
		name, response, wantState, wantReason string
		statusCode, wantRows                  int
		wantCoverage, wantComplete            bool
	}{
		{name: "complete empty window", response: `{"items":[],"coverage":{"complete":true,"reason":""}}`, wantState: "not_observed", wantCoverage: true, wantComplete: true},
		{name: "analytics unavailable", response: `{"items":[],"coverage":{"complete":false,"reason":"telemetry_unavailable"}}`, wantState: "unavailable", wantReason: "telemetry_unavailable", wantCoverage: true},
		{name: "incomplete unrelated history", response: `{"items":[{"client":"claude-code","requested_model":"other"}],"coverage":{"complete":false,"reason":"segment_unreadable"}}`, wantState: "unavailable", wantReason: "segment_unreadable", wantCoverage: true},
		{name: "legacy unknown coverage", response: `{"items":[]}`, wantState: "unavailable"},
		{name: "observed incomplete history", response: `{"items":[{"client":"claude-desktop","completed_at":"2026-01-01T00:00:00Z","requested_model":"claude-baseten-alpha","served_model":"example/alpha","status":200,"termination_reason":"completed"}],"coverage":{"complete":false,"reason":"segment_unreadable"}}`, wantState: "observed", wantReason: "segment_unreadable", wantCoverage: true, wantRows: 1},
		{name: "request API unavailable", statusCode: http.StatusServiceUnavailable, wantState: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newSubagentTestEnv(t, true)
			if err := config.SetupClaudeDesktop(env.cfgPath, "127.0.0.1:45274"); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/admin/requests" {
					if test.statusCode != 0 {
						w.WriteHeader(test.statusCode)
					}
					fmt.Fprint(w, test.response)
					return
				}
				raw, _, _ := readExactConfig(env.cfgPath)
				hash := exactConfigHash(raw)
				fmt.Fprintf(w, `{"config_path":%q,"active_config_hash":%q,"desired_config_hash":%q,"clients":[{"name":"claude-desktop","bind_addr":"127.0.0.1:45274","currently_bound":true}]}`, env.cfgPath, hash, hash)
			}))
			defer server.Close()
			t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", hostPort(t, server.URL))
			rc, body := desktopCommand(t, "status", "--json")
			var status claudeDesktopStatus
			if rc != 0 || json.Unmarshal([]byte(body), &status) != nil {
				t.Fatalf("status command failed: %d %s", rc, body)
			}
			evidence := status.RequestEvidence
			if evidence.State != test.wantState || (evidence.Coverage != nil) != test.wantCoverage {
				t.Fatalf("request evidence = %#v, want %q with coverage=%t", evidence, test.wantState, test.wantCoverage)
			}
			if test.wantCoverage && (evidence.Coverage.Reason != test.wantReason || evidence.Coverage.Complete != test.wantComplete) {
				t.Fatalf("coverage was not retained: %#v", evidence.Coverage)
			}
			if test.wantRows == 0 && evidence.LastRequest != nil {
				t.Fatalf("unexpected Desktop request: %#v", evidence.LastRequest)
			}
			if test.wantRows == 1 && (evidence.LastRequest == nil || evidence.LastRequest.RequestedModel != "claude-baseten-alpha" || evidence.LastRequest.CompletedAt != "2026-01-01T00:00:00Z") {
				t.Fatalf("observed historical request was dropped: %#v", evidence.LastRequest)
			}
		})
	}
}

func TestClaudeDesktopRejectsStaleMutationWithoutChangingConfig(t *testing.T) {
	env := newSubagentTestEnv(t, false)
	desktopSuccess(t, "setup")
	before, _ := os.ReadFile(env.cfgPath)
	rc, body := desktopCommand(t, "models", "add", "example/alpha", "--json", "--if-config-hash", "sha256:"+strings.Repeat("0", 64))
	if rc != 1 || !strings.Contains(body, "stale_config_hash") {
		t.Fatalf("stale edit accepted: %d %s", rc, body)
	}
	after, _ := os.ReadFile(env.cfgPath)
	if !bytes.Equal(before, after) {
		t.Fatal("stale edit wrote config")
	}
}

func TestClaudeCodeAdapterNeverSelectsDesktopAsLegacyFallback(t *testing.T) {
	file := &config.File{Clients: []config.Client{{Name: "claude-desktop", ProtocolShape: "anthropic", BindAddr: "127.0.0.1:45274"}}}
	if _, err := claudeTargetClientName(file); err == nil {
		t.Fatal("Code adapter targeted Desktop")
	}
	if _, _, _, err := claudeDoorPort(file); err == nil {
		t.Fatal("Code door lookup targeted Desktop")
	}
	file.Clients = append(file.Clients, config.Client{Name: "custom-code", ProtocolShape: "anthropic"})
	if name, err := claudeTargetClientName(file); err != nil || name != "custom-code" {
		t.Fatalf("legacy custom client behavior changed: %q %v", name, err)
	}
}
