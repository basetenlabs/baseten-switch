package claude

import (
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
	fixtureEvent   = "0123456789abcdef0123456789abcdef"
)

var (
	selectionSince = time.Date(2026, 8, 13, 15, 4, 0, 0, time.UTC)
	selectionUntil = time.Date(2026, 8, 13, 15, 5, 0, 0, time.UTC)
)

func TestCollectorCorrelatesAndNormalizesTerminalTurn(t *testing.T) {
	root, sessionPath := installFixture(t, fixtureSession, "")
	collector := Collector{ConfigRoot: root}
	selection := fixtureSelection(`{"id":"msg_fixture_final","type":"message"}`)

	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CandidateFileCount != 1 || plan.CandidateBytes == 0 {
		t.Fatalf("unexpected discovery plan: %+v", plan)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || result.TraceLinks[fixtureEvent] == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	turn := result.Turns[0]
	if turn.MatchMode != "response_id" || len(turn.SwitchEventIDs) != 1 ||
		turn.BundleSessionID == fixtureSession || turn.BundleTurnID == "" {
		t.Fatalf("unexpected normalized turn: %+v", turn)
	}
	if len(turn.Events) != 4 {
		t.Fatalf("got %d normalized events, want 4", len(turn.Events))
	}
	toolUse := turn.Events[1].Content[2].BundleToolCallID
	toolResult := turn.Events[2].Content[0].BundleToolCallID
	if toolUse == nil || toolResult == nil || *toolUse != *toolResult || *toolUse == "toolu_fixture_1" {
		t.Fatalf("tool relationship was not remapped: use=%v result=%v", toolUse, toolResult)
	}
	encoded, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		fixtureSession, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "toolu_fixture_1",
		"request-private-structure", "opaque-signature", "opaque-encrypted-value",
		"private-branch", sessionPath,
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("normalized turn exposed structural or excluded value %q", forbidden)
		}
	}
	if !strings.Contains(string(encoded), "I should use the synthetic tool.") {
		t.Fatal("visible thinking was not preserved")
	}
}

func TestCollectorExtractsStreamingMessageStartID(t *testing.T) {
	root, _ := installFixture(t, fixtureSession, "")
	selection := fixtureSelection("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture_final\"}}\n\n")
	collector := Collector{ConfigRoot: root}
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(result.Turns))
	}
}

