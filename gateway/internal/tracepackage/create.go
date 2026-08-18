package tracepackage

import (
	"archive/zip"
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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

var (
	removeAllPath = os.RemoveAll
	linkPath      = os.Link
)

type boundedZIPWriter struct {
	destination io.Writer
	remaining   int64
}

func (w *boundedZIPWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, fmt.Errorf("%w: final ZIP", ErrLimitExceeded)
	}
	if int64(len(p)) > w.remaining {
		written, err := w.destination.Write(p[:w.remaining])
		w.remaining -= int64(written)
		if err != nil {
			return written, err
		}
		return written, fmt.Errorf("%w: final ZIP", ErrLimitExceeded)
	}
	written, err := w.destination.Write(p)
	w.remaining -= int64(written)
	return written, err
}

type stagedMember struct {
	Name        string
	Path        string
	RecordCount int
	Bytes       int64
	SHA256      string
}

type scannerAccumulator struct {
	version                string
	scannedBodies          int
	unscannedBodies        int
	scannedNativeRecords   int
	unscannedNativeRecords int
	detectedCategoryCounts map[string]int
}

// Create writes and publishes a Switch-only trace package. It never performs
// network I/O and never overwrites Destination.
func Create(ctx context.Context, options Options) (result Result, returnErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := options.Selection.Validate(); err != nil {
		return result, err
	}
	if err := validateNativeLinkage(options.NativeMembers, options.TraceNativeLinks, options.OperatorSelectedNativeTurns); err != nil {
		return result, err
	}
	if options.Sources.Traces == nil {
		return result, errors.New("trace package: trace source is required")
	}
	destination, err := validateDestination(options.Destination)
	if err != nil {
		return result, err
	}
	options.Destination = destination
	options.Selection = options.Selection.normalized()
	now := options.Now
	if now == nil {
		now = time.Now
	}
	commandStartedAt := now().UTC()
	if !options.Selection.Since.Before(commandStartedAt) {
		return result, fmt.Errorf("%w: interval is future-only", ErrInvalidSelection)
	}

	result.Destination = destination
	archiveID, err := randomHex(16)
	if err != nil {
		return result, fmt.Errorf("generate archive ID: %w", err)
	}
	result.ArchiveID = archiveID

	stageDir, err := os.MkdirTemp(filepath.Dir(destination), ".baseten-switch-package-*")
	if err != nil {
		return result, fmt.Errorf("create private package staging directory: %w", err)
	}
	if err := os.Chmod(stageDir, 0o700); err != nil {
		_ = removeAllPath(stageDir)
		return result, fmt.Errorf("set package staging permissions: %w", err)
	}
	defer func() {
		cleanupErr := removeAllPath(stageDir)
		if cleanupErr == nil || errors.Is(cleanupErr, os.ErrNotExist) {
			return
		}
		cleanup := &CleanupError{
			Published:    result.Published,
			RecoveryRoot: filepath.Dir(stageDir),
			Err:          cleanupErr,
		}
		if options.Quarantine != nil {
			cleanupID, quarantineErr := options.Quarantine(ctx, stageDir)
			if quarantineErr == nil && strings.TrimSpace(cleanupID) != "" {
				cleanup.CleanupID = cleanupID
				cleanup.RecoveryRoot = ""
				result.CleanupID = cleanupID
			} else if quarantineErr != nil {
				cleanup.Err = errors.Join(cleanupErr, quarantineErr)
			}
		}
		returnErr = errors.Join(returnErr, cleanup)
	}()

	snapshotStartedAt := commandStartedAt
	traceSnapshot, err := options.Sources.Traces(ctx, options.Selection)
	if err != nil {
		return result, fmt.Errorf("snapshot traces: %w", err)
	}
	if err := traceSnapshot.validate("trace"); err != nil {
		return result, err
	}

	scanner := scannerAccumulator{
		version:                strings.TrimSpace(options.Scanner.Version),
		detectedCategoryCounts: make(map[string]int),
	}
	traceMember, eventIDs, traceSchemaVersions, err := stageTraces(
		ctx,
		stageDir,
		options.Selection,
		traceSnapshot,
		options.Scanner,
		&scanner,
		options.TraceNativeLinks,
	)
	if err != nil {
		return result, err
	}
	result.TraceCount = traceMember.RecordCount

	members := []stagedMember{traceMember}
	exclusions := make(map[string]int)
	correlations := make([]string, 0, 1)
	nativeCollectors := make([]NativeCollectorManifestV1, 0, len(options.NativeMembers))
	if options.Sources.Telemetry != nil && len(eventIDs) > 0 {
		telemetrySnapshot, err := options.Sources.Telemetry(
			ctx,
			options.Selection,
			slices.Clone(eventIDs),
		)
		if err != nil {
			return result, fmt.Errorf("snapshot telemetry: %w", err)
		}
		if err := telemetrySnapshot.validate("telemetry"); err != nil {
			return result, err
		}
		telemetryMember, missing, err := stageTelemetry(
			ctx,
			stageDir,
			telemetrySnapshot,
			eventIDs,
			&scanner,
		)
		if err != nil {
			return result, err
		}
		result.TelemetryCount = telemetryMember.RecordCount
		if telemetryMember.RecordCount > 0 {
			members = append(members, telemetryMember)
			correlations = append(correlations, "telemetry_event_id")
		}
		if missing > 0 {
			exclusions["telemetry_unmatched"] = missing
		}
	} else if len(eventIDs) > 0 {
		exclusions["telemetry_unavailable"] = len(eventIDs)
	}

	for _, native := range options.NativeMembers {
		if err := validateNativeManifestMetadata(native); err != nil {
			return result, err
		}
		scanPlainMetadata([]byte(strings.Join(append(append([]string{
			native.Client, native.SourceKind, native.CollectorVersion,
		}, native.ClientVersions...), native.CorrelationMethods...), "\n")), &scanner)
		for reason := range native.Exclusions {
			scanPlainMetadata([]byte(reason), &scanner)
		}
		member, err := stageNativeMember(ctx, stageDir, native, &scanner)
		if err != nil {
			return result, err
		}
		for reason, count := range native.Exclusions {
			exclusions[reason] += count
		}
		collectorManifest := NativeCollectorManifestV1{
			Client: strings.TrimSpace(native.Client), SourceKind: strings.TrimSpace(native.SourceKind),
			CollectorVersion: strings.TrimSpace(native.CollectorVersion),
			ClientVersions:   slices.Clone(native.ClientVersions), CorrelationMethods: slices.Clone(native.CorrelationMethods),
		}
		if member.RecordCount == 0 {
			nativeCollectors = append(nativeCollectors, collectorManifest)
			continue
		}
		if result.NativeTurnCount > MaxNormalizedNativeTurns-member.RecordCount {
			return result, fmt.Errorf("%w: normalized native turns", ErrLimitExceeded)
		}
		result.NativeTurnCount += member.RecordCount
		members = append(members, member)
		correlations = append(correlations, native.CorrelationMethods...)
		collectorManifest.Member = member.Name
		nativeCollectors = append(nativeCollectors, collectorManifest)
	}
	scanPlainMetadata([]byte(options.SwitchVersion), &scanner)

	if scanner.unscannedBodies+scanner.unscannedNativeRecords > 0 && !options.AllowUnscannedContent {
		return result, &ContentScanError{
			Err:            ErrUnscannedContent,
			UnscannedCount: scanner.unscannedBodies + scanner.unscannedNativeRecords,
			CategoryCounts: cloneIntCounts(scanner.detectedCategoryCounts),
		}
	}
	if detectedCount(scanner.detectedCategoryCounts) > 0 && !options.AllowDetectedSecrets {
		return result, &ContentScanError{
			Err:            ErrDetectedSecrets,
			UnscannedCount: scanner.unscannedBodies + scanner.unscannedNativeRecords,
			CategoryCounts: cloneIntCounts(scanner.detectedCategoryCounts),
		}
	}

	snapshotCompletedAt := now().UTC()
	manifest := ManifestV1{
		PackageSchemaVersion: PackageSchemaVersionV1,
		ArchiveID:            archiveID,
		CreatedAt:            snapshotCompletedAt,
		SwitchVersion:        strings.TrimSpace(options.SwitchVersion),
		TraceSchemaVersions:  traceSchemaVersions,
		Selection: SelectionManifestV1{
			Since:   options.Selection.Since,
			Until:   options.Selection.Until,
			Clients: slices.Clone(options.Selection.Clients),
		},
		Snapshot: SnapshotManifestV1{
			StartedAt:   snapshotStartedAt,
			CompletedAt: snapshotCompletedAt,
		},
		CorrelationMethods:          slices.Clone(correlations),
		NativeCollectors:            nativeCollectors,
		OperatorSelectedNativeTurns: slices.Clone(options.OperatorSelectedNativeTurns),
		Scanner: ScannerManifestV1{
			Version:                    scanner.version,
			ScannedBodyCount:           scanner.scannedBodies,
			UnscannedBodyCount:         scanner.unscannedBodies,
			ScannedNativeRecordCount:   scanner.scannedNativeRecords,
			UnscannedNativeRecordCount: scanner.unscannedNativeRecords,
			DetectedCategoryCounts:     scanner.detectedCategoryCounts,
			AllowUnscannedContentUsed:  scanner.unscannedBodies+scanner.unscannedNativeRecords > 0 && options.AllowUnscannedContent,
			AllowDetectedSecretsUsed:   detectedCount(scanner.detectedCategoryCounts) > 0 && options.AllowDetectedSecrets,
		},
		Exclusions:          exclusions,
		NoUploadPerformed:   true,
		SensitiveDataNotice: "This package may contain secrets, personal data, source code, and regulated data. It is not sanitized.",
	}
	for _, member := range members {
		manifest.Members = append(manifest.Members, MemberManifestV1{
			Name:        member.Name,
			RecordCount: member.RecordCount,
			Bytes:       member.Bytes,
			SHA256:      member.SHA256,
		})
	}
	manifestMember, err := stageManifest(stageDir, manifest)
	if err != nil {
		return result, err
	}

	var uncompressed int64
	for _, member := range members {
		if member.Bytes > MaxUncompressedBytes-uncompressed {
			return result, fmt.Errorf("%w: uncompressed package payload", ErrLimitExceeded)
		}
		uncompressed += member.Bytes
	}
	if manifestMember.Bytes > MaxUncompressedBytes-uncompressed {
		return result, fmt.Errorf("%w: uncompressed package payload", ErrLimitExceeded)
	}

	zipPath, zipBytes, zipSHA256, err := buildZIP(
		stageDir,
		manifestMember,
		members,
	)
	if err != nil {
		return result, err
	}
	result.ArchiveBytes = zipBytes
	result.ArchiveSHA256 = zipSHA256
	published, err := publishWithoutReplacement(zipPath, destination)
	result.Published = published
	if err != nil {
		return result, err
	}
	return result, nil
}

