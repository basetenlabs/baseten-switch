package tracepackage

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	claudenative "github.com/basetenlabs/baseten-switch/gateway/internal/nativecapture/claude"
	codexnative "github.com/basetenlabs/baseten-switch/gateway/internal/nativecapture/codex"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

const maxManifestBytes = 4 << 20

var ErrDecodeDestinationExists = errors.New("trace package decode: destination already exists")

type DecodeOptions struct {
	PackagePath string
	OutputDir   string
	inspectOnly bool
}

type decodedBodyIndexV1 struct {
	Boundary        string                    `json:"boundary"`
	ContentType     string                    `json:"content_type"`
	ContentEncoding string                    `json:"content_encoding"`
	BodyEncoding    string                    `json:"body_encoding"`
	ObservedBytes   int64                     `json:"observed_bytes"`
	CaptureState    tracecapture.CaptureState `json:"capture_state"`
	DecodedPath     *string                   `json:"decoded_path"`
	DecodedBytes    *int64                    `json:"decoded_bytes"`
	DecodedSHA256   *string                   `json:"decoded_sha256"`
}

type packagedTraceEnvelope struct {
	PackageSchemaVersion     int     `json:"package_schema_version"`
	SourceTraceSchemaVersion int     `json:"source_trace_schema_version"`
	NativeTurnID             *string `json:"native_turn_id"`
}

type extractedMember struct {
	manifest MemberManifestV1
	path     string
}

type nativeTurnLink struct {
	events    map[string]struct{}
	matchMode string
}

type nativeTurnEnvelope struct {
	NativeSchemaVersion int
	Client              string
	BundleSessionID     string
	BundleTurnID        string
	SwitchEventIDs      []string
	StartedAt           time.Time
	CompletedAt         time.Time
	Status              string
	MatchMode           string
	Events              []json.RawMessage
}

type decodedBodyTotals struct {
	Count int
	Bytes int64
	Names []string
}

type decodeExecutionResult struct {
	OutputDir       string
	TraceCount      int
	TelemetryCount  int
	NativeTurnCount int
	BodyCount       int
	DecodedBytes    int64
	ArchiveID       string
	PackageSHA256   string
	MemberNames     []string
	Scanner         ScannerManifestV1
}

