package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/analytics"
	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/proxy"
	"github.com/basetenlabs/baseten-switch/gateway/internal/reasoning"
	"github.com/basetenlabs/baseten-switch/gateway/internal/requestprofile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/responsescompat"
	"github.com/basetenlabs/baseten-switch/gateway/internal/sanitize"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/translate"
	"github.com/basetenlabs/baseten-switch/gateway/internal/usage"
)

const (
	DefaultPort         = 45273
	DefaultBasetenURL   = "https://inference.baseten.co"
	DefaultAnthropicURL = "https://api.anthropic.com"
	DefaultOpenAIURL    = "https://api.openai.com"
	DefaultAdminAddr    = "127.0.0.1:45273"
	// CodexCompatibilityModel is emitted by the managed Codex profile. It
	// deliberately remains unknown to Codex so the CLI keeps the reduced
	// request shape validated against Baseten. It is a routing sentinel, not
	// an upstream model: global routing On resolves it through default_model,
	// and it is never sent to a native OpenAI fallback.
	CodexCompatibilityModel = "baseten-switch-compat-v1"
	// subagentAgentIDHeader is the Claude Code sidechain identity header.
	// Main-thread requests omit it; sidechain (subagent) requests carry
	// it. The gateway gates the subagent rewrite on its presence. See
	// the subagent-routing contract.
	subagentAgentIDHeader = "x-claude-code-agent-id"
)

type Config struct {
	TelemetryDir           string
	TelemetryEnabled       *bool
	TelemetryRetentionDays int
	PidFile                string
	BasetenURL             string
	AnthropicURL           string
	OpenAIURL              string
	BasetenKey             string
	OAuthProfile           string
	OAuthHost              string
	APIKeyFallback         bool
	AdminAddr              string
	ConfigPath             string
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func homeJoin(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		all := append([]string{os.TempDir()}, parts...)
		return filepath.Join(all...)
	}
	all := append([]string{home}, parts...)
	return filepath.Join(all...)
}

func LoadConfig() Config {
	loadDotEnv()
	pf := pidfile.Path()
	oauthProfile := os.Getenv("BASETEN_SWITCH_OAUTH_PROFILE")
	oauthHost := auth.DefaultHost()
	apiKeyFallback := false
	switch strings.ToLower(os.Getenv("BASETEN_SWITCH_API_KEY_FALLBACK")) {
	case "1", "true", "yes":
		apiKeyFallback = true
	}
	telemetryEnabled := true
	return Config{
		TelemetryDir:           config.DefaultTelemetryDir(),
		TelemetryEnabled:       &telemetryEnabled,
		TelemetryRetentionDays: config.DefaultTelemetryRetentionDays,
		PidFile:                pf,
		BasetenURL:             env("BASETEN_BASE_URL", DefaultBasetenURL),
		AnthropicURL:           env("ANTHROPIC_API_BASE_URL", DefaultAnthropicURL),
		OpenAIURL:              env("OPENAI_BASE_URL", DefaultOpenAIURL),
		BasetenKey:             os.Getenv("BASETEN_API_KEY"),
		OAuthProfile:           oauthProfile,
		OAuthHost:              oauthHost,
		APIKeyFallback:         apiKeyFallback,
		AdminAddr:              env("BASETEN_SWITCH_ADMIN_ADDR", DefaultAdminAddr),
		ConfigPath:             env("BASETEN_SWITCH_CONFIG_PATH", config.DefaultPath()),
	}
}

