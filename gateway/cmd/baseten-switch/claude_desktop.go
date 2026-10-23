package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

const mutationSurfaceClaudeDesktop = "claude-desktop"
const claudeDesktopDefaultAddr = "127.0.0.1:45274"

var desktopModelSlug = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

func cmdClaudeDesktop(args []string) int {
	return runClaudeDesktop(args, os.Stdout, os.Stderr)
}

func parseDesktopOptions(args []string) (mutationOptions, []string, error) {
	var alias string
	dryRun := false
	var clean, positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--alias":
			if i+1 >= len(args) || args[i+1] == "" {
				return mutationOptions{}, nil, fmt.Errorf("--alias requires a value")
			}
			i++
			alias = args[i]
		case "--dry-run":
			dryRun = true
		case "--before":
			positional = append(positional, args[i])
		default:
			if isMutationOption(args[i]) {
				width, err := mutationOptionWidth(args, i)
				if err != nil {
					return mutationOptions{}, nil, err
				}
				clean = append(clean, args[i:i+width]...)
				i += width - 1
			} else if strings.HasPrefix(args[i], "-") {
				return mutationOptions{}, nil, fmt.Errorf("unknown option %q", args[i])
			} else {
				positional = append(positional, args[i])
			}
		}
	}
	opts, _, err := parseMutationOptions(clean)
	opts.PickerAlias, opts.DryRun = alias, dryRun
	return opts, positional, err
}

func desktopMutationSpec(args []string, opts mutationOptions) (journaledMutationSpec, error) {
	spec := journaledMutationSpec{Surface: mutationSurfaceClaudeDesktop, Client: config.ClaudeDesktopClientName, Key: "model_picker"}
	if len(args) == 1 && args[0] == "setup" && opts.PickerAlias == "" && !opts.DryRun {
		spec.Operation, spec.Key = "setup_claude_desktop", "connection"
		return spec, nil
	}
	if len(args) < 3 || args[0] != "models" {
		return spec, fmt.Errorf("use claude-desktop setup|status|models add <slug>|remove <alias>|move <alias> --before <alias>")
	}
	spec.RequestedTarget = args[2]
	switch args[1] {
	case "add":
		if len(args) != 3 || !strings.Contains(args[2], "/") || !desktopModelSlug.MatchString(args[2]) {
			return spec, fmt.Errorf("models add requires one Baseten model slug")
		}
		spec.Operation = "add_claude_desktop_model"
		if opts.PickerAlias != "" {
			spec.RequestedTarget += " via " + opts.PickerAlias
		}
	case "remove":
		if len(args) != 3 || opts.PickerAlias != "" || opts.DryRun {
			return spec, fmt.Errorf("models remove requires one alias")
		}
		spec.Operation = "remove_claude_desktop_model"
	case "move":
		if len(args) != 5 || args[3] != "--before" || opts.PickerAlias != "" || opts.DryRun {
			return spec, fmt.Errorf("models move requires <alias> --before <alias>")
		}
		spec.Operation = "move_claude_desktop_model"
		spec.RequestedTarget += " before " + args[4]
	default:
		return spec, fmt.Errorf("unknown Claude Desktop model action")
	}
	return spec, nil
}

