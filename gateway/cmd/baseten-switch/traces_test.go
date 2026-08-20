package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracepackage"
)

func traceCommandFixture(t *testing.T) (string, tracecapture.RuntimePaths) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "gateway.yaml")
	raw := []byte(`global:
  routing_enabled: false
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    default_model: example/model
`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", path)
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(root, "missing.pid"))
	paths, err := tracecapture.ResolveRuntimePaths(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, paths
}

func TestTraceEnableDisableEditsOnlyPolicy(t *testing.T) {
	path, _ := traceCommandFixture(t)
	var out bytes.Buffer
	if code := cmdTracesEnable([]string{"--client", "claude-code", "--retention-days", "14"}, &out); code != 0 {
		t.Fatalf("enable exit = %d", code)
	}
	file, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := config.ResolveTraceCapture(file)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Enabled || policy.RetentionDays != 14 || len(policy.Clients) != 1 {
		t.Fatalf("enabled policy = %#v", policy)
	}
	if code := cmdTracesDisable(nil, &out); code != 0 {
		t.Fatalf("disable exit = %d", code)
	}
	file, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err = config.ResolveTraceCapture(file)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Enabled || policy.RetentionDays != 14 || len(policy.Clients) != 1 {
		t.Fatalf("disabled policy = %#v", policy)
	}
}

func TestTraceStatusSeparatesConfiguredPolicyFromRuntimeState(t *testing.T) {
	path, _ := traceCommandFixture(t)
	if err := config.SetTraceCapture(path, config.TraceCapture{
		Enabled: true, Clients: []string{"claude-code"}, RetentionDays: 7,
	}); err != nil {
		t.Fatal(err)
	}
	lastError := "store_locked"
	oldFetch := fetchTraceRuntimeAdminStatus
	fetchTraceRuntimeAdminStatus = func(string) (traceRuntimeAdminStatus, error) {
		return traceRuntimeAdminStatus{
			ConfigPath: path,
			TraceCapture: traceRuntimeProjection{
				Enabled:        false,
				State:          "disabled",
				LastError:      &lastError,
				DroppedRecords: map[string]uint64{"queue_full": 2},
				BodyOmissions:  map[string]uint64{"request_limit": 1},
			},
		}, nil
	}
	t.Cleanup(func() { fetchTraceRuntimeAdminStatus = oldFetch })

	var out bytes.Buffer
	if code := cmdTracesStatus(nil, &out); code != 0 {
		t.Fatalf("status exit = %d, output = %s", code, out.String())
	}
	for _, want := range []string{
		"configured: enabled",
		"capture: disabled",
		"runtime_enabled: false",
		"last_error: store_locked",
		"dropped_records: queue_full=2",
		"body_omissions: request_limit=1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, out.String())
		}
	}
}

