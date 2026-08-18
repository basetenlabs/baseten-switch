package tracepackage

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

const (
	selectedEventID = "00112233445566778899aabbccddeeff"
	otherEventID    = "ffeeddccbbaa99887766554433221100"
)

func TestCreateBuildsSwitchOnlyPackageWithExactTelemetryJoin(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	selectedTrace := traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour))
	otherClient := traceJSON(t, otherEventID, "other-client", base.Add(time.Hour))
	outsideTime := traceJSON(t, otherEventID, "claude-code", base.Add(-time.Hour))
	traceData := jsonl(selectedTrace, otherClient, outsideTime)

	selectedTelemetry := telemetryJSON(t, selectedEventID, base.Add(time.Hour))
	otherTelemetry := telemetryJSON(t, otherEventID, base.Add(time.Hour))
	telemetryData := jsonl(otherTelemetry, selectedTelemetry)

	var scannerInputs []BodyForScan
	verifiedTraces := false
	verifiedTelemetry := false
	destination := filepath.Join(t.TempDir(), "training.zip")
	options := Options{
		Destination: destination,
		Selection: Selection{
			Since:   base,
			Until:   base.Add(2 * time.Hour),
			Clients: []string{"claude-code"},
		},
		SwitchVersion: "v-test",
		Sources: Sources{
			Traces: func(context.Context, Selection) (Snapshot, error) {
				return bytesSnapshot(traceData, func() { verifiedTraces = true }), nil
			},
			Telemetry: func(_ context.Context, _ Selection, eventIDs []string) (Snapshot, error) {
				if !reflect.DeepEqual(eventIDs, []string{selectedEventID}) {
					t.Fatalf("telemetry event IDs = %v", eventIDs)
				}
				return bytesSnapshot(telemetryData, func() { verifiedTelemetry = true }), nil
			},
		},
		Scanner: Scanner{
			Version: "scanner-v1",
			Scan: func(_ context.Context, body BodyForScan) (BodyScanResult, error) {
				scannerInputs = append(scannerInputs, body)
				return BodyScanResult{Scanned: true}, nil
			},
		},
		Now: fixedClock(base.Add(3*time.Hour), base.Add(4*time.Hour)),
	}

	result, err := Create(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Published || result.TraceCount != 1 || result.TelemetryCount != 1 {
		t.Fatalf("result = %+v", result)
	}
	if !verifiedTraces || !verifiedTelemetry {
		t.Fatalf("snapshot verification: traces=%v telemetry=%v", verifiedTraces, verifiedTelemetry)
	}
	if len(scannerInputs) != 2 || scannerInputs[0].BodyBase64 != "e30=" ||
		scannerInputs[1].BodyBase64 != "ZGF0YToge30K" {
		t.Fatalf("scanner inputs = %+v", scannerInputs)
	}

	archive := readZIP(t, destination)
	if got := sortedKeys(archive); !reflect.DeepEqual(got, []string{
		ManifestMemberName,
		TelemetryMemberName,
		TraceMemberName,
	}) {
		t.Fatalf("ZIP members = %v", got)
	}

	var packaged PackagedTraceV1
	traceLine := bytes.TrimSpace(archive[TraceMemberName])
	if err := json.Unmarshal(traceLine, &packaged); err != nil {
		t.Fatal(err)
	}
	if packaged.PackageSchemaVersion != PackageSchemaVersionV1 ||
		packaged.SourceTraceSchemaVersion != 1 || packaged.NativeTurnID != nil {
		t.Fatalf("packaged trace = %+v", packaged)
	}
	if _, present := packaged.Fields["native_correlation"]; present {
		t.Fatal("native_correlation leaked into package")
	}
	if !bytes.Contains(traceLine, []byte(`"body_base64":"e30="`)) ||
		!bytes.Contains(traceLine, []byte(`"body_base64":"ZGF0YToge30K"`)) {
		t.Fatalf("exact body encodings missing from %s", traceLine)
	}
	if !bytes.Contains(archive[TelemetryMemberName], []byte(selectedEventID)) ||
		bytes.Contains(archive[TelemetryMemberName], []byte(otherEventID)) {
		t.Fatalf("telemetry join = %s", archive[TelemetryMemberName])
	}

	var manifest ManifestV1
	if err := json.Unmarshal(archive[ManifestMemberName], &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.NoUploadPerformed || manifest.SwitchVersion != "v-test" ||
		!reflect.DeepEqual(manifest.CorrelationMethods, []string{"telemetry_event_id"}) {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Scanner.ScannedBodyCount != 2 || manifest.Scanner.UnscannedBodyCount != 0 {
		t.Fatalf("scanner manifest = %+v", manifest.Scanner)
	}
	assertMemberManifests(t, manifest.Members, archive)

	archiveHash := sha256.Sum256(mustReadFile(t, destination))
	if result.ArchiveSHA256 != hex.EncodeToString(archiveHash[:]) {
		t.Fatalf("archive hash = %s", result.ArchiveSHA256)
	}
	if stages, err := filepath.Glob(filepath.Join(filepath.Dir(destination), ".baseten-switch-package-*")); err != nil || len(stages) != 0 {
		t.Fatalf("staging remnants = %v, err=%v", stages, err)
	}
}

func TestCreateAddsValidatedNormalizedNativeMemberAndBidirectionalLink(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	options := basicOptions(filepath.Join(t.TempDir(), "native.zip"), base, jsonl(
		traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour)),
	))
	turnID := "bundle-turn-example"
	row, err := json.Marshal(map[string]any{
		"native_schema_version": 1,
		"client":                "claude-code",
		"bundle_session_id":     "bundle-session-example",
		"bundle_turn_id":        turnID,
		"switch_event_ids":      []string{selectedEventID},
		"match_mode":            "response_id",
		"status":                "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	options.NativeMembers = []NativeMember{{
		Name: "native/claude-code/turns.jsonl", Client: "claude-code", SourceKind: "claude-code-session-jsonl", Rows: []json.RawMessage{row},
		CollectorVersion: "claude-test-v1", ClientVersions: []string{"2.1.0"},
		CorrelationMethods: []string{"response_id"},
	}}
	options.TraceNativeLinks = map[string]string{selectedEventID: turnID}

	result, err := Create(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if result.NativeTurnCount != 1 {
		t.Fatalf("result = %#v", result)
	}
	archive := readZIP(t, options.Destination)
	if !bytes.Equal(bytes.TrimSpace(archive["native/claude-code/turns.jsonl"]), row) {
		t.Fatalf("native member = %s", archive["native/claude-code/turns.jsonl"])
	}
	var packaged PackagedTraceV1
	if err := json.Unmarshal(bytes.TrimSpace(archive[TraceMemberName]), &packaged); err != nil {
		t.Fatal(err)
	}
	if packaged.NativeTurnID == nil || *packaged.NativeTurnID != turnID {
		t.Fatalf("native turn link = %#v", packaged.NativeTurnID)
	}
	var manifest ManifestV1
	if err := json.Unmarshal(archive[ManifestMemberName], &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.NativeCollectors) != 1 || manifest.NativeCollectors[0].CollectorVersion != "claude-test-v1" ||
		manifest.Scanner.ScannedNativeRecordCount != 1 {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestCreateRejectsOneSidedNativeLinkage(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	options := basicOptions(filepath.Join(t.TempDir(), "native.zip"), base, jsonl(
		traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour)),
	))
	row := json.RawMessage(`{"bundle_turn_id":"turn","switch_event_ids":[]}`)
	options.NativeMembers = []NativeMember{{Name: "native/claude-code/turns.jsonl", Rows: []json.RawMessage{row}}}
	options.TraceNativeLinks = map[string]string{selectedEventID: "turn"}
	if _, err := Create(context.Background(), options); err == nil || !strings.Contains(err.Error(), "bidirectional") {
		t.Fatalf("Create() error = %v", err)
	}
}

func TestCreateRecordsOperatorSelectedNativeTurnsAndZeroRowExclusions(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	options := basicOptions(filepath.Join(t.TempDir(), "native.zip"), base, jsonl(
		traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour)),
	))
	row := json.RawMessage(`{"bundle_turn_id":"operator-turn","switch_event_ids":[],"match_mode":"explicit_session"}`)
	options.NativeMembers = []NativeMember{
		{Name: "native/claude-code/turns.jsonl", Client: "claude-code", SourceKind: "claude-code-session-jsonl", CollectorVersion: "test-v1", Rows: []json.RawMessage{row}},
		{Name: "native/codex/turns.jsonl", Client: "codex", SourceKind: "codex-rollout-jsonl", CollectorVersion: "test-v1", Exclusions: map[string]int{"no_match": 2}},
	}
	options.OperatorSelectedNativeTurns = []string{"operator-turn"}
	if _, err := Create(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	var manifest ManifestV1
	if err := json.Unmarshal(readZIP(t, options.Destination)[ManifestMemberName], &manifest); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifest.OperatorSelectedNativeTurns, []string{"operator-turn"}) || manifest.Exclusions["no_match"] != 2 {
		t.Fatalf("manifest provenance = %#v", manifest)
	}
	if len(manifest.NativeCollectors) != 2 || manifest.NativeCollectors[1].Client != "codex" || manifest.NativeCollectors[1].Member != "" {
		t.Fatalf("native collectors = %#v", manifest.NativeCollectors)
	}

	options.Destination = filepath.Join(t.TempDir(), "incomplete.zip")
	options.OperatorSelectedNativeTurns = nil
	if _, err := Create(context.Background(), options); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("Create() error = %v", err)
	}
}

