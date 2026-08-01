// Package door implements the `baseten-switch door` front-door reverse proxy that
// owns the harness-facing ports in front of the router listeners.
// Each Door binds one port and knows only static targets: the baseten-switch
// router's internal listener, and the native upstream(s) for the port's
// protocol shape(s). Requests stream to the router while it is healthy
// and fail over to the native upstream when tripped; a shared-shape port
// picks the upstream per path prefix. Port specs come from gateway.yaml's
// door: section or from launch flags. Intentionally boring: no admin
// API, no model logic.
package door

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

type Shape string

const (
	ShapeAnthropic Shape = "anthropic"
	ShapeOpenAI    Shape = "openai"
)

const (
	DefaultAnthropicBase = "https://api.anthropic.com"
	DefaultOpenAIBase    = "https://api.openai.com"
	DefaultCooldown      = 15 * time.Second
	DefaultProbeInterval = 3 * time.Second
	// DefaultMaxReplay caps the request-body buffer kept for replaying a
	// request against the fallback after a trip. Larger bodies go
	// router-only (no failover for that request).
	DefaultMaxReplay = 20 << 20
)

// DefaultFallbackBase returns the hardcoded native upstream for a shape.
func DefaultFallbackBase(shape Shape) string {
	switch shape {
	case ShapeAnthropic:
		return DefaultAnthropicBase
	case ShapeOpenAI:
		return DefaultOpenAIBase
	}
	return ""
}

// fallbackPaths lists the exact request paths the native upstream can
// serve while tripped. Anything else gets a 502 naming the door.
var fallbackPaths = map[Shape]map[string]bool{
	ShapeAnthropic: {
		"/v1/messages":              true,
		"/v1/messages/count_tokens": true,
		"/v1/models":                true,
	},
	ShapeOpenAI: {
		"/v1/chat/completions": true,
		"/v1/responses":        true,
		"/v1/models":           true,
	},
}

// hopByHop headers are stripped in both directions. Unlike the router's
// set (internal/proxy.HopByHop), authorization and x-api-key are NOT
// here: the door never injects credentials, so the harness's own auth
// must pass through verbatim to whichever upstream serves the request.
var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"content-length":      true,
	"host":                true,
}

// FallbackRule maps a request path prefix to the native upstream that
// serves it while tripped. Shape names the provider owning the prefix;
// it drives the /v1/models header disambiguation on shared-shape ports
// and is not part of the /doorz payload.
type FallbackRule struct {
	Prefix string `json:"prefix"`
	Shape  Shape  `json:"-"`
	Base   string `json:"base"`
}

// Config describes one front-door port.
type Config struct {
	ListenAddr   string // e.g. "127.0.0.1:45271"; ":0" allowed for tests
	Shape        Shape  // ignored and re-derived from Fallbacks when rules are set
	RouterTarget string // router listener host:port, e.g. "127.0.0.1:45272"
	FallbackBase string // defaults per shape; overridable for tests
	// Fallbacks routes tripped requests per path prefix (longest match
	// wins). Empty keeps the single-FallbackBase behavior unchanged.
	// When set, FallbackBase serves only allowlisted paths no rule
	// matches (/v1/models on a single-shape port).
	Fallbacks     []FallbackRule
	Cooldown      time.Duration
	ProbeInterval time.Duration
	MaxReplay     int64
	Logf          func(format string, args ...any) // defaults to stderr
}

// Door is a single-port front door. Create with New, bind with Start,
// run with Serve, stop with Shutdown.
type Door struct {
	cfg Config

	// allowFallback is the exact-path set servable while tripped: the
	// shape's allowlist, or the union over the rule shapes.
	allowFallback map[string]bool

	listener net.Listener
	server   *http.Server
	client   *http.Client
	probe    *http.Client

	mu             sync.Mutex
	tripped        bool
	trippedUntil   time.Time
	lastTransition time.Time
	warnedNoReplay bool

	// inflight holds cancel funcs for router forwards still waiting on
	// response headers. A trip cancels them all so a request stuck
	// behind a wedged router is rescued into the fallback replay
	// instead of hanging until the harness gives up. Forwards that
	// received headers are never registered here, so a trip can never
	// cut a live response body. A fixed header deadline would be wrong
	// in this position: the router's response headers legitimately
	// arrive late when a model is slow to first token, and that case
	// belongs to the router's own ttft_timeout, not the door.
	inflightMu  sync.Mutex
	inflight    map[uint64]context.CancelFunc
	inflightSeq uint64

	stopProbe chan struct{}
	stopOnce  sync.Once
	probeDone chan struct{}
}