func cloneIntCounts(source map[string]int) map[string]int {
	result := make(map[string]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func validateNativeLinkage(members []NativeMember, links map[string]string, operatorSelected []string) error {
	type linkFields struct {
		BundleTurnID   string   `json:"bundle_turn_id"`
		SwitchEventIDs []string `json:"switch_event_ids"`
		MatchMode      string   `json:"match_mode"`
	}
	turnEvents := make(map[string]map[string]struct{})
	explicitTurns := make(map[string]struct{})
	var retainedBytes int64
	for _, member := range members {
		for _, row := range member.Rows {
			if int64(len(row)) > MaxRetainedNativeBytes-retainedBytes {
				return fmt.Errorf("%w: retained normalized native rows", ErrLimitExceeded)
			}
			retainedBytes += int64(len(row))
			var fields linkFields
			if json.Unmarshal(row, &fields) != nil || strings.TrimSpace(fields.BundleTurnID) == "" {
				return errors.New("trace package: native row has invalid bundle_turn_id")
			}
			if _, duplicate := turnEvents[fields.BundleTurnID]; duplicate {
				return errors.New("trace package: duplicate native bundle_turn_id")
			}
			events := make(map[string]struct{}, len(fields.SwitchEventIDs))
			for _, eventID := range fields.SwitchEventIDs {
				if !validEventID(eventID) {
					return errors.New("trace package: native row has invalid switch_event_id")
				}
				if _, duplicate := events[eventID]; duplicate {
					return errors.New("trace package: native row repeats a switch_event_id")
				}
				events[eventID] = struct{}{}
			}
			turnEvents[fields.BundleTurnID] = events
			if fields.MatchMode == "explicit_session" {
				if len(events) != 0 {
					return errors.New("trace package: operator-selected native turn must not claim a Switch link")
				}
				explicitTurns[fields.BundleTurnID] = struct{}{}
			}
		}
	}
	for eventID, turnID := range links {
		if !validEventID(eventID) || strings.TrimSpace(turnID) == "" {
			return errors.New("trace package: invalid trace-to-native link")
		}
		events, exists := turnEvents[turnID]
		if !exists {
			return errors.New("trace package: trace link names a missing native turn")
		}
		if _, linkedBack := events[eventID]; !linkedBack {
			return errors.New("trace package: native linkage is not bidirectional")
		}
	}
	for turnID, events := range turnEvents {
		for eventID := range events {
			if links[eventID] != turnID {
				return errors.New("trace package: native linkage is not bidirectional")
			}
		}
	}
	listed := make(map[string]struct{}, len(operatorSelected))
	for _, turnID := range operatorSelected {
		if _, duplicate := listed[turnID]; duplicate {
			return errors.New("trace package: duplicate operator-selected native turn")
		}
		if _, exists := explicitTurns[turnID]; !exists {
			return errors.New("trace package: operator-selected native turn list is inconsistent")
		}
		listed[turnID] = struct{}{}
	}
	if len(listed) != len(explicitTurns) {
		return errors.New("trace package: operator-selected native turn list is incomplete")
	}
	return nil
}

func stageTraces(
	ctx context.Context,
	stageDir string,
	selection Selection,
	snapshot Snapshot,
	scanner Scanner,
	accumulator *scannerAccumulator,
	nativeLinks map[string]string,
) (stagedMember, []string, []int, error) {
	member, err := newStagedMember(stageDir, TraceMemberName)
	if err != nil {
		return stagedMember{}, nil, nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = member.file.Close()
			_ = os.Remove(member.Path)
		}
	}()

	clients := make(map[string]struct{}, len(selection.Clients))
	for _, client := range selection.Clients {
		clients[client] = struct{}{}
	}
	eventSet := make(map[string]struct{})
	schemaSet := make(map[int]struct{})
	err = readSnapshotLines(ctx, snapshot, MaxTraceLineBytes, func(line []byte) error {
		projected, err := projectTraceV1(line)
		if err != nil {
			return fmt.Errorf("read selected trace: %w", err)
		}
		if projected.StartedAt.Before(selection.Since) ||
			!projected.StartedAt.Before(selection.Until) {
			return nil
		}
		if _, selected := clients[projected.Client]; !selected {
			return nil
		}
		if _, duplicate := eventSet[projected.EventID]; duplicate {
			return fmt.Errorf("read selected trace: duplicate event_id")
		}
		if len(eventSet) >= MaxSelectedTraces {
			return fmt.Errorf("%w: selected Switch traces", ErrLimitExceeded)
		}
		for _, body := range projected.Bodies {
			if err := scanBody(ctx, scanner, body, accumulator); err != nil {
				return err
			}
		}
		if nativeTurnID := strings.TrimSpace(nativeLinks[projected.EventID]); nativeTurnID != "" {
			projected.Packaged.NativeTurnID = &nativeTurnID
		}
		encoded, err := json.Marshal(projected.Packaged)
		if err != nil {
			return fmt.Errorf("encode packaged trace: %w", err)
		}
		if len(encoded)+1 > MaxTraceLineBytes {
			return fmt.Errorf("%w: packaged trace row", ErrLimitExceeded)
		}
		scanPlainMetadata(encoded, accumulator)
		if err := member.appendJSONLine(encoded, MaxUncompressedBytes); err != nil {
			return err
		}
		eventSet[projected.EventID] = struct{}{}
		schemaSet[projected.SchemaVersion] = struct{}{}
		return nil
	})
	if err != nil {
		return stagedMember{}, nil, nil, err
	}
	if err := member.closeAndHash(); err != nil {
		return stagedMember{}, nil, nil, err
	}
	keep = true

	eventIDs := make([]string, 0, len(eventSet))
	for eventID := range eventSet {
		eventIDs = append(eventIDs, eventID)
	}
	for eventID := range nativeLinks {
		if _, selected := eventSet[eventID]; !selected {
			return stagedMember{}, nil, nil, errors.New("trace package: native link names an unselected trace")
		}
	}
	slices.Sort(eventIDs)
	schemaVersions := make([]int, 0, len(schemaSet))
	for version := range schemaSet {
		schemaVersions = append(schemaVersions, version)
	}
	slices.Sort(schemaVersions)
	return member.stagedMember, eventIDs, schemaVersions, nil
}

func stageTelemetry(
	ctx context.Context,
	stageDir string,
	snapshot Snapshot,
	eventIDs []string,
	accumulator *scannerAccumulator,
) (stagedMember, int, error) {
	member, err := newStagedMember(stageDir, TelemetryMemberName)
	if err != nil {
		return stagedMember{}, 0, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = member.file.Close()
			_ = os.Remove(member.Path)
		}
	}()

	wanted := make(map[string]struct{}, len(eventIDs))
	for _, eventID := range eventIDs {
		wanted[eventID] = struct{}{}
	}
	included := make(map[string]struct{}, len(eventIDs))
	err = readSnapshotLines(ctx, snapshot, MaxTelemetryLineBytes, func(line []byte) error {
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var event telemetry.EventV1
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("read telemetry row: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return errors.New("read telemetry row: expected one JSON object")
		}
		if _, selected := wanted[event.EventID]; !selected {
			return nil
		}
		if event.SchemaVersion != telemetry.SchemaVersionV1 ||
			event.Event != telemetry.EventRequest || event.Validate() != nil {
			return errors.New("read telemetry row: selected row is invalid")
		}
		if _, duplicate := included[event.EventID]; duplicate {
			return errors.New("read telemetry row: duplicate selected event_id")
		}
		if len(included) >= MaxSelectedTelemetry {
			return fmt.Errorf("%w: selected telemetry rows", ErrLimitExceeded)
		}
		scanPlainMetadata(line, accumulator)
		if err := member.appendJSONLine(line, MaxUncompressedBytes); err != nil {
			return err
		}
		included[event.EventID] = struct{}{}
		return nil
	})
	if err != nil {
		return stagedMember{}, 0, err
	}
	if err := member.closeAndHash(); err != nil {
		return stagedMember{}, 0, err
	}
	keep = true
	return member.stagedMember, len(wanted) - len(included), nil
}

