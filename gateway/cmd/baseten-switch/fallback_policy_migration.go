package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

type fallbackPolicyMigrationVerificationOps struct {
	readInstalled func(string) ([]byte, os.FileMode, error)
	restore       func(string, string, []byte, os.FileMode) error
	readRestored  func(string) ([]byte, os.FileMode, error)
}

var defaultFallbackPolicyMigrationVerificationOps = fallbackPolicyMigrationVerificationOps{
	readInstalled: readExactConfig,
	restore:       compareAndSwapExactConfig,
	readRestored:  readExactConfig,
}

// migrateFallbackPolicyConfig writes the explicit Claude fallback target
// required by this binary before a new router process reads the config. It is
// intentionally a launcher migration, never a request-time router mutation.
func migrateFallbackPolicyConfig(path string) (bool, string, error) {
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		return false, "", err
	}
	defer lock.close()
	if err := recoverInterruptedExactConfigCommit(path); err != nil {
		return false, "", fmt.Errorf("recover interrupted exact config commit: %w", err)
	}
	if operationID, err := unfinishedMutationOperation(path); err != nil {
		return false, "", fmt.Errorf("inspect routing mutation state: %w", err)
	} else if operationID != "" {
		return false, "", fmt.Errorf("routing mutation %s must be reconciled before config migration", operationID)
	}
	prior, mode, err := readExactConfig(path)
	if err != nil {
		return false, "", err
	}
	file, err := config.Load(path)
	if err != nil {
		return false, "", err
	}
	needsMigration := false
	for _, client := range file.Clients {
		if client.Name == "claude-code" && client.FallbackRoute == "anthropic" && client.NativeFallbackModel == "" {
			needsMigration = true
			break
		}
	}
	if !needsMigration {
		return false, "", nil
	}
	desired, err := previewExactConfigEdit(path, prior, mode, func(editPath string) error {
		return config.SetClientNativeFallbackModel(editPath, "claude-code", config.DefaultClaudeNativeFallbackModel)
	})
	if err != nil {
		return false, "", fmt.Errorf("prepare fallback policy migration: %w", err)
	}
	if err := validateConfigBytesForRouter(path, desired, mode); err != nil {
		return false, "", fmt.Errorf("validate fallback policy migration: %w", err)
	}
	backup, err := writeUniqueConfigBackup(path, prior, "pre-fallback-policy")
	if err != nil {
		return false, "", fmt.Errorf("create fallback policy migration backup: %w", err)
	}
	if err := compareAndSwapExactConfig(path, exactConfigHash(prior), desired, mode); err != nil {
		return false, backup, fmt.Errorf("install fallback policy migration (backup retained at %s): %w", backup, err)
	}
	if err := verifyOrRestoreFallbackPolicyMigration(path, prior, desired, mode, backup, defaultFallbackPolicyMigrationVerificationOps); err != nil {
		return false, backup, err
	}
	return true, backup, nil
}

func verifyOrRestoreFallbackPolicyMigration(
	path string,
	prior, desired []byte,
	mode os.FileMode,
	backup string,
	ops fallbackPolicyMigrationVerificationOps,
) error {
	written, writtenMode, verificationErr := ops.readInstalled(path)
	if verificationErr == nil && exactConfigHash(written) == exactConfigHash(desired) && writtenMode.Perm() == mode.Perm() {
		return nil
	}
	if verificationErr == nil {
		verificationErr = fmt.Errorf("installed bytes or permissions differ from the validated migration")
	}

	if restoreErr := ops.restore(path, exactConfigHash(desired), prior, mode); restoreErr != nil {
		return fmt.Errorf(
			"verify fallback policy migration: %v; restore prior exact config failed: %v (backup retained at %s)",
			verificationErr, restoreErr, backup,
		)
	}
	restored, restoredMode, restoreVerificationErr := ops.readRestored(path)
	if restoreVerificationErr != nil || exactConfigHash(restored) != exactConfigHash(prior) || restoredMode.Perm() != mode.Perm() {
		if restoreVerificationErr == nil {
			restoreVerificationErr = fmt.Errorf("restored bytes or permissions differ from the prior exact config")
		}
		return fmt.Errorf(
			"verify fallback policy migration: %v; prior config restore could not be verified: %v (backup retained at %s)",
			verificationErr, restoreVerificationErr, backup,
		)
	}
	return fmt.Errorf(
		"verify fallback policy migration: %v; prior exact config restored (backup retained at %s)",
		verificationErr, backup,
	)
}

func migrateFallbackPolicyBeforeStart(path string, out *os.File) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	changed, backup, err := migrateFallbackPolicyConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		// Preserve the existing startup error path for a missing config. There
		// is nothing for this migration to change before the router validates it.
		return nil
	}
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintf(out, "config: added Claude fallback target %s (backup: %s)\n", config.DefaultClaudeNativeFallbackModel, backup)
	}
	return nil
}
