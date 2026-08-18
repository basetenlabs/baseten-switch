package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
	"github.com/basetenlabs/baseten-switch/gateway/internal/tracepackage"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

const traceSensitiveWarning = "captured request and response bodies may contain prompts, responses, reasoning summaries, tool definitions, tool arguments and results, source code, files, terminal output, MCP output, images, documents, pasted credentials, personal data, and regulated data"

const tracePackageSelectionMemoryLimit = tracepackage.MaxWorkingMemoryBytes / 4

var traceCommandNow = time.Now

type traceRuntimeAdminStatus struct {
	ConfigPath   string                 `json:"config_path"`
	TraceCapture traceRuntimeProjection `json:"trace_capture"`
}

type traceRuntimeProjection struct {
	Enabled         bool              `json:"enabled"`
	State           string            `json:"state"`
	StoreBytes      int64             `json:"store_bytes"`
	StoreLimitBytes int64             `json:"store_limit_bytes"`
	RetentionDays   int               `json:"retention_days"`
	ActiveSegment   string            `json:"active_segment"`
	LastWriteAt     *string           `json:"last_write_at"`
	LastError       *string           `json:"last_error"`
	InFlight        int64             `json:"in_flight"`
	InFlightBytes   int64             `json:"in_flight_bytes"`
	QueuedRecords   int               `json:"queued_records"`
	QueuedBytes     int64             `json:"queued_bytes"`
	DroppedRecords  map[string]uint64 `json:"dropped_records"`
	BodyOmissions   map[string]uint64 `json:"body_omissions"`
}

var fetchTraceRuntimeAdminStatus = func(adminAddr string) (traceRuntimeAdminStatus, error) {
	var status traceRuntimeAdminStatus
	err := getJSON(adminAddr, "/v1/admin/status", &status)
	return status, err
}

var tracePackageInputIsTerminal = func(in io.Reader) bool {
	file, ok := in.(*os.File)
	if !ok {
		return false
	}
	return isTerminal(file)
}

func cmdTraces(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch traces <status|enable|disable|package|purge|cleanup>")
		return 2
	}
	switch args[0] {
	case "status":
		return cmdTracesStatus(args[1:], os.Stdout)
	case "enable":
		return cmdTracesEnable(args[1:], os.Stdout)
	case "disable":
		return cmdTracesDisable(args[1:], os.Stdout)
	case "package":
		return cmdTracesPackage(args[1:], os.Stdin, os.Stdout)
	case "purge":
		return cmdTracesPurge(args[1:], os.Stdout)
	case "cleanup":
		return cmdTracesCleanup(args[1:], os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown traces subcommand: %s\n", args[0])
		return 2
	}
}

func loadTraceCommandConfig() (string, *config.File, tracecapture.RuntimePaths, error) {
	path, notices := resolveConfigPath()
	for _, notice := range notices {
		fmt.Fprintln(os.Stderr, notice)
	}
	file, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, tracecapture.RuntimePaths{}, errors.New(config.MissingConfigMessage(path))
		}
		return "", nil, tracecapture.RuntimePaths{}, errors.New(config.MalformedConfigMessage(path, err))
	}
	paths, err := tracecapture.ResolveRuntimePaths(config.ExpandPath(path))
	if err != nil {
		return "", nil, tracecapture.RuntimePaths{}, err
	}
	return path, file, paths, nil
}

