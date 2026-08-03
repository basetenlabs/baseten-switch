package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/launchd"
)

type uninstallOptions struct {
	dryRun bool
	purge  bool
	yes    bool
}

type uninstallStep struct {
	description string
	run         func() error
}

// cmdUninstall removes only the current Baseten Switch installation. It does
// not know about pre-release names or paths, and it never touches credentials
// owned by the Baseten CLI.
func cmdUninstall(args []string) int {
	opts, err := parseUninstallOptions(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return runUninstall(opts, defaultUninstallSteps(), os.Stderr)
}

func parseUninstallOptions(args []string) (uninstallOptions, error) {
	var opts uninstallOptions
	for _, arg := range args {
		switch arg {
		case "--dry-run":
			opts.dryRun = true
		case "--purge":
			opts.purge = true
		case "--yes":
			opts.yes = true
		default:
			return uninstallOptions{}, fmt.Errorf("unknown flag for uninstall: %s (usage: baseten-switch uninstall [--dry-run] [--purge --yes])", arg)
		}
	}
	if opts.yes && !opts.purge {
		return uninstallOptions{}, errors.New("uninstall: --yes is valid only with --purge")
	}
	if opts.purge && !opts.yes && !opts.dryRun {
		return uninstallOptions{}, errors.New("uninstall: --purge requires explicit confirmation with --yes")
	}
	return opts, nil
}

func runUninstall(opts uninstallOptions, steps []uninstallStep, out io.Writer) int {
	failures := 0
	for _, step := range steps {
		if opts.dryRun {
			fmt.Fprintf(out, "would %s\n", step.description)
			continue
		}
		if err := step.run(); err != nil {
			fmt.Fprintf(out, "uninstall: %s: %v\n", step.description, err)
			failures++
		}
	}
	if opts.purge {
		root := basetenSwitchDataRoot()
		if opts.dryRun {
			fmt.Fprintf(out, "would permanently remove current product data root %s\n", root)
		} else if err := purgeBasetenSwitchDataRoot(root); err != nil {
			fmt.Fprintf(out, "uninstall: purge %s: %v\n", root, err)
			failures++
		}
	}
	if opts.dryRun {
		fmt.Fprintln(out, "dry run complete; no changes made")
		return 0
	}
	if failures != 0 {
		fmt.Fprintf(out, "uninstall completed with %d incomplete step(s)\n", failures)
		return 1
	}
	if opts.purge {
		fmt.Fprintln(out, "Baseten Switch uninstalled and current product data purged")
	} else {
		fmt.Fprintln(out, "Baseten Switch uninstalled; config, secrets, telemetry, logs, and backups were retained")
	}
	return 0
}

func defaultUninstallSteps() []uninstallStep {
	root := basetenSwitchDataRoot()
	return []uninstallStep{
		{
			description: "restore Claude Code settings only when Baseten Switch ownership can be proven",
			run:         uninstallClaude,
		},
		{
			description: "restore or remove the Codex overlay only when Baseten Switch ownership can be proven",
			run:         uninstallCodex,
		},
		{
			description: "boot out and remove the current Baseten Switch launchd agents",
			run:         uninstallLaunchd,
		},
		{
			description: "stop the current Baseten Switch door and router",
			run:         uninstallProcesses,
		},
		{
			description: "quit the current Baseten Switch menubar app",
			run:         uninstallQuitMenubar,
		},
		{
			description: "remove current Baseten Switch runtime residue",
			run: func() error {
				return removeRuntimeResidue(root)
			},
		},
		{
			description: "verify the current Baseten Switch menubar app can be safely unregistered and removed, otherwise require manual removal",
			run:         uninstallManagedApp,
		},
	}
}

func uninstallClaude() error {
	settings := envDefault("BASETEN_SWITCH_CLAUDE_SETTINGS", homeJoin(".claude", "settings.json"))
	backupRoot := envDefault("BASETEN_SWITCH_BACKUP_DIR", filepath.Join(basetenSwitchDataRoot(), "backups"))
	backup := claudeBackupPath(backupRoot, settings)
	// Harness settings may intentionally be a symlink. The adapter restores
	// through a validated safe-file snapshot; the internal backup itself must
	// remain an ordinary file.
	if err := rejectSymlinkTargets(backup); err != nil {
		return err
	}
	if !pathExists(settings) && !pathExists(backup) {
		fmt.Fprintln(os.Stderr, "claude: no settings or backup to restore")
		return nil
	}
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		if !pathExists(backup) {
			fmt.Fprintf(os.Stderr, "claude: config unavailable; leaving settings unchanged because ownership cannot be proven (%v)\n", err)
			return nil
		}
		return fmt.Errorf("managed backup exists but ownership cannot be checked: %w", err)
	}
	return restoreWithRetainedBackup(backup, a.off)
}

