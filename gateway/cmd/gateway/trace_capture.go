package gateway

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/proxy"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
)

const traceCaptureMemoryLimit int64 = 256 << 20

const traceJanitorInterval = 6 * time.Hour

const traceCaptureWarning = "Captured bodies may contain prompts, responses, reasoning summaries, tool definitions, tool arguments and results, source code, files, terminal output, MCP output, images, documents, credentials pasted into prompts, personal data, and regulated data."

type traceRequestCapture struct {
	mu sync.Mutex

	generation      uint64
	eventID         string
	startedAt       time.Time
	client          string
	shape           tracecapture.ProtocolShape
	kind            tracecapture.APIKind
	endpoint        string
	configuredRoute string
	requestedModel  string

	nativeCorrelation *tracecapture.NativeCorrelationV1
	providerStateful  bool
	request           traceCapturedBody
	response          traceCapturedBody
	status            *int
	responseWriteErr  bool
	finalized         bool

	gateway *Gateway
}

type traceCapturedBody struct {
	boundary        string
	contentType     string
	contentEncoding string
	bytes           []byte
	observed        int64
	state           tracecapture.CaptureState
	reserved        int64
}

func resolveTraceDirectory(configPath string) (string, error) {
	if configPath == "" {
		configPath = config.DefaultPath()
	}
	paths, err := tracecapture.ResolveRuntimePaths(config.ExpandPath(configPath))
	if err != nil {
		return "", err
	}
	return paths.TraceDir, nil
}

func sameTraceClients(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, client := range a {
		seen[client] = struct{}{}
	}
	for _, client := range b {
		if _, ok := seen[client]; !ok {
			return false
		}
	}
	return true
}

func traceClientAllowed(policy config.ResolvedTraceCapture, client string) bool {
	if !policy.Enabled {
		return false
	}
	for _, allowed := range policy.Clients {
		if allowed == client {
			return true
		}
	}
	return false
}

func (g *Gateway) initializeTraceCapture(runtimeCfg Config) error {
	policy := runtimeCfg.TraceCapture
	g.traceMu.Lock()
	g.tracePolicy = policy
	g.traceDir = runtimeCfg.TraceDir
	g.traceGeneration = 1
	g.traceWriterTransition = 1
	g.traceMu.Unlock()
	if !policy.Enabled {
		g.sweepDisabledTraceStore()
		return nil
	}
	if err := g.openTraceWriter(runtimeCfg, 0); err != nil {
		return errors.New(traceCaptureErrorCode(err))
	}
	return nil
}

func (g *Gateway) runTraceJanitor(ctx context.Context) {
	defer g.wg.Done()
	ticker := time.NewTicker(traceJanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.traceJanitorStop:
			return
		case <-ticker.C:
			g.sweepDisabledTraceStore()
		}
	}
}

func (g *Gateway) sweepDisabledTraceStore() {
	g.traceMu.Lock()
	if g.traceWriter != nil || g.traceDir == "" {
		g.traceMu.Unlock()
		return
	}
	dir := g.traceDir
	retentionDays := g.tracePolicy.RetentionDays
	g.traceMu.Unlock()
	if retentionDays <= 0 {
		retentionDays = tracecapture.DefaultRetentionDays
	}
	paths, err := tracecapture.ResolveRuntimePaths(g.activeConfigPath())
	if err != nil || paths.TraceDir != dir {
		return
	}
	if err := tracecapture.ValidateRuntimeTraceStore(paths); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "[gateway] trace retention sweep skipped: %s\n", traceCaptureErrorCode(err))
		}
		return
	}
	if _, err := tracecapture.SweepRetention(dir, retentionDays, time.Now()); err != nil &&
		!errors.Is(err, os.ErrNotExist) && !errors.Is(err, tracecapture.ErrStoreLocked) {
		fmt.Fprintf(os.Stderr, "[gateway] trace retention sweep failed: %s\n", traceCaptureErrorCode(err))
	}
}

func (g *Gateway) openTraceWriter(runtimeCfg Config, expectedTransition uint64) error {
	g.traceOpenMu.Lock()
	defer g.traceOpenMu.Unlock()
	writer, key, dir, correlationErr, err := createTraceWriter(runtimeCfg)
	if err != nil {
		return err
	}
	g.traceMu.Lock()
	if g.traceStopped || !g.tracePolicy.Enabled || g.traceDir != dir ||
		(expectedTransition != 0 && (g.traceWriterTransition != expectedTransition ||
			g.tracePolicy.RetentionDays != runtimeCfg.TraceCapture.RetentionDays ||
			!sameTraceClients(g.tracePolicy.Clients, runtimeCfg.TraceCapture.Clients))) {
		g.traceMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		writer.Close(ctx)
		return nil
	}
	g.traceWriter = writer
	g.traceCorrelationKey = key
	g.traceCorrelationErr = correlationErr
	g.traceMu.Unlock()
	fmt.Fprintf(os.Stderr, "[gateway] WARNING: local trace capture is enabled. %s\n", traceCaptureWarning)
	return nil
}