func TestCreateRejectsUnknownTraceFields(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	var row map[string]any
	if err := json.Unmarshal(traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour)), &row); err != nil {
		t.Fatal(err)
	}
	row["future_unvalidated_field"] = "must fail closed"
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	options := basicOptions(filepath.Join(t.TempDir(), "unknown.zip"), base, jsonl(encoded))
	if _, err := Create(context.Background(), options); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Create() error = %v", err)
	}
}

func TestCreateBlocksUnscannedContentUnlessAcknowledged(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	options := basicOptions(
		filepath.Join(directory, "blocked.zip"),
		base,
		jsonl(traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour))),
	)
	options.Scanner = Scanner{}
	if _, err := Create(context.Background(), options); !errors.Is(err, ErrUnscannedContent) {
		t.Fatalf("Create error = %v, want ErrUnscannedContent", err)
	}
	if _, err := os.Lstat(options.Destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked destination exists: %v", err)
	}

	options.Destination = filepath.Join(directory, "allowed.zip")
	options.AllowUnscannedContent = true
	result, err := Create(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Published {
		t.Fatalf("result = %+v", result)
	}
	var manifest ManifestV1
	if err := json.Unmarshal(readZIP(t, options.Destination)[ManifestMemberName], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Scanner.UnscannedBodyCount != 2 || !manifest.Scanner.AllowUnscannedContentUsed {
		t.Fatalf("scanner manifest = %+v", manifest.Scanner)
	}
}

func TestCreateBlocksDetectedCredentialsUnlessAcknowledged(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	options := basicOptions(
		filepath.Join(directory, "blocked.zip"),
		base,
		jsonl(traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour))),
	)
	options.Scanner = Scanner{
		Version: "scanner-v1",
		Scan: func(context.Context, BodyForScan) (BodyScanResult, error) {
			return BodyScanResult{Scanned: true, DetectedCategories: []string{"api_key", "api_key"}}, nil
		},
	}
	if _, err := Create(context.Background(), options); !errors.Is(err, ErrDetectedSecrets) {
		t.Fatalf("Create error = %v, want ErrDetectedSecrets", err)
	}

	options.Destination = filepath.Join(directory, "allowed.zip")
	options.AllowDetectedSecrets = true
	if _, err := Create(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	var manifest ManifestV1
	if err := json.Unmarshal(readZIP(t, options.Destination)[ManifestMemberName], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Scanner.DetectedCategoryCounts["api_key"] != 2 ||
		!manifest.Scanner.AllowDetectedSecretsUsed {
		t.Fatalf("scanner manifest = %+v", manifest.Scanner)
	}
}

func TestCreateRefusesOverwrite(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	destination := filepath.Join(t.TempDir(), "existing.zip")
	if err := os.WriteFile(destination, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := basicOptions(destination, base, nil)
	if _, err := Create(context.Background(), options); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("Create error = %v, want ErrDestinationExists", err)
	}
	if got := string(mustReadFile(t, destination)); got != "keep" {
		t.Fatalf("destination = %q", got)
	}
}

func TestCreateFailsWhenSnapshotChanges(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	destination := filepath.Join(t.TempDir(), "training.zip")
	options := basicOptions(
		destination,
		base,
		jsonl(traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour))),
	)
	options.Sources.Traces = func(context.Context, Selection) (Snapshot, error) {
		return Snapshot{
			EstimatedBytes: 1,
			Open: func(context.Context) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("{}\n")), nil
			},
			Verify: func(context.Context) error { return errors.New("replaced") },
		}, nil
	}
	// Use a valid row so the post-read Verify callback is reached.
	valid := jsonl(traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour)))
	options.Sources.Traces = func(context.Context, Selection) (Snapshot, error) {
		snapshot := bytesSnapshot(valid, nil)
		snapshot.Verify = func(context.Context) error { return errors.New("replaced") }
		return snapshot, nil
	}
	if _, err := Create(context.Background(), options); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatalf("Create error = %v, want ErrSnapshotChanged", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination exists after unstable snapshot: %v", err)
	}
}