func stageNativeMember(
	ctx context.Context,
	stageDir string,
	native NativeMember,
	accumulator *scannerAccumulator,
) (stagedMember, error) {
	if native.Name != "native/claude-code/turns.jsonl" &&
		native.Name != "native/codex/turns.jsonl" {
		return stagedMember{}, fmt.Errorf("%w: unsupported native member", ErrUnsafeMember)
	}
	if len(native.Rows) > MaxNormalizedNativeTurns {
		return stagedMember{}, fmt.Errorf("%w: normalized native turns", ErrLimitExceeded)
	}
	member, err := newStagedMember(stageDir, native.Name)
	if err != nil {
		return stagedMember{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = member.file.Close()
			_ = os.Remove(member.Path)
		}
	}()
	for _, row := range native.Rows {
		if err := ctx.Err(); err != nil {
			return stagedMember{}, err
		}
		trimmed := bytes.TrimSpace(row)
		if len(trimmed) == 0 || len(trimmed)+1 > MaxNativeLineBytes || !json.Valid(trimmed) || trimmed[0] != '{' {
			return stagedMember{}, errors.New("trace package: invalid normalized native row")
		}
		scanPlainNativeRecord(ctx, trimmed, accumulator)
		if err := member.appendJSONLine(trimmed, MaxUncompressedBytes); err != nil {
			return stagedMember{}, err
		}
	}
	if err := member.closeAndHash(); err != nil {
		return stagedMember{}, err
	}
	keep = true
	return member.stagedMember, nil
}

