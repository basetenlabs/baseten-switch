package responsescompat

import (
	"bytes"
	"encoding/json"
	"time"
)

type StreamLimits struct {
	MaxArgumentBytes int
	MaxActiveCalls   int
	MaxEventBytes    int
	MaxHeldBytes     int
	MaxHoldDuration  time.Duration
	Now              func() time.Time
}

func (l StreamLimits) withDefaults() StreamLimits {
	if l.MaxArgumentBytes <= 0 {
		l.MaxArgumentBytes = DefaultMaxArgumentBytes
	}
	if l.MaxActiveCalls <= 0 {
		l.MaxActiveCalls = DefaultMaxActiveCalls
	}
	if l.MaxEventBytes <= 0 {
		l.MaxEventBytes = DefaultMaxEventBytes
	}
	if l.MaxHeldBytes <= 0 {
		l.MaxHeldBytes = DefaultMaxHeldBytes
	}
	if l.MaxHoldDuration <= 0 {
		l.MaxHoldDuration = DefaultMaxHoldDuration
	}
	if l.Now == nil {
		l.Now = time.Now
	}
	return l
}

type StreamResult struct {
	RepairedEvents   int
	ValidationErrors int
	Changed          bool
}

func (r *StreamResult) add(other StreamResult) {
	r.RepairedEvents += other.RepairedEvents
	r.ValidationErrors += other.ValidationErrors
	r.Changed = r.Changed || other.Changed
}

type callState struct {
	arguments        []byte
	sawDelta         bool
	seenFunctionDone bool
	seenOutputDone   bool
}

type heldEvent struct {
	itemID string
	raw    []byte
}

// SSEGuard incrementally checks and conditionally repairs typed Responses SSE
// function-call finalization events. Push may retain an incomplete SSE event,
// or a premature done event, until enough information arrives. Flush must be
// called on connection close or cancellation.
type SSEGuard struct {
	limits     StreamLimits
	pending    []byte
	calls      map[string]*callState
	held       []heldEvent
	heldBytes  int
	heldAt     time.Time
	finished   map[string]struct{}
	finishFIFO []string
	bypass     bool
	total      StreamResult
}

func NewSSEGuard(limits StreamLimits) *SSEGuard {
	return &SSEGuard{
		limits:   limits.withDefaults(),
		calls:    make(map[string]*callState),
		finished: make(map[string]struct{}),
	}
}

func (g *SSEGuard) Push(chunk []byte) ([]byte, StreamResult) {
	if g == nil || len(chunk) == 0 {
		return chunk, StreamResult{}
	}
	if g.bypass {
		return chunk, StreamResult{}
	}

	var out bytes.Buffer
	var result StreamResult
	if g.holdExpired() {
		g.failOpen(&out, &result)
		out.Write(chunk)
		g.total.add(result)
		return out.Bytes(), result
	}

	g.pending = append(g.pending, chunk...)
	for {
		end := completeEventEnd(g.pending, false)
		if end == 0 {
			break
		}
		if end > g.limits.MaxEventBytes {
			g.failOpen(&out, &result)
			out.Write(g.pending)
			g.pending = nil
			g.total.add(result)
			return out.Bytes(), result
		}
		raw := append([]byte(nil), g.pending[:end]...)
		g.pending = g.pending[end:]
		if !g.processEvent(raw, &out, &result) {
			out.Write(g.pending)
			g.pending = nil
			g.total.add(result)
			return out.Bytes(), result
		}
		if g.bypass {
			out.Write(g.pending)
			g.pending = nil
			g.total.add(result)
			return out.Bytes(), result
		}
	}
	if len(g.pending) > g.limits.MaxEventBytes {
		g.failOpen(&out, &result)
		out.Write(g.pending)
		g.pending = nil
	}
	g.total.add(result)
	return out.Bytes(), result
}

// FlushExpired releases held events unchanged if their bounded hold duration
// elapsed without a resolving delta. Callers with independent timers may use
// this even when no new upstream bytes arrive.
func (g *SSEGuard) FlushExpired() ([]byte, StreamResult) {
	if g == nil || g.bypass || !g.holdExpired() {
		return nil, StreamResult{}
	}
	var out bytes.Buffer
	var result StreamResult
	g.failOpen(&out, &result)
	g.total.add(result)
	return out.Bytes(), result
}