func runClaudeDesktop(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		args = []string{"status"}
	}
	opts, positional, err := parseDesktopOptions(args)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	path, _ := resolveConfigPath()
	if len(positional) == 1 && positional[0] == "status" && opts.PickerAlias == "" && !opts.DryRun {
		status, err := readClaudeDesktopStatus(path)
		if err != nil {
			fmt.Fprintln(errOut, "claude-desktop status:", err)
			return 1
		}
		_ = json.NewEncoder(out).Encode(status)
		return 0
	}
	spec, err := desktopMutationSpec(positional, opts)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	bind := envDefault("BASETEN_SWITCH_CLAUDE_DESKTOP_ADDR", claudeDesktopDefaultAddr)
	if !opts.DryRun {
		if replayed, rc := preflightTerminalReplay(path, opts, spec, out); replayed {
			return rc
		}
		lock, err := acquireConfigMutationLock(path)
		if err != nil {
			return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, ""), "mutation_locked", err.Error(), true, 1)
		}
		defer lock.close()
		if err := recoverInterruptedExactConfigCommit(path); err != nil {
			return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, ""), "commit_recovery_failed", err.Error(), true, 1)
		}
	}
	raw, mode, err := readExactConfig(path)
	if err != nil {
		return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, ""), "config_read_failed", "read the Switch configuration before setting up Claude Desktop", false, 1)
	}
	file, err := config.Load(path)
	if err != nil {
		return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, exactConfigHash(raw)), "config_load_failed", err.Error(), false, 1)
	}
	if spec.Operation == "setup_claude_desktop" {
		spec.Apply = func(previewPath string) error { return config.SetupClaudeDesktop(previewPath, bind) }
		spec.HumanSuccess = "Claude Desktop gateway configuration saved. Review claude-desktop status for setup fields and runtime readiness."
		return runJournaledMutationLocked(path, raw, mode, opts, out, spec)
	}
	client := desktopConfigClient(file)
	if client == nil || config.IsNativeAllowed(*client) || client.ProtocolShape != "anthropic" || !client.Enabled || client.ModelPicker == nil || !client.ModelPicker.Enabled {
		return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, exactConfigHash(raw)), "desktop_setup_required", "run claude-desktop setup before managing models", false, 1)
	}
	picker := *client.ModelPicker
	picker.Models = append([]config.ModelPickerModel(nil), picker.Models...)
	aliasesToAdd := map[string]string{}
	switch positional[1] {
	case "add":
		if opts.DryRun && !opts.JSON {
			fmt.Fprintln(errOut, "models add --dry-run requires --json")
			return 2
		}
		if opts.PickerAlias == "" && len(aliasesForSlug(positional[2], client.ModelAliases)) > 1 {
			choices := []claudePickerPreview{}
			for _, alias := range aliasesForSlug(positional[2], client.ModelAliases) {
				choices = append(choices, pickerPreview(alias, positional[2]))
			}
			_ = json.NewEncoder(out).Encode(claudePickerAliasAmbiguity{OK: false, Slug: positional[2], AliasChoices: choices, Error: mutationError{Code: "ambiguous_alias", Message: "multiple configured aliases route to this slug; select one"}})
			return 1
		}
		preview, err := buildPickerPreview(positional[2], client.ModelAliases, opts.PickerAlias)
		if err != nil {
			return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, exactConfigHash(raw)), "invalid_model", err.Error(), false, 1)
		}
		if desktopPickerIndex(picker.Models, preview.Alias) < 0 {
			picker.Models = append(picker.Models, config.ModelPickerModel{Alias: preview.Alias})
		}
		proposedAliases := copyPickerAliases(client.ModelAliases)
		proposedAliases[preview.Alias] = preview.Slug
		if err := preflightClaudePickerContext(&picker, proposedAliases); err != nil {
			code := "config_validation_failed"
			if isPickerContextMinimum(err) {
				code = claudePickerContextMinimumErrorCode
			}
			return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, exactConfigHash(raw)), code, err.Error(), false, 1)
		}
		if opts.DryRun {
			_ = json.NewEncoder(out).Encode(preview)
			return 0
		}
		if _, exists := client.ModelAliases[preview.Alias]; !exists {
			aliasesToAdd[preview.Alias] = preview.Slug
		}
	case "remove":
		index := desktopPickerIndex(picker.Models, positional[2])
		if index < 0 {
			return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, exactConfigHash(raw)), "unknown_model", "alias is not in the configured Desktop model list", false, 1)
		}
		picker.Models = append(picker.Models[:index], picker.Models[index+1:]...)
	case "move":
		from, before := desktopPickerIndex(picker.Models, positional[2]), desktopPickerIndex(picker.Models, positional[4])
		if from < 0 || before < 0 {
			return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, exactConfigHash(raw)), "unknown_model", "both aliases must be configured for Claude Desktop", false, 1)
		}
		if from != before {
			row := picker.Models[from]
			picker.Models = append(picker.Models[:from], picker.Models[from+1:]...)
			before = desktopPickerIndex(picker.Models, positional[4])
			picker.Models = append(picker.Models, config.ModelPickerModel{})
			copy(picker.Models[before+1:], picker.Models[before:])
			picker.Models[before] = row
		}
	}
	spec.Apply = func(previewPath string) error {
		if len(aliasesToAdd) > 0 {
			if err := config.SetClientModelAliases(previewPath, config.ClaudeDesktopClientName, aliasesToAdd); err != nil {
				return err
			}
		}
		return config.SetClientModelPicker(previewPath, config.ClaudeDesktopClientName, &picker)
	}
	spec.HumanSuccess = "Claude Desktop models saved. Restart Claude Desktop to refresh its picker."
	return runJournaledMutationLocked(path, raw, mode, opts, out, spec)
}

