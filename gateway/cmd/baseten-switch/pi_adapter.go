package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	piProviderID             = "baseten"
	piProviderName           = "Baseten"
	piProviderBaseURL        = "https://inference.baseten.co/v1"
	piProviderAPI            = "openai-completions"
	piAPIKeyReference        = "$BASETEN_API_KEY"
	piAuthHeaderValue        = "Api-Key $BASETEN_API_KEY"
	piDefaultMaxTokens       = 16384
	piModelsFilename         = "models.json"
	piStatusExitNotInstalled = 3
)

var errPiConfigBackendUnavailable = errors.New("Pi configuration backend is unavailable")

// piProviderSpec is the package boundary between the CLI and the safe Pi
// configuration writer. It contains configuration literals only, never the
// value of BASETEN_API_KEY.
type piProviderSpec struct {
	ID      string
	Name    string
	BaseURL string
	API     string
	APIKey  string
	Headers map[string]string
	Models  []piProviderModel
}

type piProviderModel struct {
	ID              string
	Name            string
	ContextWindow   int
	MaxTokens       int
	Cost            piProviderCost
	Reasoning       bool
	Input           []string
	CapabilityKnown bool
}

type piProviderCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

type piInstallRequest struct {
	AgentDir string
	Provider piProviderSpec
}

type piInstallResult struct {
	ModelsPath string
	ModelCount int
	Changed    bool
}

type piStatusResult struct {
	ModelsPath string
	ModelCount int
	Installed  bool
	Healthy    bool
	Detail     string
}

type piUninstallResult struct {
	ModelsPath string
	Changed    bool
}

// piConfigStore keeps the CLI independent of backup, drift, and atomic-write
// implementation details.
type piConfigStore interface {
	Install(context.Context, piInstallRequest) (piInstallResult, error)
	Status(context.Context, string) (piStatusResult, error)
	Uninstall(context.Context, string) (piUninstallResult, error)
}

// unavailablePiConfigStore keeps configuration mutation fail-closed if the
// production file store is not registered during package initialization.
type unavailablePiConfigStore struct{}

func (unavailablePiConfigStore) Install(context.Context, piInstallRequest) (piInstallResult, error) {
	return piInstallResult{}, errPiConfigBackendUnavailable
}

func (unavailablePiConfigStore) Status(context.Context, string) (piStatusResult, error) {
	return piStatusResult{}, errPiConfigBackendUnavailable
}

func (unavailablePiConfigStore) Uninstall(context.Context, string) (piUninstallResult, error) {
	return piUninstallResult{}, errPiConfigBackendUnavailable
}

type piCatalogSource interface {
	Models(context.Context) ([]piProviderModel, error)
}

type piCapabilityEnrichment struct {
	Models  []piProviderModel
	Matched int
}

type piCapabilitySource interface {
	Enrich(context.Context, []piProviderModel) (piCapabilityEnrichment, error)
}

type basetenCLICatalogSource struct {
	find    func() (string, error)
	version func(string) string
	run     func(context.Context, string, ...string) ([]byte, error)
	timeout time.Duration
}

func (s basetenCLICatalogSource) Models(ctx context.Context) ([]piProviderModel, error) {
	bin, err := s.find()
	if err != nil {
		return nil, fmt.Errorf("find Baseten CLI: %w", err)
	}
	if !filepath.IsAbs(bin) {
		return nil, fmt.Errorf("Baseten CLI path is not absolute: %s", bin)
	}
	versionOutput := s.version(bin)
	version, err := parseSemanticVersion(versionOutput)
	if err != nil {
		return nil, fmt.Errorf("determine Baseten CLI version: %w", err)
	}
	minimum, _ := parseSemanticVersion(minimumBasetenCLIVersion)
	if version.less(minimum) {
		return nil, fmt.Errorf(
			"Baseten CLI v%s is too old; v%s or newer is required",
			version, minimum,
		)
	}
	timeout := s.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := s.run(runCtx, bin, "model-api", "list", "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("run 'baseten model-api list --output json': %w", err)
	}
	return parsePiModelCatalog(raw)
}

func runPiCatalogCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

type piAdapterDependencies struct {
	catalog      piCatalogSource
	capabilities piCapabilitySource
	store        piConfigStore
	homeDir      func() (string, error)
	getenv       func(string) string
}