func New(cfg Config) (*Door, error) {
	if len(cfg.Fallbacks) > 0 {
		// Copy before normalizing so the caller's slice is untouched.
		rules := make([]FallbackRule, len(cfg.Fallbacks))
		copy(rules, cfg.Fallbacks)
		cfg.Fallbacks = rules
		for i, fr := range rules {
			if fr.Shape != ShapeAnthropic && fr.Shape != ShapeOpenAI {
				return nil, fmt.Errorf("door: fallback rule %q: unknown shape %q", fr.Prefix, fr.Shape)
			}
			if !strings.HasPrefix(fr.Prefix, "/") {
				return nil, fmt.Errorf("door: fallback rule prefix %q must start with /", fr.Prefix)
			}
			u, err := url.Parse(fr.Base)
			if err != nil || u.Scheme == "" || u.Host == "" {
				return nil, fmt.Errorf("door: fallback rule %q: bad base %q", fr.Prefix, fr.Base)
			}
			rules[i].Base = strings.TrimRight(fr.Base, "/")
		}
		cfg.Shape = shapeLabel(cfg.Fallbacks)
		if cfg.FallbackBase == "" {
			cfg.FallbackBase = rules[0].Base
			for _, fr := range rules {
				if fr.Shape == ShapeAnthropic {
					cfg.FallbackBase = fr.Base
					break
				}
			}
		}
	} else if cfg.Shape != ShapeAnthropic && cfg.Shape != ShapeOpenAI {
		return nil, fmt.Errorf("door: unknown shape %q", cfg.Shape)
	}
	if _, _, err := net.SplitHostPort(cfg.RouterTarget); err != nil {
		return nil, fmt.Errorf("door: bad router target %q: %v", cfg.RouterTarget, err)
	}
	if cfg.FallbackBase == "" {
		cfg.FallbackBase = DefaultFallbackBase(cfg.Shape)
	}
	u, err := url.Parse(cfg.FallbackBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("door: bad fallback base %q", cfg.FallbackBase)
	}
	cfg.FallbackBase = strings.TrimRight(cfg.FallbackBase, "/")
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultCooldown
	}
	if cfg.ProbeInterval <= 0 {
		cfg.ProbeInterval = DefaultProbeInterval
	}
	if cfg.MaxReplay <= 0 {
		cfg.MaxReplay = DefaultMaxReplay
	}
	if cfg.Logf == nil {
		cfg.Logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}
	// DisableCompression keeps the door a pure byte pipe: the transport
	// never adds Accept-Encoding of its own or transparently decodes.
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 8,
	}
	probeTimeout := 2 * time.Second
	if cfg.ProbeInterval < probeTimeout {
		probeTimeout = cfg.ProbeInterval
	}
	allow := fallbackPaths[cfg.Shape]
	if len(cfg.Fallbacks) > 0 {
		allow = map[string]bool{}
		for _, fr := range cfg.Fallbacks {
			for p := range fallbackPaths[fr.Shape] {
				allow[p] = true
			}
		}
	}
	return &Door{
		cfg:           cfg,
		allowFallback: allow,
		client:        &http.Client{Transport: tr, CheckRedirect: noRedirect},
		probe:         &http.Client{Transport: tr.Clone(), Timeout: probeTimeout},
		inflight:      map[uint64]context.CancelFunc{},
		stopProbe:     make(chan struct{}),
		probeDone:     make(chan struct{}),
	}, nil
}