func loadDotEnv() {
	path := env("BASETEN_SWITCH_ENV_FILE", homeJoin(".config", "baseten-switch", "env"))
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

// resolvedClientConfig is the per-listener resolved state, computed at
// startup or SIGHUP from gateway.yaml. It is immutable for the lifetime
// of the listener it produced.
type resolvedClientConfig struct {
	Name          string
	BindAddr      string
	ProtocolShape string // anthropic | openai
	Route         string // baseten | anthropic | openai | monitor
	// GlobalRoutingEnabled carries the sole config routing gate into the
	// immutable live resolver. Route is the derived effective base route:
	// Baseten while On, and the protocol's native provider while Off.
	// HasGlobalRoutingGate is true for every client resolved from the
	// clean gateway.yaml schema. Direct in-process embedders may provide
	// an already-resolved Route without a backing config gate.
	HasGlobalRoutingGate bool
	GlobalRoutingEnabled bool
	DefaultModel         string
	ModelAliases         map[string]string // anthropic shape only; alias id -> baseten slug
	// SubagentModel is the rewrite target for sidechain (subagent)
	// requests carrying x-claude-code-agent-id: a gateway alias, a raw
	// Baseten slug, or a native claude-*/anthropic-* id. Empty = no
	// rewrite. Anthropic shape only. See the subagent-routing contract.
	SubagentModel string
	// SubagentRouting is the live toggle: "on" or "off". Absent means on
	// when SubagentModel is set. See the subagent-routing contract.
	SubagentRouting string
	// ModelRoutes pins per-family routing for an anthropic-shape client,
	// overriding the switch for matched traffic. Keys are the bare family
	// words fable, opus, sonnet, and haiku; values are "native", a gateway
	// alias (must exist in model_aliases), or a raw Baseten slug (contains
	// "/"). Empty map means no pins. See config/schema.md.
	ModelRoutes map[string]string
	// ModelOptions is the immutable, client-scoped provider/model reasoning
	// policy for this listener generation.
	ModelOptions    config.ModelOptions
	SanitizeHistory bool
	FallbackRoute   string // "" = no fallback
	UpstreamShape   string // baseten route only; "" = listener shape
	// ResponsesStripToolTypes lists tools[] entry types stripped from
	// /v1/responses bodies on baseten-route attempts. Openai shape
	// only; nil/empty = no strip. Config order from gateway.yaml is
	// preserved. See the Responses compatibility contract.
	ResponsesStripToolTypes []string
	// ResponsesCompatibility is the normalized Responses safeguards policy.
	// An absent gateway.yaml block resolves to all rules off and zero TTL.
	ResponsesCompatibility config.ResolvedResponsesCompatibility
	TTFTTimeout            time.Duration // per-attempt first-byte deadline; 0 = disabled
}

func cloneModelOptions(options config.ModelOptions) config.ModelOptions {
	if len(options) == 0 {
		return nil
	}
	out := make(config.ModelOptions, len(options))
	for provider, models := range options {
		clonedModels := make(map[string]config.ModelOption, len(models))
		for model, option := range models {
			cloned := option
			if option.Reasoning != nil {
				reasoning := *option.Reasoning
				cloned.Reasoning = &reasoning
			}
			clonedModels[model] = cloned
		}
		out[provider] = clonedModels
	}
	return out
}

// hash returns a stable content hash over the fields that should
// trigger a listener respawn on SIGHUP: bind_addr, protocol_shape,
// route, sanitize_history, fallback_route, upstream_shape,
// ttft_timeout, default_model, model_aliases, subagent_model, subagent_routing,
// model_routes, responses_strip_tool_types, responses_compatibility, and the
// global routing gate.
// Listener identity (name) is compared separately when diffing the set.
func (r resolvedClientConfig) hash() string {
	h := sha256.New()
	fmt.Fprintln(h, r.BindAddr)
	fmt.Fprintln(h, r.ProtocolShape)
	fmt.Fprintln(h, r.Route)
	fmt.Fprintln(h, r.HasGlobalRoutingGate)
	fmt.Fprintln(h, r.GlobalRoutingEnabled)
	fmt.Fprintln(h, r.SanitizeHistory)
	fmt.Fprintln(h, r.FallbackRoute)
	fmt.Fprintln(h, r.UpstreamShape)
	fmt.Fprintln(h, r.TTFTTimeout)
	fmt.Fprintln(h, r.DefaultModel)
	keys := make([]string, 0, len(r.ModelAliases))
	for k := range r.ModelAliases {
		keys = append(keys, k)
	}
	for _, k := range sortedKeys(keys) {
		fmt.Fprintf(h, "%s=%s\n", k, r.ModelAliases[k])
	}
	fmt.Fprintln(h, r.SubagentModel)
	fmt.Fprintln(h, r.SubagentRouting)
	keys = keys[:0]
	for k := range r.ModelRoutes {
		keys = append(keys, k)
	}
	for _, k := range sortedKeys(keys) {
		fmt.Fprintf(h, "%s=%s\n", k, r.ModelRoutes[k])
	}
	providers := make([]string, 0, len(r.ModelOptions))
	for provider := range r.ModelOptions {
		providers = append(providers, provider)
	}
	for _, provider := range sortedKeys(providers) {
		models := r.ModelOptions[provider]
		modelIDs := make([]string, 0, len(models))
		for modelID := range models {
			modelIDs = append(modelIDs, modelID)
		}
		for _, modelID := range sortedKeys(modelIDs) {
			option := models[modelID]
			if option.Reasoning == nil {
				fmt.Fprintf(h, "model_option:%s:%s=<nil>\n", provider, modelID)
				continue
			}
			fmt.Fprintf(
				h,
				"model_option:%s:%s=%s:%s\n",
				provider,
				modelID,
				option.Reasoning.Mode,
				option.Reasoning.Effort,
			)
		}
	}
	// Deliberately order-sensitive (unlike the sorted maps above): the
	// list keeps its gateway.yaml order, so a pure reorder also respawns
	// the listener. A spurious respawn is safe; a missed one silently
	// breaks hot-reload.
	fmt.Fprintln(h, strings.Join(r.ResponsesStripToolTypes, ","))
	fmt.Fprintln(h, r.ResponsesCompatibility.TextFormatDefault)
	fmt.Fprintln(h, r.ResponsesCompatibility.AdditionalToolsInput)
	fmt.Fprintln(h, r.ResponsesCompatibility.ReasoningEffort)
	fmt.Fprintln(h, r.ResponsesCompatibility.FunctionArgumentsConsistency)
	return hex.EncodeToString(h.Sum(nil))
}

// subagentEnabled reports whether the subagent rewrite gate should fire:
// a target is configured and the toggle is not off.
func (r resolvedClientConfig) subagentEnabled() bool {
	return r.SubagentModel != "" && r.SubagentRouting != "off"
}

func (r resolvedClientConfig) usesGlobalRouting() bool {
	return r.HasGlobalRoutingGate && r.Route != "monitor"
}

func (r resolvedClientConfig) globalRoutingOff() bool {
	return r.usesGlobalRouting() && !r.GlobalRoutingEnabled
}

func sortedKeys(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// clientListener is the per-request client context: one resolved
// client plus the listener group serving its bind_addr. Clients
// sharing a bind_addr point at the same group.
type clientListener struct {
	name                   string
	cfg                    resolvedClientConfig
	group                  *listenerGroup
	responsesCompatibility *responsesCompatibilityState
}

func (cl *clientListener) Addr() net.Addr {
	if cl.group != nil && cl.group.listener != nil {
		return cl.group.listener.Addr()
	}
	return nil
}

// listenerGroup owns one bound address and its HTTP server. A group
// serves every client configured at that bind_addr; shared addresses
// resolve the owning client per request from the path.
type listenerGroup struct {
	key       string // identity for SIGHUP diffing: addr, or addr+name for port-0 binds
	addr      string
	clientsMu sync.RWMutex
	clients   []*clientListener
	listener  net.Listener
	server    *http.Server
	// acceptingStopped is set before intentionally closing listener during a
	// topology commit. It lets Serve distinguish that closure from an
	// unexpected listener failure. retired is published under routingMu with
	// the new active snapshot; requests already selected before publication
	// may finish, while later requests on an accepted keep-alive connection
	// fail closed instead of using the stale resolver.
	acceptingStopped atomic.Bool
	retired          atomic.Bool
}

func (lg *listenerGroup) displayName() string {
	clients := lg.snapshotClients()
	names := make([]string, 0, len(clients))
	for _, cl := range clients {
		names = append(names, cl.name)
	}
	return strings.Join(names, "+")
}

func (lg *listenerGroup) clientConfigs() []resolvedClientConfig {
	clients := lg.snapshotClients()
	out := make([]resolvedClientConfig, 0, len(clients))
	for _, cl := range clients {
		out = append(out, cl.cfg)
	}
	return out
}

func (lg *listenerGroup) snapshotClients() []*clientListener {
	lg.clientsMu.RLock()
	defer lg.clientsMu.RUnlock()
	return append([]*clientListener(nil), lg.clients...)
}

// replaceClientConfigs publishes one immutable per-request resolver slice.
// A request that already selected a client keeps its old immutable pointer;
// the next request observes the complete replacement.
func (lg *listenerGroup) replaceClientConfigs(cfgs []resolvedClientConfig) []*clientListener {
	clients := make([]*clientListener, 0, len(cfgs))
	for _, rc := range cfgs {
		clients = append(clients, &clientListener{
			name:                   rc.Name,
			cfg:                    rc,
			group:                  lg,
			responsesCompatibility: newResponsesCompatibilityState(rc.ResponsesCompatibility),
		})
	}
	lg.clientsMu.Lock()
	lg.clients = clients
	lg.clientsMu.Unlock()
	return clients
}

func (lg *listenerGroup) stopAccepting() {
	lg.acceptingStopped.Store(true)
	_ = lg.listener.Close()
}

func (lg *listenerGroup) serve() error {
	err := lg.server.Serve(lg.listener)
	if lg.acceptingStopped.Load() {
		return http.ErrServerClosed
	}
	return err
}

// groupSpec is a desired listener group before binding.
type groupSpec struct {
	key  string
	addr string
	cfgs []resolvedClientConfig
}

func (s groupSpec) displayName() string {
	names := make([]string, 0, len(s.cfgs))
	for _, rc := range s.cfgs {
		names = append(names, rc.Name)
	}
	return strings.Join(names, "+")
}

// bindAddrSharable reports whether an address can host more than one
// client. Port-0 binds get a kernel-assigned port per listener, so
// two port-0 clients can never actually share a socket.
func bindAddrSharable(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && port != "0"
}

// groupResolved groups resolved clients by bind_addr, preserving
// config order for groups and for clients within a group.
func groupResolved(resolved []resolvedClientConfig) []groupSpec {
	specs := []groupSpec{}
	index := map[string]int{}
	for _, rc := range resolved {
		key := rc.BindAddr
		if !bindAddrSharable(rc.BindAddr) {
			key = rc.BindAddr + "\x00" + rc.Name
		}
		if i, ok := index[key]; ok {
			specs[i].cfgs = append(specs[i].cfgs, rc)
			continue
		}
		index[key] = len(specs)
		specs = append(specs, groupSpec{key: key, addr: rc.BindAddr, cfgs: []resolvedClientConfig{rc}})
	}
	return specs
}

// groupContentHash covers every client on the address (name plus the
// per-client hash), order-independent, so a change to any member
// respawns the shared listener on SIGHUP.
func groupContentHash(cfgs []resolvedClientConfig) string {
	parts := make([]string, 0, len(cfgs))
	for _, rc := range cfgs {
		parts = append(parts, rc.Name+"\x00"+rc.hash())
	}
	h := sha256.New()
	for _, p := range sortedKeys(parts) {
		fmt.Fprintln(h, p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type Gateway struct {
	cfgMu   sync.RWMutex
	cfg     Config
	pricing *pricing.Pricing
	client  *http.Client
	// catalogRefresh owns the asynchronous Baseten /v1/models refresh
	// lifecycle. Pricing snapshots themselves live in g.pricing.
	catalogRefresh       *catalogRefreshManager
	publicCatalogRefresh *publicCatalogRefreshManager
	catalogCacheMu       sync.Mutex

	analyticsMu      sync.Mutex
	analyticsIndex   *analytics.Index
	analyticsCacheMu sync.Mutex
	analyticsCache   []analyticsResponseCacheEntry

	// telemetryMu owns the lazy v1 writer and its configuration identity.
	// Disabling collection or changing the store settings on reload closes
	// the current writer. Re-enabling opens the next writer only when a
	// request event is ready to persist.
	telemetryMu            sync.Mutex
	telemetryWriter        telemetry.EventWriter
	telemetryWriterDir     string
	telemetryRetentionDays int
	telemetryLastHealth    telemetry.WriterHealth
	telemetryLastDir       string
	telemetryStopped       bool

	// Admin listener: lives for the entire gateway lifetime; never
	// re-bound on SIGHUP.
	adminListener net.Listener
	adminServer   *http.Server

	// Per-client request contexts keyed by client name, plus the
	// listener groups (one per bound address) that serve them.
	mu      sync.Mutex
	clients map[string]*clientListener
	groups  map[string]*listenerGroup
	// clientOrder preserves the accepted configuration order for status and
	// deterministic diagnostics. It is guarded by mu with clients/groups.
	clientOrder []string
	// routingMu makes publication of listener resolver slices and the
	// active config token one atomic observation for request selection and
	// admin status. reloadMu serializes SIGHUP and admin-triggered reloads.
	routingMu sync.RWMutex
	reloadMu  sync.Mutex

	// Shared auth state across all baseten-routed listeners.
	oauthClient     *http.Client
	oauthProfileErr error
	// profileAPIKey holds the readable key of an api_key-type baseten
	// CLI profile ('baseten auth login' with an API key). It is a
	// first-class credential: served after OAuth and before the
	// BASETEN_API_KEY env fallback. Guarded by authMu; profileAPIKeyName
	// is the resolved profile name for status surfaces.
	profileAPIKey     string
	profileAPIKeyName string
	// Credential-health state (all guarded by authMu). signed_in has
	// always meant "a credential exists in the store"; these fields
	// track whether that credential actually WORKS, fed by
	// noteAuthRefresh on every token-refresh round trip
	// (auth.HTTPClientWithNotify). authDead is set when the token
	// endpoint rejects the grant (e.g. invalid_grant: refresh token
	// expired, revoked, or lost a rotation race) and cleared on a
	// successful refresh or when refreshAuth sees a different stored
	// refresh token than the one that died (re-login detection, by
	// fingerprint; the token value itself is never kept here).
	authLastOKAt  time.Time
	authLastErr   string
	authLastErrAt time.Time
	authDead      bool
	// authCredFP fingerprints the refresh token of the CURRENT client
	// lineage: set from the same store read that built the client
	// (refreshAuth) and advanced on every successful refresh, because
	// rotation moves the lineage's token without a client rebuild and a
	// later death must be recorded against the token that actually died.
	authCredFP string
	authDeadFP string // authCredFP at the moment the credential went dead
	// authGen counts client builds. Refresh outcomes carry the
	// generation they were built under; a refresh in flight across a
	// refreshAuth swap must not mark the new credential dead (or heal
	// it) when it completes.
	authGen int
	// authTick performs one token acquisition through the same
	// oauth2.ReuseTokenSource oauthClient uses (nil when signed out or
	// on an API-key profile). Guarded by authMu like oauthClient, but
	// only ever CALLED outside it: a tick can be a token-endpoint round
	// trip and its outcome re-enters noteAuthRefresh.
	authTick func() error
	authMu   sync.Mutex
	// Tick goroutine lifecycle. authTickStop is closed by Shutdown:
	// callers may Shutdown without cancelling Serve's ctx, and wg.Wait
	// would otherwise block on the tick goroutine until the deadline.
	// authTickDone closes when the goroutine exits; tests assert on it
	// to prove shutdown does not leak the goroutine. authTickKick
	// (1-buffered) re-arms the loop's timer when the credential flips
	// dead mid-arm, so the tight dead cadence engages immediately
	// instead of after the remainder of a healthy-interval wait.
	authTickStop     chan struct{}
	authTickStopOnce sync.Once
	authTickDone     chan struct{}
	authTickKick     chan struct{}

	emailMu        sync.Mutex
	emailCached    string
	emailFetchedAt time.Time

	// Fallback cooldown state, keyed by listener name: until this
	// deadline, requests go straight to the fallback route without
	// re-trying the tripped primary.
	fallbackMu    sync.Mutex
	fallbackUntil map[string]time.Time

	// Active routing snapshot. Admin status reads this exact accepted
	// configuration instead of independently parsing gateway.yaml.
	stateMu          sync.RWMutex
	routerBootID     string
	activeGeneration uint64
	activeConfigHash string
	activeConfigFile *config.File
	reloadError      string

	wg  sync.WaitGroup
	ctx context.Context
}

// runtimeConfig returns one coherent copy of process-wide runtime settings.
// Per-client routing lives in immutable clientListener snapshots; this
// separate snapshot covers upstream URLs, credentials, auth settings, paths,
// and telemetry settings that reloadConfig may replace while requests run.
func (g *Gateway) runtimeConfig() Config {
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	return g.cfg
}

func (g *Gateway) setRuntimeConfig(cfg Config) {
	g.cfgMu.Lock()
	g.cfg = cfg
	g.cfgMu.Unlock()
}

// New constructs a Gateway with an always-on admin listener plus one
// HTTP listener per bind address; clients sharing a BindAddr are
// served by one listener. A bind failure for any listener aborts the
// whole constructor (and closes any already-opened listeners).
func New(cfg Config, p *pricing.Pricing, adminListener net.Listener, resolved []resolvedClientConfig) (*Gateway, error) {
	var snapshot *resolvedConfigSnapshot
	if cfg.ConfigPath != "" {
		cfgForLoad := cfg
		if loaded, err := loadResolvedConfigSnapshot(&cfgForLoad); err == nil &&
			sameResolvedClients(loaded.clients, resolved) {
			snapshot = loaded
		}
	}
	return newGatewayWithSnapshot(cfg, p, adminListener, resolved, snapshot)
}

func newGatewayWithSnapshot(
	cfg Config,
	p *pricing.Pricing,
	adminListener net.Listener,
	resolved []resolvedClientConfig,
	snapshot *resolvedConfigSnapshot,
) (*Gateway, error) {
	g := &Gateway{
		cfg:                  cfg,
		pricing:              p,
		analyticsIndex:       analytics.NewIndex(analytics.IndexOptions{}),
		client:               &http.Client{Timeout: 0, Transport: defaultTransport()},
		adminListener:        adminListener,
		clients:              map[string]*clientListener{},
		groups:               map[string]*listenerGroup{},
		authTickStop:         make(chan struct{}),
		authTickDone:         make(chan struct{}),
		authTickKick:         make(chan struct{}, 1),
		catalogRefresh:       newCatalogRefreshManager(),
		publicCatalogRefresh: newPublicCatalogRefreshManager(),
		routerBootID:         newRouterBootID(),
	}
	// Admin mux: admin endpoints + /healthz + / only.
	adminMux := http.NewServeMux()
	g.registerAdmin(adminMux)
	adminMux.HandleFunc("/", g.adminRoot)
	g.adminServer = &http.Server{
		Handler:           adminMux,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	prepared := make([]*listenerGroup, 0, len(resolved))
	for _, spec := range groupResolved(resolved) {
		lg, err := g.prepareGroup(spec)
		if err != nil {
			for _, candidate := range prepared {
				_ = candidate.listener.Close()
			}
			if g.adminServer != nil {
				_ = g.adminServer.Close()
			}
			return nil, fmt.Errorf("bind client %q at %s: %w", spec.displayName(), spec.addr, err)
		}
		prepared = append(prepared, lg)
	}
	g.mu.Lock()
	for _, lg := range prepared {
		g.groups[lg.key] = lg
		for _, cl := range lg.snapshotClients() {
			g.clients[cl.name] = cl
		}
	}
	g.clientOrder = g.clientOrder[:0]
	for _, rc := range resolved {
		g.clientOrder = append(g.clientOrder, rc.Name)
	}
	g.mu.Unlock()
	if snapshot != nil {
		g.activateConfigSnapshot(snapshot.file, snapshot.raw)
	}
	g.refreshAuth()
	return g, nil
}

func sameResolvedClients(a, b []resolvedClientConfig) bool {
	return len(a) == len(b) && groupContentHash(a) == groupContentHash(b)
}

func newRouterBootID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// This identifier is an ordering token, not a secret. A
		// time-based fallback remains process-unique enough if the OS
		// random source is unexpectedly unavailable.
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", os.Getpid(), time.Now().UnixNano())))
		copy(b[:], sum[:16])
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return strings.Join([]string{
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	}, "-")
}

func exactConfigHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func readConfigSnapshot(path string) (*config.File, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var f config.File
	if err := config.UnmarshalStrict(raw, &f); err != nil {
		return nil, raw, err
	}
	return &f, raw, nil
}

type resolvedConfigSnapshot struct {
	file    *config.File
	raw     []byte
	clients []resolvedClientConfig
}

func (g *Gateway) activateConfigSnapshot(f *config.File, raw []byte) {
	var immutable config.File
	if err := config.UnmarshalStrict(raw, &immutable); err != nil {
		// The caller already parsed these exact bytes. Keep a defensive
		// fallback clone rather than retaining its potentially mutable
		// pointer if an impossible second parse fails.
		immutable = *f
	}
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	g.activeConfigFile = &immutable
	g.activeConfigHash = exactConfigHash(raw)
	g.activeGeneration++
	g.reloadError = ""
}

func (g *Gateway) noteReloadError(err error) {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	g.reloadError = err.Error()
}

func (g *Gateway) prepareGroup(spec groupSpec) (*listenerGroup, error) {
	l, err := net.Listen("tcp", spec.addr)
	if err != nil {
		return nil, err
	}
	lg := &listenerGroup{
		key:      spec.key,
		addr:     spec.addr,
		listener: l,
	}
	lg.replaceClientConfigs(spec.cfgs)
	mux := http.NewServeMux()
	mux.HandleFunc("/", g.groupHandle(lg))
	lg.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return lg, nil
}

func (g *Gateway) bindGroup(spec groupSpec) error {
	lg, err := g.prepareGroup(spec)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.groups[spec.key] = lg
	for _, cl := range lg.snapshotClients() {
		g.clients[cl.name] = cl
	}
	g.mu.Unlock()
	return nil
}

func (g *Gateway) shutdownAllClients() {
	g.mu.Lock()
	groups := g.groups
	g.groups = map[string]*listenerGroup{}
	g.clients = map[string]*clientListener{}
	g.clientOrder = nil
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, lg := range groups {
		_ = lg.server.Shutdown(ctx)
		lg.listener.Close()
	}
}

func (g *Gateway) refreshAuth() {
	cfg := g.runtimeConfig()
	g.authMu.Lock()
	defer g.authMu.Unlock()
	// Each build is a new lineage: outcomes from clients built before
	// this swap are stale and noteAuthRefresh drops them by generation.
	g.authGen++
	gen := g.authGen
	notify := func(fp string, err error) { g.noteAuthRefresh(gen, fp, err) }
	client, tick, credFP, err := auth.HTTPClientWithNotify(context.Background(), cfg.OAuthProfile, cfg.OAuthHost, notify)
	switch {
	case err == nil:
		g.oauthClient = client
		g.authTick = tick
		g.oauthProfileErr = nil
		g.profileAPIKey = ""
		g.profileAPIKeyName = ""
	case errors.Is(err, auth.ErrNotSignedIn):
		g.oauthClient = nil
		g.authTick = nil
		g.oauthProfileErr = err
		g.profileAPIKey = ""
		g.profileAPIKeyName = ""
	case errors.Is(err, auth.ErrAPIKeyProfile):
		// The baseten CLI profile authenticates with an API key, not
		// OAuth. A readable key is a first-class credential ('baseten
		// auth login' with an API key was the explicit opt-in), served
		// after OAuth and before the BASETEN_API_KEY env fallback.
		g.oauthClient = nil
		g.authTick = nil
		g.oauthProfileErr = err
		g.profileAPIKey = ""
		g.profileAPIKeyName = ""
		var ak *auth.APIKeyProfileError
		if errors.As(err, &ak) {
			g.profileAPIKey = ak.Key
			g.profileAPIKeyName = ak.Profile
		}
	default:
		g.oauthClient = nil
		g.authTick = nil
		g.oauthProfileErr = err
		g.profileAPIKey = ""
		g.profileAPIKeyName = ""
		fmt.Fprintf(os.Stderr, "[gateway] auth refresh failed: %v\n", err)
	}
	// Re-login detection: if the credential was marked dead but the
	// store now holds a DIFFERENT refresh token than the one that died
	// (the user ran 'baseten auth login' again), clear the dead state so
	// health recovers on reload instead of lingering until the next
	// successful refresh (which could be up to an access-token lifetime
	// away). Compared by fingerprint; token values are never retained.
	// credFP comes from the SAME store read that built the client: a
	// login landing between two separate reads would pin the fingerprint
	// to a credential this client was never built from, permanently
	// hiding the re-login from the dead-tick store watch.
	g.authCredFP = credFP
	if g.authDead && credFP != g.authDeadFP {
		g.authDead = false
		g.authDeadFP = ""
		g.authLastErr = ""
		g.authLastErrAt = time.Time{}
	}
	fmt.Fprintf(os.Stderr, "[gateway] auth: profile=%s signed_in=%t health=%s fallback=%t\n",
		cfg.OAuthProfile, g.oauthClient != nil, g.authHealthLocked(), cfg.APIKeyFallback)
	g.kickCatalogRefresh()
}

// noteAuthRefresh records the outcome of one OAuth token-refresh round
// trip (auth.CachingTokenSource.Notify). A token-endpoint rejection
// (invalid_grant and friends) marks the credential dead: the store still
// HAS a credential (signed_in stays true) but every baseten-routed
// request will fail until the user re-authenticates. Transient errors
// (network, timeout) record last_error without marking dead. gen is the
// client generation the outcome was built under: a refresh in flight
// across a refreshAuth swap completes against the OLD credential, and
// letting it mark the new lineage dead (with the new fingerprint) would
// disarm both the dead-tick store watch and the re-login clear right
// after a successful reauth. fp identifies the refresh token the outcome
// belongs to (see auth.CachingTokenSource); on success it advances
// authCredFP so rotation keeps the lineage fingerprint truthful. Runs on
// request goroutines; must stay fast.
func (g *Gateway) noteAuthRefresh(gen int, fp string, err error) {
	g.authMu.Lock()
	defer g.authMu.Unlock()
	if gen != g.authGen {
		return
	}
	now := time.Now()
	if err == nil {
		g.authLastOKAt = now
		g.authLastErr = ""
		g.authLastErrAt = time.Time{}
		g.authDead = false
		g.authDeadFP = ""
		if fp != "" {
			g.authCredFP = fp
		}
		return
	}
	g.authLastErr = err.Error()
	g.authLastErrAt = now
	if code := auth.RefreshErrorCode(err); code != "" {
		if !g.authDead {
			fmt.Fprintf(os.Stderr,
				"[gateway] auth: token refresh rejected (%s); the stored Baseten credential is dead and baseten-routed requests will fail. Fix: 'baseten auth login', then SIGHUP the gateway (or 'baseten-switch up')\n",
				code)
			// Nudge the tick loop onto the dead cadence now: the timer
			// may be mid-way through a healthy-interval arm, and the
			// store watch must engage within authTickDeadInterval (the
			// ~30s self-heal auth login promises).
			select {
			case g.authTickKick <- struct{}{}:
			default:
			}
		}
		g.authDead = true
		g.authDeadFP = fp
		if fp == "" {
			g.authDeadFP = g.authCredFP
		}
	}
}

// Background auth-tick cadences. Package vars so tests can shrink them.
// The normal tick rides oauth2.ReuseTokenSource, so real refresh traffic
// stays at roughly one token-endpoint round trip per access-token lifetime
// (~1h) regardless of the tick rate. The dead cadence only re-reads the
// local credential store (no network), so it can run much tighter to pick
// up a re-login quickly.
var (
	authTickInterval     = 5 * time.Minute
	authTickDeadInterval = 30 * time.Second
)

// runAuthTick keeps credential health truthful on an idle gateway and
// self-heals after a re-login without SIGHUP (the credential-refresh contract,
// "Gateway self-heal"). Without it, a credential that dies overnight is
// only discovered by the next harness request. Exits on Serve ctx cancel
// or Shutdown (whichever first); authTickDone closes on exit.
func (g *Gateway) runAuthTick(ctx context.Context) {
	defer g.wg.Done()
	defer close(g.authTickDone)
	timer := time.NewTimer(g.authTickPeriod())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.authTickStop:
			return
		case <-g.authTickKick:
			// A request-path refresh failure flipped the credential dead
			// mid-arm; re-arm on the dead cadence instead of waiting out
			// the remainder of a healthy-interval arm (the failure that
			// kicked was itself a refresh outcome, so no tick here).
			timer.Reset(g.authTickPeriod())
			continue
		case <-timer.C:
		}
		g.authTickOnce()
		timer.Reset(g.authTickPeriod())
	}
}

// authTickPeriod picks the wait before the next tick: tight while the
// credential is dead or absent (the tick is then a local store read and
// recovery latency is what the user feels after a login), relaxed
// otherwise.
func (g *Gateway) authTickPeriod() time.Duration {
	g.authMu.Lock()
	defer g.authMu.Unlock()
	if g.authDead || errors.Is(g.oauthProfileErr, auth.ErrNotSignedIn) ||
		errors.Is(g.oauthProfileErr, auth.ErrAPIKeyProfile) {
		// All three states tick with a local store read only (no
		// network), so the tight interval costs nothing and keeps
		// login/rotation pickup fast.
		return authTickDeadInterval
	}
	return authTickInterval
}

// authTickOnce performs one background tick. State is snapshotted under
// authMu and acted on outside it: the healthy-path tick can hit the token
// endpoint (and re-enters noteAuthRefresh), and the self-heal path calls
// refreshAuth, which takes authMu itself.
func (g *Gateway) authTickOnce() {
	cfg := g.runtimeConfig()
	g.authMu.Lock()
	dead := g.authDead
	deadFP := g.authDeadFP
	tick := g.authTick
	profile := cfg.OAuthProfile
	signedOut := errors.Is(g.oauthProfileErr, auth.ErrNotSignedIn)
	apiKeyProfile := errors.Is(g.oauthProfileErr, auth.ErrAPIKeyProfile)
	curProfileKey := g.profileAPIKey
	g.authMu.Unlock()
	if dead {
		// A dead credential never comes back on its own; replaying the
		// refresh would only burn token-endpoint calls. Watch the store
		// (local read, no network) for a refresh token that differs from
		// the one that died: that is a re-login, so rebuild the client
		// through the same path SIGHUP uses.
		tok, _, err := auth.Load(profile)
		if err != nil || tok == nil {
			return
		}
		if auth.CredFingerprint(tok.RefreshToken) == deadFP {
			return
		}
		fmt.Fprintf(os.Stderr, "[gateway] auth: stored credential changed after refresh failure; reloading credential (re-login detected, no SIGHUP needed)\n")
		g.refreshAuth()
		return
	}
	if signedOut {
		// A FIRST login is otherwise invisible: with no credential at
		// boot there is no client and no tick to notice one appearing,
		// and auth login's SIGHUP fallback promises the router picks it
		// up on its own. Local store read, no network. A login can also
		// land as an api_key-type profile (auth.Load then returns
		// *APIKeyProfileError, not a token): that too is a credential
		// appearing, so reload for it as well.
		tok, _, err := auth.Load(profile)
		if err != nil {
			var ak *auth.APIKeyProfileError
			if errors.As(err, &ak) && ak.Key != "" {
				fmt.Fprintf(os.Stderr, "[gateway] auth: API-key profile appeared in the store; loading it (login detected, no SIGHUP needed)\n")
				g.refreshAuth()
			}
			return
		}
		if tok == nil {
			return
		}
		fmt.Fprintf(os.Stderr, "[gateway] auth: credential appeared in the store; loading it (login detected, no SIGHUP needed)\n")
		g.refreshAuth()
		return
	}
	if apiKeyProfile {
		// Serving with an api_key-type profile's key. Watch the store
		// (local read, no network) for a change: a switch to OAuth, a
		// rotated key, or a logout. Reload through the same path SIGHUP
		// uses so the served credential tracks the store.
		if _, _, err := auth.Load(profile); err != nil {
			var ak *auth.APIKeyProfileError
			if errors.As(err, &ak) && ak.Key == curProfileKey {
				return
			}
			fmt.Fprintf(os.Stderr, "[gateway] auth: stored API-key profile changed; reloading credential (no SIGHUP needed)\n")
			g.refreshAuth()
			return
		}
		// err == nil: the store now holds an OAuth credential (or
		// nothing at all after a logout); either way this key is stale.
		fmt.Fprintf(os.Stderr, "[gateway] auth: stored credential changed from API-key profile; reloading (no SIGHUP needed)\n")
		g.refreshAuth()
		return
	}
	if tick == nil {
		return
	}
	// Rides oauth2.ReuseTokenSource: the cached access token is returned
	// while valid; only a near-expiry token triggers a real refresh, whose
	// outcome reaches noteAuthRefresh via Notify.
	_ = tick()
}

// authHealthLocked derives the health enum. Caller holds authMu.
//   - signed_out:     no credential in the store
//   - refresh_failed: credential present but the token endpoint rejected
//     it (dead until re-login)
//   - error:          last refresh attempt failed transiently
//   - ok:             credential present, no known problem (an api_key-type
//     profile's key counts: it has no refresh lifecycle to fail)
func (g *Gateway) authHealthLocked() string {
	switch {
	case g.oauthClient == nil && g.profileAPIKey != "":
		return "ok"
	case g.oauthClient == nil:
		return "signed_out"
	case g.authDead:
		return "refresh_failed"
	case g.authLastErr != "":
		return "error"
	default:
		return "ok"
	}
}

// authHealthState is the snapshot the admin handlers render.
type authHealthState struct {
	Health      string
	LastError   string
	LastErrorAt time.Time
	LastOKAt    time.Time
}

func (g *Gateway) authHealth() authHealthState {
	g.authMu.Lock()
	defer g.authMu.Unlock()
	return authHealthState{
		Health:      g.authHealthLocked(),
		LastError:   g.authLastErr,
		LastErrorAt: g.authLastErrAt,
		LastOKAt:    g.authLastOKAt,
	}
}

// rfc3339OrEmpty renders a timestamp for JSON: "" for the zero value.
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (g *Gateway) authState() (signedIn bool, fallbackInUse bool) {
	cfg := g.runtimeConfig()
	g.authMu.Lock()
	defer g.authMu.Unlock()
	signedIn = g.oauthClient != nil
	if !signedIn && g.profileAPIKey == "" && cfg.APIKeyFallback && cfg.BasetenKey != "" {
		fallbackInUse = true
	}
	return
}

// authMode names the credential the baseten route would serve with right
// now: "oauth", "api_key_profile" (api_key-type CLI profile),
// "api_key_env" (BASETEN_API_KEY + BASETEN_SWITCH_API_KEY_FALLBACK), or
// "none". Mirrors basetenAuthClient's priority order.
func (g *Gateway) authMode() (mode string, profileName string) {
	cfg := g.runtimeConfig()
	g.authMu.Lock()
	defer g.authMu.Unlock()
	switch {
	case g.oauthClient != nil:
		return "oauth", ""
	case g.profileAPIKey != "":
		return "api_key_profile", g.profileAPIKeyName
	case cfg.APIKeyFallback && cfg.BasetenKey != "":
		return "api_key_env", ""
	}
	return "none", ""
}

// basetenAuthClient picks the credential for a baseten-routed request:
// OAuth when signed in, else the api_key-type CLI profile's key, else the
// env fallback key. apiKey is non-empty exactly when the request must
// carry "Authorization: Api-Key <apiKey>". client == nil means no
// credential at all.
func (g *Gateway) basetenAuthClient() (useOAuth bool, client *http.Client, apiKey string) {
	cfg := g.runtimeConfig()
	g.authMu.Lock()
	defer g.authMu.Unlock()
	if g.oauthClient != nil {
		return true, g.oauthClient, ""
	}
	if g.profileAPIKey != "" {
		return false, g.client, g.profileAPIKey
	}
	if cfg.APIKeyFallback && cfg.BasetenKey != "" {
		return false, g.client, cfg.BasetenKey
	}
	return false, nil, ""
}

func defaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 16
	return t
}

// AdminAddr returns the admin listener's address.
func (g *Gateway) AdminAddr() net.Addr {
	if g.adminListener != nil {
		return g.adminListener.Addr()
	}
	return nil
}

// ClientAddr returns the bound address of the named per-client
// listener (or nil if not currently bound).
func (g *Gateway) ClientAddr(name string) net.Addr {
	g.routingMu.RLock()
	defer g.routingMu.RUnlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if cl, ok := g.clients[name]; ok {
		return cl.Addr()
	}
	return nil
}

// groupHandle returns the http.HandlerFunc bound to one listener
// group. It resolves the owning client per request, then runs the
// existing per-client pipeline unchanged.
func (g *Gateway) groupHandle(lg *listenerGroup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.routingMu.RLock()
		retired := lg.retired.Load()
		cl := (*clientListener)(nil)
		if !retired {
			cl = lg.resolveClient(r)
		}
		g.routingMu.RUnlock()
		if retired {
			proxy.DrainBody(r)
			g.reject(w, http.StatusServiceUnavailable, "listener retired during configuration reload")
			return
		}
		if cl == nil {
			proxy.DrainBody(r)
			g.reject(w, 404, "not found: "+r.URL.Path)
			return
		}
		g.handleClient(cl, w, r)
	}
}

// resolveClient picks the client that owns a request. Single-client
// addresses always resolve to their one client, so per-address
// behavior is unchanged; shared addresses dispatch by the disjoint
// path sets the two shapes own. / and /healthz are client-agnostic;
// /v1/models is shape-ambiguous and follows the anthropic-version
// header (Claude Code always sends it).
func (lg *listenerGroup) resolveClient(r *http.Request) *clientListener {
	clients := lg.snapshotClients()
	if len(clients) == 1 {
		return clients[0]
	}
	byShape := func(shape string) *clientListener {
		for _, cl := range clients {
			if cl.cfg.ProtocolShape == shape {
				return cl
			}
		}
		return nil
	}
	switch r.URL.Path {
	case "/", "/healthz":
		if len(clients) > 0 {
			return clients[0]
		}
		return nil
	case "/v1/messages", "/v1/messages/count_tokens":
		return byShape("anthropic")
	case "/v1/chat/completions", "/v1/responses":
		return byShape("openai")
	case "/v1/models":
		want := "openai"
		if r.Header.Get("anthropic-version") != "" {
			want = "anthropic"
		}
		if cl := byShape(want); cl != nil {
			return cl
		}
		if len(clients) > 0 {
			return clients[0]
		}
		return nil
	}
	return nil
}

func (g *Gateway) handleClient(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	shape := cl.cfg.ProtocolShape
	switch r.Method {
	case http.MethodGet:
		switch path {
		case "/", "/healthz":
			g.healthzClient(cl, w, r)
			return
		case "/v1/models":
			g.wg.Add(1)
			defer g.wg.Done()
			g.forwardModelsGet(cl, w, r)
			return
		default:
			proxy.DrainBody(r)
			g.reject(w, 404, "not found: "+r.URL.Path)
			return
		}
	case http.MethodHead:
		proxy.DrainBody(r)
		if path == "/" || path == "/healthz" {
			h := w.Header()
			h.Set("Content-Type", "application/json")
			h.Set("Content-Length", "0")
			w.WriteHeader(200)
			return
		}
		g.reject(w, 404, "not found: "+r.URL.Path)
		return
	case http.MethodPost:
		g.wg.Add(1)
		defer g.wg.Done()
		switch shape {
		case "anthropic":
			switch path {
			case "/v1/messages":
				g.forwardMessages(cl, w, r)
				return
			case "/v1/messages/count_tokens":
				g.countTokens(cl, w, r)
				return
			default:
				proxy.DrainBody(r)
				g.reject(w, 404, "not found: "+r.URL.Path)
				return
			}
		case "openai":
			switch path {
			case "/v1/chat/completions":
				g.forwardChatCompletions(cl, w, r)
				return
			case "/v1/responses":
				g.forwardResponses(cl, w, r)
				return
			default:
				proxy.DrainBody(r)
				g.reject(w, 404, "not found: "+r.URL.Path)
				return
			}
		default:
			proxy.DrainBody(r)
			g.reject(w, 404, "unknown protocol_shape: "+shape)
			return
		}
	default:
		proxy.DrainBody(r)
		g.reject(w, 405, "method not allowed: "+r.Method)
		return
	}
}

// shapeCompatible reports whether the (protocol_shape, route) pair
// is a same-shape native combination. Cross-shape pairs (openai
// port + anthropic route and vice versa) are 501.
// shapeCompatible reports whether a listener shape can serve a route,
// either natively or via translation. anthropic-listener -> openai-route
// is served by cross-shape translation (WS2); the reverse direction is
// still rejected.
func shapeCompatible(shape, rt string) bool {
	switch {
	case shape == "anthropic" && (rt == "baseten" || rt == "anthropic" || rt == "monitor" || rt == "openai"):
		return true
	case shape == "openai" && (rt == "baseten" || rt == "openai" || rt == "monitor"):
		return true
	}
	return false
}

// upstreamShapeFor resolves the wire shape used toward the upstream for
// one attempted route: native routes fix the shape, baseten follows the
// listener unless upstream_shape overrides it.
func upstreamShapeFor(rc resolvedClientConfig, rt string) string {
	switch rt {
	case "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	case "baseten":
		if rc.UpstreamShape != "" {
			return rc.UpstreamShape
		}
	}
	return rc.ProtocolShape
}

func (g *Gateway) rejectCrossShape(w http.ResponseWriter) {
	body, _ := json.Marshal(map[string]string{"error": "cross-shape translation not implemented"})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(body)))
	w.WriteHeader(501)
	_, _ = w.Write(body)
}