func scanPlainNativeRecord(
	ctx context.Context,
	row []byte,
	accumulator *scannerAccumulator,
) {
	if ctx.Err() != nil || len(row) > MaxNativeLineBytes {
		accumulator.unscannedNativeRecords++
		return
	}
	accumulator.scannedNativeRecords++
	for _, candidate := range highConfidenceCredentialPatterns {
		if candidate.pattern.Match(row) {
			accumulator.detectedCategoryCounts[candidate.category]++
		}
	}
}

func scanPlainMetadata(value []byte, accumulator *scannerAccumulator) {
	for _, candidate := range highConfidenceCredentialPatterns {
		if candidate.pattern.Match(value) {
			accumulator.detectedCategoryCounts[candidate.category]++
		}
	}
}

func validateNativeManifestMetadata(native NativeMember) error {
	if len(native.Client) == 0 || len(native.Client) > 64 || len(native.SourceKind) == 0 || len(native.SourceKind) > 64 ||
		len(native.CollectorVersion) == 0 || len(native.CollectorVersion) > 64 {
		return errors.New("trace package: invalid native collector manifest metadata")
	}
	for _, version := range native.ClientVersions {
		if !safeClientVersion(version) {
			return errors.New("trace package: invalid native client version")
		}
	}
	for _, method := range native.CorrelationMethods {
		if len(method) == 0 || len(method) > 64 {
			return errors.New("trace package: invalid native correlation method")
		}
	}
	for reason, count := range native.Exclusions {
		if len(reason) == 0 || len(reason) > 64 || count < 0 {
			return errors.New("trace package: invalid native exclusion metadata")
		}
	}
	return nil
}