// Flush releases all retained bytes unchanged and clears per-response state.
// A partial event or unresolved held done event is a validation error.
func (g *SSEGuard) Flush() ([]byte, StreamResult) {
	if g == nil {
		return nil, StreamResult{}
	}
	var out bytes.Buffer
	var result StreamResult
	if len(g.held) > 0 {
		for _, held := range g.held {
			out.Write(held.raw)
		}
		result.ValidationErrors++
	}
	if len(g.pending) > 0 {
		out.Write(g.pending)
		result.ValidationErrors++
	}
	if len(g.calls) > 0 && len(g.held) == 0 {
		result.ValidationErrors++
	}
	g.reset()
	g.pending = nil
	g.total.add(result)
	return out.Bytes(), result
}

func (g *SSEGuard) Total() StreamResult {
	if g == nil {
		return StreamResult{}
	}
	return g.total
}

func (g *SSEGuard) processEvent(raw []byte, out *bytes.Buffer, result *StreamResult) bool {
	event, ok := parseSSEEvent(raw)
	if !ok {
		g.failOpen(out, result)
		out.Write(raw)
		return false
	}
	if event.eventType == "" {
		out.Write(raw)
		return true
	}

	switch event.eventType {
	case "response.function_call_arguments.delta":
		itemID, okID := stringField(event.data, "item_id")
		delta, okDelta := stringField(event.data, "delta")
		if !okID || !okDelta || itemID == "" {
			g.failOpen(out, result)
			out.Write(raw)
			return false
		}
		state, ok := g.call(itemID)
		if !ok {
			g.failOpen(out, result)
			out.Write(raw)
			return false
		}
		state.sawDelta = true
		if len(state.arguments)+len(delta) > g.limits.MaxArgumentBytes {
			g.failOpen(out, result)
			out.Write(raw)
			return false
		}
		state.arguments = append(state.arguments, delta...)
		out.Write(raw)
		return true

	case "response.function_call_arguments.done":
		itemID, okID := stringField(event.data, "item_id")
		if !okID || itemID == "" {
			g.failOpen(out, result)
			out.Write(raw)
			return false
		}
		return g.processDone(raw, event, itemID, false, out, result)

	case "response.output_item.done":
		item, ok := event.data["item"].(map[string]any)
		if !ok || item["type"] != "function_call" {
			out.Write(raw)
			return true
		}
		itemID, okID := stringField(item, "id")
		if !okID || itemID == "" {
			g.failOpen(out, result)
			out.Write(raw)
			return false
		}
		return g.processDone(raw, event, itemID, true, out, result)

	case "response.completed":
		g.flushAtCompletion(out, result)
		out.Write(raw)
		g.reset()
		g.bypass = true
		return true

	case "response.failed", "response.incomplete":
		g.flushAtFailure(out, result)
		out.Write(raw)
		g.reset()
		g.bypass = true
		return true

	default:
		out.Write(raw)
		return true
	}
}

func (g *SSEGuard) processDone(
	raw []byte,
	event parsedEvent,
	itemID string,
	outputItem bool,
	out *bytes.Buffer,
	result *StreamResult,
) bool {
	if _, duplicate := g.finished[itemID]; duplicate {
		g.failOpen(out, result)
		out.Write(raw)
		return false
	}
	state, ok := g.call(itemID)
	if !ok {
		g.failOpen(out, result)
		out.Write(raw)
		return false
	}
	if outputItem {
		if state.seenOutputDone {
			g.failOpen(out, result)
			out.Write(raw)
			return false
		}
		state.seenOutputDone = true
	} else {
		if state.seenFunctionDone {
			g.failOpen(out, result)
			out.Write(raw)
			return false
		}
		state.seenFunctionDone = true
	}
	final, hasFinal := doneArguments(event.data, outputItem)
	accumulatedValid := state.sawDelta && json.Valid(state.arguments)
	finalValid := hasFinal && json.Valid([]byte(final))

	if len(g.held) > 0 {
		if !g.holdDone(itemID, raw) {
			g.failOpen(out, result)
			out.Write(raw)
			return false
		}
		return true
	}

	if accumulatedValid {
		if final == string(state.arguments) {
			out.Write(raw)
		} else {
			if !g.holdDone(itemID, raw) {
				g.failOpen(out, result)
				out.Write(raw)
				return false
			}
		}
		if outputItem && final == string(state.arguments) {
			g.finishCall(itemID)
		}
		return true
	}

	if finalValid {
		// A valid final value without deltas is a legitimate pass-through.
		// Invalid observed deltas are inconsistent but not safely repairable.
		if state.sawDelta && len(state.arguments) > 0 {
			result.ValidationErrors++
		}
		out.Write(raw)
		if outputItem {
			g.finishCall(itemID)
		}
		return true
	}

	if !g.holdDone(itemID, raw) {
		g.failOpen(out, result)
		out.Write(raw)
		return false
	}
	return true
}