func (g *Gateway) reject(w http.ResponseWriter, code int, msg string) {
	body, _ := json.Marshal(map[string]string{"error": msg})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(body)))
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (g *Gateway) rejectNeedsLogin(w http.ResponseWriter) {
	w.Header().Set("X-Baseten-Switch", "needs-login")
	g.reject(w, 503, "not signed in; run 'baseten auth login'")
}

func (g *Gateway) adminRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/healthz" {
		g.reject(w, 404, "not found: "+r.URL.Path)
		return
	}
	g.adminHealthz(w, r)
}

func (g *Gateway) countTokens(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		g.reject(w, 400, "could not read request body")
		return
	}
	n := usage.SynthCountTokens(body)
	out, _ := json.Marshal(map[string]int{"input_tokens": n})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(out)))
	w.WriteHeader(200)
	_, _ = w.Write(out)
}

func (g *Gateway) healthzClient(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	rt := cl.cfg.Route
	upstreamModel := g.upstreamModelFor(cl.cfg)
	signedIn, _ := g.authState()
	body, _ := json.Marshal(map[string]any{
		"status":          "ok",
		"client":          cl.cfg.Name,
		"protocol_shape":  cl.cfg.ProtocolShape,
		"effective_route": rt,
		"upstream_model":  upstreamModel,
		"signed_in":       signedIn,
	})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(body)))
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// upstreamModelFor resolves the slug a baseten-routed request should
// be rewritten to. For non-baseten routes it returns "" (meaning
// passthrough: the upstream model = the requested model). For
// baseten it returns the per-listener default model.
func (g *Gateway) upstreamModelFor(rc resolvedClientConfig) string {
	if rc.Route != "baseten" {
		return ""
	}
	return rc.DefaultModel
}

func (g *Gateway) forwardModelsGet(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	cfg := g.runtimeConfig()
	if cl.cfg.ProtocolShape == "anthropic" && len(cl.cfg.ModelAliases) > 0 {
		g.aliasModelsGet(cl, w, r)
		return
	}
	rt := cl.cfg.Route
	if !shapeCompatible(cl.cfg.ProtocolShape, rt) {
		g.rejectCrossShape(w)
		return
	}
	var upstream string
	if rt == "baseten" {
		upstream = cfg.BasetenURL
	} else if rt == "anthropic" {
		upstream = cfg.AnthropicURL
	} else if rt == "openai" {
		upstream = cfg.OpenAIURL
	} else {
		// monitor: synthesize a tiny model list, no upstream.
		g.monitorModels(cl, w, r)
		return
	}
	url := strings.TrimRight(upstream, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		g.reject(w, 500, "could not build upstream request")
		return
	}
	req.Header.Set("Accept", "application/json")
	var upClient *http.Client
	if rt == "baseten" {
		useOAuth, c, apiKey := g.basetenAuthClient()
		if useOAuth {
			upClient = c
		} else if apiKey != "" {
			upClient = c
			req.Header.Set("Authorization", "Api-Key "+apiKey)
		} else {
			g.rejectNeedsLogin(w)
			return
		}
	} else {
		upClient = g.client
		if v := r.Header.Get("Authorization"); v != "" {
			req.Header.Set("Authorization", v)
		}
		if v := r.Header.Get("X-Api-Key"); v != "" {
			req.Header.Set("X-Api-Key", v)
		}
	}
	resp, err := upClient.Do(req)
	if err != nil {
		g.reject(w, 502, "upstream models call failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	h := w.Header()
	proxy.CopyHeader(h, resp.Header)
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json")
	}
	h.Set("Content-Length", itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// aliasModelCreatedAt is a constant so the synthesized list is
// byte-stable across restarts: Claude Code only rewrites its picker
// cache (~/.claude/cache/gateway-models.json) when the list changes.
const aliasModelCreatedAt = "2026-07-07T00:00:00Z"

// aliasModelEntries synthesizes the /v1/models entries for a client's
// model_aliases in the Anthropic list-entry shape, sorted by alias id
// for stable ordering. display_name is populated but the picker does
// not render it (validated 2026-07-07): the alias id IS the UX.
func aliasModelEntries(aliases map[string]string) []map[string]any {
	ids := make([]string, 0, len(aliases))
	for id := range aliases {
		ids = append(ids, id)
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range sortedKeys(ids) {
		slug := aliases[id]
		short := slug
		if i := strings.LastIndex(slug, "/"); i >= 0 {
			short = slug[i+1:]
		}
		out = append(out, map[string]any{
			"type":         "model",
			"id":           id,
			"display_name": short + " via Baseten",
			"created_at":   aliasModelCreatedAt,
		})
	}
	return out
}

// catalogAnthropicModelEntries projects catalog-ready provider-public models
// into Anthropic's list-entry shape. Account availability can remain unknown:
// this list is discovery metadata, while the provider still enforces access on
// inference. Exact standard pricing is required so Baseten never publishes a
// picker entry that its Traffic view cannot price.
func (g *Gateway) catalogAnthropicModelEntries() []map[string]any {
	records := g.pricing.Capture().Models(pricing.ProviderAnthropic)
	out := make([]map[string]any, 0, len(records))
	for _, record := range records {
		if !discoveryPrefixOK(record.CanonicalModelID) {
			continue
		}
		if _, priced := record.Prices[pricing.ProfileStandard]; !priced {
			continue
		}
		out = append(out, map[string]any{
			"type":         "model",
			"id":           record.CanonicalModelID,
			"display_name": record.DisplayName,
			"created_at":   aliasModelCreatedAt,
		})
	}
	return out
}

// aliasModelsGet serves GET /v1/models locally for anthropic-shape
// listeners with model_aliases configured (the model-discovery contract).
// Alias entries are synthesized without any upstream call: Claude
// Code's discovery timeout is ~3s and the list must never hang on
// Baseten or Anthropic availability. On the native anthropic route
// (switch OFF) the native list is additionally proxied with a short
// deadline and appended after the aliases, keeping the native escape
// hatch visible; a failed native fetch degrades to aliases-only
// rather than failing discovery.
func (g *Gateway) aliasModelsGet(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	entries := make([]any, 0, len(cl.cfg.ModelAliases))
	seen := map[string]bool{}
	for _, e := range aliasModelEntries(cl.cfg.ModelAliases) {
		entries = append(entries, e)
		if id, ok := e["id"].(string); ok {
			seen[id] = true
		}
	}
	for _, e := range g.catalogAnthropicModelEntries() {
		if id, ok := e["id"].(string); ok && seen[id] {
			continue
		}
		entries = append(entries, e)
		if id, ok := e["id"].(string); ok {
			seen[id] = true
		}
	}
	if cl.cfg.Route == "anthropic" {
		for _, e := range g.nativeModelEntries(r) {
			if id, ok := e["id"].(string); ok && seen[id] {
				continue
			}
			entries = append(entries, e)
		}
	}
	entries, hasMore, paginationErr := paginateAnthropicModelEntries(
		entries,
		r.URL.Query(),
	)
	if paginationErr != nil {
		g.reject(w, http.StatusBadRequest, paginationErr.Error())
		return
	}
	var firstID, lastID any
	if len(entries) > 0 {
		firstID = entries[0].(map[string]any)["id"]
		lastID = entries[len(entries)-1].(map[string]any)["id"]
	}
	body, _ := json.Marshal(map[string]any{
		"data":     entries,
		"has_more": hasMore,
		"first_id": firstID,
		"last_id":  lastID,
	})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(body)))
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

func paginateAnthropicModelEntries(
	entries []any,
	query url.Values,
) ([]any, bool, error) {
	afterID := strings.TrimSpace(query.Get("after_id"))
	beforeID := strings.TrimSpace(query.Get("before_id"))
	if afterID != "" && beforeID != "" {
		return nil, false, errors.New(
			"model discovery accepts only one of after_id or before_id",
		)
	}
	start, end := 0, len(entries)
	find := func(target string) int {
		for index, value := range entries {
			entry, _ := value.(map[string]any)
			id, _ := entry["id"].(string)
			if id == target {
				return index
			}
		}
		return -1
	}
	if afterID != "" {
		index := find(afterID)
		if index < 0 {
			return []any{}, false, nil
		}
		start = index + 1
	}
	if beforeID != "" {
		index := find(beforeID)
		if index < 0 {
			return []any{}, false, nil
		}
		end = index
	}
	if start > end {
		return []any{}, false, nil
	}
	limit := end - start
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return nil, false, errors.New(
				"model discovery limit must be a positive integer",
			)
		}
		if parsed < limit {
			limit = parsed
		}
	}
	pageEnd := start + limit
	return entries[start:pageEnd], pageEnd < end, nil
}

// nativeModelEntries fetches the Anthropic model list with the harness's own
// passthrough credential and a 2s deadline (inside the picker's ~3s discovery
// budget). A complete valid page also refreshes normalized account-availability
// metadata and its private runtime cache. Credentials and raw provider
// responses are never persisted. Any failure returns nil: the caller has
// already projected last-known-good priced catalog entries, so discovery never
// loses those entries when the authenticated fetch fails.
func (g *Gateway) nativeModelEntries(r *http.Request) []map[string]any {
	cfg := g.runtimeConfig()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	url := strings.TrimRight(cfg.AnthropicURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	query := r.URL.Query()
	query.Del("after_id")
	query.Del("before_id")
	query.Set("limit", "1000")
	req.URL.RawQuery = query.Encode()
	for _, k := range []string{"Authorization", "X-Api-Key", "Anthropic-Version"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil || resp.StatusCode != 200 {
		return nil
	}
	entries, availability, complete, err := parseAnthropicModelsPage(body)
	if err != nil {
		fmt.Fprintf(
			os.Stderr,
			"[gateway] ignored invalid authenticated Anthropic model list: %v\n",
			err,
		)
		return nil
	}
	if complete {
		if err := g.publishAnthropicAvailability(availability); err != nil {
			fmt.Fprintf(
				os.Stderr,
				"[gateway] could not cache authenticated Anthropic model availability: %v\n",
				err,
			)
		}
	}
	return entries
}

func (g *Gateway) monitorModels(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	body, _ := json.Marshal(map[string]any{"object": "list", "data": []any{}})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(body)))
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

func (g *Gateway) upstreamURL(rt string) string {
	cfg := g.runtimeConfig()
	switch rt {
	case "baseten":
		return cfg.BasetenURL
	case "anthropic":
		return cfg.AnthropicURL
	case "openai":
		return cfg.OpenAIURL
	}
	return ""
}

// upstreamEndpoint returns the full upstream URL for a given shape
// and route (one of the native same-shape combos).
func upstreamEndpoint(baseURL, shape string) string {
	switch shape {
	case "anthropic":
		return strings.TrimRight(baseURL, "/") + "/v1/messages"
	case "openai":
		return strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
	}
	return ""
}

// upstreamResponsesEndpoint returns the full upstream URL for an
// OpenAI Responses API call (/v1/responses), used by codex and other
// clients that speak the Responses shape instead of chat completions.
func upstreamResponsesEndpoint(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/v1/responses"
}

// buildUpstreamClient returns the HTTP client, auth mode and api-key
// to use for forwarding to the given route.
func (g *Gateway) buildUpstreamClient(rt string) (mode proxy.UpstreamAuthMode, basetenKey string, upClient *http.Client, ok bool) {
	if rt == "baseten" {
		useOAuth, c, apiKey := g.basetenAuthClient()
		if useOAuth {
			return proxy.UpstreamModeOAuth, "", c, true
		}
		if apiKey != "" {
			return proxy.UpstreamModeAPIKey, apiKey, c, true
		}
		return 0, "", nil, false
	}
	return proxy.UpstreamModePassthrough, "", g.client, true
}

// forwardMessages handles anthropic-shape POST /v1/messages.
func (g *Gateway) forwardMessages(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	rt := cl.cfg.Route
	if !shapeCompatible(cl.cfg.ProtocolShape, rt) {
		g.rejectCrossShape(w)
		return
	}
	if rt == "monitor" {
		g.monitorStubAnthropic(cl, w, r)
		return
	}
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		g.reject(w, 400, "could not read request body")
		return
	}
	requestedReasoning := reasoning.InspectAnthropicMessages(body)
	// History repair for anthropic-shape upstreams: harnesses replay
	// prior turns containing empty text blocks and (after a route flip
	// through an openai-shape provider) tool ids Anthropic rejects.
	sanitized := false
	if cl.cfg.SanitizeHistory {
		body, sanitized = sanitize.AnthropicBody(body)
	}
	attempts, err := g.resolveAttemptsWithRequestedReasoning(
		cl,
		r,
		body,
		"messages",
		requestedReasoning,
		true,
	)
	if err != nil {
		if errors.Is(err, errNeedsLogin) {
			g.rejectNeedsLogin(w)
		} else {
			g.reject(w, 400, err.Error())
		}
		return
	}
	for i := range attempts {
		attempts[i].res.Sanitized = sanitized
	}
	applyMultimodalStateForRequest(cl, attempts, "messages", body)
	isStream := attempts[0].res.IsStream
	if !isStream && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		isStream = true
	}
	g.streamForward(cl, w, r, attempts, isStream, start)
}