func safeClientVersion(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for index, char := range value {
		if char >= '0' && char <= '9' || char == '.' || char == '-' || char == '+' ||
			(index == 0 && char == 'v') || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' {
			continue
		}
		return false
	}
	return value[0] == 'v' || value[0] >= '0' && value[0] <= '9'
}

func scanBody(
	ctx context.Context,
	scanner Scanner,
	body BodyForScan,
	accumulator *scannerAccumulator,
) error {
	if scanner.Scan == nil || strings.TrimSpace(scanner.Version) == "" {
		accumulator.unscannedBodies++
		return nil
	}
	result, err := scanner.Scan(ctx, body)
	if err != nil {
		return fmt.Errorf("scan captured body: %w", err)
	}
	if !result.Scanned {
		accumulator.unscannedBodies++
		return nil
	}
	accumulator.scannedBodies++
	seen := make(map[string]struct{}, len(result.DetectedCategories))
	for _, category := range result.DetectedCategories {
		category = strings.TrimSpace(category)
		if category == "" {
			return errors.New("scan captured body: empty detected category")
		}
		if _, duplicate := seen[category]; duplicate {
			continue
		}
		seen[category] = struct{}{}
		accumulator.detectedCategoryCounts[category]++
	}
	return nil
}

func detectedCount(categories map[string]int) int {
	count := 0
	for _, value := range categories {
		count += value
	}
	return count
}