// Decode validates a Switch trace package, decodes captured HTTP bodies, and
// atomically publishes a private directory. It never modifies PackagePath and
// never replaces OutputDir.
func decodeLegacy(ctx context.Context, options DecodeOptions) (result decodeExecutionResult, returnErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	packagePath, outputDir, err := validateDecodePaths(options)
	if err != nil {
		return result, err
	}
	result.OutputDir = outputDir
	packageFile, packageIdentity, openedPath, err := openSecurePackage(packagePath)
	if err != nil {
		return result, err
	}
	defer packageFile.Close()
	packagePath = openedPath
	sourceHashBefore, err := hashOpenFile(packageFile, packageIdentity.size)
	if err != nil {
		return result, fmt.Errorf("trace package decode: hash source package: %w", err)
	}

	archive, err := zip.NewReader(packageFile, packageIdentity.size)
	if err != nil {
		return result, fmt.Errorf("trace package decode: open package: %w", err)
	}
	files, manifestFile, err := inspectZIP(archive.File)
	if err != nil {
		return result, err
	}
	manifestBytes, err := readZIPMember(manifestFile, maxManifestBytes)
	if err != nil {
		return result, fmt.Errorf("trace package decode: read manifest: %w", err)
	}
	manifest, err := decodeManifestStrict(manifestBytes)
	if err != nil {
		return result, err
	}
	memberManifest, err := validateDecodeManifest(manifest, files)
	if err != nil {
		return result, err
	}

	stageDir, err := os.MkdirTemp(filepath.Dir(outputDir), ".baseten-switch-decode-*")
	if err != nil {
		return result, fmt.Errorf("trace package decode: create staging directory: %w", err)
	}
	if err := os.Chmod(stageDir, 0o700); err != nil {
		_ = os.RemoveAll(stageDir)
		return result, fmt.Errorf("trace package decode: protect staging directory: %w", err)
	}
	defer func() {
		if stageDir != "" {
			returnErr = errors.Join(returnErr, os.RemoveAll(stageDir))
		}
	}()

	rawDir := filepath.Join(stageDir, ".validated-input")
	if err := os.Mkdir(rawDir, 0o700); err != nil {
		return result, fmt.Errorf("trace package decode: create input staging: %w", err)
	}
	extracted := make(map[string]extractedMember, len(memberManifest))
	var totalBytes int64
	for _, file := range files {
		if file.Name == ManifestMemberName {
			continue
		}
		expected := memberManifest[file.Name]
		if expected.Bytes > MaxUncompressedBytes-totalBytes {
			return result, fmt.Errorf("%w: decoded package payload", ErrLimitExceeded)
		}
		totalBytes += expected.Bytes
		nameHash := sha256.Sum256([]byte(file.Name))
		path := filepath.Join(rawDir, hex.EncodeToString(nameHash[:]))
		if err := extractAndVerify(ctx, file, expected, path); err != nil {
			return result, err
		}
		extracted[file.Name] = extractedMember{manifest: expected, path: path}
	}

	traceEvents, err := validateTraceRows(extracted[TraceMemberName])
	if err != nil {
		return result, err
	}
	result.TraceCount = len(traceEvents)
	if member, ok := extracted[TelemetryMemberName]; ok {
		count, err := validateTelemetryRows(member, traceEvents)
		if err != nil {
			return result, err
		}
		result.TelemetryCount = count
	}
	nativeTurns := make(map[string]nativeTurnLink)
	for name, member := range extracted {
		if !strings.HasPrefix(name, "native/") {
			continue
		}
		count, turns, err := validateNativeRows(member)
		if err != nil {
			return result, err
		}
		result.NativeTurnCount += count
		for turnID, events := range turns {
			if _, duplicate := nativeTurns[turnID]; duplicate {
				return result, errors.New("trace package decode: duplicate native bundle_turn_id")
			}
			nativeTurns[turnID] = events
		}
	}
	if err := validateDecodedNativeLinkage(traceEvents, nativeTurns, manifest.OperatorSelectedNativeTurns); err != nil {
		return result, err
	}
	result.ArchiveID = manifest.ArchiveID
	result.PackageSHA256 = sourceHashBefore
	result.Scanner = manifest.Scanner
	decodedFixedBytes := int64(len(manifestBytes))
	for name, member := range extracted {
		if name == TelemetryMemberName || strings.HasPrefix(name, "native/") {
			if member.manifest.Bytes > MaxUncompressedBytes-decodedFixedBytes {
				return result, fmt.Errorf("%w: fixed decoded output", ErrLimitExceeded)
			}
			decodedFixedBytes += member.manifest.Bytes
		}
	}
	if options.inspectOnly {
		bodyBudget := MaxUncompressedBytes - decodedFixedBytes
		bodyTotals, err := decodeTraceRows(ctx, extracted[TraceMemberName].path, "", manifest.ArchiveID, bodyBudget, io.Discard, false)
		if err != nil {
			return result, err
		}
		result.BodyCount = bodyTotals.Count
		result.DecodedBytes = bodyTotals.Bytes
		result.MemberNames = append([]string{"decode-manifest.json", "index.jsonl", "source-manifest.json"}, bodyTotals.Names...)
		for name := range extracted {
			if name == TelemetryMemberName || strings.HasPrefix(name, "native/") {
				result.MemberNames = append(result.MemberNames, name)
			}
		}
		slices.Sort(result.MemberNames)
		if err := verifyOpenPackageUnchanged(packageFile, packageIdentity, packagePath, sourceHashBefore); err != nil {
			return result, err
		}
		return result, nil
	}

	if err := decodeWritePrivateFile(filepath.Join(stageDir, "source-manifest.json"), manifestBytes); err != nil {
		return result, err
	}
	indexPath := filepath.Join(stageDir, "index.jsonl")
	indexFile, err := openPrivateNew(indexPath)
	if err != nil {
		return result, err
	}
	bodyBudget := MaxUncompressedBytes - decodedFixedBytes
	bodyTotals, decodeErr := decodeTraceRows(ctx, extracted[TraceMemberName].path, stageDir, manifest.ArchiveID, bodyBudget, indexFile, true)
	syncErr := indexFile.Sync()
	closeErr := indexFile.Close()
	if decodeErr != nil || syncErr != nil || closeErr != nil {
		return result, errors.Join(decodeErr, syncErr, closeErr)
	}
	for name, member := range extracted {
		if name != TelemetryMemberName && !strings.HasPrefix(name, "native/") {
			continue
		}
		if err := copyPrivateFile(ctx, member.path, filepath.Join(stageDir, filepath.FromSlash(name))); err != nil {
			return result, err
		}
	}
	result.BodyCount = bodyTotals.Count
	result.DecodedBytes = bodyTotals.Bytes
	decodedMembers, outputBytes, err := buildDecodedMemberManifest(stageDir, extracted, result.TraceCount)
	if err != nil {
		return result, err
	}
	decodeManifest := DecodeManifestV1{
		DecodeSchemaVersion:        DecodeSchemaVersionV1,
		CreatedAt:                  time.Now().UTC(),
		DecoderSwitchVersion:       version.Version,
		SourceArchiveID:            manifest.ArchiveID,
		SourcePackageSchemaVersion: manifest.PackageSchemaVersion,
		SourcePackageSHA256:        sourceHashBefore,
		Members:                    decodedMembers,
		NoUploadPerformed:          true,
		RedactionPerformed:         false,
		SensitiveDataNotice:        "Decoded output may contain secrets, personal data, source code, and regulated data. It is not sanitized.",
	}
	decodeManifestBytes, err := json.MarshalIndent(decodeManifest, "", "  ")
	if err != nil {
		return result, fmt.Errorf("trace package decode: encode decode manifest: %w", err)
	}
	decodeManifestBytes = append(decodeManifestBytes, '\n')
	if int64(len(decodeManifestBytes)) > MaxUncompressedBytes-outputBytes {
		return result, fmt.Errorf("%w: total decoded output", ErrLimitExceeded)
	}
	if err := decodeWritePrivateFile(filepath.Join(stageDir, "decode-manifest.json"), decodeManifestBytes); err != nil {
		return result, err
	}
	if err := os.RemoveAll(rawDir); err != nil {
		return result, fmt.Errorf("trace package decode: remove validated input staging: %w", err)
	}
	if err := verifyOpenPackageUnchanged(packageFile, packageIdentity, packagePath, sourceHashBefore); err != nil {
		return result, err
	}
	if err := publishDecodedDirectory(stageDir, outputDir); err != nil {
		return result, err
	}
	stageDir = ""
	return result, nil
}

func verifyOpenPackageUnchanged(file *os.File, identity fileIdentity, path, expectedHash string) error {
	actualHash, err := hashOpenFile(file, identity.size)
	currentInfo, statErr := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || statErr != nil || pathErr != nil || actualHash != expectedHash ||
		!sameIdentity(currentInfo, identity) || !os.SameFile(currentInfo, pathInfo) {
		return errors.New("trace package decode: source package changed during decoding")
	}
	return nil
}

func validateDecodePaths(options DecodeOptions) (string, string, error) {
	if strings.TrimSpace(options.PackagePath) == "" || strings.TrimSpace(options.OutputDir) == "" || options.OutputDir == "-" {
		return "", "", errors.New("trace package decode: package and output directory are required")
	}
	packagePath, err := validateDecodeSource(options.PackagePath)
	if err != nil {
		return "", "", err
	}
	outputDir, err := ValidateDecodeOutputPath(options.OutputDir)
	if err != nil {
		return "", "", err
	}
	return filepath.Clean(packagePath), outputDir, nil
}

