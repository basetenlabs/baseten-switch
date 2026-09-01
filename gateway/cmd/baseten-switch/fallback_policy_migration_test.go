package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func TestMigrateFallbackPolicyConfigAddsExplicitClaudeTargetOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	prior := bytes.Replace(config.InitTemplate,
		[]byte("    native_fallback_model: "+config.DefaultClaudeNativeFallbackModel+"\n"), nil, 1)
	if bytes.Equal(prior, config.InitTemplate) {
		t.Fatal("test fixture did not remove native_fallback_model")
	}
	if err := os.WriteFile(path, prior, 0o640); err != nil {
		t.Fatal(err)
	}
	changed, backup, err := migrateFallbackPolicyConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || backup == "" {
		t.Fatalf("changed=%t backup=%q", changed, backup)
	}
	backupBytes, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupBytes, prior) {
		t.Fatal("backup did not preserve exact prior bytes")
	}
	file, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if file.Clients[0].NativeFallbackModel != config.DefaultClaudeNativeFallbackModel {
		t.Fatalf("native_fallback_model=%q", file.Clients[0].NativeFallbackModel)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%04o want 0640", info.Mode().Perm())
	}
	changed, secondBackup, err := migrateFallbackPolicyConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed || secondBackup != "" {
		t.Fatalf("second migration changed=%t backup=%q", changed, secondBackup)
	}
}

func TestMigrateFallbackPolicyConfigPreservesCustomTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	custom := bytes.Replace(config.InitTemplate,
		[]byte(config.DefaultClaudeNativeFallbackModel), []byte("claude-sonnet-5"), 1)
	if err := os.WriteFile(path, custom, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, backup, err := migrateFallbackPolicyConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed || backup != "" {
		t.Fatalf("changed=%t backup=%q", changed, backup)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, custom) {
		t.Fatal("custom config changed")
	}
}

func TestFallbackPolicyMigrationVerificationFailureRestoresExactPriorConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	prior := []byte("prior exact config\n")
	desired := []byte("desired migrated config\n")
	if err := os.WriteFile(path, desired, 0o640); err != nil {
		t.Fatal(err)
	}
	verificationErr := errors.New("synthetic post-install read failure")
	err := verifyOrRestoreFallbackPolicyMigration(
		path,
		prior,
		desired,
		0o640,
		path+".backup",
		fallbackPolicyMigrationVerificationOps{
			readInstalled: func(string) ([]byte, os.FileMode, error) {
				return nil, 0, verificationErr
			},
			restore:      compareAndSwapExactConfig,
			readRestored: readExactConfig,
		},
	)
	if err == nil || !strings.Contains(err.Error(), verificationErr.Error()) || !strings.Contains(err.Error(), "prior exact config restored") {
		t.Fatalf("error=%v", err)
	}
	restored, restoredMode, readErr := readExactConfig(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(restored, prior) || restoredMode.Perm() != 0o640 {
		t.Fatalf("restored=%q mode=%04o", restored, restoredMode.Perm())
	}
}

func TestFallbackPolicyMigrationReportsRestoreFailureAndRetainsBackupPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	prior := []byte("prior exact config\n")
	desired := []byte("desired migrated config\n")
	if err := os.WriteFile(path, desired, 0o600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".backup"
	restoreErr := errors.New("synthetic atomic restore failure")
	err := verifyOrRestoreFallbackPolicyMigration(
		path,
		prior,
		desired,
		0o600,
		backup,
		fallbackPolicyMigrationVerificationOps{
			readInstalled: func(string) ([]byte, os.FileMode, error) {
				return nil, 0, errors.New("synthetic verification failure")
			},
			restore: func(string, string, []byte, os.FileMode) error {
				return restoreErr
			},
			readRestored: func(string) ([]byte, os.FileMode, error) {
				t.Fatal("restore verification must not run after restore failure")
				return nil, 0, nil
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), restoreErr.Error()) || !strings.Contains(err.Error(), backup) {
		t.Fatalf("error=%v", err)
	}
	written, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(written, desired) {
		t.Fatal("restore failure path unexpectedly changed the installed config")
	}
}
