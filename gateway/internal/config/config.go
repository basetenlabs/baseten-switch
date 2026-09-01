package config

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type AuthToken struct {
	Header string `yaml:"header" json:"header"`
	Value  string `yaml:"value"  json:"value"`
}

// ResponsesCompatibilityMode controls one Responses API compatibility rule.
// It is a string on the wire so gateway.yaml remains operator-readable.
type ResponsesCompatibilityMode string

const (
	ResponsesCompatibilityModeOn  ResponsesCompatibilityMode = "on"
	ResponsesCompatibilityModeOff ResponsesCompatibilityMode = "off"
)

// ResponsesCompatibility is the optional gateway.yaml block for Responses
// API compatibility rules. A nil block is deliberately different from a
// present block: nil disables every optional rule, while a present block
// receives the documented per-rule defaults.
type ResponsesCompatibility struct {
	TextFormatDefault            ResponsesCompatibilityMode `yaml:"text_format_default,omitempty" json:"text_format_default,omitempty"`
	AdditionalToolsInput         ResponsesCompatibilityMode `yaml:"additional_tools_input,omitempty" json:"additional_tools_input,omitempty"`
	ReasoningEffort              ResponsesCompatibilityMode `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
	FunctionArgumentsConsistency ResponsesCompatibilityMode `yaml:"function_arguments_consistency,omitempty" json:"function_arguments_consistency,omitempty"`
}

// ResolvedResponsesCompatibility is the normalized immutable policy carried
// by a resolved client.
type ResolvedResponsesCompatibility struct {
	TextFormatDefault            ResponsesCompatibilityMode
	AdditionalToolsInput         ResponsesCompatibilityMode
	ReasoningEffort              ResponsesCompatibilityMode
	FunctionArgumentsConsistency ResponsesCompatibilityMode
}

// ResolveResponsesCompatibility applies the documented defaults and rejects
// invalid enum values. Configs without the block resolve to an all-off policy.
func ResolveResponsesCompatibility(raw *ResponsesCompatibility) (ResolvedResponsesCompatibility, error) {
	if raw == nil {
		return ResolvedResponsesCompatibility{
			TextFormatDefault:            ResponsesCompatibilityModeOff,
			AdditionalToolsInput:         ResponsesCompatibilityModeOff,
			ReasoningEffort:              ResponsesCompatibilityModeOff,
			FunctionArgumentsConsistency: ResponsesCompatibilityModeOff,
		}, nil
	}

	resolved := ResolvedResponsesCompatibility{
		TextFormatDefault:            defaultResponsesCompatibilityMode(raw.TextFormatDefault, ResponsesCompatibilityModeOn),
		AdditionalToolsInput:         defaultResponsesCompatibilityMode(raw.AdditionalToolsInput, ResponsesCompatibilityModeOff),
		ReasoningEffort:              defaultResponsesCompatibilityMode(raw.ReasoningEffort, ResponsesCompatibilityModeOn),
		FunctionArgumentsConsistency: defaultResponsesCompatibilityMode(raw.FunctionArgumentsConsistency, ResponsesCompatibilityModeOn),
	}
	for _, field := range []struct {
		name string
		mode ResponsesCompatibilityMode
	}{
		{"text_format_default", resolved.TextFormatDefault},
		{"additional_tools_input", resolved.AdditionalToolsInput},
		{"reasoning_effort", resolved.ReasoningEffort},
		{"function_arguments_consistency", resolved.FunctionArgumentsConsistency},
	} {
		if !validResponsesCompatibilityMode(field.mode) {
			return ResolvedResponsesCompatibility{}, fmt.Errorf("responses_compatibility.%s: invalid mode %q (allowed: on, off)", field.name, field.mode)
		}
	}
	return resolved, nil
}

func defaultResponsesCompatibilityMode(value, fallback ResponsesCompatibilityMode) ResponsesCompatibilityMode {
	if value == "" {
		return fallback
	}
	return value
}

func validResponsesCompatibilityMode(mode ResponsesCompatibilityMode) bool {
	switch mode {
	case ResponsesCompatibilityModeOn, ResponsesCompatibilityModeOff:
		return true
	default:
		return false
	}
}

type Client struct {
	Name          string     `yaml:"name"       json:"name"`
	Enabled       bool       `yaml:"enabled"   json:"enabled"`
	BindAddr      string     `yaml:"bind_addr"  json:"bind_addr"`
	ProtocolShape string     `yaml:"protocol_shape,omitempty" json:"protocol_shape,omitempty"`
	AuthToken     *AuthToken `yaml:"auth_token,omitempty" json:"auth_token,omitempty"`
	DefaultModel  string     `yaml:"default_model,omitempty" json:"default_model,omitempty"`
	// ModelAliases maps stable picker and request model ids to Baseten slugs.
	// The gateway also publishes them through Claude Code gateway discovery for
	// compatibility with older clients (the model-discovery contract).
	// Anthropic-shape clients only. Alias ids must begin with "claude"
	// or "anthropic" (the picker drops everything else before caching)
	// and must not shadow real Anthropic model names;
	// violations are config-load errors. While global routing is On, a
	// request naming an alias is an explicit Baseten choice. While Off,
	// the request fails locally without consulting Baseten.
	ModelAliases map[string]string `yaml:"model_aliases,omitempty" json:"model_aliases,omitempty"`
	// ModelPicker is the ordered set of configured aliases that Switch projects
	// into Claude Code's user-level modelPicker setting. It is presentation
	// state only: model_aliases remains the routing contract. A nil pointer means
	// the integration has not been configured; a present disabled or empty block
	// remains explicit saved intent.
	ModelPicker *ModelPicker `yaml:"model_picker,omitempty" json:"model_picker,omitempty"`
	// SubagentModel is the rewrite target for Claude Code sidechain
	// (subagent) requests on an anthropic-shape client: a gateway alias
	// (must exist in this client's model_aliases), a raw Baseten slug
	// (contains "/"), or a native claude-*/anthropic-* id. Empty means
	// no rewrite. See the subagent-routing contract.
	SubagentModel string `yaml:"subagent_model,omitempty" json:"subagent_model,omitempty"`
	// SubagentRouting toggles the subagent rewrite live: "on" or "off"
	// (strings; yaml.v3 does not resolve them as booleans). Absent means
	// on when SubagentModel is set, so the menubar can flip off without
	// losing the configured model. Enabled = SubagentModel non-empty and
	// SubagentRouting not "off". See the subagent-routing contract.
	SubagentRouting string `yaml:"subagent_routing,omitempty" json:"subagent_routing,omitempty"`
	// ModelRoutes pins per-family routing for an anthropic-shape client,
	// overriding the switch for the matched traffic. Keys are the bare
	// family words fable, opus, sonnet, and haiku; values are "native", a
	// gateway alias (must exist in model_aliases), or a raw Baseten slug
	// (contains "/"). See config/schema.md.
	ModelRoutes map[string]string `yaml:"model_routes,omitempty" json:"model_routes,omitempty"`
	// ModelOptions contains client-scoped provider/model behavior.
	ModelOptions ModelOptions `yaml:"model_options,omitempty" json:"model_options,omitempty"`
	// SanitizeHistory enables history repair for anthropic-shape upstreams
	// (strip empty text blocks, normalize tool_use ids). Default true;
	// tri-state so an absent key is distinguishable from explicit false.
	SanitizeHistory *bool `yaml:"sanitize_history,omitempty" json:"sanitize_history,omitempty"`
	// FallbackRoute is the optional protocol-native target for an eligible
	// fallback attempt after Baseten. It must be the native route for
	// protocol_shape.
	FallbackRoute string `yaml:"fallback_route,omitempty" json:"fallback_route,omitempty"`
	// NativeFallbackModel is the protocol-native model used only when an
	// Anthropic-shape request starts with a Baseten-specific identity that the
	// native provider cannot accept. Native-origin requests preserve their
	// exact ingress model instead. Empty means no catch-all native target.
	NativeFallbackModel string `yaml:"native_fallback_model,omitempty" json:"native_fallback_model,omitempty"`
	// UpstreamShape overrides the wire shape used toward the upstream on
	// the baseten route. Setting "openai" on an anthropic listener makes
	// the gateway translate /v1/messages traffic to /v1/chat/completions
	// (Claude Code on an openai-only Baseten model). Empty = listener shape.
	UpstreamShape string `yaml:"upstream_shape,omitempty" json:"upstream_shape,omitempty"`
	// ResponsesStripToolTypes lists tools[] entry types the gateway
	// strips from /v1/responses bodies before a baseten-route attempt
	// (codex emits tool_search, which inference.baseten.co rejects with
	// a 400; see the Responses compatibility contract). Openai-shape clients
	// only; the field on an anthropic-shape client is a config-load
	// error. The native fallback attempt keeps the original body. No
	// tool types are baked into the gateway; empty strips nothing.
	ResponsesStripToolTypes []string `yaml:"responses_strip_tool_types,omitempty" json:"responses_strip_tool_types,omitempty"`
	// ResponsesCompatibility configures Responses API request and stream
	// safeguards for Baseten attempts. A missing block disables every optional
	// normalization rule. OpenAI-shape clients only.
	ResponsesCompatibility *ResponsesCompatibility `yaml:"responses_compatibility,omitempty" json:"responses_compatibility,omitempty"`
	// TTFTTimeout overrides global.ttft_timeout for this harness: the
	// per-attempt time-to-first-byte deadline (Go duration syntax).
	// Empty inherits the global value; "0" disables the deadline even
	// when a global one is set.
	TTFTTimeout string `yaml:"ttft_timeout,omitempty" json:"ttft_timeout,omitempty"`
}

// ModelPicker is the Switch-owned desired projection for Claude Code's
// modelPicker setting.
type ModelPicker struct {
	Enabled bool               `yaml:"enabled" json:"enabled"`
	Models  []ModelPickerModel `yaml:"models" json:"models"`
}

// ModelPickerModel identifies one picker row by its stable model_aliases key.
type ModelPickerModel struct {
	Alias string `yaml:"alias" json:"alias"`
}

// DoorPort is one harness-facing front-door port. The door forwards to
// router_addr while the router is healthy and serves a native fallback
// when tripped. Fallback hosts are derived per path prefix from the
// protocol shapes of the clients whose bind_addr equals router_addr, so
// a single-port fleet gets a path-aware fallback without extra config.
type DoorPort struct {
	BindAddr   string `yaml:"bind_addr"   json:"bind_addr"`
	RouterAddr string `yaml:"router_addr" json:"router_addr"`
}

// Door configures the `baseten-switch door` front-door process. When present,
// the process derives its port map from this section instead of launch flags, so
// gateway.yaml stays the single source of truth for the whole request
// path. Durations use Go syntax ("15s").
type Door struct {
	Cooldown      string     `yaml:"cooldown,omitempty"       json:"cooldown,omitempty"`
	ProbeInterval string     `yaml:"probe_interval,omitempty" json:"probe_interval,omitempty"`
	Ports         []DoorPort `yaml:"ports,omitempty"          json:"ports,omitempty"`
}

// ReasoningMode is a stored provider/model reasoning policy. Passthrough and
// compatibility defaults are runtime states, not config values.
type ReasoningMode string

const (
	ReasoningOff           ReasoningMode = "off"
	ReasoningFollowHarness ReasoningMode = "follow_harness"
	ReasoningFixed         ReasoningMode = "fixed"
)

type ReasoningPolicy struct {
	Mode   ReasoningMode `yaml:"mode" json:"mode"`
	Effort string        `yaml:"effort,omitempty" json:"effort,omitempty"`
}

type ModelOption struct {
	Reasoning *ReasoningPolicy `yaml:"reasoning,omitempty" json:"reasoning,omitempty"`
}

// ModelOptions is provider -> canonical model id -> provider/model options.
// The provider level is retained so the config can grow without conflating
// capabilities exposed by different providers for the same model family.
type ModelOptions map[string]map[string]ModelOption

type Global struct {
	// RoutingEnabled is intentionally a pointer so validation can
	// distinguish an explicit false from an omitted required field.
	RoutingEnabled *bool `yaml:"routing_enabled,omitempty" json:"routing_enabled,omitempty"`
	// FallbackPolicy independently controls Baseten HTTP 429 and HTTP 5xx
	// transitions to a protocol-native fallback. Omitted fields default on.
	FallbackPolicy *FallbackPolicy   `yaml:"fallback_policy,omitempty" json:"fallback_policy,omitempty"`
	Auth           map[string]string `yaml:"auth" json:"auth"`
	TelemetryDir   string            `yaml:"telemetry_dir" json:"telemetry_dir"`
	// TelemetryEnabled is a pointer so an omitted value retains the
	// documented default of true while an explicit false pauses collection.
	TelemetryEnabled       *bool `yaml:"telemetry_enabled,omitempty" json:"telemetry_enabled,omitempty"`
	TelemetryRetentionDays int   `yaml:"telemetry_retention_days,omitempty" json:"telemetry_retention_days,omitempty"`
	// TraceCapture is a separate, high-sensitivity request and response body
	// store. It is disabled when omitted and never inherits telemetry settings.
	TraceCapture   *TraceCapture `yaml:"trace_capture,omitempty" json:"trace_capture,omitempty"`
	RetryMax       int           `yaml:"retry_max" json:"retry_max"`
	RequestTimeout string        `yaml:"request_timeout" json:"request_timeout"`
	// TTFTTimeout is the per-attempt time-to-first-byte deadline (Go
	// duration syntax). If the upstream sends no first response byte
	// within it, the gateway abandons the attempt and falls through to
	// the client's fallback_route. Empty or 0 disables it (the shipped
	// default). Per-client ttft_timeout overrides this value.
	TTFTTimeout string `yaml:"ttft_timeout,omitempty" json:"ttft_timeout,omitempty"`
}

// TraceCapture is the explicit local content-capture policy. Clients is an
// allowlist of enabled harness listeners, not a route or provider selector.
type TraceCapture struct {
	Enabled       bool     `yaml:"enabled" json:"enabled"`
	Clients       []string `yaml:"clients,omitempty" json:"clients,omitempty"`
	RetentionDays int      `yaml:"retention_days,omitempty" json:"retention_days,omitempty"`
}

// ResolvedTraceCapture is the normalized policy used by the gateway runtime.
type ResolvedTraceCapture struct {
	Enabled       bool
	Clients       []string
	RetentionDays int
}

const (
	DefaultTraceRetentionDays = 7
	MaxTraceRetentionDays     = 365
)

// ResolveTraceCapture validates the content-capture trust boundary and applies
// defaults. Disabled policies retain their allowlist so a later typed local
// activation can be explicit, but they capture no traffic.
func ResolveTraceCapture(f *File) (ResolvedTraceCapture, error) {
	resolved := ResolvedTraceCapture{RetentionDays: DefaultTraceRetentionDays}
	if f == nil || f.Global.TraceCapture == nil {
		return resolved, nil
	}
	raw := f.Global.TraceCapture
	resolved.Enabled = raw.Enabled
	resolved.RetentionDays = raw.RetentionDays
	if resolved.RetentionDays == 0 {
		resolved.RetentionDays = DefaultTraceRetentionDays
	}
	if resolved.RetentionDays < 1 || resolved.RetentionDays > MaxTraceRetentionDays {
		return ResolvedTraceCapture{}, fmt.Errorf(
			"routing policy: global.trace_capture.retention_days must be between 1 and %d",
			MaxTraceRetentionDays,
		)
	}

	clientByName := make(map[string]Client, len(f.Clients))
	for _, client := range f.Clients {
		clientByName[client.Name] = client
	}
	seen := make(map[string]struct{}, len(raw.Clients))
	for _, name := range raw.Clients {
		name = strings.TrimSpace(name)
		if name == "" {
			return ResolvedTraceCapture{}, fmt.Errorf("routing policy: global.trace_capture.clients contains an empty name")
		}
		if _, ok := seen[name]; ok {
			return ResolvedTraceCapture{}, fmt.Errorf("routing policy: global.trace_capture.clients contains duplicate %q", name)
		}
		seen[name] = struct{}{}
		resolved.Clients = append(resolved.Clients, name)
	}
	if !resolved.Enabled {
		return resolved, nil
	}
	if len(resolved.Clients) == 0 {
		return ResolvedTraceCapture{}, fmt.Errorf("routing policy: enabled global.trace_capture requires at least one client")
	}

	selectedListeners := make(map[string]struct{}, len(resolved.Clients))
	for _, name := range resolved.Clients {
		client, ok := clientByName[name]
		if !ok || !client.Enabled {
			return ResolvedTraceCapture{}, fmt.Errorf("routing policy: trace capture client %q must name an enabled client", name)
		}
		if !isLoopbackBindAddr(client.BindAddr) {
			return ResolvedTraceCapture{}, fmt.Errorf(
				"routing policy: trace capture client %q bind_addr %q must be loopback",
				name,
				client.BindAddr,
			)
		}
		selectedListeners[normalizedLoopbackEndpoint(client.BindAddr)] = struct{}{}
	}
	if f.Door != nil {
		for _, port := range f.Door.Ports {
			if _, selected := selectedListeners[normalizedLoopbackEndpoint(port.RouterAddr)]; !selected {
				continue
			}
			if !isLoopbackBindAddr(port.BindAddr) {
				return ResolvedTraceCapture{}, fmt.Errorf(
					"routing policy: door bind_addr %q targeting trace-captured listener %q must be loopback",
					port.BindAddr,
					port.RouterAddr,
				)
			}
		}
	}
	return resolved, nil
}

func normalizedLoopbackEndpoint(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return "loopback:" + port
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return "loopback:" + port
	}
	return net.JoinHostPort(host, port)
}

func isLoopbackBindAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsTelemetryEnabled returns the effective telemetry collection setting.
// Collection defaults to enabled when the field is absent.
func IsTelemetryEnabled(global Global) bool {
	return global.TelemetryEnabled == nil || *global.TelemetryEnabled
}

const DefaultTelemetryRetentionDays = 90

// TelemetryRetentionDays returns the configured retention window, defaulting
// omitted and nonpositive values to the Stable 90-day policy.
func TelemetryRetentionDays(global Global) int {
	if global.TelemetryRetentionDays <= 0 {
		return DefaultTelemetryRetentionDays
	}
	return global.TelemetryRetentionDays
}

func DefaultTelemetryDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "baseten-switch", "telemetry")
}

type File struct {
	Global  Global   `yaml:"global"  json:"global"`
	Clients []Client `yaml:"clients" json:"clients"`
	Door    *Door    `yaml:"door,omitempty" json:"door,omitempty"`
}

// NativeRoute returns the switch-OFF route for a protocol shape: the
// shape's native provider (anthropic shape -> anthropic, openai shape
// -> openai).
func NativeRoute(shape string) string {
	if shape == "openai" {
		return "openai"
	}
	return "anthropic"
}

// ValidModelRouteKey reports whether key is one of the four supported
// Claude model-family route keys.
func ValidModelRouteKey(key string) bool {
	switch key {
	case "fable", "opus", "sonnet", "haiku":
		return true
	default:
		return false
	}
}

// ValidateRoutingPolicy rejects incomplete or unsafe routing files before
// the gateway activates them. The clean schema has one required global
// gate; provider-valued route fields and policy-version sentinels are not
// part of the schema.
func ValidateRoutingPolicy(f *File) error {
	if f == nil {
		return fmt.Errorf("routing policy: nil config")
	}
	if f.Global.RoutingEnabled == nil {
		return fmt.Errorf("routing policy: global.routing_enabled must be explicitly true or false")
	}
	if f.Global.TelemetryRetentionDays < 0 {
		return fmt.Errorf("routing policy: global.telemetry_retention_days must be positive when set")
	}
	if _, err := ResolveTraceCapture(f); err != nil {
		return err
	}
	for _, c := range f.Clients {
		if c.ProtocolShape != "anthropic" && c.ProtocolShape != "openai" {
			return fmt.Errorf(
				"routing policy: client %q protocol_shape must be explicitly anthropic or openai",
				c.Name,
			)
		}
		if err := validateModelOptions(
			c.ModelOptions,
			fmt.Sprintf("clients[%q].model_options", c.Name),
		); err != nil {
			return err
		}
		if err := validateModelPicker(c); err != nil {
			return err
		}
		if c.ResponsesCompatibility != nil && c.ProtocolShape != "openai" {
			return fmt.Errorf("client %q: responses_compatibility requires protocol_shape openai (got %q)", c.Name, c.ProtocolShape)
		}
		if _, err := ResolveResponsesCompatibility(c.ResponsesCompatibility); err != nil {
			return fmt.Errorf("client %q: %w", c.Name, err)
		}
		for key := range c.ModelRoutes {
			if !ValidModelRouteKey(key) {
				return fmt.Errorf("client %q: model_routes key %q is invalid (allowed: fable, opus, sonnet, haiku)", c.Name, key)
			}
		}
		if c.FallbackRoute != "" && c.FallbackRoute != NativeRoute(c.ProtocolShape) {
			return fmt.Errorf("routing policy: client %q fallback_route %q must be its native route %q", c.Name, c.FallbackRoute, NativeRoute(c.ProtocolShape))
		}
		if err := ValidateNativeFallbackModel(c); err != nil {
			return err
		}
		if !c.Enabled {
			continue
		}
		target := c.DefaultModel
		if target == "" || !strings.Contains(target, "/") {
			return fmt.Errorf("routing policy: enabled client %q requires a Baseten default_model target", c.Name)
		}
	}
	return nil
}

func validateModelPicker(c Client) error {
	if c.ModelPicker == nil {
		return nil
	}
	if c.ProtocolShape != "anthropic" {
		return fmt.Errorf(
			"client %q: model_picker requires protocol_shape anthropic (got %q)",
			c.Name,
			c.ProtocolShape,
		)
	}
	seen := make(map[string]struct{}, len(c.ModelPicker.Models))
	for i, model := range c.ModelPicker.Models {
		alias := strings.TrimSpace(model.Alias)
		if alias == "" {
			return fmt.Errorf(
				"client %q: model_picker.models[%d].alias must not be empty",
				c.Name,
				i,
			)
		}
		if alias != model.Alias {
			return fmt.Errorf(
				"client %q: model_picker.models[%d].alias %q must not contain surrounding whitespace",
				c.Name,
				i,
				model.Alias,
			)
		}
		if _, ok := c.ModelAliases[alias]; !ok {
			return fmt.Errorf(
				"client %q: model_picker alias %q is missing from model_aliases",
				c.Name,
				alias,
			)
		}
		if _, duplicate := seen[alias]; duplicate {
			return fmt.Errorf(
				"client %q: model_picker alias %q is duplicated",
				c.Name,
				alias,
			)
		}
		seen[alias] = struct{}{}
	}
	return nil
}

func validateModelOptions(
	options ModelOptions,
	path string,
) error {
	providers := make([]string, 0, len(options))
	for provider := range options {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		if provider != "baseten" {
			return fmt.Errorf(
				"routing policy: %s provider %q is unsupported (allowed: baseten)",
				path,
				provider,
			)
		}
		models := options[provider]
		modelIDs := make([]string, 0, len(models))
		for modelID := range models {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, modelID := range modelIDs {
			if strings.TrimSpace(modelID) == "" {
				return fmt.Errorf(
					"routing policy: %s.%s contains an empty model id",
					path,
					provider,
				)
			}
			option := models[modelID]
			if option.Reasoning == nil {
				return fmt.Errorf(
					"routing policy: %s.%s.%s.reasoning is required",
					path,
					provider,
					modelID,
				)
			}
			if err := ValidateReasoningPolicy(*option.Reasoning); err != nil {
				return fmt.Errorf(
					"routing policy: %s.%s.%s %w",
					path,
					provider,
					modelID,
					err,
				)
			}
		}
	}
	return nil
}

// ValidateReasoningPolicy validates the offline structural contract shared by
// gateway.yaml, admin preflight, and typed mutation callers.
func ValidateReasoningPolicy(policy ReasoningPolicy) error {
	switch policy.Mode {
	case ReasoningOff, ReasoningFollowHarness:
		if policy.Effort != "" {
			return fmt.Errorf(
				"reasoning mode %q forbids effort",
				policy.Mode,
			)
		}
	case ReasoningFixed:
		if strings.TrimSpace(policy.Effort) == "" {
			return fmt.Errorf(
				"reasoning mode %q requires effort",
				policy.Mode,
			)
		}
	default:
		return fmt.Errorf(
			"reasoning mode %q is invalid",
			policy.Mode,
		)
	}
	return nil
}

var placeholderRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func Expand(s string) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}

func ExpandPath(s string) string {
	s = Expand(s)
	if strings.HasPrefix(s, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			if s == "~" {
				return home
			}
			if strings.HasPrefix(s, "~/") {
				return filepath.Join(home, s[2:])
			}
		}
	}
	return s
}

func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := UnmarshalStrict(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &f, nil
}

// UnmarshalStrict decodes one YAML document and rejects unknown struct
// fields. This keeps removed schema fields from becoming silent no-ops.
func UnmarshalStrict(data []byte, out any) error {
	if _, ok := out.(*File); ok {
		if err := validateRawModelPickerNodes(data); err != nil {
			return err
		}
		if err := validateRawFallbackPolicyNodes(data); err != nil {
			return err
		}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple YAML documents are not supported")
		}
		return err
	}
	return nil
}

// validateRawFallbackPolicyNodes preserves omission versus explicit null for
// the optional block and its pointer-boolean children. yaml.v3 otherwise
// decodes both states to nil, which would silently turn malformed saved intent
// into the default-on policy.
func validateRawFallbackPolicyNodes(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil
	}
	if err := validateRawNativeFallbackModelNodes(doc.Content[0]); err != nil {
		return err
	}
	_, global := mappingEntry(doc.Content[0], "global")
	if global == nil || global.Kind != yaml.MappingNode {
		return nil
	}
	_, policy := mappingEntry(global, "fallback_policy")
	if policy == nil {
		return nil
	}
	if policy.Kind == yaml.ScalarNode && policy.Tag == "!!null" {
		return fmt.Errorf("global.fallback_policy must be an object, not null")
	}
	if policy.Kind != yaml.MappingNode {
		return nil
	}
	for _, key := range []string{"on_baseten_429", "on_baseten_5xx"} {
		_, value := mappingEntry(policy, key)
		if value != nil && value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
			return fmt.Errorf("global.fallback_policy.%s must be true or false, not null", key)
		}
	}
	return nil
}

func validateRawNativeFallbackModelNodes(root *yaml.Node) error {
	_, clients := mappingEntry(root, "clients")
	if clients == nil || clients.Kind != yaml.SequenceNode {
		return nil
	}
	for _, client := range clients.Content {
		if client.Kind != yaml.MappingNode {
			continue
		}
		_, nameNode := mappingEntry(client, "name")
		name := "<unnamed>"
		if nameNode != nil && nameNode.Kind == yaml.ScalarNode {
			name = nameNode.Value
		}
		_, value := mappingEntry(client, "native_fallback_model")
		if value == nil {
			continue
		}
		if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
			return fmt.Errorf("client %q native_fallback_model must be a full native model ID, not null", name)
		}
		if value.Kind == yaml.ScalarNode && strings.TrimSpace(value.Value) == "" {
			return fmt.Errorf("client %q native_fallback_model must be a full native model ID, not empty", name)
		}
	}
	return nil
}

// validateRawModelPickerNodes preserves the distinction between an absent
// model_picker and an explicitly null value. yaml.v3 otherwise decodes both
// into a nil *ModelPicker, which would turn malformed saved intent into a
// silent no-op.
func validateRawModelPickerNodes(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil
	}
	_, clients := mappingEntry(doc.Content[0], "clients")
	if clients == nil || clients.Kind != yaml.SequenceNode {
		return nil
	}
	for i, client := range clients.Content {
		_, picker := mappingEntry(client, "model_picker")
		if picker == nil {
			continue
		}
		if picker.Kind == yaml.ScalarNode && picker.Tag == "!!null" {
			return fmt.Errorf("clients[%d].model_picker must be an object, not null", i)
		}
	}
	return nil
}

// Save marshals f and rewrites path atomically (0600). Marshaling the
// parsed struct REGENERATES the file: comments, blank lines, ordering,
// and alignment are all lost. Only callers that legitimately replace
// the whole document may use it. Targeted changes to an existing file must go
// through the comment-preserving editors in edit.go (SetClientRoutes)
// instead.
func Save(path string, f *File) error {
	b, err := Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return writeFileAtomic(path, b, 0o600)
}

// Marshal renders the canonical whole-document representation used by Save.
// Whole-document callers use it to validate and hash the complete replacement
// before performing an exact-byte compare-and-swap.
func Marshal(f *File) ([]byte, error) {
	b, err := yaml.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return b, nil
}

// MissingConfigMessage is the shared hard-refusal text for a missing
// config file. The gateway startup and the up/down/status/restart
// lifecycle commands both use it so the user sees one message with one
// fix everywhere (the lifecycle contract, no-config landmine).
func MissingConfigMessage(path string) string {
	return fmt.Sprintf("no gateway config at %s. Fix: run 'baseten-switch config init' to generate the default config there (reference: config/gateway.example.yaml and config/schema.md in the baseten-switch repo), or set BASETEN_SWITCH_CONFIG_PATH to an existing config", path)
}

// MalformedConfigMessage is the shared hard-refusal text for a config
// file that exists but does not load.
func MalformedConfigMessage(path string, err error) string {
	return fmt.Sprintf("gateway config %s is malformed: %v. Fix: repair the file (reference: config/schema.md and config/gateway.example.yaml in the baseten-switch repo)", path, err)
}

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "baseten-switch", "gateway.yaml")
}

func EnvFilePath() string {
	if path := os.Getenv("BASETEN_SWITCH_ENV_FILE"); path != "" {
		return path
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "baseten-switch", "env")
}

func (f *File) CollectPlaceholders() []string {
	seen := map[string]bool{}
	add := func(s string) {
		for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
			seen[m[1]] = true
		}
	}
	for _, v := range f.Global.Auth {
		add(v)
	}
	add(f.Global.TelemetryDir)
	for _, c := range f.Clients {
		// A disabled client binds no listener and resolves nothing
		// (the gateway skips it before validation), so its ${VAR}
		// references are inert: warning about them would tell every
		// fresh install to set variables the parked template codex
		// client does not need until 'codex on' (which writes the
		// stub itself).
		if !c.Enabled {
			continue
		}
		if c.AuthToken != nil {
			add(c.AuthToken.Value)
		}
		add(c.DefaultModel)
		for _, v := range c.ModelAliases {
			add(v)
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type SecretEntry struct {
	Name     string `json:"name"`
	Resolved bool   `json:"resolved"`
}

func LoadEnvFile(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, sc.Err()
}

func SaveEnvFile(path string, env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\n')
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".env.*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