// ValidateDecodeOutputPath applies the standalone decoder's no-replace and
// no-symlink path policy without creating the output directory.
func ValidateDecodeOutputPath(value string) (string, error) {
	if strings.TrimSpace(value) == "" || value == "-" || pathHasDotDot(value) {
		return "", errors.New("trace package decode: invalid output path")
	}
	outputDir, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("trace package decode: resolve output directory: %w", err)
	}
	if _, err := os.Lstat(outputDir); err == nil {
		return "", ErrDecodeDestinationExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("trace package decode: inspect output directory: %w", err)
	}
	parent := filepath.Dir(outputDir)
	if err := rejectSymlinkComponents(parent, true); err != nil {
		return "", err
	}
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() {
		return "", errors.New("trace package decode: output parent must be an existing directory")
	}
	return filepath.Clean(outputDir), nil
}

func validateDecodeSource(value string) (string, error) {
	file, _, resolved, err := openSecurePackage(value)
	if err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return resolved, nil
}

func inspectZIP(files []*zip.File) (map[string]*zip.File, *zip.File, error) {
	seen := make(map[string]*zip.File, len(files))
	var manifest *zip.File
	var total uint64
	for _, file := range files {
		if err := ValidateMemberName(file.Name); err != nil {
			return nil, nil, err
		}
		if _, duplicate := seen[file.Name]; duplicate {
			return nil, nil, fmt.Errorf("%w: duplicate %s", ErrUnsafeMember, file.Name)
		}
		if !file.Mode().IsRegular() || file.FileInfo().IsDir() {
			return nil, nil, fmt.Errorf("%w: non-regular member %s", ErrUnsafeMember, file.Name)
		}
		if file.Method != zip.Store && file.Method != zip.Deflate {
			return nil, nil, fmt.Errorf("trace package decode: unsupported ZIP compression method for %s", file.Name)
		}
		if file.Flags&0x1 != 0 {
			return nil, nil, fmt.Errorf("trace package decode: encrypted ZIP member %s is unsupported", file.Name)
		}
		if file.UncompressedSize64 > uint64(MaxUncompressedBytes)-total {
			return nil, nil, fmt.Errorf("%w: uncompressed ZIP content", ErrLimitExceeded)
		}
		total += file.UncompressedSize64
		seen[file.Name] = file
		if file.Name == ManifestMemberName {
			manifest = file
		}
	}
	if manifest == nil {
		return nil, nil, errors.New("trace package decode: manifest.json is required")
	}
	return seen, manifest, nil
}

func readZIPMember(file *zip.File, limit int64) ([]byte, error) {
	if file.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("%w: %s", ErrLimitExceeded, file.Name)
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s", ErrLimitExceeded, file.Name)
	}
	return data, nil
}

func hashOpenFile(file *os.File, size int64) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(file, 0, size)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func decodeManifestStrict(data []byte) (ManifestV1, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest ManifestV1
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("trace package decode: invalid manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return manifest, errors.New("trace package decode: manifest must contain one JSON object")
	}
	return manifest, nil
}

