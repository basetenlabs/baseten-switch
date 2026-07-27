// Package launchd renders the baseten-switch user LaunchAgent plists and
// wraps the launchctl invocations behind a small Runner interface so
// the lifecycle commands (`up --install`, `up --uninstall`, supervised
// `down`) are testable without touching the real launchd domain
// (the lifecycle contract, "launchd supervision: up --install").
//
// Stdlib only, per the dependency policy: the plists are rendered from
// an embedded template string, not a plist library.
package launchd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The two supervised components. Homebrew's `brew services` uses its
// own homebrew.mxcl.* labels; install detection matches any label
// mentioning "baseten-switch" so the two supervision stories never fight
// over the same ports.
const (
	RouterLabel = "co.baseten.switch.router"
	DoorLabel   = "co.baseten.switch.door"
)

// ToggleBundleID is the menubar app's bundle identifier. Both agents
// set AssociatedBundleIdentifiers to it so System Settings Background
// Activity groups router, door, and app as one "Baseten Switch" entry once
// the app bundle ships (the lifecycle contract, background-items
// grouping); the key is harmless while the app is absent.
const ToggleBundleID = "co.baseten.switch"

// Job describes one LaunchAgent to render: RunAtLoad + KeepAlive with
// stdout/stderr appended to LogPath and Env pinned into launchd's
// (otherwise minimal) environment.
type Job struct {
	Label            string
	ProgramArguments []string
	Env              map[string]string
	LogPath          string
}

// RenderPlist renders a Job as a launchd property list. Env keys are
// emitted sorted so the output is deterministic.
func RenderPlist(j Job) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + xmlEscape(j.Label) + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range j.ProgramArguments {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>AssociatedBundleIdentifiers</key>
	<array>
		<string>` + ToggleBundleID + `</string>
	</array>
`)
	if len(j.Env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		keys := make([]string, 0, len(j.Env))
		for k := range j.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("\t\t<key>" + xmlEscape(k) + "</key>\n")
			b.WriteString("\t\t<string>" + xmlEscape(j.Env[k]) + "</string>\n")
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString(`	<key>StandardOutPath</key>
	<string>` + xmlEscape(j.LogPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(j.LogPath) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

func xmlUnescape(s string) string {
	r := strings.NewReplacer(
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&apos;", "'",
		"&amp;", "&", // last, so &amp;lt; round-trips
	)
	return r.Replace(s)
}

// ProgramBinary extracts the binary path (the first ProgramArguments
// entry) from a plist file. It only needs to parse RenderPlist's own
// output, not arbitrary plists; "" on a missing file or anything that
// does not look like one of ours. The lifecycle adoption path uses it
// to detect a LaunchAgent that would restart onto a different binary
// than the one running `up`.
func ProgramBinary(plistPath string) string {
	b, err := os.ReadFile(plistPath)
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.Index(s, "<key>ProgramArguments</key>")
	if i < 0 {
		return ""
	}
	s = s[i:]
	j := strings.Index(s, "<string>")
	if j < 0 {
		return ""
	}
	s = s[j+len("<string>"):]
	k := strings.Index(s, "</string>")
	if k < 0 {
		return ""
	}
	return xmlUnescape(strings.TrimSpace(s[:k]))
}

// Runner executes launchctl. The real implementation shells out; tests
// substitute a fake so no unit test ever touches the live launchd
// domain.
type Runner interface {
	// Run executes `launchctl args...` and returns its combined
	// output. A non-nil error means launchctl exited nonzero (the
	// output is still returned for classification).
	Run(args ...string) (string, error)
}

// ExecRunner shells out to launchctl.
type ExecRunner struct{}

func (ExecRunner) Run(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("launchctl %s: %v", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// GuiTarget is the per-user launchd domain target ("gui/<uid>").
func GuiTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// Loaded reports whether launchd has the label loaded in the gui
// domain (`launchctl print gui/<uid>/<label>` exits zero).
func Loaded(r Runner, label string) bool {
	_, err := r.Run("print", GuiTarget()+"/"+label)
	return err == nil
}

// Bootstrap loads a plist into the gui domain.
func Bootstrap(r Runner, plistPath string) error {
	out, err := r.Run("bootstrap", GuiTarget(), plistPath)
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// Bootout unloads a label from the gui domain. A label that is not
// loaded is tolerated (wasLoaded=false, nil error) so uninstall and
// repeated down calls are idempotent.
func Bootout(r Runner, label string) (wasLoaded bool, err error) {
	out, rerr := r.Run("bootout", GuiTarget()+"/"+label)
	if rerr == nil {
		return true, nil
	}
	low := strings.ToLower(out + " " + rerr.Error())
	if strings.Contains(low, "no such process") || strings.Contains(low, "could not find") {
		return false, nil
	}
	return false, fmt.Errorf("%v: %s", rerr, strings.TrimSpace(out))
}

// labelRe matches a bare reverse-DNS-ish launchd label token (no
// slashes, so plist file paths in the print output do not match).
var labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// LabelsMentioning extracts service labels belonging to the requested
// product namespace from
// `launchctl print gui/<uid>` output. It handles both the quoted
// (`"label" => { ... }`) and the columnar (`pid exit-status label`)
// service-list formats, deduplicates, and sorts. Over-matching is the
// safe direction here: any mention makes `up --install` refuse rather
// than double-supervise. The Homebrew formula uses "baseten-switch",
// while native agents and the app use the reverse-DNS namespace
// "co.baseten.switch".
func LabelsMentioning(out, needle string) []string {
	seen := map[string]bool{}
	var labels []string
	for _, line := range strings.Split(out, "\n") {
		for _, f := range strings.Fields(line) {
			f = strings.Trim(f, `"';,{}=><()`)
			matches := strings.Contains(f, needle)
			if needle == "baseten-switch" {
				matches = matches || strings.Contains(f, "co.baseten.switch")
			}
			if f == "" || !matches {
				continue
			}
			if !labelRe.MatchString(f) {
				continue
			}
			if !seen[f] {
				seen[f] = true
				labels = append(labels, f)
			}
		}
	}
	sort.Strings(labels)
	return labels
}

// StableProgramPath resolves this executable to the path a plist
// should reference across upgrades: symlinks are resolved, and a
// Homebrew Cellar path (which changes on every formula upgrade) is
// rewritten to the stable opt/ symlink (the lifecycle contract,
// Distribution scaling).
func StableProgramPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return RewriteCellar(exe), nil
}

// RewriteCellar maps .../Cellar/<formula>/<version>/<rest> to
// .../opt/<formula>/<rest>. Non-Cellar paths pass through unchanged.
func RewriteCellar(p string) string {
	const marker = "/Cellar/"
	i := strings.Index(p, marker)
	if i < 0 {
		return p
	}
	rest := strings.SplitN(p[i+len(marker):], "/", 3)
	if len(rest) < 3 {
		return p
	}
	return p[:i] + "/opt/" + rest[0] + "/" + rest[2]
}