func uninstallCodex() error {
	codexHome := envDefault("BASETEN_SWITCH_CODEX_HOME", homeJoin(".codex"))
	overlay := filepath.Join(codexHome, codexOverlayName)
	backupRoot := envDefault("BASETEN_SWITCH_BACKUP_DIR", filepath.Join(basetenSwitchDataRoot(), "backups"))
	backup := codexBackupPath(backupRoot, overlay)
	if err := rejectSymlinkTargets(overlay, backup); err != nil {
		return err
	}
	if !pathExists(overlay) && !pathExists(backup) {
		fmt.Fprintln(os.Stderr, "codex: no overlay or backup to restore")
		return nil
	}
	a, err := newCodexAdapterFromEnv()
	if err != nil {
		if !pathExists(backup) {
			fmt.Fprintf(os.Stderr, "codex: config unavailable; leaving overlay unchanged because ownership cannot be proven (%v)\n", err)
			return nil
		}
		return fmt.Errorf("managed backup exists but ownership cannot be checked: %w", err)
	}
	return restoreWithRetainedBackup(backup, a.off)
}

func restoreWithRetainedBackup(backupPath string, off func() int) error {
	var backup []byte
	if pathExists(backupPath) {
		var err error
		backup, err = os.ReadFile(backupPath)
		if err != nil {
			return fmt.Errorf("read backup before restore: %w", err)
		}
	}
	var errs []error
	if off() != 0 {
		errs = append(errs, errors.New("adapter restore failed"))
	}
	if backup != nil && !pathExists(backupPath) {
		retained, err := writeRetainedBackup(backupPath, backup)
		if err != nil {
			errs = append(errs, fmt.Errorf("retain consumed backup: %w", err))
		} else {
			fmt.Fprintf(os.Stderr, "uninstall: retained consumed backup at %s\n", retained)
		}
	}
	return errors.Join(errs...)
}

func writeRetainedBackup(activePath string, content []byte) (string, error) {
	base := activePath + ".uninstall-retained"
	for i := 0; ; i++ {
		path := base
		if i > 0 {
			path = fmt.Sprintf("%s.%d", base, i)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(content); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return "", err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return path, nil
	}
}

func rejectSymlinkTargets(paths ...string) error {
	for _, path := range paths {
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink %s", path)
		}
	}
	return nil
}

