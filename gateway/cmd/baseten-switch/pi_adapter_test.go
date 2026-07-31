package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakePiCatalog struct {
	models []piProviderModel
	err    error
	calls  int
}

type defaultPiCapabilities struct{}

func (defaultPiCapabilities) Enrich(
	_ context.Context,
	models []piProviderModel,
) (piCapabilityEnrichment, error) {
	enriched := append([]piProviderModel(nil), models...)
	for index := range enriched {
		enriched[index].Input = append([]string(nil), enriched[index].Input...)
		if len(enriched[index].Input) == 0 {
			enriched[index].Input = []string{"text"}
		}
		enriched[index].CapabilityKnown = true
	}
	return piCapabilityEnrichment{Models: enriched, Matched: len(enriched)}, nil
}

type fakePiCapabilities struct {
	result piCapabilityEnrichment
	err    error
	calls  int
}

func (f *fakePiCapabilities) Enrich(
	_ context.Context,
	_ []piProviderModel,
) (piCapabilityEnrichment, error) {
	f.calls++
	return f.result, f.err
}

func (f *fakePiCatalog) Models(context.Context) ([]piProviderModel, error) {
	f.calls++
	return f.models, f.err
}

type fakePiStore struct {
	installResult   piInstallResult
	installErr      error
	installRequests []piInstallRequest
	statusResult    piStatusResult
	statusErr       error
	statusDirs      []string
	uninstallResult piUninstallResult
	uninstallErr    error
	uninstallDirs   []string
}

func (f *fakePiStore) Install(_ context.Context, request piInstallRequest) (piInstallResult, error) {
	f.installRequests = append(f.installRequests, request)
	return f.installResult, f.installErr
}

func (f *fakePiStore) Status(_ context.Context, agentDir string) (piStatusResult, error) {
	f.statusDirs = append(f.statusDirs, agentDir)
	return f.statusResult, f.statusErr
}

func (f *fakePiStore) Uninstall(_ context.Context, agentDir string) (piUninstallResult, error) {
	f.uninstallDirs = append(f.uninstallDirs, agentDir)
	return f.uninstallResult, f.uninstallErr
}

func piTestDependencies(catalog piCatalogSource, store piConfigStore, env map[string]string) piAdapterDependencies {
	return piAdapterDependencies{
		catalog:      catalog,
		capabilities: defaultPiCapabilities{},
		store:        store,
		homeDir:      func() (string, error) { return "/Users/example", nil },
		getenv:       func(key string) string { return env[key] },
	}
}

func TestPiInstallUsesDirectProviderLiteralsWithoutExposingAPIKey(t *testing.T) {
	const secret = "private-test-key"
	catalog := &fakePiCatalog{models: []piProviderModel{{
		ID:              "example/model",
		Name:            "Example Model",
		ContextWindow:   128000,
		MaxTokens:       piDefaultMaxTokens,
		Cost:            piProviderCost{Input: 0.1, Output: 0.5},
		Reasoning:       true,
		Input:           []string{"text", "image"},
		CapabilityKnown: true,
	}}}
	store := &fakePiStore{installResult: piInstallResult{
		ModelsPath: "/tmp/pi/models.json",
		ModelCount: 1,
		Changed:    true,
	}}
	deps := piTestDependencies(catalog, store, map[string]string{
		"BASETEN_API_KEY":     secret,
		"PI_CODING_AGENT_DIR": "~/custom-pi",
	})
	var stdout, stderr bytes.Buffer

	if code := runPi(context.Background(), []string{"install"}, deps, &stdout, &stderr); code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, stderr.String())
	}
	if catalog.calls != 1 {
		t.Fatalf("catalog calls = %d, want 1", catalog.calls)
	}
	if len(store.installRequests) != 1 {
		t.Fatalf("install requests = %d, want 1", len(store.installRequests))
	}
	request := store.installRequests[0]
	if request.AgentDir != "/Users/example/custom-pi" {
		t.Errorf("agent dir = %q", request.AgentDir)
	}
	wantHeaders := map[string]string{"Authorization": piAuthHeaderValue}
	if request.Provider.ID != piProviderID ||
		request.Provider.Name != piProviderName ||
		request.Provider.BaseURL != piProviderBaseURL ||
		request.Provider.API != piProviderAPI ||
		request.Provider.APIKey != piAPIKeyReference ||
		!reflect.DeepEqual(request.Provider.Headers, wantHeaders) {
		t.Fatalf("provider = %#v", request.Provider)
	}
	if !reflect.DeepEqual(request.Provider.Models, catalog.models) {
		t.Fatalf("models = %#v, want %#v", request.Provider.Models, catalog.models)
	}
	allOutput := stdout.String() + stderr.String()
	if strings.Contains(allOutput, secret) {
		t.Fatal("command output exposed BASETEN_API_KEY")
	}
	if !strings.Contains(stdout.String(), "Baseten provider installed") ||
		!strings.Contains(stdout.String(), "--provider baseten") {
		t.Fatalf("install output = %q", stdout.String())
	}
}