func createTraceWriter(
	runtimeCfg Config,
) (*tracecapture.Writer, *tracecapture.CorrelationKey, string, string, error) {
	dir := runtimeCfg.TraceDir
	if dir == "" {
		var err error
		dir, err = resolveTraceDirectory(runtimeCfg.ConfigPath)
		if err != nil {
			return nil, nil, "", "", err
		}
	}
	activePath := runtimeCfg.ConfigPath
	if activePath == "" {
		activePath = config.DefaultPath()
	}
	paths, err := tracecapture.ResolveRuntimePaths(config.ExpandPath(activePath))
	if err != nil {
		return nil, nil, "", "", err
	}
	if paths.TraceDir != dir {
		return nil, nil, "", "", errors.New("resolved trace directory does not match active runtime")
	}
	if err := tracecapture.EnsureRuntimeTraceStore(paths); err != nil {
		return nil, nil, "", "", fmt.Errorf("prepare trace store: %w", err)
	}
	key, err := tracecapture.LoadOrCreateCorrelationKey(dir)
	correlationErr := ""
	if err != nil {
		key = nil
		correlationErr = "correlation_key_unavailable"
	}
	writer, err := tracecapture.NewWriter(tracecapture.Config{
		Dir:           dir,
		RetentionDays: runtimeCfg.TraceCapture.RetentionDays,
	})
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("open trace writer: %w", err)
	}
	return writer, key, dir, correlationErr, nil
}

func (g *Gateway) prepareTraceEnable(
	runtimeCfg Config,
) (*tracecapture.Writer, *tracecapture.CorrelationKey, string, error) {
	g.traceMu.Lock()
	needed := runtimeCfg.TraceCapture.Enabled && !g.tracePolicy.Enabled &&
		g.traceWriter == nil && !g.traceStopped
	g.traceMu.Unlock()
	if !needed {
		return nil, nil, "", nil
	}
	g.traceOpenMu.Lock()
	defer g.traceOpenMu.Unlock()
	writer, key, _, correlationErr, err := createTraceWriter(runtimeCfg)
	return writer, key, correlationErr, err
}

func (g *Gateway) installPreparedTraceEnable(
	runtimeCfg Config,
	writer *tracecapture.Writer,
	key *tracecapture.CorrelationKey,
	correlationErr string,
) {
	if writer == nil {
		return
	}
	g.traceMu.Lock()
	g.traceGeneration++
	g.traceWriterTransition++
	g.tracePolicy = runtimeCfg.TraceCapture
	g.traceDir = runtimeCfg.TraceDir
	g.traceWriter = writer
	g.traceCorrelationKey = key
	g.traceCorrelationErr = correlationErr
	g.traceMu.Unlock()
	fmt.Fprintf(os.Stderr, "[gateway] WARNING: local trace capture is enabled. %s\n", traceCaptureWarning)
}

