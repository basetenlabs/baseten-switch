// claude_adapter.go implements `baseten-switch claude on|off|status` and
// `baseten-switch claude subagents [<model>|off]`.
//
// `on` configures ~/.claude/settings.json for the gateway by setting
// ANTHROPIC_BASE_URL, CLAUDE_CODE_ATTRIBUTION_HEADER, and
// ENABLE_TOOL_SEARCH in the env block. It takes a key-level backup of
// the user's prior values on the FIRST on only, and never snapshots a
// gateway-owned base URL as "the user's original".
// Older versions managed CLAUDE_CODE_SUBAGENT_MODEL in settings.
// `off` retains cleanup support for that retired state. Backups grow
// additively as new keys become managed. `off` restores the backup
// exactly, or degrades to strip-only-owned when the backup is missing,
// the file drifted after `on`, or the backup itself is poisoned.
//
// JSON round-trip note: settings.json is parsed and re-marshaled with
// two-space indentation; all values (including number literals, via
// json.Number) survive byte-exactly, but top-level and nested key
// ORDER may change once on the first write (Go marshals map keys
// sorted). Content is never dropped.
//
// Status exit codes:
//
//	0  managed (routing through the gateway door)
//	3  not managed
//
// on/off exit 0 on success (including noops) and 1 on errors.
//
// Test override points (unit tests only; never point these at live
// state): BASETEN_SWITCH_CLAUDE_SETTINGS (settings.json path), BASETEN_SWITCH_BACKUP_DIR
// (backup root; the claude/ subdir is appended), BASETEN_SWITCH_CONFIG_PATH
// (gateway.yaml, as everywhere else).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
	"github.com/basetenlabs/baseten-switch/gateway/internal/safefile"
)

const (
	claudeManagedEnvKey     = "ANTHROPIC_BASE_URL"
	claudeAttributionEnvKey = "CLAUDE_CODE_ATTRIBUTION_HEADER"
	claudeToolSearchEnvKey  = "ENABLE_TOOL_SEARCH"
	claudeSubagentEnvKey    = "CLAUDE_CODE_SUBAGENT_MODEL"
	claudeAliasPrefix       = "claude-baseten-" // the model-discovery contract alias namespace
	claudeHarnessName       = "claude"

	claudeAttributionValue = "0"
	// Claude Code documents "true" as the always-on tool-search value.
	// Keep this centralized because accepted values are versioned by the
	// harness, while the adapter's backup/restore behavior is key-based.
	claudeToolSearchValue = "true"

	// Status uses 0 = on and 3 = off (1 is reserved for operational
	// errors, 2 for usage).
	statusExitOff = 3
)

// claudeOnEnvKeys is the complete settings env state installed by
// `claude on`. claudeManagedEnvKeys also includes the retired
// settings-side subagent key so `off` can still clean up old installs.
var (
	claudeOnEnvKeys = []string{
		claudeManagedEnvKey,
		claudeAttributionEnvKey,
		claudeToolSearchEnvKey,
	}
	claudeManagedEnvKeys = []string{
		claudeManagedEnvKey,
		claudeAttributionEnvKey,
		claudeToolSearchEnvKey,
		claudeSubagentEnvKey,
	}
)

func cmdClaude(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch claude on|off|status|subagents [<model>|on|inherit]|route [<family> <target|default>]|reasoning baseten <model> off|follow-harness|effort <value>|default  (start/stop are aliases for on/off)")
		return 2
	}
	a, err := newClaudeAdapterFromEnv()
	if err != nil {
		if args[0] == "route" ||
			args[0] == "subagents" ||
			args[0] == "reasoning" {
			opts, _, parseErr := parseMutationOptions(args[1:])
			if parseErr == nil && opts.JSON {
				operation := "set_claude_route"
				if args[0] == "subagents" {
					operation = "set_claude_subagents"
				} else if args[0] == "reasoning" {
					operation = "set_model_reasoning"
				}
				return failMutation(opts, os.Stdout, mutationResult{
					Operation: operation,
					Client:    "claude-code",
				},
					"config_load_failed", fmt.Sprintf("claude: %v", err), false, 1)
			}
		}
		fmt.Fprintf(os.Stderr, "claude: %v\n", err)
		return 1
	}
	switch args[0] {
	case "on", "start":
		return a.on()
	case "off", "stop":
		return a.off()
	case "status":
		return a.status(os.Stdout)
	case "subagents":
		return a.subagents(args[1:], os.Stdout)
	case "route":
		return a.route(args[1:], os.Stdout)
	case "reasoning":
		return runClientReasoning(a.clientName, args[1:], os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown claude subcommand: %s (use on|off|status|subagents|route|reasoning)\n", args[0])
		return 2
	}
}

// claudeAdapter carries the resolved paths and gateway topology for
// one on/off/status/subagents invocation. Tests construct it directly
// against t.TempDir() paths; cmdClaude builds it from env + gateway.yaml.
type claudeAdapter struct {
	settingsPath string
	backupPath   string
	configPath   string            // gateway.yaml, for the config-edit subagents verb
	clientName   string            // the anthropic-shape client the subagents verb targets
	desiredPort  string            // door port the anthropic-shape claude-code client rides
	gatewayPorts map[string]bool   // every local port we consider "ours" (door binds + router listeners)
	modelAliases map[string]string // the target client's model_aliases (subagent alias validation)
	out          io.Writer         // human-readable action/warning output
}

// claudeBeforeSettingsMutation is a test seam for changes after the adapter
// has staged recovery state but before safefile revalidates the preimage.
var claudeBeforeSettingsMutation func()

// claudeBeforeMigratedBackupReplace is a test seam for a change after one
// migrated-backup generation has been captured but before the next atomic CAS.
var claudeBeforeMigratedBackupReplace func(action string)

func (a *claudeAdapter) acquireSettingsMutationLock() (*configMutationLock, error) {
	if err := os.MkdirAll(filepath.Dir(a.backupPath), 0o700); err != nil {
		return nil, fmt.Errorf("create backup directory for settings lock: %w", err)
	}
	return acquireConfigMutationLock(a.backupPath)
}

func (a *claudeAdapter) requiresJournaledMutation(_ mutationOptions) (bool, error) {
	_, err := config.Load(a.configPath)
	return true, err
}

func (a *claudeAdapter) runClaudeJournaledMutationLocked(opts mutationOptions, stdout io.Writer, spec journaledMutationSpec) int {
	out := stdout
	if !opts.JSON {
		out = a.out
	}
	prior, mode, err := readExactConfig(a.configPath)
	if err != nil {
		return failMutation(opts, out, mutationResultForSpec(
			spec, opts.OperationID, a.configPath, ""),
			"config_read_failed", err.Error(), true, 1)
	}
	_, err = config.Load(a.configPath)
	if err != nil {
		return failMutation(opts, out, mutationResultForSpec(
			spec, opts.OperationID, a.configPath, exactConfigHash(prior)),
			"config_load_failed", err.Error(), false, 1)
	}
	return runJournaledMutationLocked(a.configPath, prior, mode, opts, out, spec)
}

func newClaudeAdapterFromEnv() (*claudeAdapter, error) {
	f, cfgPath, err := loadGatewayConfigForAdapter()
	if err != nil {
		return nil, err
	}
	port, ports, aliases, err := claudeDoorPort(f)
	if err != nil {
		return nil, fmt.Errorf("%v (config: %s)", err, cfgPath)
	}
	clientName, err := claudeTargetClientName(f)
	if err != nil {
		return nil, fmt.Errorf("%v (config: %s)", err, cfgPath)
	}
	settings := envDefault("BASETEN_SWITCH_CLAUDE_SETTINGS", homeJoin(".claude", "settings.json"))
	backupRoot := envDefault("BASETEN_SWITCH_BACKUP_DIR", homeJoin(".config", "baseten-switch", "backups"))
	return &claudeAdapter{
		settingsPath: settings,
		backupPath:   claudeBackupPath(backupRoot, settings),
		configPath:   cfgPath,
		clientName:   clientName,
		desiredPort:  port,
		gatewayPorts: ports,
		modelAliases: aliases,
		out:          os.Stderr,
	}, nil
}

// claudeTargetClientName resolves the anthropic-shape client name the
// subagents verb targets: the "claude-code" client when present, else
// the first anthropic-shape client. Mirrors the selection in
// claudeDoorPort without returning port/alias data, so it does not
// change claudeDoorPort's signature (used by other lanes).
func claudeTargetClientName(f *config.File) (string, error) {
	var target *config.Client
	for i := range f.Clients {
		c := &f.Clients[i]
		shape := c.ProtocolShape
		if shape == "" {
			shape = "anthropic"
		}
		if shape != "anthropic" {
			continue
		}
		if c.Name == "claude-code" {
			target = c
			break
		}
		if target == nil {
			target = c
		}
	}
	if target == nil {
		return "", fmt.Errorf("no anthropic-shape client in gateway.yaml; add a claude-code client (reference: config/gateway.example.yaml in the baseten-switch repo)")
	}
	return target.Name, nil
}

// loadGatewayConfigForAdapter resolves gateway.yaml the same way the
// lifecycle commands do: BASETEN_SWITCH_CONFIG_PATH, else the sticky recorded
// path, else the default.
func loadGatewayConfigForAdapter() (*config.File, string, error) {
	path := os.Getenv("BASETEN_SWITCH_CONFIG_PATH")
	if path == "" {
		if p, _ := stickyConfigPath("", pidfile.ConfigStatePath(pidfile.Path())); p != "" {
			path = p
		}
	}
	if path == "" {
		path = config.DefaultPath()
	}
	f, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("%s", config.MissingConfigMessage(path))
		}
		return nil, "", fmt.Errorf("%s", config.MalformedConfigMessage(path, err))
	}
	return f, path, nil
}

