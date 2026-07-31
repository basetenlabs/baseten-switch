package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/internal/piconfig"
	"github.com/basetenlabs/baseten-switch/gateway/internal/safefile"
)

type piFileStore struct {
	backupRoot string
}

type piBackup struct {
	Version             int      `json:"version"`
	ConfigPath          string   `json:"config_path"`
	ResolvedPath        string   `json:"resolved_path"`
	Original            []byte   `json:"original,omitempty"`
	OriginalExists      bool     `json:"original_exists"`
	OriginalHash        string   `json:"original_hash"`
	FileHashes          []string `json:"file_hashes"`
	ProviderHashes      []string `json:"provider_hashes"`
	PendingFileHash     string   `json:"pending_file_hash,omitempty"`
	PendingProviderHash string   `json:"pending_provider_hash,omitempty"`
}

type piProviderJSON struct {
	Name    string            `json:"name"`
	BaseURL string            `json:"baseUrl"`
	API     string            `json:"api"`
	APIKey  string            `json:"apiKey"`
	Headers map[string]string `json:"headers"`
	Models  []piModelJSON     `json:"models"`
}

type piModelJSON struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Reasoning     bool           `json:"reasoning"`
	Input         []string       `json:"input"`
	ContextWindow int            `json:"contextWindow"`
	MaxTokens     int            `json:"maxTokens"`
	Cost          piProviderCost `json:"cost"`
}

func init() {
	newPiConfigStore = func() piConfigStore {
		return &piFileStore{
			backupRoot: envDefault(
				"BASETEN_SWITCH_BACKUP_DIR",
				homeJoin(".config", "baseten-switch", "backups"),
			),
		}
	}
}

func (s *piFileStore) Install(_ context.Context, req piInstallRequest) (piInstallResult, error) {
	modelsPath := filepath.Join(req.AgentDir, piModelsFilename)
	lock, err := s.acquireLock(modelsPath)
	if err != nil {
		return piInstallResult{}, err
	}
	defer lock.close()
	snapshot, err := safefile.Read(modelsPath)
	if err != nil {
		return piInstallResult{}, err
	}
	current := piEditableData(snapshot.Data)
	provider, err := marshalPiProvider(req.Provider)
	if err != nil {
		return piInstallResult{}, err
	}
	providerHash := piHash(provider)

	backupPath := s.backupPath(snapshot.RequestedPath)
	backup, err := loadPiBackup(backupPath)
	if err != nil {
		return piInstallResult{}, err
	}
	existing, found, err := piconfig.Provider(current, req.Provider.ID)
	if err != nil {
		return piInstallResult{}, err
	}

	if backup == nil {
		if found {
			return piInstallResult{}, fmt.Errorf(
				"%w: provider %q is not managed by Baseten Switch",
				piconfig.ErrProviderCollision, req.Provider.ID,
			)
		}
		backup = &piBackup{
			Version:        1,
			ConfigPath:     snapshot.RequestedPath,
			ResolvedPath:   snapshot.ResolvedPath,
			Original:       append([]byte(nil), snapshot.Data...),
			OriginalExists: snapshot.Exists,
			OriginalHash:   piHash(snapshot.Data),
		}
	} else {
		if err := backup.validate(snapshot); err != nil {
			return piInstallResult{}, err
		}
		if backup.pending() {
			currentHash := piHash(current)
			if found &&
				currentHash == backup.PendingFileHash &&
				piHash(existing) == backup.PendingProviderHash {
				backup.FileHashes = piAppendHash(backup.FileHashes, backup.PendingFileHash)
				backup.ProviderHashes = piAppendHash(backup.ProviderHashes, backup.PendingProviderHash)
			} else if found && !piHashKnown(backup.ProviderHashes, piHash(existing)) {
				return piInstallResult{}, errors.New(
					"Pi recovery state records an incomplete install and the provider changed; recovery state was retained",
				)
			}
			backup.clearPending()
			if err := savePiBackup(backupPath, backup); err != nil {
				return piInstallResult{}, fmt.Errorf("reconcile Pi recovery state: %w", err)
			}
		}
		if found && !piHashKnown(backup.ProviderHashes, piHash(existing)) {
			return piInstallResult{}, fmt.Errorf(
				"%w: provider %q changed outside Baseten Switch",
				piconfig.ErrProviderCollision, req.Provider.ID,
			)
		}
	}

	desired, changed, err := piconfig.UpsertProvider(current, req.Provider.ID, provider, found)
	if err != nil {
		return piInstallResult{}, err
	}
	backup.PendingProviderHash = providerHash
	backup.PendingFileHash = piHash(desired)
	if err := savePiBackup(backupPath, backup); err != nil {
		return piInstallResult{}, fmt.Errorf("save Pi recovery state: %w", err)
	}
	if changed || !snapshot.Exists || !bytes.Equal(snapshot.Data, desired) {
		if _, err := snapshot.Replace(desired, 0o600); err != nil {
			return piInstallResult{}, fmt.Errorf("write Pi model configuration: %w", err)
		}
	}
	backup.ProviderHashes = piAppendHash(backup.ProviderHashes, backup.PendingProviderHash)
	backup.FileHashes = piAppendHash(backup.FileHashes, backup.PendingFileHash)
	backup.clearPending()
	if err := savePiBackup(backupPath, backup); err != nil {
		return piInstallResult{}, fmt.Errorf(
			"Pi provider was written but recovery state could not be finalized: %w", err,
		)
	}
	return piInstallResult{
		ModelsPath: snapshot.RequestedPath,
		ModelCount: len(req.Provider.Models),
		Changed:    changed || !snapshot.Exists,
	}, nil
}

