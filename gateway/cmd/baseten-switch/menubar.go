// menubar.go implements the menubar-app half of the lifecycle: the
// `baseten-switch menubar` verb plus the whole-system up/down steps that
// drive the app to running-and-current. The app copy lives in
// ~/Applications (which makes it Spotlight-indexable; symlinked apps
// are not indexed reliably) and is refreshed whenever the source
// bundle's version differs, so brew upgrades propagate on the next
// `baseten-switch menubar` or `baseten-switch up`.
//
// Resolution order for the source bundle: $BASETEN_SWITCH_MENUBAR_APP (opened in
// place, no copy; the dev escape hatch), then the nested app ZIP in the
// running binary's Homebrew pkgshare. With no source and no existing
// ~/Applications copy, the verb errors with the install hint; `up`
// skips instead, because the app is optional UX and must never fail a
// server bring-up. Process probes cannot distinguish development builds:
// up/down quit any BasetenSwitch instance the current user runs.
// BASETEN_SWITCH_MENUBAR_APP (no keg copy touched) and
// BASETEN_SWITCH_MENUBAR=off isolate development sessions.
package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const menubarAppName = "Baseten Switch.app"
const menubarPayloadName = menubarAppName + ".zip"

// menubarProcName is the executable name inside the bundle; the
// pgrep/pkill probes match it exactly (-x) so no other process can be
// mistaken for the app.
const menubarProcName = "BasetenSwitch"

// menubarKegPaths is a test seam for the pre-payload bundle source.
// Production releases resolve the nested ZIP from the running
// executable's Homebrew Cellar prefix. Keeping the seam lets the
// lifecycle tests exercise copy/version behavior without constructing
// signed macOS bundles.
var menubarKegPaths []string

// menubarExecutable is an os.Executable seam for Homebrew-prefix tests.
var menubarExecutable = os.Executable

// menubarGOOS gates the darwin-only paths. Package var, like
// menubarLocalBin, so tests can drive the non-darwin skip without a
// cross-compile.
var menubarGOOS = runtime.GOOS

// runCmd is an exec seam so tests can fake
// open/ditto/plutil/codesign/pgrep/pkill.
var runCmd = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// menubarDisabled turns off the menubar steps of up and down (never
// the explicit `menubar` verb, which is direct user intent). check.sh
// scratch runs and the unit tests set it so no run ever quits or
// relaunches the user's real menubar app, mirroring BASETEN_SWITCH_LAUNCHD=off.
func menubarDisabled() bool { return os.Getenv("BASETEN_SWITCH_MENUBAR") == "off" }

// menubarInstallHint is the one-line remediation for a machine with no
// bundle to open, shared by errMenubarAppMissing and the status row so
// the two never drift.
const menubarInstallHint = "brew install basetenlabs/baseten/baseten-switch"

// errMenubarAppMissing marks the no-source-no-copy case so `up` can
// skip it (the app is optional) while real refresh failures stay loud.
var errMenubarAppMissing = errors.New("app not found. Install it with '" + menubarInstallHint + "' (or set BASETEN_SWITCH_MENUBAR_APP to a built bundle)")

// errMenubarKegWithoutApp marks a Homebrew keg that exists but carries
// no app payload. errMenubarAppMissing's hint would tell those users to
// brew-install what they just installed. `up` skips it the same way
// (the app is optional UX); menubarKegWithoutAppErr wraps it with the
// keg path that was checked.
var errMenubarKegWithoutApp = errors.New("the installed keg has no menubar app bundle")

// menubarUpgradeHint is the one-line remediation for the
// keg-without-app state, shared by menubarKegWithoutAppErr and the
// status row so the two never drift.
const menubarUpgradeHint = "brew upgrade baseten-switch"

func menubarKegWithoutAppErr(kegDir string) error {
	return fmt.Errorf("%w (checked %s). Upgrade with '%s' to get a release that includes %s", errMenubarKegWithoutApp, kegDir, menubarUpgradeHint, menubarPayloadName)
}