func (g *Gateway) reconcileTraceCapture(runtimeCfg Config) {
	g.traceMu.Lock()
	oldPolicy := g.tracePolicy
	oldDir := g.traceDir
	consentChanged := oldPolicy.Enabled != runtimeCfg.TraceCapture.Enabled ||
		!sameTraceClients(oldPolicy.Clients, runtimeCfg.TraceCapture.Clients)
	writerChanged := oldDir != runtimeCfg.TraceDir ||
		oldPolicy.RetentionDays != runtimeCfg.TraceCapture.RetentionDays
	transitionChanged := consentChanged || writerChanged
	if transitionChanged {
		g.traceWriterTransition++
	}
	transition := g.traceWriterTransition
	if consentChanged {
		g.traceGeneration++
	}
	var invalidated []*traceRequestCapture
	if consentChanged {
		for capture := range g.traceCaptures {
			invalidated = append(invalidated, capture)
		}
	}
	g.tracePolicy = runtimeCfg.TraceCapture
	g.traceDir = runtimeCfg.TraceDir
	oldWriter := (*tracecapture.Writer)(nil)
	if g.traceWriter != nil &&
		(!runtimeCfg.TraceCapture.Enabled || writerChanged) {
		oldWriter = g.traceWriter
		g.traceLastStatus = oldWriter.Status()
		g.traceClosing = true
		g.traceWriter = nil
		g.traceCorrelationKey = nil
		g.traceCorrelationErr = ""
	}
	needOpen := runtimeCfg.TraceCapture.Enabled && g.traceWriter == nil && !g.traceStopped && !g.traceClosing
	g.traceMu.Unlock()
	for _, capture := range invalidated {
		capture.invalidateConsent()
	}

	if oldWriter != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result := oldWriter.Close(ctx)
		cancel()
		if result.Error != "" {
			fmt.Fprintf(os.Stderr, "[gateway] trace writer close failed: %s\n", result.Error)
		}
		g.traceMu.Lock()
		g.traceLastStatus = oldWriter.Status()
		g.traceClosing = !result.Drained
		if result.Drained {
			needOpen = g.tracePolicy.Enabled && g.traceWriter == nil && !g.traceStopped
			transition = g.traceWriterTransition
		}
		g.traceMu.Unlock()
		if !result.Drained {
			go g.finishTraceWriterTransition(oldWriter)
			return
		}
	}
	if needOpen {
		runtimeCfg = g.runtimeConfig()
		if err := g.openTraceWriter(runtimeCfg, transition); err != nil {
			fmt.Fprintf(os.Stderr, "[gateway] trace writer failed: %s\n", traceCaptureErrorCode(err))
			g.noteTraceOpenError(err)
		}
	}
}

func (g *Gateway) finishTraceWriterTransition(oldWriter *tracecapture.Writer) {
	result := oldWriter.Close(context.Background())
	g.traceMu.Lock()
	g.traceLastStatus = oldWriter.Status()
	g.traceClosing = false
	reopen := g.tracePolicy.Enabled && g.traceWriter == nil && !g.traceStopped
	transition := g.traceWriterTransition
	g.traceMu.Unlock()
	if result.Error != "" {
		fmt.Fprintf(os.Stderr, "[gateway] trace writer close failed: %s\n", result.Error)
	}
	if reopen {
		runtimeCfg := g.runtimeConfig()
		if err := g.openTraceWriter(runtimeCfg, transition); err != nil {
			fmt.Fprintf(os.Stderr, "[gateway] trace writer failed: %s\n", traceCaptureErrorCode(err))
			g.noteTraceOpenError(err)
		}
	}
}

func (g *Gateway) noteTraceOpenError(err error) {
	g.traceMu.Lock()
	g.traceLastStatus.State = "disabled"
	g.traceLastStatus.LastError = traceCaptureErrorCode(err)
	if g.traceLastStatus.DroppedRecords == nil {
		g.traceLastStatus.DroppedRecords = make(map[string]uint64)
	}
	g.traceMu.Unlock()
}

func (g *Gateway) closeTraceCapture(ctx context.Context) error {
	g.traceMu.Lock()
	g.traceStopped = true
	g.traceGeneration++
	g.traceWriterTransition++
	writer := g.traceWriter
	if writer != nil {
		g.traceLastStatus = writer.Status()
		g.traceClosing = true
	}
	g.traceWriter = nil
	g.traceCorrelationKey = nil
	g.traceCorrelationErr = ""
	g.traceMu.Unlock()
	if writer == nil {
		return nil
	}
	result := writer.Close(ctx)
	g.traceMu.Lock()
	g.traceLastStatus = writer.Status()
	g.traceClosing = false
	g.traceMu.Unlock()
	if result.Error != "" {
		return errors.New(result.Error)
	}
	if !result.Drained {
		return errors.New("trace writer did not drain before shutdown")
	}
	return nil
}

func (g *Gateway) beginTraceCapture(
	cl *clientListener,
	r *http.Request,
	startedAt time.Time,
	body []byte,
	kind tracecapture.APIKind,
) *traceRequestCapture {
	return g.beginTraceCaptureAtGeneration(cl, r, startedAt, body, kind, g.traceAdmissionGeneration(cl))
}

func (g *Gateway) traceAdmissionGeneration(cl *clientListener) uint64 {
	g.traceMu.Lock()
	defer g.traceMu.Unlock()
	if g.traceStopped || g.traceWriter == nil || !traceClientAllowed(g.tracePolicy, cl.cfg.Name) {
		return 0
	}
	return g.traceGeneration
}

