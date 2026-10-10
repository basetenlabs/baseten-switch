package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/updates"
	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

func newUpdateChecker() updates.Checker {
	executable, _ := os.Executable()
	resolved, err := filepath.EvalSymlinks(executable)
	if err == nil {
		executable = resolved
	}
	return updates.Checker{Root: filepath.Join(filepath.Dir(pidfile.Path()), "updates"), CurrentVersion: version.Version, InstallSource: updateInstallSource(executable), Private: os.Getenv("BASETEN_SWITCH_PRIVATE_RUNTIME") == "1"}
}

func updateInstallSource(executable string) string {
	path := filepath.ToSlash(executable)
	if strings.Contains(path, "/Cellar/baseten-switch/") && strings.HasSuffix(path, "/bin/baseten-switch") {
		return "homebrew"
	}
	if strings.HasPrefix(path, "/nix/store/") && strings.HasSuffix(path, "/bin/baseten-switch") {
		return "nix"
	}
	return "unknown"
}

func cmdUpdate(args []string) int { return runUpdate(args, newUpdateChecker(), os.Stdout, os.Stderr) }

func runUpdate(args []string, checker updates.Checker, out, errOut io.Writer) int {
	if (len(args) == 2 || (len(args) == 3 && args[2] == "--json")) && args[0] == "automatic" && (args[1] == "on" || args[1] == "off") {
		if err := checker.SetAutomatic(args[1] == "on"); err != nil {
			fmt.Fprintf(errOut, "Unable to save update preference: %v\n", err)
			return 1
		}
		if len(args) == 3 {
			result, err := checker.Snapshot()
			if encodeErr := json.NewEncoder(out).Encode(result); encodeErr != nil {
				fmt.Fprintf(errOut, "Unable to write update result: %v\n", encodeErr)
				return 1
			}
			if err != nil {
				return 1
			}
		} else {
			fmt.Fprintf(out, "Automatic update checks: %s\n", args[1])
		}
		return 0
	}
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(errOut, "Usage: baseten-switch update check [--json] [--refresh] | update automatic on|off [--json]")
		return 2
	}
	jsonOutput, refresh := false, false
	for _, arg := range args[1:] {
		switch arg {
		case "--json":
			jsonOutput = true
		case "--refresh":
			refresh = true
		default:
			fmt.Fprintf(errOut, "Unknown update check flag: %s\n", arg)
			return 2
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := checker.Check(ctx, refresh)
	if jsonOutput {
		if encodeErr := json.NewEncoder(out).Encode(result); encodeErr != nil {
			fmt.Fprintf(errOut, "Unable to write update result: %v\n", encodeErr)
			return 1
		}
	} else {
		printUpdateResult(out, result)
	}
	if err != nil {
		return 1
	}
	return 0
}

func printUpdateResult(out io.Writer, result updates.Result) {
	if result.Error != "" {
		fmt.Fprintf(out, "Unable to check for updates: %s\n", result.Error)
	}
	if updates.IsNewer(result.AvailableVersion, result.CurrentVersion) {
		fmt.Fprintf(out, "Update available: %s (installed %s)\n", result.AvailableVersion, result.CurrentVersion)
		printUpgradeInstructions(out, result.InstallSource, result.ReleaseURL)
	} else {
		switch result.Status {
		case "current":
			if result.Error == "" {
				fmt.Fprintf(out, "Baseten Switch %s is up to date.\n", result.CurrentVersion)
			}
		case "disabled":
			fmt.Fprintln(out, "Automatic update checks are off. Run baseten-switch update check --refresh to check now.")
		case "unsupported":
			fmt.Fprintln(out, "Automatic release checks are unavailable for development or Preview builds.")
		default:
			if result.Error == "" {
				fmt.Fprintln(out, "No confirmed release information is available.")
			}
		}
	}
	if result.CheckedAt != nil {
		fmt.Fprintf(out, "Last checked: %s\n", result.CheckedAt.Format(time.RFC3339))
	}
}

func printUpgradeInstructions(out io.Writer, source, releaseURL string) {
	switch source {
	case "homebrew":
		fmt.Fprintln(out, "brew update && brew upgrade baseten-switch")
	case "nix":
		fmt.Fprintln(out, "nix profile upgrade --refresh baseten-switch")
	default:
		fmt.Fprintf(out, "Release and installation instructions: %s\n", releaseURL)
	}
	fmt.Fprintln(out, "When ready to adopt the new runtime: baseten-switch up (may interrupt active sessions)")
}

func updateNoticeEligible(args []string, interactive bool) bool {
	if !interactive || len(args) == 0 {
		return false
	}
	switch args[0] {
	case "status", "up", "doctor":
	default:
		return false
	}
	for _, arg := range args[1:] {
		key, _, _ := strings.Cut(arg, "=")
		if key == "--json" || key == "--help" || key == "-h" || key == "--uninstall" {
			return false
		}
	}
	return true
}

func maybePrintUpdateNotice(args []string) {
	if !updateNoticeEligible(args, isTerminal(os.Stdin) && isTerminal(os.Stdout) && isTerminal(os.Stderr)) {
		return
	}
	checker := newUpdateChecker()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, _ := checker.Check(ctx, false)
	if !result.AutomaticCheck || result.Status != "available" || !checker.TakeNotice(ctx, result.AvailableVersion) {
		return
	}
	fmt.Fprintf(os.Stderr, "\nUpdate available: %s. Run baseten-switch update check for upgrade instructions.\n", result.AvailableVersion)
}

func withUpdateNotice(args []string, code int) int {
	if code != 2 {
		maybePrintUpdateNotice(args)
	}
	return code
}