func TestPiInstallCapabilityFailureDoesNotMutate(t *testing.T) {
	catalog := &fakePiCatalog{models: []piProviderModel{{
		ID: "example/model", Name: "Example Model",
		ContextWindow: 128000, MaxTokens: piDefaultMaxTokens,
		Cost: piProviderCost{Input: 0.1, Output: 0.5},
	}}}
	capabilities := &fakePiCapabilities{err: errors.New("catalog unavailable")}
	store := &fakePiStore{}
	deps := piTestDependencies(catalog, store, map[string]string{
		"BASETEN_API_KEY": "present",
	})
	deps.capabilities = capabilities
	var stdout, stderr bytes.Buffer

	if code := runPi(context.Background(), []string{"install"}, deps, &stdout, &stderr); code != 1 {
		t.Fatalf("install exit = %d, want 1", code)
	}
	if capabilities.calls != 1 || len(store.installRequests) != 0 {
		t.Fatalf("capability failure reached store: calls=%d store=%d",
			capabilities.calls, len(store.installRequests))
	}
	if !strings.Contains(stderr.String(), "load public model capabilities") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPiInstallWarnsWhenExactCapabilitiesAreMissing(t *testing.T) {
	model := piProviderModel{
		ID: "example/model", Name: "Example Model",
		ContextWindow: 128000, MaxTokens: piDefaultMaxTokens,
		Cost:  piProviderCost{Input: 0.1, Output: 0.5},
		Input: []string{"text"},
	}
	catalog := &fakePiCatalog{models: []piProviderModel{model}}
	capabilities := &fakePiCapabilities{result: piCapabilityEnrichment{
		Models: []piProviderModel{model},
	}}
	store := &fakePiStore{installResult: piInstallResult{Changed: true}}
	deps := piTestDependencies(catalog, store, map[string]string{
		"BASETEN_API_KEY": "present",
	})
	deps.capabilities = capabilities
	var stdout, stderr bytes.Buffer

	if code := runPi(context.Background(), []string{"install"}, deps, &stdout, &stderr); code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unavailable for 1 of 1 models") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPiInstallRequiresAPIKeyBeforeCatalogOrMutation(t *testing.T) {
	catalog := &fakePiCatalog{}
	store := &fakePiStore{}
	deps := piTestDependencies(catalog, store, nil)
	var stdout, stderr bytes.Buffer

	if code := runPi(context.Background(), []string{"install"}, deps, &stdout, &stderr); code != 1 {
		t.Fatalf("install exit = %d, want 1", code)
	}
	if catalog.calls != 0 || len(store.installRequests) != 0 {
		t.Fatalf("missing key reached catalog/store: catalog=%d store=%d",
			catalog.calls, len(store.installRequests))
	}
	if !strings.Contains(stderr.String(), "BASETEN_API_KEY must be set") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPiInstallCatalogFailureDoesNotMutate(t *testing.T) {
	catalog := &fakePiCatalog{err: errors.New("catalog unavailable")}
	store := &fakePiStore{}
	deps := piTestDependencies(catalog, store, map[string]string{
		"BASETEN_API_KEY": "present",
	})
	var stdout, stderr bytes.Buffer

	if code := runPi(context.Background(), []string{"install"}, deps, &stdout, &stderr); code != 1 {
		t.Fatalf("install exit = %d, want 1", code)
	}
	if len(store.installRequests) != 0 {
		t.Fatal("catalog failure mutated Pi configuration")
	}
}

func TestPiStatusAndUninstallDoNotRequireAPIKeyOrCatalog(t *testing.T) {
	t.Run("installed status", func(t *testing.T) {
		catalog := &fakePiCatalog{}
		store := &fakePiStore{statusResult: piStatusResult{
			Installed:  true,
			Healthy:    true,
			ModelCount: 2,
		}}
		deps := piTestDependencies(catalog, store, nil)
		var stdout, stderr bytes.Buffer
		if code := runPi(context.Background(), []string{"status"}, deps, &stdout, &stderr); code != 0 {
			t.Fatalf("status exit = %d, stderr = %s", code, stderr.String())
		}
		if catalog.calls != 0 || len(store.statusDirs) != 1 {
			t.Fatalf("unexpected calls: catalog=%d status=%d", catalog.calls, len(store.statusDirs))
		}
		if !strings.Contains(stdout.String(), "(2 models)") {
			t.Fatalf("status output = %q", stdout.String())
		}
	})

	t.Run("not installed status", func(t *testing.T) {
		catalog := &fakePiCatalog{}
		store := &fakePiStore{}
		deps := piTestDependencies(catalog, store, nil)
		var stdout, stderr bytes.Buffer
		if code := runPi(context.Background(), []string{"status"}, deps, &stdout, &stderr); code != piStatusExitNotInstalled {
			t.Fatalf("status exit = %d, want %d", code, piStatusExitNotInstalled)
		}
	})

	t.Run("uninstall", func(t *testing.T) {
		catalog := &fakePiCatalog{}
		store := &fakePiStore{uninstallResult: piUninstallResult{Changed: true}}
		deps := piTestDependencies(catalog, store, nil)
		var stdout, stderr bytes.Buffer
		if code := runPi(context.Background(), []string{"uninstall"}, deps, &stdout, &stderr); code != 0 {
			t.Fatalf("uninstall exit = %d, stderr = %s", code, stderr.String())
		}
		if catalog.calls != 0 || len(store.uninstallDirs) != 1 {
			t.Fatalf("unexpected calls: catalog=%d uninstall=%d", catalog.calls, len(store.uninstallDirs))
		}
	})
}

func TestPiRejectsOnAndUnknownSubcommandsWithoutConsultingDependencies(t *testing.T) {
	for _, subcommand := range []string{"on", "start", "off"} {
		t.Run(subcommand, func(t *testing.T) {
			catalog := &fakePiCatalog{}
			store := &fakePiStore{}
			deps := piAdapterDependencies{
				catalog: catalog,
				store:   store,
				homeDir: func() (string, error) {
					t.Fatal("invalid subcommand resolved the home directory")
					return "", nil
				},
				getenv: func(string) string {
					t.Fatal("invalid subcommand read the environment")
					return ""
				},
			}
			var stdout, stderr bytes.Buffer
			if code := runPi(context.Background(), []string{subcommand}, deps, &stdout, &stderr); code != 2 {
				t.Fatalf("%s exit = %d, want 2", subcommand, code)
			}
			if catalog.calls != 0 ||
				len(store.installRequests) != 0 ||
				len(store.statusDirs) != 0 ||
				len(store.uninstallDirs) != 0 {
				t.Fatal("invalid subcommand consulted a Pi dependency")
			}
			if subcommand == "on" && !strings.Contains(stderr.String(), "does not start the gateway") {
				t.Fatalf("on stderr = %q", stderr.String())
			}
		})
	}
}

func TestPiRejectsMissingOrExtraSubcommands(t *testing.T) {
	deps := piAdapterDependencies{}
	for _, args := range [][]string{nil, {"install", "extra"}} {
		var stdout, stderr bytes.Buffer
		if code := runPi(context.Background(), args, deps, &stdout, &stderr); code != 2 {
			t.Fatalf("runPi(%v) exit = %d, want 2", args, code)
		}
	}
}

func TestResolvePiAgentDir(t *testing.T) {
	home := func() (string, error) { return "/Users/example", nil }
	tests := []struct {
		configured string
		want       string
		wantErr    bool
	}{
		{"", "/Users/example/.pi/agent", false},
		{"~", "/Users/example", false},
		{"~/agent-dir", "/Users/example/agent-dir", false},
		{"/tmp/pi", "/tmp/pi", false},
		{"~someone/pi", "", true},
	}
	for _, test := range tests {
		t.Run(test.configured, func(t *testing.T) {
			got, err := resolvePiAgentDir(test.configured, home)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Errorf("path = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBasetenCLICatalogSourceUsesMachineReadableListCommand(t *testing.T) {
	var gotName string
	var gotArgs []string
	source := basetenCLICatalogSource{
		find:    func() (string, error) { return "/usr/local/bin/baseten", nil },
		version: func(string) string { return "baseten 0.3.0" },
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			gotName = name
			gotArgs = append([]string(nil), args...)
			return []byte(`{"items":[{"name":"example/model","display_name":"Example","context_length":128000,"cost_per_million_input_tokens":"0.10","cost_per_million_output_tokens":0.5}]}`), nil
		},
	}
	models, err := source.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "/usr/local/bin/baseten" ||
		!reflect.DeepEqual(gotArgs, []string{"model-api", "list", "--output", "json"}) {
		t.Fatalf("command = %s %v", gotName, gotArgs)
	}
	if len(models) != 1 ||
		models[0].ID != "example/model" ||
		models[0].Cost.Input != 0.1 ||
		models[0].Cost.Output != 0.5 {
		t.Fatalf("models = %#v", models)
	}
}

func TestParsePiModelCatalogValidatesAndSortsModels(t *testing.T) {
	raw := []byte(`{"items":[
		{"name":"z/model","display_name":"","context_length":64000,"cost_per_million_input_tokens":0,"cost_per_million_output_tokens":"1.25"},
		{"name":"a/model","display_name":"A Model","context_length":128000,"cost_per_million_input_tokens":0.2,"cost_per_million_output_tokens":0.8}
	]}`)
	models, err := parsePiModelCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{models[0].ID, models[1].ID}; !reflect.DeepEqual(got, []string{"a/model", "z/model"}) {
		t.Fatalf("order = %v", got)
	}
	if models[1].Name != "z/model" ||
		models[1].MaxTokens != piDefaultMaxTokens ||
		models[1].Cost.Output != 1.25 {
		t.Fatalf("fallback model = %#v", models[1])
	}

	for name, body := range map[string]string{
		"malformed": `{"items":`,
		"empty":     `{"items":[]}`,
		"blank id":  `{"items":[{"name":""}]}`,
		"duplicate": `{"items":[{"name":"same"},{"name":"same"}]}`,
		"bad cost":  `{"items":[{"name":"model","cost_per_million_input_tokens":{}}]}`,
		"missing cost": `{"items":[
			{"name":"model","context_length":1,"cost_per_million_input_tokens":null,"cost_per_million_output_tokens":1}
		]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePiModelCatalog([]byte(body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPiUsesDefaultModelsPathWhenBackendOmitsIt(t *testing.T) {
	catalog := &fakePiCatalog{}
	store := &fakePiStore{uninstallResult: piUninstallResult{Changed: false}}
	deps := piTestDependencies(catalog, store, nil)
	var stdout, stderr bytes.Buffer

	if code := runPi(context.Background(), []string{"uninstall"}, deps, &stdout, &stderr); code != 0 {
		t.Fatalf("uninstall exit = %d, stderr = %s", code, stderr.String())
	}
	want := filepath.Join("/Users/example", ".pi", "agent", "models.json")
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("output = %q, want path %q", stdout.String(), want)
	}
}