// shapeLabel is the display shape for a rule set; a shared port shows
// both shapes.
func shapeLabel(rules []FallbackRule) Shape {
	var hasA, hasO bool
	for _, fr := range rules {
		switch fr.Shape {
		case ShapeAnthropic:
			hasA = true
		case ShapeOpenAI:
			hasO = true
		}
	}
	switch {
	case hasA && hasO:
		return ShapeAnthropic + "+" + ShapeOpenAI
	case hasO:
		return ShapeOpenAI
	default:
		return ShapeAnthropic
	}
}

// noRedirect keeps the door transparent: redirects relay to the harness.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// Start binds the listener and starts the background health probe.
func (d *Door) Start() error {
	ln, err := net.Listen("tcp", d.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("door: listen %s: %w", d.cfg.ListenAddr, err)
	}
	d.listener = ln
	d.server = &http.Server{Handler: d}
	go d.probeLoop()
	return nil
}

// Addr returns the bound listener address (useful with ":0").
func (d *Door) Addr() string {
	if d.listener == nil {
		return d.cfg.ListenAddr
	}
	return d.listener.Addr().String()
}

func (d *Door) port() int {
	if d.listener != nil {
		if ta, ok := d.listener.Addr().(*net.TCPAddr); ok {
			return ta.Port
		}
	}
	_, p, err := net.SplitHostPort(d.cfg.ListenAddr)
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(p, "%d", &n)
	return n
}

// Serve blocks serving the bound listener until Shutdown.
func (d *Door) Serve() error {
	if d.listener == nil {
		return errors.New("door: Serve before Start")
	}
	err := d.server.Serve(d.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the probe and gracefully drains in-flight requests.
func (d *Door) Shutdown(ctx context.Context) error {
	d.stopOnce.Do(func() { close(d.stopProbe) })
	var err error
	if d.server != nil {
		err = d.server.Shutdown(ctx)
	}
	select {
	case <-d.probeDone:
	case <-ctx.Done():
	}
	return err
}

// --- trip state ---

// skipRouter reports whether requests should bypass the router entirely
// (tripped and still inside the cooldown window). After the window
// expires the next request probes the router again; the tripped flag
// only clears on a healthy answer (lazy) or a probe success (eager).
func (d *Door) skipRouter() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tripped && time.Now().Before(d.trippedUntil)
}

func (d *Door) tripNow(reason string) {
	now := time.Now()
	d.mu.Lock()
	change := !d.tripped
	d.tripped = true
	d.trippedUntil = now.Add(d.cfg.Cooldown)
	if change {
		d.lastTransition = now
	}
	d.mu.Unlock()
	if change {
		d.cfg.Logf("[baseten-switch door] port %d (%s): tripped, serving fallback %s (%s)",
			d.port(), d.cfg.Shape, d.cfg.FallbackBase, reason)
	}
	// Rescue on every trip (not only transitions): a forward that
	// started inside the cooldown window still deserves the cancel.
	d.rescueInflight()
}

// registerInflight tracks a pre-header router forward so a trip can
// rescue it; dropInflight removes it once headers arrived (or the
// forward returned).
func (d *Door) registerInflight(cancel context.CancelFunc) uint64 {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()
	d.inflightSeq++
	id := d.inflightSeq
	d.inflight[id] = cancel
	return id
}

func (d *Door) dropInflight(id uint64) {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()
	delete(d.inflight, id)
}

// rescueInflight cancels every pre-header router forward. Called on
// trip so requests stuck behind a wedged router fall into the normal
// forward-error replay path instead of hanging.
func (d *Door) rescueInflight() {
	d.inflightMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(d.inflight))
	for _, c := range d.inflight {
		cancels = append(cancels, c)
	}
	d.inflightMu.Unlock()
	for _, c := range cancels {
		c()
	}
}

func (d *Door) clearTrip(how string) {
	now := time.Now()
	d.mu.Lock()
	change := d.tripped
	d.tripped = false
	d.trippedUntil = time.Time{}
	if change {
		d.lastTransition = now
	}
	d.mu.Unlock()
	if change {
		d.cfg.Logf("[baseten-switch door] port %d (%s): recovered, serving router %s (%s)",
			d.port(), d.cfg.Shape, d.cfg.RouterTarget, how)
	}
}

// --- health probe ---

func (d *Door) probeLoop() {
	defer close(d.probeDone)
	t := time.NewTicker(d.cfg.ProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-d.stopProbe:
			return
		case <-t.C:
			d.probeOnce()
		}
	}
}

