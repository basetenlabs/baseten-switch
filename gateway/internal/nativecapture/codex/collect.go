package codex

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
	"slices"
	"strings"
	"time"
)

type parsedSource struct {
	sessionID     string
	clientVersion string
	turns         []*parsedTurn
}

type parsedTurn struct {
	sessionID      string
	turnID         string
	model          string
	startedAt      time.Time
	completedAt    time.Time
	status         string
	events         []nativeEvent
	requestInputs  [][]byte
	unsupported    bool
	explicitSource bool
}

type nativeEvent struct {
	kind       string
	at         time.Time
	role       string
	name       string
	status     string
	itemID     string
	toolCallID string
	relatedIDs []string
	data       json.RawMessage
}

type retainedBudget struct {
	used int64
	max  int64
}

func (b *retainedBudget) reserve(bytes int64) error {
	if bytes < 0 || bytes > b.max-b.used {
		return fmt.Errorf("%w: retained Codex parse data exceeds %d bytes", ErrLimitExceeded, b.max)
	}
	b.used += bytes
	return nil
}

func (b *retainedBudget) appendEvent(turn *parsedTurn, event nativeEvent) error {
	if err := b.reserve(int64(len(event.kind)+len(event.role)+len(event.name)+len(event.status)+len(event.itemID)+len(event.toolCallID)+len(event.data)) + 256); err != nil {
		return err
	}
	turn.events = append(turn.events, event)
	return nil
}

