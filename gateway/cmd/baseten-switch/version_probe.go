// version_probe.go shares version-fetching and binary-resolution
// helpers between the status verbs (lifecycle.go) and doctor.go so both
// surfaces report the same per-component version, executable path, and
// menubar-binary skew signal. Stdlib only; no external dependencies.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// processExePath returns the executable path of the running pid via
// `ps -o comm= -p <pid>` on darwin. Empty on failure or non-darwin: a
// graceful blank rather than a hard error, matching the status rows'
// best-effort posture. The comm= format strips the column header so the
// output is just the path (ps may truncate it; that is acceptable for
// the skew smell, which is about the path prefix, not the full argv).
func processExePath(pid int) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", fmt.Sprintf("%d", pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// menubarBinaryCandidates returns the paths the menubar app would
// search for the baseten-switch binary, in documented lookup order
// (the menubar integration contract, BasetenSwitchState.swift locateBasetenSwitchBinary):
// $BASETEN_SWITCH_GATEWAY_BIN, ~/.local/bin/baseten-switch, then the two brew opt
// symlinks. $HOME-derived so tests can point ~/.local/bin at a temp
// dir; the brew paths are only probed when the caller passes them
// (tests constrain the list via the env seams so no real host binary
// is ever exec'd).
func menubarBinaryCandidates() []string {
	var out []string
	if v := os.Getenv("BASETEN_SWITCH_GATEWAY_BIN"); v != "" {
		out = append(out, v)
	}
	if p := menubarLocalBin(); p != "" {
		out = append(out, p)
	}
	return out
}

// menubarLocalBin returns the ~/.local/bin/baseten-switch candidate.
// Package var, like menubarBrewPaths, so the doctor fixture can swap
// it for a temp path and never probe (or exec) a real host binary.
var menubarLocalBin = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "bin", "baseten-switch")
}

// menubarBrewPaths are the stable Homebrew opt symlink paths for the
// binary. Package var so tests can swap them for temp dirs and never
// probe a real host brew install.
var menubarBrewPaths = []string{
	"/opt/homebrew/opt/baseten-switch/bin/baseten-switch",
	"/usr/local/opt/baseten-switch/bin/baseten-switch",
}

// resolveMenubarBinary walks the documented lookup order and returns
// the first existing executable, or "" when nothing resolves. The
// brew paths are appended after the env/home candidates.
func resolveMenubarBinary() string {
	for _, p := range append(menubarBinaryCandidates(), menubarBrewPaths...) {
		if isExecutable(p) {
			return p
		}
	}
	return ""
}

// isExecutable reports whether path exists and is executable by the
// current user. Mirrors FileManager.isExecutableFile in the Swift
// menubar (the documented lookup contract).
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// menubarVersionTimeout bounds the "<resolved> --version" probe.
var menubarVersionTimeout = 2 * time.Second

// basetenPATH returns the PATH to scan for baseten CLI installs.
// Package var, like menubarLocalBin, so the doctor fixture can swap it
// for temp dirs and never probe (or exec) a real host binary.
var basetenPATH = func() string { return os.Getenv("PATH") }

// basetenCLIVersion runs "<path> --version" with a short timeout and
// returns the first output line, trimmed (e.g. "baseten 0.2.0"). Empty
// on any failure. Package var so tests only ever exec their own fake
// scripts.
var basetenCLIVersion = func(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), menubarVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}

// scanBasetenCLIs walks every basetenPATH dir and returns the distinct
// baseten executables in PATH order. Distinct means distinct files:
// symlinked duplicates of one install (brew opt + bin) collapse to a
// single entry, reported under the first PATH-visible path, so only
// genuinely separate installs count. Relative PATH entries are skipped,
// matching exec.LookPath's ErrDot policy: the scan's results are exec'd
// for --version, and a relative entry resolves against doctor's cwd, so
// an untrusted checkout shipping bin/baseten would otherwise run with
// the user's privileges.
func scanBasetenCLIs() []string {
	seen := map[string]bool{}
	var out []string
	for _, dir := range filepath.SplitList(basetenPATH()) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, "baseten")
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
			continue
		}
		key := p
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			key = resolved
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// menubarBinaryVersion runs "<resolved> --version" with a short timeout
// and returns the version string it prints. The --version output is a
// single line "baseten-switch vX.Y.Z" (or "baseten-switch dev"); we return the
// last whitespace token, which is the version itself. Empty on any
// failure (binary missing, timeout, non-zero exit, no parseable token).
func menubarBinaryVersion(resolved string) string {
	if resolved == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), menubarVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "--version")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return ""
	}
	// "baseten-switch v0.2.0" -> "v0.2.0"; "baseten-switch dev" -> "dev".
	fields := strings.Fields(line)
	return fields[len(fields)-1]
}