func (d *Door) probeOnce() {
	req, err := http.NewRequest(http.MethodGet, "http://"+d.cfg.RouterTarget+"/healthz", nil)
	if err != nil {
		return
	}
	resp, err := d.probe.Do(req)
	if err != nil {
		d.tripNow("health probe: " + err.Error())
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		d.clearTrip("health probe ok")
	} else {
		d.tripNow(fmt.Sprintf("health probe returned %d", resp.StatusCode))
	}
}

// --- request handling ---

func (d *Door) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/doorz" {
		d.serveDoorz(w)
		return
	}
	if d.skipRouter() {
		// Inside the cooldown window: straight to fallback, streaming the
		// body (no replay buffering needed, there is no second attempt).
		d.serveFallback(w, r, r.Body, r.ContentLength)
		return
	}

	// Buffer the request body (capped) so it can be replayed against the
	// fallback if the router attempt trips.
	var bodyBuf []byte
	replayable := true
	var routerBody io.Reader = http.NoBody
	contentLength := r.ContentLength
	if r.Body != nil && r.Body != http.NoBody {
		buf, err := io.ReadAll(io.LimitReader(r.Body, d.cfg.MaxReplay+1))
		if err != nil {
			d.writeError(w, "router", http.StatusBadRequest, "baseten-switch door: failed reading request body: "+err.Error())
			return
		}
		if int64(len(buf)) > d.cfg.MaxReplay {
			replayable = false
			d.warnNoReplay()
			routerBody = io.MultiReader(bytes.NewReader(buf), r.Body)
		} else {
			bodyBuf = buf
			routerBody = bytes.NewReader(buf)
			contentLength = int64(len(buf))
		}
	}

	// The forward context is cancelable by a trip (rescueInflight): a
	// request waiting on headers behind a wedged router gets canceled
	// into the replay path below instead of hanging. Registered only
	// until headers arrive, so a trip never cuts a streaming body.
	fwdCtx, fwdCancel := context.WithCancel(r.Context())
	defer fwdCancel()
	inflightID := d.registerInflight(fwdCancel)
	resp, err := d.forward(fwdCtx, r, "http://"+d.cfg.RouterTarget, routerBody, contentLength)
	d.dropInflight(inflightID)
	if err != nil {
		if r.Context().Err() != nil {
			// The harness went away; not a router failure.
			return
		}
		reason := "router request failed: " + err.Error()
		if fwdCtx.Err() != nil {
			// Canceled by rescueInflight: the trip already happened
			// (probe or another request); this one is being rescued.
			reason = "in-flight request rescued while router tripped"
		}
		d.tripNow(reason)
		if replayable {
			d.serveFallback(w, r, bytes.NewReader(bodyBuf), int64(len(bodyBuf)))
		} else {
			d.writeError(w, "router", http.StatusBadGateway,
				"baseten-switch door: router unreachable and request body exceeds the replay cap; no failover for this request")
		}
		return
	}

	// Any HTTP response proves the direct loopback router accepted and
	// classified the request. Status-based fallback belongs to the
	// router; the door only rescues transport and health-probe failures.
	d.clearTrip("router answered")
	d.relay(w, resp, "router")
}

func (d *Door) warnNoReplay() {
	d.mu.Lock()
	warned := d.warnedNoReplay
	d.warnedNoReplay = true
	d.mu.Unlock()
	if !warned {
		d.cfg.Logf("[baseten-switch door] port %d (%s): request body exceeds replay cap (%d bytes); such requests are router-only (no failover)",
			d.port(), d.cfg.Shape, d.cfg.MaxReplay)
	}
}

func (d *Door) serveFallback(w http.ResponseWriter, r *http.Request, body io.Reader, contentLength int64) {
	base, ok := d.fallbackFor(r)
	if !ok {
		d.writeError(w, "fallback", http.StatusBadGateway,
			fmt.Sprintf("baseten-switch door: router unavailable and path %q has no native fallback for shape %s", r.URL.Path, d.cfg.Shape))
		return
	}
	resp, err := d.forward(r.Context(), r, base, body, contentLength)
	if err != nil {
		d.writeError(w, "fallback", http.StatusBadGateway, "baseten-switch door: fallback upstream request failed: "+err.Error())
		return
	}
	d.relay(w, resp, "fallback")
}

