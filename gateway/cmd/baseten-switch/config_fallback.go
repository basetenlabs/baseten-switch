package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

const configFallbackUsage = `usage:
  baseten-switch config fallback status [--json]
  baseten-switch config fallback 429 on|off [mutation options]
  baseten-switch config fallback 5xx on|off [mutation options]
  baseten-switch config fallback model <client> [--json]
  baseten-switch config fallback model <client> <native-model> [mutation options]`

type fallbackAdminClient struct {
	Name                 string                  `json:"name"`
	ModelPicker          *modelPickerAdminStatus `json:"model_picker"`
	BasetenModelFallback struct {
		ConfiguredModel string  `json:"configured_model"`
		ResolvedModel   string  `json:"resolved_model"`
		DisplayName     string  `json:"display_name"`
		ProviderReady   bool    `json:"provider_ready"`
		Ready           bool    `json:"ready"`
		Reason          *string `json:"reason"`
	} `json:"baseten_model_fallback"`
}

type modelPickerAdminStatus struct {
	Enabled bool                          `json:"enabled"`
	Models  []modelPickerAdminStatusModel `json:"models"`
}

type modelPickerAdminStatusModel struct {
	Alias         string `json:"alias"`
	Slug          string `json:"slug"`
	ContextTokens int64  `json:"context_tokens"`
}

type fallbackReadStatus struct {
	ConfigPath       string                         `json:"config_path"`
	Configured       config.ResolvedFallbackPolicy  `json:"configured"`
	Active           *config.ResolvedFallbackPolicy `json:"active,omitempty"`
	DesiredActiveGap bool                           `json:"desired_active_mismatch"`
	Clients          []fallbackReadClient           `json:"clients,omitempty"`
}

type fallbackReadClient struct {
	Name            string `json:"name"`
	ConfiguredModel string `json:"configured_model"`
	ActiveModel     string `json:"active_model,omitempty"`
	DisplayName     string `json:"display_name,omitempty"`
	Ready           bool   `json:"ready"`
	Reason          string `json:"reason,omitempty"`
	activeKnown     bool
}

func cmdConfigFallback(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, configFallbackUsage)
		return 2
	}
	opts, positional, err := parseMutationOptions(args)
	if err != nil {
		return failMutation(opts, os.Stdout, mutationResult{}, "usage", configFallbackUsage+": "+err.Error(), false, 2)
	}
	if len(positional) == 1 && positional[0] == "status" {
		return readFallbackStatus(opts, "", os.Stdout)
	}
	if len(positional) == 2 && positional[0] == "model" {
		return readFallbackStatus(opts, positional[1], os.Stdout)
	}
	if len(positional) == 2 && (positional[0] == "429" || positional[0] == "5xx") &&
		(positional[1] == "on" || positional[1] == "off") {
		return mutateFallbackPolicy(positional[0], positional[1] == "on", opts, os.Stdout)
	}
	if len(positional) == 3 && positional[0] == "model" {
		return mutateNativeFallbackModel(positional[1], positional[2], opts, os.Stdout)
	}
	return failMutation(opts, os.Stdout, mutationResult{}, "usage", configFallbackUsage, false, 2)
}

func readFallbackStatus(opts mutationOptions, onlyClient string, out io.Writer) int {
	if opts.hasOperationID || opts.hasActiveToken || opts.hasConfigHash {
		return failMutation(opts, out, mutationResult{}, "usage", "read-only fallback status accepts only --json", false, 2)
	}
	path, notices := resolveConfigPath()
	if !opts.JSON {
		for _, notice := range notices {
			fmt.Fprintln(os.Stderr, notice)
		}
	}
	file, err := config.Load(path)
	if err != nil {
		message := config.MalformedConfigMessage(path, err)
		if errors.Is(err, os.ErrNotExist) {
			message = config.MissingConfigMessage(path)
		}
		return failMutation(opts, out, mutationResult{ConfigPath: path}, "config_load_failed", message, false, 1)
	}
	status := fallbackReadStatus{ConfigPath: path, Configured: config.ResolveFallbackPolicy(file.Global.FallbackPolicy)}
	admin, adminOK := matchingFallbackAdminStatus(path)
	if adminOK {
		active := admin.FallbackPolicy
		status.Active = &active
		status.DesiredActiveGap = admin.ActiveConfigHash != "" && admin.DesiredConfigHash != "" && admin.ActiveConfigHash != admin.DesiredConfigHash
	}
	for _, client := range file.Clients {
		if onlyClient != "" && client.Name != onlyClient {
			continue
		}
		entry := fallbackReadClient{Name: client.Name, ConfiguredModel: client.NativeFallbackModel}
		if adminOK {
			for _, activeClient := range admin.Clients {
				if activeClient.Name != client.Name {
					continue
				}
				entry.ActiveModel = activeClient.BasetenModelFallback.ResolvedModel
				entry.activeKnown = true
				entry.DisplayName = activeClient.BasetenModelFallback.DisplayName
				entry.Ready = activeClient.BasetenModelFallback.Ready
				if activeClient.BasetenModelFallback.Reason != nil {
					entry.Reason = *activeClient.BasetenModelFallback.Reason
				}
				break
			}
		}
		status.Clients = append(status.Clients, entry)
	}
	if onlyClient != "" && len(status.Clients) == 0 {
		return failMutation(opts, out, mutationResult{ConfigPath: path, Client: onlyClient}, "invalid_client", fmt.Sprintf("no client named %q", onlyClient), false, 2)
	}
	if opts.JSON {
		emitJSON(out, status)
		return 0
	}
	if onlyClient != "" {
		printFallbackModelStatus(out, status.Clients[0], adminOK)
		return 0
	}
	printFallbackPolicyStatus(out, status, adminOK)
	return 0
}

