package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

const reasoningUsage = "usage: baseten-switch <claude|codex> reasoning baseten <model> off|follow-harness|effort <value>|default"

type reasoningCommandOptions struct {
	Mutation mutationOptions
}

type reasoningPreflightResult struct {
	Warning string
}

type reasoningPreflightClient interface {
	Check(adminAddr, clientName, provider, modelID string, policy config.ReasoningPolicy) (reasoningPreflightResult, error)
}

var activeReasoningPreflightClient reasoningPreflightClient = httpReasoningPreflightClient{}

func runClientReasoning(
	clientName string,
	args []string,
	out io.Writer,
) int {
	opts, positional, err := parseReasoningCommandOptions(args)
	result := mutationResult{Operation: "set_model_reasoning"}
	if err != nil {
		return failMutation(opts.Mutation, out, result, "usage",
			fmt.Sprintf("%s: %v", reasoningUsage, err), false, 2)
	}
	provider, modelID, policy, useDefault, err := parseReasoningPolicy(positional)
	result.Client = clientName
	result.Key = modelID
	if err != nil {
		return failMutation(opts.Mutation, out, result, "usage",
			fmt.Sprintf("%s: %v", reasoningUsage, err), false, 2)
	}
	result.RequestedTarget = "default"
	if !useDefault {
		result.RequestedTarget = string(policy.Mode)
		if policy.Effort != "" {
			result.RequestedTarget = "effort:" + policy.Effort
		}
	}

	path, notices := resolveConfigPath()
	result.ConfigPath = path
	if !opts.Mutation.JSON {
		for _, notice := range notices {
			fmt.Fprintln(os.Stderr, notice)
		}
	}
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		return failMutation(opts.Mutation, out, result, "mutation_locked", err.Error(), true, 1)
	}
	defer lock.close()
	if err := recoverInterruptedExactConfigCommit(path); err != nil {
		return failMutation(opts.Mutation, out, result, "commit_recovery_failed", err.Error(), true, 1)
	}
	prior, mode, err := readExactConfig(path)
	if err != nil {
		return failMutation(opts.Mutation, out, result, "config_read_failed", err.Error(), true, 1)
	}
	file, err := config.Load(path)
	if err != nil {
		return failMutation(opts.Mutation, out, result, "config_load_failed", err.Error(), false, 1)
	}
	if err := config.ValidateRoutingPolicy(file); err != nil {
		return failMutation(opts.Mutation, out, result, "invalid_routing_policy", err.Error(), false, 1)
	}

	if !useDefault {
		if state, _ := classifyPidfile(gatewayPidfilePath()); state != pidfileAlive {
			return failMutation(opts.Mutation, out, result, "router_unavailable",
				"reasoning mutations other than default require a healthy running router", true, 1)
		}
		preflight, err := activeReasoningPreflightClient.Check(
			envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr),
			clientName,
			provider,
			modelID,
			policy,
		)
		if err != nil {
			return failMutation(opts.Mutation, out, result, "reasoning_preflight_failed", err.Error(), true, 1)
		}
		if !opts.Mutation.JSON && preflight.Warning != "" {
			fmt.Fprintln(os.Stderr, "warning: "+preflight.Warning)
		}
	}

	spec := journaledMutationSpec{
		Operation:       "set_model_reasoning",
		RequestedTarget: result.RequestedTarget,
		Client:          clientName,
		Key:             modelID,
		HumanSuccess: fmt.Sprintf(
			"%s reasoning: %s/%s -> %s (in %s)",
			clientName,
			provider,
			modelID,
			result.RequestedTarget,
			path,
		),
	}
	if useDefault {
		spec.Apply = func(editPath string) error {
			return config.RemoveClientModelReasoningPolicy(
				editPath,
				clientName,
				provider,
				modelID,
			)
		}
	} else {
		spec.Apply = func(editPath string) error {
			return config.SetClientModelReasoningPolicy(
				editPath,
				clientName,
				provider,
				modelID,
				policy,
			)
		}
	}
	return runJournaledMutationLocked(path, prior, mode, opts.Mutation, out, spec)
}

func parseReasoningCommandOptions(args []string) (reasoningCommandOptions, []string, error) {
	var out reasoningCommandOptions
	mutation, positional, err := parseMutationOptions(args)
	out.Mutation = mutation
	return out, positional, err
}

func parseReasoningPolicy(args []string) (string, string, config.ReasoningPolicy, bool, error) {
	if len(args) < 3 {
		return "", "", config.ReasoningPolicy{}, false, fmt.Errorf("provider, model, and policy are required")
	}
	provider, modelID, mode := args[0], args[1], args[2]
	if provider != "baseten" {
		return provider, modelID, config.ReasoningPolicy{}, false,
			fmt.Errorf("provider %q is unsupported (allowed: baseten)", provider)
	}
	if strings.TrimSpace(modelID) == "" {
		return provider, modelID, config.ReasoningPolicy{}, false, fmt.Errorf("model cannot be empty")
	}
	switch mode {
	case "default":
		if len(args) != 3 {
			return provider, modelID, config.ReasoningPolicy{}, false, fmt.Errorf("default accepts no value")
		}
		return provider, modelID, config.ReasoningPolicy{}, true, nil
	case "off":
		if len(args) != 3 {
			return provider, modelID, config.ReasoningPolicy{}, false, fmt.Errorf("off accepts no value")
		}
		return provider, modelID, config.ReasoningPolicy{Mode: config.ReasoningOff}, false, nil
	case "follow-harness":
		if len(args) != 3 {
			return provider, modelID, config.ReasoningPolicy{}, false, fmt.Errorf("follow-harness accepts no value")
		}
		return provider, modelID, config.ReasoningPolicy{Mode: config.ReasoningFollowHarness}, false, nil
	case "effort":
		if len(args) != 4 || strings.TrimSpace(args[3]) == "" {
			return provider, modelID, config.ReasoningPolicy{}, false, fmt.Errorf("effort requires one value")
		}
		return provider, modelID, config.ReasoningPolicy{
			Mode: config.ReasoningFixed, Effort: args[3],
		}, false, nil
	default:
		return provider, modelID, config.ReasoningPolicy{}, false,
			fmt.Errorf("unknown policy %q", mode)
	}
}

type adminReasoningPreflightResponse struct {
	Error   string `json:"error"`
	Warning string `json:"warning"`
}

type httpReasoningPreflightClient struct{}

func (httpReasoningPreflightClient) Check(
	adminAddr string,
	clientName string,
	provider string,
	modelID string,
	policy config.ReasoningPolicy,
) (reasoningPreflightResult, error) {
	client := &http.Client{Timeout: mutationHTTPTimeout}
	var preflight adminReasoningPreflightResponse
	if err := postReasoningAdminJSON(
		client,
		adminAddr,
		"/v1/admin/reasoning/preflight",
		map[string]any{
			"client":   clientName,
			"provider": provider,
			"model":    modelID,
			"policy":   policy,
		},
		&preflight,
	); err != nil {
		return reasoningPreflightResult{}, fmt.Errorf(
			"run client reasoning preflight: %w",
			err,
		)
	}
	if preflight.Error != "" {
		return reasoningPreflightResult{}, errors.New(preflight.Error)
	}
	result := reasoningPreflightResult{
		Warning: preflight.Warning,
	}
	return result, nil
}

func postReasoningAdminJSON(
	client *http.Client,
	adminAddr string,
	path string,
	body any,
	target any,
) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(
		http.MethodPost,
		"http://"+adminAddr+path,
		bytes.NewReader(encoded),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", path, response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	return decoder.Decode(target)
}
