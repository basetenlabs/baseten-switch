package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/reasoning"
)

type adminReasoningPreflightRequest struct {
	Client   string                 `json:"client"`
	Provider string                 `json:"provider"`
	Model    string                 `json:"model"`
	Policy   config.ReasoningPolicy `json:"policy"`
}

type adminReasoningPreflightClient struct {
	Name              string   `json:"name"`
	Enabled           bool     `json:"enabled"`
	Reachable         bool     `json:"reachable"`
	Supported         bool     `json:"supported"`
	Reachability      []string `json:"reachability"`
	FailureBehaviors  []string `json:"failure_behaviors"`
	AvailableModes    []string `json:"available_modes"`
	AvailableEfforts  []string `json:"available_efforts"`
	UnavailableReason string   `json:"unavailable_reason"`
	Error             string   `json:"error"`
}

type adminReasoningPreflightResponse struct {
	Provider  string                          `json:"provider"`
	Model     string                          `json:"model"`
	Policy    config.ReasoningPolicy          `json:"policy"`
	Available bool                            `json:"available"`
	Error     string                          `json:"error"`
	Warning   string                          `json:"warning"`
	Clients   []adminReasoningPreflightClient `json:"clients"`
}

func (g *Gateway) adminReasoningPreflight(
	w http.ResponseWriter,
	r *http.Request,
) {
	if r.Method != http.MethodPost {
		g.reject(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request adminReasoningPreflightRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		g.reject(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values are not supported")
		}
		g.reject(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}

	response := adminReasoningPreflightResponse{
		Provider: request.Provider,
		Model:    request.Model,
		Policy:   request.Policy,
		Clients:  []adminReasoningPreflightClient{},
	}
	if strings.TrimSpace(request.Client) == "" {
		response.Error = "client cannot be empty"
		writeJSON(w, http.StatusOK, response)
		return
	}
	snapshot := g.pricing.Capture()
	if err := validateReasoningPreflightPolicy(snapshot, request); err != nil {
		response.Error = err.Error()
		writeJSON(w, http.StatusOK, response)
		return
	}
	if capability := modelCatalogReasoningFromSnapshot(
		snapshot,
		request.Model,
		modelCatalogNow().UTC(),
	); capability != nil && capability.Stale {
		response.Warning =
			"reasoning metadata is stale but remains validated"
		if capability.CapturedAt != "" {
			response.Warning +=
				" (captured " + capability.CapturedAt + ")"
		}
	}

	stored := reasoning.StoredPolicy{
		Present: true,
		Mode:    reasoning.Mode(request.Policy.Mode),
		Effort:  request.Policy.Effort,
	}
	var configured *reasoningPreflightConfiguredClient
	for _, candidate := range g.reasoningPreflightClients() {
		if candidate.name == request.Client {
			copy := candidate
			configured = &copy
			break
		}
	}
	if configured == nil {
		response.Error = fmt.Sprintf(
			"client %q is not configured",
			request.Client,
		)
		writeJSON(w, http.StatusOK, response)
		return
	}
	reachability := reasoningTargetReachability(
		configured.rc,
		request.Model,
	)
	client := adminReasoningPreflightClient{
		Name:         configured.name,
		Enabled:      configured.enabled,
		Reachable:    len(reachability) > 0,
		Reachability: reachability,
		FailureBehaviors: reasoningFailureBehaviors(
			configured.rc,
			reachability,
		),
		AvailableModes:   []string{},
		AvailableEfforts: []string{},
	}
	if !client.Reachable {
		response.Error = fmt.Sprintf(
			"client %q does not route to model %q",
			request.Client,
			request.Model,
		)
		response.Clients = append(response.Clients, client)
		writeJSON(w, http.StatusOK, response)
		return
	}
	projection := projectClientReasoningPolicy(
		configured.rc,
		snapshot,
		request.Provider,
		request.Model,
		stored,
	)
	client.Supported = projection.Available
	client.AvailableModes = projection.AvailableModes
	client.AvailableEfforts = projection.AvailableEfforts
	client.UnavailableReason = projection.UnavailableReason
	client.Error = projection.Error
	response.Clients = append(response.Clients, client)
	response.Available = client.Supported
	if !client.Supported {
		response.Error = client.Error
	}
	writeJSON(w, http.StatusOK, response)
}

func validateReasoningPreflightPolicy(
	snapshot *pricing.Snapshot,
	request adminReasoningPreflightRequest,
) error {
	if request.Provider != pricing.ProviderBaseten {
		return fmt.Errorf(
			"provider %q is unsupported",
			request.Provider,
		)
	}
	if strings.TrimSpace(request.Model) == "" {
		return fmt.Errorf("model cannot be empty")
	}
	if err := config.ValidateReasoningPolicy(request.Policy); err != nil {
		return err
	}
	capability, ok := snapshot.ModelReasoning(
		request.Provider,
		request.Model,
	)
	if !ok {
		return fmt.Errorf(
			"model %q has no validated reasoning metadata",
			request.Model,
		)
	}
	if !capability.Supported {
		return fmt.Errorf(
			"model %q does not support reasoning",
			request.Model,
		)
	}
	switch request.Policy.Mode {
	case config.ReasoningOff:
		for _, option := range capability.Options {
			if option.Type == pricing.ReasoningToggle ||
				(option.Type == pricing.ReasoningEffort &&
					catalogEffortHasDisabled(option.Values)) {
				return nil
			}
		}
		return fmt.Errorf(
			"model %q does not advertise a reasoning-off control",
			request.Model,
		)
	case config.ReasoningFollowHarness:
		if len(capability.Options) == 0 {
			return fmt.Errorf(
				"model %q has no verified configurable reasoning control",
				request.Model,
			)
		}
		return nil
	case config.ReasoningFixed:
		for _, option := range capability.Options {
			if option.Type != pricing.ReasoningEffort {
				continue
			}
			for _, effort := range option.Values {
				if effort != nil && *effort == request.Policy.Effort {
					return nil
				}
			}
		}
		return fmt.Errorf(
			"model %q does not advertise reasoning effort %q",
			request.Model,
			request.Policy.Effort,
		)
	default:
		return fmt.Errorf(
			"reasoning mode %q is invalid",
			request.Policy.Mode,
		)
	}
}

func catalogEffortHasDisabled(values []*string) bool {
	for _, value := range values {
		if value == nil || *value == "none" {
			return true
		}
	}
	return false
}

type reasoningPreflightConfiguredClient struct {
	name    string
	enabled bool
	rc      resolvedClientConfig
}

func (g *Gateway) reasoningPreflightClients() []reasoningPreflightConfiguredClient {
	g.routingMu.RLock()
	state := g.activeRoutingState()
	runtimeClients := g.snapshotClients()
	g.routingMu.RUnlock()
	runtimeByName := map[string]resolvedClientConfig{}
	for _, client := range runtimeClients {
		runtimeByName[client.cfg.Name] = client.cfg
	}
	if state.file != nil {
		out := make(
			[]reasoningPreflightConfiguredClient,
			0,
			len(state.file.Clients),
		)
		for _, client := range state.file.Clients {
			rc, ok := runtimeByName[client.Name]
			if !ok {
				rc = resolvedStatusClient(state.file, client)
			}
			out = append(out, reasoningPreflightConfiguredClient{
				name:    client.Name,
				enabled: client.Enabled,
				rc:      rc,
			})
		}
		return out
	}
	out := make(
		[]reasoningPreflightConfiguredClient,
		0,
		len(runtimeClients),
	)
	for _, client := range runtimeClients {
		out = append(out, reasoningPreflightConfiguredClient{
			name:    client.cfg.Name,
			enabled: true,
			rc:      client.cfg,
		})
	}
	return out
}

func reasoningTargetReachability(
	rc resolvedClientConfig,
	canonicalID string,
) []string {
	sources := map[string]bool{}
	if canonical, ok := canonicalBasetenTarget(
		rc,
		rc.DefaultModel,
	); ok && canonical == canonicalID {
		sources["default_model"] = true
	}
	for _, target := range rc.ModelRoutes {
		if canonical, ok := canonicalBasetenTarget(rc, target); ok &&
			canonical == canonicalID {
			sources["family_mapping"] = true
		}
	}
	for _, target := range rc.ModelAliases {
		if canonical, ok := canonicalBasetenTarget(rc, target); ok &&
			canonical == canonicalID {
			sources["alias"] = true
		}
	}
	if canonical, ok := canonicalBasetenTarget(
		rc,
		rc.SubagentModel,
	); ok && canonical == canonicalID {
		sources["subagent_override"] = true
	}
	if (rc.ProtocolShape == "" || rc.ProtocolShape == "anthropic") &&
		strings.Contains(canonicalID, "/") {
		sources["explicit_raw_slug"] = true
	}
	out := make([]string, 0, len(sources))
	for source := range sources {
		out = append(out, source)
	}
	sort.Strings(out)
	return out
}

func reasoningFailureBehaviors(
	rc resolvedClientConfig,
	reachability []string,
) []string {
	behaviors := map[string]bool{}
	for _, source := range reachability {
		switch {
		case source == "default_model",
			source == "family_mapping":
			if rc.FallbackRoute == config.NativeRoute(rc.ProtocolShape) {
				behaviors["native_fallback"] = true
			} else {
				behaviors["local_error"] = true
			}
		default:
			behaviors["local_error"] = true
		}
	}
	out := make([]string, 0, len(behaviors))
	for behavior := range behaviors {
		out = append(out, behavior)
	}
	sort.Strings(out)
	return out
}