// menubarKegDirWithoutApp returns the Homebrew keg directory that
// exists on this machine, "" when none does. Callers consult it only
// after every payload/copy probe came up empty, so a non-empty result
// means the package is installed but ships no nested app ZIP.
func menubarKegDirWithoutApp() string {
	for _, p := range menubarKegPaths {
		kegDir := filepath.Dir(p)
		if st, err := os.Stat(kegDir); err == nil && st.IsDir() {
			return kegDir
		}
	}
	_, prefix, homebrew := homebrewMenubarPayload()
	if homebrew {
		return prefix
	}
	return ""
}

// homebrewMenubarPayload derives the formula's nested app payload from
// this binary rather than assuming a Homebrew installation root. Both
// /opt/homebrew and /usr/local work because Homebrew's bin symlink is
// resolved to:
//
//	.../Cellar/baseten-switch/<version>/bin/baseten-switch
//
// The bool reports whether the executable has that exact Homebrew
// shape. A recognized prefix is returned even when the payload is
// absent, so callers can distinguish a broken/old formula from a source
// build that simply has no optional app.
func homebrewMenubarPayload() (payload, prefix string, homebrew bool) {
	executable, err := menubarExecutable()
	if err != nil {
		return "", "", false
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", "", false
	}
	if filepath.Base(executable) != "baseten-switch" || filepath.Base(filepath.Dir(executable)) != "bin" {
		return "", "", false
	}
	prefix = filepath.Dir(filepath.Dir(executable))
	formulaDir := filepath.Dir(prefix)
	if filepath.Base(formulaDir) != "baseten-switch" || filepath.Base(filepath.Dir(formulaDir)) != "Cellar" {
		return "", "", false
	}
	return filepath.Join(prefix, "share", "baseten-switch", menubarPayloadName), prefix, true
}

// menubarTarget is the resolved bundle the menubar verb and the up
// step operate on.
type menubarTarget struct {
	path       string // bundle to open
	managed    bool   // path is the ~/Applications copy (not the BASETEN_SWITCH_MENUBAR_APP override)
	refreshed  bool   // the ~/Applications copy was replaced this run
	oldVersion string // the copy's bundle version before the refresh ("" = no prior copy)
}

