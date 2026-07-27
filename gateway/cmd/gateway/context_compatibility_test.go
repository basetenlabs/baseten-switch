package gateway

import (
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

func TestContextCompatibilityWarning(t *testing.T) {
	catalog := []byte(`{
		"anthropic": {
			"id": "anthropic",
			"name": "Anthropic",
			"models": {
				"claude-opus-5": {
					"id": "claude-opus-5",
					"name": "Claude Opus 5",
					"family": "opus",
					"limit": {"context": 1000000, "output": 128000},
					"cost": {"input": 5, "output": 25}
				}
			}
		},
		"openai": {
			"id": "openai",
			"name": "OpenAI",
			"models": {
				"gpt-5": {
					"id": "gpt-5",
					"name": "GPT-5",
					"limit": {"context": 400000, "output": 128000},
					"cost": {"input": 1, "output": 8}
				}
			}
		},
		"baseten": {
			"id": "baseten",
			"name": "Baseten",
			"models": {
				"zai-org/GLM-5.2": {
					"id": "zai-org/GLM-5.2",
					"name": "GLM 5.2",
					"limit": {"context": 200000, "output": 32000},
					"cost": {"input": 0.5, "output": 1.5}
				}
			}
		}
	}`)
	p := pricing.New()
	capturedAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	if err := p.ReplaceModelsDev(catalog, capturedAt, ""); err != nil {
		t.Fatal(err)
	}
	million := int64(1_000_000)
	if got := contextCompatibilityWarning(
		p.Capture(),
		pricing.ProviderBaseten,
		"zai-org/GLM-5.2",
		&million,
	); got == "" {
		t.Fatal("expected a warning for a target with a smaller known context limit")
	}
	if got := contextCompatibilityWarning(
		p.Capture(),
		pricing.ProviderAnthropic,
		"claude-opus-5",
		&million,
	); got != "" {
		t.Fatalf("compatible target warning = %q", got)
	}
	if got := contextCompatibilityWarning(
		p.Capture(),
		pricing.ProviderBaseten,
		"unknown-model",
		&million,
	); got != "" {
		t.Fatalf("unknown target warning = %q", got)
	}
}