func stageManifest(stageDir string, manifest ManifestV1) (stagedMember, error) {
	member, err := newStagedMember(stageDir, ManifestMemberName)
	if err != nil {
		return stagedMember{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = member.file.Close()
			_ = os.Remove(member.Path)
		}
	}()
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return stagedMember{}, fmt.Errorf("encode package manifest: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := member.append(encoded, MaxUncompressedBytes); err != nil {
		return stagedMember{}, err
	}
	member.RecordCount = 1
	if err := member.closeAndHash(); err != nil {
		return stagedMember{}, err
	}
	keep = true
	return member.stagedMember, nil
}

type stagedMemberWriter struct {
	stagedMember
	file *os.File
}

func newStagedMember(stageDir, name string) (*stagedMemberWriter, error) {
	if err := ValidateMemberName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(stageDir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create staged member directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create staged member %s: %w", name, err)
	}
	return &stagedMemberWriter{
		stagedMember: stagedMember{Name: name, Path: path},
		file:         file,
	}, nil
}

func (w *stagedMemberWriter) appendJSONLine(line []byte, max int64) error {
	if err := w.append(line, max); err != nil {
		return err
	}
	if err := w.append([]byte{'\n'}, max); err != nil {
		return err
	}
	w.RecordCount++
	return nil
}