func uninstallLaunchd() error {
	if menubarGOOS != "darwin" {
		return nil
	}
	var errs []error
	for _, label := range []string{launchd.DoorLabel, launchd.RouterLabel} {
		if _, err := launchd.Bootout(launchdRunner, label); err != nil {
			errs = append(errs, fmt.Errorf("%s bootout: %w", label, err))
		}
		path := plistPathFor(label)
		if err := removeExactNonSymlink(path); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func uninstallProcesses() error {
	var doorAddrsResolved, routerAddrs []string
	if lc, err := resolveLifecycle(); err == nil {
		doorAddrsResolved = doorAddrs(lc.doorSpecs)
		routerAddrs = []string{lc.adminAddr}
	}
	var errs []error
	if stopComponent("door", doorPidfilePath(), doorAddrsResolved, doorHealthPath, doorHealthMarker) != 0 {
		errs = append(errs, errors.New("door did not stop cleanly"))
	}
	if stopComponent("router", gatewayPidfilePath(), routerAddrs, routerHealthPath, routerHealthMarker) != 0 {
		errs = append(errs, errors.New("router did not stop cleanly"))
	}
	return errors.Join(errs...)
}

func uninstallQuitMenubar() error {
	if menubarGOOS != "darwin" || !menubarRunning() {
		return nil
	}
	if err := quitMenubar(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "menubar: quit")
	return nil
}

// uninstallManagedApp removes the managed menubar app. The bundle's
// SMAppService login item can only be unregistered by the app itself,
// so bundles advertising the headless login-item control (the
// BasetenSwitchLoginItemCLI Info.plist marker written by
// build-menubar.sh) are driven with --unregister-login-item first and
// then removed. Bundles that predate the marker, fail the managed
// identity check, or reject the unregister keep the manual-removal
// requirement: a stale login item pointing at a deleted bundle is the
// failure mode this step exists to avoid.
func uninstallManagedApp() error {
	if menubarGOOS != "darwin" {
		return nil
	}
	appPath := homeJoin("Applications", menubarAppName)
	st, err := os.Lstat(appPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink %s", appPath)
	}
	if !st.IsDir() {
		return fmt.Errorf("refusing to remove %s: not an app bundle directory", appPath)
	}
	if err := verifyManagedAppBundle(appPath); err != nil {
		return err
	}
	if pid := menubarPid(); pid != 0 {
		return fmt.Errorf("the menubar app is still running (pid %d); quit it and rerun uninstall", pid)
	}
	if !managedAppSupportsLoginItemCLI(appPath) {
		return manualAppRemovalError(appPath, "the installed app predates CLI login-item control")
	}
	if out, err := runManagedAppCLI(appPath, menubarLoginItemCLIFlag); err != nil {
		return manualAppRemovalError(appPath, fmt.Sprintf("the app's login-item unregister failed (%v %s)", err, out))
	}
	// The executable we just ran is part of the bundle being removed.
	// Revalidate after it exits so a modified bundle is never deleted
	// under the managed-app identity established before execution.
	if err := verifyManagedAppBundle(appPath); err != nil {
		return err
	}
	if err := os.RemoveAll(appPath); err != nil {
		return fmt.Errorf("remove %s: %w", appPath, err)
	}
	fmt.Fprintf(os.Stderr, "menubar: removed %s\n", appPath)
	return nil
}

func manualAppRemovalError(appPath, reason string) error {
	return fmt.Errorf("manual action required: %s; open %s, turn off Start at Login, quit the app, then remove it by hand", reason, appPath)
}

// verifyManagedAppBundle proves the bundle at appPath satisfies the
// same complete integrity contract used at install time before the CLI
// drives its executable or removes it. Anything else at the managed
// path is left for manual removal.
func verifyManagedAppBundle(appPath string) error {
	if err := validateMaterializedMenubarApp(appPath); err != nil {
		return manualAppRemovalError(appPath,
			fmt.Sprintf("%s does not pass managed app validation (%v)", appPath, err))
	}
	return nil
}

// managedAppSupportsLoginItemCLI reports whether the installed bundle
// advertises the headless --unregister-login-item mode. Only such
// bundles can be driven to unregister their own SMAppService login
// item; anything older requires the manual path.
func managedAppSupportsLoginItemCLI(appPath string) bool {
	out, err := runCmd("/usr/bin/plutil", "-extract", "BasetenSwitchLoginItemCLI", "raw",
		filepath.Join(appPath, "Contents", "Info.plist"))
	return err == nil && out == "true"
}

// menubarLoginItemCLIFlag is the bundle's headless one-shot mode that
// unregisters the app's own SMAppService login item and exits.
const menubarLoginItemCLIFlag = "--unregister-login-item"

// runManagedAppCLI invokes the installed bundle's executable in a
// headless one-shot mode, bounded so a bundle that mishandles the flag
// (launches the UI instead of exiting) cannot hang the uninstall. A var
// so tests can stub the exec.
var runManagedAppCLI = func(appPath string, args ...string) (string, error) {
	exe := filepath.Join(appPath, "Contents", "MacOS", menubarProcName)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func basetenSwitchDataRoot() string {
	return homeJoin(".config", "baseten-switch")
}

func removeRuntimeResidue(root string) error {
	if err := validateBasetenSwitchDataRoot(root); err != nil {
		return err
	}
	var errs []error
	for _, name := range []string{"door.pid", "gateway.pid", "gateway.config-path"} {
		path := filepath.Join(root, name)
		if err := removeExactNonSymlink(path); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func purgeBasetenSwitchDataRoot(root string) error {
	if err := validateBasetenSwitchDataRoot(root); err != nil {
		return err
	}
	st, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink data root %s", root)
	}
	return os.RemoveAll(root)
}

func validateBasetenSwitchDataRoot(root string) error {
	want := filepath.Clean(homeJoin(".config", "baseten-switch"))
	if filepath.Clean(root) != want {
		return fmt.Errorf("refusing unexpected data root %s (expected %s)", root, want)
	}
	parent := filepath.Dir(want)
	if st, err := os.Lstat(parent); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing data root beneath symlink %s", parent)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func removeExactNonSymlink(path string) error {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to remove symlink")
	}
	if st.IsDir() {
		return errors.New("refusing to remove directory")
	}
	return os.Remove(path)
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
