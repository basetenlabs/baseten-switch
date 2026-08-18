package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
)

const (
	fixtureSession = "11111111-1111-4111-8111-111111111111"
	secondSession  = "22222222-2222-4222-8222-222222222222"
	eventID        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var (
	selectionSince = time.Date(2026, 8, 17, 11, 59, 0, 0, time.UTC)
	selectionUntil = time.Date(2026, 8, 17, 12, 5, 0, 0, time.UTC)
)

func TestResolveCodexHomeUsesEnvironment(t *testing.T) {
	want := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", want)
	got, err := ResolveCodexHome("")
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(want)
	if got != abs {
		t.Fatalf("ResolveCodexHome() = %q, want %q", got, abs)
	}
}

func TestDiscoverScansOnlyRecognizedRolloutsAndArchivedIsOptIn(t *testing.T) {
	home := t.TempDir()
	active := writeFixture(t, home, fixtureSession, false, true)
	_ = active
	if err := os.WriteFile(filepath.Join(home, "history.jsonl"), []byte("sensitive global history"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(home, "log")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "codex.log"), []byte("not a rollout"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, home, secondSession, true, true)
	symlink := filepath.Join(home, "sessions", "2026", "08", "17", "rollout-symlink.jsonl")
	if err := os.Symlink(filepath.Join(home, "history.jsonl"), symlink); err != nil {
		t.Fatal(err)
	}

	collector := Collector{CodexHome: home}
	selection := baseSelection()
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CandidateFileCount != 1 {
		t.Fatalf("active candidate count = %d, want 1", plan.CandidateFileCount)
	}
	selection.IncludeArchived = true
	plan, err = collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CandidateFileCount != 2 {
		t.Fatalf("archived candidate count = %d, want 2", plan.CandidateFileCount)
	}
}

func TestCollectCanonicalRequestNormalizesTerminalTurn(t *testing.T) {
	home := t.TempDir()
	writeFixture(t, home, fixtureSession, false, true)
	collector := Collector{CodexHome: home}
	selection := baseSelection()
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.CollectorVersion != CollectorVersion || len(result.Turns) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	turn := result.Turns[0]
	if turn.MatchMode != "canonical_request" || turn.Status != "completed" {
		t.Fatalf("unexpected normalized turn: %+v", turn)
	}
	if result.TraceLinks[eventID] != turn.BundleTurnID {
		t.Fatalf("missing bidirectional trace link: %+v", result.TraceLinks)
	}
	if len(turn.SwitchEventIDs) != 1 || turn.SwitchEventIDs[0] != eventID {
		t.Fatalf("unexpected switch event IDs: %v", turn.SwitchEventIDs)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{
		fixtureSession, "turn-native-secret", "msg-native-secret", "call-native-secret",
		"/private/example/checkout", "raw reasoning must not be exported",
		"encrypted-native-secret", "opaque-native-secret", "output-ciphertext",
		"base_instructions", "dynamic_tools", "git",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("normalized output contains forbidden value %q: %s", forbidden, text)
		}
	}
	for _, retained := range []string{"Classify this request.", "Check requirements.", "tool output", "mcp output"} {
		if !strings.Contains(text, retained) {
			t.Fatalf("normalized output omitted %q: %s", retained, text)
		}
	}
	if !strings.Contains(text, "tool_") || !strings.Contains(text, "item_") || strings.Contains(text, `"native_correlation"`) {
		t.Fatalf("structural IDs were not remapped: %s", text)
	}
}

func TestCollectUsesUniqueKeyedTurnBeforeRequestFallback(t *testing.T) {
	home := t.TempDir()
	writeFixture(t, home, fixtureSession, false, true)
	keyRoot, err := os.MkdirTemp(".", ".correlation-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(keyRoot) })
	key, err := tracecapture.LoadOrCreateCorrelationKey(filepath.Join(keyRoot, "traces"))
	if err != nil {
		t.Fatal(err)
	}
	turnHash, err := key.Hash(ClientName, "turn", "turn-native-secret")
	if err != nil {
		t.Fatal(err)
	}
	selection := baseSelection()
	selection.Traces[0].RequestBody = []byte(`{"unsupported":true}`)
	selection.Traces[0].NativeCorrelation = &tracecapture.NativeCorrelationV1{
		Scheme: "hmac-sha256-v1", KeyID: key.ID(), Turn: &turnHash,
	}
	collector := Collector{CodexHome: home, CorrelationKey: key}
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || result.Turns[0].MatchMode != "turn_hash" {
		t.Fatalf("keyed turn was not selected: %+v", result)
	}
}

func TestCollectRejectsAmbiguousCanonicalMatches(t *testing.T) {
	home := t.TempDir()
	writeFixture(t, home, fixtureSession, false, true)
	writeFixture(t, home, secondSession, false, true)
	collector := Collector{CodexHome: home}
	plan, err := collector.Discover(context.Background(), baseSelection())
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 0 || result.Exclusions["native_ambiguous"] != 1 {
		t.Fatalf("ambiguous match was not omitted: %+v", result)
	}
}

func TestCanonicalRequestWithDuplicateFieldsDoesNotCorrelate(t *testing.T) {
	home := t.TempDir()
	writeFixture(t, home, fixtureSession, false, true)
	selection := baseSelection()
	selection.Traces[0].RequestBody = []byte(`{"model":"gpt-example","model":"gpt-example","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Classify this request."}]}]}`)
	collector := Collector{CodexHome: home}
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 0 || result.Exclusions["native_request_unsupported"] != 1 {
		t.Fatalf("duplicate JSON fields were not rejected: %+v", result)
	}
}

func TestExplicitSessionEmitsOnlyTerminalTurnsWithoutTraceLink(t *testing.T) {
	home := t.TempDir()
	writeFixture(t, home, fixtureSession, false, true)
	collector := Collector{CodexHome: home}
	selection := Selection{Since: selectionSince, Until: selectionUntil, ExplicitSessions: []string{fixtureSession}}
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || result.Turns[0].MatchMode != "explicit_session" || len(result.Turns[0].SwitchEventIDs) != 0 || len(result.TraceLinks) != 0 {
		t.Fatalf("unexpected explicit selection result: %+v", result)
	}
}

func TestExplicitSessionRejectsActiveArchivedDuplicate(t *testing.T) {
	home := t.TempDir()
	writeFixture(t, home, fixtureSession, false, true)
	writeFixture(t, home, fixtureSession, true, true)
	collector := Collector{CodexHome: home}
	selection := Selection{
		Since: selectionSince, Until: selectionUntil,
		ExplicitSessions: []string{fixtureSession}, IncludeArchived: true,
	}
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	_, err = collector.Collect(context.Background(), plan)
	if !errors.Is(err, ErrExplicitSource) {
		t.Fatalf("duplicate explicit source error = %v", err)
	}
}

func TestCollectionAllowsAppendButRejectsMutationInsideSnapshot(t *testing.T) {
	home := t.TempDir()
	path := writeFixture(t, home, fixtureSession, false, true)
	selection := baseSelection()
	appendCollector := Collector{CodexHome: home, afterRead: func(path string) {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.WriteString("{\"appended\":true}\n")
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}}
	plan, err := appendCollector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := appendCollector.Collect(context.Background(), plan)
	if err != nil || len(result.Turns) != 1 {
		t.Fatalf("append beyond boundary was not accepted: result=%+v err=%v", result, err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents[:len(contents)-len("{\"appended\":true}\n")], 0o600); err != nil {
		t.Fatal(err)
	}
	mutationCollector := Collector{CodexHome: home, afterRead: func(path string) {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.WriteAt([]byte("{"), 0)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}}
	plan, err = mutationCollector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err = mutationCollector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	// Writing the same leading byte would not mutate the fixture, so force a
	// changed byte and rerun when needed.
	if result.Exclusions["native_unstable"] == 0 {
		mutationCollector.afterRead = func(path string) {
			file, openErr := os.OpenFile(path, os.O_RDWR, 0)
			if openErr != nil {
				t.Fatal(openErr)
			}
			_, writeErr := file.WriteAt([]byte(" "), 0)
			_ = file.Close()
			if writeErr != nil {
				t.Fatal(writeErr)
			}
		}
		plan, err = mutationCollector.Discover(context.Background(), selection)
		if err != nil {
			t.Fatal(err)
		}
		result, err = mutationCollector.Collect(context.Background(), plan)
		if err != nil {
			t.Fatal(err)
		}
	}
	if result.Exclusions["native_unstable"] != 1 {
		t.Fatalf("snapshot mutation was not rejected: %+v", result)
	}
}

func TestIncompleteAndUnknownSourcesFailClosed(t *testing.T) {
	t.Run("incomplete", func(t *testing.T) {
		home := t.TempDir()
		path := writeFixture(t, home, fixtureSession, false, true)
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents[:len(contents)-1], 0o600); err != nil {
			t.Fatal(err)
		}
		collector := Collector{CodexHome: home}
		plan, err := collector.Discover(context.Background(), baseSelection())
		if err != nil {
			t.Fatal(err)
		}
		result, err := collector.Collect(context.Background(), plan)
		if err != nil || result.Exclusions["native_incomplete"] != 1 {
			t.Fatalf("incomplete source was not omitted: result=%+v err=%v", result, err)
		}
	})

	t.Run("unknown essential record", func(t *testing.T) {
		home := t.TempDir()
		path := writeFixture(t, home, fixtureSession, false, true)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.WriteString("{\"timestamp\":\"2026-08-17T12:00:04Z\",\"type\":\"future_essential_record\",\"payload\":{}}\n")
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		collector := Collector{CodexHome: home}
		plan, err := collector.Discover(context.Background(), baseSelection())
		if err != nil {
			t.Fatal(err)
		}
		result, err := collector.Collect(context.Background(), plan)
		if err != nil || result.Exclusions["native_unsupported"] != 1 {
			t.Fatalf("unknown source was not omitted: result=%+v err=%v", result, err)
		}
	})
}

func TestExplicitMissingSessionFailsDiscovery(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	collector := Collector{CodexHome: home}
	_, err := collector.Discover(context.Background(), Selection{Since: selectionSince, Until: selectionUntil, ExplicitSessions: []string{fixtureSession}})
	if !errors.Is(err, ErrExplicitSource) {
		t.Fatalf("missing explicit session error = %v", err)
	}
}

func TestUnsupportedCodexVersionIsOmittedOrFailsExplicitSelection(t *testing.T) {
	home := t.TempDir()
	path := writeFixture(t, home, fixtureSession, false, false)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("0.147.0-alpha.1.2"), []byte("9.9.9"))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	collector := Collector{CodexHome: home}
	plan, err := collector.Discover(context.Background(), baseSelection())
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil || result.Exclusions["native_unsupported"] != 1 || len(result.Turns) != 0 {
		t.Fatalf("unsupported version result=%+v err=%v", result, err)
	}
	explicit := baseSelection()
	explicit.ExplicitSessions = []string{fixtureSession}
	plan, err = collector.Discover(context.Background(), explicit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collector.Collect(context.Background(), plan); !errors.Is(err, ErrExplicitSource) {
		t.Fatalf("explicit unsupported version error = %v", err)
	}
}

func baseSelection() Selection {
	return Selection{
		Since: selectionSince,
		Until: selectionUntil,
		Traces: []TraceReference{{
			EventID:        eventID,
			StartedAt:      time.Date(2026, 8, 17, 12, 0, 1, 150_000_000, time.UTC),
			CompletedAt:    time.Date(2026, 8, 17, 12, 0, 2, 500_000_000, time.UTC),
			RequestedModel: "gpt-example",
			RequestBody:    []byte(`{"model":"gpt-example","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Classify this request."}]}]}`),
		}},
	}
}

func writeFixture(t *testing.T, home, sessionID string, archived, trailingNewline bool) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", "rollout-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.ReplaceAll(string(contents), fixtureSession, sessionID))
	if !trailingNewline {
		contents = contents[:len(contents)-1]
	}
	name := "rollout-2026-08-17T12-00-00-" + sessionID + ".jsonl"
	dir := filepath.Join(home, "sessions", "2026", "08", "17")
	if archived {
		dir = filepath.Join(home, "archived_sessions")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