// resolveMenubarTarget locates the bundle to open and refreshes the
// ~/Applications copy when the source keg carries a different bundle
// version. Shared by cmdMenubar and upMenubar so the two never drift
// on resolution order or refresh rules.
func resolveMenubarTarget() (menubarTarget, error) {
	if v := os.Getenv("BASETEN_SWITCH_MENUBAR_APP"); v != "" {
		if _, err := os.Stat(v); err != nil {
			return menubarTarget{}, fmt.Errorf("BASETEN_SWITCH_MENUBAR_APP=%s does not exist", v)
		}
		return menubarTarget{path: v}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return menubarTarget{}, err
	}
	dst := filepath.Join(home, "Applications", menubarAppName)

	src := ""
	materialized := false
	cleanup := func() {}
	for _, p := range menubarKegPaths {
		if _, err := os.Stat(p); err == nil {
			src = p
			break
		}
	}
	if src == "" && len(menubarKegPaths) == 0 {
		payload, _, homebrew := homebrewMenubarPayload()
		if homebrew {
			if _, statErr := os.Lstat(payload); statErr == nil {
				src, cleanup, err = materializeMenubarPayload(payload, filepath.Dir(dst))
				if err != nil {
					return menubarTarget{}, err
				}
				defer cleanup()
				materialized = true
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return menubarTarget{}, fmt.Errorf("reading menubar payload %s: %w", payload, statErr)
			}
		}
	}
	if src == "" {
		if _, err := os.Stat(dst); err == nil {
			return menubarTarget{path: dst, managed: true}, nil
		}
		// A keg dir that exists with no app inside is not a missing
		// install: the brew-install hint would point the user at the
		// package they already have (pre-universal releases on Intel).
		if kegDir := menubarKegDirWithoutApp(); kegDir != "" {
			return menubarTarget{}, menubarKegWithoutAppErr(kegDir)
		}
		return menubarTarget{}, errMenubarAppMissing
	}

	if !needsMenubarRefresh(src, dst) {
		return menubarTarget{path: dst, managed: true}, nil
	}
	old := bundleVersion(dst)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return menubarTarget{}, err
	}
	if materialized {
		if err := activateMenubarBundle(src, dst); err != nil {
			return menubarTarget{}, err
		}
		now := time.Now()
		_ = os.Chtimes(dst, now, now)
		fmt.Fprintf(os.Stderr, "menubar: installed %s (version %s)\n", dst, bundleVersion(dst))
		return menubarTarget{path: dst, managed: true, refreshed: true, oldVersion: old}, nil
	}
	if err := os.RemoveAll(dst); err != nil {
		return menubarTarget{}, fmt.Errorf("replacing %s: %v", dst, err)
	}
	// ditto preserves the bundle structure, metadata, and the
	// code signature, unlike a plain recursive copy.
	if out, err := runCmd("/usr/bin/ditto", src, dst); err != nil {
		return menubarTarget{}, fmt.Errorf("copy to %s failed: %v %s", dst, err, out)
	}
	// ditto also preserves the source mtimes, so stamp the bundle root
	// with the install time: menubarInstanceStale compares a running
	// instance's start time against it, which is what lets a later up
	// converge an instance that outlived the refresh that replaced it.
	now := time.Now()
	_ = os.Chtimes(dst, now, now)
	fmt.Fprintf(os.Stderr, "menubar: installed %s (version %s)\n", dst, bundleVersion(dst))
	return menubarTarget{path: dst, managed: true, refreshed: true, oldVersion: old}, nil
}

// materializeMenubarPayload preflights the ZIP before asking ditto to
// preserve the bundle metadata and signature during extraction. The
// result lives in a private staging directory beside the destination,
// so activation can use same-filesystem renames.
func materializeMenubarPayload(payload, appDir string) (app string, cleanup func(), err error) {
	if err := preflightMenubarPayload(payload); err != nil {
		return "", func() {}, err
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return "", func() {}, err
	}
	stage, err := os.MkdirTemp(appDir, ".baseten-switch-menubar.")
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(stage) }
	fail := func(format string, args ...any) (string, func(), error) {
		cleanup()
		return "", func() {}, fmt.Errorf(format, args...)
	}
	if out, extractErr := runCmd("/usr/bin/ditto", "-x", "-k", payload, stage); extractErr != nil {
		return fail("extracting %s failed: %v %s", payload, extractErr, out)
	}
	app = filepath.Join(stage, menubarAppName)
	if err := validateMaterializedMenubarApp(app); err != nil {
		return fail("validating %s: %v", payload, err)
	}
	return app, cleanup, nil
}

// preflightMenubarPayload rejects path traversal, symlinks, and
// unrelated top-level content before extraction. __MACOSX entries are
// permitted because ditto uses them for AppleDouble metadata.
func preflightMenubarPayload(payload string) error {
	st, err := os.Lstat(payload)
	if err != nil {
		return fmt.Errorf("reading menubar payload %s: %w", payload, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("menubar payload %s is not a regular file", payload)
	}
	zr, err := zip.OpenReader(payload)
	if err != nil {
		return fmt.Errorf("opening menubar payload %s: %w", payload, err)
	}
	defer zr.Close()
	if len(zr.File) == 0 {
		return fmt.Errorf("menubar payload %s is empty", payload)
	}
	appPrefix := menubarAppName + "/"
	hasPlist, hasExecutable := false, false
	for _, file := range zr.File {
		name := file.Name
		clean := path.Clean(name)
		if clean == "." || path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("menubar payload contains unsafe path %q", name)
		}
		if clean != menubarAppName && !strings.HasPrefix(clean, appPrefix) && clean != "__MACOSX" && !strings.HasPrefix(clean, "__MACOSX/") {
			return fmt.Errorf("menubar payload contains unexpected path %q", name)
		}
		if file.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("menubar payload contains symlink %q", name)
		}
		switch clean {
		case menubarAppName + "/Contents/Info.plist":
			hasPlist = true
		case menubarAppName + "/Contents/MacOS/" + menubarProcName:
			hasExecutable = true
		}
	}
	if !hasPlist || !hasExecutable {
		return fmt.Errorf("menubar payload does not contain the expected app structure")
	}
	return nil
}

