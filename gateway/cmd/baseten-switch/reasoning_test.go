package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

type failingReasoningPreflight struct {
	called bool
}

func (f *failingReasoningPreflight) Check(
	string,
	string,
	string,
	string,
	config.ReasoningPolicy,
) (reasoningPreflightResult, error) {
	f.called = true
	return reasoningPreflightResult{}, fmt.Errorf("preflight must not run")
}

func installReasoningPreflight(t *testing.T, client reasoningPreflightClient) {
	t.Helper()
	old := activeReasoningPreflightClient
	activeReasoningPreflightClient = client
	t.Cleanup(func() { activeReasoningPreflightClient = old })
}

func TestParseReasoningPolicy(t *testing.T) {
	tests := []struct {
		args        []string
		want        config.ReasoningPolicy
		wantDefault bool
		wantErr     bool
	}{
		{args: []string{"baseten", "zai-org/GLM-5.2-Fast", "on"}, want: config.ReasoningPolicy{Mode: config.ReasoningOn}},
		{args: []string{"baseten", "zai-org/GLM-5.2", "off"}, want: config.ReasoningPolicy{Mode: config.ReasoningOff}},
		{args: []string{"baseten", "zai-org/GLM-5.2", "follow-harness"}, want: config.ReasoningPolicy{Mode: config.ReasoningFollowHarness}},
		{args: []string{"baseten", "deepseek-ai/DeepSeek-V4-Pro", "effort", "high"}, want: config.ReasoningPolicy{Mode: config.ReasoningFixed, Effort: "high"}},
		{args: []string{"baseten", "zai-org/GLM-5.2", "default"}, wantDefault: true},
		{args: []string{"openai", "gpt-5", "off"}, wantErr: true},
		{args: []string{"baseten", "model", "effort"}, wantErr: true},
		{args: []string{"baseten", "model", "off", "high"}, wantErr: true},
		{args: []string{"baseten", "model", "on", "medium"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			_, _, got, gotDefault, err := parseReasoningPolicy(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if err == nil && (got != tc.want || gotDefault != tc.wantDefault) {
				t.Fatalf("policy/default = %#v/%t, want %#v/%t", got, gotDefault, tc.want, tc.wantDefault)
			}
		})
	}
}

func TestReasoningDefaultResetsFastOnOverrideAndSkipsPreflight(t *testing.T) {
	path := writeSwitchFixture(t, `global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2-Fast
    model_options:
      baseten:
        "zai-org/GLM-5.2-Fast":
          reasoning:
            mode: on
`)
	preflight := &failingReasoningPreflight{}
	installReasoningPreflight(t, preflight)
	var out strings.Builder
	rc := runClientReasoning(mutationSurfaceClaude, "claude-code", []string{
		"baseten", "zai-org/GLM-5.2-Fast", "default",
		"--operation-id", "reasoning-default-test",
	}, &out)
	if rc != 0 {
		t.Fatalf("rc = %d, output = %s", rc, out.String())
	}
	if preflight.called {
		t.Fatal("default called semantic preflight")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "model_options:") {
		t.Fatalf("default did not remove the client override:\n%s", body)
	}
	if !strings.Contains(out.String(), "claude-code reasoning: baseten/zai-org/GLM-5.2-Fast -> default") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestReasoningNonDefaultRequiresRunningRouterBeforePreflight(t *testing.T) {
	path := writeSwitchFixture(t, `global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	preflight := &failingReasoningPreflight{}
	installReasoningPreflight(t, preflight)
	var out strings.Builder
	rc := runClientReasoning(
		mutationSurfaceClaude,
		"claude-code",
		[]string{"baseten", "zai-org/GLM-5.2", "off"},
		&out,
	)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	if preflight.called {
		t.Fatal("preflight called without a running router")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("config changed after refused mutation")
	}
}

func TestHTTPReasoningPreflightUsesGatewaySnapshotAndClientProjection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/admin/reasoning/preflight":
			if r.Method != http.MethodPost {
				t.Errorf("preflight method = %s, want POST", r.Method)
			}
			var request struct {
				Client string                 `json:"client"`
				Policy config.ReasoningPolicy `json:"policy"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Client != "claude-code" {
				t.Errorf("preflight client = %q", request.Client)
			}
			if request.Policy.Effort == "xhigh" {
				fmt.Fprint(w, `{"available":false,"error":"model does not advertise reasoning effort \"xhigh\"","clients":[]}`)
				return
			}
			fmt.Fprint(w, `{"available":true,"warning":"reasoning metadata is stale but remains validated (captured 2026-07-25T00:00:00Z)","clients":[{"name":"claude-code","reachable":true,"supported":true,"failure_behaviors":[]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	got, err := (httpReasoningPreflightClient{}).Check(addr, "claude-code", "baseten", "deepseek-ai/DeepSeek-V4-Pro", config.ReasoningPolicy{
		Mode: config.ReasoningFixed, Effort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Warning, "stale") || !strings.Contains(got.Warning, "2026-07-25") {
		t.Fatalf("warning = %q", got.Warning)
	}

	_, err = (httpReasoningPreflightClient{}).Check(addr, "claude-code", "baseten", "deepseek-ai/DeepSeek-V4-Pro", config.ReasoningPolicy{
		Mode: config.ReasoningFixed, Effort: "xhigh",
	})
	if err == nil || !strings.Contains(err.Error(), "does not advertise") {
		t.Fatalf("invalid effort error = %v", err)
	}
}

func TestHTTPReasoningPreflightRequiresGatewayContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	_, err := (httpReasoningPreflightClient{}).Check(
		strings.TrimPrefix(server.URL, "http://"),
		"claude-code",
		"baseten",
		"zai-org/GLM-5.2",
		config.ReasoningPolicy{Mode: config.ReasoningOff},
	)
	if err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("error = %v", err)
	}
}
