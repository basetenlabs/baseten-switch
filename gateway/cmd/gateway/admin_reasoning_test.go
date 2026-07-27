package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

const adminReasoningCatalogFixture = `{
  "anthropic": {
    "id": "anthropic",
    "models": {
      "claude-test": {"id": "claude-test", "name": "Claude Test"}
    }
  },
  "openai": {
    "id": "openai",
    "models": {
      "gpt-test": {"id": "gpt-test", "name": "GPT Test"}
    }
  },
  "baseten": {
    "id": "baseten",
    "models": {
      "zai-org/GLM-5.2": {
        "id": "zai-org/GLM-5.2",
        "name": "GLM 5.2",
        "family": "glm",
        "reasoning": true,
        "reasoning_options": [{"type": "toggle"}]
      },
      "moonshotai/Kimi-K2.7-Code": {
        "id": "moonshotai/Kimi-K2.7-Code",
        "name": "Kimi K2.7 Code",
        "reasoning": true,
        "reasoning_options": [{"type": "toggle"}]
      },
      "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B": {
        "id": "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B",
        "name": "Nemotron Ultra",
        "reasoning": true,
        "reasoning_options": [{"type": "toggle"}]
      },
      "deepseek-ai/DeepSeek-V4-Pro": {
        "id": "deepseek-ai/DeepSeek-V4-Pro",
        "name": "DeepSeek V4 Pro",
        "reasoning": true,
        "reasoning_options": [
          {"type": "effort", "values": ["low", "high"]}
        ]
      },
      "example/No-Control": {
        "id": "example/No-Control",
        "name": "No Control",
        "reasoning": true,
        "reasoning_options": []
      }
    }
  }
}`

func adminReasoningSnapshot(
	t *testing.T,
	capturedAt time.Time,
) *pricing.Snapshot {
	t.Helper()
	catalog := pricing.New()
	if err := catalog.ReplaceModelsDev(
		[]byte(adminReasoningCatalogFixture),
		capturedAt,
		`"reasoning-status"`,
	); err != nil {
		t.Fatal(err)
	}
	return catalog.Capture()
}

func TestClientReasoningProjectionDeduplicatesReachableTargets(t *testing.T) {
	snapshot := adminReasoningSnapshot(t, time.Now().UTC())
	rc := resolvedClientConfig{
		Name:          "claude-code",
		ProtocolShape: "anthropic",
		Route:         "baseten",
		DefaultModel:  "zai-org/GLM-5.2",
		ModelAliases: map[string]string{
			"claude-baseten-glm":  "zai-org/GLM-5.2",
			"claude-baseten-kimi": "moonshotai/Kimi-K2.7-Code",
		},
		ModelRoutes: map[string]string{
			"opus":   "claude-baseten-glm",
			"sonnet": "zai-org/GLM-5.2",
		},
		SubagentModel: "claude-baseten-glm",
		ModelOptions: config.ModelOptions{
			"baseten": {
				"zai-org/GLM-5.2": {},
				// An explicit raw slug is reachable on an Anthropic
				// client even when it is not a current route surface.
				"deepseek-ai/DeepSeek-V4-Pro": {},
			},
		},
	}
	got := computeClientModelOptions(rc, snapshot)
	models := got[pricing.ProviderBaseten]
	if len(models) != 3 {
		t.Fatalf("models = %#v, want three unique canonical targets", models)
	}
	glm := models["zai-org/GLM-5.2"].Reasoning
	if glm == nil ||
		glm.Configured.Mode != "default" ||
		glm.Effective.Mode != "off" ||
		glm.Source != "compatibility_default" ||
		len(glm.AvailableModes) != 2 ||
		glm.AvailableModes[0] != "off" ||
		glm.AvailableModes[1] != "follow_harness" ||
		!glm.Available ||
		glm.UnavailableReason != "" ||
		glm.Error != "" {
		t.Fatalf("GLM projection = %+v", glm)
	}
	kimi := models["moonshotai/Kimi-K2.7-Code"].Reasoning
	if kimi == nil ||
		kimi.Effective.Mode != "off" ||
		kimi.Source != "compatibility_default" ||
		!kimi.Available ||
		len(kimi.AvailableModes) != 2 ||
		kimi.AvailableModes[0] != "off" ||
		kimi.AvailableModes[1] != "follow_harness" {
		t.Fatalf("Kimi projection = %+v", kimi)
	}
	deepseek := models["deepseek-ai/DeepSeek-V4-Pro"].Reasoning
	if deepseek == nil ||
		deepseek.Effective.Mode != "passthrough" ||
		deepseek.Source != "internal_passthrough" ||
		!deepseek.Available ||
		len(deepseek.AvailableModes) != 0 ||
		deepseek.UnavailableReason != "" ||
		deepseek.Error != "" {
		t.Fatalf("adapter-less projection = %+v", deepseek)
	}
}