func desktopPickerIndex(models []config.ModelPickerModel, alias string) int {
	for i, model := range models {
		if model.Alias == alias {
			return i
		}
	}
	return -1
}

func desktopConfigClient(file *config.File) *config.Client {
	for i := range file.Clients {
		if file.Clients[i].Name == config.ClaudeDesktopClientName {
			return &file.Clients[i]
		}
	}
	return nil
}

type claudeDesktopStatus struct {
	ModelDiscovery    *desktopModelDiscoveryEvidence `json:"model_discovery,omitempty"`
	ModelListRevision string                         `json:"model_list_revision,omitempty"`
	Client            string                         `json:"client"`
	Configured        bool                           `json:"configured"`
	Enabled           bool                           `json:"enabled"`
	Connection        struct {
		Provider       string `json:"provider"`
		BaseURL        string `json:"base_url"`
		APIKey         string `json:"api_key"`
		AuthScheme     string `json:"auth_scheme"`
		ModelDiscovery bool   `json:"model_discovery"`
		AutoMode       bool   `json:"auto_mode"`
	} `json:"connection"`
	ModelPicker struct {
		Enabled bool                 `json:"enabled"`
		Models  []desktopStatusModel `json:"models"`
	} `json:"model_picker"`
	Runtime struct {
		State                string `json:"state"`
		CurrentlyBound       bool   `json:"currently_bound"`
		DesiredMatchesActive bool   `json:"desired_matches_active"`
	} `json:"runtime"`
	Auth struct {
		State string `json:"state"`
	} `json:"auth"`
	RequestEvidence struct {
		State       string                  `json:"state"`
		Scope       string                  `json:"scope"`
		LastRequest *desktopRequestEvidence `json:"last_request"`
		Coverage    *desktopRequestCoverage `json:"coverage,omitempty"`
	} `json:"request_evidence"`
}

type desktopModelDiscoveryEvidence struct {
	LastServedAt string `json:"last_served_at"`
	ModelCount   int    `json:"model_count"`
	Revision     string `json:"revision,omitempty"`
}

type desktopRequestCoverage struct {
	Complete bool   `json:"complete"`
	Reason   string `json:"reason"`
}

type desktopStatusModel struct {
	claudePickerPreview
	ContextTokens int64 `json:"context_tokens"`
}

type desktopRequestEvidence struct {
	CompletedAt       string `json:"completed_at"`
	RequestedModel    string `json:"requested_model"`
	ServedModel       string `json:"served_model"`
	Status            *int   `json:"status"`
	TerminationReason string `json:"termination_reason"`
}