func (w *stagedMemberWriter) append(data []byte, max int64) error {
	if int64(len(data)) > max-w.Bytes {
		return fmt.Errorf("%w: staged member %s", ErrLimitExceeded, w.Name)
	}
	n, err := w.file.Write(data)
	w.Bytes += int64(n)
	if err != nil {
		return fmt.Errorf("write staged member %s: %w", w.Name, err)
	}
	if n != len(data) {
		return fmt.Errorf("write staged member %s: %w", w.Name, io.ErrShortWrite)
	}
	return nil
}

func (w *stagedMemberWriter) closeAndHash() error {
	if err := w.file.Sync(); err != nil {
		_ = w.file.Close()
		return fmt.Errorf("sync staged member %s: %w", w.Name, err)
	}
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close staged member %s: %w", w.Name, err)
	}
	file, err := os.Open(w.Path)
	if err != nil {
		return fmt.Errorf("open staged member %s for hashing: %w", w.Name, err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("hash staged member %s: %w", w.Name, errors.Join(copyErr, closeErr))
	}
	w.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return nil
}

func readSnapshotLines(
	ctx context.Context,
	snapshot Snapshot,
	maxLineBytes int,
	consume func([]byte) error,
) error {
	if err := snapshot.validate("source"); err != nil {
		return err
	}
	reader, err := snapshot.Open(ctx)
	if err != nil {
		return fmt.Errorf("open bounded snapshot: %w", err)
	}
	limited := &io.LimitedReader{R: reader, N: snapshot.EstimatedBytes + 1}
	buffered := bufio.NewReaderSize(limited, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			_ = reader.Close()
			return err
		}
		line, readErr := readBoundedLine(buffered, maxLineBytes)
		if len(line) > 0 && readErr == nil {
			if err := consume(line); err != nil {
				_ = reader.Close()
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = reader.Close()
			return readErr
		}
	}
	if limited.N <= 0 {
		_ = reader.Close()
		return fmt.Errorf("%w: snapshot exceeded captured boundary", ErrSnapshotChanged)
	}
	if err := reader.Close(); err != nil {
		return fmt.Errorf("close bounded snapshot: %w", err)
	}
	if err := snapshot.Verify(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotChanged, err)
	}
	return nil
}

