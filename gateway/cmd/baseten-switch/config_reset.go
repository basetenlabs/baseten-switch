// config_reset.go implements the configuration reset:
// `baseten-switch config reset --yes` replaces the whole active gateway.yaml with
// config.InitTemplate. It does not merge or interpret the prior schema.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/door"
)

const configResetUsage = "usage: baseten-switch config reset --yes [--preview-root PATH --router-addr HOST:PORT --door-addr HOST:PORT]"

func cmdConfigReset(args []string) int {
	confirmed := false
	var previewPolicy config.PreviewPolicy
	previewFlags := 0
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--yes":
			if confirmed {
				fmt.Fprintf(os.Stderr, "%s (--yes supplied more than once)\n", configResetUsage)
				return 2
			}
			confirmed = true
		case arg == "--preview-root", arg == "--router-addr", arg == "--door-addr":
			if seen[arg] {
				fmt.Fprintf(os.Stderr, "%s supplied more than once\n", arg)
				return 2
			}
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				fmt.Fprintf(os.Stderr, "%s requires a value\n%s\n", arg, configResetUsage)
				return 2
			}
			seen[arg] = true
			previewFlags++
			i++
			switch arg {
			case "--preview-root":
				previewPolicy.Root = args[i]
			case "--router-addr":
				previewPolicy.RouterAddr = args[i]
			case "--door-addr":
				previewPolicy.DoorAddr = args[i]
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s (%s)\n", arg, configResetUsage)
			return 2
		}
	}
	if !confirmed {
		fmt.Fprintf(os.Stderr, "config reset replaces the entire active gateway.yaml and discards custom routing settings; confirmation required.\n%s\n", configResetUsage)
		return 2
	}
	if previewFlags != 0 && previewFlags != 3 {
		fmt.Fprintf(os.Stderr, "--preview-root, --router-addr, and --door-addr must be supplied together\n%s\n", configResetUsage)
		return 2
	}

	desired := append([]byte(nil), config.InitTemplate...)
	var desiredPreviewPolicy *config.PreviewPolicy
	if previewFlags == 3 {
		var canonical config.File
		if err := config.UnmarshalStrict(config.InitTemplate, &canonical); err != nil {
			fmt.Fprintf(os.Stderr, "config reset: parse canonical template: %v\n", err)
			return 1
		}
		preview, err := config.BuildPreviewConfig(&canonical, previewPolicy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config reset: %v\n", err)
			return 2
		}
		desired, err = config.Marshal(preview)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config reset: render Preview config: %v\n", err)
			return 1
		}
		desiredPreviewPolicy = &previewPolicy
	}
	path, notices := resolveConfigPath()
	for _, notice := range notices {
		fmt.Fprintln(os.Stderr, notice)
	}
	return runConfigResetDesired(path, desired, desiredPreviewPolicy, os.Stderr)
}

// runConfigReset performs the whole-file replacement. Tests call this with a
// temporary path; command dispatch resolves BASETEN_SWITCH_CONFIG_PATH, sticky runtime
// state, and the default path before entering here.
func runConfigReset(path string, out io.Writer) int {
	return runConfigResetDesired(path, config.InitTemplate, nil, out)
}

func runConfigResetDesired(
	path string,
	desired []byte,
	previewPolicy *config.PreviewPolicy,
	out io.Writer,
) int {
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		fmt.Fprintf(out, "config reset: %v\n", err)
		return 1
	}
	defer lock.close()
	if err := recoverInterruptedExactConfigCommit(path); err != nil {
		fmt.Fprintf(out, "config reset: recover interrupted exact config commit: %v\n", err)
		return 1
	}

	original, originalMode, err := readExactConfig(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "config reset: %s does not exist; run 'baseten-switch config init' first\n", path)
		} else {
			fmt.Fprintf(out, "config reset: read %s: %v\n", path, err)
		}
		return 1
	}
	if err := validateResetTemplate(path, desired, previewPolicy); err != nil {
		fmt.Fprintf(out, "config reset: canonical template is invalid: %v (this is a baseten-switch bug; no changes written)\n", err)
		return 1
	}

	originalHash := exactConfigHash(original)
	desiredHash := exactConfigHash(desired)
	routerRunning := false
	var before routingAdminStatus
	adminAddr := envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)
	switch state, pid := classifyPidfile(gatewayPidfilePath()); state {
	case pidfileAlive:
		status, statusErr := fetchRoutingAdminStatus(adminAddr)
		if statusErr != nil {
			fmt.Fprintf(out, "config reset: running router status is unavailable: %v; no changes written\n", statusErr)
			return 1
		}
		if err := validateMutationAdminStatus(status, path, originalHash); err != nil {
			fmt.Fprintf(out, "config reset: %v; no changes written\n", err)
			return 1
		}
		if err := validateManagedRouterIdentity(status, pid); err != nil {
			fmt.Fprintf(out, "config reset: %v; no changes written\n", err)
			return 1
		}
		routerRunning = true
		before = status
	}

	backupPath, err := writeUniqueResetBackup(path, original)
	if err != nil {
		fmt.Fprintf(out, "config reset: backup: %v\n", err)
		return 1
	}
	if err := compareAndSwapExactConfig(path, originalHash, desired, 0o600); err != nil {
		fmt.Fprintf(out, "config reset: install canonical config: %v; no replacement committed (backup %s)\n", err, backupPath)
		return 1
	}
	if err := validateInstalledReset(path, desired, desiredHash, previewPolicy); err != nil {
		restoreErr := compareAndSwapExactConfig(path, desiredHash, original, originalMode)
		if restoreErr != nil {
			fmt.Fprintf(out, "config reset: installed config failed validation: %v; prior config restore failed: %v (backup %s)\n", err, restoreErr, backupPath)
			return 1
		}
		fmt.Fprintf(out, "config reset: installed config failed validation: %v; restored prior exact bytes (backup %s)\n", err, backupPath)
		return 1
	}

	if !routerRunning {
		fmt.Fprintf(out, "reset %s to the canonical configuration; original saved to %s; applies at next router start\n", path, backupPath)
		return 0
	}

	_, signalErr := signalVerifiedRouter(before, adminAddr)
	if signalErr == nil {
		if _, ok := waitConfigHashActivation(adminAddr, path, desiredHash, before.activeToken(), true, routeApplyTimeout); ok {
			fmt.Fprintf(out, "reset %s to the canonical configuration; original saved to %s; active router confirmed\n", path, backupPath)
			return 0
		}
	}

	if restoreErr := compareAndSwapExactConfig(path, desiredHash, original, originalMode); restoreErr != nil {
		fmt.Fprintf(out, "config reset: activation failed and prior config restore failed: %v (backup %s)\n", restoreErr, backupPath)
		return 1
	}
	if rollbackErr := confirmOrReloadResetRollback(adminAddr, path, originalHash); rollbackErr != nil {
		cause := "router did not activate the canonical config"
		if signalErr != nil {
			cause = signalErr.Error()
		}
		fmt.Fprintf(out, "config reset: activation failed (%s); prior exact config restored on disk but active rollback is unconfirmed: %v (backup %s)\n", cause, rollbackErr, backupPath)
		return 1
	}
	fmt.Fprintf(out, "config reset: activation failed; restored and reactivated prior exact config (backup %s)\n", backupPath)
	return 1
}