func validateMaterializedMenubarApp(app string) error {
	plistPath := filepath.Join(app, "Contents", "Info.plist")
	executablePath := filepath.Join(app, "Contents", "MacOS", menubarProcName)
	for _, p := range []string{plistPath, executablePath} {
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("%s is missing or is not a regular file", p)
		}
	}
	if st, _ := os.Stat(executablePath); st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", executablePath)
	}
	fields := []struct {
		key, want string
	}{
		{"CFBundleIdentifier", "co.baseten.switch"},
		{"CFBundleDisplayName", "Baseten Switch"},
		{"CFBundleExecutable", menubarProcName},
	}
	for _, field := range fields {
		out, err := runCmd("/usr/bin/plutil", "-extract", field.key, "raw", plistPath)
		if err != nil || out != field.want {
			return fmt.Errorf("unexpected %s %q", field.key, out)
		}
	}
	if bundleVersion(app) == "" {
		return errors.New("app bundle version is missing")
	}
	if out, err := runCmd("/usr/bin/codesign", "--verify", "--deep", "--strict", app); err != nil {
		return fmt.Errorf("app code signature is invalid: %v %s", err, out)
	}
	return nil
}

// activateMenubarBundle replaces the managed copy with same-filesystem
// renames and restores the prior copy if activation fails.
func activateMenubarBundle(src, dst string) error {
	backup := filepath.Join(filepath.Dir(src), "previous-"+menubarAppName)
	hadDestination := false
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Rename(dst, backup); err != nil {
			return fmt.Errorf("backing up %s: %w", dst, err)
		}
		hadDestination = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", dst, err)
	}
	if err := os.Rename(src, dst); err != nil {
		if hadDestination {
			_ = os.Rename(backup, dst)
		}
		return fmt.Errorf("activating %s: %w", dst, err)
	}
	return nil
}

func cmdMenubar(args []string) int {
	// --which prints the baseten-switch binary the menubar app will resolve
	// for its shell-outs (the documented lookup order shared with
	// status and doctor via version_probe.go). Machine-readable so
	// tooling asks the binary instead of re-deriving the order.
	if len(args) == 1 && args[0] == "--which" {
		p := resolveMenubarBinary()
		if p == "" {
			fmt.Fprintln(os.Stderr, "menubar: no baseten-switch binary found in the menubar lookup order")
			return 1
		}
		fmt.Println(p)
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch menubar [--which]")
		return 2
	}
	if menubarGOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "menubar: the menubar app is macOS-only")
		return 1
	}
	tgt, err := resolveMenubarTarget()
	if err != nil {
		fmt.Fprintf(os.Stderr, "menubar: %v\n", err)
		return 1
	}
	// A stale running instance must be quit first, exactly like the up
	// step: `open` on a running app only activates the old instance
	// (the app's single-instance guard exits the new copy), so without
	// the quit a refreshed copy would never take over and the next up
	// would misread the survivor as current.
	if pid := menubarPid(); pid != 0 && menubarStale(tgt, pid) {
		if err := quitMenubar(); err != nil {
			fmt.Fprintf(os.Stderr, "menubar: %v\n", err)
			return 1
		}
	}
	return openApp(tgt.path)
}