// forwardChatCompletions handles openai-shape POST /v1/chat/completions.
func (g *Gateway) forwardChatCompletions(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	rt := cl.cfg.Route
	if !shapeCompatible(cl.cfg.ProtocolShape, rt) {
		g.rejectCrossShape(w)
		return
	}
	if rt == "monitor" {
		g.monitorStubOpenAI(cl, w, r)
		return
	}
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		g.reject(w, 400, "could not read request body")
		return
	}
	attempts, err := g.resolveAttempts(cl, r, body, "chat")
	if err != nil {
		if errors.Is(err, errNeedsLogin) {
			g.rejectNeedsLogin(w)
		} else {
			g.reject(w, 400, err.Error())
		}
		return
	}
	applyMultimodalStateForRequest(cl, attempts, "chat", body)
	isStream := attempts[0].res.IsStream
	if !isStream && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		isStream = true
	}
	g.streamForward(cl, w, r, attempts, isStream, start)
}

// forwardResponses handles openai-shape POST /v1/responses (the
// OpenAI Responses API path that codex CLI uses). It is a close
// sibling of forwardChatCompletions: same route-aware model rewrite
// (baseten only; passthrough routes forward verbatim), same
// buildUpstreamClient + streamForward pipeline, only the upstream
// path differs (/v1/responses instead of /v1/chat/completions).
func (g *Gateway) forwardResponses(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	rt := cl.cfg.Route
	if !shapeCompatible(cl.cfg.ProtocolShape, rt) {
		g.rejectCrossShape(w)
		return
	}
	if rt == "monitor" {
		g.monitorStubOpenAIResponses(cl, w, r)
		return
	}
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		g.reject(w, 400, "could not read request body")
		return
	}
	attempts, err := g.resolveAttempts(cl, r, body, "responses")
	if err != nil {
		if errors.Is(err, errNeedsLogin) {
			g.rejectNeedsLogin(w)
		} else {
			g.reject(w, 400, err.Error())
		}
		return
	}
	applyMultimodalStateForRequest(cl, attempts, "responses", body)
	isStream := attempts[0].res.IsStream
	if !isStream && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		isStream = true
	}
	g.streamForward(cl, w, r, attempts, isStream, start)
}

// upstreamAttempt bundles everything needed to try one upstream for a
// request: the route it represents, the resolved endpoint URL, the
// (possibly model-rewritten) body, auth headers and HTTP client.
type upstreamAttempt struct {
	route        string
	kind         string
	url          string
	res          proxy.RewriteResult
	headers      http.Header
	client       *http.Client
	modelForCost string
	// catalogSnapshot is captured once per logical harness request and shared
	// by routing, policy resolution, every attempt, forwarding, and telemetry.
	catalogSnapshot *pricing.Snapshot
	// requestedReasoning is captured once from raw ingress before any
	// sanitizer or translation and copied to every attempt.
	requestedReasoning         reasoning.RequestedReasoning
	requestedReasoningObserved bool
	// requestProfile is extracted once from the original harness body before
	// subagent rewriting, provider translation, or model canonicalization.
	requestProfile requestprofile.Profile
	// imageInput is derived once from the original protocol request. It gates
	// only the exact Baseten multimodal-rejection retry.
	imageInput bool
	// providerStateful marks Responses requests whose provider-owned state
	// cannot safely move to the native provider.
	providerStateful bool
	// telemetryRequest is shared by every attempt in one logical request.
	// telemetryAttempt captures the immutable price quote for this attempt
	// before it is sent. The served attempt supplies the event's actual cost.
	telemetryRequest *telemetryRequestCaptureV1
	telemetryAttempt telemetryAttemptCaptureV1
	fallbackCount    int
	// translate: the request body has been converted from the anthropic
	// shape to openai chat.completions, and the response must be
	// converted back before reaching the client.
	translate bool
	// normalizeAnthropicUsage marks a baseten-route attempt whose upstream
	// speaks the Anthropic shape (pass-through, no translation). Its usage
	// objects must have input_tokens de-double-counted before relay; see
	// usage.NormalizeAnthropicUsage for the Baseten inclusive-input bug.
	normalizeAnthropicUsage bool
	// fallbackTrigger records why this fallback attempt was selected, including
	// an earlier attempt failure, an active cooldown, or unavailable primary
	// credentials. It flows into telemetry as fallback_trigger.
	fallbackTrigger string
	// origRequestedModel is the model the harness originally sent, before
	// the subagent rewrite replaced it. Empty unless the subagent gate
	// fired. When set, telemetry requested_model uses this instead of
	// res.RequestedModel (which reflects the rewritten body). See
	// the subagent-routing contract.
	origRequestedModel string
	// subagent is true when the request carried a non-empty
	// x-claude-code-agent-id header, regardless of the toggle. Flows
	// into the telemetry row's subagent field.
	subagent bool
	// subagentModel is the configured rewrite target applied by the
	// subagent gate, set only when the rewrite fired. Flows into the
	// telemetry row's subagent_model field.
	subagentModel string
	// strippedToolTypes names the tools[] entry types the responses
	// sanitizer removed from this attempt's body, in body order. Set
	// only on baseten responses attempts whose strip fired; fallback
	// attempts are built from the original bytes and never carry it.
	// Flows into the telemetry row's stripped_tool_types field.
	strippedToolTypes []string
	// responsesCompatibility owns per-logical-request request retries,
	// stream repair counts, and the telemetry summary. It is present only
	// for Baseten /v1/responses attempts with configured compatibility work.
	responsesCompatibility *responsesCompatibilityRequest
}

// telRequestedModel returns the model to record as requested_model in
// telemetry: the original harness model when the subagent gate rewrote
// the body, otherwise the parsed requested model from the (possibly
// rewritten) body. See the subagent-routing contract.
func (at upstreamAttempt) telRequestedModel() string {
	if at.origRequestedModel != "" {
		return at.origRequestedModel
	}
	return at.res.RequestedModel
}

// telStrippedToolTypes returns the comma-joined stripped type names for
// the telemetry row's stripped_tool_types field; "" means no strip.
func (at upstreamAttempt) telStrippedToolTypes() string {
	return strings.Join(at.strippedToolTypes, ",")
}

// fallbackCooldown is how long a listener sends requests straight to
// its fallback route after the primary trips (connect error, 429,
// 5xx), so a dead upstream doesn't add per-request latency.
const fallbackCooldown = 30 * time.Second

func fallbackTriggerStatus(code int) bool {
	return code == 429 || code >= 500
}

// fallbackTriggerTTFT is the telemetry fallback_trigger value for an
// attempt abandoned by the time-to-first-byte deadline.
const fallbackTriggerTTFT = "ttft_timeout"

const (
	fallbackTriggerCooldown         = "cooldown"
	fallbackTriggerAuthUnavailable  = "auth_unavailable"
	fallbackTriggerImageUnsupported = "image_input_unsupported"
)

func allowsImageTranslationFallback(err error) bool {
	var unsupported translate.ErrUnsupportedContent
	return errors.As(err, &unsupported) && unsupported.BlockType == "image"
}

// parseTTFTTimeout parses a ttft_timeout config value. Empty and "0"
// (any zero duration) mean disabled. Malformed or negative values are
// load errors, not silent disables: a typo that quietly turned the
// deadline off would defeat the feature.
func parseTTFTTimeout(field, val string) (time.Duration, error) {
	if val == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q (Go duration syntax, e.g. 30s; 0 disables)", field, val)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: negative duration %q (0 disables)", field, val)
	}
	return d, nil
}

// prefixedBody re-attaches bytes consumed by the first-byte wait in
// front of the live response body; Close still closes the upstream.
type prefixedBody struct {
	io.Reader
	io.Closer
}

// awaitFirstByte blocks until the first Read of body returns (data,
// EOF or error) or the TTFT deadline fires. On expiry it cancels the
// attempt context to unblock the pending Read and reports expired;
// the caller must not touch body afterward. On arrival it returns
// whatever the read produced so the caller can stitch it back in
// front of the body; a read error resurfaces on the next Read.
func awaitFirstByte(body io.Reader, deadline <-chan time.Time, cancel context.CancelFunc) (pre []byte, expired bool) {
	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, 32<<10)
	ch := make(chan readResult, 1)
	go func() {
		n, err := body.Read(buf)
		ch <- readResult{n: n, err: err}
	}()
	select {
	case rr := <-ch:
		return buf[:rr.n], false
	case <-deadline:
		cancel()
		<-ch
		return nil, true
	}
}

func (g *Gateway) fallbackActive(name string) bool {
	g.fallbackMu.Lock()
	defer g.fallbackMu.Unlock()
	return time.Now().Before(g.fallbackUntil[name])
}

func (g *Gateway) fallbackDeadline(name string) (time.Time, bool) {
	g.fallbackMu.Lock()
	defer g.fallbackMu.Unlock()
	deadline := g.fallbackUntil[name]
	return deadline, time.Now().Before(deadline)
}

func (g *Gateway) tripFallback(name string) {
	g.fallbackMu.Lock()
	defer g.fallbackMu.Unlock()
	if g.fallbackUntil == nil {
		g.fallbackUntil = map[string]time.Time{}
	}
	g.fallbackUntil[name] = time.Now().Add(fallbackCooldown)
}

// errNeedsLogin marks an attempt whose route has no usable credentials
// (baseten without a CLI login or API-key fallback).
var errNeedsLogin = errors.New("baseten route needs login")

type routingDisabledModelError struct {
	model string
}

func (e routingDisabledModelError) Error() string {
	return fmt.Sprintf("global routing is Off, so Baseten model %q cannot be used; select a native model or turn routing On", e.model)
}

// attemptPath returns the upstream endpoint path for a handler kind and
// upstream wire shape. kind is "messages" (anthropic listener),
// "chat" or "responses" (openai listeners).
func attemptPath(kind, upShape string) string {
	switch kind {
	case "messages":
		if upShape == "openai" {
			return "/v1/chat/completions"
		}
		return "/v1/messages"
	case "chat":
		return "/v1/chat/completions"
	case "responses":
		return "/v1/responses"
	}
	return "/v1/messages"
}

func inspectRequestedReasoning(
	kind string,
	body []byte,
) (reasoning.RequestedReasoning, bool) {
	if kind != "messages" {
		return reasoning.RequestedReasoning{}, false
	}
	return reasoning.InspectAnthropicMessages(body), true
}

// buildAttempt resolves one route into an upstreamAttempt for the given
// request body and handler kind. When the upstream speaks a different
// shape than the listener (anthropic listener, openai upstream) the body
// is translated before model rewriting. Tier model rewriting applies
// only when the attempted route is baseten;
// passthrough routes preserve the model except for documented harness
// decorations such as a literal trailing [1m], which is canonicalized.
func (g *Gateway) buildAttempt(cl *clientListener, r *http.Request, body []byte, rt, kind string) (upstreamAttempt, error) {
	requested, observed := inspectRequestedReasoning(kind, body)
	return g.buildAttemptTargetWithSnapshot(
		g.pricing.Capture(),
		requested,
		observed,
		cl,
		r,
		body,
		rt,
		kind,
		"",
		false,
	)
}

// buildAttemptTarget is buildAttempt with an optional forced target
// model: forced attempts carry an explicitly chosen Baseten slug
// (alias mapping or raw slug) and bypasses the default model.
func (g *Gateway) buildAttemptTarget(cl *clientListener, r *http.Request, body []byte, rt, kind, forcedModel string, forced bool) (upstreamAttempt, error) {
	requested, observed := inspectRequestedReasoning(kind, body)
	return g.buildAttemptTargetWithSnapshot(
		g.pricing.Capture(),
		requested,
		observed,
		cl,
		r,
		body,
		rt,
		kind,
		forcedModel,
		forced,
	)
}

func (g *Gateway) buildAttemptWithSnapshot(
	snapshot *pricing.Snapshot,
	requestedReasoning reasoning.RequestedReasoning,
	requestedObserved bool,
	cl *clientListener,
	r *http.Request,
	body []byte,
	rt string,
	kind string,
) (upstreamAttempt, error) {
	return g.buildAttemptTargetWithSnapshot(
		snapshot,
		requestedReasoning,
		requestedObserved,
		cl,
		r,
		body,
		rt,
		kind,
		"",
		false,
	)
}

func (g *Gateway) buildAttemptTargetWithSnapshot(
	snapshot *pricing.Snapshot,
	requestedReasoning reasoning.RequestedReasoning,
	requestedObserved bool,
	cl *clientListener,
	r *http.Request,
	body []byte,
	rt string,
	kind string,
	forcedModel string,
	forced bool,
) (upstreamAttempt, error) {
	upShape := upstreamShapeFor(cl.cfg, rt)
	needsTranslate := kind == "messages" && upShape == "openai"
	if kind == "messages" {
		profile := requestprofile.Inspect(body)
		if profile.RawModel != "" && profile.CanonicalModel != profile.RawModel {
			body = proxy.RewriteModelInBody(body, profile.CanonicalModel).NewBody
		}
	}
	projectFastProfile := cl.cfg.ProtocolShape == "anthropic" && rt != "anthropic"
	if projectFastProfile {
		body, _, _ = requestprofile.RemoveUnsupportedFastProfile(body, nil)
	}
	targetModel := forcedModel
	if rt == "baseten" && !forced {
		rc := cl.cfg
		rc.Route = "baseten"
		targetModel = g.upstreamModelFor(rc)
	}
	reasoningTelemetry := reasoningTelemetryV1{}
	if rt == "baseten" {
		var err error
		body, reasoningTelemetry, err = applyBasetenReasoningPolicy(
			snapshot,
			cl.cfg,
			body,
			targetModel,
			kind,
			upShape,
			requestedReasoning,
		)
		if err != nil {
			return upstreamAttempt{}, err
		}
	}
	if needsTranslate {
		tb, err := translate.RequestToOpenAI(body)
		if err != nil {
			return upstreamAttempt{}, err
		}
		body = tb
	}
	var res proxy.RewriteResult
	var strippedTypes []string
	switch {
	case forced:
		if rt == "baseten" && kind == "responses" && len(cl.cfg.ResponsesStripToolTypes) > 0 {
			res, strippedTypes = stripAndRewriteResponses(body, cl.cfg.ResponsesStripToolTypes, forcedModel)
			break
		}
		res = proxy.RewriteModelInBody(body, forcedModel)
	case rt == "baseten":
		if kind == "responses" && len(cl.cfg.ResponsesStripToolTypes) > 0 {
			res, strippedTypes = stripAndRewriteResponses(body, cl.cfg.ResponsesStripToolTypes, targetModel)
			break
		}
		res = proxy.RewriteModelInBody(body, targetModel)
	default:
		res = proxy.RewriteModelInBody(body, "")
	}
	modelForCost := res.UpstreamModel
	if modelForCost == "" {
		modelForCost = res.RequestedModel
	}
	mode, basetenKey, upClient, ok := g.buildUpstreamClient(rt)
	if !ok {
		return upstreamAttempt{}, errNeedsLogin
	}
	headers, err := proxy.BuildUpstreamHeaders(r.Header, mode, basetenKey)
	if err != nil {
		// Empty API key in APIKey mode is a credential problem; surface
		// it as needs-login rather than sending an unauthenticated
		// upstream request.
		return upstreamAttempt{}, fmt.Errorf("%w: %v", errNeedsLogin, err)
	}
	if projectFastProfile {
		_, headers, _ = requestprofile.RemoveUnsupportedFastProfile(nil, headers)
	}
	telemetryAttempt := captureTelemetryAttemptV1(
		snapshot,
		time.Now(),
		rt,
		modelForCost,
	)
	telemetryAttempt.reasoning = reasoningTelemetry
	return upstreamAttempt{
		route:                      rt,
		kind:                       kind,
		url:                        strings.TrimRight(g.upstreamURL(rt), "/") + attemptPath(kind, upShape),
		res:                        res,
		headers:                    headers,
		client:                     upClient,
		modelForCost:               modelForCost,
		catalogSnapshot:            snapshot,
		requestedReasoning:         requestedReasoning,
		requestedReasoningObserved: requestedObserved,
		telemetryAttempt:           telemetryAttempt,
		translate:                  needsTranslate,
		normalizeAnthropicUsage:    rt == "baseten" && upShape == "anthropic",
		strippedToolTypes:          strippedTypes,
	}, nil
}