func (g *Gateway) beginTraceCaptureAtGeneration(
	cl *clientListener,
	r *http.Request,
	startedAt time.Time,
	body []byte,
	kind tracecapture.APIKind,
	admissionGeneration uint64,
) *traceRequestCapture {
	g.traceMu.Lock()
	if admissionGeneration == 0 || admissionGeneration != g.traceGeneration ||
		g.traceStopped || g.traceWriter == nil ||
		!traceClientAllowed(g.tracePolicy, cl.cfg.Name) {
		g.traceMu.Unlock()
		return nil
	}
	eventID, err := telemetry.NewEventID()
	if err != nil {
		g.traceMu.Unlock()
		fmt.Fprintln(os.Stderr, "[gateway] trace admission failed: random_id_unavailable")
		return nil
	}
	capture := &traceRequestCapture{
		generation:      g.traceGeneration,
		eventID:         eventID,
		startedAt:       startedAt.UTC(),
		client:          cl.cfg.Name,
		shape:           tracecapture.ProtocolShape(cl.cfg.ProtocolShape),
		kind:            kind,
		endpoint:        r.URL.Path,
		configuredRoute: cl.cfg.Route,
		gateway:         g,
		request: traceCapturedBody{
			boundary:        "client_ingress",
			contentType:     traceHeaderValues(r.Header, "Content-Type"),
			contentEncoding: traceHeaderValues(r.Header, "Content-Encoding"),
		},
		response: traceCapturedBody{
			boundary: "gateway_egress",
			state:    tracecapture.CaptureStateCaptured,
		},
	}
	if rewritten := proxy.RewriteModelInBody(body, ""); rewritten.Parsed {
		capture.requestedModel = rewritten.RequestedModel
	}
	capture.request.observeAllLocked(g, body)
	capture.providerStateful = kind == tracecapture.APIKindResponses &&
		responsesRequestUsesProviderState(body)
	capture.nativeCorrelation = nativeTraceCorrelation(
		g.traceCorrelationKey,
		cl.cfg.Name,
		r,
	)
	if g.traceCaptures == nil {
		g.traceCaptures = make(map[*traceRequestCapture]struct{})
	}
	g.traceCaptures[capture] = struct{}{}
	g.traceInFlight++
	g.traceMu.Unlock()
	return capture
}

func traceCaptureErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, tracecapture.ErrStoreLocked):
		return "store_locked"
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EROFS):
		return "permission_denied"
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return "storage_full"
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		return "file_limit_reached"
	default:
		return "storage_unavailable"
	}
}

func (c *traceRequestCapture) invalidateConsent() {
	c.mu.Lock()
	if c.finalized {
		c.mu.Unlock()
		return
	}
	released := c.request.reserved + c.response.reserved
	c.request.bytes = nil
	c.request.reserved = 0
	c.response.bytes = nil
	c.response.reserved = 0
	c.request.state = tracecapture.CaptureStateUnavailable
	c.response.state = tracecapture.CaptureStateUnavailable
	c.mu.Unlock()
	if released == 0 {
		return
	}
	c.gateway.traceMu.Lock()
	c.gateway.traceReservedBytes -= released
	if c.gateway.traceReservedBytes < 0 {
		c.gateway.traceReservedBytes = 0
	}
	c.gateway.traceMu.Unlock()
}

func (b *traceCapturedBody) observeAllLocked(g *Gateway, body []byte) {
	b.observed = int64(len(body))
	if len(body) > tracecapture.MaxBodyBytes {
		b.state = tracecapture.CaptureStateOmittedSizeLimit
		g.incrementTraceBodyOmissionLocked(string(b.state))
		return
	}
	if g.traceReservedBytes+int64(len(body)) > traceCaptureMemoryLimit {
		b.state = tracecapture.CaptureStateOmittedMemoryLimit
		g.incrementTraceBodyOmissionLocked(string(b.state))
		return
	}
	b.bytes = append([]byte(nil), body...)
	b.reserved = int64(len(body))
	g.traceReservedBytes += b.reserved
	b.state = tracecapture.CaptureStateCaptured
}

func (c *traceRequestCapture) ObserveResponseStart(
	status int,
	contentType string,
	contentEncoding string,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finalized || c.status != nil {
		return
	}
	c.status = cloneIntPointer(&status)
	c.response.contentType = contentType
	c.response.contentEncoding = contentEncoding
}