func validateResetTemplate(
	configPath string,
	desired []byte,
	previewPolicy *config.PreviewPolicy,
) error {
	dir := filepath.Dir(configPath)
	file, err := os.CreateTemp(dir, "."+filepath.Base(configPath)+".reset-validation-*")
	if err != nil {
		return err
	}
	validationPath := file.Name()
	defer os.Remove(validationPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(desired); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return validateResetConfigFile(validationPath, previewPolicy)
}

func validateInstalledReset(
	path string,
	desired []byte,
	desiredHash string,
	previewPolicy *config.PreviewPolicy,
) error {
	written, mode, err := readExactConfig(path)
	if err != nil {
		return err
	}
	if mode != 0o600 {
		return fmt.Errorf("installed mode is %04o, want 0600", mode)
	}
	if exactConfigHash(written) != desiredHash || !bytes.Equal(written, desired) {
		return fmt.Errorf("installed bytes do not match the canonical template")
	}
	return validateResetConfigFile(path, previewPolicy)
}

func validateResetConfigFile(path string, previewPolicy *config.PreviewPolicy) error {
	file, err := config.Load(path)
	if err != nil {
		return err
	}
	if err := gateway.ValidateConfigFile(file); err != nil {
		return err
	}
	if _, err := door.SpecsFromConfig(file, door.SpecsOptions{Logf: func(string, ...any) {}}); err != nil {
		return fmt.Errorf("door section: %w", err)
	}
	if _, _, _, err := claudeDoorPort(file); err != nil {
		return fmt.Errorf("claude adapter door port: %w", err)
	}
	if previewPolicy != nil {
		if err := config.ValidatePreviewConfig(file, *previewPolicy); err != nil {
			return err
		}
	}
	return nil
}

// writeUniqueResetBackup creates a new adjacent 0600 backup with O_EXCL
// semantics. It never reuses or overwrites a prior backup.
func writeUniqueResetBackup(configPath string, original []byte) (string, error) {
	return writeUniqueConfigBackup(configPath, original, "pre-reset")
}

func writeUniqueConfigBackup(configPath string, original []byte, label string) (string, error) {
	dir := filepath.Dir(configPath)
	file, err := os.CreateTemp(dir, filepath.Base(configPath)+"."+label+"-*.bak")
	if err != nil {
		return "", err
	}
	path := file.Name()
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := file.Write(original); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := syncDirectory(dir); err != nil {
		return "", err
	}
	backedUp, mode, err := readExactConfig(path)
	if err != nil {
		return "", err
	}
	if mode != 0o600 || !bytes.Equal(backedUp, original) {
		return "", fmt.Errorf("backup verification failed")
	}
	remove = false
	return path, nil
}

func confirmOrReloadResetRollback(adminAddr, path, priorHash string) error {
	status, err := fetchRoutingAdminStatus(adminAddr)
	if err != nil {
		return fmt.Errorf("read router status after restore: %w", err)
	}
	state, pid := classifyPidfile(gatewayPidfilePath())
	if state != pidfileAlive {
		return fmt.Errorf("router is no longer running with a managed pid")
	}
	if err := validateManagedRouterIdentity(status, pid); err != nil {
		return err
	}
	if canonicalPath(status.ConfigPath) == canonicalPath(path) &&
		status.ActiveConfigHash == priorHash &&
		status.DesiredConfigHash == priorHash {
		return nil
	}
	previousToken := status.activeToken()
	if _, err := signalVerifiedRouter(status, adminAddr); err != nil {
		return fmt.Errorf("reload restored config: %w", err)
	}
	if _, ok := waitConfigHashActivation(adminAddr, path, priorHash, previousToken, true, routeApplyTimeout); !ok {
		return fmt.Errorf("router did not confirm the restored config")
	}
	return nil
}