// stripAndRewriteResponses applies the responses tool-type strip and
// the baseten model rewrite on one decode of the body: codex turn
// bodies run 80 KB+, so the two-step path (RewriteModelInBody plus a
// second full parse for the strip) would double the JSON work. The
// result mirrors proxy.RewriteModelInBody and preserves the
// untouched-bytes contract when nothing changes or the body does not
// parse.
func stripAndRewriteResponses(body []byte, types []string, target string) (proxy.RewriteResult, []string) {
	res := proxy.RewriteResult{NewBody: body}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return res, nil
	}
	res.Parsed = true
	if m, ok := data["model"].(string); ok {
		res.RequestedModel = m
	}
	if s, ok := data["stream"].(bool); ok {
		res.IsStream = s
	}
	res.UpstreamModel = res.RequestedModel
	stripped := sanitize.ResponsesStripToolTypesMap(data, types)
	if target != "" && target != res.RequestedModel {
		res.UpstreamModel = target
		data["model"] = target
	} else if stripped == nil {
		return res, nil
	}
	nb, err := json.Marshal(data)
	if err != nil {
		return res, nil
	}
	res.NewBody = nb
	return res, stripped
}

// aliasNamespacePrefixes is the picker-visible model id namespace the
// gateway owns (the model-discovery contract). Ids under it never fall
// through to the default model or to Anthropic: unknown ones are a loud
// 400, because a stale ~/.claude/cache/gateway-models.json keeps
// offering aliases after they are removed from config.
var aliasNamespacePrefixes = []string{"claude-baseten-", "anthropic-baseten-"}

// InAliasNamespace is exported so the baseten-switch claude adapter
// (`claude subagents`) shares this one namespace resolution instead of
// re-implementing the prefix list (same rule as config.NativeRoute).
func InAliasNamespace(id string) bool {
	for _, p := range aliasNamespacePrefixes {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// unknownAliasError names the rejected id, the configured aliases and
// the fix, so a request for a removed alias is actionable instead of
// a silent default-model route.
func unknownAliasError(rc resolvedClientConfig, id string) error {
	fix := fmt.Sprintf("add it to model_aliases for client %q in gateway.yaml and reload the gateway (kill -HUP $(cat ~/.config/baseten-switch/gateway.pid)), or pick another model; Claude Code refreshes its cached picker list (~/.claude/cache/gateway-models.json) at next launch", rc.Name)
	if len(rc.ModelAliases) == 0 {
		return fmt.Errorf("unknown gateway model %q: client %q has no model_aliases configured. Fix: %s", id, rc.Name, fix)
	}
	ids := make([]string, 0, len(rc.ModelAliases))
	for a := range rc.ModelAliases {
		ids = append(ids, a)
	}
	return fmt.Errorf("unknown gateway model %q: configured model_aliases for client %q are [%s]. Fix: %s", id, rc.Name, strings.Join(sortedKeys(ids), ", "), fix)
}

// resolveExplicitModelAttempt implements explicit-choice-wins routing
// (the model-discovery contract): configured aliases remain an Anthropic-shape
// feature, while a raw Baseten slug (contains "/") is explicit on every
// request shape. Both are single attempts with no fallback_route: silently
// substituting a native provider would violate the user's model selection.
// On Anthropic-shape listeners, an unrecognized
// claude-baseten-*/anthropic-baseten-* id is a loud 400, never a default-model route.
func (g *Gateway) resolveExplicitModelAttempt(cl *clientListener, r *http.Request, body []byte, kind string) (upstreamAttempt, bool, error) {
	requested, observed := inspectRequestedReasoning(kind, body)
	return g.resolveExplicitModelAttemptWithSnapshot(
		g.pricing.Capture(),
		requested,
		observed,
		cl,
		r,
		body,
		kind,
	)
}

func (g *Gateway) resolveExplicitModelAttemptWithSnapshot(
	snapshot *pricing.Snapshot,
	requestedReasoning reasoning.RequestedReasoning,
	requestedObserved bool,
	cl *clientListener,
	r *http.Request,
	body []byte,
	kind string,
) (upstreamAttempt, bool, error) {
	requested := proxy.RewriteModelInBody(body, "").RequestedModel
	if requested == "" {
		return upstreamAttempt{}, false, nil
	}
	target := ""
	if strings.Contains(requested, "/") {
		target = requested
	} else if cl.cfg.ProtocolShape != "anthropic" {
		return upstreamAttempt{}, false, nil
	} else if slug, ok := cl.cfg.ModelAliases[requested]; ok {
		target = slug
	} else if InAliasNamespace(requested) {
		return upstreamAttempt{}, false, unknownAliasError(cl.cfg, requested)
	} else {
		return upstreamAttempt{}, false, nil
	}
	at, err := g.buildAttemptTargetWithSnapshot(
		snapshot,
		requestedReasoning,
		requestedObserved,
		cl,
		r,
		body,
		"baseten",
		kind,
		target,
		true,
	)
	if err != nil {
		return upstreamAttempt{}, false, err
	}
	return at, true, nil
}

// resolveAttempts turns a listener's route + fallback_route into the
// ordered attempts for this request: primary first, fallback second.
// A fallback equal to the primary attempt's effective route is dormant
// and dropped for that request (single attempt, as if unset). During
// an active cooldown (or when the primary has no credentials)
// the fallback is promoted to primary and tried alone. A non-auth error
// building the primary (e.g. untranslatable content) is returned to the
// caller; fallback build errors just drop the fallback. Explicitly
// chosen Baseten models (alias or raw slug) preempt all of this with a
// single baseten attempt.
//
// Subagent gate (the subagent-routing contract): on an anthropic-shape
// listener with subagent routing enabled, a request carrying a
// non-empty x-claude-code-agent-id header has its model rewritten to
// subagent_model before the resolution ladder runs, so alias/slug
// targets take the explicit-choice-wins path and native targets fall
// through to the switch position. The original requested model is
// preserved on the attempt for telemetry. A body that does not parse
// as JSON skips the rewrite and passes through untouched.
func (g *Gateway) resolveAttempts(cl *clientListener, r *http.Request, body []byte, kind string) ([]upstreamAttempt, error) {
	requested, observed := inspectRequestedReasoning(kind, body)
	return g.resolveAttemptsWithRequestedReasoning(
		cl,
		r,
		body,
		kind,
		requested,
		observed,
	)
}

func (g *Gateway) resolveAttemptsWithRequestedReasoning(
	cl *clientListener,
	r *http.Request,
	body []byte,
	kind string,
	requested reasoning.RequestedReasoning,
	requestedObserved bool,
) ([]upstreamAttempt, error) {
	snapshot := g.pricing.Capture()
	profile := requestprofile.Inspect(body)
	isSubagent := false
	origRequested := ""
	subagentModel := ""
	if cl.cfg.ProtocolShape == "anthropic" && !cl.cfg.globalRoutingOff() {
		isSubagent = r.Header.Get(subagentAgentIDHeader) != ""
		if cl.cfg.subagentEnabled() && isSubagent {
			if res := proxy.RewriteModelInBody(body, ""); res.Parsed {
				origRequested = res.RequestedModel
				body = proxy.RewriteModelInBody(body, cl.cfg.SubagentModel).NewBody
				subagentModel = cl.cfg.SubagentModel
			}
		}
	}
	attempts, err := g.resolveAttemptsLadderWithSnapshot(
		snapshot,
		requested,
		requestedObserved,
		cl,
		r,
		body,
		kind,
	)
	if err != nil {
		return nil, err
	}
	for i := range attempts {
		attempts[i].requestedReasoning = requested
		attempts[i].requestedReasoningObserved = requestedObserved
		attempts[i].requestProfile = profile
		attempts[i].subagent = isSubagent
		if subagentModel != "" {
			attempts[i].subagentModel = subagentModel
			attempts[i].origRequestedModel = origRequested
		}
	}
	return attempts, nil
}

// resolveAttemptsLadder is the pre-subagent-gate resolution ladder:
// explicit alias/slug choice, then model-route pin, then primary +
// fallback. Split out so the subagent gate in resolveAttempts can
// rewrite the body first.
func (g *Gateway) resolveAttemptsLadder(cl *clientListener, r *http.Request, body []byte, kind string) ([]upstreamAttempt, error) {
	requested, observed := inspectRequestedReasoning(kind, body)
	return g.resolveAttemptsLadderWithSnapshot(
		g.pricing.Capture(),
		requested,
		observed,
		cl,
		r,
		body,
		kind,
	)
}

func (g *Gateway) resolveAttemptsLadderWithSnapshot(
	snapshot *pricing.Snapshot,
	requestedReasoning reasoning.RequestedReasoning,
	requestedObserved bool,
	cl *clientListener,
	r *http.Request,
	body []byte,
	kind string,
) ([]upstreamAttempt, error) {
	requested := proxy.RewriteModelInBody(body, "").RequestedModel
	if cl.cfg.globalRoutingOff() {
		// The global routing gate is terminal. Inspect the requested model only
		// to reject explicit Baseten choices clearly; otherwise build one
		// protocol-native attempt. Do not consult aliases, pins, subagent
		// policy, fallback, cooldown, or Baseten credentials.
		if _, alias := cl.cfg.ModelAliases[requested]; alias ||
			InAliasNamespace(requested) ||
			strings.Contains(requested, "/") ||
			(cl.cfg.ProtocolShape == "openai" &&
				requested == CodexCompatibilityModel) {
			return nil, routingDisabledModelError{model: requested}
		}
		native := config.NativeRoute(cl.cfg.ProtocolShape)
		at, err := g.buildAttemptWithSnapshot(
			snapshot,
			requestedReasoning,
			requestedObserved,
			cl,
			r,
			body,
			native,
			kind,
		)
		if err != nil {
			return nil, err
		}
		return []upstreamAttempt{at}, nil
	}
	if at, ok, err := g.resolveExplicitModelAttemptWithSnapshot(
		snapshot,
		requestedReasoning,
		requestedObserved,
		cl,
		r,
		body,
		kind,
	); err != nil {
		return nil, err
	} else if ok {
		return []upstreamAttempt{at}, nil
	}
	// Model-route pin (config/schema.md): family entry for native
	// claude-*/anthropic-* ids. When
	// pinned, the PRIMARY attempt is built with the pin's effective route
	// and forced target instead of the client route. Unpinned or
	// no-family ids fall through to the switch.
	primary, perr := g.resolveModelRoutePrimaryWithSnapshot(
		snapshot,
		requestedReasoning,
		requestedObserved,
		cl,
		r,
		body,
		kind,
	)
	if perr != nil &&
		!errors.Is(perr, errNeedsLogin) &&
		!allowsImageTranslationFallback(perr) &&
		!reasoning.AllowsFallback(perr) {
		return nil, perr
	}
	var attempts []upstreamAttempt
	if perr == nil {
		attempts = append(attempts, primary)
	}
	// The managed Codex profile's compatibility model is an instruction to
	// use this client's configured Baseten target, never an OpenAI model.
	// Suppress the native fallback even when the Baseten attempt cannot be
	// built or later fails.
	if cl.cfg.ProtocolShape == "openai" &&
		requested == CodexCompatibilityModel {
		if len(attempts) != 0 {
			return attempts, nil
		}
		return nil, perr
	}
	if fb := cl.cfg.FallbackRoute; fb != "" {
		// Fallback inertness is decided here, per request, against the
		// primary attempt's EFFECTIVE route rather than the configured
		// route, because a model_routes pin can move the primary
		// (config/schema.md, model route fallback semantics). A fallback
		// equal to the effective primary is dormant for this request:
		// no duplicate same-route retry, no spurious cooldown trip.
		// When they differ the waterfall is live. A primary that failed to build
		// (errNeedsLogin) keeps the fallback regardless.
		if perr == nil && fb == primary.route {
			return attempts, nil
		}
		if fba, ferr := g.buildAttemptWithSnapshot(
			snapshot,
			requestedReasoning,
			requestedObserved,
			cl,
			r,
			body,
			fb,
			kind,
		); ferr == nil {
			if perr == nil && g.fallbackActive(cl.cfg.Name) {
				fba.fallbackCount = 1
				fba.fallbackTrigger = fallbackTriggerCooldown
				attempts = []upstreamAttempt{fba}
			} else {
				if errors.Is(perr, errNeedsLogin) {
					fba.fallbackCount = 1
					fba.fallbackTrigger = fallbackTriggerAuthUnavailable
				} else if allowsImageTranslationFallback(perr) {
					fba.fallbackCount = 1
					fba.fallbackTrigger = fallbackTriggerImageUnsupported
				} else if reasoning.AllowsFallback(perr) {
					fba.fallbackCount = 1
					fba.fallbackTrigger = "reasoning_policy_error"
				}
				attempts = append(attempts, fba)
			}
		}
	}
	if len(attempts) == 0 {
		if reasoning.IsPolicyError(perr) {
			return nil, perr
		}
		if allowsImageTranslationFallback(perr) {
			return nil, perr
		}
		return nil, errNeedsLogin
	}
	return attempts, nil
}

// modelRoutePin is the resolved pin decision for a requested id: the
// effective route and forced target for the primary attempt. It is the
// shared resolution the request path (resolveModelRoutePrimary) and the
// admin status effective table (computeFamilies) both use, so the table
// cannot drift from live behavior.
type modelRoutePin struct {
	route       string // effective route for the primary attempt
	forcedModel string // forced upstream model (target pin only; "" otherwise)
	forced      bool   // true when forcedModel is set (bypass default model)
	pinned      bool   // true when a model_routes pin matched
	source      string // family_mapping | native_mapping
}

// resolveModelRoutePin returns the pin decision for a requested id on an
// anthropic-shape client. It classifies the requested id into one of the
// supported families and consults that family key only. When no pin
// matches it returns pinned=false so the caller falls through to the
// switch position. Alias values are resolved through model_aliases here
// so both callers see the same target slug.
func resolveModelRoutePin(rc resolvedClientConfig, requestedID string) modelRoutePin {
	return resolveModelRoutePinWithFamily(rc, requestedID, "")
}

func resolveModelRoutePinWithFamily(
	rc resolvedClientConfig,
	requestedID string,
	catalogFamily string,
) modelRoutePin {
	if rc.ProtocolShape != "anthropic" || len(rc.ModelRoutes) == 0 || requestedID == "" {
		return modelRoutePin{}
	}
	fam := strings.ToLower(strings.TrimSpace(catalogFamily))
	if fam == "" {
		fam = familyOf(requestedID)
	}
	if fam == "" {
		return modelRoutePin{}
	}
	pin, ok := rc.ModelRoutes[fam]
	if !ok {
		return modelRoutePin{}
	}
	out := modelRoutePinValue(rc, pin)
	if pin == "native" {
		out.source = "native_mapping"
	} else {
		out.source = "family_mapping"
	}
	return out
}

// modelRoutePinValue interprets one model_routes pin VALUE into the pin
// decision: "native" pins to the client's native route; anything else is
// a Baseten target, resolved through model_aliases so all callers see
// the same slug. Shared by resolveModelRoutePin (request path) and
// computeFamilies (admin status).
func modelRoutePinValue(rc resolvedClientConfig, pin string) modelRoutePin {
	if pin == "native" {
		return modelRoutePin{route: config.NativeRoute(rc.ProtocolShape), pinned: true, source: "native_mapping"}
	}
	target := pin
	if alias, ok := rc.ModelAliases[pin]; ok {
		target = alias
	}
	return modelRoutePin{route: "baseten", forcedModel: target, forced: true, pinned: true}
}

type modelPolicyDecision struct {
	route  string
	model  string
	source string
	forced bool
}

// resolveNativeModelPolicy is the server-owned native-model resolver used
// by both live attempts and admin status. Explicit alias/raw-slug choices
// are handled one step earlier because they intentionally suppress fallback.
func resolveNativeModelPolicy(rc resolvedClientConfig, requestedID string) modelPolicyDecision {
	return resolveNativeModelPolicyWithFamily(
		rc,
		requestedID,
		"",
	)
}

func resolveNativeModelPolicyWithFamily(
	rc resolvedClientConfig,
	requestedID string,
	catalogFamily string,
) modelPolicyDecision {
	if rc.globalRoutingOff() {
		return modelPolicyDecision{
			route:  config.NativeRoute(rc.ProtocolShape),
			source: "global_off",
		}
	}
	if pin := resolveModelRoutePinWithFamily(
		rc,
		requestedID,
		catalogFamily,
	); pin.pinned {
		return modelPolicyDecision{
			route: pin.route, model: pin.forcedModel,
			source: pin.source, forced: pin.forced,
		}
	}
	if rc.Route != "baseten" {
		return modelPolicyDecision{route: rc.Route, source: "native_mapping"}
	}
	target := rc.DefaultModel
	return modelPolicyDecision{
		route: "baseten", model: target, source: "default_baseten", forced: target != "",
	}
}

// resolveModelRoutePrimary builds the primary attempt, applying a
// model_routes pin when the (possibly subagent-rewritten) requested id
// matches a family entry. Pin semantics are defined in config/schema.md:
//   - "native": primary attempt is the client's native route (anthropic
//     for an anthropic-shape listener), with only documented harness model
//     decorations canonicalized.
//   - target (alias resolved through model_aliases, slug verbatim):
//     primary attempt is baseten with the upstream model FORCED to the
//     target (default model bypassed for this request), body rewritten
//     via proxy.RewriteModelInBody.
//
// Unpinned or no-family ids build the primary from the client route.
// The fallback waterfall is unaffected: it is built by the caller from
// the original requested id.
func (g *Gateway) resolveModelRoutePrimary(cl *clientListener, r *http.Request, body []byte, kind string) (upstreamAttempt, error) {
	requested, observed := inspectRequestedReasoning(kind, body)
	return g.resolveModelRoutePrimaryWithSnapshot(
		g.pricing.Capture(),
		requested,
		observed,
		cl,
		r,
		body,
		kind,
	)
}

func (g *Gateway) resolveModelRoutePrimaryWithSnapshot(
	snapshot *pricing.Snapshot,
	requestedReasoning reasoning.RequestedReasoning,
	requestedObserved bool,
	cl *clientListener,
	r *http.Request,
	body []byte,
	kind string,
) (upstreamAttempt, error) {
	requested := ""
	if res := proxy.RewriteModelInBody(body, ""); res.Parsed {
		requested = res.RequestedModel
	}
	catalogFamily := ""
	if family, ok := snapshot.ModelFamily(
		pricing.ProviderAnthropic,
		normalizeModelID(requested),
	); ok {
		catalogFamily = family
	}
	decision := resolveNativeModelPolicyWithFamily(
		cl.cfg,
		requested,
		catalogFamily,
	)
	if decision.forced {
		return g.buildAttemptTargetWithSnapshot(
			snapshot,
			requestedReasoning,
			requestedObserved,
			cl,
			r,
			body,
			decision.route,
			kind,
			decision.model,
			true,
		)
	}
	return g.buildAttemptWithSnapshot(
		snapshot,
		requestedReasoning,
		requestedObserved,
		cl,
		r,
		body,
		decision.route,
		kind,
	)
}

// routeEffective returns the value for the telemetry route_effective
// column: set only when the served route differs from the configured one.
func routeEffective(cl *clientListener, at upstreamAttempt) string {
	if at.route != cl.cfg.Route {
		return at.route
	}
	return ""
}

// ExpectedPrimaryRoute resolves the route a healthy request for
// requestedID is served by on one config client: native while global
// routing is Off, otherwise a matching model_routes family pin or the
// Baseten default. Exported for doctor --probe's served-route check, which
// must share the request path's pin resolution:
// a pin-served probe is the designed route, not fallback evidence, and a
// telemetry route_effective value alone cannot tell the two apart. An
// empty requestedID matches no pin and yields the Baseten default.
func ExpectedPrimaryRoute(c config.Client, globalRoutingEnabled bool, requestedID string) string {
	shape := c.ProtocolShape
	rt := "baseten"
	if !globalRoutingEnabled {
		rt = config.NativeRoute(shape)
	}
	rc := resolvedClientConfig{
		ProtocolShape:        shape,
		Route:                rt,
		HasGlobalRoutingGate: true,
		GlobalRoutingEnabled: globalRoutingEnabled,
		ModelRoutes:          c.ModelRoutes,
		ModelAliases:         c.ModelAliases,
	}
	if rc.globalRoutingOff() {
		return rt
	}
	if pin := resolveModelRoutePin(rc, requestedID); pin.pinned {
		return pin.route
	}
	return rt
}

// streamForward tries each attempt in order, relays the first usable
// response to the client (streaming or not), parses usage and writes
// one telemetry row tagged with the listener name. A later attempt is
// tried (and the fallback cooldown tripped) when an earlier one fails
// to connect, returns 429/5xx, or exceeds the ttft_timeout first-byte
// deadline; nothing has been written to the client at that point, so
// the retry is invisible. A TTFT expiry on the final attempt (or on an
// explicit alias/slug choice, which has no fallback) surfaces as a 504
// naming the model and the deadline. Once the first response byte has
// arrived the deadline is inert: a live stream is never truncated.
func (g *Gateway) streamForward(cl *clientListener, w http.ResponseWriter, r *http.Request, attempts []upstreamAttempt, isStream bool, start time.Time) {
	pricingSnapshot := attempts[0].catalogSnapshot
	if pricingSnapshot == nil {
		pricingSnapshot = g.pricing.Capture()
	}
	requestCapture, captureErr := captureTelemetryRequestProfileV1(
		pricingSnapshot,
		start,
		cl.cfg.Name,
		cl.cfg.Route,
		cl.cfg.ProtocolShape,
		attempts[0].telRequestedModel(),
		attempts[0].requestProfile,
	)
	if captureErr != nil {
		fmt.Fprintf(os.Stderr, "[gateway] telemetry capture failed: %v\n", captureErr)
	} else {
		requestCapture.requestedReasoning = attempts[0].requestedReasoning
		requestCapture.requestedReasoningObserved =
			attempts[0].requestedReasoningObserved
		requestCapture.primaryReasoning = attempts[0].telemetryAttempt.reasoning
		for i := range attempts {
			attempts[i].telemetryRequest = &requestCapture
		}
		if !requestCapture.nativeQuote.Priced {
			g.kickPublicCatalogRefresh()
		}
	}
	for _, candidate := range attempts {
		if candidate.telemetryAttempt.actualStandardQuote.Priced {
			continue
		}
		g.kickPublicCatalogRefresh()
		if candidate.route == pricing.ProviderBaseten {
			g.kickCatalogRefreshForPricingMiss()
		}
	}
	upCtx, upCancel := context.WithCancel(r.Context())
	defer upCancel()
	gDone := make(chan struct{})
	go func() {
		select {
		case <-g.ctx.Done():
			upCancel()
		case <-gDone:
		}
	}()
	defer close(gDone)

	var reqCtx context.Context = upCtx
	if !isStream {
		tctx, tcancel := context.WithTimeout(upCtx, 600*time.Second)
		defer tcancel()
		reqCtx = tctx
	}

	var resp *http.Response
	var at upstreamAttempt
attemptLoop:
	for i := range attempts {
		at = attempts[i]
		at.fallbackCount += i
		last := i == len(attempts)-1
		res := at.res
		if warning := contextCompatibilityWarning(
			pricingSnapshot,
			at.route,
			at.modelForCost,
			at.requestProfile.RequestedContextBudgetTokens,
		); warning != "" {
			fmt.Fprintf(
				os.Stderr,
				"[gateway] context compatibility warning client=%s: %s\n",
				cl.cfg.Name,
				warning,
			)
		}

		if compatibility := beginResponsesCompatibilityRequest(cl, at); compatibility != nil {
			at.responsesCompatibility = compatibility
			res.NewBody = at.responsesCompatibility.body
			at.res = res
			at.strippedToolTypes = append(
				[]string(nil),
				at.responsesCompatibility.strippedToolTypes...,
			)
		}
		var ttftDeadline time.Time
		if d := cl.cfg.TTFTTimeout; d > 0 {
			// Compatibility retries are sub-attempts inside this one
			// provider attempt. They share this absolute first-byte
			// budget instead of resetting ttft_timeout per rewrite.
			ttftDeadline = time.Now().Add(d)
		}

		ttftExpired := false
		var err error
		for {
			var classifierBody []byte
			classifierReadAttempted := false
			classifierBodyComplete := false
			var watch upstreamTTFTWatch
			resp, err, ttftExpired, watch = startUpstreamSubAttempt(
				reqCtx,
				at,
				res.NewBody,
				ttftDeadline,
			)
			if ttftExpired || err != nil {
				watch.stop()
				watch.cancel()
				break
			}

			if !last && fallbackTriggerStatus(resp.StatusCode) {
				watch.stop()
				fmt.Fprintf(os.Stderr,
					"[gateway] fallback client=%s route=%s status=%d -> trying %s\n",
					cl.cfg.Name, at.route, resp.StatusCode, attempts[i+1].route)
				g.tripFallback(cl.cfg.Name)
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				resp.Body.Close()
				watch.cancel()
				if at.hasActiveResponsesCompatibility() {
					attempts[i+1].responsesCompatibility =
						at.responsesCompatibility
				}
				attempts[i+1].fallbackTrigger =
					fmt.Sprintf("http_%d", resp.StatusCode)
				continue attemptLoop
			}

			// Headers arrived but the harness only unfreezes on the first
			// body byte. Every compatibility sub-attempt consumes the
			// remaining absolute TTFT budget before any bytes are visible.
			if watch.c != nil {
				pre, expired := awaitFirstByte(resp.Body, watch.c, watch.cancel)
				if expired {
					resp.Body.Close()
					ttftExpired = true
				} else {
					watch.stop()
					resp.Body = prefixedBody{
						Reader: io.MultiReader(bytes.NewReader(pre), resp.Body),
						Closer: resp.Body,
					}
				}
			}
			if ttftExpired {
				watch.stop()
				watch.cancel()
				break
			}

			// The winning response body still uses this sub-attempt context.
			// Cancel it only after relay completes.
			defer watch.cancel()
			if !last &&
				reactiveImageFallbackEligible(
					cl,
					at,
					attempts[i+1],
					resp,
				) {
				if !classifierReadAttempted {
					if !hasDeclaredBoundedClassifierBody(resp) {
						break
					}
					var classifierReadExpired bool
					classifierBody,
						classifierBodyComplete,
						classifierReadExpired =
						bufferBoundedClassifierBody(
							resp,
							ttftDeadline,
							watch.cancel,
						)
					if classifierReadExpired {
						ttftExpired = true
						watch.stop()
						watch.cancel()
						break
					}
				}
				if classifierBodyComplete &&
					isBasetenMultimodalUnsupported(
						at,
						resp.StatusCode,
						classifierBody,
					) {
					_ = resp.Body.Close()
					watch.cancel()
					fmt.Fprintf(
						os.Stderr,
						"[gateway] fallback client=%s route=%s trigger=%s -> trying %s\n",
						cl.cfg.Name,
						at.route,
						fallbackTriggerImageUnsupported,
						attempts[i+1].route,
					)
					if at.hasActiveResponsesCompatibility() {
						attempts[i+1].responsesCompatibility =
							at.responsesCompatibility
					}
					attempts[i+1].fallbackTrigger =
						fallbackTriggerImageUnsupported
					continue attemptLoop
				}
			}
			break
		}

		if !ttftExpired && err != nil {
			// The harness has gone away, so retrying the same canceled context
			// cannot succeed. More importantly, caller cancellation says
			// nothing about upstream health and must not trip the listener-wide
			// fallback cooldown used by unrelated requests.
			if requestErr := r.Context().Err(); requestErr != nil {
				g.recordTelemetryV1(cl, at, telemetryCompletionV1{
					completedAt:  time.Now(),
					isStream:     isStream,
					contextErr:   requestErr,
					requestBytes: len(res.NewBody),
				})
				return
			}
			code := 502
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				code = 504
			}
			fmt.Fprintf(os.Stderr,
				"[gateway] upstream err client=%s route=%s upstream=%s status=0 code=%d is_stream=%t dur=%s err=%v\n",
				cl.cfg.Name, at.route, res.UpstreamModel, code, isStream, time.Since(start), err)
			if !last {
				g.tripFallback(cl.cfg.Name)
				if at.hasActiveResponsesCompatibility() {
					attempts[i+1].responsesCompatibility =
						at.responsesCompatibility
				}
				attempts[i+1].fallbackTrigger = "transport_error"
				continue
			}
			g.reject(w, code, "upstream unreachable: "+err.Error())
			g.recordTelemetryV1(cl, at, telemetryCompletionV1{
				completedAt:  time.Now(),
				isStream:     isStream,
				contextErr:   r.Context().Err(),
				requestBytes: len(res.NewBody),
			})
			return
		}
		if ttftExpired {
			if !last {
				fmt.Fprintf(os.Stderr,
					"[gateway] fallback client=%s route=%s ttft_timeout=%s no first byte -> trying %s\n",
					cl.cfg.Name, at.route, cl.cfg.TTFTTimeout, attempts[i+1].route)
				g.tripFallback(cl.cfg.Name)
				if at.hasActiveResponsesCompatibility() {
					attempts[i+1].responsesCompatibility =
						at.responsesCompatibility
				}
				attempts[i+1].fallbackTrigger = fallbackTriggerTTFT
				continue
			}
			fmt.Fprintf(os.Stderr,
				"[gateway] upstream ttft timeout client=%s route=%s model=%s ttft_timeout=%s dur=%s\n",
				cl.cfg.Name, at.route, at.modelForCost, cl.cfg.TTFTTimeout, time.Since(start))
			g.reject(w, 504, fmt.Sprintf("upstream timeout: model %s sent no first byte within ttft_timeout %s", at.modelForCost, cl.cfg.TTFTTimeout))
			status := 504
			trigger := fallbackTriggerTTFT
			g.recordTelemetryV1(cl, at, telemetryCompletionV1{
				completedAt:     time.Now(),
				status:          &status,
				isStream:        isStream,
				gatewayFailure:  true,
				fallbackTrigger: &trigger,
				requestBytes:    len(res.NewBody),
			})
			return
		}
		break
	}
	res := at.res
	defer resp.Body.Close()

	if at.translate {
		g.relayTranslated(cl, w, resp, at, isStream, start)
		return
	}
	if at.normalizeAnthropicUsage {
		g.relayBasetenAnthropic(cl, w, resp, at, isStream, start)
		return
	}

	upstreamCT := resp.Header.Get("Content-Type")
	sse := strings.Contains(strings.ToLower(upstreamCT), "text/event-stream")
	var responsesGuard *responsescompat.SSEGuard
	if sse && at.hasActiveResponsesCompatibility() {
		candidate := at.responsesCompatibility.newSSEGuard()
		if candidate != nil {
			if resp.Header.Get("Content-Encoding") == "" {
				responsesGuard = candidate
			} else {
				// Do not inspect or rewrite encoded bytes. Preserve the
				// upstream representation and its headers intact.
				at.responsesCompatibility.summary.ValidationErrors++
			}
		}
	}
	h := w.Header()
	proxy.CopyHeader(h, resp.Header)
	if responsesGuard != nil {
		// A guard may change event bytes after upstream headers arrive.
		// Remove representation metadata up front so any repair cannot leave
		// a stale encoding, integrity digest, entity tag, or length.
		removeResponsesGuardStaleHeaders(h)
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	collected := &bytes.Buffer{}
	var firstDeltaAt time.Time
	responseComplete := false
	var relayErr error
	if responsesGuard != nil {
		guarded := relayResponsesSSE(
			r.Context(),
			resp.Body,
			w,
			flusher,
			responsesGuard,
			at.responsesCompatibility,
		)
		collected = bytes.NewBuffer(
			append([]byte(nil), guarded.collected.Bytes()...),
		)
		firstDeltaAt = guarded.firstDeltaAt
		responseComplete = guarded.responseComplete
		relayErr = guarded.relayErr
	} else {
		buf := make([]byte, 8192)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				_, werr := w.Write(chunk)
				if flusher != nil {
					flusher.Flush()
				}
				collected.Write(chunk)
				if werr != nil {
					relayErr = r.Context().Err()
					break
				}
				if firstDeltaAt.IsZero() && sse && containsOutputDelta(collected.Bytes()) {
					firstDeltaAt = time.Now()
				}
			}
			if rerr != nil {
				responseComplete = errors.Is(rerr, io.EOF)
				if !responseComplete {
					relayErr = rerr
				}
				break
			}
		}
	}
	upstreamCE := resp.Header.Get("Content-Encoding")
	var u usage.Usage
	usageComplete := false
	toolCalls := 0
	stopReason := ""
	providerReportedModel := ""
	var responseSpeed *string
	if sse {
		md := usage.ParseSSEUsageWithSaw(usage.MaybeDecompress(collected.Bytes(), upstreamCE))
		u = md.Usage
		usageComplete = md.Saw && md.Complete && responseComplete
		toolCalls = md.ToolCalls
		stopReason = md.StopReason
		providerReportedModel = md.Model
		if md.SpeedPresent {
			responseSpeed = stringPointer(md.Speed)
		}
	} else {
		md := usage.ParseUsageWithSaw(usage.MaybeDecompress(collected.Bytes(), upstreamCE))
		u = md.Usage
		usageComplete = md.Saw && responseComplete
		toolCalls = md.ToolCalls
		stopReason = md.StopReason
		providerReportedModel = md.Model
		if md.SpeedPresent {
			responseSpeed = stringPointer(md.Speed)
		}
	}
	if !usageComplete && len(collected.Bytes()) > 0 {
		if alt := usage.ParseUsageWithSaw(
			usage.MaybeDecompress(collected.Bytes(), upstreamCE),
		); alt.Saw {
			u = alt.Usage
			usageComplete = !sse && responseComplete
			if providerReportedModel == "" {
				providerReportedModel = alt.Model
			}
			if responseSpeed == nil && alt.SpeedPresent {
				responseSpeed = stringPointer(alt.Speed)
			}
		}
	}
	status := resp.StatusCode
	var firstOutputAt *time.Time
	if !firstDeltaAt.IsZero() {
		firstOutputAt = &firstDeltaAt
	}
	var stopReasonPointer *string
	if stopReason != "" {
		stopReasonPointer = &stopReason
	}
	var providerReportedModelPointer *string
	if providerReportedModel != "" {
		providerReportedModelPointer = &providerReportedModel
	}
	g.recordTelemetryV1(cl, at, telemetryCompletionV1{
		completedAt:           time.Now(),
		providerReportedModel: providerReportedModelPointer,
		status:                &status,
		isStream:              isStream,
		firstOutputAt:         firstOutputAt,
		responseComplete:      responseComplete,
		contextErr:            relayErr,
		usageComplete:         usageComplete,
		usage: observedGatewayUsageV1(
			u,
			usageComplete,
			telemetryRequestedOneHourCache(at),
		),
		effectiveSpeed:           responseSpeed,
		actualPricingUnsupported: u.CacheCreationTokenBreakdownInconsistent,
		providerStopReason:       stopReasonPointer,
		toolCalls:                toolCalls,
		requestBytes:             len(res.NewBody),
	})
}