type rolloutLine struct {
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type idMapper struct {
	values map[string]string
	seed   [32]byte
}

func (c Collector) Collect(ctx context.Context, plan Plan) (Result, error) {
	if err := validateSelection(plan.selection); err != nil {
		return Result{}, err
	}
	result := Result{
		CollectorVersion: CollectorVersion,
		TraceLinks:       make(map[string]string),
		Exclusions:       make(map[string]int),
	}
	var sources []parsedSource
	budget := retainedBudget{max: MaxNormalizedBytes}
	versions := make(map[string]struct{})
	explicitSourceCounts := make(map[string]int)
	for _, source := range plan.files {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		parsed, reason, err := c.collectSource(ctx, source, &budget)
		if err != nil {
			if source.explicit {
				return Result{}, fmt.Errorf("%w: selected Codex session could not be collected", ErrExplicitSource)
			}
			result.Exclusions[reason]++
			if reason == "native_unsupported" {
				result.SchemaDrift.ExcludedSources++
			}
			continue
		}
		if parsed.clientVersion != "" {
			versions[parsed.clientVersion] = struct{}{}
		}
		for _, turn := range parsed.turns {
			if turn.unsupported && codexTurnInSelection(turn, plan.selection) {
				result.SchemaDrift.ExcludedSemanticTurns++
			}
		}
		if source.explicit {
			explicitSourceCounts[parsed.sessionID]++
			if explicitSourceCounts[parsed.sessionID] > 1 {
				return Result{}, fmt.Errorf("%w: selected Codex session has multiple rollout sources", ErrExplicitSource)
			}
		}
		sources = append(sources, parsed)
	}

	allTurns := make([]*parsedTurn, 0)
	for i := range sources {
		allTurns = append(allTurns, sources[i].turns...)
	}
	links, methods := c.correlate(plan.selection, allTurns, result.Exclusions)
	mapper, err := newIDMapper()
	if err != nil {
		return Result{}, fmt.Errorf("create bundle-local Codex identifiers: %w", err)
	}
	selected := make(map[*parsedTurn][]string)
	for eventID, turn := range links {
		selected[turn] = append(selected[turn], eventID)
	}
	for _, turn := range allTurns {
		if turn.explicitSource && terminalTurnInSelection(turn, plan.selection) {
			if _, linked := selected[turn]; !linked {
				selected[turn] = nil
			}
		}
	}
	if len(selected) > MaxNativeTurns {
		return Result{}, fmt.Errorf("%w: more than %d normalized turns", ErrLimitExceeded, MaxNativeTurns)
	}

	sessionBundles := make(map[string]string)
	var normalizedBytes int64
	for _, turn := range allTurns {
		eventIDs, ok := selected[turn]
		if !ok || turn.unsupported || turn.completedAt.IsZero() {
			continue
		}
		slices.Sort(eventIDs)
		sessionBundle := sessionBundles[turn.sessionID]
		if sessionBundle == "" {
			sessionBundle = mapper.mapID("session", turn.sessionID)
			sessionBundles[turn.sessionID] = sessionBundle
		}
		bundleTurn := mapper.mapID("turn", turn.sessionID+"\x00"+turn.turnID)
		matchMode := "explicit_session"
		if len(eventIDs) > 0 {
			matchMode = "canonical_request"
			for _, eventID := range eventIDs {
				if methods[eventID] == "turn_hash" {
					matchMode = "turn_hash"
					break
				}
			}
		}
		normalized := NativeTurnV1{
			NativeSchemaVersion: NativeSchemaVersionV1,
			Client:              ClientName,
			BundleSessionID:     sessionBundle,
			BundleTurnID:        bundleTurn,
			SwitchEventIDs:      slices.Clone(eventIDs),
			MatchMode:           matchMode,
			StartedAt:           turn.startedAt.UTC(),
			CompletedAt:         turn.completedAt.UTC(),
			Status:              turn.status,
		}
		for _, event := range turn.events {
			normalized.Events = append(normalized.Events, normalizeEvent(event, &mapper))
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return Result{}, fmt.Errorf("normalize Codex turn: %w", err)
		}
		rowBytes := int64(len(encoded) + 1)
		if rowBytes > MaxNormalizedBytes-normalizedBytes {
			return Result{}, fmt.Errorf("%w: normalized Codex bytes exceed %d", ErrLimitExceeded, MaxNormalizedBytes)
		}
		normalizedBytes += rowBytes
		result.Turns = append(result.Turns, normalized)
		for _, eventID := range eventIDs {
			result.TraceLinks[eventID] = bundleTurn
		}
	}
	for version := range versions {
		result.ClientVersions = append(result.ClientVersions, version)
	}
	slices.Sort(result.ClientVersions)
	methodSet := make(map[string]struct{})
	for _, method := range methods {
		methodSet[method] = struct{}{}
	}
	for method := range methodSet {
		result.CorrelationMethods = append(result.CorrelationMethods, method)
	}
	slices.Sort(result.CorrelationMethods)
	result.SchemaDrift.finalize(len(result.Turns))
	return result, nil
}

func (c Collector) collectSource(ctx context.Context, source sourceFile, budget *retainedBudget) (parsedSource, string, error) {
	file, info, identity, err := openSourceNoFollow(source.path)
	if err != nil {
		return parsedSource{}, "native_unstable", err
	}
	defer file.Close()
	if identity != source.identity || info.Size() < source.size {
		return parsedSource{}, "native_unstable", errors.New("Codex source changed before collection")
	}
	digest := sha256.New()
	limited := io.TeeReader(io.LimitReader(file, source.size), digest)
	parsed, complete, err := parseRollout(ctx, limited, source, budget)
	if err != nil {
		return parsedSource{}, "native_unsupported", err
	}
	if !complete {
		return parsedSource{}, "native_incomplete", errors.New("Codex source boundary ends with an incomplete record")
	}
	if c.afterRead != nil {
		c.afterRead(source.path)
	}
	current, currentIdentity, err := inspectSourceFile(source.path)
	if err != nil || currentIdentity != source.identity || current.Size() < source.size {
		return parsedSource{}, "native_unstable", errors.New("Codex source changed during collection")
	}
	verify, verifyInfo, verifyIdentity, err := openSourceNoFollow(source.path)
	if err != nil {
		return parsedSource{}, "native_unstable", errors.New("Codex source changed during collection")
	}
	defer verify.Close()
	if verifyIdentity != source.identity || verifyInfo.Size() < source.size {
		return parsedSource{}, "native_unstable", errors.New("Codex source changed during collection")
	}
	verifyDigest := sha256.New()
	if _, err := io.CopyN(verifyDigest, verify, source.size); err != nil || !bytes.Equal(digest.Sum(nil), verifyDigest.Sum(nil)) {
		return parsedSource{}, "native_unstable", errors.New("Codex source changed inside the captured boundary")
	}
	return parsed, "", nil
}

func parseRollout(ctx context.Context, reader io.Reader, source sourceFile, budget *retainedBudget) (parsedSource, bool, error) {
	tracked := &lastByteReader{reader: reader}
	scanner := bufio.NewScanner(tracked)
	scanner.Buffer(make([]byte, 64<<10), MaxRecordBytes)
	parsed := parsedSource{}
	var history []json.RawMessage
	var completedStarts []int
	var active *parsedTurn
	var pendingModel string
	dirtyInput := false
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		if err := ctx.Err(); err != nil {
			return parsedSource{}, false, err
		}
		lineBytes := scanner.Bytes()
		if len(bytes.TrimSpace(lineBytes)) == 0 {
			continue
		}
		var line rolloutLine
		if err := json.Unmarshal(lineBytes, &line); err != nil || line.Timestamp.IsZero() || line.Type == "" {
			return parsedSource{}, false, fmt.Errorf("malformed Codex rollout record at line %d", lineNumber)
		}
		switch line.Type {
		case "session_meta":
			if lineNumber != 1 || parsed.sessionID != "" {
				return parsedSource{}, false, errors.New("Codex session metadata is not a unique first record")
			}
			var payload struct {
				ID          string `json:"id"`
				CLIVersion  string `json:"cli_version"`
				HistoryMode string `json:"history_mode"`
			}
			if json.Unmarshal(line.Payload, &payload) != nil || !validUUID(payload.ID) || payload.ID != source.sessionID {
				return parsedSource{}, false, errors.New("Codex session metadata does not match its rollout")
			}
			if payload.HistoryMode != "" && payload.HistoryMode != "legacy" && payload.HistoryMode != "paginated" {
				return parsedSource{}, false, errors.New("unsupported Codex history mode")
			}
			if !supportedCodexVersion(strings.TrimSpace(payload.CLIVersion)) {
				return parsedSource{}, false, errors.New("unsupported Codex client version")
			}
			parsed.sessionID = payload.ID
			parsed.clientVersion = strings.TrimSpace(payload.CLIVersion)
		case "turn_context":
			var payload struct {
				TurnID string `json:"turn_id"`
				Model  string `json:"model"`
			}
			if json.Unmarshal(line.Payload, &payload) != nil || strings.TrimSpace(payload.Model) == "" {
				return parsedSource{}, false, errors.New("unsupported Codex turn context")
			}
			pendingModel = payload.Model
			if active != nil {
				active.model = payload.Model
				if payload.TurnID != "" && active.turnID != "" && payload.TurnID != active.turnID {
					return parsedSource{}, false, errors.New("conflicting Codex turn identifiers")
				}
			}
		case "response_item":
			if parsed.sessionID == "" {
				return parsedSource{}, false, errors.New("Codex response item precedes session metadata")
			}
			var payload map[string]any
			if err := decodeStrict(line.Payload, &payload); err != nil {
				return parsedSource{}, false, errors.New("unsupported Codex response item")
			}
			itemType, _ := payload["type"].(string)
			canonical, disposition, err := canonicalNativeItem(payload)
			if err != nil {
				return parsedSource{}, false, err
			}
			if active != nil && disposition == itemOutput && dirtyInput {
				inputBytes := canonicalListBytes(history)
				if err := budget.reserve(inputBytes + 32); err != nil {
					return parsedSource{}, false, err
				}
				active.requestInputs = append(active.requestInputs, marshalCanonicalList(history))
				dirtyInput = false
			}
			if disposition != itemExcluded {
				if err := budget.reserve(int64(len(canonical)) + 32); err != nil {
					return parsedSource{}, false, err
				}
				history = append(history, canonical)
				if disposition == itemInput {
					dirtyInput = true
				}
			}
			if active != nil {
				event, ok := normalizeResponseItem(line.Timestamp, itemType, payload)
				if !ok {
					markUnsupportedTurn(&parsed, active)
				} else if event.kind != "" {
					if err := budget.appendEvent(active, event); err != nil {
						return parsedSource{}, false, err
					}
				}
			}
		case "event_msg":
			var payload map[string]any
			if err := decodeStrict(line.Payload, &payload); err != nil {
				return parsedSource{}, false, errors.New("unsupported Codex event record")
			}
			eventType, _ := payload["type"].(string)
			switch eventType {
			case "task_started", "turn_started":
				turnID := stringField(payload, "turn_id")
				if turnID == "" || active != nil {
					return parsedSource{}, false, errors.New("unbounded Codex turn start")
				}
				if err := budget.reserve(int64(len(parsed.sessionID)+len(turnID)+len(pendingModel)) + 512); err != nil {
					return parsedSource{}, false, err
				}
				active = &parsedTurn{sessionID: parsed.sessionID, turnID: turnID, model: pendingModel, startedAt: eventTime(payload, "started_at", line.Timestamp), status: "in_progress", explicitSource: source.explicit}
				if err := budget.appendEvent(active, nativeEvent{kind: "turn_started", at: line.Timestamp}); err != nil {
					return parsedSource{}, false, err
				}
				completedStarts = append(completedStarts, len(history))
			case "task_complete", "turn_complete":
				if active == nil || (stringField(payload, "turn_id") != "" && stringField(payload, "turn_id") != active.turnID) {
					return parsedSource{}, false, errors.New("unbounded Codex turn completion")
				}
				active.completedAt = eventTime(payload, "completed_at", line.Timestamp)
				active.status = "completed"
				if payload["error"] != nil {
					active.status = "failed"
				}
				if err := budget.appendEvent(active, nativeEvent{kind: "turn_completed", at: line.Timestamp, status: active.status}); err != nil {
					return parsedSource{}, false, err
				}
				parsed.turns = append(parsed.turns, active)
				active = nil
				dirtyInput = false
			case "turn_aborted":
				if active == nil {
					return parsedSource{}, false, errors.New("unbounded Codex turn abort")
				}
				active.completedAt = eventTime(payload, "completed_at", line.Timestamp)
				active.status = "aborted"
				data, _ := marshalAllowed(payload, "reason")
				if err := budget.appendEvent(active, nativeEvent{kind: "turn_aborted", at: line.Timestamp, status: "aborted", data: data}); err != nil {
					return parsedSource{}, false, err
				}
				parsed.turns = append(parsed.turns, active)
				active = nil
				dirtyInput = false
			case "thread_rolled_back":
				count := int(numberField(payload, "num_turns"))
				if count <= 0 || count > len(completedStarts) {
					return parsedSource{}, false, errors.New("unsupported Codex rollback")
				}
				history = history[:completedStarts[len(completedStarts)-count]]
				completedStarts = completedStarts[:len(completedStarts)-count]
				if active != nil {
					data, _ := marshalAllowed(payload, "num_turns")
					if err := budget.appendEvent(active, nativeEvent{kind: "rollback", at: line.Timestamp, data: data}); err != nil {
						return parsedSource{}, false, err
					}
				}
			case "agent_reasoning_raw_content":
				// Raw reasoning is intentionally neither retained nor normalized.
			case "token_count", "context_compacted", "user_message", "agent_message", "agent_reasoning", "entered_review_mode", "exited_review_mode", "patch_apply_end", "mcp_tool_call_end", "web_search_end", "image_generation_end", "sub_agent_activity", "item_completed", "thread_settings_applied", "thread_goal_updated":
				if active != nil {
					event, ok := normalizeEventMessage(line.Timestamp, eventType, payload)
					if !ok {
						markUnsupportedTurn(&parsed, active)
					} else if event.kind != "" {
						if err := budget.appendEvent(active, event); err != nil {
							return parsedSource{}, false, err
						}
					}
				}
			default:
				return parsedSource{}, false, fmt.Errorf("unsupported persisted Codex event %q", eventType)
			}
		case "compacted":
			var payload map[string]any
			if err := decodeStrict(line.Payload, &payload); err != nil {
				return parsedSource{}, false, errors.New("unsupported Codex compaction record")
			}
			if replacement, ok := payload["replacement_history"].([]any); ok {
				history = nil
				for _, rawItem := range replacement {
					item, ok := rawItem.(map[string]any)
					if !ok {
						return parsedSource{}, false, errors.New("unsupported Codex replacement history")
					}
					canonical, disposition, err := canonicalNativeItem(item)
					if err != nil {
						return parsedSource{}, false, err
					}
					if disposition != itemExcluded {
						if err := budget.reserve(int64(len(canonical)) + 32); err != nil {
							return parsedSource{}, false, err
						}
						history = append(history, canonical)
					}
				}
				completedStarts = nil
			}
			if active != nil {
				data, _ := marshalAllowed(payload, "message", "window_number")
				if err := budget.appendEvent(active, nativeEvent{kind: "compaction", at: line.Timestamp, data: data}); err != nil {
					return parsedSource{}, false, err
				}
			}
		case "inter_agent_communication", "inter_agent_communication_metadata":
			if active != nil {
				data, _ := sanitizeAndMarshal(line.Payload)
				if err := budget.appendEvent(active, nativeEvent{kind: line.Type, at: line.Timestamp, data: data}); err != nil {
					return parsedSource{}, false, err
				}
			}
		case "world_state", "security_risk_score":
			// Deliberately excluded: environment/repository state and derived
			// security metadata are outside the native normalization contract.
		default:
			return parsedSource{}, false, fmt.Errorf("unsupported Codex rollout record %q", line.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return parsedSource{}, false, fmt.Errorf("read Codex rollout: %w", err)
	}
	if parsed.sessionID == "" {
		return parsedSource{}, true, errors.New("Codex rollout has no session metadata")
	}
	if active != nil {
		// A nonterminal turn is never eligible, but earlier terminal turns remain
		// safe because every source record up to the boundary was understood.
	}
	complete := source.size == 0 || (tracked.readAny && tracked.last == '\n')
	return parsed, complete, nil
}

func markUnsupportedTurn(_ *parsedSource, turn *parsedTurn) {
	if turn.unsupported {
		return
	}
	turn.unsupported = true
}

func codexTurnInSelection(turn *parsedTurn, selection Selection) bool {
	if turn.startedAt.IsZero() || !turn.startedAt.Before(selection.Until) {
		return false
	}
	if turn.completedAt.IsZero() {
		return !turn.startedAt.Before(selection.Since)
	}
	return turn.completedAt.After(selection.Since)
}

func supportedCodexVersion(value string) bool {
	if !strings.HasPrefix(value, "0.147.") || len(value) > 32 {
		return false
	}
	for _, char := range value {
		if char >= '0' && char <= '9' || char >= 'a' && char <= 'z' ||
			char >= 'A' && char <= 'Z' || char == '.' || char == '-' || char == '+' {
			continue
		}
		return false
	}
	return true
}

type lastByteReader struct {
	reader  io.Reader
	last    byte
	readAny bool
}

func (r *lastByteReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.last = p[n-1]
		r.readAny = true
	}
	return n, err
}

type itemDisposition int

const (
	itemExcluded itemDisposition = iota
	itemInput
	itemOutput
)

func canonicalNativeItem(payload map[string]any) (json.RawMessage, itemDisposition, error) {
	itemType, _ := payload["type"].(string)
	disposition := itemOutput
	switch itemType {
	case "message":
		role, _ := payload["role"].(string)
		if role == "user" || role == "developer" {
			disposition = itemInput
		} else if role != "assistant" {
			return nil, itemExcluded, errors.New("unsupported Codex message role")
		}
	case "agent_message":
		disposition = itemInput
	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		disposition = itemInput
	case "reasoning", "local_shell_call", "function_call", "custom_tool_call", "tool_search_call", "web_search_call", "image_generation_call", "compaction", "compaction_summary", "context_compaction":
		disposition = itemOutput
	case "additional_tools", "compaction_trigger":
		return nil, itemExcluded, nil
	default:
		return nil, itemExcluded, fmt.Errorf("unsupported Codex response item %q", itemType)
	}
	canonical := cloneMap(payload)
	delete(canonical, "id")
	delete(canonical, "internal_chat_message_metadata_passthrough")
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, itemExcluded, err
	}
	return encoded, disposition, nil
}