func TestKeyedSessionNarrowsDuplicateResponseID(t *testing.T) {
	root, _ := installFixture(t, fixtureSession, "")
	installFixtureAtRoot(t, root, secondSession, "")
	keyDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := tracecapture.LoadOrCreateCorrelationKey(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := key.Hash(ClientName, "session", fixtureSession)
	if err != nil {
		t.Fatal(err)
	}
	selection := fixtureSelection(`{"id":"msg_fixture_final"}`)
	selection.Traces[0].NativeCorrelation = &tracecapture.NativeCorrelationV1{
		Scheme: "hmac-sha256-v1", KeyID: key.ID(), Session: &hash,
	}
	collector := Collector{ConfigRoot: root, CorrelationKey: key}
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CandidateFileCount != 2 {
		t.Fatalf("got %d candidates, want both conservative response-ID candidates", plan.CandidateFileCount)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || result.Exclusions["ambiguous"] != 0 {
		t.Fatalf("keyed scope did not disambiguate: %+v", result)
	}
}

func TestDuplicateResponseIDWithoutKeyIsAmbiguous(t *testing.T) {
	root, _ := installFixture(t, fixtureSession, "")
	installFixtureAtRoot(t, root, secondSession, "")
	collector := Collector{ConfigRoot: root}
	plan, err := collector.Discover(context.Background(), fixtureSelection(`{"id":"msg_fixture_final"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 0 || result.Exclusions["ambiguous"] != 1 {
		t.Fatalf("unexpected ambiguity result: %+v", result)
	}
}

func TestKeyedAgentNarrowsDuplicateSubagentResponseID(t *testing.T) {
	root := t.TempDir()
	installFixtureAtRoot(t, root, fixtureSession, "agent-one")
	installFixtureAtRoot(t, root, fixtureSession, "agent-two")
	keyDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := tracecapture.LoadOrCreateCorrelationKey(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	sessionHash, _ := key.Hash(ClientName, "session", fixtureSession)
	agentHash, _ := key.Hash(ClientName, "agent", "agent-one")
	selection := fixtureSelection(`{"id":"msg_fixture_final"}`)
	selection.Traces[0].NativeCorrelation = &tracecapture.NativeCorrelationV1{
		Scheme: "hmac-sha256-v1", KeyID: key.ID(), Session: &sessionHash, Agent: &agentHash,
	}
	collector := Collector{ConfigRoot: root, CorrelationKey: key}
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || result.Turns[0].BundleAgentID == nil ||
		result.Exclusions["ambiguous"] != 0 {
		t.Fatalf("keyed agent did not disambiguate: %+v", result)
	}
}

func TestTimestampOverlapAloneIncludesNothing(t *testing.T) {
	root, _ := installFixture(t, fixtureSession, "")
	selection := fixtureSelection(`{"id":"msg_different"}`)
	collector := Collector{ConfigRoot: root}
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 0 || result.Exclusions["unmatched"] != 1 {
		t.Fatalf("timestamp-only candidate was included: %+v", result)
	}
}

func TestExplicitSessionProducesUnlinkedContext(t *testing.T) {
	root, _ := installFixture(t, fixtureSession, "")
	selection := Selection{
		Since: selectionSince, Until: selectionUntil,
		ExplicitSessions: []string{fixtureSession},
	}
	collector := Collector{ConfigRoot: root}
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || result.Turns[0].MatchMode != "explicit_session" ||
		len(result.Turns[0].SwitchEventIDs) != 0 || len(result.TraceLinks) != 0 {
		t.Fatalf("unexpected explicit result: %+v", result)
	}
}

func TestCompactionMarkersAreAttachedOnlyToFollowingSelectedTurn(t *testing.T) {
	root, path := installFixture(t, fixtureSession, "")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	additional := strings.Join([]string{
		`{"type":"system","subtype":"compact_boundary","uuid":"11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa","parentUuid":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee","sessionId":"` + fixtureSession + `","timestamp":"2026-08-13T15:04:10Z","version":"2.1.139"}`,
		`{"type":"user","isCompactSummary":true,"uuid":"22222222-aaaa-4aaa-8aaa-aaaaaaaaaaaa","parentUuid":"11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa","sessionId":"` + fixtureSession + `","timestamp":"2026-08-13T15:04:11Z","version":"2.1.139","message":{"role":"user","content":"Synthetic compact summary."}}`,
		`{"type":"user","uuid":"33333333-aaaa-4aaa-8aaa-aaaaaaaaaaaa","parentUuid":"22222222-aaaa-4aaa-8aaa-aaaaaaaaaaaa","sessionId":"` + fixtureSession + `","timestamp":"2026-08-13T15:04:12Z","version":"2.1.139","message":{"role":"user","content":"Continue after compaction."}}`,
		`{"type":"assistant","uuid":"44444444-aaaa-4aaa-8aaa-aaaaaaaaaaaa","parentUuid":"33333333-aaaa-4aaa-8aaa-aaaaaaaaaaaa","sessionId":"` + fixtureSession + `","timestamp":"2026-08-13T15:04:13Z","version":"2.1.139","message":{"id":"msg_after_compact","role":"assistant","content":[{"type":"text","text":"Continued."}],"stop_reason":"end_turn"}}`,
	}, "\n") + "\n"
	if _, err := file.WriteString(additional); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	selection := fixtureSelection(`{"id":"msg_after_compact"}`)
	collector := Collector{ConfigRoot: root}
	plan, err := collector.Discover(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || len(result.Turns[0].Events) != 4 ||
		result.Turns[0].Events[0].Kind != "compaction" ||
		result.Turns[0].Events[1].Kind != "compaction" {
		t.Fatalf("compaction context was not attached to the selected turn: %+v", result)
	}
}

func TestDiscoveryRejectsExplicitSymlinkAndIgnoresUnrecognizedFiles(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "projects", "synthetic-project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), fixtureSession+".jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(project, fixtureSession+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "history.jsonl"), []byte("private history\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := Collector{ConfigRoot: root}
	_, err := collector.Discover(context.Background(), Selection{
		Since: selectionSince, Until: selectionUntil, ExplicitSessions: []string{fixtureSession},
	})
	if !errors.Is(err, ErrExplicitSource) {
		t.Fatalf("got %v, want explicit source error", err)
	}
}

func TestResolveConfigRootUsesClaudeOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "claude-data")
	t.Setenv("CLAUDE_CONFIG_DIR", want)
	got, err := ResolveConfigRoot("")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	explicit := filepath.Join(t.TempDir(), "explicit")
	got, err = ResolveConfigRoot(explicit)
	if err != nil || got != explicit {
		t.Fatalf("explicit root got %q, err=%v", got, err)
	}
}

func TestDiscoverRejectsSymlinkedConfigRoot(t *testing.T) {
	realRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(realRoot, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "claude-root")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	collector := Collector{ConfigRoot: link}
	selection := Selection{
		Since: time.Now().Add(-time.Hour), Until: time.Now(),
		ExplicitSessions: []string{"00000000-0000-4000-8000-000000000001"},
	}
	if _, err := collector.Discover(context.Background(), selection); err == nil {
		t.Fatal("Discover accepted a symlinked Claude Code config root")
	}
}

func TestSnapshotAllowsAppendAndRejectsBoundaryMutation(t *testing.T) {
	root, path := installFixture(t, fixtureSession, "")
	selection := fixtureSelection(`{"id":"msg_fixture_final"}`)
	appendCollector := Collector{ConfigRoot: root, afterRead: func(path string) {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.WriteString("{\"type\":\"custom-title\"}\n")
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
		t.Fatalf("append should be allowed, result=%+v err=%v", result, err)
	}

	root, path = installFixture(t, fixtureSession, "")
	mutationCollector := Collector{ConfigRoot: root, afterRead: func(string) {
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.WriteAt([]byte("X"), 1)
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
	if len(result.Turns) != 0 || result.Exclusions["unstable"] != 1 {
		t.Fatalf("boundary mutation was not rejected: %+v", result)
	}
}

func TestUnsupportedVersionAndIncompleteFinalRecordFailClosed(t *testing.T) {
	root, path := installFixture(t, fixtureSession, "")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), `"version":"2.1.139"`, `"version":"2.2.0"`))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	collector := Collector{ConfigRoot: root}
	plan, err := collector.Discover(context.Background(), fixtureSelection(`{"id":"msg_fixture_final"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := collector.Collect(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Exclusions["unsupported"] != 1 || len(result.Turns) != 0 {
		t.Fatalf("unsupported version was not omitted: %+v", result)
	}

	root = t.TempDir()
	project := filepath.Join(root, "projects", "synthetic-project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	incomplete := `{"type":"user","uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","parentUuid":null,"sessionId":"` + fixtureSession + `","timestamp":"2026-08-13T15:04:05Z","version":"2.1.139","message":{"role":"user","content":"synthetic"}}` + "\n" +
		`{"type":"assistant","uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","parentUuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","sessionId":"` + fixtureSession + `","timestamp":"2026-08-13T15:04:08Z","version":"2.1.139","message":{"id":"msg_fixture_final","role":"assistant","content":[{"type":"text","text":"not committed"}],"stop_reason":"end_turn"}}`
	if err := os.WriteFile(filepath.Join(project, fixtureSession+".jsonl"), []byte(incomplete), 0o600); err != nil {
		t.Fatal(err)
	}
	collector = Collector{ConfigRoot: root}
	incompleteSelection := fixtureSelection(`{"id":"msg_fixture_final"}`)
	incompleteSelection.ExplicitSessions = []string{fixtureSession}
	plan, err = collector.Discover(context.Background(), incompleteSelection)
	if err != nil {
		t.Fatal(err)
	}
	_, err = collector.Collect(context.Background(), plan)
	if !errors.Is(err, ErrExplicitSource) {
		t.Fatalf("got %v, want explicit source error for incomplete turn", err)
	}
}

func fixtureSelection(response string) Selection {
	return Selection{
		Since: selectionSince, Until: selectionUntil,
		Traces: []TraceReference{{
			EventID: fixtureEvent, StartedAt: selectionSince.Add(4 * time.Second),
			CompletedAt: selectionSince.Add(10 * time.Second), ResponseBody: []byte(response),
		}},
	}
}

func installFixture(t *testing.T, sessionID, agentID string) (string, string) {
	t.Helper()
	root := t.TempDir()
	return root, installFixtureAtRoot(t, root, sessionID, agentID)
}

func installFixtureAtRoot(t *testing.T, root, sessionID, agentID string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "session-v2.1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), fixtureSession, sessionID))
	project := filepath.Join(root, "projects", "synthetic-project")
	path := filepath.Join(project, sessionID+".jsonl")
	if agentID != "" {
		project = filepath.Join(project, sessionID, "subagents")
		path = filepath.Join(project, "agent-"+agentID+".jsonl")
		raw = []byte(strings.ReplaceAll(string(raw), `"version":"2.1.139"`,
			`"version":"2.1.139","agentId":"`+agentID+`","isSidechain":true`))
	}
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
