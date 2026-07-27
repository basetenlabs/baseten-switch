package gateway

import (
	"fmt"
	"sort"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/reasoning"
)

const (
	reasoningUnavailableCatalogUnknown = "catalog_capability_unknown"
	reasoningUnavailableUnsupported    = "reasoning_unsupported"
	reasoningUnavailableEffortRemoved  = "configured_effort_unavailable"
	reasoningUnavailableAdapter        = "protocol_adapter_unsupported"
)

type adminReasoningPolicy struct {
	Mode   string `json:"mode"`
	Effort string `json:"effort,omitempty"`
}

type adminReasoningStatus struct {
	Configured        adminReasoningPolicy `json:"configured"`
	Effective         adminReasoningPolicy `json:"effective"`
	Source            string               `json:"source"`
	AvailableModes    []string             `json:"available_modes"`
	AvailableEfforts  []string             `json:"available_efforts"`
	Available         bool                 `json:"available"`
	UnavailableReason string               `json:"unavailable_reason"`
	Error             string               `json:"error"`
}

type adminModelOptionStatus struct {
	Reasoning *adminReasoningStatus `json:"reasoning,omitempty"`
}

type adminClientModelOptions map[string]map[string]adminModelOptionStatus

// computeClientModelOptions projects client-specific provider/model policy
// through one client's reachable targets and reviewed protocol adapter. Every returned
// key is the exact provider and canonical model identity used at request time.
func computeClientModelOptions(
	rc resolvedClientConfig,
	snapshot *pricing.Snapshot,
) adminClientModelOptions {
	result := adminClientModelOptions{}
	targets := reachableBasetenReasoningTargets(rc)
	baseten := make(map[string]adminModelOptionStatus, len(targets))
	for _, canonicalID := range targets {
		projection := projectClientReasoning(
			rc,
			snapshot,
			pricing.ProviderBaseten,
			canonicalID,
		)
		baseten[canonicalID] = adminModelOptionStatus{
			Reasoning: &projection,
		}
	}
	if len(baseten) > 0 {
		result[pricing.ProviderBaseten] = baseten
	}
	return result
}

func reachableBasetenReasoningTargets(
	rc resolvedClientConfig,
) []string {
	targets := map[string]struct{}{}
	add := func(target string) {
		if canonical, ok := canonicalBasetenTarget(rc, target); ok {
			targets[canonical] = struct{}{}
		}
	}
	add(rc.DefaultModel)
	for _, target := range rc.ModelAliases {
		add(target)
	}
	for _, target := range rc.ModelRoutes {
		add(target)
	}
	add(rc.SubagentModel)

	// Anthropic clients accept an explicit raw Baseten slug. Therefore a
	// client-scoped option for that slug is reachable even if it is not a
	// current tier, alias, family mapping, or subagent target. Preserve it in
	// status so saved and removed catalog values remain visible.
	if rc.ProtocolShape == "" || rc.ProtocolShape == "anthropic" {
		for canonicalID := range rc.ModelOptions[pricing.ProviderBaseten] {
			add(canonicalID)
		}
	}

	ordered := make([]string, 0, len(targets))
	for target := range targets {
		ordered = append(ordered, target)
	}
	sort.Strings(ordered)
	return ordered
}

func canonicalBasetenTarget(
	rc resolvedClientConfig,
	target string,
) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" || target == "native" {
		return "", false
	}
	if alias, ok := rc.ModelAliases[target]; ok {
		target = strings.TrimSpace(alias)
	}
	if !strings.Contains(target, "/") {
		return "", false
	}
	return target, true
}

func projectClientReasoning(
	rc resolvedClientConfig,
	snapshot *pricing.Snapshot,
	provider string,
	canonicalID string,
) adminReasoningStatus {
	return projectClientReasoningPolicy(
		rc,
		snapshot,
		provider,
		canonicalID,
		storedReasoningPolicy(rc, provider, canonicalID),
	)
}