func (c *traceRequestCapture) ObserveResponseBytes(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finalized || len(p) == 0 {
		return
	}
	c.response.observed += int64(len(p))
	if c.response.state != tracecapture.CaptureStateCaptured {
		return
	}
	if c.response.observed > tracecapture.MaxBodyBytes {
		c.gateway.traceMu.Lock()
		c.releaseBodyWithGatewayLocked(&c.response)
		c.response.state = tracecapture.CaptureStateOmittedSizeLimit
		c.gateway.incrementTraceBodyOmissionLocked(string(c.response.state))
		c.gateway.traceMu.Unlock()
		return
	}
	c.gateway.traceMu.Lock()
	if c.gateway.traceReservedBytes+int64(len(p)) > traceCaptureMemoryLimit {
		c.releaseBodyWithGatewayLocked(&c.response)
		c.response.state = tracecapture.CaptureStateOmittedMemoryLimit
		c.gateway.incrementTraceBodyOmissionLocked(string(c.response.state))
		c.gateway.traceMu.Unlock()
		return
	}
	c.response.bytes = append(c.response.bytes, p...)
	c.response.reserved += int64(len(p))
	c.gateway.traceReservedBytes += int64(len(p))
	c.gateway.traceMu.Unlock()
}

func (g *Gateway) incrementTraceBodyOmissionLocked(reason string) {
	if g.traceBodyOmissions == nil {
		g.traceBodyOmissions = make(map[string]uint64)
	}
	g.traceBodyOmissions[reason]++
}

func (c *traceRequestCapture) ObserveResponseWriteError(error) {
	c.mu.Lock()
	c.responseWriteErr = true
	c.mu.Unlock()
}

func (c *traceRequestCapture) releaseBodyLocked(body *traceCapturedBody) {
	c.gateway.traceMu.Lock()
	c.releaseBodyWithGatewayLocked(body)
	c.gateway.traceMu.Unlock()
}

func (c *traceRequestCapture) releaseBodyWithGatewayLocked(body *traceCapturedBody) {
	c.gateway.traceReservedBytes -= body.reserved
	if c.gateway.traceReservedBytes < 0 {
		c.gateway.traceReservedBytes = 0
	}
	body.bytes = nil
	body.reserved = 0
}

func responsesRequestUsesProviderState(body []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return false
	}
	for _, name := range []string{"previous_response_id", "conversation"} {
		value := bytes.TrimSpace(fields[name])
		if len(value) > 0 && !bytes.Equal(value, []byte("null")) &&
			!bytes.Equal(value, []byte(`""`)) {
			return true
		}
	}
	return false
}

func nativeTraceCorrelation(
	key *tracecapture.CorrelationKey,
	client string,
	r *http.Request,
) *tracecapture.NativeCorrelationV1 {
	if key == nil || client != "claude-code" {
		return nil
	}
	result := &tracecapture.NativeCorrelationV1{
		Scheme: "hmac-sha256-v1",
		KeyID:  key.ID(),
	}
	hashHeader := func(target **string, field, header string) {
		value := strings.TrimSpace(r.Header.Get(header))
		if value == "" {
			return
		}
		hash, err := key.Hash(client, field, value)
		if err == nil {
			*target = stringPointer(hash)
		}
	}
	hashHeader(&result.Session, "session", "x-claude-code-session-id")
	hashHeader(&result.Agent, "agent", subagentAgentIDHeader)
	if result.Session == nil && result.Agent == nil {
		return nil
	}
	return result
}

func (c *traceRequestCapture) finalizeDefault() {
	c.finalize(traceFinalizeInput{
		completedAt:     time.Now(),
		configuredRoute: c.configuredRoute,
		requestedModel:  c.requestedModel,
		providerOutcome: tracecapture.ProviderOutcomeFailed,
		gatewayComplete: true,
	})
}

type traceFinalizeInput struct {
	completedAt       time.Time
	configuredRoute   string
	effectiveProvider string
	requestedModel    string
	servedModel       string
	termination       tracecapture.TerminationReason
	outcomeSource     tracecapture.OutcomeSource
	providerOutcome   tracecapture.ProviderOutcome
	isStream          bool
	providerStateful  bool
	translated        bool
	sanitized         bool
	fallbackCount     int
	fallbackTrigger   *string
	primaryAttempted  bool
	protocolComplete  bool
	gatewayComplete   bool
}