// upMenubar drives the menubar app to running-and-current, the final
// step of a whole-system up (darwin only; a silent skip elsewhere and
// under BASETEN_SWITCH_MENUBAR=off). Not running: open the freshly refreshed
// ~/Applications copy. Running stale (per menubarStale): quit it
// gracefully and open the refreshed copy; open(1) detaches, so an `up`
// invoked from inside the app's own shell-out survives the quit.
// Running and current: no-op, which is what makes the app's own
// "Start Baseten Switch" action (it runs `up`) safe against self-kill
// loops.
func upMenubar() int {
	if menubarGOOS != "darwin" || menubarDisabled() {
		return 0
	}
	tgt, err := resolveMenubarTarget()
	if err != nil {
		if errors.Is(err, errMenubarAppMissing) || errors.Is(err, errMenubarKegWithoutApp) {
			// The app is optional UX; a machine without the brew keg
			// (a source build) or with a keg that ships no bundle (a
			// pre-universal release on Intel) must not fail the server
			// bring-up.
			fmt.Fprintf(os.Stderr, "menubar: %v; skipping\n", err)
			return 0
		}
		fmt.Fprintf(os.Stderr, "menubar: %v\n", err)
		return 1
	}
	pid := menubarPid()
	if pid == 0 {
		return openApp(tgt.path)
	}
	if !menubarStale(tgt, pid) {
		fmt.Fprintln(os.Stderr, "menubar: running and current; leaving it alone")
		return 0
	}
	if tgt.refreshed {
		was := tgt.oldVersion
		if was == "" {
			was = "unknown"
		}
		fmt.Fprintf(os.Stderr, "menubar: adopting %s (was %s)\n", bundleVersionLabel(tgt.path), was)
	} else {
		fmt.Fprintf(os.Stderr, "menubar: adopting %s (running instance predates the installed copy)\n", bundleVersionLabel(tgt.path))
	}
	if err := quitMenubar(); err != nil {
		fmt.Fprintf(os.Stderr, "menubar: %v\n", err)
		return 1
	}
	return openApp(tgt.path)
}

// menubarStale reports whether the running instance (pid) must be quit
// and relaunched to converge on tgt. Two signals: this run replaced
// the ~/Applications copy (the running instance is by definition on
// the replaced version), or the instance started before the copy was
// installed. The second signal is what survives across invocations:
// the disk copy can be current while an old instance keeps running
// (a `baseten-switch menubar` whose open only activated the survivor, an
// up whose quit timed out), and the refresh-this-run bit alone would
// misread that state as running-and-current forever.
func menubarStale(tgt menubarTarget, pid int) bool {
	if tgt.refreshed {
		return true
	}
	return tgt.managed && menubarInstanceStale(pid, tgt.path)
}

// menubarInstanceStale reports whether pid started before the bundle
// at path was installed (the bundle root mtime, stamped by the refresh
// in resolveMenubarTarget). Unknowns read as not-stale: quitting the
// app on a probe failure would break the running-and-current no-op
// that keeps the app's own `up` shell-out safe.
func menubarInstanceStale(pid int, path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	out, err := runCmd("/bin/ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	if err != nil {
		return false
	}
	// lstart is locale-C "Mon Jan  2 15:04:05 2006" with a padded day;
	// collapse the whitespace instead of matching the padding.
	started, perr := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(out), " "), time.Local)
	if perr != nil {
		return false
	}
	return started.Before(st.ModTime())
}