func (s *piFileStore) Status(_ context.Context, agentDir string) (piStatusResult, error) {
	modelsPath := filepath.Join(agentDir, piModelsFilename)
	lock, err := s.acquireLock(modelsPath)
	if err != nil {
		return piStatusResult{}, err
	}
	defer lock.close()
	snapshot, err := safefile.Read(modelsPath)
	if err != nil {
		return piStatusResult{}, err
	}
	result := piStatusResult{ModelsPath: snapshot.RequestedPath}
	if !snapshot.Exists || len(bytes.TrimSpace(snapshot.Data)) == 0 {
		return result, nil
	}
	provider, found, err := piconfig.Provider(snapshot.Data, piProviderID)
	if err != nil {
		return piStatusResult{}, err
	}
	if !found {
		return result, nil
	}
	result.Installed = true

	backup, err := loadPiBackup(s.backupPath(snapshot.RequestedPath))
	if err != nil {
		return piStatusResult{}, err
	}
	if backup == nil {
		result.Detail = "provider exists but has no Baseten Switch ownership state"
		return result, nil
	}
	if err := backup.validate(snapshot); err != nil {
		result.Detail = err.Error()
		return result, nil
	}
	if backup.pending() {
		result.Detail = "the previous Pi install did not finalize its recovery state; run 'baseten-switch pi install' again"
		return result, nil
	}
	if !piHashKnown(backup.ProviderHashes, piHash(provider)) {
		result.Detail = "managed provider changed outside Baseten Switch"
		return result, nil
	}
	result.ModelCount, err = piProviderModelCount(provider)
	if err != nil {
		result.Detail = err.Error()
		return result, nil
	}
	result.Healthy = true
	return result, nil
}