func cmdTracesEnable(args []string, out io.Writer) int {
	options, err := parseTracePolicyOptions(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	path, _, _, err := loadTraceCommandConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	policy := config.TraceCapture{
		Enabled:       true,
		Clients:       append([]string(nil), options.clients...),
		RetentionDays: options.retentionDays,
	}
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "traces enable: %v\n", err)
		return 1
	}
	defer lock.close()
	if err := recoverInterruptedExactConfigCommit(path); err != nil {
		fmt.Fprintf(os.Stderr, "traces enable: %v\n", err)
		return 1
	}
	if err := config.SetTraceCapture(path, policy); err != nil {
		fmt.Fprintf(os.Stderr, "traces enable: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "WARNING: trace capture configured: %s.\n", traceSensitiveWarning)
	reloadTraceConfig(out)
	fmt.Fprintf(out, "trace capture configured for %s with %d-day retention\n", strings.Join(policy.Clients, ", "), policy.RetentionDays)
	return 0
}

func cmdTracesDisable(args []string, out io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch traces disable")
		return 2
	}
	path, file, _, err := loadTraceCommandConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	policy := config.TraceCapture{RetentionDays: config.DefaultTraceRetentionDays}
	if file.Global.TraceCapture != nil {
		policy = *file.Global.TraceCapture
		policy.Clients = append([]string(nil), policy.Clients...)
	}
	policy.Enabled = false
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "traces disable: %v\n", err)
		return 1
	}
	defer lock.close()
	if err := recoverInterruptedExactConfigCommit(path); err != nil {
		fmt.Fprintf(os.Stderr, "traces disable: %v\n", err)
		return 1
	}
	latest, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "traces disable: %v\n", err)
		return 1
	}
	policy = config.TraceCapture{RetentionDays: config.DefaultTraceRetentionDays}
	if latest.Global.TraceCapture != nil {
		policy = *latest.Global.TraceCapture
		policy.Clients = append([]string(nil), policy.Clients...)
	}
	policy.Enabled = false
	if err := config.SetTraceCapture(path, policy); err != nil {
		fmt.Fprintf(os.Stderr, "traces disable: %v\n", err)
		return 1
	}
	reloadTraceConfig(out)
	fmt.Fprintln(out, "trace capture configured disabled; retained traces were not deleted")
	return 0
}

func reloadTraceConfig(out io.Writer) {
	pid, err := pidfile.Read()
	if err != nil || !pidfile.IsAlive(pid) {
		fmt.Fprintln(out, "router is not running; the policy will apply on next start")
		return
	}
	if err := signalRouter(pid); err != nil {
		fmt.Fprintf(os.Stderr, "warning: config saved but router reload failed: %v\n", err)
		return
	}
	fmt.Fprintf(out, "router reload requested (pid %d)\n", pid)
}

func cmdTracesStatus(args []string, out io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch traces status")
		return 2
	}
	path, file, paths, err := loadTraceCommandConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	policy, err := config.ResolveTraceCapture(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "traces status: %v\n", err)
		return 1
	}
	configuredState := "disabled"
	if policy.Enabled {
		configuredState = "enabled"
	}
	var storeBytes int64
	segments, segmentErr := tracecapture.DiscoverSegments(paths.TraceDir)
	if segmentErr != nil && !errors.Is(segmentErr, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "traces status: %v\n", segmentErr)
		return 1
	}
	for _, segment := range segments {
		storeBytes += segment.Size
	}
	exports, exportErr := tracecapture.InspectRuntimeExports(paths)
	if exportErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not inspect retained packages: %v\n", exportErr)
	}
	fmt.Fprintf(out, "configured: %s\n", configuredState)
	runtimeStatus, runtimeErr := fetchTraceRuntimeAdminStatus(
		envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr),
	)
	runtimeAvailable := runtimeErr == nil && runtimeStatus.ConfigPath != "" &&
		canonicalPath(runtimeStatus.ConfigPath) == canonicalPath(path)
	if !runtimeAvailable {
		fmt.Fprintln(out, "capture: unavailable")
		fmt.Fprintln(out, "runtime: router is not running, unreachable, or using a different config")
	} else {
		runtime := runtimeStatus.TraceCapture
		fmt.Fprintf(out, "capture: %s\n", runtime.State)
		fmt.Fprintf(out, "runtime_enabled: %t\n", runtime.Enabled)
		fmt.Fprintf(out, "active_segment: %s\n", displayTraceStatusValue(runtime.ActiveSegment))
		fmt.Fprintf(out, "last_write_at: %s\n", displayTraceStatusPointer(runtime.LastWriteAt))
		fmt.Fprintf(out, "last_error: %s\n", displayTraceStatusPointer(runtime.LastError))
		fmt.Fprintf(out, "in_flight: %d (%d bytes)\n", runtime.InFlight, runtime.InFlightBytes)
		fmt.Fprintf(out, "queue: %d records (%d bytes)\n", runtime.QueuedRecords, runtime.QueuedBytes)
		fmt.Fprintf(out, "dropped_records: %s\n", formatTraceStatusCounts(runtime.DroppedRecords))
		fmt.Fprintf(out, "body_omissions: %s\n", formatTraceStatusCounts(runtime.BodyOmissions))
	}
	fmt.Fprintf(out, "clients: %s\n", strings.Join(policy.Clients, ", "))
	fmt.Fprintf(out, "retention_days: %d\n", policy.RetentionDays)
	fmt.Fprintf(out, "store_bytes: %d\n", storeBytes)
	fmt.Fprintf(out, "store_limit_bytes: %d\n", tracecapture.DefaultMaxStoreBytes)
	fmt.Fprintf(out, "trace_path: %s\n", paths.TraceDir)
	fmt.Fprintf(out, "packages: %d (%d bytes)\n", exports.PackageCount, exports.PackageBytes)
	fmt.Fprintf(out, "quarantine: %d (%d bytes)\n", exports.QuarantineCount, exports.QuarantineBytes)
	if policy.Enabled {
		fmt.Fprintf(out, "warning: %s\n", traceSensitiveWarning)
	}
	return 0
}