// flushWriter flushes after every write so translated SSE events reach
// the harness immediately.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// relayBasetenAnthropic relays a baseten-route, Anthropic-shape (pass-through,
// untranslated) response while de-double-counting cached tokens in its usage
// (see usage.NormalizeAnthropicUsage for the Baseten inclusive-input bug,
// 2026-07-21). Non-streaming bodies are rewritten in full; SSE is normalized
// per event as it flows so streams are never buffered end to end. Telemetry
// parses the emitted (normalized) bytes so it matches what the client sees.
func (g *Gateway) relayBasetenAnthropic(cl *clientListener, w http.ResponseWriter, resp *http.Response, at upstreamAttempt, isStream bool, start time.Time) {
	res := at.res
	upstreamCE := resp.Header.Get("Content-Encoding")
	upstreamCT := resp.Header.Get("Content-Type")
	sse := strings.Contains(strings.ToLower(upstreamCT), "text/event-stream")

	if !sse {
		body, readErr := io.ReadAll(resp.Body)
		decoded := usage.MaybeDecompress(body, upstreamCE)
		wasCompressed := !bytes.Equal(decoded, body)
		h := w.Header()
		proxy.CopyHeader(h, resp.Header)
		out := body
		if normalized, changed := usage.NormalizeAnthropicBody(decoded); changed {
			// We now emit decompressed bytes; the copied Content-Encoding
			// and Content-Length no longer describe them.
			out = normalized
			if wasCompressed {
				h.Del("Content-Encoding")
			}
			h.Set("Content-Length", itoa(len(out)))
		}
		w.WriteHeader(resp.StatusCode)
		_, writeErr := w.Write(out)
		md := usage.ParseUsageWithSaw(
			usage.MaybeDecompress(out, h.Get("Content-Encoding")),
		)
		status := resp.StatusCode
		responseComplete := readErr == nil && writeErr == nil
		contextErr := readErr
		if writeErr != nil {
			contextErr = writeErr
		}
		g.recordTelemetryV1(cl, at, telemetryCompletionV1{
			completedAt:           time.Now(),
			providerReportedModel: optionalStringPointer(md.Model),
			status:                &status,
			isStream:              isStream,
			responseComplete:      responseComplete,
			contextErr:            contextErr,
			usageComplete:         md.Saw && responseComplete,
			usage: observedGatewayUsageV1(
				md.Usage,
				md.Saw && responseComplete,
				telemetryRequestedOneHourCache(at),
			),
			actualPricingUnsupported: md.Usage.CacheCreationTokenBreakdownInconsistent,
			requestBytes:             len(res.NewBody),
		})
		return
	}

	h := w.Header()
	proxy.CopyHeader(h, resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	collected := &bytes.Buffer{}
	var firstDeltaAt time.Time
	responseComplete := false
	var relayErr error
	br := bufio.NewReader(resp.Body)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			out := line
			// Rewrite only the JSON on data: lines; event:/blank framing
			// lines pass through byte for byte.
			if trimmed := bytes.TrimSpace(line); bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[len("data:"):])
				if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
					if nb, changed := usage.NormalizeAnthropicBody(payload); changed {
						out = append(append([]byte("data: "), nb...), '\n')
					}
				}
			}
			_, werr := w.Write(out)
			if flusher != nil {
				flusher.Flush()
			}
			collected.Write(out)
			if werr != nil {
				relayErr = werr
				break
			}
			if firstDeltaAt.IsZero() && containsOutputDelta(collected.Bytes()) {
				firstDeltaAt = time.Now()
			}
		}
		if rerr != nil {
			responseComplete = errors.Is(rerr, io.EOF)
			if !responseComplete {
				relayErr = rerr
			}
			break
		}
	}
	md := usage.ParseSSEUsageWithSaw(usage.MaybeDecompress(collected.Bytes(), upstreamCE))
	status := resp.StatusCode
	var firstOutputAt *time.Time
	if !firstDeltaAt.IsZero() {
		firstOutputAt = &firstDeltaAt
	}
	var stopReason *string
	if md.StopReason != "" {
		stopReason = &md.StopReason
	}
	usageComplete := md.Saw && md.Complete && responseComplete
	g.recordTelemetryV1(cl, at, telemetryCompletionV1{
		completedAt:           time.Now(),
		providerReportedModel: optionalStringPointer(md.Model),
		status:                &status,
		isStream:              isStream,
		firstOutputAt:         firstOutputAt,
		responseComplete:      responseComplete,
		contextErr:            relayErr,
		usageComplete:         usageComplete,
		usage: observedGatewayUsageV1(
			md.Usage,
			usageComplete,
			telemetryRequestedOneHourCache(at),
		),
		actualPricingUnsupported: md.Usage.CacheCreationTokenBreakdownInconsistent,
		providerStopReason:       stopReason,
		toolCalls:                md.ToolCalls,
		requestBytes:             len(res.NewBody),
	})
}