func (s *piFileStore) Uninstall(_ context.Context, agentDir string) (piUninstallResult, error) {
	modelsPath := filepath.Join(agentDir, piModelsFilename)
	lock, err := s.acquireLock(modelsPath)
	if err != nil {
		return piUninstallResult{}, err
	}
	defer lock.close()
	snapshot, err := safefile.Read(modelsPath)
	if err != nil {
		return piUninstallResult{}, err
	}
	result := piUninstallResult{ModelsPath: snapshot.RequestedPath}
	backupPath := s.backupPath(snapshot.RequestedPath)
	backup, err := loadPiBackup(backupPath)
	if err != nil {
		return result, err
	}

	current := piEditableData(snapshot.Data)
	provider, found, err := piconfig.Provider(current, piProviderID)
	if err != nil {
		return result, err
	}
	if backup == nil {
		if found {
			return result, fmt.Errorf(
				"%w: provider %q is not managed by Baseten Switch",
				piconfig.ErrProviderCollision, piProviderID,
			)
		}
		return result, nil
	}
	if err := backup.validate(snapshot); err != nil {
		return result, err
	}
	if backup.pending() {
		return result, errors.New(
			"the previous Pi install did not finalize its recovery state; run 'baseten-switch pi install' again before uninstalling",
		)
	}
	if backup.OriginalExists &&
		(!snapshot.Exists || len(bytes.TrimSpace(snapshot.Data)) == 0) {
		return result, errors.New(
			"Pi model configuration was removed or emptied after install; recovery state was retained",
		)
	}

	switch {
	case piHashKnown(backup.FileHashes, piHash(current)):
		if backup.OriginalExists {
			if _, err := snapshot.Replace(backup.Original, 0o600); err != nil {
				return result, fmt.Errorf("restore Pi model configuration: %w", err)
			}
		} else if err := snapshot.Remove(); err != nil {
			return result, fmt.Errorf("remove created Pi model configuration: %w", err)
		}
		result.Changed = true
	case !found:
		// The user already removed the managed entry. Only ownership state
		// remains to clean up.
	case piHashKnown(backup.ProviderHashes, piHash(provider)):
		desired, changed, err := piconfig.RemoveProvider(current, piProviderID)
		if err != nil {
			return result, err
		}
		if changed {
			if _, err := snapshot.Replace(desired, 0o600); err != nil {
				return result, fmt.Errorf("remove managed Pi provider: %w", err)
			}
			result.Changed = true
		}
	default:
		return result, fmt.Errorf(
			"%w: provider %q changed outside Baseten Switch; recovery state was retained",
			piconfig.ErrProviderCollision, piProviderID,
		)
	}
	if err := removePiBackup(backupPath); err != nil {
		return result, fmt.Errorf("remove Pi recovery state: %w", err)
	}
	return result, nil
}

func marshalPiProvider(spec piProviderSpec) ([]byte, error) {
	if spec.ID == "" || len(spec.Models) == 0 {
		return nil, errors.New("Pi provider requires an ID and at least one model")
	}
	models := make([]piModelJSON, 0, len(spec.Models))
	for _, model := range spec.Models {
		if model.ID == "" ||
			model.ContextWindow <= 0 ||
			model.MaxTokens <= 0 ||
			len(model.Input) == 0 {
			return nil, fmt.Errorf("invalid Pi model metadata for %q", model.ID)
		}
		for _, modality := range model.Input {
			if modality != "text" && modality != "image" {
				return nil, fmt.Errorf(
					"invalid Pi input modality %q for %q",
					modality,
					model.ID,
				)
			}
		}
		models = append(models, piModelJSON{
			ID:            model.ID,
			Name:          model.Name,
			Reasoning:     model.Reasoning,
			Input:         append([]string(nil), model.Input...),
			ContextWindow: model.ContextWindow,
			MaxTokens:     model.MaxTokens,
			Cost:          model.Cost,
		})
	}
	return json.MarshalIndent(piProviderJSON{
		Name: spec.Name, BaseURL: spec.BaseURL, API: spec.API,
		APIKey: spec.APIKey, Headers: spec.Headers, Models: models,
	}, "", "  ")
}

func piEditableData(data []byte) []byte {
	if len(bytes.TrimSpace(data)) == 0 {
		return []byte("{}\n")
	}
	return data
}

func piProviderModelCount(provider []byte) (int, error) {
	var value struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(provider, &value); err != nil {
		return 0, fmt.Errorf("parse managed provider: %w", err)
	}
	return len(value.Models), nil
}

func (b *piBackup) validate(snapshot *safefile.Snapshot) error {
	if b.Version != 1 {
		return fmt.Errorf("Pi recovery state has unsupported version %d", b.Version)
	}
	if b.ConfigPath != snapshot.RequestedPath {
		return fmt.Errorf("Pi recovery state belongs to a different configured path")
	}
	if b.ResolvedPath != snapshot.ResolvedPath {
		return fmt.Errorf("Pi model configuration resolves to a different target")
	}
	if b.OriginalHash != piHash(b.Original) {
		return errors.New("Pi recovery state original-content hash is invalid")
	}
	if !b.OriginalExists && len(b.Original) != 0 {
		return errors.New("Pi recovery state records content for a file that did not exist")
	}
	for _, hash := range append(append([]string{}, b.FileHashes...), b.ProviderHashes...) {
		if !piValidHash(hash) {
			return errors.New("Pi recovery state contains an invalid managed-content hash")
		}
	}
	if (b.PendingFileHash == "") != (b.PendingProviderHash == "") ||
		(b.PendingFileHash != "" &&
			(!piValidHash(b.PendingFileHash) || !piValidHash(b.PendingProviderHash))) {
		return errors.New("Pi recovery state contains an invalid pending mutation")
	}
	return nil
}

