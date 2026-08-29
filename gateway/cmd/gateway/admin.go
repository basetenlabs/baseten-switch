package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/modelmeta"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

var (
	gatewayStart    = time.Now()
	gatewayStartMu  sync.RWMutex
	gatewayStartSet = false
)

func (g *Gateway) markStarted() {
	gatewayStartMu.Lock()
	gatewayStart = time.Now()
	gatewayStartSet = true
	gatewayStartMu.Unlock()
}

func (g *Gateway) uptimeSeconds() int {
	gatewayStartMu.RLock()
	defer gatewayStartMu.RUnlock()
	return int(time.Since(gatewayStart).Seconds())
}

func (g *Gateway) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("/v1/admin/healthz", g.adminHealthz)
	mux.HandleFunc("/v1/admin/config", g.adminConfig)
	mux.HandleFunc("/v1/admin/model-catalog", g.adminModelCatalog)
	mux.HandleFunc("/v1/admin/status", g.adminStatus)
	mux.HandleFunc(
		"/v1/admin/reasoning/preflight",
		g.adminReasoningPreflight,
	)
	mux.HandleFunc("/v1/admin/secrets", g.adminSecrets)
	mux.HandleFunc("/v1/admin/telemetry", g.adminTelemetry)
	mux.HandleFunc("/v1/admin/stats", g.adminStats)
	mux.HandleFunc("/v1/admin/analytics", g.adminAnalytics)
	mux.HandleFunc("/v1/admin/requests", g.adminRequests)
	mux.HandleFunc("/v1/admin/auth/reload", g.handleAuthReload)
	mux.HandleFunc("/v1/admin/auth/status", g.handleAuthStatus)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func (g *Gateway) adminHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		g.reject(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":             true,
		"uptime_seconds": g.uptimeSeconds(),
		"version":        version.Version,
	})
}

func (g *Gateway) adminConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		f, err := config.Load(g.activeConfigPath())
		if err != nil {
			g.reject(w, 500, "load config: "+err.Error())
			return
		}
		writeJSON(w, 200, f)
	case http.MethodPut:
		var f config.File
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			g.reject(w, 400, "decode body: "+err.Error())
			return
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			if err == nil {
				err = fmt.Errorf("multiple JSON values are not supported")
			}
			g.reject(w, 400, "decode body: "+err.Error())
			return
		}
		if err := ValidateConfigFile(&f); err != nil {
			g.reject(w, 400, "invalid config: "+err.Error())
			return
		}
		active, err := config.Load(g.activeConfigPath())
		if err != nil {
			g.reject(w, 500, "load active config: "+err.Error())
			return
		}
		if err := validateAdminTraceCaptureMutation(active, &f); err != nil {
			g.reject(w, 403, err.Error())
			return
		}
		if err := config.Save(g.activeConfigPath(), &f); err != nil {
			g.reject(w, 500, "save config: "+err.Error())
			return
		}
		applyConfigEnv(&f)
		// Trigger an in-process rebind rather than relying on an
		// external SIGHUP round-trip. reloadConfig re-reads the file
		// we just saved.
		g.reloadConfig()
		writeJSON(w, 200, f)
	default:
		g.reject(w, 405, "method not allowed")
	}
}

// validateAdminTraceCaptureMutation keeps the unauthenticated loopback admin
// surface from increasing content-capture scope. Activation and allowlist
// expansion require a typed local CLI action or an exact local file edit.
func validateAdminTraceCaptureMutation(active, proposed *config.File) error {
	current, err := config.ResolveTraceCapture(active)
	if err != nil {
		return fmt.Errorf("active trace capture policy is invalid")
	}
	next, err := config.ResolveTraceCapture(proposed)
	if err != nil {
		return fmt.Errorf("invalid trace capture policy: %w", err)
	}
	if !current.Enabled && next.Enabled {
		return fmt.Errorf("trace capture cannot be enabled through the administration API; use a local CLI command or edit gateway.yaml")
	}
	if !next.Enabled {
		return nil
	}
	allowed := make(map[string]struct{}, len(current.Clients))
	for _, client := range current.Clients {
		allowed[client] = struct{}{}
	}
	for _, client := range next.Clients {
		if _, ok := allowed[client]; !ok {
			return fmt.Errorf("trace capture client allowlist cannot be expanded through the administration API; use a local CLI command or edit gateway.yaml")
		}
	}
	return nil
}