// downMenubar quits a running menubar app as part of a whole-system
// down (darwin only). A not-running app is a silent no-op so down
// stays quiet on machines that never launched it.
func downMenubar() int {
	if menubarGOOS != "darwin" || menubarDisabled() {
		return 0
	}
	if !menubarRunning() {
		return 0
	}
	if err := quitMenubar(); err != nil {
		fmt.Fprintf(os.Stderr, "menubar: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "menubar: quit")
	return 0
}

// menubarPid returns the pid of a running BasetenSwitch process owned by
// the current user, 0 when none (pgrep -x exits nonzero when nothing
// matches). The -U scope matters on multi-user machines: an unscoped
// match would read another user's instance as ours, and pkill could
// never signal it, so up/down would wait on a quit that cannot happen.
func menubarPid() int {
	out, err := runCmd("/usr/bin/pgrep", "-U", strconv.Itoa(os.Getuid()), "-x", menubarProcName)
	if err != nil {
		return 0
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0
	}
	pid, perr := strconv.Atoi(fields[0])
	if perr != nil {
		return 0
	}
	return pid
}

func menubarRunning() bool { return menubarPid() != 0 }

// menubarInstalled reports whether any bundle exists for the lifecycle
// to open: the BASETEN_SWITCH_MENUBAR_APP override, a Homebrew keg, or the
// ~/Applications copy. Pure stat probes, deliberately NOT
// resolveMenubarTarget: that helper refreshes the ~/Applications copy
// as a side effect, and callers of this one (the status row) must stay
// read-only.
func menubarInstalled() bool {
	if v := os.Getenv("BASETEN_SWITCH_MENUBAR_APP"); v != "" {
		_, err := os.Stat(v)
		return err == nil
	}
	for _, p := range menubarKegPaths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if len(menubarKegPaths) == 0 {
		if payload, _, homebrew := homebrewMenubarPayload(); homebrew {
			if st, err := os.Lstat(payload); err == nil && st.Mode().IsRegular() {
				return true
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, "Applications", menubarAppName))
	return err == nil
}

// menubarBundlePath returns the app bundle a running instance was
// launched from, derived from the process table: ps comm= reports the
// executable (".../Baseten Switch.app/Contents/MacOS/BasetenSwitch"), and the
// bundle is three levels up. Empty when the probe fails or the
// executable does not live inside an app bundle (a bare swift-build
// binary).
func menubarBundlePath(pid int) string {
	out, err := runCmd("/bin/ps", "-o", "comm=", "-p", strconv.Itoa(pid))
	if err != nil || out == "" {
		return ""
	}
	macos := filepath.Dir(out)
	contents := filepath.Dir(macos)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" {
		return ""
	}
	return filepath.Dir(contents)
}

// menubarStatusLines renders the Menubar rows for `status`: nil when
// the row does not apply (off-darwin, or BASETEN_SWITCH_MENUBAR=off), otherwise
// the state line first (up, DOWN, or not installed) and indented
// detail lines after, matching the other component rows. Read-only
// probes over the runCmd seam (pgrep for the pid, ps for the bundle
// path and start time, plutil for the bundle version) plus stat-only
// install checks; unlike the up/down steps it never opens, quits, or
// refreshes anything.
//
// The menubar app is optional UX, so the DOWN row is informational
// only: printStatus never counts it against the exit code (see the
// comment there). The row gives the operator a repair hint without making
// the optional app part of server health.
func menubarStatusLines() []string {
	if menubarGOOS != "darwin" || menubarDisabled() {
		return nil
	}
	pid := menubarPid()
	if pid == 0 {
		// "fix: baseten-switch up" is a dead end when there is no bundle to
		// open (upMenubar skips errMenubarAppMissing with exit 0), so
		// the not-installed state gets the install hint instead of a
		// permanently unfixable DOWN row (darwin source builds).
		if !menubarInstalled() {
			// Same distinction resolveMenubarTarget draws: a keg with
			// no bundle inside is not a missing install, and the
			// brew-install hint would point the user at the package
			// they already have (a pre-universal release on an Intel
			// Mac); only an upgrade puts an app in the keg. Skipped
			// under BASETEN_SWITCH_MENUBAR_APP, which bypasses the keg entirely
			// (menubarInstalled already answered for the override).
			if os.Getenv("BASETEN_SWITCH_MENUBAR_APP") == "" {
				if kegDir := menubarKegDirWithoutApp(); kegDir != "" {
					return []string{"not installed (keg " + kegDir + " has no app bundle; upgrade: " + menubarUpgradeHint + ")"}
				}
			}
			return []string{"not installed (install: " + menubarInstallHint + ")"}
		}
		return []string{"DOWN (fix: baseten-switch up)"}
	}
	bundle := menubarBundlePath(pid)
	if bundle == "" {
		// Running, but the bundle cannot be derived (a bare binary or
		// a ps failure): report what is known.
		return []string{fmt.Sprintf("up (pid %d)", pid)}
	}
	ver := bundleVersion(bundle)
	label := ver
	if label == "" {
		label = "unknown"
	}
	// The version above is read from the bundle on DISK, which is not
	// what the process runs when the instance outlived a refresh (an up
	// whose quit timed out, a `menubar` whose open only activated the
	// survivor). menubarInstanceStale is the same start-time-vs-install
	// signal the up step converges on; without it the row would report
	// the refreshed copy's version for the old code still running, the
	// exact skew this row exists to surface.
	if menubarInstanceStale(pid, bundle) {
		return []string{
			fmt.Sprintf("up (pid %d) STALE: instance predates the installed copy (%s on disk; fix: baseten-switch up)", pid, label),
			bundle,
		}
	}
	lines := []string{fmt.Sprintf("up (pid %d) %s", pid, label), bundle}
	// An unreadable bundle version is a probe failure, not known skew;
	// only a readable, different version gets the skew line (mirroring
	// the web row's unknown-version guard).
	if ver != "" {
		if skew := versionSkewLine(ver); skew != "" {
			lines = append(lines, skew)
		}
	}
	return lines
}

// menubarQuitTimeout bounds the wait for a quit instance to leave the
// process table. Package var so tests can shorten it.
var menubarQuitTimeout = 5 * time.Second

// quitMenubar sends the graceful TERM (pkill's default) and waits for
// the process to be gone before returning: opening the refreshed copy
// while the old instance is still dying trips the app's
// single-instance guard (the new instance sees the old one and exits),
// which would leave the stale instance in place.
func quitMenubar() error {
	out, err := runCmd("/usr/bin/pkill", "-U", strconv.Itoa(os.Getuid()), "-x", menubarProcName)
	deadline := time.Now().Add(menubarQuitTimeout)
	for time.Now().Before(deadline) {
		if !menubarRunning() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("quit %s failed: %v %s", menubarProcName, err, out)
	}
	return fmt.Errorf("%s still running %s after quit", menubarProcName, menubarQuitTimeout)
}

// needsMenubarRefresh reports whether dst is absent or carries a
// different bundle version than src. A source whose version cannot be
// read is a loud skip, never a refresh: the freshly copied dst would
// be version-less too, so every subsequent invocation would read as
// needing a refresh and quit-and-relaunch the running app forever,
// breaking the running-and-current no-op.
func needsMenubarRefresh(src, dst string) bool {
	if _, err := os.Stat(dst); err != nil {
		return true
	}
	sv, dv := bundleVersion(src), bundleVersion(dst)
	if sv == "" {
		fmt.Fprintf(os.Stderr, "menubar: cannot read the bundle version of %s; not refreshing the %s copy\n", src, dst)
		return false
	}
	return sv != dv
}

// bundleVersionLabel is bundleVersion with a printable fallback for
// report lines.
func bundleVersionLabel(app string) string {
	if v := bundleVersion(app); v != "" {
		return v
	}
	return "unknown"
}

// bundleVersion reads CFBundleShortVersionString from the app bundle,
// empty on any error.
func bundleVersion(app string) string {
	out, err := runCmd("/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw",
		filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return ""
	}
	return out
}

func openApp(path string) int {
	if out, err := runCmd("/usr/bin/open", path); err != nil {
		fmt.Fprintf(os.Stderr, "menubar: open %s failed: %v %s\n", path, err, out)
		return 1
	}
	fmt.Fprintf(os.Stderr, "menubar: opened %s\n", path)
	return 0
}