func (g *SSEGuard) holdDone(itemID string, raw []byte) bool {
	if g.heldBytes+len(raw) > g.limits.MaxHeldBytes {
		return false
	}
	if len(g.held) == 0 {
		g.heldAt = g.limits.Now()
	}
	g.held = append(g.held, heldEvent{
		itemID: itemID,
		raw:    append([]byte(nil), raw...),
	})
	g.heldBytes += len(raw)
	return true
}

func (g *SSEGuard) call(itemID string) (*callState, bool) {
	if state, ok := g.calls[itemID]; ok {
		return state, true
	}
	if len(g.calls) >= g.limits.MaxActiveCalls {
		return nil, false
	}
	state := &callState{}
	g.calls[itemID] = state
	return state, true
}

func (g *SSEGuard) flushAtCompletion(out *bytes.Buffer, result *StreamResult) {
	if len(g.held) == 0 {
		if len(g.calls) > 0 {
			result.ValidationErrors++
		}
		return
	}
	for _, held := range g.held {
		state := g.calls[held.itemID]
		event, ok := parseSSEEvent(held.raw)
		if !ok {
			g.flushHeldUnchanged(out, result)
			return
		}
		outputItem := event.eventType == "response.output_item.done"
		final, hasFinal := doneArguments(event.data, outputItem)
		finalValid := hasFinal && json.Valid([]byte(final))
		accumulatedValid := state != nil && state.sawDelta && json.Valid(state.arguments)
		if !accumulatedValid && !finalValid {
			g.flushHeldUnchanged(out, result)
			return
		}
	}

	var release bytes.Buffer
	var releaseResult StreamResult
	for _, held := range g.held {
		state := g.calls[held.itemID]
		event, _ := parseSSEEvent(held.raw)
		outputItem := event.eventType == "response.output_item.done"
		final, _ := doneArguments(event.data, outputItem)
		if state != nil && state.sawDelta && json.Valid(state.arguments) {
			if final == string(state.arguments) {
				release.Write(held.raw)
			} else if rewritten, repaired := rewriteArguments(event, string(state.arguments), outputItem); repaired {
				release.Write(rewritten)
				releaseResult.RepairedEvents++
				releaseResult.Changed = true
			} else {
				g.flushHeldUnchanged(out, result)
				return
			}
		} else {
			release.Write(held.raw)
		}
	}
	out.Write(release.Bytes())
	result.add(releaseResult)
}

func (g *SSEGuard) flushAtFailure(out *bytes.Buffer, result *StreamResult) {
	if len(g.held) > 0 || len(g.calls) > 0 {
		g.flushHeldUnchanged(out, result)
	}
}

func (g *SSEGuard) flushHeldUnchanged(out *bytes.Buffer, result *StreamResult) {
	for _, held := range g.held {
		out.Write(held.raw)
	}
	result.ValidationErrors++
}

func (g *SSEGuard) failOpen(out *bytes.Buffer, result *StreamResult) {
	for _, held := range g.held {
		out.Write(held.raw)
	}
	result.ValidationErrors++
	g.reset()
	g.bypass = true
}

func (g *SSEGuard) holdExpired() bool {
	return len(g.held) > 0 &&
		!g.heldAt.IsZero() &&
		g.limits.Now().Sub(g.heldAt) >= g.limits.MaxHoldDuration
}

func (g *SSEGuard) reset() {
	clear(g.calls)
	clear(g.finished)
	g.held = nil
	g.heldBytes = 0
	g.heldAt = time.Time{}
	g.finishFIFO = nil
}

func (g *SSEGuard) finishCall(itemID string) {
	delete(g.calls, itemID)
	if _, exists := g.finished[itemID]; exists {
		return
	}
	g.finished[itemID] = struct{}{}
	g.finishFIFO = append(g.finishFIFO, itemID)
	if len(g.finishFIFO) <= g.limits.MaxActiveCalls {
		return
	}
	oldest := g.finishFIFO[0]
	g.finishFIFO = g.finishFIFO[1:]
	delete(g.finished, oldest)
}

type sseDataLine struct {
	start      int
	prefixEnd  int
	contentEnd int
	lineEnd    int
}

