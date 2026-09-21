package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeReasoningEditConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func readReasoningEditConfig(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestSetClientModelReasoningPolicyCreatesNestedMappingsAndPreservesBytes(t *testing.T) {
	path := writeReasoningEditConfig(t, "global:\n  # gate stays here\n  routing_enabled: true # untouched\nclients:\n  - name: claude-code\n    enabled: true\n    protocol_shape: anthropic\n    default_model: zai-org/GLM-5.2\n")

	err := SetClientModelReasoningPolicy(
		path,
		"claude-code",
		"baseten",
		"zai-org/GLM-5.2",
		ReasoningPolicy{Mode: ReasoningOff},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := readReasoningEditConfig(t, path)
	want := "global:\n  # gate stays here\n  routing_enabled: true # untouched\nclients:\n  - name: claude-code\n    enabled: true\n    protocol_shape: anthropic\n    default_model: zai-org/GLM-5.2\n    model_options:\n      baseten:\n        \"zai-org/GLM-5.2\":\n          reasoning:\n            mode: off\n"
	if got != want {
		t.Fatalf("edited config:\n%s\nwant:\n%s", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
}

func TestSetClientModelReasoningPolicyRoundTripsOn(t *testing.T) {
	path := writeReasoningEditConfig(t, "global:\n  routing_enabled: true\nclients:\n  - name: claude-code\n    enabled: true\n    protocol_shape: anthropic\n    default_model: zai-org/GLM-5.2-Fast\n")
	if err := SetClientModelReasoningPolicy(
		path,
		"claude-code",
		"baseten",
		"zai-org/GLM-5.2-Fast",
		ReasoningPolicy{Mode: ReasoningOn},
	); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := file.Clients[0].ModelOptions["baseten"]["zai-org/GLM-5.2-Fast"].Reasoning
	if got == nil || got.Mode != ReasoningOn || got.Effort != "" {
		t.Fatalf("reasoning policy = %#v", got)
	}
}

func TestSetClientModelReasoningPolicyExpandsEmptyMappings(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "empty model options",
			body: "global:\n  routing_enabled: true\nclients:\n  - name: claude-code\n    enabled: true\n    protocol_shape: anthropic\n    default_model: zai-org/GLM-5.2\n    model_options: {}\n",
			want: "    model_options:\n      baseten:\n        \"zai-org/GLM-5.2\":\n          reasoning:\n            mode: off\n",
		},
		{
			name: "null model options",
			body: "global:\n  routing_enabled: true\nclients:\n  - name: claude-code\n    enabled: true\n    protocol_shape: anthropic\n    default_model: zai-org/GLM-5.2\n    model_options: null # preserve this comment\n",
			want: "    model_options: # preserve this comment\n      baseten:\n        \"zai-org/GLM-5.2\":\n          reasoning:\n            mode: off\n",
		},
		{
			name: "empty provider",
			body: "global:\n  routing_enabled: true\nclients:\n  - name: claude-code\n    enabled: true\n    protocol_shape: anthropic\n    default_model: zai-org/GLM-5.2\n    model_options:\n      baseten: {}\n",
			want: "      baseten:\n        \"zai-org/GLM-5.2\":\n          reasoning:\n            mode: off\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeReasoningEditConfig(t, tc.body)
			err := SetClientModelReasoningPolicy(
				path,
				"claude-code",
				"baseten",
				"zai-org/GLM-5.2",
				ReasoningPolicy{Mode: ReasoningOff},
			)
			if err != nil {
				t.Fatal(err)
			}
			got := readReasoningEditConfig(t, path)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("edited config:\n%s\nwant exact block:\n%s", got, tc.want)
			}
		})
	}
}