func validateDecodeManifest(manifest ManifestV1, files map[string]*zip.File) (map[string]MemberManifestV1, error) {
	if manifest.PackageSchemaVersion != PackageSchemaVersionV1 {
		return nil, fmt.Errorf("trace package decode: unsupported package schema %d", manifest.PackageSchemaVersion)
	}
	if !manifest.NoUploadPerformed {
		return nil, errors.New("trace package decode: source manifest does not assert local-only packaging")
	}
	for _, version := range manifest.TraceSchemaVersions {
		if version != tracecapture.SchemaVersionV1 {
			return nil, fmt.Errorf("trace package decode: unsupported trace schema %d", version)
		}
	}
	if manifest.Scanner.ScannedBodyCount < 0 || manifest.Scanner.UnscannedBodyCount < 0 ||
		manifest.Scanner.ScannedNativeRecordCount < 0 || manifest.Scanner.UnscannedNativeRecordCount < 0 {
		return nil, errors.New("trace package decode: invalid scanner counts")
	}
	if strings.TrimSpace(manifest.Scanner.Version) == "" || strings.TrimSpace(manifest.SensitiveDataNotice) == "" {
		return nil, errors.New("trace package decode: incomplete scanner or sensitivity metadata")
	}
	unscanned := manifest.Scanner.UnscannedBodyCount + manifest.Scanner.UnscannedNativeRecordCount
	if (unscanned > 0) != manifest.Scanner.AllowUnscannedContentUsed {
		return nil, errors.New("trace package decode: inconsistent unscanned-content override metadata")
	}
	detected := 0
	allowedScannerCategories := make(map[string]struct{}, len(highConfidenceCredentialPatterns))
	for _, pattern := range highConfidenceCredentialPatterns {
		allowedScannerCategories[pattern.category] = struct{}{}
	}
	for category, count := range manifest.Scanner.DetectedCategoryCounts {
		if _, allowed := allowedScannerCategories[category]; !allowed || count < 0 {
			return nil, errors.New("trace package decode: invalid scanner category counts")
		}
		detected += count
	}
	if (detected > 0) != manifest.Scanner.AllowDetectedSecretsUsed {
		return nil, errors.New("trace package decode: inconsistent detected-secret override metadata")
	}
	for reason, count := range manifest.Exclusions {
		if strings.TrimSpace(reason) == "" || count < 0 {
			return nil, errors.New("trace package decode: invalid exclusion counts")
		}
	}
	correlations := make(map[string]struct{}, len(manifest.CorrelationMethods))
	for _, method := range manifest.CorrelationMethods {
		if strings.TrimSpace(method) == "" {
			return nil, errors.New("trace package decode: invalid empty correlation method")
		}
		if _, duplicate := correlations[method]; duplicate {
			return nil, errors.New("trace package decode: duplicate correlation method")
		}
		correlations[method] = struct{}{}
	}
	if !validEventID(manifest.ArchiveID) || manifest.CreatedAt.IsZero() || manifest.Snapshot.StartedAt.IsZero() || manifest.Snapshot.CompletedAt.Before(manifest.Snapshot.StartedAt) {
		return nil, errors.New("trace package decode: invalid manifest metadata")
	}
	selection := Selection{Since: manifest.Selection.Since, Until: manifest.Selection.Until, Clients: manifest.Selection.Clients}
	if err := selection.Validate(); err != nil {
		return nil, fmt.Errorf("trace package decode: invalid manifest selection: %w", err)
	}
	wanted := make(map[string]MemberManifestV1, len(manifest.Members))
	for _, member := range manifest.Members {
		if member.Name != TraceMemberName && member.Name != TelemetryMemberName && member.Name != "native/claude-code/turns.jsonl" && member.Name != "native/codex/turns.jsonl" {
			return nil, fmt.Errorf("trace package decode: unsupported member %q", member.Name)
		}
		if _, duplicate := wanted[member.Name]; duplicate {
			return nil, fmt.Errorf("trace package decode: duplicate manifest member %q", member.Name)
		}
		if member.RecordCount < 0 || member.Bytes < 0 || len(member.SHA256) != 64 {
			return nil, fmt.Errorf("trace package decode: invalid member metadata for %q", member.Name)
		}
		if _, err := hex.DecodeString(member.SHA256); err != nil || strings.ToLower(member.SHA256) != member.SHA256 {
			return nil, fmt.Errorf("trace package decode: invalid member hash for %q", member.Name)
		}
		file, exists := files[member.Name]
		if !exists || file.UncompressedSize64 != uint64(member.Bytes) {
			return nil, fmt.Errorf("trace package decode: member size mismatch for %q", member.Name)
		}
		limit := MaxNormalizedNativeTurns
		if member.Name == TraceMemberName {
			limit = MaxSelectedTraces
		} else if member.Name == TelemetryMemberName {
			limit = MaxSelectedTelemetry
		}
		if member.RecordCount > limit {
			return nil, fmt.Errorf("%w: records in %s", ErrLimitExceeded, member.Name)
		}
		wanted[member.Name] = member
	}
	if _, ok := wanted[TraceMemberName]; !ok {
		return nil, errors.New("trace package decode: switch/traces.jsonl is required")
	}
	traceCount := wanted[TraceMemberName].RecordCount
	if traceCount == 0 && len(manifest.TraceSchemaVersions) != 0 {
		return nil, errors.New("trace package decode: empty trace member must not claim trace schemas")
	}
	if traceCount > 0 && !slices.Contains(manifest.TraceSchemaVersions, tracecapture.SchemaVersionV1) {
		return nil, errors.New("trace package decode: trace schema manifest is incomplete")
	}
	for name := range files {
		if name == ManifestMemberName {
			continue
		}
		if _, expected := wanted[name]; !expected {
			return nil, fmt.Errorf("trace package decode: unknown ZIP member %q", name)
		}
	}
	if len(files) != len(wanted)+1 {
		return nil, errors.New("trace package decode: manifest and ZIP member sets differ")
	}
	collectorMembers := make(map[string]struct{})
	for _, collector := range manifest.NativeCollectors {
		if strings.TrimSpace(collector.Client) == "" || strings.TrimSpace(collector.SourceKind) == "" || strings.TrimSpace(collector.CollectorVersion) == "" {
			return nil, errors.New("trace package decode: incomplete native collector metadata")
		}
		if collector.SchemaDrift.IgnoredMetadataRecords < 0 ||
			collector.SchemaDrift.ExcludedSemanticTurns < 0 ||
			collector.SchemaDrift.ExcludedSources < 0 {
			return nil, errors.New("trace package decode: invalid native schema drift counts")
		}
		normalizedTurns := 0
		if collector.Member != "" {
			member, exists := wanted[collector.Member]
			if !exists {
				return nil, errors.New("trace package decode: native collector member is missing")
			}
			normalizedTurns = member.RecordCount
		}
		if collector.CollectionStatus == "" {
			if collector.SchemaDrift != (NativeSchemaDriftV1{}) {
				return nil, errors.New("trace package decode: native schema drift is missing collection status")
			}
		} else if _, err := resolveNativeCollectionStatus(
			collector.CollectionStatus,
			collector.SchemaDrift,
			normalizedTurns,
		); err != nil {
			return nil, errors.New("trace package decode: inconsistent native collection status")
		}
		if collector.Member == "" {
			continue
		}
		if !strings.HasPrefix(collector.Member, "native/") {
			return nil, errors.New("trace package decode: native collector names a non-native member")
		}
		expectedClient := ""
		switch collector.Member {
		case "native/claude-code/turns.jsonl":
			expectedClient = "claude-code"
		case "native/codex/turns.jsonl":
			expectedClient = "codex"
		}
		if expectedClient != "" && collector.Client != expectedClient {
			return nil, errors.New("trace package decode: native collector client does not match its member")
		}
		if _, duplicate := collectorMembers[collector.Member]; duplicate {
			return nil, errors.New("trace package decode: duplicate native collector member")
		}
		collectorMembers[collector.Member] = struct{}{}
	}
	for name := range wanted {
		if strings.HasPrefix(name, "native/") {
			if _, ok := collectorMembers[name]; !ok {
				return nil, errors.New("trace package decode: native member has no collector metadata")
			}
		}
	}
	return wanted, nil
}

func extractAndVerify(ctx context.Context, file *zip.File, expected MemberManifestV1, destination string) error {
	reader, err := file.Open()
	if err != nil {
		return fmt.Errorf("trace package decode: open %s: %w", file.Name, err)
	}
	defer reader.Close()
	out, err := openPrivateNew(destination)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(contextBoundReader{ctx: ctx, reader: reader}, expected.Bytes+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("trace package decode: extract %s: %w", file.Name, errors.Join(copyErr, closeErr))
	}
	if written != expected.Bytes || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return fmt.Errorf("trace package decode: hash or byte count mismatch for %s", file.Name)
	}
	var extra [1]byte
	if n, err := reader.Read(extra[:]); n != 0 || (err != nil && err != io.EOF) {
		return fmt.Errorf("trace package decode: member exceeds declared size for %s", file.Name)
	}
	return nil
}