func (b *piBackup) pending() bool {
	return b.PendingFileHash != ""
}

func (b *piBackup) clearPending() {
	b.PendingFileHash = ""
	b.PendingProviderHash = ""
}

func (s *piFileStore) acquireLock(modelsPath string) (*configMutationLock, error) {
	absolute, err := filepath.Abs(modelsPath)
	if err != nil {
		return nil, fmt.Errorf("resolve Pi model path for locking: %w", err)
	}
	lockBase := s.backupPath(filepath.Clean(absolute))
	if err := os.MkdirAll(filepath.Dir(lockBase), 0o700); err != nil {
		return nil, fmt.Errorf("create Pi state directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(lockBase), 0o700); err != nil {
		return nil, fmt.Errorf("secure Pi state directory: %w", err)
	}
	return acquireConfigMutationLock(lockBase)
}

func (s *piFileStore) backupPath(modelsPath string) string {
	sum := sha256.Sum256([]byte(modelsPath))
	return filepath.Join(s.backupRoot, "pi", "config-backup."+hex.EncodeToString(sum[:])[:16]+".json")
}

func loadPiBackup(path string) (*piBackup, error) {
	snapshot, err := safefile.Read(path)
	if err != nil {
		return nil, err
	}
	if !snapshot.Exists {
		return nil, nil
	}
	if piBackupFinalSymlink(path) {
		return nil, errors.New("Pi recovery state path must not be a symbolic link")
	}
	if snapshot.Mode.Perm() != 0o600 {
		if err := os.Chmod(snapshot.ResolvedPath, 0o600); err != nil {
			return nil, fmt.Errorf("secure Pi recovery state: %w", err)
		}
		snapshot, err = safefile.Read(path)
		if err != nil {
			return nil, err
		}
	}
	if err := rejectDuplicatePiBackupKeys(snapshot.Data); err != nil {
		return nil, fmt.Errorf("parse Pi recovery state: %w", err)
	}
	var backup piBackup
	if err := json.Unmarshal(snapshot.Data, &backup); err != nil {
		return nil, fmt.Errorf("parse Pi recovery state: %w", err)
	}
	return &backup, nil
}

func savePiBackup(path string, backup *piBackup) error {
	data, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	snapshot, err := safefile.Read(path)
	if err != nil {
		return err
	}
	if piBackupFinalSymlink(path) {
		return errors.New("Pi recovery state path must not be a symbolic link")
	}
	if snapshot.Exists && snapshot.Mode.Perm() != 0o600 {
		if err := os.Chmod(snapshot.ResolvedPath, 0o600); err != nil {
			return fmt.Errorf("secure Pi recovery state: %w", err)
		}
		snapshot, err = safefile.Read(path)
		if err != nil {
			return err
		}
	}
	_, err = snapshot.Replace(data, 0o600)
	return err
}

func removePiBackup(path string) error {
	snapshot, err := safefile.Read(path)
	if err != nil {
		return err
	}
	if piBackupFinalSymlink(path) {
		return errors.New("Pi recovery state path must not be a symbolic link")
	}
	return snapshot.Remove()
}

func piBackupFinalSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

func piHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func piHashKnown(hashes []string, hash string) bool {
	for _, candidate := range hashes {
		if candidate == hash {
			return true
		}
	}
	return false
}

func piAppendHash(hashes []string, hash string) []string {
	if piHashKnown(hashes, hash) {
		return hashes
	}
	return append(hashes, hash)
}

func piValidHash(hash string) bool {
	if len(hash) != sha256.Size*2 || strings.ToLower(hash) != hash {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func rejectDuplicatePiBackupKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate key %q", key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delim)
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