func (c *traceRequestCapture) finalize(input traceFinalizeInput) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.finalized {
		c.mu.Unlock()
		return
	}
	c.finalized = true
	if input.completedAt.IsZero() {
		input.completedAt = time.Now()
	}
	if input.termination == "" {
		input.termination = tracecapture.TerminationGatewayError
	}
	if input.outcomeSource == "" {
		input.outcomeSource = tracecapture.OutcomeSourceGateway
	}
	if input.providerOutcome == "" {
		input.providerOutcome = tracecapture.ProviderOutcomeUnknown
	}
	status := cloneIntPointer(c.status)
	request := c.request.bodyV1()
	response := tracecapture.ResponseBodyV1{
		BodyV1:               c.response.bodyV1(),
		GatewayWriteComplete: input.gatewayComplete && !c.responseWriteErr,
		ProtocolComplete:     input.protocolComplete && !c.responseWriteErr,
	}
	trace := tracecapture.TraceV1{
		SchemaVersion:     tracecapture.SchemaVersionV1,
		Event:             tracecapture.EventTraceV1,
		EventID:           c.eventID,
		NativeCorrelation: c.nativeCorrelation,
		StartedAt:         c.startedAt,
		CompletedAt:       input.completedAt.UTC(),
		Client:            c.client,
		ProtocolShape:     c.shape,
		APIKind:           c.kind,
		Endpoint:          c.endpoint,
		ConfiguredRoute:   optionalStringPointer(input.configuredRoute),
		RequestedModel:    optionalStringPointer(input.requestedModel),
		Status:            status,
		TerminationReason: input.termination,
		OutcomeSource:     input.outcomeSource,
		ProviderOutcome:   input.providerOutcome,
		IsStream:          input.isStream,
		ProviderStateful:  c.providerStateful || input.providerStateful,
		Translated:        input.translated,
		Sanitized:         input.sanitized,
		Fallback: tracecapture.FallbackV1{
			Attempted:        input.fallbackCount > 0,
			Count:            input.fallbackCount,
			PrimaryAttempted: input.primaryAttempted,
			Trigger:          cloneStringPointer(input.fallbackTrigger),
		},
		Request:  request,
		Response: response,
	}
	if input.outcomeSource == tracecapture.OutcomeSourceProvider {
		trace.EffectiveProvider = optionalStringPointer(input.effectiveProvider)
		trace.ServedModel = optionalStringPointer(input.servedModel)
	}
	requestReserved := c.request.reserved
	responseReserved := c.response.reserved
	c.request.bytes = nil
	c.request.reserved = 0
	c.response.bytes = nil
	c.response.reserved = 0
	c.mu.Unlock()

	g := c.gateway
	g.traceMu.Lock()
	if g.traceInFlight > 0 {
		g.traceInFlight--
	}
	delete(g.traceCaptures, c)
	eligible := !g.traceStopped && g.traceWriter != nil &&
		g.traceGeneration == c.generation &&
		traceClientAllowed(g.tracePolicy, c.client)
	writer := g.traceWriter
	if eligible {
		release := func() {
			g.traceMu.Lock()
			g.traceReservedBytes -= requestReserved + responseReserved
			if g.traceReservedBytes < 0 {
				g.traceReservedBytes = 0
			}
			g.traceMu.Unlock()
		}
		result := writer.EnqueueWithRelease(trace, release)
		if !result.Accepted {
			g.traceReservedBytes -= requestReserved + responseReserved
			if g.traceReservedBytes < 0 {
				g.traceReservedBytes = 0
			}
			fmt.Fprintf(os.Stderr, "[gateway] trace record dropped: %s\n", result.Reason)
		}
	} else {
		g.traceReservedBytes -= requestReserved + responseReserved
		if g.traceReservedBytes < 0 {
			g.traceReservedBytes = 0
		}
	}
	g.traceMu.Unlock()
}

func (b traceCapturedBody) bodyV1() tracecapture.BodyV1 {
	return tracecapture.BodyV1{
		Boundary:        b.boundary,
		ContentType:     b.contentType,
		ContentEncoding: b.contentEncoding,
		BodyEncoding:    "base64",
		RawBody:         b.bytes,
		ObservedBytes:   b.observed,
		CaptureState:    b.state,
	}
}

