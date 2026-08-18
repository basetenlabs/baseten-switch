package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/responsescompat"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

const responsesGuardPollInterval = 100 * time.Millisecond
const responsesTelemetryCaptureLimit = responsescompat.DefaultMaxEventBytes

type upstreamTTFTWatch struct {
	cancel context.CancelFunc
	timer  *time.Timer
	c      <-chan time.Time
}

func (watch upstreamTTFTWatch) stop() {
	if watch.timer != nil {
		watch.timer.Stop()
	}
}

// startUpstreamSubAttempt waits for response headers under the remaining
// absolute TTFT budget. The returned watch stays armed for the first body byte
// and owns the sub-attempt context. dispatched is true only after client.Do
// has been started; request construction and an already-expired budget return
// false without contacting the upstream.
func startUpstreamSubAttempt(
	ctx context.Context,
	at upstreamAttempt,
	body []byte,
	ttftDeadline time.Time,
) (*http.Response, error, bool, bool, upstreamTTFTWatch) {
	attemptCtx, attemptCancel := context.WithCancel(ctx)
	watch := upstreamTTFTWatch{cancel: attemptCancel}
	upReq, err := http.NewRequestWithContext(
		attemptCtx,
		http.MethodPost,
		at.url,
		bytes.NewReader(body),
	)
	if err != nil {
		attemptCancel()
		return nil, err, false, false, watch
	}
	upReq.Header = at.headers

	if !ttftDeadline.IsZero() {
		remaining := time.Until(ttftDeadline)
		if remaining <= 0 {
			attemptCancel()
			return nil, nil, true, false, watch
		}
		watch.timer = time.NewTimer(remaining)
		watch.c = watch.timer.C
	}

	type doResult struct {
		resp *http.Response
		err  error
	}
	doCh := make(chan doResult, 1)
	go func() {
		resp, err := at.client.Do(upReq)
		doCh <- doResult{resp: resp, err: err}
	}()
	select {
	case result := <-doCh:
		return result.resp, result.err, false, true, watch
	case <-watch.c:
		attemptCancel()
		result := <-doCh
		if result.resp != nil {
			result.resp.Body.Close()
		}
		return result.resp, result.err, true, true, watch
	}
}

func readCompatibilityErrorPrefix(
	body io.ReadCloser,
	limit int64,
	ttftDeadline time.Time,
	cancel context.CancelFunc,
) ([]byte, error, bool) {
	read := func() ([]byte, error) {
		return io.ReadAll(io.LimitReader(body, limit))
	}
	if ttftDeadline.IsZero() {
		prefix, err := read()
		return prefix, err, false
	}
	remaining := time.Until(ttftDeadline)
	if remaining <= 0 {
		cancel()
		_ = body.Close()
		return nil, nil, true
	}
	type result struct {
		prefix []byte
		err    error
	}
	readCh := make(chan result, 1)
	go func() {
		prefix, err := read()
		readCh <- result{prefix: prefix, err: err}
	}()
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case got := <-readCh:
		return got.prefix, got.err, false
	case <-timer.C:
		cancel()
		_ = body.Close()
		<-readCh
		return nil, nil, true
	}
}

type responsesGuardedRelayResult struct {
	collected        *responsesCappedTail
	firstDeltaAt     time.Time
	responseComplete bool
	relayErr         error
}

type responsesUpstreamRead struct {
	chunk []byte
	err   error
}

type responsesCappedTail struct {
	data  []byte
	limit int
}

func newResponsesCappedTail(limit int) *responsesCappedTail {
	return &responsesCappedTail{limit: limit}
}

func (tail *responsesCappedTail) Write(p []byte) {
	if tail == nil || tail.limit <= 0 || len(p) == 0 {
		return
	}
	if len(p) >= tail.limit {
		tail.data = append(tail.data[:0], p[len(p)-tail.limit:]...)
		return
	}
	overflow := len(tail.data) + len(p) - tail.limit
	if overflow > 0 {
		copy(tail.data, tail.data[overflow:])
		tail.data = tail.data[:len(tail.data)-overflow]
	}
	tail.data = append(tail.data, p...)
}

func (tail *responsesCappedTail) Bytes() []byte {
	if tail == nil {
		return nil
	}
	return tail.data
}

type responsesOutputDeltaDetector struct {
	pending []byte
	found   bool
}

func (detector *responsesOutputDeltaDetector) Feed(chunk []byte) bool {
	if detector == nil || detector.found {
		return detector != nil && detector.found
	}
	detector.pending = append(detector.pending, chunk...)
	for {
		lineEnd := bytes.IndexByte(detector.pending, '\n')
		if lineEnd < 0 {
			break
		}
		line := detector.pending[:lineEnd+1]
		detector.pending = detector.pending[lineEnd+1:]
		if containsOutputDelta(line) {
			detector.found = true
			detector.pending = nil
			return true
		}
	}
	if len(detector.pending) > responsescompat.DefaultMaxEventBytes {
		detector.pending = nil
	}
	return false
}

