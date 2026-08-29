package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/internal/tracepackage"
)

type traceDecodeOptions struct {
	packagePath string
	outputDir   string
	yes         bool
}

func parseTraceDecodeOptions(args []string) (traceDecodeOptions, error) {
	var result traceDecodeOptions
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--output":
			index++
			if index >= len(args) || strings.TrimSpace(args[index]) == "" || strings.HasPrefix(args[index], "--") {
				return result, errors.New("traces decode: --output requires a value")
			}
			if result.outputDir != "" {
				return result, errors.New("traces decode: --output may be supplied only once")
			}
			result.outputDir = args[index]
		case "--yes":
			result.yes = true
		default:
			if strings.HasPrefix(args[index], "-") {
				return result, fmt.Errorf("traces decode: unknown option %s", args[index])
			}
			if result.packagePath != "" {
				return result, errors.New("traces decode: exactly one package path is required")
			}
			result.packagePath = args[index]
		}
	}
	if result.packagePath == "" {
		return result, errors.New("traces decode: package path is required")
	}
	if result.outputDir == "" {
		return result, errors.New("traces decode: --output is required")
	}
	return result, nil
}

func cmdTracesDecode(args []string, in io.Reader, out io.Writer) int {
	options, err := parseTraceDecodeOptions(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	resolvedOutput, err := tracepackage.ValidateDecodeOutputPath(options.outputDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "traces decode: %v\n", err)
		return 1
	}
	options.outputDir = resolvedOutput
	plan, err := tracepackage.InspectDecode(context.Background(), options.packagePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "traces decode: %v\n", err)
		return 1
	}
	preflight := plan.Preflight
	fmt.Fprintf(out, "destination: %s\n", options.outputDir)
	fmt.Fprintf(out, "archive: %s; package sha256: %s\n", preflight.ArchiveID, preflight.PackageSHA256)
	fmt.Fprintf(out, "traces: %d; captured bodies: %d; omitted bodies: %d; decoded bytes: %d\n", preflight.TraceCount, preflight.CapturedBodyCount, preflight.OmittedBodyCount, preflight.DecodedBytes)
	fmt.Fprintf(out, "telemetry: %d; native turns: %d; members: %s\n", preflight.TelemetryRows, preflight.NativeRows, strings.Join(preflight.MemberNames, ", "))
	fmt.Fprintf(out, "scanner: categories=%s; allow unscanned=%t; allow detected secrets=%t\n", formatTraceCounts(preflight.Scanner.DetectedCategoryCounts), preflight.Scanner.AllowUnscannedContentUsed, preflight.Scanner.AllowDetectedSecretsUsed)
	fmt.Fprintf(out, "warning: decoded files are discoverable plaintext; %s\n", traceSensitiveWarning)
	fmt.Fprintln(out, "no upload will be performed")
	if !options.yes {
		if !tracePackageInputIsTerminal(in) {
			fmt.Fprintln(os.Stderr, "traces decode: noninteractive use requires --yes")
			return 2
		}
		confirmed, err := confirmSensitiveDecode(in, out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "traces decode: %v\n", err)
			return 1
		}
		if !confirmed {
			fmt.Fprintln(out, "trace decode cancelled")
			return 1
		}
	}
	result, err := tracepackage.MaterializeDecode(context.Background(), plan, options.outputDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "traces decode: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "decoded %s\n", result.OutputDir)
	fmt.Fprintf(out, "archive: %s; package sha256: %s\n", result.ArchiveID, result.PackageSHA256)
	fmt.Fprintf(out, "traces: %d; materialized bodies: %d; decoded bytes: %d\n", result.TraceCount, result.MaterializedBodies, result.DecodedBytes)
	fmt.Fprintf(out, "scanner overrides: allow unscanned=%t; allow detected secrets=%t\n", preflight.Scanner.AllowUnscannedContentUsed, preflight.Scanner.AllowDetectedSecretsUsed)
	fmt.Fprintf(out, "warning: %s\n", traceSensitiveWarning)
	fmt.Fprintln(out, "no upload was performed")
	return 0
}

func confirmSensitiveDecode(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprintf(out, "Decoded files contain sensitive trace content. %s. Decode it? [y/N] ", traceSensitiveWarning)
	line, err := bufio.NewReader(io.LimitReader(in, 4<<10)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