func readClaudeDesktopStatus(path string) (claudeDesktopStatus, error) {
	var status claudeDesktopStatus
	status.Client = config.ClaudeDesktopClientName
	status.ModelPicker.Models = []desktopStatusModel{}
	status.Connection.Provider, status.Connection.APIKey, status.Connection.AuthScheme = "gateway", "baseten-switch-local", "bearer"
	status.Connection.ModelDiscovery = true
	status.Runtime.State, status.Auth.State = "unavailable", "unknown"
	status.RequestEvidence.State, status.RequestEvidence.Scope = "unavailable", "recent_500_requests"
	bind := envDefault("BASETEN_SWITCH_CLAUDE_DESKTOP_ADDR", claudeDesktopDefaultAddr)
	file, err := config.Load(path)
	if err != nil {
		return status, err
	}
	client := desktopConfigClient(file)
	if client != nil {
		if config.IsNativeAllowed(*client) || client.ProtocolShape != "anthropic" {
			return status, fmt.Errorf("Claude Desktop requires an independent Anthropic-shape listener with allow_native: false")
		}
		status.Configured, status.Enabled, bind = true, client.Enabled, client.BindAddr
		if client.ModelPicker != nil {
			aliases := make([]string, 0, len(client.ModelPicker.Models))
			for _, row := range client.ModelPicker.Models {
				aliases = append(aliases, row.Alias)
			}
			status.ModelListRevision = config.DesktopModelRevision(aliases, client.ModelAliases)
			status.ModelPicker.Enabled = client.ModelPicker.Enabled
			for _, model := range client.ModelPicker.Models {
				status.ModelPicker.Models = append(status.ModelPicker.Models, desktopStatusModel{claudePickerPreview: pickerPreview(model.Alias, client.ModelAliases[model.Alias])})
			}
		}
	}
	host, port, err := net.SplitHostPort(bind)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return status, fmt.Errorf("Claude Desktop listener must use a loopback IP address and valid port")
	}
	status.Connection.BaseURL = "http://" + bind
	admin := envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)
	var live struct {
		ConfigPath     string                         `json:"config_path"`
		ActiveHash     string                         `json:"active_config_hash"`
		DesiredHash    string                         `json:"desired_config_hash"`
		Auth           *doctorRouterAuth              `json:"auth"`
		Clients        []doctorAdminClient            `json:"clients"`
		ModelDiscovery *desktopModelDiscoveryEvidence `json:"claude_desktop_model_discovery"`
	}
	if getJSON(admin, "/v1/admin/status", &live) != nil {
		return status, nil
	}
	if canonicalPath(live.ConfigPath) != canonicalPath(path) {
		status.Runtime.State = "identity_mismatch"
		return status, nil
	}
	raw, _, err := readExactConfig(path)
	if err != nil {
		return status, err
	}
	status.Runtime.DesiredMatchesActive = live.ActiveHash == exactConfigHash(raw) && live.DesiredHash == live.ActiveHash
	status.Runtime.State = "not_configured"
	if client != nil {
		status.Runtime.State = "not_bound"
		for _, active := range live.Clients {
			if active.Name == config.ClaudeDesktopClientName && active.BindAddr == bind {
				status.Runtime.CurrentlyBound = active.CurrentlyBound
			}
		}
		if !status.Runtime.DesiredMatchesActive {
			status.Runtime.State = "pending"
		} else if status.Runtime.CurrentlyBound {
			status.Runtime.State = "ready"
		}
	}
	if live.Auth != nil {
		switch {
		case live.Auth.Health == "refresh_failed":
			status.Auth.State = "refresh_failed"
		case live.Auth.SignedIn || live.Auth.FallbackInUse:
			status.Auth.State = "ready"
		default:
			status.Auth.State = "signed_out"
		}
	}
	if status.Runtime.State == "ready" {
		status.ModelDiscovery = live.ModelDiscovery
	}
	var requests struct {
		Items []struct {
			Client string `json:"client"`
			desktopRequestEvidence
		} `json:"items"`
		Coverage *desktopRequestCoverage `json:"coverage"`
	}
	if getJSON(admin, "/v1/admin/requests?limit=500", &requests) == nil {
		status.RequestEvidence.Coverage = requests.Coverage
		if requests.Coverage != nil && requests.Coverage.Complete {
			status.RequestEvidence.State = "not_observed"
		}
		for _, request := range requests.Items {
			if request.Client == config.ClaudeDesktopClientName {
				evidence := request.desktopRequestEvidence
				status.RequestEvidence.LastRequest = &evidence
				status.RequestEvidence.State = "observed"
				break
			}
		}
	}
	return status, nil
}