// relayResponsesSSE feeds only bytes emitted by the guard to the harness and
// usage parser. A small independent poll enforces the guard's hold deadline
// even when the upstream stalls after a premature done event.
func relayResponsesSSE(
	ctx context.Context,
	body io.ReadCloser,
	w http.ResponseWriter,
	flusher http.Flusher,
	guard *responsescompat.SSEGuard,
	compatibility *responsesCompatibilityRequest,
) responsesGuardedRelayResult {
	result := responsesGuardedRelayResult{
		collected: newResponsesCappedTail(responsesTelemetryCaptureLimit),
	}
	var deltaDetector responsesOutputDeltaDetector
	readCh := make(chan responsesUpstreamRead, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := body.Read(buf)
			read := responsesUpstreamRead{err: err}
			if n > 0 {
				read.chunk = append([]byte(nil), buf[:n]...)
			}
			select {
			case readCh <- read:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(responsesGuardPollInterval)
	defer ticker.Stop()
	relay := func(out []byte) bool {
		if len(out) == 0 {
			return true
		}
		_, err := w.Write(out)
		if flusher != nil {
			flusher.Flush()
		}
		result.collected.Write(out)
		if result.firstDeltaAt.IsZero() &&
			deltaDetector.Feed(out) {
			result.firstDeltaAt = time.Now()
		}
		if err != nil {
			result.relayErr = ctx.Err()
			if result.relayErr == nil {
				result.relayErr = err
			}
			return false
		}
		return true
	}
	flush := func(relayBytes bool) {
		out, streamResult := guard.Flush()
		compatibility.addStreamResult(streamResult)
		if relayBytes {
			relay(out)
		}
	}

	for {
		select {
		case read := <-readCh:
			if len(read.chunk) > 0 {
				out, streamResult := guard.Push(read.chunk)
				compatibility.addStreamResult(streamResult)
				if !relay(out) {
					flush(false)
					return result
				}
			}
			if read.err == nil {
				continue
			}
			flush(true)
			result.responseComplete = errors.Is(read.err, io.EOF)
			if !result.responseComplete {
				result.relayErr = read.err
			}
			return result

		case <-ticker.C:
			out, streamResult := guard.FlushExpired()
			compatibility.addStreamResult(streamResult)
			if !relay(out) {
				flush(false)
				return result
			}

		case <-ctx.Done():
			_ = body.Close()
			flush(false)
			result.relayErr = ctx.Err()
			return result
		}
	}
}

func removeResponsesGuardStaleHeaders(header http.Header) {
	for _, name := range []string{
		"Content-Encoding",
		"Content-Length",
		"Content-MD5",
		"Digest",
		"Content-Digest",
		"Repr-Digest",
		"ETag",
	} {
		header.Del(name)
	}
}

type responsesCompatibilityNormalizationRule struct {
	rule responsescompat.RequestRule
	mode responsescompat.Mode
}

func (at upstreamAttempt) hasActiveResponsesCompatibility() bool {
	return at.route == "baseten" &&
		at.kind == "responses" &&
		at.responsesCompatibility != nil
}

// responsesCompatibilityState is shared by requests using one immutable
// client policy.
type responsesCompatibilityState struct {
	cfg   config.ResolvedResponsesCompatibility
	rules []responsesCompatibilityNormalizationRule
}

func newResponsesCompatibilityState(cfg config.ResolvedResponsesCompatibility) *responsesCompatibilityState {
	if !responsesCompatibilityModeEnabled(cfg.TextFormatDefault) &&
		!responsesCompatibilityModeEnabled(cfg.AdditionalToolsInput) &&
		!responsesCompatibilityModeEnabled(cfg.ReasoningEffort) &&
		!responsesCompatibilityModeEnabled(cfg.FunctionArgumentsConsistency) {
		return nil
	}
	reasoning, err := responsescompat.NewReasoningEffortRule(
		responsescompat.ReasoningEffortPolicy{
			Allowed: []string{"none", "high", "max"},
			Map: map[string]string{
				"minimal": "high",
				"low":     "high",
				"medium":  "high",
				"xhigh":   "max",
			},
		},
	)
	if err != nil {
		panic(err)
	}
	return &responsesCompatibilityState{
		cfg: cfg,
		rules: []responsesCompatibilityNormalizationRule{
			{
				rule: responsescompat.NewAdditionalToolsRule(),
				mode: responsescompat.Mode(cfg.AdditionalToolsInput),
			},
			{
				rule: reasoning,
				mode: responsescompat.Mode(cfg.ReasoningEffort),
			},
		},
	}
}

// responsesCompatibilityRequest is private to one logical request. Its body
// starts from the Baseten-derived body, after model rewriting and the explicit
// tool denylist. Native and provider-fallback attempts never reference it.
type responsesCompatibilityRequest struct {
	state *responsesCompatibilityState

	summary telemetry.ResponsesCompatibilityV1
	body    []byte

	denylist          []string
	strippedToolTypes []string
}

func beginResponsesCompatibilityRequest(
	cl *clientListener,
	at upstreamAttempt,
) *responsesCompatibilityRequest {
	if at.route != "baseten" || at.kind != "responses" {
		return nil
	}
	state := cl.responsesCompatibility
	if state == nil && len(cl.cfg.ResponsesStripToolTypes) == 0 {
		return nil
	}
	req := &responsesCompatibilityRequest{
		state:    state,
		body:     at.res.NewBody,
		denylist: append([]string(nil), cl.cfg.ResponsesStripToolTypes...),
		strippedToolTypes: append(
			[]string(nil),
			at.strippedToolTypes...,
		),
	}
	if len(cl.cfg.ResponsesStripToolTypes) > 0 {
		req.addConsidered(responsescompat.RuleToolTypeDenylist)
		if len(at.strippedToolTypes) > 0 {
			req.addApplied(responsescompat.RuleToolTypeDenylist)
			req.addForced(responsescompat.RuleToolTypeDenylist)
		}
	}
	if state == nil {
		return req
	}

	if responsesCompatibilityModeEnabled(state.cfg.TextFormatDefault) {
		req.addConsidered(responsescompat.RuleTextFormatDefault)
		if next, changed := responsescompat.NormalizeTextFormat(req.body); changed {
			req.body = next
			req.addApplied(responsescompat.RuleTextFormatDefault)
			req.addForced(responsescompat.RuleTextFormatDefault)
		}
	}
	for _, normalization := range state.rules {
		if normalization.mode != responsescompat.ModeOn {
			continue
		}
		id := normalization.rule.ID()
		req.addConsidered(id)
		next, changed, err := normalization.rule.Apply(
			req.body,
			responsescompat.RequestContext{},
		)
		if err != nil {
			req.summary.ValidationErrors++
			continue
		}
		if changed {
			req.body = next
			req.reapplyExplicitDenylist()
			req.addApplied(id)
			req.addForced(id)
		}
	}
	return req
}

// reapplyExplicitDenylist runs after request normalization because
// additional_tools hoisting can move previously nested tool definitions into
// top-level tools[]. The emergency control must still apply to that derived
// Baseten body.
func (req *responsesCompatibilityRequest) reapplyExplicitDenylist() {
	if req == nil || len(req.denylist) == 0 {
		return
	}
	next, stripped := responsescompat.StripToolTypes(req.body, req.denylist)
	if len(stripped) == 0 {
		return
	}
	req.body = next
	req.addConsidered(responsescompat.RuleToolTypeDenylist)
	req.addApplied(responsescompat.RuleToolTypeDenylist)
	req.addForced(responsescompat.RuleToolTypeDenylist)
	for _, toolType := range stripped {
		if !containsString(req.strippedToolTypes, toolType) {
			req.strippedToolTypes = append(req.strippedToolTypes, toolType)
		}
	}
}

func (req *responsesCompatibilityRequest) newSSEGuard() *responsescompat.SSEGuard {
	if req == nil || req.state == nil ||
		!responsesCompatibilityModeEnabled(
			req.state.cfg.FunctionArgumentsConsistency,
		) {
		return nil
	}
	req.addConsidered(responsescompat.RuleFunctionArgumentsConsistency)
	return responsescompat.NewSSEGuard(responsescompat.StreamLimits{})
}

func responsesCompatibilityModeEnabled(mode config.ResponsesCompatibilityMode) bool {
	return mode == config.ResponsesCompatibilityModeOn
}

func (req *responsesCompatibilityRequest) addStreamResult(result responsescompat.StreamResult) {
	if req == nil {
		return
	}
	req.summary.RepairedEvents += result.RepairedEvents
	req.summary.ValidationErrors += result.ValidationErrors
	if result.RepairedEvents > 0 {
		req.addApplied(responsescompat.RuleFunctionArgumentsConsistency)
	}
}

func (req *responsesCompatibilityRequest) telemetry() *telemetry.ResponsesCompatibilityV1 {
	if req == nil {
		return nil
	}
	out := req.summary
	return &out
}

func (req *responsesCompatibilityRequest) addConsidered(id string) {
	req.summary.Considered = appendUniqueRuleID(req.summary.Considered, id)
}

func (req *responsesCompatibilityRequest) addApplied(id string) {
	req.summary.Applied = appendUniqueRuleID(req.summary.Applied, id)
}

func (req *responsesCompatibilityRequest) addForced(id string) {
	req.summary.Forced = appendUniqueRuleID(req.summary.Forced, id)
}

func appendUniqueRuleID(ids []string, id string) []string {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
