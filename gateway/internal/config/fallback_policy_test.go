package config

import (
	"strings"
	"testing"
)

func boolPointer(value bool) *bool { return &value }

func TestResolveFallbackPolicyDefaultsIndependently(t *testing.T) {
	tests := []struct {
		name string
		raw  *FallbackPolicy
		want ResolvedFallbackPolicy
	}{
		{"absent", nil, ResolvedFallbackPolicy{OnBaseten429: true, OnBaseten5xx: true}},
		{"empty", &FallbackPolicy{}, ResolvedFallbackPolicy{OnBaseten429: true, OnBaseten5xx: true}},
		{"429 off", &FallbackPolicy{OnBaseten429: boolPointer(false)}, ResolvedFallbackPolicy{OnBaseten429: false, OnBaseten5xx: true}},
		{"5xx off", &FallbackPolicy{OnBaseten5xx: boolPointer(false)}, ResolvedFallbackPolicy{OnBaseten429: true, OnBaseten5xx: false}},
		{"both off", &FallbackPolicy{OnBaseten429: boolPointer(false), OnBaseten5xx: boolPointer(false)}, ResolvedFallbackPolicy{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ResolveFallbackPolicy(test.raw); got != test.want {
				t.Fatalf("ResolveFallbackPolicy() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestFallbackPolicyRejectsNullAndUnknownValues(t *testing.T) {
	prefix := "global:\n  routing_enabled: true\n"
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"null block", prefix + "  fallback_policy: null\n", "must be an object, not null"},
		{"null 429", prefix + "  fallback_policy:\n    on_baseten_429: null\n", "on_baseten_429 must be true or false, not null"},
		{"null 5xx", prefix + "  fallback_policy:\n    on_baseten_5xx: null\n", "on_baseten_5xx must be true or false, not null"},
		{"unknown", prefix + "  fallback_policy:\n    on_timeout: true\n", "field on_timeout not found"},
		{"wrong type", prefix + "  fallback_policy:\n    on_baseten_429: yes-please\n", "cannot unmarshal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var file File
			err := UnmarshalStrict([]byte(test.yaml), &file)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("UnmarshalStrict() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateNativeFallbackModel(t *testing.T) {
	base := Client{
		Name:          "claude-code",
		ProtocolShape: "anthropic",
		FallbackRoute: "anthropic",
		ModelAliases: map[string]string{
			"claude-baseten-glm": "example/GLM",
		},
	}
	tests := []struct {
		name  string
		model string
		alter func(*Client)
		want  string
	}{
		{"valid opus", DefaultClaudeNativeFallbackModel, nil, ""},
		{"empty optional", "", nil, ""},
		{"bare family", "opus", nil, "full anthropic-native model ID"},
		{"raw slug", "example/GLM", nil, "full anthropic-native model ID"},
		{"configured alias", "claude-baseten-glm", nil, "configured Baseten alias"},
		{"unknown alias namespace", "claude-baseten-removed", nil, "full anthropic-native model ID"},
		{"compatibility sentinel", ManagedCodexCompatibilityModel, nil, "full anthropic-native model ID"},
		{"leading whitespace", " claude-opus-5", nil, "surrounding whitespace"},
		{"missing route", DefaultClaudeNativeFallbackModel, func(client *Client) { client.FallbackRoute = "" }, "requires fallback_route"},
		{"wrong native namespace", "gpt-5.6", nil, "full anthropic-native model ID"},
		{"openai unsupported", "gpt-5.6", func(client *Client) {
			client.ProtocolShape = "openai"
			client.FallbackRoute = "openai"
		}, "supported only for anthropic-shape"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := base
			client.NativeFallbackModel = test.model
			if test.alter != nil {
				test.alter(&client)
			}
			err := ValidateNativeFallbackModel(client)
			if test.want == "" {
				if err != nil {
					t.Fatalf("ValidateNativeFallbackModel() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateNativeFallbackModel() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestNativeFallbackModelRejectsExplicitNullOrEmpty(t *testing.T) {
	prefix := `global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: false
    protocol_shape: anthropic
    fallback_route: anthropic
`
	for _, test := range []struct {
		name, value, want string
	}{
		{"null", "null", "not null"},
		{"empty", `""`, "not empty"},
		{"blank", `"   "`, "not empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var file File
			err := UnmarshalStrict([]byte(prefix+"    native_fallback_model: "+test.value+"\n"), &file)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("UnmarshalStrict() error=%v want containing %q", err, test.want)
			}
		})
	}
}