func (g *Gateway) finalizeTraceV1(
	cl *clientListener,
	at upstreamAttempt,
	completion telemetryCompletionV1,
) {
	capture := at.traceCapture
	if capture == nil {
		return
	}
	termination := tracecapture.TerminationReason(classifyTelemetryTermination(completion))
	outcomeSource := tracecapture.OutcomeSourceProvider
	if at.traceGatewayOutcome || completion.gatewayFailure || completion.status == nil {
		outcomeSource = tracecapture.OutcomeSourceGateway
	}
	providerOutcome := tracecapture.ProviderOutcomeUnknown
	switch termination {
	case tracecapture.TerminationCompleted:
		providerOutcome = tracecapture.ProviderOutcomeCompleted
	case tracecapture.TerminationIncompleteStream,
		tracecapture.TerminationClientCancelled:
		providerOutcome = tracecapture.ProviderOutcomeIncomplete
	case tracecapture.TerminationUpstreamHTTPError,
		tracecapture.TerminationUpstreamTransportError,
		tracecapture.TerminationGatewayError:
		providerOutcome = tracecapture.ProviderOutcomeFailed
	}
	if observed := capture.responsesProviderOutcome(completion.isStream); observed != "" {
		providerOutcome = observed
	}
	if at.traceGatewayOutcome {
		providerOutcome = tracecapture.ProviderOutcomeUnknown
	}
	primaryAttempted := at.primaryAttempted
	if at.primary != nil {
		primaryAttempted = at.primary.Attempted
	}
	protocolComplete := capture.protocolComplete(
		completion.isStream,
		completion.responseComplete,
		providerOutcome,
	)
	gatewayComplete := !errors.Is(completion.contextErr, context.Canceled)
	capture.finalize(traceFinalizeInput{
		completedAt:       completion.completedAt,
		configuredRoute:   cl.cfg.Route,
		effectiveProvider: at.route,
		requestedModel:    at.telRequestedModel(),
		servedModel:       at.modelForCost,
		termination:       termination,
		outcomeSource:     outcomeSource,
		providerOutcome:   providerOutcome,
		isStream:          completion.isStream,
		providerStateful:  at.providerStateful,
		translated:        completion.translated,
		sanitized:         completion.sanitized,
		fallbackCount:     completion.fallbackCount,
		fallbackTrigger:   completion.fallbackTrigger,
		primaryAttempted:  primaryAttempted,
		protocolComplete:  protocolComplete,
		gatewayComplete:   gatewayComplete,
	})
}

func (c *traceRequestCapture) responsesProviderOutcome(
	isStream bool,
) tracecapture.ProviderOutcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kind != tracecapture.APIKindResponses ||
		c.response.state != tracecapture.CaptureStateCaptured {
		return ""
	}
	body, decodable := decodedTraceResponse(c.response)
	if !decodable {
		return ""
	}
	if isStream {
		switch {
		case sseHasTerminalEvent(body, "response.failed"):
			return tracecapture.ProviderOutcomeFailed
		case sseHasTerminalEvent(body, "response.incomplete"):
			return tracecapture.ProviderOutcomeIncomplete
		case sseHasTerminalEvent(body, "response.completed"):
			return tracecapture.ProviderOutcomeCompleted
		default:
			return ""
		}
	}
	var value struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &value) != nil {
		return ""
	}
	switch value.Status {
	case "completed":
		return tracecapture.ProviderOutcomeCompleted
	case "failed":
		return tracecapture.ProviderOutcomeFailed
	case "incomplete":
		return tracecapture.ProviderOutcomeIncomplete
	default:
		return ""
	}
}

func (c *traceRequestCapture) protocolComplete(
	isStream bool,
	transportComplete bool,
	outcome tracecapture.ProviderOutcome,
) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !transportComplete || c.responseWriteErr ||
		c.response.state != tracecapture.CaptureStateCaptured ||
		c.status == nil || *c.status >= 400 ||
		outcome != tracecapture.ProviderOutcomeCompleted {
		return false
	}
	body, decodable := decodedTraceResponse(c.response)
	if !decodable {
		return false
	}
	if isStream {
		switch c.kind {
		case tracecapture.APIKindMessages:
			return sseHasTerminalEvent(body, "message_stop")
		case tracecapture.APIKindChatCompletions:
			return sseHasTerminalEvent(body, "[DONE]")
		case tracecapture.APIKindResponses:
			return sseHasTerminalEvent(body, "response.completed")
		}
		return false
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(body, &value) != nil {
		return false
	}
	switch c.kind {
	case tracecapture.APIKindMessages:
		var kind string
		return json.Unmarshal(value["type"], &kind) == nil && kind == "message"
	case tracecapture.APIKindChatCompletions:
		var object string
		return json.Unmarshal(value["object"], &object) == nil &&
			object == "chat.completion"
	case tracecapture.APIKindResponses:
		var status string
		return json.Unmarshal(value["status"], &status) == nil && status == "completed"
	}
	return false
}

