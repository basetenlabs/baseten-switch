package claude

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
	"strconv"
	"strings"
	"time"
)

type rawRecord struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	UUID             string          `json:"uuid"`
	ParentUUID       *string         `json:"parentUuid"`
	SessionID        string          `json:"sessionId"`
	AgentID          string          `json:"agentId"`
	ParentToolUseID  string          `json:"parentToolUseID"`
	ParentToolUseAlt string          `json:"parent_tool_use_id"`
	IsSidechain      *bool           `json:"isSidechain"`
	Timestamp        string          `json:"timestamp"`
	Version          string          `json:"version"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Message          json.RawMessage `json:"message"`
}

type rawMessage struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	StopReason *string         `json:"stop_reason"`
}

type rawContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type candidateTurn struct {
	sessionID     string
	agentID       string
	parentToolID  string
	startedAt     time.Time
	endedAt       time.Time
	complete      bool
	invalid       bool
	driftExcluded bool
	approxBytes   int
	events        []candidateEvent
	matches       map[string]struct{}
	explicit      bool
}

type candidateEvent struct {
	sourceID string
	parentID string
	kind     string
	at       time.Time
	role     string
	subtype  string
	content  []candidateContent
}

type candidateContent struct {
	typeName string
	text     string
	thinking string
	name     string
	toolID   string
	input    json.RawMessage
	result   json.RawMessage
	isError  bool
}

type parsedSource struct {
	turns                 []*candidateTurn
	versions              map[string]struct{}
	digest                [sha256.Size]byte
	retainedBytes         int
	ignoredMetadataDrifts []ignoredMetadataDrift
	excludedDriftTurns    []*candidateTurn
}

type ignoredMetadataDrift struct {
	at   time.Time
	turn *candidateTurn
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
	responseIDs := traceResponseIDs(plan.selection.Traces)
	traceByResponseID := make(map[string][]TraceReference)
	for _, trace := range plan.selection.Traces {
		for _, responseID := range responseIDs[trace.EventID] {
			traceByResponseID[responseID] = append(traceByResponseID[responseID], trace)
		}
	}

	candidatesByEvent := make(map[string][]*candidateTurn)
	explicitTurns := make([]*candidateTurn, 0)
	explicitTurnKeys := make(map[string]struct{})
	versions := make(map[string]struct{})
	retainedBytes := 0
	retainedTurns := 0
	for _, source := range plan.files {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		parsed, err := c.readSource(
			ctx, source, plan.selection, traceByResponseID,
			MaxNativeTurns-retainedTurns, MaxNormalizedBytes-retainedBytes,
		)
		if err != nil {
			if errors.Is(err, ErrLimitExceeded) {
				return Result{}, err
			}
			if source.explicit {
				return Result{}, fmt.Errorf("%w: selected Claude Code session could not be read", ErrExplicitSource)
			}
			reason := reasonForSourceError(err)
			result.Exclusions[reason]++
			if reason == "unsupported" {
				result.SchemaDrift.ExcludedSources++
			}
			continue
		}
		if c.afterRead != nil {
			c.afterRead(source.path)
		}
		if err := verifySourceBoundary(source, parsed.digest); err != nil {
			if source.explicit {
				return Result{}, fmt.Errorf("%w: selected Claude Code session changed", ErrExplicitSource)
			}
			result.Exclusions["unstable"]++
			continue
		}
		for version := range parsed.versions {
			versions[version] = struct{}{}
		}
		for _, drift := range parsed.ignoredMetadataDrifts {
			if ignoredMetadataDriftInSelection(drift, plan.selection) {
				result.SchemaDrift.IgnoredMetadataRecords++
			}
		}
		retainedBytes += parsed.retainedBytes
		retainedTurns += len(parsed.turns)
		for _, turn := range parsed.excludedDriftTurns {
			if candidateTurnInSelection(turn, plan.selection) {
				result.SchemaDrift.ExcludedSemanticTurns++
			}
		}
		for _, turn := range parsed.turns {
			if turn.invalid {
				if turn.explicit {
					return Result{}, fmt.Errorf("%w: selected Claude Code turn is malformed", ErrExplicitSource)
				}
				result.Exclusions["malformed"]++
				continue
			}
			if !turn.complete {
				if turn.explicit {
					return Result{}, fmt.Errorf("%w: selected Claude Code turn is incomplete", ErrExplicitSource)
				}
				result.Exclusions["incomplete"]++
				continue
			}
			if !intervalsOverlap(turn.startedAt, turn.endedAt, plan.selection.Since, plan.selection.Until) {
				continue
			}
			if len(turn.matches) > 0 {
				for eventID := range turn.matches {
					candidatesByEvent[eventID] = append(candidatesByEvent[eventID], turn)
				}
			} else if turn.explicit {
				logicalKey := turn.sessionID + "\x00" + turn.agentID + "\x00" + turn.events[0].sourceID
				if _, duplicate := explicitTurnKeys[logicalKey]; duplicate {
					return Result{}, fmt.Errorf("%w: selected Claude Code session has duplicate terminal turns", ErrExplicitSource)
				}
				explicitTurnKeys[logicalKey] = struct{}{}
				explicitTurns = append(explicitTurns, turn)
			}
		}
	}

	selectedTurns := make(map[*candidateTurn]map[string]struct{})
	for _, trace := range plan.selection.Traces {
		candidates := uniqueTurns(candidatesByEvent[trace.EventID])
		switch len(candidates) {
		case 0:
			result.Exclusions["unmatched"]++
		case 1:
			if selectedTurns[candidates[0]] == nil {
				selectedTurns[candidates[0]] = make(map[string]struct{})
			}
			selectedTurns[candidates[0]][trace.EventID] = struct{}{}
		default:
			result.Exclusions["ambiguous"]++
		}
	}

	remapper := newRemapper()
	normalizedBytes := 0
	ordered := make([]*candidateTurn, 0, len(selectedTurns)+len(explicitTurns))
	for turn := range selectedTurns {
		ordered = append(ordered, turn)
	}
	slices.SortFunc(ordered, compareCandidateTurns)
	for _, turn := range explicitTurns {
		if _, linked := selectedTurns[turn]; !linked {
			ordered = append(ordered, turn)
		}
	}
	if len(ordered) > MaxNativeTurns {
		return Result{}, fmt.Errorf("%w: more than %d normalized turns", ErrLimitExceeded, MaxNativeTurns)
	}
	for _, turn := range ordered {
		eventIDs := sortedSet(selectedTurns[turn])
		matchMode := "explicit_session"
		if len(eventIDs) > 0 {
			matchMode = "response_id"
		}
		normalized, err := normalizeTurn(turn, eventIDs, matchMode, remapper)
		if err != nil {
			return Result{}, err
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return Result{}, fmt.Errorf("normalize Claude Code turn: %w", err)
		}
		if normalizedBytes > MaxNormalizedBytes-len(encoded)-1 {
			return Result{}, fmt.Errorf("%w: normalized turns exceed %d bytes", ErrLimitExceeded, MaxNormalizedBytes)
		}
		normalizedBytes += len(encoded) + 1
		result.Turns = append(result.Turns, normalized)
		for _, eventID := range eventIDs {
			result.TraceLinks[eventID] = normalized.BundleTurnID
		}
	}
	result.ClientVersions = sortedKeys(versions)
	if len(result.TraceLinks) > 0 {
		result.CorrelationMethods = []string{"response_id"}
	}
	result.SchemaDrift.finalize(len(result.Turns))
	return result, nil
}

func (c Collector) readSource(
	ctx context.Context,
	source sourceFile,
	selection Selection,
	traceByResponseID map[string][]TraceReference,
	turnBudget int,
	byteBudget int,
) (parsedSource, error) {
	file, info, identity, err := openSourceNoFollow(source.path)
	if err != nil {
		return parsedSource{}, fmt.Errorf("unstable: open source")
	}
	defer file.Close()
	if identity != source.identity || info.Size() < source.size {
		return parsedSource{}, fmt.Errorf("unstable: source identity changed")
	}
	reader := bufio.NewReaderSize(io.LimitReader(file, source.size), 64<<10)
	hash := sha256.New()
	tee := io.TeeReader(reader, hash)
	lineReader := bufio.NewReaderSize(tee, 64<<10)
	parsed := parsedSource{versions: make(map[string]struct{})}
	var active *candidateTurn
	var pending []candidateEvent
	for {
		if err := ctx.Err(); err != nil {
			return parsedSource{}, err
		}
		line, complete, readErr := readBoundedLine(lineReader, MaxRecordBytes)
		if readErr != nil {
			if errors.Is(readErr, errRecordTooLarge) {
				return parsedSource{}, readErr
			}
			return parsedSource{}, fmt.Errorf("malformed: read source")
		}
		if !complete {
			// Drain the remaining fixed boundary through the digest. An incomplete
			// final record is never parsed or normalized.
			_, _ = io.Copy(io.Discard, lineReader)
			break
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			return parsedSource{}, fmt.Errorf("malformed: empty JSONL record")
		}
		var record rawRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return parsedSource{}, fmt.Errorf("malformed: invalid JSONL record")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
			return parsedSource{}, fmt.Errorf("malformed: invalid JSONL record")
		}
		if err := c.processRecord(
			&parsed, &active, &pending, source, selection, record, fields,
			traceByResponseID, turnBudget, byteBudget,
		); err != nil {
			return parsedSource{}, err
		}
	}
	if active != nil {
		if err := retainCandidateTurn(&parsed, active, turnBudget, byteBudget); err != nil {
			return parsedSource{}, err
		}
	}
	copy(parsed.digest[:], hash.Sum(nil))
	return parsed, nil
}

func (c Collector) processRecord(
	parsed *parsedSource,
	active **candidateTurn,
	pending *[]candidateEvent,
	source sourceFile,
	selection Selection,
	record rawRecord,
	fields map[string]json.RawMessage,
	traceByResponseID map[string][]TraceReference,
	turnBudget int,
	byteBudget int,
) error {
	switch record.Type {
	case "user", "assistant", "tool_result":
		if !supportedClaudeVersion(record.Version) {
			return fmt.Errorf("unsupported: Claude Code record version")
		}
		if record.SessionID != source.sessionID {
			return fmt.Errorf("malformed: session identifier mismatch")
		}
		if source.agentID != "" && record.AgentID != source.agentID {
			return fmt.Errorf("malformed: agent identifier mismatch")
		}
		if source.agentID != "" && (record.IsSidechain == nil || !*record.IsSidechain) {
			return fmt.Errorf("malformed: subagent record is not marked as a sidechain")
		}
		parsed.versions[record.Version] = struct{}{}
	case "system", "progress", "file-history-snapshot", "queue-operation",
		"custom-title", "ai-title", "summary", "attachment", "last-prompt", "pr-link",
		"atis-latch", "mode":
		// These recognized records are either normalized below or deliberately
		// excluded. They do not broaden the event allowlist.
	default:
		metadataOnly, err := classifyUnknownMetadata(fields, source)
		if err != nil {
			return err
		}
		if metadataOnly {
			at, _ := parseTimestamp(record.Timestamp)
			parsed.ignoredMetadataDrifts = append(parsed.ignoredMetadataDrifts, ignoredMetadataDrift{at: at, turn: *active})
			return nil
		}
		if *active != nil {
			invalidateTurnForSchemaDrift(parsed, *active)
			return nil
		}
		return fmt.Errorf("unsupported: unbounded Claude Code semantic record")
	}

	at, timestampOK := parseTimestamp(record.Timestamp)
	switch record.Type {
	case "user", "tool_result":
		message, contents, continuation, valid := decodeUserRecord(record)
		_ = message
		if !valid || !timestampOK {
			if *active != nil {
				invalidateTurnForSchemaDrift(parsed, *active)
			}
			return nil
		}
		if record.IsCompactSummary {
			if err := appendPendingEvent(pending, candidateEvent{
				sourceID: record.UUID, parentID: pointerValue(record.ParentUUID),
				kind: "compaction", at: at, subtype: "summary", content: contents,
			}); err != nil {
				return err
			}
			return nil
		}
		if !continuation {
			if *active != nil {
				if err := retainCandidateTurn(parsed, *active, turnBudget, byteBudget); err != nil {
					return err
				}
			}
			*active = &candidateTurn{
				sessionID:    source.sessionID,
				agentID:      source.agentID,
				parentToolID: firstNonempty(record.ParentToolUseID, record.ParentToolUseAlt),
				startedAt:    at,
				endedAt:      at,
				matches:      make(map[string]struct{}),
				explicit:     source.explicit,
				events:       slices.Clone(*pending),
			}
			for _, event := range *pending {
				(*active).approxBytes += candidateEventBytes(event)
			}
			*pending = nil
		}
		if *active == nil {
			return nil
		}
		(*active).endedAt = at
		if err := appendCandidateEvent(*active, candidateEvent{
			sourceID: record.UUID, parentID: pointerValue(record.ParentUUID),
			kind: "message", at: at, role: "user", content: contents,
		}); err != nil {
			return err
		}
	case "assistant":
		if *active == nil || !timestampOK {
			return nil
		}
		message, contents, valid := decodeAssistantRecord(record)
		if !valid {
			invalidateTurnForSchemaDrift(parsed, *active)
			return nil
		}
		(*active).endedAt = at
		if err := appendCandidateEvent(*active, candidateEvent{
			sourceID: record.UUID, parentID: pointerValue(record.ParentUUID),
			kind: "message", at: at, role: "assistant", content: contents,
		}); err != nil {
			return err
		}
		for _, trace := range traceByResponseID[message.ID] {
			if c.traceMatchesSource(trace, source, at, selection) {
				(*active).matches[trace.EventID] = struct{}{}
			}
		}
		if message.StopReason != nil && *message.StopReason != "" && *message.StopReason != "tool_use" {
			(*active).complete = true
		}
	case "system":
		if !timestampOK {
			return nil
		}
		switch record.Subtype {
		case "compact_boundary":
			if !supportedClaudeVersion(record.Version) || record.SessionID != source.sessionID {
				return fmt.Errorf("unsupported: compaction marker schema")
			}
			parsed.versions[record.Version] = struct{}{}
			if err := appendPendingEvent(pending, candidateEvent{
				sourceID: record.UUID, parentID: pointerValue(record.ParentUUID),
				kind: "compaction", at: at, subtype: "boundary",
			}); err != nil {
				return err
			}
		case "turn_duration":
			if *active == nil {
				return nil
			}
			(*active).endedAt = at
			if len((*active).events) > 1 {
				(*active).complete = true
			}
		case "resume":
			if !supportedClaudeVersion(record.Version) || record.SessionID != source.sessionID {
				return fmt.Errorf("unsupported: resume marker schema")
			}
			parsed.versions[record.Version] = struct{}{}
			if err := appendPendingEvent(pending, candidateEvent{
				sourceID: record.UUID, parentID: pointerValue(record.ParentUUID),
				kind: "resume", at: at,
			}); err != nil {
				return err
			}
		default:
			if *active != nil {
				invalidateTurnForSchemaDrift(parsed, *active)
				return nil
			}
			return fmt.Errorf("unsupported: unbounded Claude Code system record")
		}
	}
	return nil
}

func invalidateTurnForSchemaDrift(parsed *parsedSource, turn *candidateTurn) {
	turn.invalid = true
	if turn.driftExcluded {
		return
	}
	turn.driftExcluded = true
	parsed.excludedDriftTurns = append(parsed.excludedDriftTurns, turn)
}

func ignoredMetadataDriftInSelection(drift ignoredMetadataDrift, selection Selection) bool {
	if drift.turn != nil {
		return candidateTurnInSelection(drift.turn, selection)
	}
	return !drift.at.IsZero() && !drift.at.Before(selection.Since) && drift.at.Before(selection.Until)
}

func candidateTurnInSelection(turn *candidateTurn, selection Selection) bool {
	if turn.startedAt.IsZero() || !turn.startedAt.Before(selection.Until) {
		return false
	}
	if turn.endedAt.IsZero() {
		return !turn.startedAt.Before(selection.Since)
	}
	return !turn.endedAt.Before(selection.Since)
}

func classifyUnknownMetadata(fields map[string]json.RawMessage, source sourceFile) (bool, error) {
	if len(fields) == 0 || len(fields) > 64 {
		return false, nil
	}
	var recordType string
	if raw, exists := fields["type"]; !exists || json.Unmarshal(raw, &recordType) != nil || recordType == "" {
		return false, nil
	}
	if raw, exists := fields["sessionId"]; exists {
		var sessionID string
		if json.Unmarshal(raw, &sessionID) != nil || sessionID == "" || sessionID != source.sessionID {
			return false, fmt.Errorf("unsupported: unknown metadata session identifier")
		}
	}
	if raw, exists := fields["agentId"]; exists {
		var agentID string
		if json.Unmarshal(raw, &agentID) != nil || agentID == "" || agentID != source.agentID {
			return false, fmt.Errorf("unsupported: unknown metadata agent identifier")
		}
	}
	for key, raw := range fields {
		lower := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
		for _, semantic := range []string{
			"message", "content", "tool", "input", "output", "result",
			"thinking", "reason", "summary", "compact", "prompt", "response",
			"attachment", "completion", "lifecycle", "parent", "turn", "uuid",
			"lineage", "error",
		} {
			if strings.Contains(lower, semantic) {
				return false, nil
			}
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || len(trimmed) > 4<<10 || trimmed[0] == '{' || trimmed[0] == '[' {
			return false, nil
		}
		var primitive any
		if json.Unmarshal(trimmed, &primitive) != nil {
			return false, nil
		}
		switch value := primitive.(type) {
		case nil, bool, float64:
		case string:
			if !safeUnknownMetadataString(value) {
				return false, nil
			}
		default:
			return false, nil
		}
	}
	return true, nil
}

func safeUnknownMetadataString(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("._-+:/@", char) {
			continue
		}
		return false
	}
	return true
}

func decodeUserRecord(record rawRecord) (rawMessage, []candidateContent, bool, bool) {
	var message rawMessage
	if err := json.Unmarshal(record.Message, &message); err != nil || message.Role != "user" {
		return rawMessage{}, nil, false, false
	}
	contents, toolResultsOnly, valid := decodeContent(message.Content, "user")
	return message, contents, record.Type == "tool_result" || toolResultsOnly, valid
}

func decodeAssistantRecord(record rawRecord) (rawMessage, []candidateContent, bool) {
	var message rawMessage
	if err := json.Unmarshal(record.Message, &message); err != nil ||
		message.Role != "assistant" || message.ID == "" {
		return rawMessage{}, nil, false
	}
	contents, _, valid := decodeContent(message.Content, "assistant")
	return message, contents, valid
}

func decodeContent(raw json.RawMessage, role string) ([]candidateContent, bool, bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false, false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []candidateContent{{typeName: "text", text: text}}, false, true
	}
	var blocks []rawContent
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false, false
	}
	contents := make([]candidateContent, 0, len(blocks))
	toolResultsOnly := len(blocks) > 0
	for _, block := range blocks {
		switch block.Type {
		case "text":
			contents = append(contents, candidateContent{typeName: "text", text: block.Text})
			toolResultsOnly = false
		case "thinking":
			if role != "assistant" {
				return nil, false, false
			}
			contents = append(contents, candidateContent{typeName: "thinking", thinking: block.Thinking})
			toolResultsOnly = false
		case "redacted_thinking":
			// Opaque or encrypted reasoning is deliberately excluded.
			toolResultsOnly = false
		case "tool_use":
			if role != "assistant" || block.ID == "" || block.Name == "" || !validJSONObject(block.Input) {
				return nil, false, false
			}
			contents = append(contents, candidateContent{
				typeName: "tool_use", name: block.Name, toolID: block.ID,
				input: slices.Clone(block.Input),
			})
			toolResultsOnly = false
		case "tool_result":
			if role != "user" || block.ToolUseID == "" || !validJSONValue(block.Content) {
				return nil, false, false
			}
			contents = append(contents, candidateContent{
				typeName: "tool_result", toolID: block.ToolUseID,
				result: slices.Clone(block.Content), isError: block.IsError,
			})
		case "image", "document", "server_tool_use", "web_search_tool_result":
			// Attachments and provider-owned opaque blocks are not copied from
			// native storage. The exact Switch body remains authoritative.
			toolResultsOnly = false
		default:
			return nil, false, false
		}
	}
	return contents, toolResultsOnly, true
}

func validJSONObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &value) == nil && value != nil
}

func validJSONValue(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Valid(raw)
}

func supportedClaudeVersion(value string) bool {
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) < 2 {
		return false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	return majorErr == nil && minorErr == nil && major == 2 && minor == 1
}

func parseTimestamp(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed.UTC(), err == nil
}

func (c Collector) traceMatchesSource(
	trace TraceReference,
	source sourceFile,
	assistantAt time.Time,
	selection Selection,
) bool {
	if assistantAt.Before(selection.Since) || !assistantAt.Before(selection.Until) {
		return false
	}
	if !trace.StartedAt.IsZero() && !trace.CompletedAt.IsZero() &&
		!intervalsOverlap(trace.StartedAt, trace.CompletedAt, selection.Since, selection.Until) {
		return false
	}
	// Keyed fields narrow response-ID matches. A session or agent hash alone is
	// never accepted as a turn-level link.
	correlation := trace.NativeCorrelation
	if correlation == nil || c.CorrelationKey == nil ||
		correlation.KeyID != c.CorrelationKey.ID() {
		return true
	}
	if correlation.Session != nil {
		hash, err := c.CorrelationKey.Hash(ClientName, "session", source.sessionID)
		if err != nil || hash != *correlation.Session {
			return false
		}
	}
	if correlation.Agent != nil {
		if source.agentID == "" {
			return false
		}
		hash, err := c.CorrelationKey.Hash(ClientName, "agent", source.agentID)
		if err != nil || hash != *correlation.Agent {
			return false
		}
	}
	return true
}

var errRecordTooLarge = errors.New("oversized: native JSONL record")

func readBoundedLine(reader *bufio.Reader, limit int) ([]byte, bool, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line) > limit-len(fragment) {
			return nil, false, errRecordTooLarge
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line, true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line, false, nil
		default:
			return nil, false, err
		}
	}
}

func verifySourceBoundary(source sourceFile, digest [sha256.Size]byte) error {
	file, info, identity, err := openSourceNoFollow(source.path)
	if err != nil {
		return err
	}
	defer file.Close()
	if identity != source.identity || info.Size() < source.size {
		return errors.New("source replaced or truncated")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, source.size)); err != nil {
		return err
	}
	if !bytes.Equal(hash.Sum(nil), digest[:]) {
		return errors.New("source changed inside captured boundary")
	}
	return nil
}

func reasonForSourceError(err error) string {
	message := err.Error()
	switch {
	case strings.HasPrefix(message, "unsupported:"):
		return "unsupported"
	case strings.HasPrefix(message, "oversized:"):
		return "oversized"
	case strings.HasPrefix(message, "unstable:"):
		return "unstable"
	default:
		return "malformed"
	}
}

func intervalsOverlap(aStart, aEnd, bStart, bEnd time.Time) bool {
	if aEnd.Before(aStart) {
		return false
	}
	return aStart.Before(bEnd) && !aEnd.Before(bStart)
}

func retainCandidateTurn(
	parsed *parsedSource,
	turn *candidateTurn,
	turnBudget int,
	byteBudget int,
) error {
	if !turn.explicit && len(turn.matches) == 0 {
		return nil
	}
	if len(parsed.turns) >= turnBudget {
		return fmt.Errorf("%w: normalized native turn count", ErrLimitExceeded)
	}
	if turn.approxBytes < 0 || parsed.retainedBytes > byteBudget-turn.approxBytes {
		return fmt.Errorf("%w: normalized native turn bytes", ErrLimitExceeded)
	}
	parsed.turns = append(parsed.turns, turn)
	parsed.retainedBytes += turn.approxBytes
	return nil
}

func appendCandidateEvent(turn *candidateTurn, event candidateEvent) error {
	size := candidateEventBytes(event)
	if size < 0 || turn.approxBytes > MaxNormalizedBytes-size {
		return fmt.Errorf("%w: active native turn bytes", ErrLimitExceeded)
	}
	turn.events = append(turn.events, event)
	turn.approxBytes += size
	return nil
}

func appendPendingEvent(events *[]candidateEvent, event candidateEvent) error {
	total := candidateEventBytes(event)
	for _, existing := range *events {
		total += candidateEventBytes(existing)
		if total > MaxNormalizedBytes {
			return fmt.Errorf("%w: pending native context bytes", ErrLimitExceeded)
		}
	}
	*events = append(*events, event)
	return nil
}

func candidateEventBytes(event candidateEvent) int {
	total := len(event.sourceID) + len(event.parentID) + len(event.kind) + len(event.role) + len(event.subtype) + 64
	for _, content := range event.content {
		total += len(content.typeName) + len(content.text) + len(content.thinking) +
			len(content.name) + len(content.toolID) + len(content.input) + len(content.result) + 64
	}
	return total
}

func uniqueTurns(turns []*candidateTurn) []*candidateTurn {
	seen := make(map[*candidateTurn]struct{}, len(turns))
	result := make([]*candidateTurn, 0, len(turns))
	for _, turn := range turns {
		if _, exists := seen[turn]; exists {
			continue
		}
		seen[turn] = struct{}{}
		result = append(result, turn)
	}
	return result
}

func compareCandidateTurns(a, b *candidateTurn) int {
	if comparison := a.startedAt.Compare(b.startedAt); comparison != 0 {
		return comparison
	}
	if comparison := strings.Compare(a.sessionID, b.sessionID); comparison != 0 {
		return comparison
	}
	return strings.Compare(a.agentID, b.agentID)
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func sortedKeys(values map[string]struct{}) []string { return sortedSet(values) }

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type remapper struct {
	sessions map[string]string
	turns    map[string]string
	agents   map[string]string
	events   map[string]string
	tools    map[string]string
}

func newRemapper() *remapper {
	return &remapper{
		sessions: make(map[string]string), turns: make(map[string]string), agents: make(map[string]string),
		events: make(map[string]string), tools: make(map[string]string),
	}
}

func (r *remapper) id(kind, source string, values map[string]string) (string, error) {
	if existing := values[source]; existing != "" {
		return existing, nil
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate bundle-local identifier: %w", err)
	}
	id := kind + "_" + hex.EncodeToString(random)
	values[source] = id
	return id, nil
}

func normalizeTurn(
	turn *candidateTurn,
	eventIDs []string,
	matchMode string,
	remapper *remapper,
) (NativeTurnV1, error) {
	bundleSessionID, err := remapper.id("session", turn.sessionID, remapper.sessions)
	if err != nil {
		return NativeTurnV1{}, err
	}
	bundleTurnID, err := remapper.id("turn", turn.sessionID+"\x00"+turn.events[0].sourceID, remapper.turns)
	if err != nil {
		return NativeTurnV1{}, err
	}
	normalized := NativeTurnV1{
		NativeSchemaVersion: NativeSchemaVersionV1,
		Client:              ClientName, BundleSessionID: bundleSessionID, BundleTurnID: bundleTurnID,
		SwitchEventIDs: slices.Clone(eventIDs), MatchMode: matchMode,
		StartedAt: turn.startedAt.UTC(), CompletedAt: turn.endedAt.UTC(), Status: "completed",
		Events: make([]NativeEventV1, 0, len(turn.events)),
	}
	if turn.agentID != "" {
		agentID, err := remapper.id("agent", turn.sessionID+"\x00"+turn.agentID, remapper.agents)
		if err != nil {
			return NativeTurnV1{}, err
		}
		normalized.BundleAgentID = &agentID
	}
	if turn.parentToolID != "" {
		parentToolID, err := remapper.id("tool", turn.sessionID+"\x00"+turn.parentToolID, remapper.tools)
		if err != nil {
			return NativeTurnV1{}, err
		}
		normalized.BundleParentToolID = &parentToolID
	}
	includedEvents := make(map[string]string, len(turn.events))
	for _, event := range turn.events {
		if event.sourceID == "" {
			return NativeTurnV1{}, errors.New("Claude Code native event has no structural identifier")
		}
		if _, duplicate := includedEvents[event.sourceID]; duplicate {
			return NativeTurnV1{}, errors.New("Claude Code native turn has duplicate structural identifiers")
		}
		bundleEventID, err := remapper.id("event", turn.sessionID+"\x00"+event.sourceID, remapper.events)
		if err != nil {
			return NativeTurnV1{}, err
		}
		includedEvents[event.sourceID] = bundleEventID
	}
	for _, event := range turn.events {
		bundleEventID := includedEvents[event.sourceID]
		nativeEvent := NativeEventV1{
			Kind: event.kind, BundleEventID: bundleEventID, At: event.at.UTC(),
			Role: event.role, Subtype: event.subtype,
		}
		if parentID := includedEvents[event.parentID]; parentID != "" {
			nativeEvent.BundleParentEventID = &parentID
		}
		for _, content := range event.content {
			nativeContent := NativeContentV1{
				Type: content.typeName, Text: content.text, Thinking: content.thinking,
				Name: content.name, Input: slices.Clone(content.input),
				Result: slices.Clone(content.result), IsError: content.isError,
			}
			if content.toolID != "" {
				bundleToolID, err := remapper.id("tool", turn.sessionID+"\x00"+content.toolID, remapper.tools)
				if err != nil {
					return NativeTurnV1{}, err
				}
				nativeContent.BundleToolCallID = &bundleToolID
			}
			nativeEvent.Content = append(nativeEvent.Content, nativeContent)
		}
		normalized.Events = append(normalized.Events, nativeEvent)
	}
	return normalized, nil
}
