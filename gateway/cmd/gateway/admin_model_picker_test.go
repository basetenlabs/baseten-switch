package gateway

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

func TestModelPickerAdminStatusPreservesConfiguredOrder(t *testing.T) {
	basSrv := recordingStub(t, nil, "B")
	defer basSrv.Close()
	cfg := testConfig(t, basSrv.URL, basSrv.URL)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
	rc := modelRoutesClient(t, "baseten", nil)
	writeModelRoutesYAML(t, cfg.ConfigPath, rc)
	file, err := config.Load(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	file.Clients[0].ModelPicker = &config.ModelPicker{
		Enabled: true,
		Models: []config.ModelPickerModel{
			{
				Alias: "claude-baseten-nemotron",
			},
			{
				Alias: "claude-baseten-glm-5-2",
			},
		},
	}
	if err := config.Save(cfg.ConfigPath, file); err != nil {
		t.Fatal(err)
	}
	cfgForLoad := cfg
	loadedSnapshot, err := loadResolvedConfigSnapshot(&cfgForLoad)
	if err != nil {
		t.Fatalf("load config snapshot: %v", err)
	}
	g, adminL, _ := newGateway(t, cfg, loadedSnapshot.clients...)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	status := adminStatusGet(t, g)
	clients := status["clients"].([]any)
	if len(clients) != 1 {
		t.Fatalf("clients = %v, want one", clients)
	}
	picker, ok := clients[0].(map[string]any)["model_picker"].(map[string]any)
	if !ok {
		t.Fatalf("model_picker = %v, want object", clients[0].(map[string]any)["model_picker"])
	}
	if picker["enabled"] != true {
		t.Errorf("model_picker.enabled = %v, want true", picker["enabled"])
	}
	models, ok := picker["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("model_picker.models = %v, want two rows", picker["models"])
	}
	first := models[0].(map[string]any)
	if first["alias"] != "claude-baseten-nemotron" ||
		first["slug"] != "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B" ||
		first["label"] != "NVIDIA Nemotron 3 Ultra 550B A55B via Baseten" ||
		first["description"] != "Served by Baseten." {
		t.Errorf("first picker row = %v", first)
	}
	second := models[1].(map[string]any)
	if second["alias"] != "claude-baseten-glm-5-2" ||
		second["slug"] != "zai-org/GLM-5.2" ||
		second["label"] != "GLM 5.2 via Baseten" ||
		second["description"] != "Served by Baseten." {
		t.Errorf("second picker row = %v", second)
	}
}

func TestModelPickerAdminStatusProjectsExactBasetenContextTokens(t *testing.T) {
	catalog := pricing.New()
	if err := catalog.ReplaceModelsDev([]byte(`{
		"anthropic":{"models":{"claude-test":{"id":"claude-test","name":"Claude Test","limit":{"context":200000,"output":1}}}},
		"openai":{"models":{"gpt-test":{"id":"gpt-test","name":"GPT Test","limit":{"context":200000,"output":1}}}},
		"baseten":{"models":{
			"org/one-million":{"id":"org/one-million","name":"One Million","limit":{"context":1048576,"output":1}},
			"org/two-hundred-thousand":{"id":"org/two-hundred-thousand","name":"Two Hundred Thousand","limit":{"context":200000,"output":1}}
		}}
	}`), time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC), ""); err != nil {
		t.Fatal(err)
	}
	client := config.Client{
		ModelAliases: map[string]string{
			"claude-baseten-million":  "org/one-million",
			"claude-baseten-standard": "org/two-hundred-thousand",
			"claude-baseten-unknown":  "org/not-in-models-dev",
		},
		ModelPicker: &config.ModelPicker{
			Enabled: true,
			Models: []config.ModelPickerModel{
				{Alias: "claude-baseten-million"},
				{Alias: "claude-baseten-standard"},
				{Alias: "claude-baseten-unknown"},
			},
		},
	}
	status := computeModelPickerStatus(client, catalog.Capture())
	if got := status.Models[0].ContextTokens; got != 1_048_576 {
		t.Fatalf("one-million context_tokens = %d", got)
	}
	if got := status.Models[1].ContextTokens; got != 200_000 {
		t.Fatalf("standard context_tokens = %d", got)
	}
	if got := status.Models[2].ContextTokens; got != 0 {
		t.Fatalf("unknown context_tokens = %d, want 0", got)
	}
}