func TestTracePackageCommandCreatesLocalZIPWithoutTelemetry(t *testing.T) {
	_, paths := traceCommandFixture(t)
	if err := tracecapture.EnsureRuntimeTraceStore(paths); err != nil {
		t.Fatal(err)
	}
	writer, err := tracecapture.NewWriter(tracecapture.Config{Dir: paths.TraceDir, RetentionDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)
	status := 200
	body := base64.StdEncoding.EncodeToString([]byte(`{"message":"synthetic"}`))
	trace := tracecapture.TraceV1{
		SchemaVersion:     tracecapture.SchemaVersionV1,
		Event:             tracecapture.EventTraceV1,
		EventID:           "00112233445566778899aabbccddeeff",
		StartedAt:         now.Add(-time.Minute),
		CompletedAt:       now.Add(-time.Minute + time.Second),
		Client:            "claude-code",
		ProtocolShape:     tracecapture.ProtocolAnthropic,
		APIKind:           tracecapture.APIKindMessages,
		Endpoint:          "/v1/messages",
		Status:            &status,
		TerminationReason: tracecapture.TerminationCompleted,
		OutcomeSource:     tracecapture.OutcomeSourceProvider,
		ProviderOutcome:   tracecapture.ProviderOutcomeCompleted,
		Request: tracecapture.BodyV1{
			Boundary: "client_ingress", BodyEncoding: "base64", BodyBase64: body,
			ObservedBytes: int64(len(`{"message":"synthetic"}`)), CaptureState: tracecapture.CaptureStateCaptured,
		},
		Response: tracecapture.ResponseBodyV1{
			BodyV1: tracecapture.BodyV1{
				Boundary: "gateway_egress", BodyEncoding: "base64", BodyBase64: body,
				ObservedBytes: int64(len(`{"message":"synthetic"}`)), CaptureState: tracecapture.CaptureStateCaptured,
			},
			GatewayWriteComplete: true,
			ProtocolComplete:     true,
		},
	}
	if result := writer.Enqueue(trace); !result.Accepted {
		t.Fatalf("enqueue = %#v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := writer.Close(ctx); !result.Drained || result.Error != "" {
		t.Fatalf("close = %#v", result)
	}

	oldNow := traceCommandNow
	traceCommandNow = func() time.Time { return now }
	t.Cleanup(func() { traceCommandNow = oldNow })
	var out bytes.Buffer
	if code := cmdTracesPackage([]string{
		"--since", "1h", "--client", "claude-code", "--yes",
	}, bytes.NewReader(nil), &out); code != 0 {
		t.Fatalf("package exit = %d, output = %s", code, out.String())
	}
	destination := filepath.Join(paths.ExportDir, "baseten-switch-traces-20260817T200000Z.zip")
	archive, err := zip.OpenReader(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	seen := map[string]bool{}
	for _, member := range archive.File {
		seen[member.Name] = true
	}
	if !seen["manifest.json"] || !seen["switch/traces.jsonl"] || seen["switch/telemetry.jsonl"] {
		t.Fatalf("members = %#v", seen)
	}

	resolvedDestination, err := filepath.EvalSymlinks(destination)
	if err != nil {
		t.Fatal(err)
	}
	decodedRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	decoded := filepath.Join(decodedRoot, "decoded")
	out.Reset()
	if code := cmdTracesDecode([]string{resolvedDestination, "--output", decoded, "--yes"}, bytes.NewReader(nil), &out); code != 0 {
		t.Fatalf("decode exit = %d, output = %s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(decoded, "index.jsonl")); err != nil {
		t.Fatalf("decoded index: %v", err)
	}
	if strings.Contains(out.String(), "synthetic") {
		t.Fatalf("decode printed body content: %s", out.String())
	}
	if !strings.Contains(out.String(), "warning: decoded files are discoverable plaintext") {
		t.Fatalf("--yes decode omitted preflight sensitivity warning: %s", out.String())
	}
}

func TestTracePackageSummaryWarnsOnNativeSchemaDrift(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	options := tracePackageOptions{
		selection: tracepackage.Selection{
			Since: now.Add(-time.Hour), Until: now, Clients: []string{"claude-code"},
		},
		includeNative: true,
	}
	native := nativePackageData{members: []tracepackage.NativeMember{{
		Name:             "native/claude-code/turns.jsonl",
		Client:           "claude-code",
		Rows:             []json.RawMessage{json.RawMessage(`{}`)},
		CollectionStatus: tracepackage.NativeCollectionPartial,
		SchemaDrift: tracepackage.NativeSchemaDriftV1{
			IgnoredMetadataRecords: 2,
			ExcludedSemanticTurns:  1,
		},
	}}}
	var out bytes.Buffer
	summarizeTracePackage(options, nil, tracecapture.TraceSelectionStats{}, native, &out, true)
	want := "WARNING: native schema drift detected: claude-code status=partial ignored_metadata=2 excluded_turns=1 excluded_sources=0; Switch traces are unaffected."
	if strings.Count(out.String(), want) != 1 {
		t.Fatalf("summary warning = %q", out.String())
	}
}

func TestValidateRequiredNative(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	options := tracePackageOptions{
		requireNative: true,
		selection: tracepackage.Selection{
			Since: now.Add(-time.Hour), Until: now, Clients: []string{"claude-code"},
		},
	}
	traces := []tracecapture.TraceV1{{Client: "claude-code", StartedAt: now.Add(-time.Minute)}}
	complete := tracepackage.NativeMember{
		Client: "claude-code", CollectionStatus: tracepackage.NativeCollectionComplete,
		Rows: []json.RawMessage{json.RawMessage(`{}`)},
	}
	tests := []struct {
		name    string
		native  nativePackageData
		wantErr string
	}{
		{name: "complete", native: nativePackageData{members: []tracepackage.NativeMember{complete}}},
		{name: "partial", native: nativePackageData{members: []tracepackage.NativeMember{{
			Client: "claude-code", CollectionStatus: tracepackage.NativeCollectionPartial,
			Rows: []json.RawMessage{json.RawMessage(`{}`)},
		}}}, wantErr: "is partial"},
		{name: "empty", native: nativePackageData{members: []tracepackage.NativeMember{{
			Client: "claude-code", CollectionStatus: tracepackage.NativeCollectionComplete,
		}}}, wantErr: "produced no normalized turns"},
		{name: "missing", native: nativePackageData{}, wantErr: "is unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRequiredNative(options, traces, test.native)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestTraceDecodeRequiresOutputAndYesWhenNoninteractive(t *testing.T) {
	if _, err := parseTraceDecodeOptions([]string{"package.zip"}); err == nil {
		t.Fatal("missing --output accepted")
	}
	if options, err := parseTraceDecodeOptions([]string{"package.zip", "--output", "decoded", "--yes"}); err != nil || !options.yes {
		t.Fatalf("parse = %#v, %v", options, err)
	}
	if _, err := parseTraceDecodeOptions([]string{"package.zip", "--output", "--yes"}); err == nil {
		t.Fatal("--yes accepted as the output path")
	}
}

func TestReadNativeSessionSelectorRequiresPrivateRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selector.json")
	if err := os.WriteFile(path, []byte(`{"claude-code":["00000000-0000-4000-8000-000000000001"],"codex":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	selector, err := readNativeSessionSelector(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(selector.ClaudeCode) != 1 {
		t.Fatalf("selector = %#v", selector)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeSessionSelector(path); err == nil {
		t.Fatal("selector accepted mode 0644")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "selector-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeSessionSelector(link); err == nil {
		t.Fatal("selector accepted a symlink")
	}
}