func decodedTraceResponse(response traceCapturedBody) ([]byte, bool) {
	encoding := strings.ToLower(strings.TrimSpace(response.contentEncoding))
	if encoding == "" || encoding == "identity" {
		return response.bytes, true
	}
	var reader io.ReadCloser
	var err error
	switch encoding {
	case "gzip", "x-gzip":
		reader, err = gzip.NewReader(bytes.NewReader(response.bytes))
	case "deflate":
		reader, err = zlib.NewReader(bytes.NewReader(response.bytes))
		if err != nil {
			reader = flate.NewReader(bytes.NewReader(response.bytes))
			err = nil
		}
	default:
		return nil, false
	}
	if err != nil {
		return nil, false
	}
	defer reader.Close()
	limited := &io.LimitedReader{R: reader, N: 64<<20 + 1}
	decoded, err := io.ReadAll(limited)
	if err != nil || limited.N <= 0 {
		return nil, false
	}
	return decoded, true
}

func sseHasTerminalEvent(body []byte, expected string) bool {
	var eventName string
	var data bytes.Buffer
	flush := func() bool {
		defer func() {
			eventName = ""
			data.Reset()
		}()
		if eventName == expected {
			return true
		}
		payload := bytes.TrimSpace(data.Bytes())
		if expected == "[DONE]" {
			return bytes.Equal(payload, []byte("[DONE]"))
		}
		var value struct {
			Type string `json:"type"`
		}
		return len(payload) > 0 && json.Unmarshal(payload, &value) == nil && value.Type == expected
	}
	for _, rawLine := range bytes.Split(body, []byte{'\n'}) {
		line := bytes.TrimSuffix(rawLine, []byte{'\r'})
		if len(line) == 0 {
			if flush() {
				return true
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			eventName = strings.TrimSpace(string(line[len("event:"):]))
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimSpace(line[len("data:"):]))
		}
	}
	return flush()
}

func (g *Gateway) traceAdminHealth() map[string]any {
	g.traceMu.Lock()
	retentionDays := g.tracePolicy.RetentionDays
	if retentionDays <= 0 {
		retentionDays = tracecapture.DefaultRetentionDays
	}
	status := tracecapture.Status{
		State:           "disabled",
		RetentionDays:   retentionDays,
		DroppedRecords:  map[string]uint64{},
		StoreLimitBytes: tracecapture.DefaultMaxStoreBytes,
	}
	if g.traceWriter != nil {
		status = g.traceWriter.Status()
	} else if g.traceLastStatus.DroppedRecords != nil {
		status = g.traceLastStatus
		status.RetentionDays = retentionDays
	}
	if g.traceClosing {
		status.State = "disabling"
	}
	var lastWriteAt any
	if status.LastSuccessfulWrite != nil {
		lastWriteAt = status.LastSuccessfulWrite.UTC().Format(time.RFC3339Nano)
	}
	var lastError any
	if g.traceCorrelationErr != "" {
		lastError = g.traceCorrelationErr
	} else if status.LastError != "" {
		lastError = status.LastError
	}
	var warning any
	if g.tracePolicy.Enabled {
		warning = traceCaptureWarning
	}
	adapters := make(map[string]bool, len(g.tracePolicy.Clients))
	for _, client := range g.tracePolicy.Clients {
		adapters[client] = client == "claude-code" || client == "codex"
	}
	result := map[string]any{
		"enabled":           g.tracePolicy.Enabled,
		"state":             status.State,
		"store_bytes":       status.StoreBytes,
		"store_limit_bytes": status.StoreLimitBytes,
		"retention_days":    status.RetentionDays,
		"active_segment":    status.ActiveSegment,
		"last_write_at":     lastWriteAt,
		"last_error":        lastError,
		"in_flight":         g.traceInFlight,
		"in_flight_bytes":   g.traceReservedBytes,
		"queued_records":    status.QueuedRecords,
		"queued_bytes":      status.QueuedBytesEstimate,
		"dropped_records":   status.DroppedRecords,
		"body_omissions":    cloneUint64Map(g.traceBodyOmissions),
		"native_adapters":   adapters,
		"warning":           warning,
	}
	g.traceMu.Unlock()
	if paths, err := tracecapture.ResolveRuntimePaths(g.activeConfigPath()); err == nil {
		result["runtime_store_id"] = paths.RuntimeID
		if exports, err := tracecapture.InspectRuntimeExports(paths); err == nil {
			result["packages"] = map[string]any{
				"count": exports.PackageCount, "bytes": exports.PackageBytes,
				"quarantine_count": exports.QuarantineCount,
				"quarantine_bytes": exports.QuarantineBytes,
			}
		}
	}
	return result
}

func cloneUint64Map(source map[string]uint64) map[string]uint64 {
	result := make(map[string]uint64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