// claudeDoorPort resolves, from gateway.yaml, the door port that
// carries the anthropic-shape claude-code client (the port the
// harness should be pointed at), the full set of local ports the
// adapter considers gateway-owned (door binds and router listeners,
// so a base URL pointing directly at the router also reads as
// managed), and that client's model_aliases (the id set `claude
// subagents` validates against). All values are derived from config,
// never hardcoded.
func claudeDoorPort(f *config.File) (string, map[string]bool, map[string]string, error) {
	ports := map[string]bool{}
	addPort := func(addr string) string {
		_, p, err := net.SplitHostPort(addr)
		if err != nil || p == "" {
			return ""
		}
		ports[p] = true
		return p
	}
	var target *config.Client
	for i := range f.Clients {
		c := &f.Clients[i]
		addPort(c.BindAddr)
		shape := c.ProtocolShape
		if shape == "" {
			shape = "anthropic" // empty defaults to anthropic (internal/door)
		}
		if shape != "anthropic" {
			continue
		}
		if c.Name == "claude-code" {
			target = c
		} else if target == nil {
			target = c
		}
	}
	if f.Door != nil {
		for _, dp := range f.Door.Ports {
			addPort(dp.BindAddr)
		}
	}
	if target == nil {
		return "", nil, nil, fmt.Errorf("no anthropic-shape client in gateway.yaml; add a claude-code client (reference: config/gateway.example.yaml in the baseten-switch repo)")
	}
	if f.Door == nil || len(f.Door.Ports) == 0 {
		return "", nil, nil, fmt.Errorf("gateway.yaml has no door: section; the claude adapter points the harness at the front door, so add door.ports mapping a bind_addr to the %s router listener %s (reference: config/gateway.example.yaml in the baseten-switch repo)", target.Name, target.BindAddr)
	}
	for _, dp := range f.Door.Ports {
		if dp.RouterAddr == target.BindAddr {
			_, p, err := net.SplitHostPort(dp.BindAddr)
			if err != nil || p == "" {
				return "", nil, nil, fmt.Errorf("door port %q for client %s has no parseable port", dp.BindAddr, target.Name)
			}
			return p, ports, target.ModelAliases, nil
		}
	}
	return "", nil, nil, fmt.Errorf("no door port routes to the %s client listener %s; add a door.ports entry with router_addr: %s", target.Name, target.BindAddr, target.BindAddr)
}

func (a *claudeAdapter) desiredURL() string {
	return "http://127.0.0.1:" + a.desiredPort
}

func (a *claudeAdapter) desiredClaudeEnvValue(key string) string {
	switch key {
	case claudeManagedEnvKey:
		return a.desiredURL()
	case claudeAttributionEnvKey:
		return claudeAttributionValue
	case claudeToolSearchEnvKey:
		return claudeToolSearchValue
	default:
		return ""
	}
}

// claudeOnState reports both the routing ownership state and whether all
// three `claude on` values exactly match the desired configuration.
func (a *claudeAdapter) claudeOnState(env map[string]any) (baseManaged, complete bool) {
	base, baseSet := envString(env, claudeManagedEnvKey)
	baseManaged = baseSet && a.isGatewayURL(base)
	if !baseManaged {
		return false, false
	}
	for _, key := range claudeOnEnvKeys {
		cur, ok := envString(env, key)
		if !ok || cur != a.desiredClaudeEnvValue(key) {
			return true, false
		}
	}
	return true, true
}

// isGatewayURL reports whether a base URL points at a local gateway
// port (127.0.0.1 or localhost on a door or router port). This is the
// "we own this value" test used for the managed check, the
// strip-only-owned fallback, and the poisoned-backup guard.
func (a *claudeAdapter) isGatewayURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" {
		return false
	}
	return a.gatewayPorts[u.Port()]
}

// Subagent model classes. Alias and slug values only work through the
// gateway (Anthropic rejects both), so those two are gateway-owned for
// strip purposes and require the base-URL wiring; a native id works
// against Anthropic directly, so it is user territory.
const (
	subagentClassAlias  = "gateway alias"
	subagentClassSlug   = "raw Baseten slug"
	subagentClassNative = "native id"
)

// classifySubagentModel maps a model id to its class. The alias check
// runs first: alias ids also carry the claude-/anthropic- prefix.
func classifySubagentModel(id string) (string, bool) {
	switch {
	case gateway.InAliasNamespace(id):
		return subagentClassAlias, true
	case strings.Contains(id, "/"):
		return subagentClassSlug, true
	case strings.HasPrefix(id, "claude-") || strings.HasPrefix(id, "anthropic-"):
		return subagentClassNative, true
	}
	return "", false
}

// claudeSubagentOwned is the "we own this value" test for the subagent
// key: a baseten alias or a raw Baseten slug only resolves through the
// gateway, so stripping it can never lose a user-chosen native model.
func claudeSubagentOwned(v string) bool {
	return gateway.InAliasNamespace(v) || strings.Contains(v, "/")
}

// ownedEnvValue dispatches the per-key ownership test used by the
// strip-only-owned paths and the poisoned-backup guard.
func (a *claudeAdapter) ownedEnvValue(key, val string) bool {
	switch key {
	case claudeSubagentEnvKey:
		return claudeSubagentOwned(val)
	case claudeManagedEnvKey:
		return a.isGatewayURL(val)
	default:
		// Fixed-value Claude Code knobs can legitimately predate
		// Baseten Switch. They are removable only when a backup proves
		// this invocation managed them.
		return false
	}
}

// --- settings.json round-trip ---------------------------------------------

// loadClaudeSettings parses settings.json into a generic tree. Numbers
// are decoded as json.Number so literals survive the round-trip
// byte-exactly. An absent file returns existed=false and an empty root.
func loadClaudeSettings(path string) (root map[string]any, raw []byte, existed bool, err error) {
	root, snap, err := readClaudeSettings(path)
	if err != nil {
		return nil, nil, false, err
	}
	return root, snap.Data, snap.Exists, nil
}

// readClaudeSettings keeps the safe-file snapshot associated with the bytes
// that were parsed. Mutating callers must commit through this exact snapshot
// so concurrent edits and link retargeting are detected.
func readClaudeSettings(path string) (map[string]any, *safefile.Snapshot, error) {
	snap, err := safefile.Read(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if !snap.Exists {
		return map[string]any{}, snap, nil
	}
	raw := snap.Data
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	root := map[string]any{}
	if err := dec.Decode(&root); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w (not touching it)", path, err)
	}
	return root, snap, nil
}

// writeClaudeSettings marshals the tree with two-space indentation and
// replaces it through the exact snapshot that was read, preserving the
// existing file mode (0600 for a newly created file since the env block can
// hold tokens). The safe-file layer preserves final-component symlinks and
// rejects stale, dangling, non-regular, or multiply hard-linked targets.
func writeClaudeSettings(snap *safefile.Snapshot, root map[string]any) ([]byte, *safefile.Snapshot, error) {
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal settings: %w", err)
	}
	b = append(b, '\n')
	committed, err := snap.Replace(b, 0o600)
	if err != nil {
		return b, committed, err
	}
	return b, committed, nil
}

// settingsEnv returns the env block as a mutable map, or nil when
// absent. A non-object env block is an error (we refuse to guess).
func settingsEnv(root map[string]any) (map[string]any, error) {
	v, ok := root["env"]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("settings env block is not a JSON object; fix it by hand")
	}
	return m, nil
}

func envString(env map[string]any, key string) (string, bool) {
	if env == nil {
		return "", false
	}
	s, ok := env[key].(string)
	return s, ok
}

// --- backup ----------------------------------------------------------------

// claudeBackup is the key-level snapshot taken on the first `on`.
// Values/Missing cover the managed env keys only, so user edits to
// hooks, permissions, and other env between on and off survive off.
// Model/ModelMissing snapshot the top-level "model" field for the
// model-discovery contract persisted-alias trap. WrittenHash, ResolvedPath,
// and WrittenIdentity bind the backup to the file generation written by
// `on`; a mismatch at `off` downgrades restore to strip-only-owned, while a
// changed resolved target is refused.
type claudeBackup struct {
	ConfigPath      string             `json:"config_path"`
	ResolvedPath    string             `json:"resolved_path,omitempty"`
	WrittenIdentity *safefile.Identity `json:"written_identity,omitempty"`
	Values          map[string]string  `json:"values"`
	Missing         []string           `json:"missing"`
	EnvExisted      bool               `json:"env_existed"`
	Model           string             `json:"model,omitempty"`
	ModelMissing    bool               `json:"model_missing"`
	Existed         bool               `json:"existed"`
	WrittenHash     string             `json:"written_hash"`
}

// claudeBackupPath keys the backup file by sha256(settings path) so
// backups never cross-restore between paths.
func claudeBackupPath(backupRoot, settingsPath string) string {
	sum := sha256.Sum256([]byte(settingsPath))
	name := "config-backup." + hex.EncodeToString(sum[:])[:16] + ".json"
	return filepath.Join(backupRoot, claudeHarnessName, name)
}

func loadClaudeBackup(path string) (*claudeBackup, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read backup %s: %w", path, err)
	}
	var bak claudeBackup
	if err := json.Unmarshal(b, &bak); err != nil {
		return nil, fmt.Errorf("parse backup %s: %w", path, err)
	}
	return &bak, nil
}

// loadClaudeBackupSnapshot parses a backup from the same safe-file snapshot
// that a legacy migration later replaces. The snapshot prevents a concurrent
// backup writer from being overwritten between load and migration.
func loadClaudeBackupSnapshot(path string) (*claudeBackup, *safefile.Snapshot, error) {
	snap, err := safefile.Read(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read backup %s: %w", path, err)
	}
	if !snap.Exists {
		return nil, snap, nil
	}
	var bak claudeBackup
	if err := json.Unmarshal(snap.Data, &bak); err != nil {
		return nil, nil, fmt.Errorf("parse backup %s: %w", path, err)
	}
	return &bak, snap, nil
}