func TestClientReasoningEditTouchesOnlySelectedClient(t *testing.T) {
	path := writeReasoningEditConfig(t, "global:\n  routing_enabled: true\nclients:\n  - name: claude-code\n    enabled: true\n    protocol_shape: anthropic\n    default_model: zai-org/GLM-5.2\n    model_options:\n      baseten:\n        \"zai-org/GLM-5.2\": # model comment\n          reasoning:\n            mode: fixed # mode comment\n            effort: low # effort comment\n  - name: codex\n    enabled: true\n    protocol_shape: openai\n    default_model: zai-org/GLM-5.2\n    model_options:\n      baseten:\n        \"zai-org/GLM-5.2\":\n          reasoning:\n            mode: off\n")

	err := SetClientModelReasoningPolicy(
		path,
		"claude-code",
		"baseten",
		"zai-org/GLM-5.2",
		ReasoningPolicy{Mode: ReasoningFollowHarness},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := readReasoningEditConfig(t, path)
	if !strings.Contains(got, "mode: follow_harness # mode comment") {
		t.Fatalf("selected client was not updated:\n%s", got)
	}
	if strings.Count(got, "mode: off") != 1 {
		t.Fatalf("other client changed:\n%s", got)
	}
	if strings.Contains(got, "effort:") {
		t.Fatalf("obsolete effort was not removed:\n%s", got)
	}
}

func TestRemoveClientModelReasoningPolicyPrunesOnlySelectedClient(t *testing.T) {
	path := writeReasoningEditConfig(t, "global:\n  routing_enabled: true\nclients:\n  - name: claude-code\n    enabled: true\n    protocol_shape: anthropic\n    default_model: zai-org/GLM-5.2\n    model_options:\n      baseten:\n        \"zai-org/GLM-5.2\":\n          reasoning:\n            mode: off\n  - name: codex\n    enabled: true\n    protocol_shape: openai\n    default_model: zai-org/GLM-5.2\n    model_options:\n      baseten:\n        \"zai-org/GLM-5.2\":\n          reasoning:\n            mode: follow_harness\n")

	if err := RemoveClientModelReasoningPolicy(
		path,
		"claude-code",
		"baseten",
		"zai-org/GLM-5.2",
	); err != nil {
		t.Fatal(err)
	}
	got := readReasoningEditConfig(t, path)
	if strings.Count(got, "model_options:") != 1 ||
		!strings.Contains(got, "mode: follow_harness") {
		t.Fatalf("other client was changed or empty parents remain:\n%s", got)
	}
}

func TestClientReasoningPolicyRejectsUnsafeValuesWithoutWriting(t *testing.T) {
	path := writeReasoningEditConfig(t, "global:\n  routing_enabled: true\nclients:\n  - name: claude-code\n    enabled: true\n    protocol_shape: anthropic\n    default_model: zai-org/GLM-5.2\n")
	before := readReasoningEditConfig(t, path)
	tests := []struct {
		name     string
		client   string
		provider string
		model    string
		policy   ReasoningPolicy
	}{
		{name: "missing client", client: "missing", provider: "baseten", model: "org/model", policy: ReasoningPolicy{Mode: ReasoningOff}},
		{name: "provider", client: "claude-code", provider: "openai", model: "gpt", policy: ReasoningPolicy{Mode: ReasoningOff}},
		{name: "empty model", client: "claude-code", provider: "baseten", model: " ", policy: ReasoningPolicy{Mode: ReasoningOff}},
		{name: "newline", client: "claude-code", provider: "baseten", model: "bad\nkey", policy: ReasoningPolicy{Mode: ReasoningOff}},
		{name: "fixed missing effort", client: "claude-code", provider: "baseten", model: "org/model", policy: ReasoningPolicy{Mode: ReasoningFixed}},
		{name: "off with effort", client: "claude-code", provider: "baseten", model: "org/model", policy: ReasoningPolicy{Mode: ReasoningOff, Effort: "high"}},
		{name: "on with effort", client: "claude-code", provider: "baseten", model: "org/model", policy: ReasoningPolicy{Mode: ReasoningOn, Effort: "medium"}},
		{name: "unsafe effort", client: "claude-code", provider: "baseten", model: "org/model", policy: ReasoningPolicy{Mode: ReasoningFixed, Effort: "high\ninjected: true"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := SetClientModelReasoningPolicy(
				path,
				tc.client,
				tc.provider,
				tc.model,
				tc.policy,
			)
			if err == nil {
				t.Fatal("expected error")
			}
			if got := readReasoningEditConfig(t, path); got != before {
				t.Fatalf("file changed on rejected edit:\n%s", got)
			}
		})
	}
}