func marshalCanonicalList(items []json.RawMessage) []byte {
	buffer := bytes.NewBuffer(make([]byte, 0, 1024))
	buffer.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buffer.WriteByte(',')
		}
		buffer.Write(item)
	}
	buffer.WriteByte(']')
	return buffer.Bytes()
}

func canonicalListBytes(items []json.RawMessage) int64 {
	size := int64(2)
	for index, item := range items {
		size += int64(len(item))
		if index > 0 {
			size++
		}
	}
	return size
}

func canonicalTraceRequest(body []byte) (string, []byte, error) {
	var request map[string]any
	if err := decodeStrict(body, &request); err != nil {
		return "", nil, err
	}
	if request["previous_response_id"] != nil || request["conversation"] != nil {
		return "", nil, errors.New("stateful Responses requests require keyed turn correlation")
	}
	model, _ := request["model"].(string)
	input, ok := request["input"].([]any)
	if !ok || len(input) == 0 {
		return "", nil, errors.New("Responses request input is not a nonempty ordered list")
	}
	items := make([]json.RawMessage, 0, len(input))
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			return "", nil, errors.New("Responses request contains a non-object input item")
		}
		canonical, disposition, err := canonicalNativeItem(item)
		if err != nil || disposition == itemExcluded {
			return "", nil, errors.New("Responses request contains an unsupported input item")
		}
		items = append(items, canonical)
	}
	return model, marshalCanonicalList(items), nil
}