func forEachJSONLine(member extractedMember, maxLine int, fn func([]byte) error) (int, error) {
	file, err := os.Open(member.path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, maxLine+1)
	count := 0
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxLine {
			return count, fmt.Errorf("%w: line in %s", ErrLimitExceeded, member.manifest.Name)
		}
		if len(line) > 0 {
			if err == io.EOF || line[len(line)-1] != '\n' {
				return count, fmt.Errorf("trace package decode: incomplete JSONL row in %s", member.manifest.Name)
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				return count, fmt.Errorf("trace package decode: empty JSONL row in %s", member.manifest.Name)
			}
			if err := fn(trimmed); err != nil {
				return count, err
			}
			count++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
	}
	if count != member.manifest.RecordCount {
		return count, fmt.Errorf("trace package decode: record count mismatch for %s", member.manifest.Name)
	}
	return count, nil
}

func decodePackagedTrace(line []byte) (tracecapture.TraceV1, packagedTraceEnvelope, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
		return tracecapture.TraceV1{}, packagedTraceEnvelope{}, errors.New("trace package decode: invalid packaged trace object")
	}
	var envelope packagedTraceEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return tracecapture.TraceV1{}, envelope, fmt.Errorf("trace package decode: invalid packaged trace envelope: %w", err)
	}
	if envelope.PackageSchemaVersion != PackageSchemaVersionV1 || envelope.SourceTraceSchemaVersion != tracecapture.SchemaVersionV1 {
		return tracecapture.TraceV1{}, envelope, errors.New("trace package decode: unsupported packaged trace schema")
	}
	delete(fields, "package_schema_version")
	delete(fields, "source_trace_schema_version")
	delete(fields, "native_turn_id")
	encoded, err := json.Marshal(fields)
	if err != nil {
		return tracecapture.TraceV1{}, envelope, err
	}
	trace, err := tracecapture.DecodeTraceV1Strict(encoded)
	if err != nil {
		return tracecapture.TraceV1{}, envelope, fmt.Errorf("trace package decode: malformed packaged trace projection: %w", err)
	}
	if trace.SchemaVersion != envelope.SourceTraceSchemaVersion {
		return tracecapture.TraceV1{}, envelope, errors.New("trace package decode: trace schema projection mismatch")
	}
	return trace, envelope, nil
}

func validateTraceRows(member extractedMember) (map[string]string, error) {
	events := make(map[string]string, member.manifest.RecordCount)
	_, err := forEachJSONLine(member, MaxTraceLineBytes, func(line []byte) error {
		trace, envelope, err := decodePackagedTrace(line)
		if err != nil {
			return err
		}
		if _, duplicate := events[trace.EventID]; duplicate {
			return errors.New("trace package decode: duplicate trace event_id")
		}
		nativeTurnID := ""
		if envelope.NativeTurnID != nil {
			nativeTurnID = strings.TrimSpace(*envelope.NativeTurnID)
			if nativeTurnID == "" {
				return errors.New("trace package decode: native_turn_id must be null or non-empty")
			}
		}
		events[trace.EventID] = nativeTurnID
		return nil
	})
	return events, err
}

func validateTelemetryRows(member extractedMember, events map[string]string) (int, error) {
	seen := make(map[string]struct{})
	return forEachJSONLine(member, MaxTelemetryLineBytes, func(line []byte) error {
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var event telemetry.EventV1
		if err := decoder.Decode(&event); err != nil || event.Validate() != nil || event.SchemaVersion != telemetry.SchemaVersionV1 || event.Event != telemetry.EventRequest {
			return errors.New("trace package decode: invalid telemetry row")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return errors.New("trace package decode: invalid telemetry row framing")
		}
		if _, ok := events[event.EventID]; !ok {
			return errors.New("trace package decode: telemetry row does not match a trace")
		}
		if _, duplicate := seen[event.EventID]; duplicate {
			return errors.New("trace package decode: duplicate telemetry event_id")
		}
		seen[event.EventID] = struct{}{}
		return nil
	})
}

func validateNativeRows(member extractedMember) (int, map[string]nativeTurnLink, error) {
	expectedClient := ""
	switch member.manifest.Name {
	case "native/claude-code/turns.jsonl":
		expectedClient = "claude-code"
	case "native/codex/turns.jsonl":
		expectedClient = "codex"
	default:
		return 0, nil, errors.New("trace package decode: unsupported normalized native member")
	}
	turns := make(map[string]nativeTurnLink)
	count, err := forEachJSONLine(member, MaxNativeLineBytes, func(line []byte) error {
		link, err := decodeNativeTurnStrict(member.manifest.Name, line)
		if err != nil || link.NativeSchemaVersion != 1 || link.Client != expectedClient ||
			!validBundleID(link.BundleSessionID, "session") || !validBundleID(link.BundleTurnID, "turn") || link.StartedAt.IsZero() ||
			link.CompletedAt.IsZero() || link.CompletedAt.Before(link.StartedAt) || link.Status != "completed" || link.Events == nil ||
			!validNativeMatchMode(expectedClient, link.MatchMode) {
			return errors.New("trace package decode: invalid normalized native turn envelope")
		}
		if _, duplicate := turns[link.BundleTurnID]; duplicate {
			return errors.New("trace package decode: duplicate native bundle_turn_id")
		}
		events := make(map[string]struct{}, len(link.SwitchEventIDs))
		for _, eventID := range link.SwitchEventIDs {
			if !validEventID(eventID) {
				return errors.New("trace package decode: native row has invalid switch_event_id")
			}
			if _, duplicate := events[eventID]; duplicate {
				return errors.New("trace package decode: native row repeats switch_event_id")
			}
			events[eventID] = struct{}{}
		}
		if link.MatchMode == "explicit_session" && len(events) != 0 {
			return errors.New("trace package decode: explicit native turn must not claim Switch links")
		}
		if link.MatchMode != "explicit_session" && len(events) == 0 {
			return errors.New("trace package decode: correlated native turn must claim a Switch link")
		}
		turns[link.BundleTurnID] = nativeTurnLink{events: events, matchMode: link.MatchMode}
		return nil
	})
	return count, turns, err
}