func TestClientReasoningProjectionDefaultsFromCapabilityAndAdapter(
	t *testing.T,
) {
	snapshot := adminReasoningSnapshot(t, time.Now().UTC())
	for _, tc := range []struct {
		name       string
		model      string
		wantMode   string
		wantSource string
		wantModes  []string
	}{
		{
			name:       "GLM toggle",
			model:      "zai-org/GLM-5.2",
			wantMode:   "off",
			wantSource: "compatibility_default",
			wantModes:  []string{"off", "follow_harness"},
		},
		{
			name:       "Kimi toggle",
			model:      "moonshotai/Kimi-K2.7-Code",
			wantMode:   "off",
			wantSource: "compatibility_default",
			wantModes:  []string{"off", "follow_harness"},
		},
		{
			name:       "Nemotron toggle",
			model:      "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B",
			wantMode:   "off",
			wantSource: "compatibility_default",
			wantModes:  []string{"off", "follow_harness"},
		},
		{
			name:       "effort only is read-only passthrough",
			model:      "deepseek-ai/DeepSeek-V4-Pro",
			wantMode:   "passthrough",
			wantSource: "internal_passthrough",
			wantModes:  []string{},
		},
		{
			name:       "no controls is read-only passthrough",
			model:      "example/No-Control",
			wantMode:   "passthrough",
			wantSource: "internal_passthrough",
			wantModes:  []string{},
		},
		{
			name:       "unknown is read-only passthrough",
			model:      "example/Unknown",
			wantMode:   "passthrough",
			wantSource: "internal_passthrough",
			wantModes:  []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := resolvedClientConfig{
				Name:          "claude-code",
				ProtocolShape: "anthropic",
				Route:         "baseten",
				DefaultModel:  tc.model,
			}
			status := computeClientModelOptions(
				rc,
				snapshot,
			)[pricing.ProviderBaseten][tc.model].Reasoning
			if status == nil ||
				status.Configured.Mode != "default" ||
				status.Effective.Mode != tc.wantMode ||
				status.Source != tc.wantSource ||
				!status.Available ||
				status.UnavailableReason != "" ||
				status.Error != "" ||
				!slices.Equal(status.AvailableModes, tc.wantModes) {
				t.Fatalf("projection = %+v", status)
			}
		})
	}
}

func TestClientReasoningProjectionRejectsSavedUnsupportedOff(t *testing.T) {
	snapshot := adminReasoningSnapshot(t, time.Now().UTC())
	rc := resolvedClientConfig{
		Name:          "claude-code",
		ProtocolShape: "anthropic",
		Route:         "baseten",
		DefaultModel:  "deepseek-ai/DeepSeek-V4-Pro",
		ModelOptions: config.ModelOptions{
			pricing.ProviderBaseten: {
				"deepseek-ai/DeepSeek-V4-Pro": {
					Reasoning: &config.ReasoningPolicy{
						Mode: config.ReasoningOff,
					},
				},
			},
		},
	}
	status := computeClientModelOptions(
		rc,
		snapshot,
	)[pricing.ProviderBaseten]["deepseek-ai/DeepSeek-V4-Pro"].Reasoning
	if status == nil ||
		status.Configured.Mode != "off" ||
		status.Effective.Mode != "off" ||
		status.Available ||
		status.UnavailableReason != reasoningUnavailableAdapter ||
		status.Error == "" {
		t.Fatalf("saved unsupported Off projection = %+v", status)
	}
}

func TestClientReasoningProjectionPreservesUnavailableSavedEffort(t *testing.T) {
	snapshot := adminReasoningSnapshot(t, time.Now().UTC())
	rc := resolvedClientConfig{
		Name:          "claude-code",
		ProtocolShape: "anthropic",
		Route:         "baseten",
		DefaultModel:  "deepseek-ai/DeepSeek-V4-Pro",
		ModelOptions: config.ModelOptions{
			"baseten": {
				"deepseek-ai/DeepSeek-V4-Pro": {
					Reasoning: &config.ReasoningPolicy{
						Mode:   config.ReasoningFixed,
						Effort: "xhigh",
					},
				},
			},
		},
	}
	status := computeClientModelOptions(
		rc,
		snapshot,
	)[pricing.ProviderBaseten]["deepseek-ai/DeepSeek-V4-Pro"].Reasoning
	if status == nil ||
		status.Configured.Mode != "fixed" ||
		status.Configured.Effort != "xhigh" ||
		status.Effective.Mode != "fixed" ||
		status.Effective.Effort != "xhigh" ||
		status.Available ||
		status.UnavailableReason != reasoningUnavailableEffortRemoved ||
		status.Error == "" {
		t.Fatalf("saved effort projection = %+v", status)
	}
}

