package main

import (
	"fmt"
	"io"
	"net/url"
)

// Migrate only a previously managed value, never enable an integration or
// repair other settings as a side effect of starting the gateway.
func migrateClaudeAttributionBeforeStart(out io.Writer) {
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		// Configurations without a Claude client have nothing to migrate.
		return
	}
	changed, err := a.migrateAttribution()
	if err != nil {
		if changed {
			fmt.Fprintf(out, "claude: attribution enabled, but migration record could not be saved: %v; run 'baseten-switch claude on' to repair the backup, then restart Claude Code sessions\n", err)
		} else {
			fmt.Fprintf(out, "claude: attribution settings migration skipped: %v; run 'baseten-switch claude on' to repair settings\n", err)
		}
	} else if changed {
		fmt.Fprintln(out, "claude: enabled attribution for native requests; restart Claude Code sessions to pick this up")
	}
}

func (a *claudeAdapter) migrateAttribution() (bool, error) {
	bak, err := loadClaudeBackup(a.backupPath)
	if err != nil || bak == nil {
		return false, err
	}
	if written, owned := backupWrittenValue(bak, claudeAttributionEnvKey); bak.WrittenValues != nil && (!owned || written != "0") {
		return false, nil
	}
	lock, err := a.acquireSettingsMutationLock()
	if err != nil {
		return false, err
	}
	defer lock.close()
	bak, err = loadClaudeBackup(a.backupPath)
	if err != nil || bak == nil {
		return false, err
	}
	if written, owned := backupWrittenValue(bak, claudeAttributionEnvKey); bak.WrittenValues != nil && (!owned || written != "0") {
		return false, nil
	}
	root, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil || !snap.Exists {
		return false, err
	}
	env, err := settingsEnv(root)
	if err != nil {
		return false, err
	}
	base, _ := envString(env, claudeManagedEnvKey)
	baseURL, _ := url.Parse(base)
	if !a.isGatewayURL(base) || baseURL == nil || baseURL.Port() != a.desiredPort {
		return false, nil
	}
	cur, _ := envString(env, claudeAttributionEnvKey)
	if cur != "0" && cur != claudeAttributionValue {
		return false, nil
	}
	if bak.ConfigPath != a.settingsPath || !claudeBackupTargetSafe(bak, snap) {
		return false, fmt.Errorf("settings target changed")
	}
	if !backupCovers(bak, claudeAttributionEnvKey) || bak.Values[claudeAttributionEnvKey] == "0" {
		if cur == "0" {
			return false, fmt.Errorf("attribution value is not proven to be Switch-owned")
		}
		return false, nil
	}
	clean := bak.WrittenHash == sha256Hex(snap.Data) && claudeBackupMatchesFile(bak, snap)
	a.initializeWrittenValues(bak, env)
	changed := cur == "0"
	if changed {
		env[claudeAttributionEnvKey] = claudeAttributionValue
		if claudeBeforeSettingsMutation != nil {
			claudeBeforeSettingsMutation()
		}
		var raw []byte
		raw, snap, err = writeClaudeSettings(snap, root)
		if err != nil {
			return false, err
		}
		// A migration must not convert an already-drifted backup into a
		// clean restore that would overwrite later user changes on `off`.
		if clean {
			bak.WrittenHash = sha256Hex(raw)
			recordClaudeBackupFile(bak, snap)
		}
	} else if err := snap.Verify(); err != nil {
		return false, err
	}
	if changed {
		recordWrittenValues(bak, env, []string{claudeAttributionEnvKey})
	} else {
		delete(bak.WrittenValues, claudeAttributionEnvKey)
	}
	if err := saveClaudeBackup(a.backupPath, bak); err != nil {
		return changed, err
	}
	return changed, nil
}