func decodeNativeTurnStrict(memberName string, line []byte) (nativeTurnEnvelope, error) {
	var result nativeTurnEnvelope
	decode := func(target any) error {
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return err
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return errors.New("native row must contain one JSON object")
		}
		return nil
	}
	switch memberName {
	case "native/claude-code/turns.jsonl":
		var turn claudenative.NativeTurnV1
		if err := decode(&turn); err != nil {
			return result, errors.New("trace package decode: invalid Claude native turn")
		}
		var events []json.RawMessage
		if turn.Events != nil {
			events = make([]json.RawMessage, len(turn.Events))
			for index, event := range turn.Events {
				encoded, err := json.Marshal(event)
				if err != nil {
					return result, err
				}
				events[index] = encoded
			}
		}
		result = nativeTurnEnvelope{turn.NativeSchemaVersion, turn.Client, turn.BundleSessionID, turn.BundleTurnID, turn.SwitchEventIDs, turn.StartedAt, turn.CompletedAt, turn.Status, turn.MatchMode, events}
	case "native/codex/turns.jsonl":
		var turn codexnative.NativeTurnV1
		if err := decode(&turn); err != nil {
			return result, errors.New("trace package decode: invalid Codex native turn")
		}
		var events []json.RawMessage
		if turn.Events != nil {
			events = make([]json.RawMessage, len(turn.Events))
			for index, event := range turn.Events {
				encoded, err := json.Marshal(event)
				if err != nil {
					return result, err
				}
				events[index] = encoded
			}
		}
		result = nativeTurnEnvelope{turn.NativeSchemaVersion, turn.Client, turn.BundleSessionID, turn.BundleTurnID, turn.SwitchEventIDs, turn.StartedAt, turn.CompletedAt, turn.Status, turn.MatchMode, events}
	default:
		return result, errors.New("trace package decode: unsupported normalized native member")
	}
	return result, nil
}

func validateDecodedNativeLinkage(traces map[string]string, turns map[string]nativeTurnLink, operatorSelected []string) error {
	for eventID, turnID := range traces {
		if turnID == "" {
			continue
		}
		turn, ok := turns[turnID]
		if !ok {
			return errors.New("trace package decode: trace links to a missing native turn")
		}
		if _, linkedBack := turn.events[eventID]; !linkedBack {
			return errors.New("trace package decode: native linkage is not bidirectional")
		}
	}
	for turnID, turn := range turns {
		for eventID := range turn.events {
			if traces[eventID] != turnID {
				return errors.New("trace package decode: native linkage is not bidirectional")
			}
		}
	}
	selected := make(map[string]struct{}, len(operatorSelected))
	for _, turnID := range operatorSelected {
		if strings.TrimSpace(turnID) == "" {
			return errors.New("trace package decode: empty operator-selected native turn")
		}
		if _, duplicate := selected[turnID]; duplicate {
			return errors.New("trace package decode: duplicate operator-selected native turn")
		}
		turn, exists := turns[turnID]
		if !exists {
			return errors.New("trace package decode: operator-selected native turn is missing")
		}
		if turn.matchMode != "explicit_session" || len(turn.events) != 0 {
			return errors.New("trace package decode: operator-selected turn is not an explicit unlinked turn")
		}
		selected[turnID] = struct{}{}
	}
	for turnID, turn := range turns {
		_, listed := selected[turnID]
		if (turn.matchMode == "explicit_session") != listed {
			return errors.New("trace package decode: operator-selected native turn list is inconsistent")
		}
	}
	return nil
}

func validBundleID(value, kind string) bool {
	prefix := kind + "_"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	digest := strings.TrimPrefix(value, prefix)
	if len(digest) != 16 && len(digest) != 32 {
		return false
	}
	if digest != strings.ToLower(digest) {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded)*2 == len(digest)
}

func validNativeMatchMode(client, value string) bool {
	switch client {
	case "claude-code":
		return value == "response_id" || value == "explicit_session"
	case "codex":
		return value == "canonical_request" || value == "turn_hash" || value == "explicit_session"
	default:
		return false
	}
}

func decodeTraceRows(ctx context.Context, source, stageDir, archiveID string, bodyBudget int64, index io.Writer, materialize bool) (decodedBodyTotals, error) {
	var totals decodedBodyTotals
	member := extractedMember{manifest: MemberManifestV1{Name: TraceMemberName}, path: source}
	_, err := forEachJSONLineUnchecked(member, MaxTraceLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		trace, _, err := decodePackagedTrace(line)
		if err != nil {
			return err
		}
		request, err := decodeOneBody(ctx, stageDir, trace.EventID, "request", trace.Request, materialize)
		if err != nil {
			return err
		}
		responseBody, err := decodeOneBody(ctx, stageDir, trace.EventID, "response", trace.Response.BodyV1, materialize)
		if err != nil {
			return err
		}
		entry, err := buildDecodedTraceIndex(line, archiveID, trace.EventID, request, responseBody)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		encoded = append(encoded, '\n')
		if _, err := index.Write(encoded); err != nil {
			return fmt.Errorf("trace package decode: write index: %w", err)
		}
		for _, body := range []decodedBodyIndexV1{request, responseBody} {
			if body.DecodedBytes != nil {
				if *body.DecodedBytes > bodyBudget-totals.Bytes {
					return fmt.Errorf("%w: aggregate decoded bodies", ErrLimitExceeded)
				}
				totals.Count++
				totals.Bytes += *body.DecodedBytes
				totals.Names = append(totals.Names, *body.DecodedPath)
			}
		}
		return nil
	})
	return totals, err
}

func buildDecodedTraceIndex(line []byte, archiveID, eventID string, request, response decodedBodyIndexV1) (DecodedTraceIndexV1, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(line, &top); err != nil || top == nil {
		return DecodedTraceIndexV1{}, errors.New("trace package decode: invalid packaged trace fields")
	}
	requestRaw, ok := top["request"]
	if !ok {
		return DecodedTraceIndexV1{}, errors.New("trace package decode: packaged trace is missing request")
	}
	responseRaw, ok := top["response"]
	if !ok {
		return DecodedTraceIndexV1{}, errors.New("trace package decode: packaged trace is missing response")
	}
	delete(top, "request")
	delete(top, "response")
	requestFields, err := buildDecodedBoundaryIndex(requestRaw, request)
	if err != nil {
		return DecodedTraceIndexV1{}, err
	}
	responseFields, err := buildDecodedBoundaryIndex(responseRaw, response)
	if err != nil {
		return DecodedTraceIndexV1{}, err
	}
	return DecodedTraceIndexV1{
		DecodeSchemaVersion: DecodeSchemaVersionV1,
		ArchiveID:           archiveID, EventID: eventID, TraceMetadata: top,
		Request: requestFields, Response: responseFields,
	}, nil
}

