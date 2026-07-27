package gateway

import (
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func TestResponsesCompatibilityResolvesPerClient(t *testing.T) {
	routingEnabled := true
	f := &config.File{
		Global: config.Global{
			RoutingEnabled: &routingEnabled,
		},
		Clients: []config.Client{
			{
				Name:                   "codex",
				Enabled:                true,
				BindAddr:               "127.0.0.1:0",
				ProtocolShape:          "openai",
				DefaultModel:           "zai-org/GLM-5.2",
				ResponsesCompatibility: &config.ResponsesCompatibility{},
			},
			{
				Name:          "opencode",
				Enabled:       true,
				BindAddr:      "127.0.0.1:1",
				ProtocolShape: "openai",
				DefaultModel:  "zai-org/GLM-5.2",
			},
		},
	}

	got, err := resolveFromFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("resolved %d clients, want 2", len(got))
	}
	byName := map[string]config.ResolvedResponsesCompatibility{}
	for _, client := range got {
		byName[client.Name] = client.ResponsesCompatibility
	}
	codex := byName["codex"]
	if codex.TextFormatDefault != config.ResponsesCompatibilityModeOn ||
		codex.AdditionalToolsInput != config.ResponsesCompatibilityModeOff ||
		codex.ReasoningEffort != config.ResponsesCompatibilityModeOn ||
		codex.FunctionArgumentsConsistency != config.ResponsesCompatibilityModeOn {
		t.Fatalf("codex compatibility = %+v, want present-block defaults", codex)
	}
	opencode := byName["opencode"]
	if opencode.TextFormatDefault != config.ResponsesCompatibilityModeOff ||
		opencode.AdditionalToolsInput != config.ResponsesCompatibilityModeOff ||
		opencode.ReasoningEffort != config.ResponsesCompatibilityModeOff ||
		opencode.FunctionArgumentsConsistency != config.ResponsesCompatibilityModeOff {
		t.Fatalf("opencode compatibility = %+v, want absent-block all-off policy", opencode)
	}
}

func TestResponsesCompatibilityParticipatesInResolvedHash(t *testing.T) {
	off, err := config.ResolveResponsesCompatibility(nil)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := config.ResolveResponsesCompatibility(&config.ResponsesCompatibility{})
	if err != nil {
		t.Fatal(err)
	}
	base := resolvedClientConfig{
		Name:                   "codex",
		BindAddr:               "127.0.0.1:18081",
		ProtocolShape:          "openai",
		Route:                  "baseten",
		ResponsesCompatibility: off,
	}
	withDefaults := base
	withDefaults.ResponsesCompatibility = defaults
	if base.hash() == withDefaults.hash() {
		t.Fatal("hash must change when responses_compatibility is enabled")
	}

	fields := []struct {
		name string
		edit func(*config.ResolvedResponsesCompatibility)
	}{
		{"text format", func(c *config.ResolvedResponsesCompatibility) {
			c.TextFormatDefault = config.ResponsesCompatibilityModeOff
		}},
		{"additional tools", func(c *config.ResolvedResponsesCompatibility) {
			c.AdditionalToolsInput = config.ResponsesCompatibilityModeOn
		}},
		{"reasoning", func(c *config.ResolvedResponsesCompatibility) {
			c.ReasoningEffort = config.ResponsesCompatibilityModeOff
		}},
		{"function arguments", func(c *config.ResolvedResponsesCompatibility) {
			c.FunctionArgumentsConsistency = config.ResponsesCompatibilityModeOff
		}},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			changed := withDefaults
			field.edit(&changed.ResponsesCompatibility)
			if withDefaults.hash() == changed.hash() {
				t.Fatalf("hash must change when %s changes", field.name)
			}
		})
	}
}