func marshalClaudeBackup(bak *claudeBackup) ([]byte, error) {
	b, err := json.MarshalIndent(bak, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// saveClaudeBackup writes dir 0700 / file 0600: snapshots can embed
// other providers' base URLs and sit next to future codex snapshots
// that embed credentials.
func saveClaudeBackup(path string, bak *claudeBackup) error {
	b, err := marshalClaudeBackup(bak)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// backupCovers reports whether the backup records the key, either as a
// prior value or as a missing marker. A covered key is safe to delete
// and restore on `off`; an uncovered one was never managed by us.
func backupCovers(bak *claudeBackup, key string) bool {
	if bak == nil {
		return false
	}
	if _, ok := bak.Values[key]; ok {
		return true
	}
	for _, k := range bak.Missing {
		if k == key {
			return true
		}
	}
	return false
}

// recordBackupKey adds the key's pre-management state (value or
// missing marker) to the backup. Additive only: never called for a
// key the backup already covers.
func recordBackupKey(bak *claudeBackup, key, cur string, curSet bool) {
	if bak.Values == nil {
		bak.Values = map[string]string{}
	}
	if curSet {
		bak.Values[key] = cur
	} else {
		bak.Missing = append(bak.Missing, key)
	}
}

func cloneClaudeBackup(bak *claudeBackup) *claudeBackup {
	if bak == nil {
		return nil
	}
	cloned := *bak
	cloned.Values = make(map[string]string, len(bak.Values))
	for key, value := range bak.Values {
		cloned.Values[key] = value
	}
	cloned.Missing = append([]string(nil), bak.Missing...)
	if bak.WrittenIdentity != nil {
		identity := *bak.WrittenIdentity
		cloned.WrittenIdentity = &identity
	}
	return &cloned
}

// poisonedBackup reports whether restoring the backup would route the
// harness back through the gateway. The fixed attribution/tool-search
// values do not control routing and can legitimately predate `on`, so
// they are safe to restore.
func (a *claudeAdapter) poisonedBackup(bak *claudeBackup) bool {
	for k, v := range bak.Values {
		if (k == claudeManagedEnvKey || k == claudeSubagentEnvKey) && a.ownedEnvValue(k, v) {
			return true
		}
	}
	return false
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func recordClaudeBackupFile(bak *claudeBackup, snap *safefile.Snapshot) {
	bak.ResolvedPath = snap.ResolvedPath
	if snap.Exists {
		identity := snap.Identity
		bak.WrittenIdentity = &identity
	} else {
		bak.WrittenIdentity = nil
	}
}

func replaceClaudeBackupSnapshot(snap *safefile.Snapshot, bak *claudeBackup) (*safefile.Snapshot, error) {
	if snap == nil {
		return nil, fmt.Errorf("missing backup snapshot")
	}
	b, err := marshalClaudeBackup(bak)
	if err != nil {
		return nil, err
	}
	return snap.Replace(b, 0o600)
}

func replaceMigratedClaudeBackup(snap *safefile.Snapshot, bak *claudeBackup, action string) (*safefile.Snapshot, error) {
	if claudeBeforeMigratedBackupReplace != nil {
		claudeBeforeMigratedBackupReplace(action)
	}
	committed, err := replaceClaudeBackupSnapshot(snap, bak)
	if err == nil {
		return committed, nil
	}
	if safefile.CommitApplied(err) {
		if errors.Is(err, safefile.ErrConflict) {
			return committed, fmt.Errorf("%s was applied, but post-commit verification found that the migrated backup changed concurrently; the concurrent backup was not overwritten: %w", action, err)
		}
		return committed, fmt.Errorf("%s was applied, but its durability or post-commit verification failed: %w", action, err)
	}
	if errors.Is(err, safefile.ErrConflict) {
		return nil, fmt.Errorf("%s refused because the migrated backup changed concurrently and was not overwritten: %w", action, err)
	}
	return nil, fmt.Errorf("%s failed before backup replacement, so the replacement was not applied: %w", action, err)
}

// migrateLegacyLinkedBackupSnapshot upgrades a pre-link-aware backup only when
// its recorded file hash proves that the current linked target is the exact file
// generation the backup describes. The settings snapshot supplies the bytes,
// resolved path, identity, and pre-save verification. The backup snapshot binds
// the parsed backup preimage to its atomic replacement. Migration changes only
// target provenance and persists it before the caller may mutate settings.
func (a *claudeAdapter) migrateLegacyLinkedBackupSnapshot(bak *claudeBackup, backupSnap, settingsSnap *safefile.Snapshot) (*claudeBackup, bool, *safefile.Snapshot, error) {
	if bak == nil {
		return nil, false, backupSnap, nil
	}
	if bak.ConfigPath != a.settingsPath {
		return bak, false, backupSnap, fmt.Errorf("refusing to modify %s because backup %s is for %s, not %s; settings and backup left intact", a.settingsPath, a.backupPath, bak.ConfigPath, a.settingsPath)
	}

	resolvedRecorded := bak.ResolvedPath != ""
	identityRecorded := bak.WrittenIdentity != nil
	if resolvedRecorded != identityRecorded {
		return bak, false, backupSnap, fmt.Errorf("refusing to modify %s because the backup has incomplete linked-target metadata; link and backup left intact", a.settingsPath)
	}
	if resolvedRecorded || !settingsSnap.Linked {
		return bak, false, backupSnap, nil
	}
	if backupSnap == nil || !backupSnap.Exists {
		return bak, false, backupSnap, fmt.Errorf("refusing to migrate the legacy backup for %s because its snapshotted backup file is missing; settings left intact", a.settingsPath)
	}
	if !settingsSnap.Exists || bak.WrittenHash == "" || sha256Hex(settingsSnap.Data) != bak.WrittenHash {
		return bak, false, backupSnap, fmt.Errorf("refusing to modify %s because its legacy backup predates linked-settings support and the current linked target does not exactly match its recorded file generation; link and backup left intact; restore the link to the file managed by the earlier 'claude on' and retry, or restore the backed-up values manually; do not delete the backup", a.settingsPath)
	}

	migrated := cloneClaudeBackup(bak)
	recordClaudeBackupFile(migrated, settingsSnap)
	if err := settingsSnap.Verify(); err != nil {
		return bak, false, backupSnap, fmt.Errorf("verify linked target before migrating legacy backup: %w; link and backup left intact", err)
	}
	committedBackup, err := replaceClaudeBackupSnapshot(backupSnap, migrated)
	if err != nil {
		if safefile.CommitApplied(err) {
			if errors.Is(err, safefile.ErrConflict) {
				return migrated, true, committedBackup, fmt.Errorf("legacy backup migration was applied, but post-commit verification found that the backup changed concurrently; the concurrent backup was not overwritten: %w; settings untouched; inspect %s before retrying", err, a.backupPath)
			}
			return migrated, true, committedBackup, fmt.Errorf("legacy backup migration was applied, but its durability or post-commit verification failed: %w; settings untouched; inspect %s before retrying", err, a.backupPath)
		}
		if errors.Is(err, safefile.ErrConflict) {
			return bak, false, backupSnap, fmt.Errorf("legacy backup changed concurrently and was not overwritten: %w; settings untouched", err)
		}
		return bak, false, backupSnap, fmt.Errorf("atomically migrate legacy backup: %w; settings untouched and backup replacement was not applied", err)
	}
	return migrated, true, committedBackup, nil
}

// migrateLegacyLinkedBackup keeps the focused migration contract used by
// tests and callers that do not perform later backup writes.
func (a *claudeAdapter) migrateLegacyLinkedBackup(bak *claudeBackup, backupSnap, settingsSnap *safefile.Snapshot) (*claudeBackup, bool, error) {
	migrated, changed, _, err := a.migrateLegacyLinkedBackupSnapshot(bak, backupSnap, settingsSnap)
	return migrated, changed, err
}

// claudeBackupMatchesFile binds a link-aware backup to the file committed by
// `on`. Legacy backups remain valid for direct regular paths. A legacy backup
// is not allowed to clean-restore through a link because it does not identify
// the target to which its values belong.
func claudeBackupMatchesFile(bak *claudeBackup, snap *safefile.Snapshot) bool {
	if bak.ResolvedPath == "" || bak.WrittenIdentity == nil {
		return !snap.Linked
	}
	return bak.ResolvedPath == snap.ResolvedPath &&
		*bak.WrittenIdentity == snap.Identity
}

// claudeBackupTargetSafe permits the normal Claude Code rewrite case, where
// the file generation changes at the same logical target. It refuses a linked
// path that now resolves somewhere else, and refuses legacy backups through
// links because those backups have no target provenance.
func claudeBackupTargetSafe(bak *claudeBackup, snap *safefile.Snapshot) bool {
	if bak.ResolvedPath == "" || bak.WrittenIdentity == nil {
		return !snap.Linked
	}
	return bak.ResolvedPath == snap.ResolvedPath
}

// --- on ---------------------------------------------------------------------

func (a *claudeAdapter) on() int {
	lock, err := a.acquireSettingsMutationLock()
	if err != nil {
		fmt.Fprintf(a.out, "claude on: lock settings mutation: %v\n", err)
		return 1
	}
	defer lock.close()

	root, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		fmt.Fprintf(a.out, "claude on: %v\n", err)
		return 1
	}
	raw := snap.Data
	existed := snap.Exists
	env, err := settingsEnv(root)
	if err != nil {
		fmt.Fprintf(a.out, "claude on: %v\n", err)
		return 1
	}
	envExistedPre := env != nil
	baseManaged, _ := a.claudeOnState(env)

	bak, backupSnap, err := loadClaudeBackupSnapshot(a.backupPath)
	if err != nil {
		fmt.Fprintf(a.out, "claude on: %v\n", err)
		return 1
	}
	bak, migratedBackup, backupSnap, err := a.migrateLegacyLinkedBackupSnapshot(bak, backupSnap, snap)
	if err != nil {
		fmt.Fprintf(a.out, "claude on: %v\n", err)
		return 1
	}
	// Adoption is independently durable. Roll back later additive snapshots to
	// this provenance-bearing baseline, never to the ambiguous legacy backup.
	originalBackup := cloneClaudeBackup(bak)
	saveExistingBackup := func(next *claudeBackup, action string) error {
		if !migratedBackup {
			return saveClaudeBackup(a.backupPath, next)
		}
		committedBackup, replaceErr := replaceMigratedClaudeBackup(backupSnap, next, action)
		if committedBackup != nil {
			backupSnap = committedBackup
		}
		return replaceErr
	}
	if bak != nil && !claudeBackupTargetSafe(bak, snap) {
		fmt.Fprintf(a.out, "claude on: refusing to modify %s because it no longer resolves to the target recorded by the backup; link and backup left intact\n",
			a.settingsPath)
		return 1
	}

	changedKeys := make([]string, 0, len(claudeOnEnvKeys))
	for _, key := range claudeOnEnvKeys {
		cur, curSet := envString(env, key)
		if !curSet || cur != a.desiredClaudeEnvValue(key) {
			changedKeys = append(changedKeys, key)
		}
	}
	changed := len(changedKeys) > 0

	// Persist the backup BEFORE modifying the settings file, so no
	// failure or crash window exists in which the file is modified but
	// the user's original state is not durably recorded. The staged
	// WrittenHash is the pre-on file hash: if we crash before the
	// write below, off sees a hash match and its restore is a no-op.
	//
	// A legacy backup may cover only ANTHROPIC_BASE_URL. Extend it
	// additively with both fixed-value keys before upgrading the file.
	// A gateway-owned base URL is never recorded as an original value,
	// but the fixed-value keys are backed up even in a base-only legacy
	// state so custom or manually-added values restore exactly.
	var nb *claudeBackup
	backupChanged := migratedBackup
	if bak == nil && changed {
		nb = &claudeBackup{
			ConfigPath:  a.settingsPath,
			Values:      map[string]string{},
			EnvExisted:  envExistedPre,
			Existed:     existed,
			WrittenHash: sha256Hex(raw),
		}
		for _, key := range claudeOnEnvKeys {
			if key == claudeManagedEnvKey && baseManaged {
				continue
			}
			cur, curSet := envString(env, key)
			recordBackupKey(nb, key, cur, curSet)
		}
		if m, ok := root["model"].(string); ok {
			nb.Model = m
		} else {
			nb.ModelMissing = true
		}
		recordClaudeBackupFile(nb, snap)
		if err := saveClaudeBackup(a.backupPath, nb); err != nil {
			fmt.Fprintf(a.out, "claude on: write backup: %v (settings untouched)\n", err)
			return 1
		}
		backupChanged = true
	} else if bak != nil {
		for _, key := range claudeOnEnvKeys {
			if backupCovers(bak, key) || (key == claudeManagedEnvKey && baseManaged) {
				continue
			}
			cur, curSet := envString(env, key)
			recordBackupKey(bak, key, cur, curSet)
			backupChanged = true
		}
		if backupChanged {
			bak.WrittenHash = sha256Hex(raw)
			recordClaudeBackupFile(bak, snap)
			if err := saveExistingBackup(bak, "migrated backup additive update"); err != nil {
				fmt.Fprintf(a.out, "claude on: write backup: %v (settings untouched)\n", err)
				return 1
			}
		}
	}

	newRaw := raw
	committed := snap
	if changed {
		if env == nil {
			env = map[string]any{}
			root["env"] = env
		}
		for _, key := range changedKeys {
			env[key] = a.desiredClaudeEnvValue(key)
		}
		if claudeBeforeSettingsMutation != nil {
			claudeBeforeSettingsMutation()
		}
		newRaw, committed, err = writeClaudeSettings(snap, root)
		if err != nil {
			if !safefile.CommitApplied(err) {
				switch {
				case nb != nil:
					// Nothing was modified; drop the staged backup.
					_ = os.Remove(a.backupPath)
				case backupChanged && originalBackup != nil:
					if restoreErr := saveExistingBackup(originalBackup, "migrated backup rollback"); restoreErr != nil {
						fmt.Fprintf(a.out, "claude on: warning: settings were untouched, but backup rollback did not complete cleanly: %v\n", restoreErr)
					}
				}
			}
			if safefile.CommitApplied(err) {
				hashTarget := nb
				if hashTarget == nil {
					hashTarget = bak
				}
				if committed == nil {
					if current, readErr := safefile.Read(a.settingsPath); readErr == nil &&
						current.Exists && bytes.Equal(current.Data, newRaw) {
						committed = current
					}
				}
				if hashTarget != nil && committed != nil {
					hashTarget.WrittenHash = sha256Hex(newRaw)
					recordClaudeBackupFile(hashTarget, committed)
					var saveErr error
					if nb != nil {
						saveErr = saveClaudeBackup(a.backupPath, hashTarget)
					} else {
						saveErr = saveExistingBackup(hashTarget, "migrated backup commit binding")
					}
					if saveErr != nil {
						fmt.Fprintf(a.out, "claude on: warning: backup binding after the settings commit did not complete cleanly: %v\n", saveErr)
					}
				}
				fmt.Fprintln(a.out, "claude on: settings changed, but post-commit verification failed; original backup retained")
			}
			fmt.Fprintf(a.out, "claude on: %v\n", err)
			return 1
		}
	}

	if !changed && backupChanged {
		if claudeBeforeSettingsMutation != nil {
			claudeBeforeSettingsMutation()
		}
		if err := snap.Verify(); err != nil {
			if originalBackup != nil {
				if restoreErr := saveExistingBackup(originalBackup, "migrated backup rollback"); restoreErr != nil {
					fmt.Fprintf(a.out, "claude on: warning: settings changed concurrently and backup rollback did not complete cleanly: %v\n", restoreErr)
				}
			}
			fmt.Fprintf(a.out, "claude on: %v\n", err)
			return 1
		}
	}

	// Refresh the drift hash to the file as written at on. A failure
	// here only degrades off to its strip-only-owned drift path (the
	// original user values above are already durable), so warn rather
	// than fail.
	if hashTarget := nb; hashTarget != nil || (bak != nil && (changed || backupChanged)) {
		if hashTarget == nil {
			hashTarget = bak
		}
		hashTarget.WrittenHash = sha256Hex(newRaw)
		recordClaudeBackupFile(hashTarget, committed)
		var refreshErr error
		if nb != nil {
			refreshErr = saveClaudeBackup(a.backupPath, hashTarget)
		} else {
			refreshErr = saveExistingBackup(hashTarget, "migrated backup drift-hash refresh")
		}
		if refreshErr != nil {
			fmt.Fprintf(a.out, "claude on: warning: backup drift-hash refresh did not complete cleanly: %v\n", refreshErr)
		}
	}

	if changed {
		assignments := make([]string, 0, len(claudeOnEnvKeys))
		for _, key := range claudeOnEnvKeys {
			assignments = append(assignments, key+"="+a.desiredClaudeEnvValue(key))
		}
		fmt.Fprintf(a.out, "claude: on (%s in %s)\n", strings.Join(assignments, ", "), a.settingsPath)
		fmt.Fprintln(a.out, "restart Claude Code sessions to pick this up; revert with 'baseten-switch claude off'.")
	} else {
		fmt.Fprintf(a.out, "claude: already on (%s, %s=%s, %s=%s)\n",
			a.desiredURL(), claudeAttributionEnvKey, claudeAttributionValue, claudeToolSearchEnvKey, claudeToolSearchValue)
	}
	return 0
}

// --- off --------------------------------------------------------------------

func (a *claudeAdapter) off() int {
	lock, err := a.acquireSettingsMutationLock()
	if err != nil {
		fmt.Fprintf(a.out, "claude off: lock settings mutation: %v\n", err)
		return 1
	}
	defer lock.close()

	root, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		fmt.Fprintf(a.out, "claude off: %v\n", err)
		return 1
	}
	raw := snap.Data
	existed := snap.Exists
	bak, backupSnap, err := loadClaudeBackupSnapshot(a.backupPath)
	if err != nil {
		fmt.Fprintf(a.out, "claude off: %v\n", err)
		return 1
	}
	if bak != nil && bak.ConfigPath != a.settingsPath {
		// Should be unreachable given the hash-keyed filename; refuse
		// to restore a snapshot of a different file.
		fmt.Fprintf(a.out, "claude off: refusing to modify %s because backup %s is for %s; settings and backup left intact\n",
			a.settingsPath, a.backupPath, bak.ConfigPath)
		return 1
	}
	if bak != nil && a.poisonedBackup(bak) {
		fmt.Fprintf(a.out, "warning: backup %s itself points at a gateway port; discarding it instead of restoring (poisoned-backup guard)\n", a.backupPath)
		if err := os.Remove(a.backupPath); err != nil {
			fmt.Fprintf(a.out, "claude off: remove poisoned backup: %v\n", err)
			return 1
		}
		bak = nil
	}

	if bak == nil {
		return a.offStripOnly(root, snap, nil)
	}
	bak, _, err = a.migrateLegacyLinkedBackup(bak, backupSnap, snap)
	if err != nil {
		fmt.Fprintf(a.out, "claude off: %v\n", err)
		return 1
	}

	if !claudeBackupTargetSafe(bak, snap) {
		fmt.Fprintf(a.out, "claude off: refusing to modify %s because it no longer resolves to the target recorded by 'on'; link and backup left intact\n",
			a.settingsPath)
		return 1
	}

	if !existed {
		// Nothing on disk to restore into. If we created the file at
		// on, gone is exactly the restored state.
		if bak.Existed {
			fmt.Fprintf(a.out, "warning: %s is gone but the backup says it existed before 'on'; original managed values: %v\n", a.settingsPath, bak.Values)
		}
		if err := snap.Remove(); err != nil {
			fmt.Fprintf(a.out, "claude off: verify absent settings %s: %v\n", a.settingsPath, err)
			return 1
		}
		if err := os.Remove(a.backupPath); err != nil {
			fmt.Fprintf(a.out, "claude off: remove backup: %v\n", err)
			return 1
		}
		fmt.Fprintln(a.out, "claude: off (settings file absent; backup cleared)")
		return 0
	}

	if sha256Hex(raw) != bak.WrittenHash || !claudeBackupMatchesFile(bak, snap) {
		env, envErr := settingsEnv(root)
		cur, curSet := envString(env, claudeManagedEnvKey)
		stillManaged := envErr == nil && curSet && a.isGatewayURL(cur)
		if stillManaged {
			// The NORMAL off path, not breakage: Claude Code rewrites
			// settings.json routinely as it runs, so the hash almost
			// always drifts between 'on' and 'off'. Say what happens
			// and that user edits survive; no "warning:" prefix.
			// Present tense: the definitive success line comes from
			// offStripOnly after the write lands, so nothing is
			// claimed done before it is.
			if sha256Hex(raw) == bak.WrittenHash {
				fmt.Fprintf(a.out, "claude off: %s no longer resolves to the file generation recorded by 'on', so stripping only the gateway-owned keys instead of restoring the backup\n",
					a.settingsPath)
			} else {
				fmt.Fprintf(a.out, "claude off: %s changed since 'on' (normal: Claude Code rewrites it as you work), so stripping only the gateway-owned keys instead of restoring the backup; your edits are kept\n",
					a.settingsPath)
			}
			rc := a.offStripOnly(root, snap, bak)
			// Strip-only DELETES gateway-owned values; it never restores
			// bak.Values. Name every pre-'on' value that was not put
			// back so the conservative drift behavior is explicit.
			if rc == 0 {
				a.reportUnrestoredBackupValues(root, bak)
			}
			return rc
		}
		// Genuinely surprising, unlike the drift above: the file no
		// longer points at the gateway at all, so something other than
		// this tool redirected the harness. Keep the warning prefix.
		fmt.Fprintf(a.out, "warning: %s changed since 'on' and no longer points at the gateway; preserving user-owned values, stripping covered Switch values, and clearing the backup. Original %s: %s\n",
			a.settingsPath, claudeManagedEnvKey, backupValueLabel(bak))
		rc := a.offStripOnly(root, snap, bak)
		if rc == 0 {
			a.reportUnrestoredBackupValues(root, bak)
		}
		return rc
	}

	// Clean restore: hash matches the file exactly as written at on.
	env, err := settingsEnv(root)
	if err != nil {
		fmt.Fprintf(a.out, "claude off: %v\n", err)
		return 1
	}
	if env != nil {
		// Delete a managed key only when the backup covers it or its
		// value is gateway-owned: an uncovered user value (e.g. a
		// subagent model set before we ever managed that key) survives.
		for _, k := range claudeManagedEnvKeys {
			cur, ok := envString(env, k)
			if backupCovers(bak, k) || (ok && a.ownedEnvValue(k, cur)) {
				delete(env, k)
			}
		}
		for k, v := range bak.Values {
			env[k] = v
		}
		for _, k := range bak.Missing {
			if _, restored := bak.Values[k]; !restored {
				delete(env, k)
			}
		}
		if len(env) == 0 && !bak.EnvExisted {
			delete(root, "env")
		}
	}
	a.fixModelAlias(root, bak)

	if !bak.Existed && len(root) == 0 {
		if snap.FinalLinked {
			fmt.Fprintf(a.out, "claude off: refusing to remove linked settings target %s; backup retained\n", a.settingsPath)
			return 1
		}
		if err := snap.Remove(); err != nil {
			fmt.Fprintf(a.out, "claude off: remove %s: %v\n", a.settingsPath, err)
			return 1
		}
		fmt.Fprintf(a.out, "claude: off (%s did not exist before 'on'; removed)\n", a.settingsPath)
	} else {
		if _, _, err := writeClaudeSettings(snap, root); err != nil {
			fmt.Fprintf(a.out, "claude off: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.out, "claude: off (restored %s from backup)\n", a.settingsPath)
	}
	if err := os.Remove(a.backupPath); err != nil {
		fmt.Fprintf(a.out, "claude off: remove backup: %v\n", err)
		return 1
	}
	return 0
}

// offStripOnly is the conservative path: delete routing values only
// when they are gateway-owned, and delete fixed-value Claude knobs only
// when a backup proves `on` changed them and they still match what it
// wrote. Never touch preexisting matching, drifted, or unproven values,
// and never create a missing file. bak, when non-nil, is consumed.
func (a *claudeAdapter) offStripOnly(root map[string]any, snap *safefile.Snapshot, bak *claudeBackup) int {
	if !snap.Exists {
		if bak != nil {
			if err := snap.Remove(); err != nil {
				fmt.Fprintf(a.out, "claude off: verify absent settings %s: %v\n", a.settingsPath, err)
				return 1
			}
			os.Remove(a.backupPath)
		}
		fmt.Fprintf(a.out, "claude: off (no settings file at %s; nothing to do)\n", a.settingsPath)
		return 0
	}
	env, err := settingsEnv(root)
	if err != nil {
		fmt.Fprintf(a.out, "claude off: %v\n", err)
		return 1
	}
	changed := false
	var strippedKeys []string
	for _, k := range claudeManagedEnvKeys {
		cur, ok := envString(env, k)
		strip := ok && a.ownedEnvValue(k, cur)
		prior, priorSet := "", false
		if bak != nil {
			prior, priorSet = bak.Values[k]
		}
		// A covered prior value equal to the desired value predated
		// `on`; preserve it during drift instead of claiming ownership.
		if ok && backupCovers(bak, k) &&
			(k == claudeAttributionEnvKey || k == claudeToolSearchEnvKey) &&
			cur == a.desiredClaudeEnvValue(k) &&
			!(priorSet && prior == cur) {
			strip = true
		}
		if strip {
			delete(env, k)
			changed = true
			strippedKeys = append(strippedKeys, k)
		}
	}
	if len(env) == 0 && changed {
		delete(root, "env")
	}
	if a.fixModelAlias(root, bak) {
		changed = true
	}
	if changed {
		if _, _, err := writeClaudeSettings(snap, root); err != nil {
			fmt.Fprintf(a.out, "claude off: %v\n", err)
			return 1
		}
	}
	if bak != nil {
		if !changed {
			if err := snap.Verify(); err != nil {
				fmt.Fprintf(a.out, "claude off: settings changed before backup cleanup: %v\n", err)
				return 1
			}
		}
		if err := os.Remove(a.backupPath); err != nil {
			fmt.Fprintf(a.out, "claude off: remove backup: %v\n", err)
			return 1
		}
	}
	switch {
	case len(strippedKeys) > 0:
		fmt.Fprintf(a.out, "claude: off (stripped gateway-owned %s from %s)\n", strings.Join(strippedKeys, ", "), a.settingsPath)
	case changed:
		fmt.Fprintf(a.out, "claude: off (%s was already user-owned)\n", a.settingsPath)
	default:
		fmt.Fprintf(a.out, "claude: off (noop; %s is not gateway-managed)\n", a.settingsPath)
	}
	return 0
}

// reportUnrestoredBackupValues explains values that the conservative
// drift path could not safely restore. Do not report a value when the
// post-strip settings already contain the same value.
func (a *claudeAdapter) reportUnrestoredBackupValues(root map[string]any, bak *claudeBackup) {
	afterEnv, _ := settingsEnv(root)
	for _, key := range claudeManagedEnvKeys {
		v, ok := bak.Values[key]
		if !ok {
			continue
		}
		if got, set := envString(afterEnv, key); set && got == v {
			continue
		}
		fmt.Fprintf(a.out, "note: your pre-'on' %s %q was NOT restored on this path; re-add it to %s manually if you still need it\n",
			key, v, a.settingsPath)
	}
}

// fixModelAlias handles the the model-discovery contract persisted-alias
// trap: a model picked via the gateway's /v1/models aliases persists
// in settings.json, and Anthropic rejects it once routing is off. If
// the current model looks like ours (claude-baseten-*), reset it to the
// backed-up value or delete it. Reports whether root changed.
func (a *claudeAdapter) fixModelAlias(root map[string]any, bak *claudeBackup) bool {
	m, ok := root["model"].(string)
	if !ok || !strings.HasPrefix(m, claudeAliasPrefix) {
		return false
	}
	if bak != nil && bak.Model != "" && !strings.HasPrefix(bak.Model, claudeAliasPrefix) {
		root["model"] = bak.Model
		fmt.Fprintf(a.out, "note: persisted model %q is a gateway alias; reset to backed-up model %q\n", m, bak.Model)
		return true
	}
	delete(root, "model")
	fmt.Fprintf(a.out, "note: persisted model %q is a gateway alias with no backed-up model; removed it (Anthropic would reject it)\n", m)
	return true
}

// --- subagents ----------------------------------------------------------------

const claudeSubagentsUsage = "usage: baseten-switch claude subagents [<model>|on|inherit]  (inherit: no subagent-specific model rewrite; Claude Code's requested model follows its family mapping or the unmatched-model default; \"off\" is an accepted alias; model: a configured claude-baseten-*/anthropic-baseten-* alias, a raw Baseten slug like org/model, or a native claude-*/anthropic-* id)"

func (a *claudeAdapter) subagents(args []string, stdout io.Writer) int {
	if len(args) == 0 {
		return a.subagentsStatus(stdout)
	}
	opts, positional, err := parseMutationOptions(args)
	if err != nil {
		return failMutation(opts, stdout, mutationResult{Operation: "set_claude_subagents"},
			"usage", fmt.Sprintf("%s: %v", claudeSubagentsUsage, err), false, 2)
	}
	lock, err := acquireConfigMutationLock(a.configPath)
	if err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation: "set_claude_subagents", Client: a.clientName, ConfigPath: a.configPath,
		}, "mutation_locked", err.Error(), true, 1)
	}
	defer lock.close()
	if err := recoverInterruptedExactConfigCommit(a.configPath); err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation: "set_claude_subagents", Client: a.clientName, ConfigPath: a.configPath,
		}, "commit_recovery_failed", err.Error(), true, 1)
	}
	journaled, err := a.requiresJournaledMutation(opts)
	if err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation: "set_claude_subagents", Client: a.clientName, ConfigPath: a.configPath,
		}, "config_load_failed", err.Error(), false, 1)
	}
	switch {
	case len(positional) != 1:
		return failMutation(opts, stdout, mutationResult{
			Operation: "set_claude_subagents", Client: a.clientName, ConfigPath: a.configPath,
		}, "usage", claudeSubagentsUsage, false, 2)
	case positional[0] == "off", positional[0] == "inherit":
		// "inherit" is the preferred name for the state (no
		// subagent-specific model rewrite); "off" is the config wire
		// value and stays accepted.
		return a.subagentsRouting("off", "inherit", opts, stdout, journaled)
	case positional[0] == "on":
		return a.subagentsRouting("on", "on", opts, stdout, journaled)
	default:
		return a.subagentsSetModel(positional[0], opts, stdout, journaled)
	}
}