// activeConfigPath returns the path the gateway is currently reading
// gateway.yaml from: cfg.ConfigPath when set (BASETEN_SWITCH_CONFIG_PATH), else
// the default ~/.config/baseten-switch/gateway.yaml. Admin endpoints
// always operate on this path so they reflect the live config rather
// than the homedir default.
func (g *Gateway) activeConfigPath() string {
	if cfg := g.runtimeConfig(); cfg.ConfigPath != "" {
		return cfg.ConfigPath
	}
	return config.DefaultPath()
}

// applyConfigEnv expands global.auth values and exports them to the
// process environment. Shared by the admin config PUT path and, via
// applyGlobalAuth, by startup and SIGHUP reload.
func applyConfigEnv(f *config.File) {
	for k, v := range f.Global.Auth {
		if v != "" {
			expanded := config.Expand(v)
			if expanded != "" {
				switch k {
				case "baseten":
					os.Setenv("BASETEN_API_KEY", expanded)
				case "anthropic":
					os.Setenv("ANTHROPIC_API_KEY", expanded)
				}
			}
		}
	}
}

// familyRepresentativeIDs maps each configurable family to a representative
// native model id used to compute the admin status effective table.
// Family extraction (familyOf) is substring-based and works for any id
// in the family.
var familyRepresentativeIDs = map[string]string{
	"fable":  "claude-fable-5",
	"opus":   "claude-opus-4-8",
	"sonnet": "claude-sonnet-4-6",
	"haiku":  "claude-haiku-4-5",
}

// familyEntry is one row of the admin status "families" table.
type familyEntry struct {
	Family           string  `json:"family"`
	ConfiguredTarget *string `json:"configured_target"`
	ConfiguredSource string  `json:"configured_source"`
	EffectiveRoute   string  `json:"effective_route"`
	EffectiveModel   string  `json:"effective_model"`
	EffectiveSource  string  `json:"effective_source"`
}

// computeFamilies builds the server-computed effective table for one
// anthropic-shape client, one entry per family in modelFamilySet. The
// family row reflects rc.ModelRoutes[fam] and falls through to the
// switch position when absent. EffectiveModel "" means native
// passthrough. Non-anthropic clients get nil.
func computeFamilies(rc resolvedClientConfig) []familyEntry {
	if rc.ProtocolShape != "anthropic" {
		return nil
	}
	out := make([]familyEntry, 0, len(modelFamilySet))
	for _, fam := range modelFamilySet {
		entry := familyEntry{
			Family:           fam,
			ConfiguredSource: "default",
		}
		pin := rc.ModelRoutes[fam]
		if pin != "" {
			target := pin
			entry.ConfiguredTarget = &target
			entry.ConfiguredSource = "explicit"
		} else {
			if target := rc.DefaultModel; target != "" {
				entry.ConfiguredTarget = &target
			}
		}
		if pin != "" {
			resolvedPin := modelRoutePinValue(rc, pin)
			if rc.globalRoutingOff() {
				entry.EffectiveRoute = config.NativeRoute(rc.ProtocolShape)
				entry.EffectiveSource = "global_off"
			} else {
				entry.EffectiveRoute = resolvedPin.route
				entry.EffectiveModel = resolvedPin.forcedModel
				if pin == "native" {
					entry.EffectiveSource = "native_mapping"
				} else {
					entry.EffectiveSource = "family_mapping"
				}
			}
		} else {
			repID := familyRepresentativeIDs[fam]
			decision := resolveNativeModelPolicy(rc, repID)
			entry.EffectiveRoute = decision.route
			entry.EffectiveModel = decision.model
			entry.EffectiveSource = decision.source
		}
		out = append(out, entry)
	}
	return out
}

// modelCatalogEntry is one selectable family target in the admin status
// "model_catalog" table.
type modelCatalogEntry struct {
	Label         string `json:"label"`
	Slug          string `json:"slug"`
	StorageTarget string `json:"storage_target"`
	Alias         string `json:"alias"`
	Available     bool   `json:"available"`
}

type modelPickerStatus struct {
	Enabled bool                     `json:"enabled"`
	Models  []modelPickerStatusModel `json:"models"`
}