var newPiConfigStore = func() piConfigStore {
	return unavailablePiConfigStore{}
}

var piDependencies = func() piAdapterDependencies {
	return piAdapterDependencies{
		catalog: basetenCLICatalogSource{
			find:    lookBaseten,
			version: basetenCLIVersion,
			run:     runPiCatalogCommand,
			timeout: 30 * time.Second,
		},
		capabilities: newModelsDevPiCapabilitySource(),
		store:        newPiConfigStore(),
		homeDir:      os.UserHomeDir,
		getenv:       os.Getenv,
	}
}

func cmdPi(args []string) int {
	return runPi(context.Background(), args, piDependencies(), os.Stdout, os.Stderr)
}

func runPi(
	ctx context.Context,
	args []string,
	deps piAdapterDependencies,
	out io.Writer,
	errOut io.Writer,
) int {
	if len(args) != 1 {
		fmt.Fprintln(errOut, "usage: baseten-switch pi install|status|uninstall")
		return 2
	}
	switch args[0] {
	case "install", "status", "uninstall":
	case "on":
		fmt.Fprintln(errOut, "unknown pi subcommand: on (use install|status|uninstall; Pi direct mode does not start the gateway)")
		return 2
	default:
		fmt.Fprintf(errOut, "unknown pi subcommand: %s (use install|status|uninstall)\n", args[0])
		return 2
	}

	agentDir, err := resolvePiAgentDir(deps.getenv("PI_CODING_AGENT_DIR"), deps.homeDir)
	if err != nil {
		fmt.Fprintf(errOut, "pi: resolve agent directory: %v\n", err)
		return 1
	}

	switch args[0] {
	case "install":
		if strings.TrimSpace(deps.getenv("BASETEN_API_KEY")) == "" {
			fmt.Fprintln(errOut, "pi install: BASETEN_API_KEY must be set in the environment")
			return 1
		}
		models, err := deps.catalog.Models(ctx)
		if err != nil {
			fmt.Fprintf(errOut, "pi install: load Baseten model catalog: %v\n", err)
			return 1
		}
		enrichment, err := deps.capabilities.Enrich(ctx, models)
		if err != nil {
			fmt.Fprintf(errOut, "pi install: load public model capabilities: %v\n", err)
			return 1
		}
		models = enrichment.Models
		result, err := deps.store.Install(ctx, piInstallRequest{
			AgentDir: agentDir,
			Provider: piProviderSpec{
				ID:      piProviderID,
				Name:    piProviderName,
				BaseURL: piProviderBaseURL,
				API:     piProviderAPI,
				APIKey:  piAPIKeyReference,
				Headers: map[string]string{
					"Authorization": piAuthHeaderValue,
				},
				Models: models,
			},
		})
		if err != nil {
			fmt.Fprintf(errOut, "pi install: %v\n", err)
			return 1
		}
		if result.ModelsPath == "" {
			result.ModelsPath = filepath.Join(agentDir, piModelsFilename)
		}
		if result.ModelCount == 0 {
			result.ModelCount = len(models)
		}
		action := "installed"
		if !result.Changed {
			action = "already installed"
		}
		fmt.Fprintf(out, "Baseten provider %s in %s (%d models)\n",
			action, result.ModelsPath, result.ModelCount)
		if enrichment.Matched != len(models) {
			fmt.Fprintf(
				errOut,
				"pi install: warning: exact capability metadata is unavailable for %d of %d models; Pi will show conservative defaults for those models\n",
				len(models)-enrichment.Matched,
				len(models),
			)
		}
		fmt.Fprintln(out, "Select it in Pi with /model or start Pi with --provider baseten.")
		return 0

	case "status":
		result, err := deps.store.Status(ctx, agentDir)
		if err != nil {
			fmt.Fprintf(errOut, "pi status: %v\n", err)
			return 1
		}
		if result.ModelsPath == "" {
			result.ModelsPath = filepath.Join(agentDir, piModelsFilename)
		}
		if !result.Installed {
			fmt.Fprintf(out, "Baseten provider is not installed in %s\n", result.ModelsPath)
			return piStatusExitNotInstalled
		}
		if !result.Healthy {
			detail := strings.TrimSpace(result.Detail)
			if detail == "" {
				detail = "managed provider configuration is unhealthy"
			}
			fmt.Fprintf(errOut, "pi status: %s\n", detail)
			return 1
		}
		fmt.Fprintf(out, "Baseten provider is installed in %s (%d models)\n",
			result.ModelsPath, result.ModelCount)
		return 0

	case "uninstall":
		result, err := deps.store.Uninstall(ctx, agentDir)
		if err != nil {
			fmt.Fprintf(errOut, "pi uninstall: %v\n", err)
			return 1
		}
		if result.ModelsPath == "" {
			result.ModelsPath = filepath.Join(agentDir, piModelsFilename)
		}
		if result.Changed {
			fmt.Fprintf(out, "Removed the Baseten provider from %s\n", result.ModelsPath)
		} else {
			fmt.Fprintf(out, "Baseten provider is not installed in %s; nothing to do\n", result.ModelsPath)
		}
		return 0

	}
	panic("unreachable")
}

