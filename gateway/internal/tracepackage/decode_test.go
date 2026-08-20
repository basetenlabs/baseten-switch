package tracepackage

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func createDecodeFixture(t *testing.T) string {
	t.Helper()
	base := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "source.zip")
	options := basicOptions(destination, base, jsonl(traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour))))
	options.SwitchVersion = "test"
	if _, err := Create(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	return destination
}

func TestDecodeProducesVersionedContentFreeOutput(t *testing.T) {
	source := createDecodeFixture(t)
	root, _ := filepath.EvalSymlinks(t.TempDir())
	output := filepath.Join(root, "decoded")
	result, err := Decode(context.Background(), DecodeOptions{PackagePath: source, OutputDir: output})
	if err != nil {
		t.Fatal(err)
	}
	if result.TraceCount != 1 || result.OutputDir != output {
		t.Fatalf("result = %+v", result)
	}
	for path, want := range map[string]string{
		"traces/00112233445566778899aabbccddeeff/request.json": "{}",
		"traces/00112233445566778899aabbccddeeff/response.sse": "data: {}\n",
	} {
		data, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(path)))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", path, data, err)
		}
	}
	index, err := os.ReadFile(filepath.Join(output, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(index, []byte("body_base64")) || bytes.Contains(index, []byte("data: {}")) {
		t.Fatalf("index contains body content: %s", index)
	}
	for _, want := range []string{`"decode_schema_version":1`, `"archive_id":`, `"sha256":`, `"trace_metadata":`, `"native_turn_id":null`} {
		if !bytes.Contains(index, []byte(want)) {
			t.Fatalf("index missing %s: %s", want, index)
		}
	}
	decodeManifest, err := os.ReadFile(filepath.Join(output, "decode-manifest.json"))
	if err != nil || !bytes.Contains(decodeManifest, []byte(`"decode_schema_version": 1`)) ||
		!bytes.Contains(decodeManifest, []byte(`"source_package_sha256":`)) {
		t.Fatalf("decode manifest = %s, %v", decodeManifest, err)
	}
	var manifest DecodeManifestV1
	if err := json.Unmarshal(decodeManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.DecodeSchemaVersion != 1 || manifest.SourceArchiveID == "" || manifest.SourcePackageSHA256 == "" ||
		!manifest.NoUploadPerformed || manifest.RedactionPerformed || len(manifest.Members) != 4 {
		t.Fatalf("decode manifest = %+v", manifest)
	}
	for _, member := range manifest.Members {
		if member.Name == "decode-manifest.json" || member.Bytes < 0 || len(member.SHA256) != 64 || member.Kind == "" {
			t.Fatalf("decoded member = %+v", member)
		}
	}
	if _, err := os.Stat(filepath.Join(output, ".incomplete")); !os.IsNotExist(err) {
		t.Fatalf("incomplete marker remained: %v", err)
	}
	rootInfo, _ := os.Stat(output)
	fileInfo, _ := os.Stat(filepath.Join(output, "index.jsonl"))
	if rootInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes = %o, %o", rootInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
	if _, err := Decode(context.Background(), DecodeOptions{PackagePath: source, OutputDir: output}); !errorsIs(err, ErrDecodeDestinationExists) {
		t.Fatalf("second decode error = %v", err)
	}
}

func TestInspectDecodeIsAuthoritativeAndPlanIsSingleUse(t *testing.T) {
	source := createDecodeFixture(t)
	plan, err := InspectDecode(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Preflight.TraceCount != 1 || plan.Preflight.CapturedBodyCount != 2 ||
		plan.Preflight.OmittedBodyCount != 0 || plan.Preflight.DecodedBytes != 11 || len(plan.Preflight.PackageSHA256) != 64 {
		t.Fatalf("preflight = %+v", plan.Preflight)
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	output := filepath.Join(root, "decoded")
	if _, err := MaterializeDecode(context.Background(), plan, output); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeDecode(context.Background(), plan, filepath.Join(root, "second")); err == nil {
		t.Fatal("decode plan was reused")
	}
}

func TestInspectDecodeRejectsInsecureSourceMode(t *testing.T) {
	source := createDecodeFixture(t)
	if err := os.Chmod(source, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectDecode(context.Background(), source); err == nil {
		t.Fatal("mode 0644 package accepted")
	}
}

func TestInspectDecodeValidatesNativeSchemaDriftManifest(t *testing.T) {
	base := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	root, _ := filepath.EvalSymlinks(t.TempDir())
	source := filepath.Join(root, "native-drift.zip")
	options := basicOptions(source, base, jsonl(traceJSON(t, selectedEventID, "claude-code", base.Add(time.Hour))))
	options.NativeMembers = []NativeMember{{
		Name: "native/claude-code/turns.jsonl", Client: "claude-code",
		SourceKind: "claude-code-session-jsonl", CollectorVersion: "test-v1",
		CollectionStatus: NativeCollectionUnavailable,
		SchemaDrift:      NativeSchemaDriftV1{ExcludedSources: 1},
	}}
	if _, err := Create(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectDecode(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	members := readZIP(t, source)
	var manifest ManifestV1
	if err := json.Unmarshal(members[ManifestMemberName], &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.NativeCollectors[0].CollectionStatus = NativeCollectionComplete
	members[ManifestMemberName], _ = json.Marshal(manifest)
	tampered := filepath.Join(root, "inconsistent.zip")
	writeDecodeZIP(t, tampered, members)
	if _, err := InspectDecode(context.Background(), tampered); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("InspectDecode() error = %v", err)
	}

	manifest.NativeCollectors[0].CollectionStatus = NativeCollectionUnavailable
	manifest.NativeCollectors[0].SchemaDrift.ExcludedSources = -1
	members[ManifestMemberName], _ = json.Marshal(manifest)
	negative := filepath.Join(root, "negative.zip")
	writeDecodeZIP(t, negative, members)
	if _, err := InspectDecode(context.Background(), negative); err == nil || !strings.Contains(err.Error(), "invalid native schema drift") {
		t.Fatalf("InspectDecode() error = %v", err)
	}
}

func TestDecodeRejectsCompressedTrailingDataAndMapsXML(t *testing.T) {
	compressed := append(gzipBytes(t, []byte("payload")), []byte("trailing")...)
	if _, err := decodeContentEncoding(context.Background(), compressed, "gzip"); err == nil {
		t.Fatal("gzip trailing data accepted")
	}
	for _, value := range []string{"application/xml", "application/problem+xml", "text/xml"} {
		if got := extensionForContentType(value); got != ".txt" {
			t.Fatalf("extension(%q) = %q", value, got)
		}
	}
}

func TestBoundedJSONLReaderRejectsOversizedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rows.jsonl")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1025), 0o600); err != nil {
		t.Fatal(err)
	}
	member := extractedMember{manifest: MemberManifestV1{Name: "test", RecordCount: 1}, path: path}
	if _, err := forEachJSONLine(member, 1024, func([]byte) error { return nil }); err == nil {
		t.Fatal("oversized line accepted")
	}
}

func TestDecodeAcceptsClaudeAndCodexNormalizedBundleIDs(t *testing.T) {
	tests := []struct {
		client, member, sessionID, turnID, matchMode string
	}{
		{"claude-code", "native/claude-code/turns.jsonl", "session_00112233445566778899aabbccddeeff", "turn_112233445566778899aabbccddeeff00", "response_id"},
		{"codex", "native/codex/turns.jsonl", "session_0011223344556677", "turn_1122334455667788", "canonical_request"},
	}
	for _, test := range tests {
		t.Run(test.client, func(t *testing.T) {
			base := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)
			root, _ := filepath.EvalSymlinks(t.TempDir())
			source := filepath.Join(root, "source.zip")
			options := basicOptions(source, base, jsonl(traceJSON(t, selectedEventID, test.client, base.Add(time.Hour))))
			options.Selection.Clients = []string{test.client}
			nativeRow, _ := json.Marshal(map[string]any{
				"native_schema_version": 1, "client": test.client,
				"bundle_session_id": test.sessionID, "bundle_turn_id": test.turnID,
				"switch_event_ids": []string{selectedEventID}, "match_mode": test.matchMode,
				"started_at": base.Add(time.Hour), "completed_at": base.Add(time.Hour + time.Second),
				"status": "completed", "events": []any{},
			})
			options.NativeMembers = []NativeMember{{
				Name: test.member, Client: test.client, SourceKind: test.client + "-session-jsonl",
				Rows: []json.RawMessage{nativeRow}, CollectorVersion: test.client + "-native-v1",
				CorrelationMethods: []string{test.matchMode},
			}}
			options.TraceNativeLinks = map[string]string{selectedEventID: test.turnID}
			if _, err := Create(context.Background(), options); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "decoded")
			if _, err := Decode(context.Background(), DecodeOptions{PackagePath: source, OutputDir: output}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(output, filepath.FromSlash(test.member))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDecodedNativeExplicitSelectionMustMatchManifest(t *testing.T) {
	turns := map[string]nativeTurnLink{
		"turn_0011223344556677": {events: map[string]struct{}{}, matchMode: "explicit_session"},
	}
	if err := validateDecodedNativeLinkage(map[string]string{}, turns, nil); err == nil {
		t.Fatal("unlisted explicit turn accepted")
	}
	if err := validateDecodedNativeLinkage(map[string]string{}, turns, []string{"turn_0011223344556677"}); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeStrictlyRejectsUnknownNativeTurnFields(t *testing.T) {
	tests := []struct {
		client, member, sessionID, turnID, matchMode string
	}{
		{"claude-code", "native/claude-code/turns.jsonl", "session_00112233445566778899aabbccddeeff", "turn_112233445566778899aabbccddeeff00", "response_id"},
		{"codex", "native/codex/turns.jsonl", "session_0011223344556677", "turn_1122334455667788", "canonical_request"},
	}
	for _, test := range tests {
		t.Run(test.client, func(t *testing.T) {
			base := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)
			root, _ := filepath.EvalSymlinks(t.TempDir())
			source := filepath.Join(root, "source.zip")
			options := basicOptions(source, base, jsonl(traceJSON(t, selectedEventID, test.client, base.Add(time.Hour))))
			options.Selection.Clients = []string{test.client}
			nativeRow, _ := json.Marshal(map[string]any{
				"native_schema_version": 1, "client": test.client,
				"bundle_session_id": test.sessionID, "bundle_turn_id": test.turnID,
				"switch_event_ids": []string{selectedEventID}, "match_mode": test.matchMode,
				"started_at": base.Add(time.Hour), "completed_at": base.Add(time.Hour + time.Second),
				"status": "completed", "events": []any{}, "unexpected": true,
			})
			options.NativeMembers = []NativeMember{{
				Name: test.member, Client: test.client, SourceKind: test.client + "-session-jsonl",
				Rows: []json.RawMessage{nativeRow}, CollectorVersion: test.client + "-native-v1",
				CorrelationMethods: []string{test.matchMode},
			}}
			options.TraceNativeLinks = map[string]string{selectedEventID: test.turnID}
			if _, err := Create(context.Background(), options); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectDecode(context.Background(), source); err == nil {
				t.Fatal("native turn with unknown field was accepted")
			}
		})
	}
}

func TestDecodeRejectsUnknownScannerCategory(t *testing.T) {
	source := createDecodeFixture(t)
	members := readZIP(t, source)
	var manifest ManifestV1
	if err := json.Unmarshal(members[ManifestMemberName], &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Scanner.DetectedCategoryCounts = map[string]int{"\x1b[31mcontrol": 1}
	manifest.Scanner.AllowDetectedSecretsUsed = true
	encoded, _ := json.Marshal(manifest)
	members[ManifestMemberName] = append(encoded, '\n')
	root, _ := filepath.EvalSymlinks(t.TempDir())
	tampered := filepath.Join(root, "tampered.zip")
	writeDecodeZIP(t, tampered, members)
	if _, err := InspectDecode(context.Background(), tampered); err == nil {
		t.Fatal("unknown scanner category accepted")
	}
}

func TestDecodeRejectsManifestHashMismatchWithoutOutput(t *testing.T) {
	source := createDecodeFixture(t)
	members := readZIP(t, source)
	var manifest ManifestV1
	if err := json.Unmarshal(members[ManifestMemberName], &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Members[0].SHA256 = strings.Repeat("0", 64)
	encoded, _ := json.Marshal(manifest)
	members[ManifestMemberName] = append(encoded, '\n')
	root, _ := filepath.EvalSymlinks(t.TempDir())
	tampered := filepath.Join(root, "tampered.zip")
	writeDecodeZIP(t, tampered, members)
	outputRoot, _ := filepath.EvalSymlinks(t.TempDir())
	output := filepath.Join(outputRoot, "decoded")
	if _, err := Decode(context.Background(), DecodeOptions{PackagePath: tampered, OutputDir: output}); err == nil {
		t.Fatal("tampered package decoded")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("output was published: %v", err)
	}
}

func TestDecodeRejectsUnsafeZIPMember(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsafe.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	member, _ := writer.Create("../escape")
	_, _ = member.Write([]byte("x"))
	_ = writer.Close()
	_ = file.Close()
	if _, err := InspectDecode(context.Background(), path); err == nil {
		t.Fatal("unsafe member accepted")
	}
}

func writeDecodeZIP(t *testing.T, path string, members map[string][]byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, data := range members {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o600)
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func errorsIs(err, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}