func projectClientReasoningPolicy(
	rc resolvedClientConfig,
	snapshot *pricing.Snapshot,
	provider string,
	canonicalID string,
	stored reasoning.StoredPolicy,
) adminReasoningStatus {
	capability := catalogReasoningInput(
		snapshot,
		provider,
		canonicalID,
	)
	input := reasoning.Input{
		Provider:         provider,
		CanonicalModelID: canonicalID,
		WireShape:        reasoningWireShapeForClient(rc),
		Capability:       capability,
		Stored:           stored,
	}
	effective := reasoning.EffectivePolicy(input)
	adapter := reasoning.ReviewedAdapterAvailability(input)
	configured := adminReasoningPolicy{Mode: "default"}
	if stored.Present {
		configured.Mode = string(stored.Mode)
		configured.Effort = stored.Effort
	}
	projected := adminReasoningStatus{
		Configured: configured,
		Effective: adminReasoningPolicy{
			Mode:   string(effective.Mode),
			Effort: effective.Effort,
		},
		Source:           string(effective.Source),
		AvailableModes:   make([]string, 0, len(adapter.Modes)),
		AvailableEfforts: append([]string(nil), adapter.Efforts...),
	}
	if projected.AvailableEfforts == nil {
		projected.AvailableEfforts = []string{}
	}
	for _, mode := range adapter.Modes {
		projected.AvailableModes = append(
			projected.AvailableModes,
			string(mode),
		)
	}

	switch {
	case effective.Mode == reasoning.ModePassthrough:
		// Passthrough is an executable read-only state even when the
		// catalog or active adapter exposes no configurable controls.
		projected.Available = true
	case !capability.Known:
		projected.UnavailableReason =
			reasoningUnavailableCatalogUnknown
		projected.Error = fmt.Sprintf(
			"exact Baseten model %q has no validated reasoning capability",
			canonicalID,
		)
	case !capability.Supported:
		projected.UnavailableReason =
			reasoningUnavailableUnsupported
		projected.Error = fmt.Sprintf(
			"exact Baseten model %q is marked reasoning unsupported",
			canonicalID,
		)
	case effective.Mode == reasoning.ModeFixed &&
		!containsReasoningValue(capability.Efforts, effective.Effort):
		projected.UnavailableReason =
			reasoningUnavailableEffortRemoved
		projected.Error = fmt.Sprintf(
			"configured effort %q is not advertised by exact Baseten model %q",
			effective.Effort,
			canonicalID,
		)
	case !effectivePolicyAvailable(effective, adapter):
		projected.UnavailableReason =
			reasoningUnavailableAdapter
		projected.Error =
			"no reviewed reasoning control is available for this client protocol"
	default:
		if _, err := reasoning.Resolve(input); err != nil {
			projected.UnavailableReason =
				reasoningUnavailableAdapter
			projected.Error = err.Error()
		} else {
			projected.Available = true
		}
	}
	return projected
}

func effectivePolicyAvailable(
	effective reasoning.Decision,
	adapter reasoning.AdapterAvailability,
) bool {
	switch effective.Mode {
	case reasoning.ModePassthrough:
		return true
	case reasoning.ModeFixed:
		return containsReasoningValue(adapter.Efforts, effective.Effort)
	default:
		for _, mode := range adapter.Modes {
			if mode == effective.Mode {
				return true
			}
		}
		return false
	}
}

func reasoningWireShapeForClient(
	rc resolvedClientConfig,
) reasoning.WireShape {
	if rc.ProtocolShape == "" || rc.ProtocolShape == "anthropic" {
		if rc.UpstreamShape == "openai" {
			return reasoning.WireTranslatedChat
		}
		return reasoning.WireAnthropicMessages
	}
	// The current OpenAI-shape clients are Responses-first. No OpenAI
	// reasoning transform is reviewed yet, so both Responses and Chat
	// correctly project an empty adapter intersection in this release.
	return reasoning.WireOpenAIResponses
}

func catalogReasoningInput(
	snapshot *pricing.Snapshot,
	provider string,
	canonicalID string,
) reasoning.Capability {
	if snapshot == nil {
		return reasoning.Capability{}
	}
	catalogCapability, ok := snapshot.ModelReasoning(
		provider,
		canonicalID,
	)
	if !ok {
		return reasoning.Capability{}
	}
	capability := reasoning.Capability{
		Known:     true,
		Supported: catalogCapability.Supported,
	}
	for _, option := range catalogCapability.Options {
		switch option.Type {
		case pricing.ReasoningToggle:
			capability.Toggle = true
		case pricing.ReasoningEffort:
			for _, value := range option.Values {
				if value != nil {
					capability.Efforts = append(
						capability.Efforts,
						*value,
					)
				}
			}
		case pricing.ReasoningBudgetTokens:
			capability.BudgetTokens = true
		}
	}
	return capability
}

func storedReasoningPolicy(
	rc resolvedClientConfig,
	provider string,
	canonicalID string,
) reasoning.StoredPolicy {
	providerOptions, ok := rc.ModelOptions[provider]
	if !ok {
		return reasoning.StoredPolicy{}
	}
	modelOption, ok := providerOptions[canonicalID]
	if !ok || modelOption.Reasoning == nil {
		return reasoning.StoredPolicy{}
	}
	return reasoning.StoredPolicy{
		Present: true,
		Mode:    reasoning.Mode(modelOption.Reasoning.Mode),
		Effort:  modelOption.Reasoning.Effort,
	}
}

func containsReasoningValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