func (c Collector) correlate(selection Selection, turns []*parsedTurn, exclusions map[string]int) (map[string]*parsedTurn, map[string]string) {
	links := make(map[string]*parsedTurn)
	methods := make(map[string]string)
	for _, trace := range selection.Traces {
		var keyed []*parsedTurn
		if c.CorrelationKey != nil && trace.NativeCorrelation != nil &&
			trace.NativeCorrelation.KeyID == c.CorrelationKey.ID() && trace.NativeCorrelation.Turn != nil {
			for _, turn := range turns {
				if turn.unsupported || !terminalTurnInSelection(turn, selection) || !traceOverlapsTurn(trace, turn) {
					continue
				}
				hash, err := c.CorrelationKey.Hash(ClientName, "turn", turn.turnID)
				if err == nil && hash == *trace.NativeCorrelation.Turn {
					keyed = append(keyed, turn)
				}
			}
		}
		if len(keyed) == 1 {
			links[trace.EventID] = keyed[0]
			methods[trace.EventID] = "turn_hash"
			continue
		}
		if len(keyed) > 1 {
			exclusions["native_ambiguous"]++
			continue
		}
		model, requestInput, err := canonicalTraceRequest(trace.RequestBody)
		if err != nil {
			exclusions["native_request_unsupported"]++
			continue
		}
		if trace.RequestedModel != "" && model != "" && trace.RequestedModel != model {
			exclusions["native_request_model_conflict"]++
			continue
		}
		var candidates []*parsedTurn
		for _, turn := range turns {
			if turn.unsupported || !terminalTurnInSelection(turn, selection) || !traceOverlapsTurn(trace, turn) {
				continue
			}
			if model != "" && turn.model != "" && model != turn.model {
				continue
			}
			for _, candidate := range turn.requestInputs {
				if bytes.Equal(candidate, requestInput) {
					candidates = append(candidates, turn)
					break
				}
			}
		}
		switch len(candidates) {
		case 0:
			exclusions["native_unmatched"]++
		case 1:
			links[trace.EventID] = candidates[0]
			methods[trace.EventID] = "canonical_request"
		default:
			exclusions["native_ambiguous"]++
		}
	}
	return links, methods
}