// subagentsSetModel writes subagent_model (and routing on) to
// gateway.yaml via the comment-preserving scalar editor, SIGHUPs the
// running router, verifies the live state, and prints one status row.
// With the router down the config is still written and a notice says it
// applies at the next start. No restart message: this is live.
func (a *claudeAdapter) subagentsSetModel(model string, opts mutationOptions, stdout io.Writer, journaled bool) int {
	class, ok := classifySubagentModel(model)
	if !ok {
		return failMutation(opts, stdout, mutationResult{
			OperationID: opts.OperationID, Operation: "set_claude_subagents",
			RequestedTarget: model, Client: a.clientName, Key: "subagents", ConfigPath: a.configPath,
		}, "invalid_subagent_target",
			fmt.Sprintf("claude subagents: unrecognized model %q\n%s", model, claudeSubagentsUsage), false, 2)
	}
	if class == subagentClassAlias {
		if _, configured := a.modelAliases[model]; !configured {
			fix := "add it to model_aliases for the " + a.clientName + " client in gateway.yaml and SIGHUP the router (kill -HUP $(cat ~/.config/baseten-switch/gateway.pid)), or pick a configured alias"
			message := ""
			if len(a.modelAliases) == 0 {
				message = fmt.Sprintf("claude subagents: unknown gateway model %q: the %s client has no model_aliases configured. Fix: %s", model, a.clientName, fix)
			} else {
				message = fmt.Sprintf("claude subagents: unknown gateway model %q: configured model_aliases are [%s]. Fix: %s", model, strings.Join(sortedAliasIDs(a.modelAliases), ", "), fix)
			}
			return failMutation(opts, stdout, mutationResult{
				OperationID: opts.OperationID, Operation: "set_claude_subagents",
				RequestedTarget: model, Client: a.clientName, Key: "subagents", ConfigPath: a.configPath,
			}, "invalid_subagent_target", message, false, 1)
		}
	}

	if !opts.JSON {
		// Warnings (never refusals): a config edit is harmless.
		a.subagentsWarnings()
	}

	set := map[string]string{"subagent_model": model, "subagent_routing": "on"}
	if journaled {
		humanSuccess := fmt.Sprintf("claude subagents: on (subagent_model=%s in %s)", model, a.configPath)
		switch class {
		case subagentClassSlug:
			humanSuccess = fmt.Sprintf("note: %q is a raw Baseten slug; subagent requests route explicitly to Baseten.\n%s", model, humanSuccess)
		case subagentClassNative:
			humanSuccess = fmt.Sprintf("note: %q is a native id; subagent requests route per the switch position.\n%s", model, humanSuccess)
		}
		return a.runClaudeJournaledMutationLocked(opts, stdout, journaledMutationSpec{
			Operation:       "set_claude_subagents",
			RequestedTarget: model,
			Client:          a.clientName,
			Key:             "subagents",
			Apply: func(path string) error {
				return config.SetClientScalars(path, a.clientName, set)
			},
			HumanSuccess: humanSuccess,
		})
	}
	if err := config.SetClientScalars(a.configPath, a.clientName, set); err != nil {
		fmt.Fprintf(a.out, "claude subagents: %v\n", err)
		return 1
	}

	switch class {
	case subagentClassSlug:
		fmt.Fprintf(a.out, "note: %q is a raw Baseten slug; subagent requests route explicitly to Baseten.\n", model)
	case subagentClassNative:
		fmt.Fprintf(a.out, "note: %q is a native id; subagent requests route per the switch position.\n", model)
	}
	a.subagentsReloadAndVerify(model, "on")
	fmt.Fprintf(a.out, "claude subagents: on (subagent_model=%s in %s)\n", model, a.configPath)
	return 0
}