type modelPickerStatusModel struct {
	Alias       string `json:"alias"`
	Slug        string `json:"slug"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// computeModelPickerStatus projects the desired config in its saved order.
// It intentionally does not read Claude settings or infer whether policy has
// filtered a row; the Claude adapter owns those file and runtime status axes.
func computeModelPickerStatus(c config.Client) *modelPickerStatus {
	if c.ModelPicker == nil {
		return nil
	}
	status := &modelPickerStatus{
		Enabled: c.ModelPicker.Enabled,
		Models:  make([]modelPickerStatusModel, 0, len(c.ModelPicker.Models)),
	}
	for _, model := range c.ModelPicker.Models {
		slug := c.ModelAliases[model.Alias]
		status.Models = append(status.Models, modelPickerStatusModel{
			Alias:       model.Alias,
			Slug:        slug,
			Label:       modelmeta.ResolveBaseten(slug).DisplayName + " via Baseten",
			Description: "Served by Baseten.",
		})
	}
	return status
}

// computeModelCatalog builds display metadata for one client's configured
// Baseten targets: one entry per model_aliases entry, the default model when
// not covered by an alias, and raw slugs saved in model_routes or
// subagent_model. Including saved raw slugs keeps a live
// catalog selection usable when the remote catalog is unavailable later. The
// catalog is deduped by slug. Labels come from one captured normalized catalog
// snapshot, with modelmeta used only when that snapshot has no record. Every
// protocol receives the catalog so non-picker status surfaces can render the
// same human-facing names.
func computeModelCatalog(
	rc resolvedClientConfig,
	snapshots ...*pricing.Snapshot,
) []modelCatalogEntry {
	var snapshot *pricing.Snapshot
	if len(snapshots) > 0 {
		snapshot = snapshots[0]
	}
	seen := map[string]bool{}
	out := []modelCatalogEntry{}
	// Aliases first: target is the alias id the picker selects.
	aliasKeys := modelRouteKeys(rc.ModelAliases)
	for _, alias := range sortedKeys(aliasKeys) {
		slug := rc.ModelAliases[alias]
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, modelCatalogEntry{
			Label:         basetenModelDisplayName(snapshot, slug),
			Slug:          slug,
			StorageTarget: slug, Alias: alias, Available: true,
		})
	}
	// Default model not covered by an alias: target is the slug itself.
	if slug := rc.DefaultModel; slug != "" && !seen[slug] {
		seen[slug] = true
		out = append(out, modelCatalogEntry{
			Label:         basetenModelDisplayName(snapshot, slug),
			Slug:          slug,
			StorageTarget: slug, Available: true,
		})
	}
	// A live-catalog selection is persisted as a raw slug. Once persisted it
	// is configured state, so keep it in the deterministic baseline even
	// while live discovery is signed out or unavailable.
	rawConfigured := map[string]string{}
	for _, target := range rc.ModelRoutes {
		target = strings.TrimSpace(target)
		if strings.Contains(target, "/") {
			rawConfigured[target] = target
		}
	}
	subagentTarget := strings.TrimSpace(rc.SubagentModel)
	if strings.Contains(subagentTarget, "/") {
		rawConfigured[subagentTarget] = subagentTarget
	}
	for _, slug := range sortedKeys(modelRouteKeys(rawConfigured)) {
		if seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, modelCatalogEntry{
			Label:         basetenModelDisplayName(snapshot, slug),
			Slug:          slug,
			StorageTarget: slug, Available: true,
		})
	}
	return out
}

// modelRouteKeys returns the keys of m as a slice (nil-safe).
func modelRouteKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

type activeRoutingState struct {
	bootID     string
	generation uint64
	activeHash string
	file       *config.File
	reloadErr  string
}

func (g *Gateway) activeRoutingState() activeRoutingState {
	g.stateMu.RLock()
	defer g.stateMu.RUnlock()
	return activeRoutingState{
		bootID: g.routerBootID, generation: g.activeGeneration,
		activeHash: g.activeConfigHash, file: g.activeConfigFile,
		reloadErr: g.reloadError,
	}
}

func fallbackStatus(g *Gateway, rc resolvedClientConfig) map[string]any {
	deadline, active := g.fallbackDeadline(rc.Name)
	var retryAfter any
	if active {
		retryAfter = deadline.UTC().Format(time.RFC3339Nano)
	}
	cause := ""
	served := ""
	if active {
		cause = "cooldown"
		served = rc.FallbackRoute
	}
	return map[string]any{
		"active": active, "served_route": served, "cause": cause,
		"since": nil, "retry_after": retryAfter,
	}
}

func providerDisplayName(provider string) string {
	switch strings.ToLower(provider) {
	case "anthropic":
		return "Anthropic"
	case "openai":
		return "OpenAI"
	case "baseten":
		return "Baseten"
	case "":
		return "Native provider"
	default:
		return provider
	}
}

func effectiveSummary(
	rc resolvedClientConfig,
	families []familyEntry,
	snapshots ...*pricing.Snapshot,
) string {
	if rc.globalRoutingOff() {
		return "Native · " + providerDisplayName(config.NativeRoute(rc.ProtocolShape))
	}
	if rc.Route != "baseten" {
		return "Native · " + providerDisplayName(rc.Route)
	}
	model := ""
	for _, family := range families {
		if family.EffectiveRoute != "baseten" || family.EffectiveModel == "" {
			return "Custom routing"
		}
		if model == "" {
			model = family.EffectiveModel
		} else if model != family.EffectiveModel {
			return "Custom routing"
		}
	}
	unmatched := resolveNativeModelPolicy(rc, "claude-unrecognized-model")
	if unmatched.route != "baseten" || unmatched.model == "" {
		return "Custom routing"
	}
	if model != "" && model != unmatched.model {
		return "Custom routing"
	}
	var snapshot *pricing.Snapshot
	if len(snapshots) > 0 {
		snapshot = snapshots[0]
	}
	return "Baseten · " + basetenModelDisplayName(snapshot, unmatched.model)
}

func resolvedStatusClient(f *config.File, c config.Client) resolvedClientConfig {
	shape := c.ProtocolShape
	if shape == "" {
		shape = "anthropic"
	}
	rt := "baseten"
	globalEnabled := f.Global.RoutingEnabled != nil && *f.Global.RoutingEnabled
	if !globalEnabled {
		rt = config.NativeRoute(shape)
	}
	return resolvedClientConfig{
		Name: c.Name, BindAddr: c.BindAddr, ProtocolShape: shape, Route: rt,
		HasGlobalRoutingGate: true,
		GlobalRoutingEnabled: globalEnabled,
		DefaultModel:         c.DefaultModel,
		ModelAliases:         c.ModelAliases, SubagentModel: c.SubagentModel,
		SubagentRouting: c.SubagentRouting, ModelRoutes: c.ModelRoutes,
		ModelOptions:  cloneModelOptions(c.ModelOptions),
		FallbackRoute: c.FallbackRoute, UpstreamShape: c.UpstreamShape,
	}
}

func (g *Gateway) adminStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		g.reject(w, 405, "method not allowed")
		return
	}
	g.routingMu.RLock()
	state := g.activeRoutingState()
	runtimeClients := g.snapshotClients()
	runtimeCfg := g.runtimeConfig()
	g.routingMu.RUnlock()
	catalogSnapshot := g.pricing.Capture()
	runtimeByName := map[string]resolvedClientConfig{}
	boundByName := map[string]bool{}
	for _, cl := range runtimeClients {
		runtimeByName[cl.cfg.Name] = cl.cfg
		boundByName[cl.cfg.Name] = true
	}
	clients := []map[string]any{}
	if state.file != nil {
		for _, c := range state.file.Clients {
			resolved := ""
			if c.AuthToken != nil {
				resolved = config.Expand(c.AuthToken.Value)
			}
			rcEff, ok := runtimeByName[c.Name]
			if !ok {
				rcEff = resolvedStatusClient(state.file, c)
			}
			families := computeFamilies(rcEff)
			summary := effectiveSummary(rcEff, families, catalogSnapshot)
			if !c.Enabled {
				summary = "Not configured"
			}
			subagentEffective := rcEff.SubagentModel
			if subagentEffective == "" || rcEff.SubagentRouting == "off" {
				subagentEffective = "inherit"
			}
			unmatched := resolveNativeModelPolicy(rcEff, "claude-unrecognized-model")
			configuredTarget := rcEff.DefaultModel
			var configuredTargetJSON any
			if configuredTarget != "" {
				configuredTargetJSON = configuredTarget
			}
			clients = append(clients, map[string]any{
				"name":               c.Name,
				"enabled":            c.Enabled,
				"bind_addr":          c.BindAddr,
				"protocol_shape":     rcEff.ProtocolShape,
				"effective_route":    rcEff.Route,
				"native_route":       config.NativeRoute(rcEff.ProtocolShape),
				"auth_set":           resolved != "",
				"currently_bound":    boundByName[c.Name],
				"effective_summary":  summary,
				"fallback_route":     rcEff.FallbackRoute,
				"fallback":           fallbackStatus(g, rcEff),
				"subagent_model":     rcEff.SubagentModel,
				"subagent_routing":   rcEff.SubagentRouting,
				"subagent_effective": subagentEffective,
				"model_routes":       rcEff.ModelRoutes,
				"families":           families,
				"model_catalog":      computeModelCatalog(rcEff, catalogSnapshot),
				"model_picker":       computeModelPickerStatus(c),
				"model_options":      computeClientModelOptions(rcEff, catalogSnapshot),
				"unmatched_native_model": map[string]any{
					"configured_target": configuredTargetJSON,
					"effective_route":   unmatched.route,
					"effective_model":   unmatched.model,
					"effective_source":  unmatched.source,
				},
			})
		}
	} else {
		// Embedders that construct New without a backing config still get
		// coherent live listener state, but no desired/active file token.
		for _, cl := range runtimeClients {
			families := computeFamilies(cl.cfg)
			unmatched := resolveNativeModelPolicy(cl.cfg, "claude-unrecognized-model")
			configuredTarget := cl.cfg.DefaultModel
			var configuredTargetJSON any
			if configuredTarget != "" {
				configuredTargetJSON = configuredTarget
			}
			subagentEffective := cl.cfg.SubagentModel
			if subagentEffective == "" || cl.cfg.SubagentRouting == "off" {
				subagentEffective = "inherit"
			}
			clients = append(clients, map[string]any{
				"name":               cl.cfg.Name,
				"bind_addr":          cl.Addr().String(),
				"protocol_shape":     cl.cfg.ProtocolShape,
				"effective_route":    cl.cfg.Route,
				"native_route":       config.NativeRoute(cl.cfg.ProtocolShape),
				"enabled":            true,
				"currently_bound":    true,
				"effective_summary":  effectiveSummary(cl.cfg, families, catalogSnapshot),
				"fallback_route":     cl.cfg.FallbackRoute,
				"fallback":           fallbackStatus(g, cl.cfg),
				"subagent_model":     cl.cfg.SubagentModel,
				"subagent_routing":   cl.cfg.SubagentRouting,
				"subagent_effective": subagentEffective,
				"model_routes":       cl.cfg.ModelRoutes,
				"families":           families,
				"model_catalog":      computeModelCatalog(cl.cfg, catalogSnapshot),
				"model_picker":       nil,
				"model_options":      computeClientModelOptions(cl.cfg, catalogSnapshot),
				"unmatched_native_model": map[string]any{
					"configured_target": configuredTargetJSON,
					"effective_route":   unmatched.route,
					"effective_model":   unmatched.model,
					"effective_source":  unmatched.source,
				},
			})
		}
	}
	desiredHash := ""
	if raw, err := os.ReadFile(g.activeConfigPath()); err == nil {
		desiredHash = exactConfigHash(raw)
	}
	reloadState := "applied"
	reloadErr := state.reloadErr
	if reloadErr != "" {
		reloadState = "error"
	} else if desiredHash != "" && state.activeHash != "" && desiredHash != state.activeHash {
		reloadState = "pending"
	}
	globalEnabled := false
	capabilities := []string{"global_routing"}
	if state.file != nil {
		if state.file.Global.RoutingEnabled != nil {
			globalEnabled = *state.file.Global.RoutingEnabled
		}
	}
	signedIn, authType, fallbackInUse := g.authState()
	ah := g.authHealth()
	authUnavailableFallback := g.authUnavailableFallbackStatus()
	writeJSON(w, 200, map[string]any{
		"router_pid":          os.Getpid(),
		"router_boot_id":      state.bootID,
		"active_generation":   state.generation,
		"active_config_hash":  state.activeHash,
		"desired_config_hash": desiredHash,
		"capabilities":        capabilities,
		"health":              "ready",
		"reload": map[string]any{
			"state": reloadState,
			"error": reloadErr,
		},
		"global_routing_enabled": globalEnabled,
		"uptime_seconds":         g.uptimeSeconds(),
		"version":                version.Version,
		// Mutation clients must target the exact file this process has
		// loaded. Ambient CLI sticky state can point at an older scratch
		// config even while this router is healthy.
		"config_path":   g.activeConfigPath(),
		"telemetry":     g.telemetryAdminHealth(runtimeCfg),
		"trace_capture": g.traceAdminHealth(),
		"baseten_catalog": sanitizedBasetenCatalogHealthJSON(
			g.catalogHealth(),
		),
		"model_catalog": g.modelCatalogHealthJSON(),
		"auth": map[string]any{
			"signed_in":             signedIn,
			"auth_type":             authType,
			"health":                ah.Health,
			"last_refresh_error":    ah.LastError,
			"last_refresh_error_at": rfc3339OrEmpty(ah.LastErrorAt),
			"last_refresh_ok_at":    rfc3339OrEmpty(ah.LastOKAt),
			"profile":               runtimeCfg.OAuthProfile,
			"fallback_enabled":      runtimeCfg.APIKeyFallback,
			"fallback_in_use":       fallbackInUse,
		},
		"auth_unavailable_fallback": authUnavailableFallback,
		"clients":                   clients,
	})
}

func (g *Gateway) authUnavailableFallbackStatus() map[string]any {
	g.authUnavailableFallbackMu.RLock()
	count := g.authUnavailableFallbackCount
	lastAt := g.authUnavailableFallbackAt
	lastClient := g.authUnavailableFallbackClient
	lastRoute := g.authUnavailableFallbackRoute
	g.authUnavailableFallbackMu.RUnlock()
	return map[string]any{
		"count":       count,
		"last_at":     rfc3339OrEmpty(lastAt),
		"last_client": lastClient,
		"last_route":  lastRoute,
	}
}

// modelCatalogHealthJSON reports the active normalized catalog for each
// supported provider. The active Baseten catalog can come from either the
// public models.dev refresh or the authenticated Baseten /v1/models refresh.
// In the latter case, use that refresh manager's timing and error state.
func (g *Gateway) modelCatalogHealthJSON() map[string]any {
	result := make(map[string]any, 3)
	for _, provider := range []string{
		pricing.ProviderAnthropic,
		pricing.ProviderOpenAI,
		pricing.ProviderBaseten,
	} {
		health := g.publicCatalogProviderHealth(provider)
		if provider == pricing.ProviderBaseten &&
			isAuthenticatedBasetenCatalogSource(health.Source) {
			authenticated := g.catalogHealth()
			health.LastAttemptAt = authenticated.LastAttemptAt
			health.LastSuccessAt = authenticated.LastSuccessAt
			health.NextRefreshAt = authenticated.NextRefreshAt
			health.Stale = authenticated.Stale
			health.LastError = authenticated.LastError
		}
		health.LastError = sanitizeCatalogDiagnosticError(health.LastError)
		result[provider] = publicCatalogHealthJSON(health)
	}
	return result
}

func isAuthenticatedBasetenCatalogSource(source string) bool {
	switch strings.TrimSpace(source) {
	case "baseten_v1_models", "baseten-v1-models":
		return true
	default:
		return false
	}
}

func sanitizedBasetenCatalogHealthJSON(health catalogHealth) map[string]any {
	health.LastError = sanitizeCatalogDiagnosticError(health.LastError)
	return catalogHealthJSON(health)
}

// sanitizeCatalogDiagnosticError keeps status diagnostics actionable without
// exposing request URLs, credentials, local paths, or remote response bodies.
// Detailed errors remain available in the gateway's local stderr logs.
func sanitizeCatalogDiagnosticError(message string) string {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return ""
	}
	switch {
	case strings.Contains(normalized, "canceled"):
		return "refresh canceled"
	case strings.Contains(normalized, "deadline"),
		strings.Contains(normalized, "timed out"),
		strings.Contains(normalized, "timeout"):
		return "refresh timed out"
	case strings.Contains(normalized, "returned 401"),
		strings.Contains(normalized, "returned 403"),
		strings.Contains(normalized, "unauthorized"),
		strings.Contains(normalized, "forbidden"):
		return "catalog source rejected authorization"
	case strings.Contains(normalized, "returned "),
		strings.Contains(normalized, "status code"):
		return "catalog source returned a non-success status"
	case strings.Contains(normalized, "response exceeds"),
		strings.Contains(normalized, "body too large"):
		return "catalog response exceeded size limit"
	case strings.Contains(normalized, "cache"),
		strings.Contains(normalized, "persist"),
		strings.Contains(normalized, "rename"),
		strings.Contains(normalized, "permission"):
		return "catalog cache operation failed"
	case strings.Contains(normalized, "json"),
		strings.Contains(normalized, "decode"),
		strings.Contains(normalized, "invalid"),
		strings.Contains(normalized, "validation"),
		strings.Contains(normalized, "must be"),
		strings.Contains(normalized, "missing"),
		strings.Contains(normalized, "duplicate"),
		strings.Contains(normalized, "nonnegative"):
		return "catalog response failed validation"
	default:
		return "catalog refresh failed"
	}
}

// boundByName returns the set of currently-bound listener names.
func (g *Gateway) boundByName() map[string]bool {
	out := map[string]bool{}
	for _, cl := range g.snapshotClients() {
		out[cl.name] = true
	}
	return out
}

func (g *Gateway) adminSecrets(w http.ResponseWriter, r *http.Request) {
	envPath, pathErr := g.adminEnvFilePath()
	if pathErr != nil {
		g.reject(w, 403, pathErr.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		f, err := config.Load(g.activeConfigPath())
		if err != nil {
			g.reject(w, 500, "load config: "+err.Error())
			return
		}
		names := f.CollectPlaceholders()
		out := make([]config.SecretEntry, 0, len(names))
		for _, n := range names {
			out = append(out, config.SecretEntry{
				Name:     n,
				Resolved: os.Getenv(n) != "",
			})
		}
		writeJSON(w, 200, out)
	case http.MethodPut:
		var body struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			g.reject(w, 400, "decode body: "+err.Error())
			return
		}
		env, _ := config.LoadEnvFile(envPath)
		env[body.Name] = body.Value
		if err := config.SaveEnvFile(envPath, env); err != nil {
			g.reject(w, 500, "save env: "+err.Error())
			return
		}
		os.Setenv(body.Name, body.Value)
		writeJSON(w, 200, map[string]any{"ok": true, "name": body.Name})
	default:
		g.reject(w, 405, "method not allowed")
	}
}

// adminEnvFilePath honors BASETEN_SWITCH_ENV_FILE for every runtime. Preview additionally
// requires the exact private env file beside its active config. This prevents a
// misconfigured or symlinked Preview admin endpoint from reading or replacing
// Stable's ~/.config/baseten-switch/env.
func (g *Gateway) adminEnvFilePath() (string, error) {
	path := config.EnvFilePath()
	if os.Getenv("BASETEN_SWITCH_PRIVATE_RUNTIME") != "1" {
		return path, nil
	}

	configPath, err := filepath.Abs(g.activeConfigPath())
	if err != nil {
		return "", fmt.Errorf("private runtime config path: %w", err)
	}
	root := filepath.Dir(configPath)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("private runtime root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", fmt.Errorf("private runtime root must be a non-symlink directory")
	}
	if rootInfo.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("private runtime root permissions are unsafe")
	}

	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("private runtime env file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("private runtime env file must be a regular non-symlink file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("private runtime env file permissions are unsafe")
	}

	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve private runtime root: %w", err)
	}
	pathReal, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve private runtime env file: %w", err)
	}
	expected := filepath.Join(rootReal, "env")
	if filepath.Clean(pathReal) != expected {
		return "", fmt.Errorf(
			"private runtime env file %s is outside the active runtime root",
			path)
	}
	return path, nil
}

func (g *Gateway) adminTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		g.reject(w, 405, "method not allowed")
		return
	}
	n := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 && x <= 5000 {
			n = x
		}
	}
	events, err := telemetry.ReadTailEvents(
		g.runtimeConfig().TelemetryDir,
		n,
		telemetry.DefaultTailReadMaxBytes,
	)
	if err != nil {
		w.Header().Set("X-Baseten-Switch-Telemetry-Partial", "true")
		w.Header().Set("Warning", `199 baseten-switch "telemetry history is partial"`)
	}
	if events == nil {
		events = []telemetry.EventV1{}
	}
	writeJSON(w, 200, events)
}

func (g *Gateway) handleSIGHUP() {
	pid, err := pidfile.ReadFrom(g.runtimeConfig().PidFile)
	if err != nil || pid <= 0 {
		return
	}
	if pid == os.Getpid() {
		// In-process: reload env file into our own os.Environ.
		loadDotEnv()
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Signal(syscall.SIGHUP)
}