func TestCreateIgnoresPartialFinalTraceLine(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	complete := traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour))
	data := append(jsonl(complete), []byte(`{"schema_version":1`)...)
	options := basicOptions(filepath.Join(t.TempDir(), "training.zip"), base, data)
	result, err := Create(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if result.TraceCount != 1 {
		t.Fatalf("trace count = %d", result.TraceCount)
	}
}

func TestCreateReportsQuarantinedCleanupAfterPublication(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	destination := filepath.Join(t.TempDir(), "training.zip")
	options := basicOptions(destination, base, nil)
	originalRemoveAll := removeAllPath
	removeAllPath = func(string) error { return errors.New("forced cleanup failure") }
	t.Cleanup(func() { removeAllPath = originalRemoveAll })
	quarantined := false
	options.Quarantine = func(_ context.Context, stageDir string) (string, error) {
		quarantined = true
		if err := originalRemoveAll(stageDir); err != nil {
			return "", err
		}
		return "cleanup_01", nil
	}
	result, err := Create(context.Background(), options)
	var cleanup *CleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("Create error = %v, want CleanupError", err)
	}
	if !result.Published || result.CleanupID != "cleanup_01" || !cleanup.Published || !quarantined {
		t.Fatalf("result=%+v cleanup=%+v quarantined=%v", result, cleanup, quarantined)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("published destination missing: %v", err)
	}
}