type parsedEvent struct {
	raw       []byte
	eventType string
	data      map[string]any
	dataLines []sseDataLine
}

func parseSSEEvent(raw []byte) (parsedEvent, bool) {
	event := parsedEvent{raw: raw}
	var eventField string
	var dataParts [][]byte
	for start := 0; start < len(raw); {
		contentEnd, lineEnd, ok := nextLine(raw, start, true)
		if !ok {
			return parsedEvent{}, false
		}
		line := raw[start:contentEnd]
		if len(line) == 0 {
			start = lineEnd
			continue
		}
		if line[0] == ':' {
			start = lineEnd
			continue
		}
		colon := bytes.IndexByte(line, ':')
		fieldEnd := len(line)
		valueStart := len(line)
		if colon >= 0 {
			fieldEnd = colon
			valueStart = colon + 1
			if valueStart < len(line) && line[valueStart] == ' ' {
				valueStart++
			}
		}
		switch string(line[:fieldEnd]) {
		case "event":
			eventField = string(line[valueStart:])
		case "data":
			event.dataLines = append(event.dataLines, sseDataLine{
				start:      start,
				prefixEnd:  start + valueStart,
				contentEnd: contentEnd,
				lineEnd:    lineEnd,
			})
			dataParts = append(dataParts, append([]byte(nil), line[valueStart:]...))
		}
		start = lineEnd
	}
	if len(dataParts) == 0 {
		if eventField == "" {
			return event, true
		}
		return parsedEvent{}, false
	}
	payload := bytes.Join(dataParts, []byte{'\n'})
	if bytes.Equal(payload, []byte("[DONE]")) {
		event.eventType = eventField
		return event, true
	}
	if err := json.Unmarshal(payload, &event.data); err != nil || event.data == nil {
		// Unknown non-Responses SSE data remains an opaque pass-through.
		if eventField == "" || len(eventField) < len("response.") ||
			eventField[:len("response.")] != "response." {
			return event, true
		}
		return parsedEvent{}, false
	}
	dataType, _ := event.data["type"].(string)
	if eventField != "" && dataType != "" && eventField != dataType {
		return parsedEvent{}, false
	}
	event.eventType = eventField
	if event.eventType == "" {
		event.eventType = dataType
	}
	return event, true
}

func rewriteArguments(event parsedEvent, arguments string, outputItem bool) ([]byte, bool) {
	if len(event.dataLines) == 0 || event.data == nil {
		return nil, false
	}
	if outputItem {
		item, ok := event.data["item"].(map[string]any)
		if !ok || item["type"] != "function_call" {
			return nil, false
		}
		item["arguments"] = arguments
	} else {
		event.data["arguments"] = arguments
	}
	payload, err := json.Marshal(event.data)
	if err != nil {
		return nil, false
	}

	first := event.dataLines[0]
	var out bytes.Buffer
	out.Grow(len(event.raw) + len(payload))
	out.Write(event.raw[:first.prefixEnd])
	out.Write(payload)
	out.Write(event.raw[first.contentEnd:first.lineEnd])
	cursor := first.lineEnd
	for _, line := range event.dataLines[1:] {
		out.Write(event.raw[cursor:line.start])
		cursor = line.lineEnd
	}
	out.Write(event.raw[cursor:])
	return out.Bytes(), true
}

func doneArguments(data map[string]any, outputItem bool) (string, bool) {
	if outputItem {
		item, ok := data["item"].(map[string]any)
		if !ok {
			return "", false
		}
		return stringField(item, "arguments")
	}
	return stringField(data, "arguments")
}

func stringField(data map[string]any, name string) (string, bool) {
	value, ok := data[name].(string)
	return value, ok
}

func completeEventEnd(data []byte, final bool) int {
	for start := 0; start < len(data); {
		contentEnd, lineEnd, ok := nextLine(data, start, final)
		if !ok {
			return 0
		}
		if contentEnd == start {
			return lineEnd
		}
		start = lineEnd
	}
	return 0
}

func nextLine(data []byte, start int, final bool) (contentEnd, lineEnd int, ok bool) {
	for i := start; i < len(data); i++ {
		switch data[i] {
		case '\n':
			return i, i + 1, true
		case '\r':
			if i+1 < len(data) && data[i+1] == '\n' {
				return i, i + 2, true
			}
			if i+1 < len(data) || final {
				return i, i + 1, true
			}
			return 0, 0, false
		}
	}
	if final && start < len(data) {
		return len(data), len(data), true
	}
	return 0, 0, false
}