// relayTranslated converts an openai chat.completions response (SSE or
// JSON) back into the anthropic shape and relays it to the client.
// Telemetry parses the emitted anthropic-shape bytes, so usage, tool
// calls and stop_reason flow through the existing pipeline.
func (g *Gateway) relayTranslated(cl *clientListener, w http.ResponseWriter, resp *http.Response, at upstreamAttempt, isStream bool, start time.Time) {
	res := at.res
	upstreamCE := resp.Header.Get("Content-Encoding")
	if resp.StatusCode >= 400 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		ab := translate.ErrorToAnthropic(resp.StatusCode, usage.MaybeDecompress(body, upstreamCE))
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", itoa(len(ab)))
		w.WriteHeader(resp.StatusCode)
		_, writeErr := w.Write(ab)
		status := resp.StatusCode
		responseComplete := readErr == nil && writeErr == nil
		contextErr := readErr
		if writeErr != nil {
			contextErr = writeErr
		}
		g.recordTelemetryV1(cl, at, telemetryCompletionV1{
			completedAt:      time.Now(),
			status:           &status,
			isStream:         isStream,
			responseComplete: responseComplete,
			contextErr:       contextErr,
			requestBytes:     len(res.NewBody),
		})
		return
	}

	upstreamCT := resp.Header.Get("Content-Type")
	sse := strings.Contains(strings.ToLower(upstreamCT), "text/event-stream")
	collected := &bytes.Buffer{}
	var firstDeltaAt time.Time
	responseComplete := false
	var relayErr error
	if sse {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		flusher, _ := w.(http.Flusher)
		tr := translate.NewStreamTranslator(io.MultiWriter(flushWriter{w: w, f: flusher}, collected))
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[len("data:"):])
			if string(payload) == "[DONE]" {
				responseComplete = true
				break
			}
			if err := tr.HandleData(payload); err != nil {
				relayErr = err
				break
			}
			if firstDeltaAt.IsZero() && containsOutputDelta(collected.Bytes()) {
				firstDeltaAt = time.Now()
			}
		}
		if err := sc.Err(); err != nil {
			relayErr = err
			responseComplete = false
		}
		_ = tr.Finish()
	} else {
		body, err := io.ReadAll(resp.Body)
		var ab []byte
		if err == nil {
			ab, err = translate.ResponseToAnthropic(usage.MaybeDecompress(body, upstreamCE))
		}
		if err != nil {
			g.reject(w, 502, "upstream response translation failed: "+err.Error())
			status := 502
			g.recordTelemetryV1(cl, at, telemetryCompletionV1{
				completedAt:    time.Now(),
				status:         &status,
				isStream:       isStream,
				gatewayFailure: true,
				requestBytes:   len(res.NewBody),
			})
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", itoa(len(ab)))
		w.WriteHeader(resp.StatusCode)
		_, relayErr = w.Write(ab)
		collected.Write(ab)
		responseComplete = relayErr == nil
	}

	var u usage.Usage
	usageComplete := false
	toolCalls := 0
	stopReason := ""
	providerReportedModel := ""
	if sse {
		md := usage.ParseSSEUsageWithSaw(collected.Bytes())
		u = md.Usage
		usageComplete = md.Saw && md.Complete && responseComplete
		toolCalls = md.ToolCalls
		stopReason = md.StopReason
		providerReportedModel = md.Model
	} else {
		md := usage.ParseUsageWithSaw(collected.Bytes())
		u = md.Usage
		usageComplete = md.Saw && responseComplete
		providerReportedModel = md.Model
	}
	status := resp.StatusCode
	var firstOutputAt *time.Time
	if !firstDeltaAt.IsZero() {
		firstOutputAt = &firstDeltaAt
	}
	var stopReasonPointer *string
	if stopReason != "" {
		stopReasonPointer = &stopReason
	}
	g.recordTelemetryV1(cl, at, telemetryCompletionV1{
		completedAt:           time.Now(),
		providerReportedModel: optionalStringPointer(providerReportedModel),
		status:                &status,
		isStream:              isStream,
		firstOutputAt:         firstOutputAt,
		responseComplete:      responseComplete,
		contextErr:            relayErr,
		usageComplete:         usageComplete,
		usage: observedGatewayUsageV1(
			u,
			usageComplete,
			telemetryRequestedOneHourCache(at),
		),
		providerStopReason: stopReasonPointer,
		toolCalls:          toolCalls,
		requestBytes:       len(res.NewBody),
	})
}

// monitorStubAnthropic returns a 200 Anthropic-shape stub message
// echoing the request model and records a telemetry row.
func (g *Gateway) monitorStubAnthropic(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, _ := io.ReadAll(r.Body)
	requested := ""
	if res := proxy.RewriteModelInBody(body, ""); res.Parsed {
		requested = res.RequestedModel
	}
	stub := map[string]any{
		"id":          "msg_monitor_stub",
		"type":        "message",
		"role":        "assistant",
		"model":       requested,
		"content":     []map[string]string{{"type": "text", "text": "[monitor] echo stub"}},
		"stop_reason": "end_turn",
		"usage":       map[string]int{"input_tokens": 0, "output_tokens": 0},
	}
	out, _ := json.Marshal(stub)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(out)))
	w.WriteHeader(200)
	_, _ = w.Write(out)
	if at, ok := g.captureLocalTelemetryAttemptV1(cl, start, requested); ok {
		status := 200
		stopReason := "end_turn"
		g.recordTelemetryV1(cl, at, telemetryCompletionV1{
			completedAt:      time.Now(),
			status:           &status,
			responseComplete: true,
			usageComplete:    true,
			usage: observedGatewayUsageV1(
				usage.Usage{},
				true,
				false,
			),
			providerStopReason: &stopReason,
			requestBytes:       len(body),
		})
	}
}

// monitorStubOpenAI returns a 200 OpenAI-shape stub chat completion
// echoing the request model and records a telemetry row.
func (g *Gateway) monitorStubOpenAI(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, _ := io.ReadAll(r.Body)
	requested := ""
	if res := proxy.RewriteModelInBody(body, ""); res.Parsed {
		requested = res.RequestedModel
	}
	stub := map[string]any{
		"id":      "chatcmpl-monitor-stub",
		"object":  "chat.completion",
		"model":   requested,
		"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "[monitor] echo stub"}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
	out, _ := json.Marshal(stub)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(out)))
	w.WriteHeader(200)
	_, _ = w.Write(out)
	if at, ok := g.captureLocalTelemetryAttemptV1(cl, start, requested); ok {
		status := 200
		stopReason := "stop"
		g.recordTelemetryV1(cl, at, telemetryCompletionV1{
			completedAt:      time.Now(),
			status:           &status,
			responseComplete: true,
			usageComplete:    true,
			usage: observedGatewayUsageV1(
				usage.Usage{},
				true,
				false,
			),
			providerStopReason: &stopReason,
			requestBytes:       len(body),
		})
	}
}

// monitorStubOpenAIResponses returns a 200 OpenAI Responses-shape
// stub for monitor-route listeners and records a telemetry row.
// Minimal valid-ish Responses API body: codex clients only need a
// completed status and an output array; the monitor route is
// local-echo only.
func (g *Gateway) monitorStubOpenAIResponses(cl *clientListener, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, _ := io.ReadAll(r.Body)
	requested := ""
	if res := proxy.RewriteModelInBody(body, ""); res.Parsed {
		requested = res.RequestedModel
	}
	stub := map[string]any{
		"id":     "resp_monitor_stub",
		"object": "response",
		"status": "completed",
		"model":  requested,
		"output": []any{},
	}
	out, _ := json.Marshal(stub)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(out)))
	w.WriteHeader(200)
	_, _ = w.Write(out)
	if at, ok := g.captureLocalTelemetryAttemptV1(cl, start, requested); ok {
		status := 200
		stopReason := "stop"
		g.recordTelemetryV1(cl, at, telemetryCompletionV1{
			completedAt:      time.Now(),
			status:           &status,
			responseComplete: true,
			usageComplete:    true,
			usage: observedGatewayUsageV1(
				usage.Usage{},
				true,
				false,
			),
			providerStopReason: &stopReason,
			requestBytes:       len(body),
		})
	}
}

func (g *Gateway) Serve(ctx context.Context) error {
	g.ctx = ctx
	g.wg.Add(1)
	go g.runAuthTick(ctx)
	errCh := make(chan error, 2)
	go func() { errCh <- g.adminServer.Serve(g.adminListener) }()
	for _, lg := range g.snapshotGroups() {
		lg := lg
		go func() { errCh <- lg.serve() }()
	}
	for {
		select {
		case <-ctx.Done():
			return g.Shutdown(context.Background())
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				continue
			}
			return err
		}
	}
}

func (g *Gateway) snapshotClients() []*clientListener {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*clientListener, 0, len(g.clients))
	seen := make(map[string]bool, len(g.clients))
	for _, name := range g.clientOrder {
		if cl, ok := g.clients[name]; ok {
			out = append(out, cl)
			seen[name] = true
		}
	}
	names := make([]string, 0, len(g.clients)-len(out))
	for name := range g.clients {
		if !seen[name] {
			names = append(names, name)
		}
	}
	for _, name := range sortedKeys(names) {
		out = append(out, g.clients[name])
	}
	return out
}

func (g *Gateway) snapshotGroups() []*listenerGroup {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*listenerGroup, 0, len(g.groups))
	for _, lg := range g.groups {
		out = append(out, lg)
	}
	return out
}