// subagentsRouting flips subagent_routing (on or off), keeping
// subagent_model. "on" with no subagent_model configured exits 1 naming
// the fix. Same SIGHUP + verify + row as subagentsSetModel. The "off"
// state is displayed as inherit: Baseten Switch performs no subagent-specific
// rewrite, so the requested model follows its family mapping or the
// unmatched-model default. Only the config wire value stays "off".
func (a *claudeAdapter) subagentsRouting(want, requestedTarget string, opts mutationOptions, stdout io.Writer, journaled bool) int {
	f, err := config.Load(a.configPath)
	if err != nil {
		fmt.Fprintf(a.out, "claude subagents: %v\n", err)
		return 1
	}
	var cur *config.Client
	for i := range f.Clients {
		if f.Clients[i].Name == a.clientName {
			cur = &f.Clients[i]
			break
		}
	}
	if cur == nil {
		fmt.Fprintf(a.out, "claude subagents: no client named %q in %s\n", a.clientName, a.configPath)
		return 1
	}
	if want == "on" && cur.SubagentModel == "" {
		return failMutation(opts, stdout, mutationResult{
			OperationID: opts.OperationID, Operation: "set_claude_subagents",
			RequestedTarget: requestedTarget, Client: a.clientName, Key: "subagents",
			ConfigPath: a.configPath,
		}, "invalid_subagent_state",
			"claude subagents: cannot enable routing with no subagent_model configured; set one with 'baseten-switch claude subagents <model>'",
			false, 1)
	}
	// "off" with no subagent_model is already the effective state, and
	// writing subagent_routing: off alone would leave a config the router
	// refuses (routing set while model empty; validateSubagentConfig).
	// Short-circuit as a noop so the file stays loadable.
	if want == "off" && cur.SubagentModel == "" {
		if journaled {
			return a.runClaudeJournaledMutationLocked(opts, stdout, journaledMutationSpec{
				Operation:       "set_claude_subagents",
				RequestedTarget: "inherit",
				Client:          a.clientName,
				Key:             "subagents",
				Apply:           func(string) error { return nil },
				HumanSuccess:    "claude subagents: already inherit (no subagent_model configured; no subagent-specific model rewrite; requested models follow family mappings or the unmatched-model default)",
			})
		}
		fmt.Fprintf(a.out, "claude subagents: already inherit (no subagent_model configured; no subagent-specific model rewrite; requested models follow family mappings or the unmatched-model default; set one with 'baseten-switch claude subagents <model>')\n")
		return 0
	}
	// Absent routing means on when a model is set, so "on" with an
	// empty routing is already the effective state.
	effective := cur.SubagentRouting
	if effective == "" && cur.SubagentModel != "" {
		effective = "on"
	}
	set := map[string]string{"subagent_routing": want}
	if journaled {
		human := fmt.Sprintf("claude subagents: %s (subagent_model=%s)", requestedTarget, orDash(cur.SubagentModel))
		if want == "off" {
			human = fmt.Sprintf("claude subagents: inherit (no subagent-specific model rewrite; requested models follow family mappings or the unmatched-model default; subagent_model=%s)", orDash(cur.SubagentModel))
		}
		return a.runClaudeJournaledMutationLocked(opts, stdout, journaledMutationSpec{
			Operation:       "set_claude_subagents",
			RequestedTarget: requestedTarget,
			Client:          a.clientName,
			Key:             "subagents",
			Apply: func(path string) error {
				return config.SetClientScalars(path, a.clientName, set)
			},
			HumanSuccess: human,
		})
	}
	if effective == want {
		if want == "off" {
			fmt.Fprintf(a.out, "claude subagents: already inherit (no subagent-specific model rewrite; requested models follow family mappings or the unmatched-model default; subagent_routing=off, subagent_model=%s)\n", orDash(cur.SubagentModel))
		} else {
			fmt.Fprintf(a.out, "claude subagents: already %s (subagent_routing=%s, subagent_model=%s)\n", want, effective, orDash(cur.SubagentModel))
		}
		return 0
	}

	if !opts.JSON {
		a.subagentsWarnings()
	}

	if err := config.SetClientScalars(a.configPath, a.clientName, set); err != nil {
		fmt.Fprintf(a.out, "claude subagents: %v\n", err)
		return 1
	}
	a.subagentsReloadAndVerify(cur.SubagentModel, want)
	if want == "off" {
		fmt.Fprintf(a.out, "claude subagents: inherit (no subagent-specific model rewrite; requested models follow family mappings or the unmatched-model default; subagent_routing=off, subagent_model=%s)\n", orDash(cur.SubagentModel))
	} else {
		fmt.Fprintf(a.out, "claude subagents: %s (subagent_routing=%s, subagent_model=%s)\n", want, want, orDash(cur.SubagentModel))
	}
	return 0
}