func TestValidateMemberName(t *testing.T) {
	valid := []string{ManifestMemberName, TraceMemberName, TelemetryMemberName}
	for _, name := range valid {
		if err := ValidateMemberName(name); err != nil {
			t.Errorf("ValidateMemberName(%q) = %v", name, err)
		}
	}
	invalid := []string{"", "/absolute", "../escape", "a/../b", "a\\b", ".", "a//b"}
	for _, name := range invalid {
		if err := ValidateMemberName(name); !errors.Is(err, ErrUnsafeMember) {
			t.Errorf("ValidateMemberName(%q) = %v", name, err)
		}
	}
}

func TestSelectionValidate(t *testing.T) {
	base := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	tests := []Selection{
		{},
		{Since: base, Until: base, Clients: []string{"codex"}},
		{Since: base, Until: base.Add(MaxSelectionInterval + time.Second), Clients: []string{"codex"}},
		{Since: base, Until: base.Add(time.Hour)},
		{Since: base, Until: base.Add(time.Hour), Clients: []string{"codex", "codex"}},
		{Since: base, Until: base.Add(time.Hour), Clients: []string{" codex"}},
	}
	for _, selection := range tests {
		if err := selection.Validate(); !errors.Is(err, ErrInvalidSelection) {
			t.Errorf("Selection.Validate(%+v) = %v", selection, err)
		}
	}
}

func basicOptions(destination string, base time.Time, traces []byte) Options {
	return Options{
		Destination: destination,
		Selection: Selection{
			Since:   base,
			Until:   base.Add(2 * time.Hour),
			Clients: []string{"claude-code"},
		},
		Sources: Sources{
			Traces: func(context.Context, Selection) (Snapshot, error) {
				return bytesSnapshot(traces, nil), nil
			},
		},
		Scanner: Scanner{
			Version: "scanner-v1",
			Scan: func(context.Context, BodyForScan) (BodyScanResult, error) {
				return BodyScanResult{Scanned: true}, nil
			},
		},
		Now: func() time.Time { return base.Add(3 * time.Hour) },
	}
}

func bytesSnapshot(data []byte, verified func()) Snapshot {
	return Snapshot{
		EstimatedBytes: int64(len(data)),
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
		Verify: func(context.Context) error {
			if verified != nil {
				verified()
			}
			return nil
		},
	}
}

