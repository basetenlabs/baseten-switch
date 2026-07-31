package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/piconfig"
	"github.com/basetenlabs/baseten-switch/gateway/internal/safefile"
)

func testPiInstallRequest(agentDir string) piInstallRequest {
	return piInstallRequest{
		AgentDir: agentDir,
		Provider: piProviderSpec{
			ID: piProviderID, Name: piProviderName, BaseURL: piProviderBaseURL,
			API: piProviderAPI, APIKey: piAPIKeyReference,
			Headers: map[string]string{"Authorization": piAuthHeaderValue},
			Models: []piProviderModel{{
				ID: "example/model", Name: "Example Model",
				ContextWindow: 32000, MaxTokens: 4096,
				Cost:            piProviderCost{Input: 0.1, Output: 0.2},
				Reasoning:       true,
				Input:           []string{"text", "image"},
				CapabilityKnown: true,
			}},
		},
	}
}

func TestPiFileStoreInstallStatusAndExactUninstall(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	modelsPath := filepath.Join(agentDir, piModelsFilename)
	original := []byte("{\n  // retained\n  \"providers\": {\"other\": {\"baseUrl\": \"https://example.invalid\"}},\n}\n")
	if err := os.WriteFile(modelsPath, original, 0o640); err != nil {
		t.Fatal(err)
	}
	store := &piFileStore{backupRoot: filepath.Join(root, "backups")}

	installed, err := store.Install(context.Background(), testPiInstallRequest(agentDir))
	if err != nil {
		t.Fatal(err)
	}
	if !installed.Changed || installed.ModelCount != 1 {
		t.Fatalf("install result = %+v", installed)
	}
	data, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"baseten"`, `"baseUrl": "https://inference.baseten.co/v1"`,
		`"Authorization": "Api-Key $BASETEN_API_KEY"`,
		`"cacheRead": 0`, `"cacheWrite": 0`, "// retained",
	} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("installed config missing %q:\n%s", want, data)
		}
	}
	info, err := os.Stat(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %04o, want 0640", info.Mode().Perm())
	}

	status, err := store.Status(context.Background(), agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Healthy || status.ModelCount != 1 {
		t.Fatalf("status = %+v", status)
	}

	uninstalled, err := store.Uninstall(context.Background(), agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if !uninstalled.Changed {
		t.Fatal("uninstall reported no change")
	}
	restored, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("restored bytes differ:\n%s", restored)
	}
}

func TestPiFileStoreMissingFileIsRemovedOnUninstall(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	store := &piFileStore{backupRoot: filepath.Join(root, "backups")}

	if _, err := store.Install(context.Background(), testPiInstallRequest(agentDir)); err != nil {
		t.Fatal(err)
	}
	modelsPath := filepath.Join(agentDir, piModelsFilename)
	if _, err := os.Stat(modelsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Uninstall(context.Background(), agentDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(modelsPath); !os.IsNotExist(err) {
		t.Fatalf("created models file remains after uninstall: %v", err)
	}
}

func TestPiFileStoreDriftRemovesOnlyRecognizedProvider(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	store := &piFileStore{backupRoot: filepath.Join(root, "backups")}
	if _, err := store.Install(context.Background(), testPiInstallRequest(agentDir)); err != nil {
		t.Fatal(err)
	}
	modelsPath := filepath.Join(agentDir, piModelsFilename)
	managed, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	close := bytes.LastIndexByte(managed, '}')
	drifted := append([]byte(nil), managed[:close]...)
	drifted = append(drifted, []byte(",\n  \"userSetting\": true\n}\n")...)
	if err := os.WriteFile(modelsPath, drifted, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Uninstall(context.Background(), agentDir); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(after, []byte(`"baseten"`)) {
		t.Fatalf("managed provider remains:\n%s", after)
	}
	if !bytes.Contains(after, []byte(`"userSetting": true`)) {
		t.Fatalf("unrelated drift was lost:\n%s", after)
	}
}

func TestPiFileStoreRefusesForeignOrChangedProvider(t *testing.T) {
	t.Run("foreign before install", func(t *testing.T) {
		root := t.TempDir()
		agentDir := filepath.Join(root, "agent")
		if err := os.MkdirAll(agentDir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(agentDir, piModelsFilename)
		foreign := []byte(`{"providers":{"baseten":{"name":"User entry"}}}` + "\n")
		if err := os.WriteFile(path, foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		store := &piFileStore{backupRoot: filepath.Join(root, "backups")}
		_, err := store.Install(context.Background(), testPiInstallRequest(agentDir))
		if !errors.Is(err, piconfig.ErrProviderCollision) {
			t.Fatalf("install error = %v", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, foreign) {
			t.Fatal("foreign provider was changed")
		}
	})

	t.Run("changed after install", func(t *testing.T) {
		root := t.TempDir()
		agentDir := filepath.Join(root, "agent")
		store := &piFileStore{backupRoot: filepath.Join(root, "backups")}
		if _, err := store.Install(context.Background(), testPiInstallRequest(agentDir)); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(agentDir, piModelsFilename)
		data, _ := os.ReadFile(path)
		changed, _, err := piconfig.UpsertProvider(data, piProviderID, []byte(`{"name":"User replacement"}`), true)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, changed, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = store.Uninstall(context.Background(), agentDir)
		if !errors.Is(err, piconfig.ErrProviderCollision) {
			t.Fatalf("uninstall error = %v", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Contains(after, []byte("User replacement")) {
			t.Fatal("changed provider was overwritten")
		}
	})
}

func TestPiFileStorePreservesSymlinkAndNeverPersistsSecret(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	targetDir := filepath.Join(root, "target")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "models.json")
	original := []byte("{}\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(agentDir, piModelsFilename)
	if err := os.Symlink(filepath.Join("..", "target", "models.json"), configured); err != nil {
		t.Fatal(err)
	}
	store := &piFileStore{backupRoot: filepath.Join(root, "backups")}
	if _, err := store.Install(context.Background(), testPiInstallRequest(agentDir)); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(configured); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("configured path is not a preserved symlink: %v", err)
	}
	secret := "secret-value-that-must-not-persist"
	for _, path := range []string{target, store.backupPath(configured)} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("secret persisted in %s", path)
		}
	}
	if _, err := store.Uninstall(context.Background(), agentDir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(configured); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("configured path is not a preserved symlink after uninstall: %v", err)
	}
}

func TestPiFileStoreSecuresExistingBackup(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	store := &piFileStore{backupRoot: filepath.Join(root, "backups")}
	modelsPath, err := filepath.Abs(filepath.Join(agentDir, piModelsFilename))
	if err != nil {
		t.Fatal(err)
	}
	backupPath := store.backupPath(modelsPath)
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot, err := safefile.Read(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	stale := &piBackup{
		Version: 1, ConfigPath: snapshot.RequestedPath, ResolvedPath: snapshot.ResolvedPath,
		OriginalHash: piHash(nil),
	}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Install(context.Background(), testPiInstallRequest(agentDir)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %04o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(backupPath))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("backup directory mode = %04o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestPiFileStoreRetainsRecoveryWhenOriginalConfigIsLost(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	modelsPath := filepath.Join(agentDir, piModelsFilename)
	if err := os.WriteFile(modelsPath, []byte(`{"providers":{"other":{}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &piFileStore{backupRoot: filepath.Join(root, "backups")}
	if _, err := store.Install(context.Background(), testPiInstallRequest(agentDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(modelsPath); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Uninstall(context.Background(), agentDir); err == nil {
		t.Fatal("uninstall succeeded after the original config was lost")
	}
	absolute, err := filepath.Abs(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.backupPath(absolute)); err != nil {
		t.Fatalf("recovery state was not retained: %v", err)
	}
}