func buildDecodedBoundaryIndex(raw json.RawMessage, decoded decodedBodyIndexV1) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("trace package decode: invalid packaged trace body")
	}
	for _, reserved := range []string{"source_content_encoding", "source_body_encoding", "materialization", "path", "decoded_bytes", "sha256"} {
		if _, exists := fields[reserved]; exists {
			return nil, fmt.Errorf("trace package decode: body contains reserved decoder field %q", reserved)
		}
	}
	delete(fields, "body_base64")
	contentEncoding := fields["content_encoding"]
	delete(fields, "content_encoding")
	fields["source_content_encoding"] = contentEncoding
	bodyEncoding := fields["body_encoding"]
	delete(fields, "body_encoding")
	fields["source_body_encoding"] = bodyEncoding
	materialization := "not_captured"
	if decoded.DecodedPath != nil {
		encoding := strings.ToLower(strings.TrimSpace(decoded.ContentEncoding))
		materialization = "base64_decoded"
		if encoding != "" && encoding != "identity" {
			materialization = "decoded_and_decompressed"
		}
		fields["path"] = mustJSONRaw(*decoded.DecodedPath)
		fields["decoded_bytes"] = mustJSONRaw(*decoded.DecodedBytes)
		fields["sha256"] = mustJSONRaw(*decoded.DecodedSHA256)
	}
	fields["materialization"] = mustJSONRaw(materialization)
	return fields, nil
}

func mustJSONRaw(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func forEachJSONLineUnchecked(member extractedMember, maxLine int, fn func([]byte) error) (int, error) {
	member.manifest.RecordCount = -1
	file, err := os.Open(member.path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, maxLine+1)
	count := 0
	for {
		line, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) || len(line) > maxLine {
			return count, fmt.Errorf("%w: trace row", ErrLimitExceeded)
		}
		if len(line) > 0 {
			if readErr == io.EOF || line[len(line)-1] != '\n' {
				return count, errors.New("trace package decode: incomplete trace row")
			}
			if err := fn(bytes.TrimSpace(line)); err != nil {
				return count, err
			}
			count++
		}
		if readErr == io.EOF {
			return count, nil
		}
		if readErr != nil {
			return count, readErr
		}
	}
}

func decodeOneBody(ctx context.Context, stageDir, eventID, boundary string, body tracecapture.BodyV1, materialize bool) (decodedBodyIndexV1, error) {
	entry := decodedBodyIndexV1{
		Boundary: body.Boundary, ContentType: body.ContentType, ContentEncoding: body.ContentEncoding,
		BodyEncoding: body.BodyEncoding, ObservedBytes: body.ObservedBytes, CaptureState: body.CaptureState,
	}
	if body.CaptureState != tracecapture.CaptureStateCaptured {
		return entry, nil
	}
	if strings.ContainsAny(body.BodyBase64, "\r\n") {
		return entry, errors.New("trace package decode: captured base64 body is not canonical")
	}
	raw, err := base64.StdEncoding.DecodeString(body.BodyBase64)
	if err != nil {
		return entry, errors.New("trace package decode: invalid captured base64 body")
	}
	if base64.StdEncoding.EncodeToString(raw) != body.BodyBase64 {
		return entry, errors.New("trace package decode: captured base64 body is not canonical")
	}
	decoded, err := decodeContentEncoding(ctx, raw, body.ContentEncoding)
	if err != nil {
		return entry, err
	}
	relative := filepath.ToSlash(filepath.Join("traces", eventID, boundary+extensionForContentType(body.ContentType)))
	if materialize {
		if err := decodeWritePrivateFile(filepath.Join(stageDir, filepath.FromSlash(relative)), decoded); err != nil {
			return entry, err
		}
	}
	entry.DecodedPath = &relative
	decodedBytes := int64(len(decoded))
	decodedHash := sha256.Sum256(decoded)
	decodedSHA256 := hex.EncodeToString(decodedHash[:])
	entry.DecodedBytes = &decodedBytes
	entry.DecodedSHA256 = &decodedSHA256
	return entry, nil
}

func decodeContentEncoding(ctx context.Context, raw []byte, value string) ([]byte, error) {
	encoding := strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(encoding, ",") {
		return nil, errors.New("trace package decode: multiple content encodings are unsupported")
	}
	if encoding == "" || encoding == "identity" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return slices.Clone(raw), nil
	}
	var reader io.ReadCloser
	var compressed *bytes.Reader
	var err error
	switch encoding {
	case "gzip", "x-gzip":
		compressed = bytes.NewReader(raw)
		var gzipReader *gzip.Reader
		gzipReader, err = gzip.NewReader(compressed)
		if err == nil {
			gzipReader.Multistream(false)
			reader = gzipReader
		}
	case "deflate":
		compressed = bytes.NewReader(raw)
		reader, err = zlib.NewReader(compressed)
		if err != nil {
			compressed = bytes.NewReader(raw)
			reader = flate.NewReader(compressed)
			err = nil
		}
	default:
		return nil, errors.New("trace package decode: unsupported content encoding")
	}
	if err != nil {
		return nil, fmt.Errorf("trace package decode: decompress %s body: %w", encoding, err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(contextBoundReader{ctx: ctx, reader: reader}, MaxDecodedBodyScanBytes+1))
	if err != nil {
		return nil, fmt.Errorf("trace package decode: decompress %s body: %w", encoding, err)
	}
	if int64(len(decoded)) > MaxDecodedBodyScanBytes {
		return nil, fmt.Errorf("%w: decoded body", ErrLimitExceeded)
	}
	if compressed != nil && compressed.Len() != 0 {
		return nil, errors.New("trace package decode: compressed body has trailing or concatenated data")
	}
	return decoded, nil
}