// subagentsReloadAndVerify SIGHUPs the running router so the config
// hot-reloads, then polls admin status until the client reports the
// expected subagent_model AND routing (or the timeout expires). With the
// router down it prints a notice that the config applies at the next
// start. No restart message: live sessions pick changes up on their
// next sidechain request.
func (a *claudeAdapter) subagentsReloadAndVerify(wantModel, wantRouting string) {
	switch state, pid := classifyPidfile(gatewayPidfilePath()); state {
	case pidfileAlive:
		if _, err := signalExpectedRouter(pid); err != nil {
			fmt.Fprintf(a.out, "warning: could not SIGHUP router pid %d: %v; config saved, reload or restart the router to apply\n", pid, err)
			return
		}
		adminAddr := envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)
		if waitSubagentApplied(adminAddr, a.clientName, wantModel, wantRouting, routeApplyTimeout) {
			fmt.Fprintf(a.out, "router reloaded (SIGHUP pid %d); subagent config verified live\n", pid)
		} else {
			fmt.Fprintf(a.out, "note: SIGHUP sent (pid %d) but the router did not confirm the subagent config within %s; check 'baseten-switch status'\n", pid, routeApplyTimeout)
		}
	default:
		fmt.Fprintf(a.out, "router not running; %s updated and applies at next start\n", a.configPath)
	}
}