// fallbackFor picks the native base for a tripped request. Without
// rules every allowlisted path goes to FallbackBase. With rules the
// longest matching prefix wins; /v1/models has no owning prefix, so a
// port serving both shapes picks by the anthropic-version header and a
// single-shape port uses FallbackBase.
func (d *Door) fallbackFor(r *http.Request) (string, bool) {
	if !d.allowFallback[r.URL.Path] {
		return "", false
	}
	if len(d.cfg.Fallbacks) == 0 {
		return d.cfg.FallbackBase, true
	}
	if r.URL.Path == "/v1/models" {
		var aBase, oBase string
		for _, fr := range d.cfg.Fallbacks {
			switch fr.Shape {
			case ShapeAnthropic:
				if aBase == "" {
					aBase = fr.Base
				}
			case ShapeOpenAI:
				if oBase == "" {
					oBase = fr.Base
				}
			}
		}
		if aBase != "" && oBase != "" {
			if r.Header.Get("anthropic-version") != "" {
				return aBase, true
			}
			return oBase, true
		}
		return d.cfg.FallbackBase, true
	}
	base, bestLen := d.cfg.FallbackBase, -1
	for _, fr := range d.cfg.Fallbacks {
		if len(fr.Prefix) > bestLen && strings.HasPrefix(r.URL.Path, fr.Prefix) {
			base, bestLen = fr.Base, len(fr.Prefix)
		}
	}
	return base, true
}

// forward re-issues the harness request against base, copying method,
// path, query and headers verbatim minus hop-by-hop. Auth headers pass
// through untouched; the door never injects credentials.
func (d *Door) forward(ctx context.Context, r *http.Request, base string, body io.Reader, contentLength int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, r.Method, base+r.URL.RequestURI(), body)
	if err != nil {
		return nil, err
	}
	for k, vs := range r.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	return d.client.Do(req)
}

// relay streams the upstream response to the harness with a per-chunk
// flush so SSE events flow immediately (mirrors the gateway's
// flushWriter idiom).
func (d *Door) relay(w http.ResponseWriter, resp *http.Response, via string) {
	defer resp.Body.Close()
	h := w.Header()
	for k, vs := range resp.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	h.Set("X-Baseten-Switch-Door", via)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

func (d *Door) writeError(w http.ResponseWriter, via string, status int, msg string) {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "baseten_switch_door_error",
			"message": msg,
		},
		"door": map[string]any{
			"port":  d.port(),
			"shape": string(d.cfg.Shape),
		},
	})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Baseten-Switch-Door", via)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Doorz is the /doorz status payload.
type Doorz struct {
	Port                int            `json:"port"`
	Shape               string         `json:"shape"`
	Router              string         `json:"router"`
	FallbackBase        string         `json:"fallback_base"`
	Fallbacks           []FallbackRule `json:"fallbacks,omitempty"`
	Tripped             bool           `json:"tripped"`
	CooldownRemainingMS int64          `json:"cooldown_remaining_ms"`
	LastTransitionUnix  int64          `json:"last_transition_unix"`
	Version             string         `json:"version"`
}

func (d *Door) serveDoorz(w http.ResponseWriter) {
	now := time.Now()
	d.mu.Lock()
	z := Doorz{
		Port:         d.port(),
		Shape:        string(d.cfg.Shape),
		Router:       d.cfg.RouterTarget,
		FallbackBase: d.cfg.FallbackBase,
		Fallbacks:    d.cfg.Fallbacks,
		Tripped:      d.tripped,
		Version:      version.Version,
	}
	if d.tripped && d.trippedUntil.After(now) {
		z.CooldownRemainingMS = d.trippedUntil.Sub(now).Milliseconds()
	}
	if !d.lastTransition.IsZero() {
		z.LastTransitionUnix = d.lastTransition.Unix()
	}
	d.mu.Unlock()
	body, _ := json.Marshal(z)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
