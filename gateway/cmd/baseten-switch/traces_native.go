package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	claudenative "github.com/basetenlabs/baseten-switch/gateway/internal/nativecapture/claude"
	codexnative "github.com/basetenlabs/baseten-switch/gateway/internal/nativecapture/codex"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracepackage"
)

type nativeSessionSelector struct {
	ClaudeCode []string `json:"claude-code"`
	Codex      []string `json:"codex"`
}

type nativePackageData struct {
	members                 []tracepackage.NativeMember
	traceLinks              map[string]string
	candidateFiles          int
	candidateBytes          int64
	exclusions              map[string]int
	operatorSelectedTurnIDs []string
}

func readNativeSessionSelector(path string) (nativeSessionSelector, error) {
	if strings.TrimSpace(path) == "" {
		return nativeSessionSelector{}, nil
	}
	file, info, err := openTraceSelectorNoFollow(path)
	if err != nil {
		return nativeSessionSelector{}, err
	}
	defer file.Close()
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nativeSessionSelector{}, errors.New("native session selector must be a regular mode-0600 file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return nativeSessionSelector{}, errors.New("native session selector is unreadable or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var selector nativeSessionSelector
	if err := decoder.Decode(&selector); err != nil {
		return nativeSessionSelector{}, errors.New("native session selector is malformed")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nativeSessionSelector{}, errors.New("native session selector must contain one JSON object")
	}
	return selector, nil
}

func collectNativePackageData(
	ctx context.Context,
	paths tracecapture.RuntimePaths,
	options tracePackageOptions,
	traces []tracecapture.TraceV1,
) (nativePackageData, error) {
	result := nativePackageData{
		traceLinks: make(map[string]string),
		exclusions: make(map[string]int),
	}
	selector, err := readNativeSessionSelector(options.nativeSessionSelector)
	if err != nil {
		return result, err
	}
	key, _ := tracecapture.LoadCorrelationKey(paths.TraceDir)
	selectedClients := make(map[string]bool, len(options.selection.Clients))
	for _, client := range options.selection.Clients {
		selectedClients[client] = true
	}
	for client := range selectedClients {
		if client != claudenative.ClientName && client != codexnative.ClientName {
			return result, fmt.Errorf("native session collection is unsupported for client %q", client)
		}
	}
	if len(selector.ClaudeCode) > 0 && !selectedClients[claudenative.ClientName] {
		return result, errors.New("native session selector contains claude-code sessions but claude-code was not selected")
	}
	if len(selector.Codex) > 0 && !selectedClients[codexnative.ClientName] {
		return result, errors.New("native session selector contains codex sessions but codex was not selected")
	}

	if selectedClients[claudenative.ClientName] {
		selection := claudenative.Selection{
			Since: options.selection.Since, Until: options.selection.Until,
			ExplicitSessions: append([]string(nil), selector.ClaudeCode...),
		}
		for _, trace := range traces {
			if !traceSelectedForPackage(trace, options.selection, claudenative.ClientName) {
				continue
			}
			selection.Traces = append(selection.Traces, claudenative.TraceReference{
				EventID: trace.EventID, StartedAt: trace.StartedAt, CompletedAt: trace.CompletedAt,
				ResponseBody:      decodeCapturedBody(trace.Response.BodyV1),
				NativeCorrelation: trace.NativeCorrelation,
			})
		}
		if len(selection.Traces) > 0 || len(selection.ExplicitSessions) > 0 {
			root := os.Getenv("CLAUDE_CONFIG_DIR")
			if root == "" {
				home, homeErr := os.UserHomeDir()
				if homeErr != nil {
					return result, homeErr
				}
				root = filepath.Join(home, ".claude")
			}
			collector := claudenative.Collector{ConfigRoot: root, CorrelationKey: key}
			plan, err := collector.Discover(ctx, selection)
			if err != nil {
				return result, err
			}
			result.candidateFiles += plan.CandidateFileCount
			result.candidateBytes += plan.CandidateBytes
			collected, err := collector.Collect(ctx, plan)
			if err != nil {
				return result, err
			}
			rows, err := marshalNativeRows(collected.Turns)
			if err != nil {
				return result, err
			}
			collected.Turns = nil
			result.members = append(result.members, tracepackage.NativeMember{
				Name: "native/claude-code/turns.jsonl", Rows: rows,
				Client: claudenative.ClientName, SourceKind: "claude-code-session-jsonl",
				CollectorVersion:   collected.CollectorVersion,
				ClientVersions:     collected.ClientVersions,
				CorrelationMethods: collected.CorrelationMethods,
				Exclusions:         collected.Exclusions,
			})
			mergeNativeResult(&result, collected.TraceLinks, collected.Exclusions)
		}
	}

	if selectedClients[codexnative.ClientName] {
		selection := codexnative.Selection{
			Since: options.selection.Since, Until: options.selection.Until,
			ExplicitSessions: append([]string(nil), selector.Codex...),
			IncludeArchived:  options.includeCodexArchived,
		}
		for _, trace := range traces {
			if !traceSelectedForPackage(trace, options.selection, codexnative.ClientName) {
				continue
			}
			selection.Traces = append(selection.Traces, codexnative.TraceReference{
				EventID: trace.EventID, StartedAt: trace.StartedAt, CompletedAt: trace.CompletedAt,
				RequestBody:       decodeCapturedBody(trace.Request),
				RequestedModel:    pointerString(trace.RequestedModel),
				NativeCorrelation: trace.NativeCorrelation,
			})
		}
		if len(selection.Traces) > 0 || len(selection.ExplicitSessions) > 0 {
			collector := codexnative.Collector{CorrelationKey: key}
			plan, err := collector.Discover(ctx, selection)
			if err != nil {
				return result, err
			}
			result.candidateFiles += plan.CandidateFileCount
			result.candidateBytes += plan.CandidateBytes
			collected, err := collector.Collect(ctx, plan)
			if err != nil {
				return result, err
			}
			rows, err := marshalNativeRows(collected.Turns)
			if err != nil {
				return result, err
			}
			collected.Turns = nil
			result.members = append(result.members, tracepackage.NativeMember{
				Name: "native/codex/turns.jsonl", Rows: rows,
				Client: codexnative.ClientName, SourceKind: "codex-rollout-jsonl",
				CollectorVersion:   collected.CollectorVersion,
				ClientVersions:     collected.ClientVersions,
				CorrelationMethods: collected.CorrelationMethods,
				Exclusions:         collected.Exclusions,
			})
			mergeNativeResult(&result, collected.TraceLinks, collected.Exclusions)
		}
	}
	for _, member := range result.members {
		for _, row := range member.Rows {
			var fields struct {
				BundleTurnID string `json:"bundle_turn_id"`
				MatchMode    string `json:"match_mode"`
			}
			if err := json.Unmarshal(row, &fields); err != nil {
				return result, errors.New("normalized native row is malformed")
			}
			if fields.MatchMode == "explicit_session" {
				result.operatorSelectedTurnIDs = append(result.operatorSelectedTurnIDs, fields.BundleTurnID)
			}
		}
	}
	slices.Sort(result.operatorSelectedTurnIDs)
	return result, nil
}

func traceSelectedForPackage(trace tracecapture.TraceV1, selection tracepackage.Selection, client string) bool {
	return trace.Client == client && !trace.StartedAt.Before(selection.Since) && trace.StartedAt.Before(selection.Until)
}

func decodeCapturedBody(body tracecapture.BodyV1) []byte {
	if body.CaptureState != tracecapture.CaptureStateCaptured {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(body.BodyBase64)
	if err != nil {
		return nil
	}
	return decoded
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func marshalNativeRows[T any](turns []T) ([]json.RawMessage, error) {
	rows := make([]json.RawMessage, 0, len(turns))
	for _, turn := range turns {
		encoded, err := json.Marshal(turn)
		if err != nil {
			return nil, err
		}
		rows = append(rows, encoded)
	}
	return rows, nil
}

func mergeNativeResult(result *nativePackageData, links map[string]string, exclusions map[string]int) {
	for eventID, turnID := range links {
		result.traceLinks[eventID] = turnID
	}
	for reason, count := range exclusions {
		result.exclusions[reason] += count
	}
}