// waitSubagentApplied polls the admin status until the named client
// reports the expected subagent_model AND routing, or the timeout
// expires. Absent routing on the polled value means "on" when the
// polled model is non-empty, so an "on" flip that leaves the file field
// absent still verifies. Modeled on waitRoutesApplied in switchverbs.go.
func waitSubagentApplied(adminAddr, clientName, wantModel, wantRouting string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		var payload struct {
			Clients []struct {
				Name            string `json:"name"`
				SubagentModel   string `json:"subagent_model"`
				SubagentRouting string `json:"subagent_routing"`
			} `json:"clients"`
		}
		if err := getJSON(adminAddr, "/v1/admin/status", &payload); err == nil {
			for _, c := range payload.Clients {
				if c.Name != clientName {
					continue
				}
				if wantModel != "" && c.SubagentModel != wantModel {
					continue
				}
				polledRouting := c.SubagentRouting
				if polledRouting == "" && c.SubagentModel != "" {
					polledRouting = "on"
				}
				if polledRouting == wantRouting {
					return true
				}
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// subagentsWarnings emits non-fatal warnings before a config edit:
// claude wiring off (the toggle has no effect until "claude on"), and
// CLAUDE_CODE_SUBAGENT_MODEL present in the settings env block or the
// process env (double management).
func (a *claudeAdapter) subagentsWarnings() {
	root, _, existed, err := loadClaudeSettings(a.settingsPath)
	wiringWarned := false
	if err == nil && existed {
		if env, envErr := settingsEnv(root); envErr == nil {
			if v, ok := envString(env, claudeSubagentEnvKey); ok && v != "" {
				fmt.Fprintf(a.out, "warning: %s is set in %s; the gateway rewrite overrides it while toggled on, and the pin still routes explicitly while toggled off. Suggest removing it ('baseten-switch claude off' strips a gateway-owned value).\n", claudeSubagentEnvKey, a.settingsPath)
			}
			if base, ok := envString(env, claudeManagedEnvKey); ok && !a.isGatewayURL(base) {
				fmt.Fprintf(a.out, "warning: %s does not point at the gateway; the subagent toggle has no effect until 'baseten-switch claude on'.\n", claudeManagedEnvKey)
				wiringWarned = true
			}
		}
	}
	if v := os.Getenv(claudeSubagentEnvKey); v != "" {
		fmt.Fprintf(a.out, "warning: %s is set in the process environment (%s); double management with the gateway config. Suggest unsetting it.\n", claudeSubagentEnvKey, v)
	}
	// Wiring is off when no settings base URL points at the gateway and
	// no shell env var does either.
	if !wiringWarned {
		shell := os.Getenv(claudeManagedEnvKey)
		if shell == "" || !a.isGatewayURL(shell) {
			if root == nil || !existed {
				fmt.Fprintf(a.out, "warning: claude wiring is off; the subagent toggle has no effect until 'baseten-switch claude on'.\n")
			} else if env, envErr := settingsEnv(root); envErr == nil {
				if base, ok := envString(env, claudeManagedEnvKey); !ok || !a.isGatewayURL(base) {
					fmt.Fprintf(a.out, "warning: claude wiring is off; the subagent toggle has no effect until 'baseten-switch claude on'.\n")
				}
			}
		}
	}
}

// subagentsStatus prints the subagent state from gateway.yaml (model,
// class, routing, plus wiring state); always exit 0 (it is a report).
func (a *claudeAdapter) subagentsStatus(stdout io.Writer) int {
	f, err := config.Load(a.configPath)
	if err != nil {
		fmt.Fprintf(a.out, "claude subagents: %v\n", err)
		return 1
	}
	var cur *config.Client
	for i := range f.Clients {
		if f.Clients[i].Name == a.clientName {
			cur = &f.Clients[i]
			break
		}
	}
	if cur == nil {
		fmt.Fprintf(a.out, "claude subagents: no client named %q in %s\n", a.clientName, a.configPath)
		return 1
	}
	// Bare `subagents` prints wiring state per spec: the warnings helper
	// emits the wiring-off and env-var double-management notices so they
	// accompany the status label. Keep exit code semantics unchanged.
	a.subagentsWarnings()
	fmt.Fprintf(stdout, "claude subagents: %s\n", a.subagentStateLabel(cur))
	return 0
}

// subagentStateLabel renders one line of subagent state for `claude
// status` and `claude subagents`: unmanaged, or the model plus its class,
// routing, and (for aliases) whether the config knows it. Routing "off"
// is displayed as inherit (no subagent-specific model rewrite); the
// config wire value stays "off".
func (a *claudeAdapter) subagentStateLabel(c *config.Client) string {
	if c.SubagentModel == "" {
		return "unmanaged (no subagent_model in gateway.yaml)"
	}
	class, ok := classifySubagentModel(c.SubagentModel)
	if !ok {
		return fmt.Sprintf("%s (unrecognized; fix in gateway.yaml)", c.SubagentModel)
	}
	routing := "on"
	if c.SubagentRouting == "off" {
		routing = "inherit"
	}
	switch class {
	case subagentClassAlias:
		if _, configured := a.modelAliases[c.SubagentModel]; configured {
			return fmt.Sprintf("%s (%s, configured in model_aliases, routing %s)", c.SubagentModel, class, routing)
		}
		return fmt.Sprintf("%s (%s, NOT in model_aliases; requests fail loud at the gateway, routing %s)", c.SubagentModel, class, routing)
	case subagentClassSlug:
		return fmt.Sprintf("%s (%s; routes explicitly to Baseten, routing %s)", c.SubagentModel, class, routing)
	default:
		return fmt.Sprintf("%s (%s; routes per the switch position, routing %s)", c.SubagentModel, class, routing)
	}
}

func sortedAliasIDs(aliases map[string]string) []string {
	ids := make([]string, 0, len(aliases))
	for id := range aliases {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// --- route (family pins) ---------------------------------------------------
//
// `baseten-switch claude route` manages the per-client model_routes overlay
// (config/schema.md): pins that override the switch per FAMILY
// (fable, opus, sonnet, haiku). Mechanics mirror `claude subagents`:
// validate against
// gateway.yaml, splice via the comment-preserving map editor, SIGHUP the
// running router, verify via admin status, print the affected row. No
// restart message: this is live. Wiring-off and env-var warnings are
// warnings, never refusals.

// claudeFamilies is the hardcoded family set (config/schema.md
// open question 1: hardcode the four, extend deliberately). instant and
// mythos are reserved for later and not accepted as pin keys.
var claudeFamilies = []string{"fable", "opus", "sonnet", "haiku"}

// claudeFamilySet is the lookup table for isFamilyKey.
var claudeFamilySet = func() map[string]bool {
	m := make(map[string]bool, len(claudeFamilies))
	for _, f := range claudeFamilies {
		m[f] = true
	}
	return m
}()

// isFamilyKey reports whether key is a bare family word.
func isFamilyKey(key string) bool {
	return claudeFamilySet[key]
}

// routeTargetClasses mirrors the subagent model classes for family targets.
const (
	routeTargetNative = "native"
)

// validateRouteTarget classifies a family target value and reports whether
// it is valid for the client's model_aliases. "native" is always valid;
// a configured alias is always valid; a slug (contains "/") is always
// valid. Returns the class for status notes. The configured-alias map
// is checked BEFORE the alias-namespace gate, matching the router's
// load order (validateModelRoutes in cmd/gateway), so a configured
// alias with an unusual name is accepted everywhere the router
// accepts it.
func validateRouteTarget(target string, aliases map[string]string) (string, bool) {
	if target == routeTargetNative {
		return routeTargetNative, true
	}
	if _, configured := aliases[target]; configured {
		return subagentClassAlias, true
	}
	if gateway.InAliasNamespace(target) {
		return subagentClassAlias, false
	}
	if strings.Contains(target, "/") {
		return subagentClassSlug, true
	}
	return "", false
}

// validRouteTarget reports whether a family target value is valid for the
// client's model_aliases (the boolean form of validateRouteTarget). The
// doctor check uses it to flag hand-edited invalid entries the router
// would refuse at load.
func validRouteTarget(target string, aliases map[string]string) bool {
	_, ok := validateRouteTarget(target, aliases)
	return ok
}

const claudeRouteUsage = "usage: baseten-switch claude route [<family> <target|default>]  (family: one of fable, opus, sonnet, haiku; target: native, a configured alias, a raw Baseten slug like org/model, or default to remove the pin)"

func (a *claudeAdapter) route(args []string, stdout io.Writer) int {
	if len(args) == 0 {
		return a.routeStatus(stdout)
	}
	opts, positional, err := parseMutationOptions(args)
	if err != nil {
		return failMutation(opts, stdout, mutationResult{Operation: "set_claude_route"},
			"usage", fmt.Sprintf("%s: %v", claudeRouteUsage, err), false, 2)
	}
	if len(positional) != 2 {
		return failMutation(opts, stdout, mutationResult{
			Operation: "set_claude_route", Client: a.clientName, ConfigPath: a.configPath,
		}, "usage", claudeRouteUsage, false, 2)
	}
	lock, err := acquireConfigMutationLock(a.configPath)
	if err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation: "set_claude_route", Client: a.clientName, ConfigPath: a.configPath,
		}, "mutation_locked", err.Error(), true, 1)
	}
	defer lock.close()
	if err := recoverInterruptedExactConfigCommit(a.configPath); err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation: "set_claude_route", Client: a.clientName, ConfigPath: a.configPath,
		}, "commit_recovery_failed", err.Error(), true, 1)
	}
	journaled, err := a.requiresJournaledMutation(opts)
	if err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation: "set_claude_route", Client: a.clientName, ConfigPath: a.configPath,
		}, "config_load_failed", err.Error(), false, 1)
	}
	return a.routeSet(positional[0], positional[1], stdout, opts, journaled)
}

// routeStatus prints the pin table from gateway.yaml: one row per family
// (pin or "default (follow switch)").
func (a *claudeAdapter) routeStatus(stdout io.Writer) int {
	f, err := config.Load(a.configPath)
	if err != nil {
		fmt.Fprintf(a.out, "claude route: %v\n", err)
		return 1
	}
	var cur *config.Client
	for i := range f.Clients {
		if f.Clients[i].Name == a.clientName {
			cur = &f.Clients[i]
			break
		}
	}
	if cur == nil {
		fmt.Fprintf(a.out, "claude route: no client named %q in %s\n", a.clientName, a.configPath)
		return 1
	}
	// Bare `route` prints wiring warnings so they accompany the table.
	a.routeWarnings()
	switchState := "Off"
	if f.Global.RoutingEnabled != nil && *f.Global.RoutingEnabled {
		switchState = "On"
	}
	fmt.Fprintf(stdout, "claude route: client %s (global routing %s)\n", a.clientName, switchState)
	for _, fam := range claudeFamilies {
		pin, pinned := cur.ModelRoutes[fam]
		if !pinned {
			fmt.Fprintf(stdout, "  %-22s default (follow switch)\n", fam)
			continue
		}
		fmt.Fprintf(stdout, "  %-22s %s\n", fam, pin)
	}
	return 0
}

// routeSet validates the key and target, writes the pin (or removes it
// for "default"), SIGHUPs the router, verifies via admin status, and
// prints the affected row. With the router down the config is still
// written and a notice says it applies at the next start.
func (a *claudeAdapter) routeSet(key, target string, stdout io.Writer, opts mutationOptions, journaled bool) int {
	if !isFamilyKey(key) {
		message := fmt.Sprintf("claude route: %q is not a supported family (%s)\n%s",
			key, strings.Join(claudeFamilies, ", "), claudeRouteUsage)
		return failMutation(opts, stdout, mutationResult{
			OperationID: opts.OperationID, Operation: "set_claude_route",
			RequestedTarget: target, Client: a.clientName, Key: key, ConfigPath: a.configPath,
		}, "invalid_route_key", message, false, 1)
	}

	// "default" removes the pin. On an absent pin, report already-default
	// and skip the write (never write config the router would refuse;
	// removal of a missing key is safe but skip the write anyway, the
	// subagents off-without-model lesson).
	if target == "default" {
		return a.routeRemove(key, stdout, opts, journaled)
	}

	class, ok := validateRouteTarget(target, a.modelAliases)
	if !ok {
		fix := "add it to model_aliases for the " + a.clientName + " client in gateway.yaml and SIGHUP the router (kill -HUP $(cat ~/.config/baseten-switch/gateway.pid)), or pick a configured alias"
		message := ""
		if len(a.modelAliases) == 0 {
			message = fmt.Sprintf("claude route: unknown gateway model %q: the %s client has no model_aliases configured. Fix: %s", target, a.clientName, fix)
		} else {
			message = fmt.Sprintf("claude route: unknown gateway model %q: configured model_aliases are [%s]. Fix: %s", target, strings.Join(sortedAliasIDs(a.modelAliases), ", "), fix)
		}
		return failMutation(opts, stdout, mutationResult{
			OperationID: opts.OperationID, Operation: "set_claude_route",
			RequestedTarget: target, Client: a.clientName, Key: key, ConfigPath: a.configPath,
		}, "invalid_route_target", message, false, 1)
	}

	if !opts.JSON {
		// Warnings (never refusals): a config edit is harmless.
		a.routeWarnings()
	}

	if journaled {
		humanSuccess := fmt.Sprintf("claude route: %s -> %s (in %s)", key, target, a.configPath)
		if class == subagentClassSlug {
			humanSuccess = fmt.Sprintf("note: %q is a raw Baseten slug; pinned requests route explicitly to Baseten.\n%s", target, humanSuccess)
		}
		return a.runClaudeJournaledMutationLocked(opts, stdout, journaledMutationSpec{
			Operation:       "set_claude_route",
			RequestedTarget: target,
			Client:          a.clientName,
			Key:             key,
			Apply: func(path string) error {
				return config.SetClientMapEntries(path, a.clientName, "model_routes", map[string]string{key: target})
			},
			HumanSuccess: humanSuccess,
		})
	}
	if err := config.SetClientMapEntries(a.configPath, a.clientName, "model_routes", map[string]string{key: target}); err != nil {
		fmt.Fprintf(a.out, "claude route: %v\n", err)
		return 1
	}
	switch class {
	case subagentClassSlug:
		fmt.Fprintf(a.out, "note: %q is a raw Baseten slug; pinned requests route explicitly to Baseten.\n", target)
	}
	a.routeReloadAndVerify(key, target)
	fmt.Fprintf(a.out, "claude route: %s -> %s (in %s)\n", key, target, a.configPath)
	return 0
}