func traceOverlapsTurn(trace TraceReference, turn *parsedTurn) bool {
	return trace.StartedAt.Before(turn.completedAt) && trace.CompletedAt.After(turn.startedAt)
}

func terminalTurnInSelection(turn *parsedTurn, selection Selection) bool {
	return !turn.completedAt.IsZero() && turn.startedAt.Before(selection.Until) && turn.completedAt.After(selection.Since)
}

func normalizeResponseItem(at time.Time, itemType string, payload map[string]any) (nativeEvent, bool) {
	event := nativeEvent{kind: itemType, at: at, itemID: stringField(payload, "id"), status: stringField(payload, "status")}
	switch itemType {
	case "message":
		event.role = stringField(payload, "role")
		event.data, _ = marshalAllowed(payload, "content", "phase")
	case "agent_message":
		event.role = "agent"
		event.data, _ = marshalAllowed(payload, "content")
	case "reasoning":
		// Only summaries are visible reasoning. content and encrypted_content
		// are deliberately excluded.
		event.data, _ = marshalAllowed(payload, "summary")
	case "function_call":
		event.name = stringField(payload, "name")
		event.toolCallID = stringField(payload, "call_id")
		event.data, _ = marshalAllowed(payload, "namespace", "arguments")
	case "custom_tool_call":
		event.name = stringField(payload, "name")
		event.toolCallID = stringField(payload, "call_id")
		event.data, _ = marshalAllowed(payload, "namespace", "input")
	case "tool_search_call":
		event.name = "tool_search"
		event.toolCallID = stringField(payload, "call_id")
		event.data, _ = marshalAllowed(payload, "execution", "arguments")
	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		event.toolCallID = stringField(payload, "call_id")
		event.data, _ = marshalAllowed(payload, "name", "status", "execution", "output", "tools")
	case "local_shell_call":
		event.name = "local_shell"
		event.toolCallID = stringField(payload, "call_id")
		event.data, _ = marshalAllowed(payload, "status", "action")
	case "web_search_call":
		event.name = "web_search"
		event.data, _ = marshalAllowed(payload, "status", "action")
	case "image_generation_call":
		event.name = "image_generation"
		event.data, _ = marshalAllowed(payload, "status", "revised_prompt", "result")
	case "compaction", "compaction_summary", "context_compaction":
		event.kind = "compaction"
		event.data = nil
	default:
		return nativeEvent{}, false
	}
	return event, true
}