func matchingFallbackAdminStatus(path string) (routingAdminStatus, bool) {
	if state, _ := classifyPidfile(gatewayPidfilePath()); state != pidfileAlive {
		return routingAdminStatus{}, false
	}
	status, err := fetchRoutingAdminStatus(envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr))
	if err != nil || canonicalPath(status.ConfigPath) != canonicalPath(path) || !containsString(status.Capabilities, "fallback_policy") {
		return routingAdminStatus{}, false
	}
	return status, true
}

func printFallbackPolicyStatus(out io.Writer, status fallbackReadStatus, active bool) {
	fmt.Fprintln(out, "Automatic fallback")
	printFallbackPolicyTrigger(out, "429", status.Configured.OnBaseten429, status.Active, func(policy config.ResolvedFallbackPolicy) bool {
		return policy.OnBaseten429
	}, "Retry Baseten rate limits through the native provider")
	printFallbackPolicyTrigger(out, "5xx", status.Configured.OnBaseten5xx, status.Active, func(policy config.ResolvedFallbackPolicy) bool {
		return policy.OnBaseten5xx
	}, "Retry Baseten server errors through the native provider")
	if !active {
		fmt.Fprintln(out, "Active router values unavailable; showing configured values.")
	} else if status.DesiredActiveGap {
		fmt.Fprintln(out, "Configured and active router generations differ.")
	}
	fmt.Fprintln(out, "\nFallback for Baseten-specific models")
	for _, client := range status.Clients {
		if client.ConfiguredModel == "" && (!client.activeKnown || client.ActiveModel == "") {
			continue
		}
		if client.activeKnown && client.ActiveModel != client.ConfiguredModel {
			fmt.Fprintf(out, "%-13s   Baseten-specific models   configured   %-8s   %s\n",
				client.Name, nativeFallbackDisplayName(client.ConfiguredModel), client.ConfiguredModel)
			if client.ActiveModel == "" {
				fmt.Fprintf(out, "%-13s   Baseten-specific models   active       unavailable\n", client.Name)
			} else {
				fmt.Fprintf(out, "%-13s   Baseten-specific models   active       %-8s   %s\n",
					client.Name, nativeFallbackDisplayName(client.ActiveModel), client.ActiveModel)
			}
			continue
		}
		model := client.ConfiguredModel
		if client.activeKnown {
			model = client.ActiveModel
		}
		label := client.DisplayName
		if label == "" {
			label = nativeFallbackDisplayName(model)
		}
		fmt.Fprintf(out, "%-13s   Baseten-specific models   %-8s   %s\n", client.Name, label, model)
	}
	fmt.Fprintln(out, "\nNative-origin requests preserve the exact model requested by the client.")
}

func printFallbackPolicyTrigger(
	out io.Writer,
	trigger string,
	configured bool,
	active *config.ResolvedFallbackPolicy,
	activeValue func(config.ResolvedFallbackPolicy) bool,
	description string,
) {
	if active != nil && activeValue(*active) != configured {
		fmt.Fprintf(out, "%s   configured %-3s   active %-3s   %s\n", trigger, onOff(configured), onOff(activeValue(*active)), description)
		return
	}
	fmt.Fprintf(out, "%s   %-3s   %s\n", trigger, onOff(configured), description)
}