func extensionForContentType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ".bin"
	}
	mediaType = strings.ToLower(mediaType)
	switch {
	case mediaType == "application/json", strings.HasSuffix(mediaType, "+json"):
		return ".json"
	case mediaType == "text/event-stream":
		return ".sse"
	case strings.HasPrefix(mediaType, "text/"), mediaType == "application/xml", strings.HasSuffix(mediaType, "+xml"):
		return ".txt"
	default:
		return ".bin"
	}
}

func openPrivateNew(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("trace package decode: create private directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("trace package decode: create private file: %w", err)
	}
	return file, nil
}

func decodeWritePrivateFile(path string, data []byte) error {
	file, err := openPrivateNew(path)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return fmt.Errorf("trace package decode: write private file: %w", errors.Join(writeErr, syncErr, closeErr))
	}
	return nil
}

func copyPrivateFile(ctx context.Context, source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := openPrivateNew(destination)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, contextBoundReader{ctx: ctx, reader: in})
	syncErr := out.Sync()
	closeErr := out.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

type contextBoundReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextBoundReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func buildDecodedMemberManifest(stageDir string, extracted map[string]extractedMember, traceCount int) ([]DecodedMemberManifestV1, int64, error) {
	var members []DecodedMemberManifestV1
	var total int64
	err := filepath.WalkDir(stageDir, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == stageDir {
			return nil
		}
		relative, err := filepath.Rel(stageDir, filePath)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if name == ".validated-input" || strings.HasPrefix(name, ".validated-input/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("trace package decode: non-regular staged output")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		hashValue, err := hashFile(filePath)
		if err != nil {
			return err
		}
		member := DecodedMemberManifestV1{Name: name, Bytes: info.Size(), SHA256: hashValue}
		switch name {
		case "source-manifest.json":
			member.Kind = DecodedMemberSourceManifest
			member.SourceMember = ManifestMemberName
		case "index.jsonl":
			member.Kind = DecodedMemberIndex
			count := traceCount
			member.RecordCount = &count
		case TelemetryMemberName:
			member.Kind = DecodedMemberTelemetry
			member.SourceMember = TelemetryMemberName
			count := extracted[TelemetryMemberName].manifest.RecordCount
			member.RecordCount = &count
		default:
			if strings.HasPrefix(name, "native/") {
				member.Kind = DecodedMemberNative
				member.SourceMember = name
				count := extracted[name].manifest.RecordCount
				member.RecordCount = &count
			} else if strings.HasPrefix(name, "traces/") {
				parts := strings.Split(name, "/")
				if len(parts) != 3 || !validEventID(parts[1]) {
					return errors.New("trace package decode: invalid decoded body path")
				}
				member.EventID = parts[1]
				boundary := strings.TrimSuffix(parts[2], filepath.Ext(parts[2]))
				switch boundary {
				case "request":
					member.Kind = DecodedMemberRequestBody
					member.Boundary = "client_ingress"
				case "response":
					member.Kind = DecodedMemberResponseBody
					member.Boundary = "gateway_egress"
				default:
					return errors.New("trace package decode: invalid decoded body boundary")
				}
			} else {
				return errors.New("trace package decode: unknown staged output")
			}
		}
		if member.Kind == "" {
			return errors.New("trace package decode: missing decoded member kind")
		}
		if member.Bytes > MaxUncompressedBytes-total {
			return fmt.Errorf("%w: total decoded output", ErrLimitExceeded)
		}
		total += member.Bytes
		members = append(members, member)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	slices.SortFunc(members, func(left, right DecodedMemberManifestV1) int {
		return strings.Compare(left.Name, right.Name)
	})
	return members, total, nil
}

func publishDecodedDirectory(stageDir, outputDir string) error {
	// Portable Go does not expose a no-replace directory rename. Reserve the
	// destination atomically, mark it incomplete, move same-filesystem entries,
	// then remove the marker as the publication boundary.
	if err := os.Mkdir(outputDir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrDecodeDestinationExists
		}
		return fmt.Errorf("trace package decode: reserve output directory: %w", err)
	}
	reservedInfo, err := os.Lstat(outputDir)
	if err != nil {
		return err
	}
	reservedIdentity, err := identityFromFileInfo(reservedInfo)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			if current, err := os.Lstat(outputDir); err == nil && sameObjectIdentity(current, reservedIdentity) {
				_ = os.RemoveAll(outputDir)
			}
		}
	}()
	if err := syncDirectory(filepath.Dir(outputDir)); err != nil {
		return fmt.Errorf("trace package decode: sync output parent after reservation: %w", err)
	}
	marker := filepath.Join(outputDir, ".incomplete")
	if err := decodeWritePrivateFile(marker, []byte("incomplete\n")); err != nil {
		return err
	}
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		return fmt.Errorf("trace package decode: list staged output: %w", err)
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(stageDir, entry.Name()), filepath.Join(outputDir, entry.Name())); err != nil {
			return fmt.Errorf("trace package decode: publish %s: %w", entry.Name(), err)
		}
	}
	if err := syncDecodedTree(outputDir); err != nil {
		return err
	}
	if err := os.Remove(marker); err != nil {
		return fmt.Errorf("trace package decode: complete publication: %w", err)
	}
	if err := syncDirectory(outputDir); err != nil {
		return fmt.Errorf("trace package decode: sync completed output: %w", err)
	}
	if err := syncDirectory(filepath.Dir(outputDir)); err != nil {
		return fmt.Errorf("trace package decode: sync output parent: %w", err)
	}
	if err := os.Remove(stageDir); err != nil {
		return fmt.Errorf("trace package decode: remove empty staging directory: %w", err)
	}
	cleanup = false
	return nil
}

func syncDecodedTree(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		return errors.Join(syncErr, closeErr)
	})
	if err != nil {
		return fmt.Errorf("trace package decode: sync output files: %w", err)
	}
	slices.Reverse(directories)
	for _, directory := range directories {
		if err := syncDirectory(directory); err != nil {
			return fmt.Errorf("trace package decode: sync output directory: %w", err)
		}
	}
	return nil
}