func TestClientReasoningProjectionStaleRemainsUsableAndUnknownIsLoud(
	t *testing.T,
) {
	staleSnapshot := adminReasoningSnapshot(
		t,
		time.Now().UTC().Add(-72*time.Hour),
	)
	rc := resolvedClientConfig{
		Name:          "claude-code",
		ProtocolShape: "anthropic",
		Route:         "baseten",
		DefaultModel:  "zai-org/GLM-5.2",
	}
	stale := computeClientModelOptions(
		rc,
		staleSnapshot,
	)[pricing.ProviderBaseten]["zai-org/GLM-5.2"].Reasoning
	if stale == nil ||
		!stale.Available ||
		len(stale.AvailableModes) != 2 ||
		stale.AvailableModes[0] != "off" ||
		stale.AvailableModes[1] != "follow_harness" ||
		stale.UnavailableReason != "" ||
		stale.Error != "" {
		t.Fatalf("stale validated projection = %+v", stale)
	}

	rc.DefaultModel = "zai-org/GLM-6"
	unknown := computeClientModelOptions(
		rc,
		staleSnapshot,
	)[pricing.ProviderBaseten]["zai-org/GLM-6"].Reasoning
	if unknown == nil ||
		unknown.Effective.Mode != "passthrough" ||
		unknown.Source != "internal_passthrough" ||
		!unknown.Available ||
		len(unknown.AvailableModes) != 0 ||
		unknown.UnavailableReason != "" ||
		unknown.Error != "" {
		t.Fatalf("unknown GLM projection = %+v", unknown)
	}
}

func TestClientReasoningProjectionUsesEachClientAdapter(
	t *testing.T,
) {
	snapshot := adminReasoningSnapshot(t, time.Now().UTC())
	options := config.ModelOptions{
		"baseten": {
			"zai-org/GLM-5.2": {
				Reasoning: &config.ReasoningPolicy{
					Mode: config.ReasoningFollowHarness,
				},
			},
		},
	}
	claude := resolvedClientConfig{
		Name:          "claude-code",
		ProtocolShape: "anthropic",
		Route:         "baseten",
		DefaultModel:  "zai-org/GLM-5.2",
		ModelOptions:  options,
	}
	codex := resolvedClientConfig{
		Name:          "codex",
		ProtocolShape: "openai",
		Route:         "baseten",
		DefaultModel:  "zai-org/GLM-5.2",
		ModelOptions:  options,
	}
	claudeStatus := computeClientModelOptions(
		claude,
		snapshot,
	)[pricing.ProviderBaseten]["zai-org/GLM-5.2"].Reasoning
	codexStatus := computeClientModelOptions(
		codex,
		snapshot,
	)[pricing.ProviderBaseten]["zai-org/GLM-5.2"].Reasoning
	if claudeStatus == nil || !claudeStatus.Available {
		t.Fatalf("Claude projection = %+v", claudeStatus)
	}
	if codexStatus == nil ||
		codexStatus.Available ||
		len(codexStatus.AvailableModes) != 0 ||
		codexStatus.UnavailableReason != reasoningUnavailableAdapter ||
		codexStatus.Error == "" {
		t.Fatalf("Codex projection = %+v", codexStatus)
	}
}