func (g *Gateway) Shutdown(ctx context.Context) error {
	g.authTickStopOnce.Do(func() { close(g.authTickStop) })
	g.stopCatalogRefresh()
	g.stopPublicCatalogRefresh()
	var firstErr error
	if err := g.adminServer.Shutdown(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	for _, lg := range g.snapshotGroups() {
		if err := lg.server.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	if err := g.closeTelemetryWriter(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// loadResolvedClients reads gateway.yaml and produces one
// resolvedClientConfig per enabled client entry. A missing or
// malformed config file is a hard error, never an in-memory default
// (no-config landmine fix: defaults would bind the configured door ports,
// the door's own ports). As a side effect it overlays parsed values
// from global.* onto *cfg so the
// caller's Config reflects the active source of truth (YAML wins
// over env when both are set).
func loadResolvedClients(cfg *Config) ([]resolvedClientConfig, error) {
	snapshot, err := loadResolvedConfigSnapshot(cfg)
	if err != nil {
		return nil, err
	}
	return snapshot.clients, nil
}

func loadResolvedConfigSnapshot(cfg *Config) (*resolvedConfigSnapshot, error) {
	path := cfg.ConfigPath
	if path == "" {
		path = config.DefaultPath()
	}
	f, raw, err := readConfigSnapshot(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New(config.MissingConfigMessage(path))
		}
		return nil, errors.New(config.MalformedConfigMessage(path, err))
	}
	resolved, err := loadResolvedClientsInto(cfg, f, path)
	if err != nil {
		return nil, err
	}
	return &resolvedConfigSnapshot{file: f, raw: raw, clients: resolved}, nil
}

func loadResolvedClientsInto(cfg *Config, f *config.File, path string) ([]resolvedClientConfig, error) {
	if f.Global.TelemetryDir != "" {
		cfg.TelemetryDir = config.ExpandPath(f.Global.TelemetryDir)
	} else if cfg.TelemetryDir == "" {
		cfg.TelemetryDir = config.DefaultTelemetryDir()
	}
	telemetryEnabled := config.IsTelemetryEnabled(f.Global)
	cfg.TelemetryEnabled = &telemetryEnabled
	if f.Global.TelemetryRetentionDays > 0 {
		cfg.TelemetryRetentionDays = f.Global.TelemetryRetentionDays
	} else if cfg.TelemetryRetentionDays <= 0 {
		cfg.TelemetryRetentionDays = config.DefaultTelemetryRetentionDays
	}
	resolved, err := resolveFromFile(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return resolved, nil
}

// resolveFromFile turns a parsed config.File into a slice of
// resolvedClientConfig for the enabled clients, applying global
// defaults for missing per-client fields. Invalid model_aliases are a
// hard error, not a lenient skip: a bad alias either silently never
// reaches the picker or silently reroutes native requests, and both
// failure modes are exactly what aliases exist to remove
// (the model-discovery contract).
func resolveFromFile(f *config.File) ([]resolvedClientConfig, error) {
	if f == nil {
		return nil, errors.New("internal: resolveFromFile called with nil config file")
	}
	if err := config.ValidateRoutingPolicy(f); err != nil {
		return nil, err
	}
	globalTTFT, err := parseTTFTTimeout("global.ttft_timeout", f.Global.TTFTTimeout)
	if err != nil {
		return nil, err
	}
	out := make([]resolvedClientConfig, 0, len(f.Clients))
	claimed := map[string]string{} // bind_addr + shape -> owning client
	for _, c := range f.Clients {
		if !c.Enabled {
			continue
		}
		ttft := globalTTFT
		if c.TTFTTimeout != "" {
			ttft, err = parseTTFTTimeout(fmt.Sprintf("client %q ttft_timeout", c.Name), c.TTFTTimeout)
			if err != nil {
				return nil, err
			}
		}
		shape := c.ProtocolShape
		// A shared bind_addr needs disjoint path sets, which only
		// distinct shapes provide; a same-shape pair is a config error
		// and the later client is skipped (same leniency as invalid
		// fallback_route).
		if bindAddrSharable(c.BindAddr) {
			k := c.BindAddr + "\x00" + shape
			if prev, ok := claimed[k]; ok {
				fmt.Fprintf(os.Stderr,
					"[gateway] client %s: bind_addr %s already claimed by %s with shape %s; skipping\n",
					c.Name, c.BindAddr, prev, shape)
				continue
			}
			claimed[k] = c.Name
		}
		rt := "baseten"
		if !*f.Global.RoutingEnabled {
			rt = config.NativeRoute(shape)
		}
		if err := validateModelAliases(c, shape); err != nil {
			return nil, err
		}
		if err := validateSubagentConfig(c, shape); err != nil {
			return nil, err
		}
		if err := validateModelRoutes(c, shape); err != nil {
			return nil, err
		}
		if err := validateResponsesStripToolTypes(c, shape); err != nil {
			return nil, err
		}
		responsesCompatibility, err := config.ResolveResponsesCompatibility(c.ResponsesCompatibility)
		if err != nil {
			return nil, fmt.Errorf("client %q: %w", c.Name, err)
		}
		sanitize := true
		if c.SanitizeHistory != nil {
			sanitize = *c.SanitizeHistory
		}
		// A fallback_route equal to the configured route is NOT cleared
		// here: whether the fallback is inert is a request-time decision
		// against the EFFECTIVE primary route, which a model_routes pin
		// can move off the configured route (config/schema.md, model route
		// fallback semantics; resolveAttemptsLadder). fb == rt is the
		// designed dormant state for requests whose effective primary
		// remains on rt.
		fb := c.FallbackRoute
		if fb != "" && (fb == "monitor" || !shapeCompatible(shape, fb)) {
			fmt.Fprintf(os.Stderr,
				"[gateway] client %s: ignoring fallback_route %q (must be a real route other than monitor and be shape-compatible with %s)\n",
				c.Name, fb, shape)
			fb = ""
		}
		// upstream_shape stays live on non-baseten routes when
		// model_aliases exist: explicit alias/slug choices still produce
		// baseten attempts with the switch off.
		us := c.UpstreamShape
		if us != "" && ((rt != "baseten" && len(c.ModelAliases) == 0) || (us != "anthropic" && us != "openai") ||
			(shape == "openai" && us == "anthropic")) {
			fmt.Fprintf(os.Stderr,
				"[gateway] client %s: ignoring upstream_shape %q (baseten route or model_aliases only, anthropic|openai, reverse translation not implemented)\n",
				c.Name, us)
			us = ""
		}
		out = append(out, resolvedClientConfig{
			Name:                    c.Name,
			BindAddr:                c.BindAddr,
			ProtocolShape:           shape,
			Route:                   rt,
			HasGlobalRoutingGate:    true,
			GlobalRoutingEnabled:    *f.Global.RoutingEnabled,
			DefaultModel:            c.DefaultModel,
			ModelAliases:            c.ModelAliases,
			SubagentModel:           c.SubagentModel,
			SubagentRouting:         c.SubagentRouting,
			ModelRoutes:             c.ModelRoutes,
			ModelOptions:            cloneModelOptions(c.ModelOptions),
			SanitizeHistory:         sanitize,
			FallbackRoute:           fb,
			UpstreamShape:           us,
			ResponsesStripToolTypes: c.ResponsesStripToolTypes,
			ResponsesCompatibility:  responsesCompatibility,
			TTFTTimeout:             ttft,
		})
	}
	return out, nil
}

// discoveryPrefixOK mirrors Claude Code's gateway model discovery
// filter: ids not beginning with "claude" or "anthropic" are dropped before
// caching.
func discoveryPrefixOK(id string) bool {
	return strings.HasPrefix(id, "claude") || strings.HasPrefix(id, "anthropic")
}

// modelFamilySet is the per-family routing taxonomy in config/schema.md:
// the four Claude Code model families the gateway can pin
// independently. Instant and mythos are reserved for later extension and
// are deliberately not pin families yet.
var modelFamilySet = []string{"fable", "opus", "sonnet", "haiku"}

// bracketSuffixRe matches one trailing harness context selection like [1m].
// Claude Code normally removes this decoration before provider inference. If
// it reaches the gateway, Baseten captures it separately and strips it from the
// canonical provider model.
var bracketSuffixRe = regexp.MustCompile(`\[[^\]]*\]$`)

// normalizeModelID strips one trailing bracketed suffix (for example
// [1m]) from a model id. The suffix is not part of the provider model
// identity, so routing, catalog lookup, and upstream inference use the
// normalized form.
func normalizeModelID(id string) string {
	return bracketSuffixRe.ReplaceAllString(id, "")
}

// familyOf returns the first family token from modelFamilySet contained
// in the lowercased normalized id, else "". The first-match rule covers
// both current (claude-sonnet-4-6) and dated (claude-3-5-sonnet-20241022)
// naming, since the family word is a substring of the id on both shapes.
// An id with no recognizable family token is not family-routed and
// follows the switch. Normalization runs first so a [1m] suffix does not
// interfere with token extraction.
func familyOf(id string) string {
	norm := strings.ToLower(normalizeModelID(id))
	for _, fam := range modelFamilySet {
		if strings.Contains(norm, fam) {
			return fam
		}
	}
	return ""
}

// reservedAnthropicModelRe matches real Anthropic model id shapes
// (claude-opus-4-8, claude-3-5-sonnet-20241022, claude-instant-1.2,
// claude-fable-5, claude-mythos-5). An alias equal to a real model
// would hijack native requests even with the switch off (explicit
// choice wins), so these are rejected at config load. Extend this
// set when Anthropic ships a new family name; otherwise an alias can shadow
// a native model.
var reservedAnthropicModelRe = regexp.MustCompile(`^claude-([0-9]|opus|sonnet|haiku|instant|fable|mythos)`)

// validateModelAliases enforces the the model-discovery contract alias
// rules on one client entry. Errors are load failures: startup
// refuses, SIGHUP keeps the running listeners.
func validateModelAliases(c config.Client, shape string) error {
	if len(c.ModelAliases) == 0 {
		return nil
	}
	if shape != "anthropic" {
		return fmt.Errorf("client %q: model_aliases requires protocol_shape anthropic (got %q); aliases serve Claude Code's /v1/models discovery", c.Name, shape)
	}
	ids := make([]string, 0, len(c.ModelAliases))
	for id := range c.ModelAliases {
		ids = append(ids, id)
	}
	for _, id := range sortedKeys(ids) {
		if c.ModelAliases[id] == "" {
			return fmt.Errorf("client %q: model_aliases[%q] has an empty Baseten slug", c.Name, id)
		}
		if !discoveryPrefixOK(id) {
			return fmt.Errorf("client %q: model_aliases id %q would be dropped by Claude Code's discovery filter (ids must begin with \"claude\" or \"anthropic\"); rename it, e.g. claude-baseten-%s", c.Name, id, id)
		}
		if reservedAnthropicModelRe.MatchString(id) {
			return fmt.Errorf("client %q: model_aliases id %q collides with real Anthropic model names and would hijack native requests; use the claude-baseten-/anthropic-baseten- namespace instead", c.Name, id)
		}
	}
	return nil
}

// validateSubagentConfig enforces the the subagent-routing contract rules on
// the subagent_model/subagent_routing fields. Errors are load failures:
// startup refuses, SIGHUP keeps the running listeners, matching
// validateModelAliases strictness.
func validateSubagentConfig(c config.Client, shape string) error {
	if c.SubagentModel == "" && c.SubagentRouting == "" {
		return nil
	}
	if shape != "anthropic" {
		return fmt.Errorf("client %q: subagent_model requires protocol_shape anthropic (got %q); subagent routing serves Claude Code sidechain requests (the subagent-routing contract)", c.Name, shape)
	}
	switch c.SubagentRouting {
	case "", "on", "off":
	default:
		return fmt.Errorf("client %q: subagent_routing %q must be \"on\" or \"off\" (or omitted, meaning on when subagent_model is set); fix the value in gateway.yaml", c.Name, c.SubagentRouting)
	}
	if c.SubagentRouting != "" && c.SubagentModel == "" {
		return fmt.Errorf("client %q: subagent_routing is set but subagent_model is empty; set subagent_model or remove subagent_routing in gateway.yaml", c.Name)
	}
	if c.SubagentModel == "" {
		return nil
	}
	// Three value classes, same as the CLI: a configured alias, a raw
	// Baseten slug (contains "/"), or a native claude-*/anthropic-* id.
	if _, ok := c.ModelAliases[c.SubagentModel]; ok {
		return nil
	}
	if InAliasNamespace(c.SubagentModel) {
		ids := make([]string, 0, len(c.ModelAliases))
		for a := range c.ModelAliases {
			ids = append(ids, a)
		}
		return fmt.Errorf("client %q: subagent_model %q is in the gateway alias namespace but absent from model_aliases; add it to model_aliases or pick another model. Configured model_aliases: [%s]", c.Name, c.SubagentModel, strings.Join(sortedKeys(ids), ", "))
	}
	if strings.Contains(c.SubagentModel, "/") {
		return nil
	}
	if discoveryPrefixOK(c.SubagentModel) {
		return nil
	}
	return fmt.Errorf("client %q: subagent_model %q is not a configured alias, a raw Baseten slug (must contain \"/\"), or a native claude-*/anthropic-* id; fix the value in gateway.yaml", c.Name, c.SubagentModel)
}

// ValidModelRouteKey reports whether key is one of the supported
// model_routes family keys.
func ValidModelRouteKey(key string) bool {
	return config.ValidModelRouteKey(key)
}

// isModelFamilyWord reports whether key is one of the bare family words
// in modelFamilySet (fable, opus, sonnet, haiku).
func isModelFamilyWord(key string) bool {
	for _, fam := range modelFamilySet {
		if key == fam {
			return true
		}
	}
	return false
}

// validateModelRoutes enforces the config/schema.md rules on the
// model_routes map. Errors are load failures: startup refuses, SIGHUP
// keeps the running listeners, matching validateSubagentConfig strictness.
// Keys must be family words; values must be "native", a configured
// alias, or a slug with "/". Alias-namespace values absent from
// model_aliases are errors. An empty map is fine.
func validateModelRoutes(c config.Client, shape string) error {
	if len(c.ModelRoutes) == 0 {
		return nil
	}
	if shape != "anthropic" {
		return fmt.Errorf("client %q: model_routes requires protocol_shape anthropic (got %q); family pins serve Claude Code model families (see config/schema.md)", c.Name, shape)
	}
	keys := make([]string, 0, len(c.ModelRoutes))
	for k := range c.ModelRoutes {
		keys = append(keys, k)
	}
	for _, key := range sortedKeys(keys) {
		val := c.ModelRoutes[key]
		if val == "" {
			return fmt.Errorf("client %q: model_routes[%q] has an empty value; use \"native\", a configured alias, or a Baseten slug (contains \"/\")", c.Name, key)
		}
		if !ValidModelRouteKey(key) {
			return fmt.Errorf("client %q: model_routes key %q is invalid (allowed: %s); fix the key in gateway.yaml", c.Name, key, strings.Join(modelFamilySet, ", "))
		}
		// Value classes: native, a configured alias, or a slug with "/".
		if val == "native" {
			continue
		}
		if _, ok := c.ModelAliases[val]; ok {
			continue
		}
		if InAliasNamespace(val) {
			ids := make([]string, 0, len(c.ModelAliases))
			for a := range c.ModelAliases {
				ids = append(ids, a)
			}
			return fmt.Errorf("client %q: model_routes[%q] value %q is in the gateway alias namespace but absent from model_aliases; add it to model_aliases or pick another model. Configured model_aliases: [%s]", c.Name, key, val, strings.Join(sortedKeys(ids), ", "))
		}
		if strings.Contains(val, "/") {
			continue
		}
		return fmt.Errorf("client %q: model_routes[%q] value %q is not \"native\", a configured alias, or a raw Baseten slug (must contain \"/\"); fix the value in gateway.yaml", c.Name, key, val)
	}
	return nil
}

// validateResponsesStripToolTypes enforces the shape rule on the
// responses_strip_tool_types list (the Responses compatibility contract).
// Errors are load failures: startup refuses, SIGHUP keeps the running
// listeners, matching validateModelAliases strictness.
func validateResponsesStripToolTypes(c config.Client, shape string) error {
	if len(c.ResponsesStripToolTypes) == 0 {
		return nil
	}
	if shape != "openai" {
		return fmt.Errorf("client %q: responses_strip_tool_types requires protocol_shape openai (got %q); the strip applies to /v1/responses bodies, which only openai-shape listeners serve (the Responses compatibility contract)", c.Name, shape)
	}
	for _, tt := range c.ResponsesStripToolTypes {
		if tt == "" {
			return fmt.Errorf("client %q: responses_strip_tool_types contains an empty entry; remove it or name a tools[] type (e.g. tool_search)", c.Name)
		}
	}
	return nil
}

// reloadConfig is the in-process equivalent of SIGHUP. Changes on an
// existing bind address atomically replace immutable per-request client
// configs without touching the socket. Topology additions are all bound
// before publication; any bind failure leaves every old listener and the
// active config token untouched. Removed listeners retire only after the
// new topology and active snapshot have been published together.
//
// Env-sourced Config fields are refreshed only when the env var is
// actually set, so values that came from the constructor (test mocks,
// explicit defaults) are preserved across reloads.
func (g *Gateway) reloadConfig() {
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()

	loadDotEnv()
	activeFile, activeRaw, err := readConfigSnapshot(g.activeConfigPath())
	if err != nil {
		g.noteReloadError(err)
		fmt.Fprintf(os.Stderr, "[gateway] config reload failed: %v; keeping current listeners\n", err)
		return
	}
	candidateCfg := g.runtimeConfig()
	desired, err := loadResolvedClientsInto(&candidateCfg, activeFile, g.activeConfigPath())
	if err != nil {
		g.noteReloadError(err)
		fmt.Fprintf(os.Stderr, "[gateway] config reload failed: %v; keeping current listeners\n", err)
		return
	}
	specs := groupResolved(desired)

	g.mu.Lock()
	existing := make(map[string]*listenerGroup, len(g.groups))
	for key, lg := range g.groups {
		existing[key] = lg
	}
	g.mu.Unlock()

	// Prepare every genuinely new socket without publishing any of them.
	// Existing keys retain their listener regardless of policy/content changes.
	prepared := map[string]*listenerGroup{}
	for _, spec := range specs {
		if _, ok := existing[spec.key]; ok {
			continue
		}
		lg, bindErr := g.prepareGroup(spec)
		if bindErr != nil {
			for _, candidate := range prepared {
				_ = candidate.listener.Close()
			}
			err = fmt.Errorf("bind client %q at %s: %w", spec.displayName(), spec.addr, bindErr)
			g.noteReloadError(err)
			fmt.Fprintf(os.Stderr, "[gateway] config reload failed: %v; keeping current listeners\n", err)
			return
		}
		prepared[spec.key] = lg
	}

	// Apply validated auth/environment values only after topology preparation
	// succeeds, so a rejected reload cannot partially mutate runtime config.
	applyConfigEnv(activeFile)
	if v := os.Getenv("BASETEN_API_KEY"); v != "" {
		candidateCfg.BasetenKey = v
	}
	if v := os.Getenv("BASETEN_BASE_URL"); v != "" {
		candidateCfg.BasetenURL = v
	}
	if v := os.Getenv("ANTHROPIC_API_BASE_URL"); v != "" {
		candidateCfg.AnthropicURL = v
	}
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		candidateCfg.OpenAIURL = v
	}

	// Publish every resolver slice, the topology maps, and the active token
	// under one routing barrier. Requests already holding an old client
	// pointer finish on that immutable snapshot; later requests see new state.
	// Retired listeners stop accepting before that state is published. Their
	// existing keep-alive connections remain alive long enough for the handler
	// below to reject future requests instead of serving stale routing.
	var retired []*listenerGroup
	var added []*listenerGroup
	g.routingMu.Lock()
	g.mu.Lock()
	nextGroups := make(map[string]*listenerGroup, len(specs))
	nextClients := make(map[string]*clientListener, len(desired))
	for _, spec := range specs {
		lg := existing[spec.key]
		if lg == nil {
			lg = prepared[spec.key]
			added = append(added, lg)
		}
		clients := lg.replaceClientConfigs(spec.cfgs)
		nextGroups[spec.key] = lg
		for _, cl := range clients {
			nextClients[cl.name] = cl
		}
	}
	for key, lg := range g.groups {
		if _, keep := nextGroups[key]; !keep {
			retired = append(retired, lg)
		}
	}
	for _, lg := range retired {
		lg.retired.Store(true)
		lg.stopAccepting()
	}
	g.groups = nextGroups
	g.clients = nextClients
	g.clientOrder = g.clientOrder[:0]
	for _, rc := range desired {
		g.clientOrder = append(g.clientOrder, rc.Name)
	}
	g.mu.Unlock()
	g.setRuntimeConfig(candidateCfg)
	g.activateConfigSnapshot(activeFile, activeRaw)
	g.routingMu.Unlock()
	g.reconcileTelemetryWriter(candidateCfg)

	for _, lg := range added {
		go func(lg *listenerGroup) {
			if serveErr := lg.serve(); serveErr != nil &&
				!errors.Is(serveErr, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "[gateway] listener %q failed after reload: %v\n", lg.displayName(), serveErr)
			}
		}(lg)
		for _, cl := range lg.snapshotClients() {
			fmt.Fprintf(os.Stderr, "[gateway] (re)bound listener %q at %s shape=%s route=%s\n",
				cl.name, cl.Addr().String(), cl.cfg.ProtocolShape, cl.cfg.Route)
		}
	}
	for _, lg := range retired {
		go func(lg *listenerGroup) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = lg.server.Shutdown(ctx)
			fmt.Fprintf(os.Stderr, "[gateway] closed listener %q\n", lg.displayName())
		}(lg)
	}

	g.refreshAuth()
	runtimeCfg := g.runtimeConfig()
	runPreflight(&runtimeCfg, desired, os.Stderr)
}

func (g *Gateway) snapshotGroup(key string) *listenerGroup {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.groups[key]
}

// validateAdminAddrLoopback prevents the unauthenticated admin API from being
// exposed beyond the local machine.
func validateAdminAddrLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid admin address %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("admin address %q must use a loopback host", addr)
	}
	return nil
}

// Run is the gateway entrypoint. It binds the admin listener at
// cfg.AdminAddr (default 127.0.0.1:45273), reads gateway.yaml, spawns one HTTP
// listener per enabled client, and serves until SIGINT/SIGTERM.
func Run(cfg Config) error {
	if cfg.AdminAddr == "" {
		cfg.AdminAddr = DefaultAdminAddr
	}
	if err := validateAdminAddrLoopback(cfg.AdminAddr); err != nil {
		return err
	}
	if cfg.ConfigPath == "" {
		cfg.ConfigPath = config.DefaultPath()
	}
	// Parse and resolve one exact byte snapshot before any potentially slow
	// startup work. The same snapshot constructs listeners and becomes the
	// active hash/token, so a concurrent file edit is exposed as desired
	// versus active instead of being mislabeled as live.
	snapshot, err := loadResolvedConfigSnapshot(&cfg)
	if err != nil {
		return err
	}
	resolved := snapshot.clients
	// Apply global.auth from those same accepted bytes before anything reads
	// cfg.BasetenKey (pricing hydration, listeners).
	applyConfigEnv(snapshot.file)
	if v := os.Getenv("BASETEN_API_KEY"); v != "" {
		cfg.BasetenKey = v
	}
	adminL, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		return fmt.Errorf("bind admin %s: %w", cfg.AdminAddr, err)
	}
	p := pricing.New()
	loadProviderCatalogCaches(p, cfg.ConfigPath)
	if err := pidfile.WriteAt(cfg.PidFile, os.Getpid()); err != nil {
		return fmt.Errorf("write pidfile: %w", err)
	}
	defer pidfile.UnlinkAt(cfg.PidFile)
	// Record which config file this process resolved so a later
	// `gateway start` without an explicit BASETEN_SWITCH_CONFIG_PATH reuses it
	// instead of silently switching configs. Deliberately not removed
	// on shutdown: it is memory of last intent, not a lock.
	if err := pidfile.WriteConfigState(cfg.PidFile, cfg.ConfigPath); err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] warning: could not record config path next to pidfile: %v\n", err)
	}
	// Fail-fast preflight (warn-only): unresolved ${VAR} placeholders and
	// baseten-routed clients without a usable credential.
	runPreflight(&cfg, resolved, os.Stderr)
	g, err := newGatewayWithSnapshot(cfg, p, adminL, resolved, snapshot)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr,
		"[gateway] admin on %s; baseten=%s anthropic=%s openai=%s config_path=%s clients=%d\n",
		cfg.AdminAddr, cfg.BasetenURL, cfg.AnthropicURL, cfg.OpenAIURL, cfg.ConfigPath, len(resolved))
	for _, cl := range g.snapshotClients() {
		fmt.Fprintf(os.Stderr, "[gateway] listener %q on %s shape=%s route=%s\n",
			cl.name, cl.Addr().String(), cl.cfg.ProtocolShape, cl.cfg.Route)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Production owns the catalog lifecycle. New/Serve remain side-effect
	// free for embedders and tests that provide synthetic upstream handlers.
	g.startCatalogRefresh(ctx)
	g.startPublicCatalogRefresh(ctx)
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for sig := range sc {
			if sig == syscall.SIGHUP {
				fmt.Fprintln(os.Stderr, "[gateway] SIGHUP received, reloading")
				g.reloadConfig()
				continue
			}
			fmt.Fprintln(os.Stderr, "[gateway] shutting down")
			cancel()
		}
	}()
	g.markStarted()
	return g.Serve(ctx)
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