func resolvePiAgentDir(configured string, homeDir func() (string, error)) (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	if configured == "" {
		return filepath.Join(home, ".pi", "agent"), nil
	}
	switch {
	case configured == "~":
		return home, nil
	case strings.HasPrefix(configured, "~/") || strings.HasPrefix(configured, `~\`):
		return filepath.Join(home, configured[2:]), nil
	case strings.HasPrefix(configured, "~"):
		return "", fmt.Errorf("user-specific home expansion is not supported: %s", configured)
	default:
		return filepath.Clean(configured), nil
	}
}

func parsePiModelCatalog(raw []byte) ([]piProviderModel, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var response struct {
		Items []struct {
			Name                       string      `json:"name"`
			DisplayName                string      `json:"display_name"`
			ContextLength              int         `json:"context_length"`
			CostPerMillionInputTokens  interface{} `json:"cost_per_million_input_tokens"`
			CostPerMillionOutputTokens interface{} `json:"cost_per_million_output_tokens"`
		} `json:"items"`
	}
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("decode Baseten model catalog: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode Baseten model catalog: multiple JSON values")
		}
		return nil, fmt.Errorf("decode Baseten model catalog: %w", err)
	}
	if len(response.Items) == 0 {
		return nil, errors.New("Baseten model catalog contains no models")
	}
	models := make([]piProviderModel, 0, len(response.Items))
	seen := make(map[string]struct{}, len(response.Items))
	for _, item := range response.Items {
		id := strings.TrimSpace(item.Name)
		if id == "" {
			return nil, errors.New("Baseten model catalog contains an empty model name")
		}
		if item.ContextLength <= 0 {
			return nil, fmt.Errorf("Baseten model %q has invalid context length %d", id, item.ContextLength)
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("Baseten model catalog contains duplicate model %q", id)
		}
		seen[id] = struct{}{}
		input, err := piCatalogCost(item.CostPerMillionInputTokens)
		if err != nil {
			return nil, fmt.Errorf("Baseten model %q input cost: %w", id, err)
		}
		output, err := piCatalogCost(item.CostPerMillionOutputTokens)
		if err != nil {
			return nil, fmt.Errorf("Baseten model %q output cost: %w", id, err)
		}
		name := strings.TrimSpace(item.DisplayName)
		if name == "" {
			name = id
		}
		maxTokens := piDefaultMaxTokens
		if item.ContextLength < maxTokens {
			maxTokens = item.ContextLength
		}
		models = append(models, piProviderModel{
			ID:            id,
			Name:          name,
			ContextWindow: item.ContextLength,
			MaxTokens:     maxTokens,
			Cost: piProviderCost{
				Input:  input,
				Output: output,
			},
		})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models, nil
}

func piCatalogCost(value interface{}) (float64, error) {
	switch value := value.(type) {
	case nil:
		return 0, errors.New("cost is missing")
	case json.Number:
		cost, err := value.Float64()
		if err != nil {
			return 0, err
		}
		if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
			return 0, fmt.Errorf("invalid cost %q", value)
		}
		return cost, nil
	case string:
		cost, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid decimal %q", value)
		}
		if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
			return 0, fmt.Errorf("invalid cost %q", value)
		}
		return cost, nil
	default:
		return 0, fmt.Errorf("unexpected JSON type %T", value)
	}
}