func normalizeEventMessage(at time.Time, eventType string, payload map[string]any) (nativeEvent, bool) {
	event := nativeEvent{kind: eventType, at: at, status: stringField(payload, "status"), toolCallID: stringField(payload, "call_id")}
	switch eventType {
	case "token_count":
		event.data, _ = marshalAllowed(payload, "info")
	case "context_compacted":
		event.kind = "compaction"
	case "user_message":
		event.role = "user"
		event.data, _ = marshalAllowed(payload, "message", "images", "image_details", "audio")
	case "agent_message":
		event.role = "assistant"
		event.data, _ = marshalAllowed(payload, "message", "phase")
	case "agent_reasoning":
		event.kind = "reasoning_summary"
		event.data, _ = marshalAllowed(payload, "text")
	case "patch_apply_end":
		event.name = "patch"
		event.data, _ = marshalAllowed(payload, "status", "success", "stdout", "stderr")
	case "mcp_tool_call_end":
		event.name = "mcp"
		event.data, _ = marshalAllowed(payload, "invocation", "result")
	case "web_search_end":
		event.name = "web_search"
		event.data, _ = marshalAllowed(payload, "query", "action", "results")
	case "image_generation_end":
		event.name = "image_generation"
		event.data, _ = marshalAllowed(payload, "status", "revised_prompt", "result", "failure")
	case "sub_agent_activity":
		event.name = "subagent"
		event.relatedIDs = relatedIDs(payload, "agent_thread_id")
		event.data, _ = marshalAllowed(payload, "kind", "status")
	case "entered_review_mode", "exited_review_mode":
		event.name = "review"
		event.data, _ = marshalAllowed(payload, "user_facing_hint", "review_output")
	case "item_completed":
		item, ok := payload["item"].(map[string]any)
		if !ok {
			return nativeEvent{}, false
		}
		itemType := stringField(item, "type")
		if itemType == "reasoning" {
			event.kind = "item_completed.reasoning"
			event.itemID = stringField(item, "id")
			event.data, _ = marshalAllowed(item, "summary_text")
			return event, true
		}
		allowed := map[string][]string{
			"user_message": {"content"}, "hook_prompt": {"fragments"}, "agent_message": {"content", "phase"},
			"plan": {"text"}, "command_execution": {"command", "status", "stdout", "stderr", "aggregated_output", "exit_code"},
			"dynamic_tool_call":      {"namespace", "tool", "arguments", "status", "content_items", "success", "error"},
			"collab_agent_tool_call": {"tool", "status", "prompt", "model", "reasoning_effort"},
			"sub_agent_activity":     {"kind"}, "web_search": {"query", "action", "results"},
			"image_view":       {},
			"image_generation": {"status", "revised_prompt", "result", "failure"}, "file_change": {"status"},
			"mcp_tool_call": {"server", "tool", "arguments", "status", "result", "error"}, "context_compaction": {"status"},
			"entered_review_mode": {"user_facing_hint"}, "exited_review_mode": {"review_output"},
		}
		fields, known := allowed[itemType]
		if !known {
			return nativeEvent{}, false
		}
		event.kind = "item_completed." + itemType
		event.itemID = stringField(item, "id")
		event.relatedIDs = relatedIDs(item, "sender_thread_id", "receiver_thread_id", "receiver_thread_ids", "agent_thread_id")
		event.data, _ = marshalAllowed(item, fields...)
	case "thread_settings_applied":
		if settings, ok := payload["thread_settings"].(map[string]any); ok {
			event.data, _ = marshalAllowed(settings, "model", "reasoning_effort", "reasoning_summary")
		}
	case "thread_goal_updated":
		event.data, _ = marshalAllowed(payload, "status")
	default:
		return nativeEvent{}, false
	}
	return event, true
}

