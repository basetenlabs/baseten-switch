package gateway

import (
	"path/filepath"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
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