func readBoundedLine(reader *bufio.Reader, maxBytes int) ([]byte, error) {
	line := make([]byte, 0, min(maxBytes, 256<<10))
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > maxBytes-len(line) {
			return nil, fmt.Errorf("%w: JSONL record", ErrLimitExceeded)
		}
		line = append(line, fragment...)
		if err == nil {
			return line[:len(line)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			// Only newline-complete JSONL rows are eligible. Ignore a partial tail.
			return nil, io.EOF
		}
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
}

func buildZIP(
	stageDir string,
	manifest stagedMember,
	payload []stagedMember,
) (string, int64, string, error) {
	file, err := os.OpenFile(
		filepath.Join(stageDir, "package.zip"),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		return "", 0, "", fmt.Errorf("create staged ZIP: %w", err)
	}
	zipPath := file.Name()
	writer := zip.NewWriter(&boundedZIPWriter{destination: file, remaining: MaxZIPBytes})
	members := append([]stagedMember{manifest}, payload...)
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		if _, duplicate := seen[member.Name]; duplicate {
			_ = writer.Close()
			_ = file.Close()
			return "", 0, "", fmt.Errorf("%w: duplicate %s", ErrUnsafeMember, member.Name)
		}
		seen[member.Name] = struct{}{}
		if err := addZIPMember(writer, member); err != nil {
			_ = writer.Close()
			_ = file.Close()
			return "", 0, "", err
		}
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return "", 0, "", fmt.Errorf("close staged ZIP: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", 0, "", fmt.Errorf("sync staged ZIP: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", 0, "", fmt.Errorf("close staged ZIP file: %w", err)
	}
	info, err := os.Stat(zipPath)
	if err != nil {
		return "", 0, "", fmt.Errorf("stat staged ZIP: %w", err)
	}
	if info.Size() > MaxZIPBytes {
		return "", 0, "", fmt.Errorf("%w: final ZIP", ErrLimitExceeded)
	}
	hash, err := hashFile(zipPath)
	if err != nil {
		return "", 0, "", err
	}
	return zipPath, info.Size(), hash, nil
}

func addZIPMember(writer *zip.Writer, member stagedMember) error {
	if err := ValidateMemberName(member.Name); err != nil {
		return err
	}
	header := &zip.FileHeader{Name: member.Name, Method: zip.Deflate}
	header.SetMode(0o600)
	header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
	destination, err := writer.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create ZIP member %s: %w", member.Name, err)
	}
	source, err := os.Open(member.Path)
	if err != nil {
		return fmt.Errorf("open staged member %s: %w", member.Name, err)
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := source.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("write ZIP member %s: %w", member.Name, errors.Join(copyErr, closeErr))
	}
	return nil
}

// ValidateMemberName rejects names that can escape or confuse an extraction
// root. Package-owned names are synthetic and always slash-separated.
func ValidateMemberName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") ||
		strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: invalid name", ErrUnsafeMember)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if clean != name || name == "." || strings.HasPrefix(name, "../") ||
		strings.Contains(name, "/../") {
		return fmt.Errorf("%w: invalid name", ErrUnsafeMember)
	}
	return nil
}

func publishWithoutReplacement(source, destination string) (bool, error) {
	if err := linkPath(source, destination); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, ErrDestinationExists
		}
		return false, fmt.Errorf("publish ZIP without replacement: %w", err)
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return true, &PublicationError{
			Applied: true,
			Err:     fmt.Errorf("sync published ZIP directory: %w", err),
		}
	}
	if err := os.Remove(source); err != nil {
		return true, &PublicationError{
			Applied: true,
			Err:     fmt.Errorf("remove staged ZIP link: %w", err),
		}
	}
	return true, nil
}

func validateDestination(value string) (string, error) {
	if strings.TrimSpace(value) == "" || value == "-" {
		return "", errors.New("trace package: destination path is required and cannot be stdout")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve package destination: %w", err)
	}
	abs = filepath.Clean(abs)
	if !strings.EqualFold(filepath.Ext(abs), ".zip") {
		return "", errors.New("trace package: destination must use a .zip extension")
	}
	parent := filepath.Dir(abs)
	info, err := os.Stat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect package destination directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("trace package: destination parent is not a directory")
	}
	if _, err := os.Lstat(abs); err == nil {
		return "", ErrDestinationExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect package destination: %w", err)
	}
	return abs, nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file for hashing: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return "", fmt.Errorf("hash file: %w", errors.Join(copyErr, closeErr))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	err = directory.Sync()
	return errors.Join(err, directory.Close())
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
