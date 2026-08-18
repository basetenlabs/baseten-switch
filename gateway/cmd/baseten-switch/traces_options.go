package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/tracepackage"
)

type tracePackageOptions struct {
	selection             tracepackage.Selection
	output                string
	includeNative         bool
	dryRun                bool
	yes                   bool
	allowUnscannedContent bool
	allowDetectedSecrets  bool
	nativeSessionSelector string
	includeCodexArchived  bool
}

func parseTracePackageOptions(args []string, now time.Time) (tracePackageOptions, error) {
	var result tracePackageOptions
	var sinceRaw, untilRaw string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		value := func(name string) (string, error) {
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return "", fmt.Errorf("traces package: %s requires a value", name)
			}
			index++
			return args[index], nil
		}
		switch arg {
		case "--since":
			var err error
			sinceRaw, err = value(arg)
			if err != nil {
				return result, err
			}
		case "--until":
			var err error
			untilRaw, err = value(arg)
			if err != nil {
				return result, err
			}
		case "--client":
			client, err := value(arg)
			if err != nil {
				return result, err
			}
			result.selection.Clients = append(result.selection.Clients, client)
		case "--output":
			var err error
			result.output, err = value(arg)
			if err != nil {
				return result, err
			}
		case "--native-session-selector":
			var err error
			result.nativeSessionSelector, err = value(arg)
			if err != nil {
				return result, err
			}
		case "--include-native":
			result.includeNative = true
		case "--include-codex-archived":
			result.includeCodexArchived = true
		case "--dry-run":
			result.dryRun = true
		case "--yes":
			result.yes = true
		case "--allow-unscanned-content":
			result.allowUnscannedContent = true
		case "--allow-detected-secrets":
			result.allowDetectedSecrets = true
		default:
			return result, fmt.Errorf("traces package: unknown option %s", arg)
		}
	}
	if sinceRaw == "" {
		return result, errors.New("traces package: --since is required")
	}
	if len(result.selection.Clients) == 0 {
		return result, errors.New("traces package: at least one --client is required")
	}
	if result.includeCodexArchived && !result.includeNative {
		return result, errors.New("traces package: --include-codex-archived requires --include-native")
	}
	if result.nativeSessionSelector != "" && !result.includeNative {
		return result, errors.New("traces package: --native-session-selector requires --include-native")
	}
	if result.yes && result.dryRun {
		return result, errors.New("traces package: --yes is not valid with --dry-run")
	}

	now = now.UTC()
	since, err := parseTraceBoundary(sinceRaw, now)
	if err != nil {
		return result, fmt.Errorf("traces package: invalid --since: %w", err)
	}
	until := now
	if untilRaw != "" {
		until, err = parseTraceBoundary(untilRaw, now)
		if err != nil {
			return result, fmt.Errorf("traces package: invalid --until: %w", err)
		}
	}
	result.selection.Since = since
	result.selection.Until = until
	if err := result.selection.Validate(); err != nil {
		return result, err
	}
	if !since.Before(now) {
		return result, errors.New("traces package: selection must begin before the command start time")
	}
	return result, nil
}

func parseTraceBoundary(value string, anchor time.Time) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return time.Time{}, errors.New("expected RFC 3339 timestamp or Go duration")
	}
	if duration == 0 {
		return time.Time{}, errors.New("duration must not be zero")
	}
	// Positive durations are lookback values. Negative durations preserve
	// their mathematical meaning, which is useful for an explicit future
	// boundary and will subsequently fail the selection safety checks.
	if duration > 0 {
		duration = -duration
	}
	return anchor.Add(duration).UTC(), nil
}

type tracePolicyOptions struct {
	clients       []string
	retentionDays int
}

func parseTracePolicyOptions(args []string) (tracePolicyOptions, error) {
	result := tracePolicyOptions{retentionDays: 7}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--client":
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return result, errors.New("traces enable: --client requires a value")
			}
			index++
			result.clients = append(result.clients, args[index])
		case "--retention-days":
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return result, errors.New("traces enable: --retention-days requires a value")
			}
			index++
			value, err := strconv.Atoi(args[index])
			if err != nil {
				return result, errors.New("traces enable: --retention-days must be an integer")
			}
			result.retentionDays = value
		default:
			return result, fmt.Errorf("traces enable: unknown option %s", args[index])
		}
	}
	if len(result.clients) == 0 {
		return result, errors.New("traces enable: at least one --client is required")
	}
	if result.retentionDays < 1 || result.retentionDays > 365 {
		return result, errors.New("traces enable: --retention-days must be between 1 and 365")
	}
	return result, nil
}