func displayTraceStatusPointer(value *string) string {
	if value == nil {
		return "none"
	}
	return displayTraceStatusValue(*value)
}

func displayTraceStatusValue(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func formatTraceStatusCounts(counts map[string]uint64) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}

func cmdTracesPackage(args []string, in io.Reader, out io.Writer) int {
	now := traceCommandNow().UTC()
	options, err := parseTracePackageOptions(args, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	_, file, paths, err := loadTraceCommandConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := tracecapture.ValidateRuntimeTraceStore(paths); err != nil {
		fmt.Fprintf(os.Stderr, "traces package: trace store is unavailable: %v\n", err)
		return 1
	}
	selectedTraces, selectionStats, err := tracecapture.ReadSelectedTraces(paths.TraceDir, tracecapture.TraceSelection{
		Since: options.selection.Since, Until: options.selection.Until,
		Clients: options.selection.Clients, MaxRetainedEncodedBytes: tracePackageSelectionMemoryLimit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "traces package: select traces: %v\n", err)
		return 1
	}
	if selectionStats.MalformedRows > 0 || selectionStats.InvalidRows > 0 {
		fmt.Fprintf(os.Stderr, "traces package: trace store contains malformed=%d invalid=%d complete row(s); refusing package\n", selectionStats.MalformedRows, selectionStats.InvalidRows)
		return 1
	}
	var nativeData nativePackageData
	if options.includeNative {
		nativeData, err = collectNativePackageData(context.Background(), paths, options, selectedTraces)
		if err != nil {
			fmt.Fprintf(os.Stderr, "traces package: native collection failed: %v\n", err)
			return 1
		}
	}
	if options.dryRun {
		return summarizeTracePackage(options, selectedTraces, selectionStats, nativeData, out, true)
	}
	if code := summarizeTracePackage(options, selectedTraces, selectionStats, nativeData, out, false); code != 0 {
		return code
	}
	if !options.yes {
		if !tracePackageInputIsTerminal(in) {
			fmt.Fprintln(os.Stderr, "traces package: noninteractive use requires --yes")
			return 2
		}
		confirmed, err := confirmSensitivePackage(in, out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "traces package: %v\n", err)
			return 1
		}
		if !confirmed {
			fmt.Fprintln(out, "trace package cancelled")
			return 1
		}
	}

	destination := options.output
	defaultDestination := destination == ""
	if err := tracecapture.EnsureRuntimeExportStore(paths); err != nil {
		fmt.Fprintf(os.Stderr, "traces package: prepare export store: %v\n", err)
		return 1
	}
	if defaultDestination {
		destination = filepath.Join(paths.ExportDir, "baseten-switch-traces-"+now.Format("20060102T150405Z")+".zip")
	} else {
		fmt.Fprintln(os.Stderr, "warning: a custom destination may be copied by repositories, shared folders, backups, or cloud synchronization")
	}
	telemetryDir := config.DefaultTelemetryDir()
	if file.Global.TelemetryDir != "" {
		telemetryDir = config.ExpandPath(file.Global.TelemetryDir)
	}
	createOptions := tracepackage.Options{
		Destination:   destination,
		Selection:     options.selection,
		SwitchVersion: version.Version,
		Sources: tracepackage.Sources{
			Traces: tracepackage.TraceValuesSource(selectedTraces, selectionStats.SelectedEncodedBytes),
		},
		Scanner:                     tracepackage.NewHighConfidenceScanner(),
		AllowUnscannedContent:       options.allowUnscannedContent,
		AllowDetectedSecrets:        options.allowDetectedSecrets,
		NativeMembers:               nativeData.members,
		TraceNativeLinks:            nativeData.traceLinks,
		OperatorSelectedNativeTurns: nativeData.operatorSelectedTurnIDs,
	}
	if _, err := os.Lstat(telemetryDir); err == nil {
		createOptions.Sources.Telemetry = tracepackage.TelemetryStoreSource(telemetryDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "traces package: inspect telemetry store: %v\n", err)
		return 1
	}
	createOptions.Quarantine = func(ctx context.Context, stageDir string) (string, error) {
		if !defaultDestination {
			return tracecapture.QuarantineExternalExportStaging(paths, stageDir, destination)
		}
		return tracecapture.QuarantineExportStaging(paths, stageDir)
	}
	result, err := tracepackage.Create(context.Background(), createOptions)
	if err != nil {
		var scanErr *tracepackage.ContentScanError
		if errors.As(err, &scanErr) {
			fmt.Fprintf(os.Stderr, "traces package: scanner blocked publication; unscanned=%d", scanErr.UnscannedCount)
			if len(scanErr.CategoryCounts) > 0 {
				fmt.Fprintf(os.Stderr, "; detected categories=%s", formatTraceCounts(scanErr.CategoryCounts))
			}
			fmt.Fprintln(os.Stderr)
		}
		var cleanup *tracepackage.CleanupError
		if errors.As(err, &cleanup) {
			if cleanup.CleanupID != "" {
				fmt.Fprintf(os.Stderr, "traces package: staging quarantined; run 'baseten-switch traces cleanup %s'\n", cleanup.CleanupID)
			} else {
				fmt.Fprintf(os.Stderr, "traces package: sensitive staging cleanup failed; inspect the private recovery directory %s\n", cleanup.RecoveryRoot)
			}
			return 1
		}
		fmt.Fprintf(os.Stderr, "traces package: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "created %s\n", result.Destination)
	fmt.Fprintf(out, "traces: %d; telemetry: %d; native turns: %d; bytes: %d\n", result.TraceCount, result.TelemetryCount, result.NativeTurnCount, result.ArchiveBytes)
	fmt.Fprintf(out, "sha256: %s\n", result.ArchiveSHA256)
	fmt.Fprintln(out, "no upload was performed")
	return 0
}

func summarizeTracePackage(options tracePackageOptions, traces []tracecapture.TraceV1, stats tracecapture.TraceSelectionStats, native nativePackageData, out io.Writer, dryRun bool) int {
	count := stats.SelectedRows
	var estimated int64
	scanner := tracepackage.NewHighConfidenceScanner()
	scannedBodies, unscannedBodies, scannedNative, unscannedNative := 0, 0, 0, 0
	detected := make(map[string]int)
	for _, trace := range traces {
		estimated += int64(len(trace.Request.BodyBase64) + len(trace.Response.BodyBase64))
		for _, body := range []tracecapture.BodyV1{trace.Request, trace.Response.BodyV1} {
			if body.CaptureState != tracecapture.CaptureStateCaptured {
				continue
			}
			result, scanErr := scanner.Scan(context.Background(), tracepackage.BodyForScan{
				EventID: trace.EventID, Boundary: body.Boundary, ContentType: body.ContentType,
				ContentEncoding: body.ContentEncoding, BodyBase64: body.BodyBase64,
			})
			if scanErr != nil || !result.Scanned {
				unscannedBodies++
				continue
			}
			scannedBodies++
			for _, category := range result.DetectedCategories {
				detected[category]++
			}
		}
	}
	sort.Strings(options.selection.Clients)
	fmt.Fprintf(out, "selection: %d trace(s), at least %d encoded body bytes\n", count, estimated)
	if options.includeNative {
		nativeTurns := 0
		for _, member := range native.members {
			nativeTurns += len(member.Rows)
			for _, row := range member.Rows {
				result, scanErr := scanner.Scan(context.Background(), tracepackage.BodyForScan{
					Boundary: "native_record", ContentType: "application/json",
					BodyBase64: base64.StdEncoding.EncodeToString(row),
				})
				if scanErr != nil || !result.Scanned {
					unscannedNative++
					continue
				}
				scannedNative++
				for _, category := range result.DetectedCategories {
					detected[category]++
				}
			}
		}
		fmt.Fprintf(out, "native candidates: %d file(s), %d bytes; normalized turns: %d\n", native.candidateFiles, native.candidateBytes, nativeTurns)
		if len(native.exclusions) > 0 {
			reasons := make([]string, 0, len(native.exclusions))
			for reason := range native.exclusions {
				reasons = append(reasons, reason)
			}
			sort.Strings(reasons)
			for _, reason := range reasons {
				fmt.Fprintf(out, "native exclusion %s: %d\n", reason, native.exclusions[reason])
			}
		}
	}
	fmt.Fprintf(out, "scanner: bodies scanned=%d unscanned=%d; native records scanned=%d unscanned=%d", scannedBodies, unscannedBodies, scannedNative, unscannedNative)
	if len(detected) > 0 {
		fmt.Fprintf(out, "; detected categories=%s", formatTraceCounts(detected))
	}
	fmt.Fprintln(out)
	members := []string{"manifest.json", "switch/traces.jsonl", "switch/telemetry.jsonl when matching telemetry exists"}
	for _, member := range native.members {
		if len(member.Rows) > 0 {
			members = append(members, member.Name)
		}
	}
	fmt.Fprintf(out, "members: %s\n", strings.Join(members, ", "))
	fmt.Fprintf(out, "clients: %s; interval: [%s, %s)\n", strings.Join(options.selection.Clients, ", "), options.selection.Since.Format(time.RFC3339), options.selection.Until.Format(time.RFC3339))
	if dryRun {
		fmt.Fprintln(out, "no archive was created and no upload was performed")
	} else {
		fmt.Fprintln(out, "no upload will be performed")
	}
	return 0
}

func formatTraceCounts(counts map[string]int) string {
	values := make([]string, 0, len(counts))
	for category, count := range counts {
		values = append(values, fmt.Sprintf("%s=%d", category, count))
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func confirmSensitivePackage(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprintf(out, "This package contains sensitive trace content. %s. Create it? [y/N] ", traceSensitiveWarning)
	line, err := bufio.NewReader(io.LimitReader(in, 4<<10)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func cmdTracesPurge(args []string, out io.Writer) int {
	packages := false
	yes := false
	for _, arg := range args {
		switch arg {
		case "--packages":
			packages = true
		case "--yes":
			yes = true
		default:
			fmt.Fprintf(os.Stderr, "traces purge: unknown option %s\n", arg)
			return 2
		}
	}
	if !yes {
		fmt.Fprintln(os.Stderr, "traces purge permanently deletes captured content and requires --yes")
		return 2
	}
	_, file, paths, err := loadTraceCommandConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	policy, err := config.ResolveTraceCapture(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if policy.Enabled {
		fmt.Fprintln(os.Stderr, "traces purge: disable trace capture and wait for the writer to close before purging")
		return 1
	}
	result, err := tracecapture.PurgeRuntimeTraceStore(paths)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "traces purge: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "removed %d trace artifact(s), %d bytes\n", result.RemovedFiles, result.RemovedBytes)
	if packages {
		exports, err := tracecapture.PurgeRuntimeExports(paths)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "traces purge packages: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "removed %d package(s), %d quarantine item(s), %d bytes\n", exports.RemovedPackages, exports.RemovedQuarantine, exports.RemovedBytes)
	}
	return 0
}

func cmdTracesCleanup(args []string, out io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch traces cleanup <identifier>")
		return 2
	}
	_, _, paths, err := loadTraceCommandConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := tracecapture.CleanupExportQuarantine(paths, args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "traces cleanup: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "removed quarantined staging item %s\n", args[0])
	return 0
}