func traceJSON(t *testing.T, eventID, client string, startedAt time.Time) []byte {
	t.Helper()
	value := map[string]any{
		"schema_version": 1,
		"event":          "request_response_trace",
		"event_id":       eventID,
		"native_correlation": map[string]any{
			"scheme":  "hmac-sha256-v1",
			"key_id":  "0011223344556677",
			"session": "00112233445566778899aabbccddeeff",
		},
		"started_at":         startedAt,
		"completed_at":       startedAt.Add(time.Second),
		"client":             client,
		"protocol_shape":     "anthropic",
		"api_kind":           "messages",
		"endpoint":           "/v1/messages",
		"configured_route":   "baseten",
		"effective_provider": "baseten",
		"requested_model":    "claude-example",
		"served_model":       "provider/model",
		"status":             200,
		"termination_reason": "completed",
		"outcome_source":     "provider",
		"provider_outcome":   "completed",
		"is_stream":          true,
		"provider_stateful":  false,
		"translated":         false,
		"sanitized":          false,
		"fallback": map[string]any{
			"attempted": false, "count": 0, "primary_attempted": true, "trigger": nil,
		},
		"request": map[string]any{
			"boundary":         "client_ingress",
			"content_type":     "application/json",
			"content_encoding": "",
			"body_encoding":    "base64",
			"body_base64":      "e30=",
			"observed_bytes":   2,
			"capture_state":    "captured",
		},
		"response": map[string]any{
			"boundary":               "gateway_egress",
			"content_type":           "text/event-stream",
			"content_encoding":       "",
			"body_encoding":          "base64",
			"body_base64":            "ZGF0YToge30K",
			"observed_bytes":         9,
			"capture_state":          "captured",
			"gateway_write_complete": true,
			"protocol_complete":      true,
		},
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func telemetryJSON(t *testing.T, eventID string, startedAt time.Time) []byte {
	t.Helper()
	revision := "fixture"
	nanoUSD := int64(0)
	zero := int64(0)
	status := 200
	event := telemetry.EventV1{
		SchemaVersion:        telemetry.SchemaVersionV1,
		Event:                telemetry.EventRequest,
		EventID:              eventID,
		StartedAt:            startedAt,
		CompletedAt:          startedAt.Add(time.Second),
		Client:               "claude-code",
		ConfiguredRoute:      "baseten",
		EffectiveProvider:    "baseten",
		RequestedModel:       "claude-example",
		RequestedModelFamily: "claude",
		ModelFamilyRevision:  "fixture",
		ServedModel:          "provider/model",
		Status:               &status,
		DurationMS:           1000,
		TerminationReason:    telemetry.TerminationCompleted,
		UsageComplete:        true,
		Usage: telemetry.UsageV1{
			InputTokens:             &zero,
			OutputTokens:            &zero,
			CacheReadInputTokens:    &zero,
			CacheWrite5mInputTokens: &zero,
			CacheWrite1hInputTokens: &zero,
		},
		ActualCost: telemetry.CostSnapshotV1{
			Priced:               true,
			NanoUSD:              &nanoUSD,
			Source:               "fixture",
			Revision:             &revision,
			CapturedAt:           &startedAt,
			RatesNanoUSDPerToken: &telemetry.TokenRatesV1{},
		},
		StrippedToolTypes: []string{},
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func jsonl(lines ...[]byte) []byte {
	var result []byte
	for _, line := range lines {
		result = append(result, line...)
		result = append(result, '\n')
	}
	return result
}

func fixedClock(values ...time.Time) func() time.Time {
	index := 0
	return func() time.Time {
		if index >= len(values) {
			return values[len(values)-1]
		}
		value := values[index]
		index++
		return value
	}
}

func readZIP(t *testing.T, path string) map[string][]byte {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	result := make(map[string][]byte, len(reader.File))
	for _, file := range reader.File {
		if file.Mode().Perm() != 0o600 || !file.Mode().IsRegular() {
			t.Fatalf("ZIP member %s mode = %v", file.Name, file.Mode())
		}
		opened, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(opened)
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal(errors.Join(readErr, closeErr))
		}
		result[file.Name] = data
	}
	return result
}

func assertMemberManifests(t *testing.T, members []MemberManifestV1, archive map[string][]byte) {
	t.Helper()
	if len(members) != 2 {
		t.Fatalf("member manifests = %+v", members)
	}
	for _, member := range members {
		data := archive[member.Name]
		hash := sha256.Sum256(data)
		if member.Bytes != int64(len(data)) || member.SHA256 != hex.EncodeToString(hash[:]) {
			t.Fatalf("member manifest = %+v", member)
		}
	}
}

func sortedKeys(values map[string][]byte) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPublishWithoutReplacementReportsLinkCollision(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.zip")
	if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalLink := linkPath
	linkPath = func(string, string) error { return fs.ErrExist }
	t.Cleanup(func() { linkPath = originalLink })
	published, err := publishWithoutReplacement(source, filepath.Join(directory, "destination.zip"))
	if published || !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("publish error = %v", err)
	}
}