func printFallbackModelStatus(out io.Writer, client fallbackReadClient, active bool) {
	if client.ConfiguredModel == "" && (!active || !client.activeKnown || client.ActiveModel == "") {
		fmt.Fprintf(out, "%s: no fallback model configured for Baseten-specific identities\n", client.Name)
		return
	}
	if active && client.activeKnown && client.ActiveModel != client.ConfiguredModel {
		fmt.Fprintf(out, "%s   configured   %s   %s\n", client.Name, nativeFallbackDisplayName(client.ConfiguredModel), client.ConfiguredModel)
		if client.ActiveModel == "" {
			fmt.Fprintf(out, "%s   active       unavailable\n", client.Name)
		} else {
			fmt.Fprintf(out, "%s   active       %s   %s\n", client.Name, nativeFallbackDisplayName(client.ActiveModel), client.ActiveModel)
		}
		return
	}
	model := client.ConfiguredModel
	if active && client.activeKnown {
		model = client.ActiveModel
	}
	fmt.Fprintf(out, "%s   %s   %s\n", client.Name, nativeFallbackDisplayName(model), model)
}

func mutateFallbackPolicy(trigger string, enabled bool, opts mutationOptions, out io.Writer) int {
	target := onOff(enabled)
	spec := journaledMutationSpec{
		Operation: "set_fallback_policy", Surface: mutationSurfaceConfig,
		Requested: enabled, RequestedTarget: target, Key: trigger,
		Apply:        func(path string) error { return config.SetFallbackPolicyTrigger(path, trigger, enabled) },
		HumanSuccess: "automatic fallback for HTTP " + trigger + " " + target,
	}
	return runConfigFallbackMutation(opts, spec, out)
}

func mutateNativeFallbackModel(clientName, model string, opts mutationOptions, out io.Writer) int {
	spec := journaledMutationSpec{
		Operation: "set_native_fallback_model", Surface: mutationSurfaceConfig,
		Client: clientName, Key: "native_fallback_model", RequestedTarget: model,
		Apply:        func(path string) error { return config.SetClientNativeFallbackModel(path, clientName, model) },
		HumanSuccess: fmt.Sprintf("%s Baseten-model fallback set to %s", clientName, model),
	}
	if strings.TrimSpace(model) == "" {
		return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, "", ""), "invalid_native_fallback_model", "native fallback model cannot be empty", false, 2)
	}
	return runConfigFallbackMutation(opts, spec, out)
}

func runConfigFallbackMutation(opts mutationOptions, spec journaledMutationSpec, out io.Writer) int {
	path, notices := resolveConfigPath()
	if !opts.JSON {
		for _, notice := range notices {
			fmt.Fprintln(os.Stderr, notice)
		}
	}
	if replayed, rc := preflightTerminalReplay(path, opts, spec, out); replayed {
		return rc
	}
	file, err := config.Load(path)
	if err != nil {
		message := config.MalformedConfigMessage(path, err)
		if errors.Is(err, os.ErrNotExist) {
			message = config.MissingConfigMessage(path)
		}
		return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, ""), "config_load_failed", message, false, 1)
	}
	if spec.Operation == "set_native_fallback_model" {
		found := false
		for _, client := range file.Clients {
			if client.Name == spec.Client {
				found = true
				break
			}
		}
		if !found {
			return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, ""), "invalid_client", fmt.Sprintf("no client named %q", spec.Client), false, 2)
		}
	}
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, ""), "mutation_locked", err.Error(), true, 1)
	}
	defer lock.close()
	if err := recoverInterruptedExactConfigCommit(path); err != nil {
		return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, ""), "commit_recovery_failed", err.Error(), true, 1)
	}
	prior, mode, err := readExactConfig(path)
	if err != nil {
		return failMutation(opts, out, mutationResultForSpec(spec, opts.OperationID, path, ""), "config_read_failed", err.Error(), true, 1)
	}
	return runJournaledMutationLocked(path, prior, mode, opts, out, spec)
}

func emitJSON(out io.Writer, value any) {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func nativeFallbackDisplayName(model string) string {
	lower := strings.ToLower(model)
	for _, family := range []struct{ token, label string }{
		{"opus", "Opus"}, {"sonnet", "Sonnet"}, {"haiku", "Haiku"}, {"fable", "Fable"},
	} {
		if strings.Contains(lower, family.token) {
			return family.label
		}
	}
	return model
}
