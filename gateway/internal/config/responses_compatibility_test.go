package config

import (
	"strings"
	"testing"
)

func TestResolveResponsesCompatibilityAbsentIsOff(t *testing.T) {
	got, err := ResolveResponsesCompatibility(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.TextFormatDefault != ResponsesCompatibilityModeOff ||
		got.AdditionalToolsInput != ResponsesCompatibilityModeOff ||
		got.ReasoningEffort != ResponsesCompatibilityModeOff ||
		got.FunctionArgumentsConsistency != ResponsesCompatibilityModeOff {
		t.Fatalf("absent block resolved to %+v, want all rules off", got)
	}
}

func TestResolveResponsesCompatibilityPresentDefaults(t *testing.T) {
	got, err := ResolveResponsesCompatibility(&ResponsesCompatibility{})
	if err != nil {
		t.Fatal(err)
	}
	if got.TextFormatDefault != ResponsesCompatibilityModeOn ||
		got.AdditionalToolsInput != ResponsesCompatibilityModeOff ||
		got.ReasoningEffort != ResponsesCompatibilityModeOn ||
		got.FunctionArgumentsConsistency != ResponsesCompatibilityModeOn {
		t.Fatalf("present empty block resolved to %+v, want documented defaults", got)
	}
}

func TestResolveResponsesCompatibilityExplicitValues(t *testing.T) {
	got, err := ResolveResponsesCompatibility(&ResponsesCompatibility{
		TextFormatDefault:            ResponsesCompatibilityModeOff,
		AdditionalToolsInput:         ResponsesCompatibilityModeOn,
		ReasoningEffort:              ResponsesCompatibilityModeOff,
		FunctionArgumentsConsistency: ResponsesCompatibilityModeOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.TextFormatDefault != ResponsesCompatibilityModeOff ||
		got.AdditionalToolsInput != ResponsesCompatibilityModeOn ||
		got.ReasoningEffort != ResponsesCompatibilityModeOff ||
		got.FunctionArgumentsConsistency != ResponsesCompatibilityModeOff {
		t.Fatalf("explicit block resolved to %+v", got)
	}
}

func TestResolveResponsesCompatibilityRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     ResponsesCompatibility
		wantErr string
	}{
		{
			name:    "text mode",
			raw:     ResponsesCompatibility{TextFormatDefault: "enabled"},
			wantErr: "text_format_default: invalid mode",
		},
		{
			name:    "additional tools mode",
			raw:     ResponsesCompatibility{AdditionalToolsInput: "learn"},
			wantErr: "additional_tools_input: invalid mode",
		},
		{
			name:    "reasoning mode",
			raw:     ResponsesCompatibility{ReasoningEffort: "force"},
			wantErr: "reasoning_effort: invalid mode",
		},
		{
			name:    "function arguments mode",
			raw:     ResponsesCompatibility{FunctionArgumentsConsistency: "true"},
			wantErr: "function_arguments_consistency: invalid mode",
		},
		{
			name:    "auto mode",
			raw:     ResponsesCompatibility{ReasoningEffort: "auto"},
			wantErr: "invalid mode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveResponsesCompatibility(&tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestResponsesCompatibilityStrictYAML(t *testing.T) {
	var got File
	err := UnmarshalStrict([]byte(`global:
  routing_enabled: true
clients:
  - name: codex
    enabled: false
    protocol_shape: openai
    responses_compatibility:
      unknown_rule: on
`), &got)
	if err == nil || !strings.Contains(err.Error(), "field unknown_rule not found") {
		t.Fatalf("error = %v, want unknown nested field rejection", err)
	}
}

func TestValidateRoutingPolicyResponsesCompatibility(t *testing.T) {
	routingEnabled := true
	base := func(shape string, compatibility *ResponsesCompatibility) *File {
		return &File{
			Global: Global{RoutingEnabled: &routingEnabled},
			Clients: []Client{{
				Name:                   "codex",
				Enabled:                false,
				ProtocolShape:          shape,
				ResponsesCompatibility: compatibility,
			}},
		}
	}

	if err := ValidateRoutingPolicy(base("openai", &ResponsesCompatibility{})); err != nil {
		t.Fatalf("valid openai policy: %v", err)
	}
	if err := ValidateRoutingPolicy(base("openai", nil)); err != nil {
		t.Fatalf("absent block: %v", err)
	}
	if err := ValidateRoutingPolicy(base("anthropic", &ResponsesCompatibility{})); err == nil ||
		!strings.Contains(err.Error(), "requires protocol_shape openai") {
		t.Fatalf("anthropic block error = %v, want shape rejection", err)
	}
	if err := ValidateRoutingPolicy(base("openai", &ResponsesCompatibility{ReasoningEffort: "invalid"})); err == nil ||
		!strings.Contains(err.Error(), `client "codex": responses_compatibility.reasoning_effort`) {
		t.Fatalf("invalid mode error = %v, want client-scoped field rejection", err)
	}
}