func TestAdminStatusIncludesDisabledClientReasoningProjection(t *testing.T) {
	enabled := true
	file := &config.File{
		Global: config.Global{
			RoutingEnabled: &enabled,
		},
		Clients: []config.Client{{
			Name:          "parked-claude",
			Enabled:       false,
			BindAddr:      "127.0.0.1:0",
			ProtocolShape: "anthropic",
			DefaultModel:  "zai-org/GLM-5.2",
			ModelOptions: config.ModelOptions{
				"baseten": {
					"zai-org/GLM-5.2": {
						Reasoning: &config.ReasoningPolicy{
							Mode: config.ReasoningFollowHarness,
						},
					},
				},
			},
		}},
	}
	catalog := pricing.New()
	if err := catalog.ReplaceModelsDev(
		[]byte(adminReasoningCatalogFixture),
		time.Now().UTC(),
		`"reasoning-status"`,
	); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{
		cfg: Config{
			ConfigPath: filepath.Join(t.TempDir(), "gateway.yaml"),
		},
		pricing:          catalog,
		activeConfigFile: file,
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/admin/status",
		nil,
	)
	recorder := httptest.NewRecorder()
	g.adminStatus(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Clients []struct {
			Name         string                  `json:"name"`
			Enabled      bool                    `json:"enabled"`
			ModelOptions adminClientModelOptions `json:"model_options"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Clients) != 1 ||
		response.Clients[0].Name != "parked-claude" ||
		response.Clients[0].Enabled {
		t.Fatalf("clients = %+v", response.Clients)
	}
	status := response.Clients[0].ModelOptions[pricing.ProviderBaseten]["zai-org/GLM-5.2"].Reasoning
	if status == nil ||
		status.Configured.Mode != "follow_harness" ||
		status.Effective.Mode != "follow_harness" ||
		status.Source != "user_config" ||
		!status.Available {
		t.Fatalf("disabled client projection = %+v", status)
	}
}

func TestReasoningPreflightChecksOnlyRequestedClient(t *testing.T) {
	enabled := true
	file := &config.File{
		Global: config.Global{
			RoutingEnabled: &enabled,
		},
		Clients: []config.Client{
			{
				Name:          "parked-claude",
				Enabled:       false,
				BindAddr:      "127.0.0.1:0",
				ProtocolShape: "anthropic",
			},
			{
				Name:          "codex",
				Enabled:       true,
				BindAddr:      "127.0.0.1:0",
				ProtocolShape: "openai",
			},
		},
	}
	catalog := pricing.New()
	if err := catalog.ReplaceModelsDev(
		[]byte(adminReasoningCatalogFixture),
		time.Now().UTC(),
		`"reasoning-preflight"`,
	); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{
		cfg:              Config{},
		pricing:          catalog,
		activeConfigFile: file,
	}
	body := `{
		"client":"parked-claude",
		"provider":"baseten",
		"model":"moonshotai/Kimi-K2.7-Code",
		"policy":{"mode":"off"}
	}`
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/admin/reasoning/preflight",
		strings.NewReader(body),
	)
	recorder := httptest.NewRecorder()
	g.adminReasoningPreflight(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response adminReasoningPreflightResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Available || len(response.Clients) != 1 {
		t.Fatalf("preflight = %+v", response)
	}
	claude := response.Clients[0]
	if claude.Name != "parked-claude" ||
		claude.Enabled ||
		!claude.Reachable ||
		!claude.Supported ||
		len(claude.Reachability) != 1 ||
		claude.Reachability[0] != "explicit_raw_slug" ||
		len(claude.FailureBehaviors) != 1 ||
		claude.FailureBehaviors[0] != "local_error" ||
		len(claude.AvailableModes) != 2 ||
		claude.AvailableModes[0] != "off" ||
		claude.AvailableModes[1] != "follow_harness" ||
		claude.UnavailableReason != "" ||
		claude.Error != "" {
		t.Fatalf("Claude impact = %+v", claude)
	}
	if response.Error != "" {
		t.Fatalf("preflight error = %q, want empty", response.Error)
	}
}

func TestReasoningPreflightRejectsInvalidPolicyStructure(t *testing.T) {
	catalog := pricing.New()
	if err := catalog.ReplaceModelsDev(
		[]byte(adminReasoningCatalogFixture),
		time.Now().UTC(),
		`"reasoning-preflight"`,
	); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{pricing: catalog}
	for _, test := range []struct {
		name   string
		policy string
		want   string
	}{
		{
			name:   "off with effort",
			policy: `{"mode":"off","effort":"high"}`,
			want:   `mode "off" forbids effort`,
		},
		{
			name:   "follow with effort",
			policy: `{"mode":"follow_harness","effort":"high"}`,
			want:   `mode "follow_harness" forbids effort`,
		},
		{
			name:   "fixed without effort",
			policy: `{"mode":"fixed"}`,
			want:   `mode "fixed" requires effort`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/admin/reasoning/preflight",
				strings.NewReader(
					`{"client":"claude-code","provider":"baseten","model":"zai-org/GLM-5.2","policy":`+
						test.policy+`}`,
				),
			)
			recorder := httptest.NewRecorder()
			g.adminReasoningPreflight(recorder, request)
			var response adminReasoningPreflightResponse
			if err := json.Unmarshal(
				recorder.Body.Bytes(),
				&response,
			); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusOK ||
				!strings.Contains(response.Error, test.want) ||
				response.Available {
				t.Fatalf(
					"status=%d response=%+v, want error containing %q",
					recorder.Code,
					response,
					test.want,
				)
			}
		})
	}
}