// routeRemove removes a pin via RemoveClientMapEntries. On an absent pin
// it reports already-default and skips the write.
func (a *claudeAdapter) routeRemove(key string, stdout io.Writer, opts mutationOptions, journaled bool) int {
	f, err := config.Load(a.configPath)
	if err != nil {
		fmt.Fprintf(a.out, "claude route: %v\n", err)
		return 1
	}
	var cur *config.Client
	for i := range f.Clients {
		if f.Clients[i].Name == a.clientName {
			cur = &f.Clients[i]
			break
		}
	}
	if cur == nil {
		fmt.Fprintf(a.out, "claude route: no client named %q in %s\n", a.clientName, a.configPath)
		return 1
	}
	if _, pinned := cur.ModelRoutes[key]; !pinned {
		fmt.Fprintf(a.out, "claude route: %s already default (no pin in gateway.yaml)\n", key)
		return 0
	}
	if !opts.JSON {
		// Warnings (never refusals): a config edit is harmless.
		a.routeWarnings()
	}
	if journaled {
		return a.runClaudeJournaledMutationLocked(opts, stdout, journaledMutationSpec{
			Operation:       "set_claude_route",
			RequestedTarget: "default",
			Client:          a.clientName,
			Key:             key,
			Apply: func(path string) error {
				return config.RemoveClientMapEntries(path, a.clientName, "model_routes", []string{key})
			},
			HumanSuccess: fmt.Sprintf("claude route: %s -> default (pin removed from %s)", key, a.configPath),
		})
	}
	if _, pinned := cur.ModelRoutes[key]; !pinned {
		fmt.Fprintf(a.out, "claude route: %s already default (no pin in gateway.yaml)\n", key)
		return 0
	}
	if err := config.RemoveClientMapEntries(a.configPath, a.clientName, "model_routes", []string{key}); err != nil {
		fmt.Fprintf(a.out, "claude route: %v\n", err)
		return 1
	}
	a.routeReloadAndVerify(key, "")
	fmt.Fprintf(a.out, "claude route: %s -> default (pin removed from %s)\n", key, a.configPath)
	return 0
}

// routeReloadAndVerify SIGHUPs the running router so the config
// hot-reloads, then polls admin status until the client reports the
// expected model_routes end state (key present with wantTarget, or key
// absent when wantTarget is empty for a removal). With the router down
// it prints a notice that the config applies at the next start.
func (a *claudeAdapter) routeReloadAndVerify(key, wantTarget string) {
	switch state, pid := classifyPidfile(gatewayPidfilePath()); state {
	case pidfileAlive:
		if _, err := signalExpectedRouter(pid); err != nil {
			fmt.Fprintf(a.out, "warning: could not SIGHUP router pid %d: %v; config saved, reload or restart the router to apply\n", pid, err)
			return
		}
		adminAddr := envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)
		if waitRouteApplied(adminAddr, a.clientName, key, wantTarget, routeApplyTimeout) {
			fmt.Fprintf(a.out, "router reloaded (SIGHUP pid %d); model route verified live\n", pid)
		} else {
			fmt.Fprintf(a.out, "note: SIGHUP sent (pid %d) but the router did not confirm the model route within %s; check 'baseten-switch status'\n", pid, routeApplyTimeout)
		}
	default:
		fmt.Fprintf(a.out, "router not running; %s updated and applies at next start\n", a.configPath)
	}
}

// waitRouteApplied polls the admin status until the named client reports
// the expected model_routes end state for one key: present with wantTarget
// (a set), or absent (wantTarget empty, a removal). Modeled on
// waitSubagentApplied in this file.
func waitRouteApplied(adminAddr, clientName, key, wantTarget string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		var payload struct {
			Clients []struct {
				Name        string            `json:"name"`
				ModelRoutes map[string]string `json:"model_routes"`
			} `json:"clients"`
		}
		if err := getJSON(adminAddr, "/v1/admin/status", &payload); err == nil {
			for _, c := range payload.Clients {
				if c.Name != clientName {
					continue
				}
				got, ok := c.ModelRoutes[key]
				if wantTarget == "" {
					if !ok {
						return true
					}
					continue
				}
				if ok && got == wantTarget {
					return true
				}
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// routeWarnings emits non-fatal warnings before a route config edit,
// mirroring subagentsWarnings: claude wiring off (the pins have no
// effect until "claude on"), and the ANTHROPIC_MODEL /
// ANTHROPIC_DEFAULT_*_MODEL env vars that change what the harness
// requests upstream of gateway pins (the fireconnect survey hazard).
func (a *claudeAdapter) routeWarnings() {
	root, _, existed, err := loadClaudeSettings(a.settingsPath)
	wiringWarned := false
	if err == nil && existed {
		if env, envErr := settingsEnv(root); envErr == nil {
			if base, ok := envString(env, claudeManagedEnvKey); ok && !a.isGatewayURL(base) {
				fmt.Fprintf(a.out, "warning: %s does not point at the gateway; family pins have no effect until 'baseten-switch claude on'.\n", claudeManagedEnvKey)
				wiringWarned = true
			}
		}
	}
	if !wiringWarned {
		shell := os.Getenv(claudeManagedEnvKey)
		if shell == "" || !a.isGatewayURL(shell) {
			if root == nil || !existed {
				fmt.Fprintf(a.out, "warning: claude wiring is off; family pins have no effect until 'baseten-switch claude on'.\n")
			} else if env, envErr := settingsEnv(root); envErr == nil {
				if base, ok := envString(env, claudeManagedEnvKey); !ok || !a.isGatewayURL(base) {
					fmt.Fprintf(a.out, "warning: claude wiring is off; family pins have no effect until 'baseten-switch claude on'.\n")
				}
			}
		}
	}
	a.routeEnvWarnings(root, existed)
}

// routeEnvWarnings warns about ANTHROPIC_MODEL and the
// ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL env vars that change what
// the harness requests upstream of family pins (double management, the
// fireconnect survey hazard). root/existed are the already-loaded
// settings (nil when unreadable) so the settings scan reuses the read.
// The warning names only the vars actually present (the sources list
// carries per-source detail), never the full checked set.
func (a *claudeAdapter) routeEnvWarnings(root map[string]any, existed bool) {
	present, sources := routeEnvSlotFindings(root, existed)
	if len(sources) > 0 {
		fmt.Fprintf(a.out, "warning: %s set in %s; these change what the harness requests upstream of family pins (double management). Suggest removing them.\n",
			strings.Join(present, ", "), strings.Join(sources, ", "))
	}
}

// routeEnvSlotFindings scans the settings env block (root/existed, when
// readable) and the process env for the routeEnvSlotKeys. It returns
// the distinct key names actually present (slot order, settings scan
// first) and the per-source findings ("KEY (settings env: ...)"). Both
// the route verb's warning and the doctor model_env check consume it so
// the two surfaces name the same vars.
func routeEnvSlotFindings(root map[string]any, existed bool) (present, sources []string) {
	checked := routeEnvSlotKeys()
	seen := make(map[string]bool)
	note := func(k, src string) {
		sources = append(sources, src)
		if !seen[k] {
			seen[k] = true
			present = append(present, k)
		}
	}
	if root != nil && existed {
		if env, envErr := settingsEnv(root); envErr == nil {
			for _, k := range checked {
				if v, ok := envString(env, k); ok && v != "" {
					note(k, fmt.Sprintf("%s (settings env: %q)", k, v))
				}
			}
		}
	}
	for _, k := range checked {
		if v := os.Getenv(k); v != "" {
			note(k, fmt.Sprintf("%s (process env: %q)", k, v))
		}
	}
	return present, sources
}

// routeEnvSlotKeys is the ordered list of harness env vars that change
// the requested model upstream of gateway pins (fireconnect survey).
func routeEnvSlotKeys() []string {
	return []string{
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	}
}

// --- status -----------------------------------------------------------------

func (a *claudeAdapter) status(stdout io.Writer) int {
	root, snap, err := readClaudeSettings(a.settingsPath)
	if err != nil {
		fmt.Fprintf(a.out, "claude status: %v\n", err)
		return 1
	}
	existed := snap.Exists
	var env map[string]any
	if existed {
		var envErr error
		env, envErr = settingsEnv(root)
		if envErr != nil {
			fmt.Fprintf(a.out, "claude status: %v\n", envErr)
			return 1
		}
	}
	baseManaged, complete := a.claudeOnState(env)
	backupPresent := false
	if _, err := os.Stat(a.backupPath); err == nil {
		backupPresent = true
	}
	// Subagent state now reads from gateway.yaml, not settings.
	subLabel := "unmanaged (no subagent_model in gateway.yaml)"
	if f, cfgErr := config.Load(a.configPath); cfgErr == nil {
		for i := range f.Clients {
			if f.Clients[i].Name == a.clientName {
				subLabel = a.subagentStateLabel(&f.Clients[i])
				break
			}
		}
	}
	state := "off (not gateway-managed)"
	if complete {
		state = "on (gateway-managed)"
	} else if baseManaged {
		state = "incomplete (base URL is gateway-managed; run 'baseten-switch claude on' to repair settings)"
	}
	fmt.Fprintf(stdout, "claude: %s\n", state)
	fmt.Fprintf(stdout, "  settings:            %s (%s)\n", a.settingsPath, safefileTopologyLabel(snap))
	for _, key := range claudeOnEnvKeys {
		cur, _ := envString(env, key)
		fmt.Fprintf(stdout, "  %-21s %s\n", key+":", orDash(cur))
	}
	fmt.Fprintf(stdout, "  subagents:           %s\n", subLabel)
	fmt.Fprintf(stdout, "  door port:           %s (target %s)\n", a.desiredPort, a.desiredURL())
	fmt.Fprintf(stdout, "  backup:              %s\n", map[bool]string{true: "present (" + a.backupPath + ")", false: "none"}[backupPresent])
	if complete {
		return 0
	}
	return statusExitOff
}

func safefileTopologyLabel(snap *safefile.Snapshot) string {
	if snap == nil {
		return "unresolved"
	}
	if !snap.Exists {
		return "absent"
	}
	if snap.Linked {
		return "linked -> " + snap.ResolvedPath
	}
	return "direct regular file"
}

// backupValueLabel renders the backed-up managed values for warnings.
func backupValueLabel(bak *claudeBackup) string {
	if v, ok := bak.Values[claudeManagedEnvKey]; ok {
		return fmt.Sprintf("%q", v)
	}
	for _, k := range bak.Missing {
		if k == claudeManagedEnvKey {
			return "(unset)"
		}
	}
	keys := make([]string, 0, len(bak.Values))
	for k := range bak.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "(not recorded; backed keys: " + strings.Join(keys, ",") + ")"
}