func normalizeEvent(event nativeEvent, mapper *idMapper) NativeEventV1 {
	normalized := NativeEventV1{Kind: event.kind, At: event.at.UTC(), Role: event.role, Name: event.name, Status: event.status, Data: event.data}
	if event.itemID != "" {
		value := mapper.mapID("item", event.itemID)
		normalized.BundleItemID = &value
	}
	if event.toolCallID != "" {
		value := mapper.mapID("tool", event.toolCallID)
		normalized.BundleToolCallID = &value
	}
	for _, relatedID := range event.relatedIDs {
		normalized.BundleRelatedIDs = append(normalized.BundleRelatedIDs, mapper.mapID("related", relatedID))
	}
	return normalized
}

func relatedIDs(source map[string]any, keys ...string) []string {
	var result []string
	for _, key := range keys {
		switch value := source[key].(type) {
		case string:
			if value != "" {
				result = append(result, value)
			}
		case []any:
			for _, raw := range value {
				if text, ok := raw.(string); ok && text != "" {
					result = append(result, text)
				}
			}
		}
	}
	return result
}

func (m *idMapper) mapID(kind, raw string) string {
	key := kind + "\x00" + raw
	if value := m.values[key]; value != "" {
		return value
	}
	digest := sha256.New()
	_, _ = digest.Write(m.seed[:])
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(kind))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(raw))
	value := kind + "_" + hex.EncodeToString(digest.Sum(nil)[:8])
	m.values[key] = value
	return value
}

func newIDMapper() (idMapper, error) {
	mapper := idMapper{values: make(map[string]string)}
	if _, err := rand.Read(mapper.seed[:]); err != nil {
		return idMapper{}, err
	}
	return mapper, nil
}
