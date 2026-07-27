package config

import (
	"strings"
	"testing"
)

func TestValidateRoutingPolicyAcceptsClientReasoningModes(t *testing.T) {
	enabled := true
	for _, tc := range []struct {
		name   string
		policy ReasoningPolicy
	}{
		{name: "off", policy: ReasoningPolicy{Mode: ReasoningOff}},
		{name: "follow harness", policy: ReasoningPolicy{Mode: ReasoningFollowHarness}},
		{name: "fixed effort", policy: ReasoningPolicy{Mode: ReasoningFixed, Effort: "high"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := &File{
				Global: Global{RoutingEnabled: &enabled},
				Clients: []Client{{
					Name:          "claude-code",
					ProtocolShape: "anthropic",
					ModelOptions: ModelOptions{
						"baseten": {
							"zai-org/GLM-5.2": ModelOption{Reasoning: &tc.policy},
						},
					},
				}},
			}
			if err := ValidateRoutingPolicy(file); err != nil {
				t.Fatalf("ValidateRoutingPolicy() error = %v", err)
			}
		})
	}
}

func TestValidateRoutingPolicyRejectsInvalidClientReasoningStructure(t *testing.T) {
	enabled := true
	for _, tc := range []struct {
		name    string
		options ModelOptions
		want    string
	}{
		{
			name: "unsupported provider",
			options: ModelOptions{
				"anthropic": {
					"claude-opus-4-1": ModelOption{
						Reasoning: &ReasoningPolicy{Mode: ReasoningOff},
					},
				},
			},
			want: `provider "anthropic" is unsupported`,
		},
		{
			name: "empty model",
			options: ModelOptions{
				"baseten": {
					"": ModelOption{
						Reasoning: &ReasoningPolicy{Mode: ReasoningOff},
					},
				},
			},
			want: "contains an empty model id",
		},
		{
			name: "missing reasoning",
			options: ModelOptions{
				"baseten": {
					"zai-org/GLM-5.2": ModelOption{},
				},
			},
			want: "reasoning is required",
		},
		{
			name: "unknown mode",
			options: ModelOptions{
				"baseten": {
					"zai-org/GLM-5.2": ModelOption{
						Reasoning: &ReasoningPolicy{Mode: "turbo"},
					},
				},
			},
			want: `mode "turbo" is invalid`,
		},
		{
			name: "fixed without effort",
			options: ModelOptions{
				"baseten": {
					"deepseek-ai/DeepSeek-V4-Pro": ModelOption{
						Reasoning: &ReasoningPolicy{Mode: ReasoningFixed},
					},
				},
			},
			want: `mode "fixed" requires effort`,
		},
		{
			name: "off with effort",
			options: ModelOptions{
				"baseten": {
					"zai-org/GLM-5.2": ModelOption{
						Reasoning: &ReasoningPolicy{
							Mode:   ReasoningOff,
							Effort: "high",
						},
					},
				},
			},
			want: `mode "off" forbids effort`,
		},
		{
			name: "follow harness with effort",
			options: ModelOptions{
				"baseten": {
					"zai-org/GLM-5.2": ModelOption{
						Reasoning: &ReasoningPolicy{
							Mode:   ReasoningFollowHarness,
							Effort: "high",
						},
					},
				},
			},
			want: `mode "follow_harness" forbids effort`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRoutingPolicy(&File{
				Global: Global{RoutingEnabled: &enabled},
				Clients: []Client{{
					Name:          "claude-code",
					ProtocolShape: "anthropic",
					ModelOptions:  tc.options,
				}},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateRoutingPolicy() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestStrictConfigParsesClientReasoningModelOptions(t *testing.T) {
	raw := []byte(`global:
  routing_enabled: true
clients:
  - name: claude-code
    protocol_shape: anthropic
    model_options:
      baseten:
        zai-org/GLM-5.2:
          reasoning:
            mode: follow_harness
`)
	var file File
	if err := UnmarshalStrict(raw, &file); err != nil {
		t.Fatalf("UnmarshalStrict() error = %v", err)
	}
	if err := ValidateRoutingPolicy(&file); err != nil {
		t.Fatalf("ValidateRoutingPolicy() error = %v", err)
	}
	got := file.Clients[0].ModelOptions["baseten"]["zai-org/GLM-5.2"].Reasoning
	if got == nil || got.Mode != ReasoningFollowHarness || got.Effort != "" {
		t.Fatalf("reasoning policy = %#v", got)
	}
}

func TestStrictConfigRejectsGlobalModelOptions(t *testing.T) {
	raw := []byte(`global:
  routing_enabled: true
  model_options: {}
clients: []
`)
	var file File
	err := UnmarshalStrict(raw, &file)
	if err == nil || !strings.Contains(err.Error(), "model_options") {
		t.Fatalf("UnmarshalStrict() error = %v, want unknown global.model_options", err)
	}
}
