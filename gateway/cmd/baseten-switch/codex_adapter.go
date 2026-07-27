// codex_adapter.go implements `baseten-switch codex on|off|status`.
//
// codex profiles are standalone overlay files: `codex --profile baseten`
// loads $CODEX_HOME/baseten.config.toml on top of config.toml for that
// session only. The adapter owns that overlay file in its ENTIRETY:
// `model_provider = "baseten"`, a synthetic compatibility model that keeps
// Codex on the reduced request shape validated against Baseten, and the
// [model_providers.baseten] table pointing at the gateway
// door. The user's config.toml is never written, so default codex
// behavior is untouched and `--profile baseten` is the whole opt-in.
//
// `on` writes the overlay and takes a WHOLE-FILE backup of any
// pre-existing file at that path on the FIRST on only, and only when
// the current content is not already gateway-managed (managed = the
// baseten root provider, the current compatibility model, the
// [model_providers.baseten] table, and a base_url on a gateway port),
// so a repeated `on` can never snapshot our own overlay as "the
// user's original". A current-shape file on an old port is backed up
// and replaced; unrecognized content is refused. `off` restores the
// backup exactly, or deletes the file
// only when we created it; codex never rewrites the overlay (2.0
// spike), so a written-hash mismatch at `off` means user-made edits
// and downgrades to strip-only-owned. An absent overlay is a
// legitimate default state, unlike claude's settings.json.
//
// When the gateway codex client is parked (the template ships it with
// enabled: false), `on` offers to un-park it: explicit consent (the
// prompt names the door-port rebind side effect), a comment-preserving
// enabled flip via config.SetClientScalars, a router SIGHUP verified
// against the admin status (currently_bound, the applied listener
// state), then a door SIGHUP so the shared port spec picks up the
// client (plan item 2.2). `off` never re-parks the client; parking is
// a config decision, not overlay state.
//
// Exit codes (status; mirrors the claude adapter):
//
//	0  managed (overlay installed and pointing at the gateway door)
//	3  not managed
//
// on/off exit 0 on success (including noops) and 1 on errors.
//
// Test override points (unit tests only; never point these at live
// state): BASETEN_SWITCH_CODEX_HOME (overlay dir, default ~/.codex),
// BASETEN_SWITCH_BACKUP_DIR (backup root; the codex/ subdir is appended),
// BASETEN_SWITCH_ENV_FILE (the gateway's local placeholder-token source), and
// BASETEN_SWITCH_CONFIG_PATH (gateway.yaml, as everywhere else).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

const (
	codexManagedEnvKey = "CODEX_AUTH_TOKEN"
	codexAuthTokenStub = "baseten-switch-local"
	codexOverlayName   = "baseten.config.toml"
	codexHarnessName   = "codex"
	codexProviderTable = "[model_providers.baseten]"
)

func cmdCodex(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch codex on|off|status|route <baseten-slug>|reasoning baseten <model> off|follow-harness|effort <value>|default  (start/stop are aliases for on/off)")
		return 2
	}
	a, err := newCodexAdapterFromEnv()
	if err != nil {
		if args[0] == "reasoning" || args[0] == "route" {
			opts, _, parseErr := parseMutationOptions(args[1:])
			if parseErr == nil && opts.JSON {
				operation := "set_codex_route"
				if args[0] == "reasoning" {
					operation = "set_model_reasoning"
				}
				return failMutation(
					opts,
					os.Stdout,
					mutationResult{
						Operation: operation,
						Client:    "codex",
					},
					"config_load_failed",
					fmt.Sprintf("codex: %v", err),
					false,
					1,
				)
			}
		}
		fmt.Fprintf(os.Stderr, "codex: %v\n", err)
		return 1
	}
	switch args[0] {
	case "on", "start":
		return a.on()
	case "off", "stop":
		return a.off()
	case "status":
		return a.status(os.Stdout)
	case "route":
		return a.route(args[1:], os.Stdout)
	case "reasoning":
		return runClientReasoning(a.clientName, args[1:], os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown codex subcommand: %s (use on|off|status|route|reasoning)\n", args[0])
		return 2
	}
}

// codexAdapter carries the resolved paths and gateway topology for one
// on/off/status invocation. Tests construct it directly against
// t.TempDir() paths; cmdCodex builds it from env + gateway.yaml.
type codexAdapter struct {
	overlayPath   string
	backupPath    string
	envFilePath   string          // gateway-local CODEX_AUTH_TOKEN placeholder source
	configPath    string          // gateway.yaml (the un-park consent flow edits it)
	clientName    string          // the openai-shape client the overlay rides
	clientEnabled bool            // false = parked; the profile has no live route
	desiredPort   string          // door port carrying the codex client
	modelSlug     string          // current gateway default Baseten route target
	slugErr       error           // target resolution failure; only on() and route need it
	gatewayPorts  map[string]bool // every local port we consider "ours" (door binds + router listeners)
	out           io.Writer       // human-readable action/warning output
	in            io.Reader       // consent answers (os.Stdin; tests substitute)
}

func newCodexAdapterFromEnv() (*codexAdapter, error) {
	f, cfgPath, err := loadGatewayConfigForAdapter()
	if err != nil {
		return nil, err
	}
	client, port, ports, err := codexDoorPort(f)
	if err != nil {
		return nil, fmt.Errorf("%v (config: %s)", err, cfgPath)
	}
	// A missing default model is not a construction error: off and status must
	// keep working (restore the user's overlay, report state) on a
	// config whose routing was later restructured; only on() writes the
	// slug into the overlay and fails on slugErr.
	slug, slugErr := codexDefaultSlug(client)
	if slugErr != nil {
		slugErr = fmt.Errorf("%v (config: %s)", slugErr, cfgPath)
	}
	codexHome := envDefault("BASETEN_SWITCH_CODEX_HOME", homeJoin(".codex"))
	backupRoot := envDefault("BASETEN_SWITCH_BACKUP_DIR", homeJoin(".config", "baseten-switch", "backups"))
	overlay := filepath.Join(codexHome, codexOverlayName)
	return &codexAdapter{
		overlayPath:   overlay,
		backupPath:    codexBackupPath(backupRoot, overlay),
		envFilePath:   envDefault("BASETEN_SWITCH_ENV_FILE", config.EnvFilePath()),
		configPath:    cfgPath,
		clientName:    client.Name,
		clientEnabled: client.Enabled,
		desiredPort:   port,
		modelSlug:     slug,
		slugErr:       slugErr,
		gatewayPorts:  ports,
		out:           os.Stderr,
		in:            os.Stdin,
	}, nil
}

// codexClientYAML is the parked codex client block from
// config/gateway.example.yaml, offered verbatim when a config lacks an
// openai-shape client entirely. Pinned to the template line for line by
// TestCodexClientYAMLMatchesTemplate.
const codexClientYAML = `  - name: codex
    enabled: false
    bind_addr: 127.0.0.1:45272
    protocol_shape: openai
    auth_token:
      header: Authorization
      value: ${CODEX_AUTH_TOKEN}
    default_model: zai-org/GLM-5.2
    responses_compatibility:
      text_format_default: on
      additional_tools_input: off
      reasoning_effort: on
      function_arguments_consistency: on
    responses_strip_tool_types: []
    fallback_route: openai`

// codexDoorPort resolves, from gateway.yaml, the openai-shape client
// the overlay targets (the "codex" client when present, else the first
// openai-shape client, even when parked so status can report the
// state), the door port that carries it, and the full set of local
// ports the adapter considers gateway-owned. The openai-shape analog
// of claudeDoorPort (which hardcodes the anthropic shape). All values
// are derived from config, never hardcoded.
func codexDoorPort(f *config.File) (*config.Client, string, map[string]bool, error) {
	ports := map[string]bool{}
	addPort := func(addr string) {
		_, p, err := net.SplitHostPort(addr)
		if err == nil && p != "" {
			ports[p] = true
		}
	}
	var target *config.Client
	for i := range f.Clients {
		c := &f.Clients[i]
		addPort(c.BindAddr)
		if c.ProtocolShape != "openai" { // empty defaults to anthropic (internal/door)
			continue
		}
		if c.Name == "codex" {
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
		return nil, "", nil, fmt.Errorf("no openai-shape client in gateway.yaml; paste this codex client into the clients: list (bind_addr must match your router listener; this is the shipped template's parked block) and re-run 'baseten-switch codex on', which will offer to enable it:\n\n%s\n", codexClientYAML)
	}
	if f.Door == nil || len(f.Door.Ports) == 0 {
		return nil, "", nil, fmt.Errorf("gateway.yaml has no door: section; the codex adapter points the harness at the front door, so add door.ports mapping a bind_addr to the %s router listener %s (reference: config/gateway.example.yaml in the baseten-switch repo)", target.Name, target.BindAddr)
	}
	for _, dp := range f.Door.Ports {
		if dp.RouterAddr == target.BindAddr {
			_, p, err := net.SplitHostPort(dp.BindAddr)
			if err != nil || p == "" {
				return nil, "", nil, fmt.Errorf("door port %q for client %s has no parseable port", dp.BindAddr, target.Name)
			}
			return target, p, ports, nil
		}
	}
	return nil, "", nil, fmt.Errorf("no door port routes to the %s client listener %s; add a door.ports entry with router_addr: %s", target.Name, target.BindAddr, target.BindAddr)
}

// codexDefaultSlug resolves the Baseten slug the overlay pins as its
// model from the client routing config.
func codexDefaultSlug(c *config.Client) (string, error) {
	if s := c.DefaultModel; s != "" {
		return s, nil
	}
	return "", fmt.Errorf("no default_model configured for the %s client in gateway.yaml (reference: config/gateway.example.yaml in the baseten-switch repo)", c.Name)
}

func (a *codexAdapter) desiredURL() string {
	return "http://127.0.0.1:" + a.desiredPort + "/v1"
}

// desiredOverlay is the whole managed file. codex reads it as a
// session overlay on top of config.toml when invoked with
// `--profile baseten`; the synthetic model makes codex send the reduced
// unknown-model body shape that inference.baseten.co accepts. The
// gateway resolves it to the Codex client's current default_model.
func (a *codexAdapter) desiredOverlay() []byte {
	return []byte(fmt.Sprintf(`# Managed by baseten-switch ('baseten-switch codex on'); remove with 'baseten-switch codex off'.
# Opt in per session: codex --profile baseten  (your config.toml is never touched)
model_provider = "baseten"
model = %q

%s
name = "Baseten Switch (local gateway)"
base_url = %q
wire_api = "responses"
`, gateway.CodexCompatibilityModel, codexProviderTable, a.desiredURL()))
}

// isGatewayURL reports whether a base URL points at a local gateway
// port. Same ownership test as claudeAdapter.isGatewayURL, against
// this adapter's port set.
func (a *codexAdapter) isGatewayURL(raw string) bool {
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

// --- overlay shape -----------------------------------------------------------

// codexOverlayShape is the minimal parse of an overlay file used by
// the ownership tests. Deliberately not a TOML parser (dependency
// policy): it reads only the root provider/model and the local baseten
// provider table markers written by desiredOverlay.
type codexOverlayShape struct {
	providerTable bool
	modelProvider string
	model         string
	baseURL       string
}

func parseCodexOverlay(raw []byte) codexOverlayShape {
	var sh codexOverlayShape
	table := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			table = line
			if table == codexProviderTable {
				sh.providerTable = true
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		switch table {
		case "":
			switch key {
			case "model_provider":
				sh.modelProvider = codexTOMLValue(v)
			case "model":
				sh.model = codexTOMLValue(v)
			}
		case codexProviderTable:
			if key == "base_url" {
				sh.baseURL = codexTOMLValue(v)
			}
		}
	}
	return sh
}

// codexTOMLValue extracts a TOML string value from the raw right-hand
// side of a key = value line: double- or single-quoted (close-quote
// scan, so a legal inline `# comment` after the value never leaks into
// it), or bare with any inline comment stripped. The ownership test
// must survive user annotations to the managed overlay, or off would
// misclassify the file as foreign and leave the gateway wiring
// installed while consuming the backup.
func codexTOMLValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
		if i := strings.IndexByte(v[1:], v[0]); i >= 0 {
			return v[1 : 1+i]
		}
		return v[1:]
	}
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// codexOursShaped reports whether a file carries the baseten-switch overlay
// shape: the baseten root provider, the current compatibility model, the
// provider table, and a base_url. Shape only; a current-shape overlay on a
// dead port is still ours. Requiring every marker avoids claiming an arbitrary
// user-authored baseten provider table or any noncurrent profile.
func codexOursShaped(sh codexOverlayShape) bool {
	return sh.modelProvider == "baseten" &&
		sh.model == gateway.CodexCompatibilityModel &&
		sh.providerTable && sh.baseURL != ""
}

// overlayManaged is the "we own this file" test: ours-shaped AND the
// base_url points at a live gateway port. Used by the refuse-to-
// snapshot check, the strip-only-owned paths, and the poisoned-backup
// guard.
func (a *codexAdapter) overlayManaged(raw []byte) bool {
	sh := parseCodexOverlay(raw)
	return codexOursShaped(sh) && a.isGatewayURL(sh.baseURL)
}

// readCodexOverlay reads the overlay. An absent file returns
// existed=false with no error: absence is the legitimate default.
func readCodexOverlay(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	return b, true, nil
}

// writeCodexOverlay writes the file atomically (temp file + rename),
// preserving the existing file mode (0600 for a newly created file).
func writeCodexOverlay(path string, b []byte) error {
	mode := fs.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+codexOverlayName+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// --- backup ------------------------------------------------------------------

// codexBackup is the whole-file snapshot taken on the first `on`.
// Content is the pre-on file bytes (absent when the file did not
// exist). WrittenHash is the sha256 of the overlay exactly as written
// at `on`; a mismatch at `off` downgrades restore to strip-only-owned.
type codexBackup struct {
	ConfigPath  string `json:"config_path"`
	Content     []byte `json:"content,omitempty"`
	Existed     bool   `json:"existed"`
	WrittenHash string `json:"written_hash"`
}

// codexBackupPath keys the backup file by sha256(overlay path) so
// backups never cross-restore between paths.
func codexBackupPath(backupRoot, overlayPath string) string {
	sum := sha256.Sum256([]byte(overlayPath))
	name := "config-backup." + hex.EncodeToString(sum[:])[:16] + ".json"
	return filepath.Join(backupRoot, codexHarnessName, name)
}

func loadCodexBackup(path string) (*codexBackup, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read backup %s: %w", path, err)
	}
	var bak codexBackup
	if err := json.Unmarshal(b, &bak); err != nil {
		return nil, fmt.Errorf("parse backup %s: %w", path, err)
	}
	return &bak, nil
}

// saveCodexBackup writes dir 0700 / file 0600, matching the claude
// backup permissions (snapshots can embed other providers' URLs).
func saveCodexBackup(path string, bak *codexBackup) error {
	b, err := json.MarshalIndent(bak, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// poisonedBackup reports whether the backup's own content is
// gateway-managed; restoring such a backup would re-manage the
// harness on `off`, so it is discarded instead.
func (a *codexAdapter) poisonedBackup(bak *codexBackup) bool {
	return bak.Existed && a.overlayManaged(bak.Content)
}

// --- on ----------------------------------------------------------------------

func (a *codexAdapter) on() int {
	if a.slugErr != nil {
		fmt.Fprintf(a.out, "codex on: %v\n", a.slugErr)
		return 1
	}
	raw, existed, err := readCodexOverlay(a.overlayPath)
	if err != nil {
		fmt.Fprintf(a.out, "codex on: %v\n", err)
		return 1
	}
	ours := existed && codexOursShaped(parseCodexOverlay(raw))
	managed := existed && a.overlayManaged(raw)
	if existed && !ours {
		fmt.Fprintf(a.out, "codex on: %s already exists and is not baseten-switch-managed (expected root model_provider = \"baseten\", model = %q, and a %s table with base_url); refusing to overwrite it. Move it aside and re-run 'baseten-switch codex on'.\n",
			a.overlayPath, gateway.CodexCompatibilityModel, codexProviderTable)
		return 1
	}

	bak, err := loadCodexBackup(a.backupPath)
	if err != nil {
		fmt.Fprintf(a.out, "codex on: %v\n", err)
		return 1
	}

	desired := a.desiredOverlay()
	changed := !existed || !bytes.Equal(raw, desired)

	// Persist the backup BEFORE modifying the overlay, so no failure
	// or crash window exists in which the file is modified but the
	// user's original state is not durably recorded. The staged
	// WrittenHash is the pre-on file hash: if we crash before the
	// write below, off sees a hash match and its restore is a no-op.
	var nb *codexBackup
	if bak == nil && !managed {
		nb = &codexBackup{
			ConfigPath:  a.overlayPath,
			Existed:     existed,
			WrittenHash: sha256Hex(raw),
		}
		if existed {
			nb.Content = raw
		}
		if err := saveCodexBackup(a.backupPath, nb); err != nil {
			fmt.Fprintf(a.out, "codex on: write backup: %v (overlay untouched)\n", err)
			return 1
		}
	}
	if ours && !managed {
		if nb != nil {
			fmt.Fprintf(a.out, "note: pre-existing %s is baseten-switch-shaped but stale (base_url not on a live gateway port); backed it up and replacing it\n", a.overlayPath)
		} else {
			// A first-'on' backup already exists, so this file was NOT
			// snapshotted; print its content so replacing it never
			// silently loses anything.
			fmt.Fprintf(a.out, "note: %s is baseten-switch-shaped but stale (base_url not on a live gateway port); replacing it WITHOUT a new backup (the first-'on' backup already holds your pre-'on' original). Its content was:\n%s\n",
				a.overlayPath, strings.TrimRight(string(raw), "\n"))
		}
	}

	if changed {
		if err := writeCodexOverlay(a.overlayPath, desired); err != nil {
			if nb != nil {
				// Nothing was modified; drop the staged backup.
				os.Remove(a.backupPath)
			}
			fmt.Fprintf(a.out, "codex on: %v\n", err)
			return 1
		}
	}

	// Refresh the drift hash to the file as written at on. A failure
	// here only degrades off to its strip-only-owned drift path (the
	// user's original file above is already durable), so warn rather
	// than fail.
	if hashTarget := nb; hashTarget != nil || (bak != nil && changed) {
		if hashTarget == nil {
			hashTarget = bak
		}
		hashTarget.WrittenHash = sha256Hex(desired)
		if err := saveCodexBackup(a.backupPath, hashTarget); err != nil {
			fmt.Fprintf(a.out, "codex on: warning: could not refresh the backup drift hash: %v\n", err)
		}
	}

	if !a.clientEnabled {
		if rc := a.unpark(); rc != 0 {
			return rc
		}
	}
	a.ensureAuthToken()

	if changed {
		fmt.Fprintf(a.out, "codex: on (wrote %s: compatibility model %s routes to %s via door %s)\n", a.overlayPath, gateway.CodexCompatibilityModel, a.modelSlug, a.desiredURL())
	} else {
		fmt.Fprintf(a.out, "codex: already on (%s is up to date)\n", a.overlayPath)
	}
	fmt.Fprintln(a.out, "opt in per session with 'codex --profile baseten' (default codex behavior is unchanged); revert with 'baseten-switch codex off'.")
	return 0
}

// ensureAuthToken supplies the local placeholder interpolated by the
// gateway Codex client. Codex itself does not read this variable: the
// managed profile intentionally has no env_key, so users do not need
// to export or maintain anything in their shell.
func (a *codexAdapter) ensureAuthToken() {
	env, err := config.LoadEnvFile(a.envFilePath)
	if err != nil {
		fmt.Fprintf(a.out, "warning: could not read %s: %v; the gateway Codex client may fail to load its local placeholder token\n",
			a.envFilePath, err)
		return
	}
	if _, ok := env[codexManagedEnvKey]; ok {
		return
	}
	if err := appendEnvLine(a.envFilePath, codexManagedEnvKey, codexAuthTokenStub); err != nil {
		fmt.Fprintf(a.out, "warning: could not write the gateway Codex placeholder token to %s: %v\n",
			a.envFilePath, err)
		return
	}
	fmt.Fprintf(a.out, "configured the gateway Codex placeholder token in %s\n", a.envFilePath)
}

// appendEnvLine appends key=value to the env file atomically,
// preserving all existing content byte-exactly (config.SaveEnvFile
// would drop comments, so it is not used here).
func appendEnvLine(path, key, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	mode := fs.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	var b bytes.Buffer
	b.Write(raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(value)
	b.WriteByte('\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".env.*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// unpark offers to flip the parked gateway client's enabled: false to
// true (plan item 2.2): explicit consent, then the comment-preserving
// scalar edit, then SIGHUP-and-verify. Declining is not an error; the
// overlay is additive and already installed, it just has no live route
// until the client is enabled. The prompt names the known side effect:
// enabling the client changes the shared door port spec, and the
// door's SIGHUP diff rebinds the door port, briefly resetting live
// Claude Code connections (the Codex integration contract risk 3).
func (a *codexAdapter) unpark() int {
	fmt.Fprintf(a.out, "the gateway %s client is parked (enabled: false in %s); 'codex --profile baseten' has no live route until it is enabled.\n", a.clientName, a.configPath)
	fmt.Fprintf(a.out, "Enabling it changes the shared door port spec: the door's SIGHUP rebinds port %s, briefly resetting live Claude Code connections.\n", a.desiredPort)
	fmt.Fprintf(a.out, "Enable the %s client now? [y/N] ", a.clientName)
	var resp string
	fmt.Fscanln(a.in, &resp)
	if !strings.EqualFold(strings.TrimSpace(resp), "y") {
		fmt.Fprintf(a.out, "left parked; re-run 'baseten-switch codex on' to enable it later, or set enabled: true on the %s client in %s and SIGHUP the router\n", a.clientName, a.configPath)
		return 0
	}
	if err := config.SetClientScalars(a.configPath, a.clientName, map[string]string{"enabled": "true"}); err != nil {
		fmt.Fprintf(a.out, "codex on: enable the %s client: %v\n", a.clientName, err)
		return 1
	}
	a.clientEnabled = true
	fmt.Fprintf(a.out, "enabled the %s client in %s\n", a.clientName, a.configPath)
	a.enableReloadAndVerify()
	return 0
}

// enableReloadAndVerify SIGHUPs the running router so the un-parked
// client hot-loads, then polls admin status until the client's
// listener reports bound (or the timeout expires); it then SIGHUPs the
// running door, whose port spec is built from enabled clients at its
// own config load, so the shared port picks up the openai shape and
// its native-fallback rule (the rebind named in the consent prompt).
// With a component down it prints a notice that the config applies at
// the next start. Modeled on the claude adapter's routeReloadAndVerify.
func (a *codexAdapter) enableReloadAndVerify() {
	switch state, pid := classifyPidfile(gatewayPidfilePath()); state {
	case pidfileAlive:
		if err := signalRouter(pid); err != nil {
			fmt.Fprintf(a.out, "warning: could not SIGHUP router pid %d: %v; config saved, reload or restart the router to apply\n", pid, err)
			return
		}
		adminAddr := envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)
		if waitClientBound(adminAddr, a.clientName, routeApplyTimeout) {
			fmt.Fprintf(a.out, "router reloaded (SIGHUP pid %d); %s client verified enabled and bound\n", pid, a.clientName)
		} else {
			fmt.Fprintf(a.out, "note: SIGHUP sent (pid %d) but the router did not report a bound %s listener within %s; the reload may have failed (check the router log and 'baseten-switch status')\n", pid, a.clientName, routeApplyTimeout)
		}
	default:
		fmt.Fprintf(a.out, "router not running; %s updated and applies at next start\n", a.configPath)
	}
	// signalRouter is the process-agnostic SIGHUP seam despite its name.
	switch state, pid := classifyPidfile(doorPidfilePath()); state {
	case pidfileAlive:
		if err := signalRouter(pid); err != nil {
			fmt.Fprintf(a.out, "warning: could not SIGHUP door pid %d: %v; reload it yourself so door port %s picks up the %s client\n", pid, err, a.desiredPort, a.clientName)
			return
		}
		fmt.Fprintf(a.out, "door reloaded (SIGHUP pid %d); port %s rebound with the %s client\n", pid, a.desiredPort, a.clientName)
	default:
		fmt.Fprintf(a.out, "door not running; port %s picks up the %s client at next start\n", a.desiredPort, a.clientName)
	}
}

// waitClientBound polls the admin status until the named client
// reports enabled AND currently bound, or the timeout expires.
// currently_bound is the router's applied listener state; the enabled
// field alone is re-read from gateway.yaml per request (the file this
// adapter just wrote), so it would confirm nothing about the reload
// (which keeps current listeners when resolve fails). Modeled on
// waitRouteApplied in claude_adapter.go.
func waitClientBound(adminAddr, clientName string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		var payload struct {
			Clients []struct {
				Name           string `json:"name"`
				Enabled        bool   `json:"enabled"`
				CurrentlyBound bool   `json:"currently_bound"`
			} `json:"clients"`
		}
		if err := getJSON(adminAddr, "/v1/admin/status", &payload); err == nil {
			for _, c := range payload.Clients {
				if c.Name == clientName && c.Enabled && c.CurrentlyBound {
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

// --- off ---------------------------------------------------------------------

func (a *codexAdapter) off() int {
	raw, existed, err := readCodexOverlay(a.overlayPath)
	if err != nil {
		fmt.Fprintf(a.out, "codex off: %v\n", err)
		return 1
	}
	bak, err := loadCodexBackup(a.backupPath)
	if err != nil {
		fmt.Fprintf(a.out, "codex off: %v\n", err)
		return 1
	}
	if bak != nil && bak.ConfigPath != a.overlayPath {
		// Should be unreachable given the hash-keyed filename; refuse
		// to restore a snapshot of a different file.
		fmt.Fprintf(a.out, "warning: backup %s is for %s, not %s; ignoring it\n", a.backupPath, bak.ConfigPath, a.overlayPath)
		bak = nil
	}
	if bak != nil && a.poisonedBackup(bak) {
		fmt.Fprintf(a.out, "warning: backup %s itself points at a gateway port; discarding it instead of restoring (poisoned-backup guard)\n", a.backupPath)
		if err := os.Remove(a.backupPath); err != nil {
			fmt.Fprintf(a.out, "codex off: remove poisoned backup: %v\n", err)
			return 1
		}
		bak = nil
	}

	if bak == nil {
		return a.offStripOnly(raw, existed)
	}

	if !existed {
		// Nothing on disk to restore into. If we created the file at
		// on, gone is exactly the restored state.
		if bak.Existed {
			fmt.Fprintf(a.out, "warning: %s is gone but the backup says it existed before 'on'\n", a.overlayPath)
			a.noteNotRestored(bak)
		}
		if err := os.Remove(a.backupPath); err != nil {
			fmt.Fprintf(a.out, "codex off: remove backup: %v\n", err)
			return 1
		}
		fmt.Fprintln(a.out, "codex: off (overlay absent; backup cleared)")
		return 0
	}

	if sha256Hex(raw) != bak.WrittenHash {
		// codex never rewrites the overlay (2.0 spike), so drift is
		// user-made, not a rewrite race: genuinely surprising, unlike
		// the claude settings.json drift, hence the warning prefix.
		if a.overlayManaged(raw) {
			fmt.Fprintf(a.out, "warning: %s changed since 'on' (codex never rewrites the overlay, so the edits are user-made) but still points at the gateway; removing the gateway-owned overlay\n", a.overlayPath)
			if err := os.Remove(a.overlayPath); err != nil {
				fmt.Fprintf(a.out, "codex off: remove %s: %v\n", a.overlayPath, err)
				return 1
			}
			a.noteNotRestored(bak)
			if err := os.Remove(a.backupPath); err != nil {
				fmt.Fprintf(a.out, "codex off: remove backup: %v\n", err)
				return 1
			}
			fmt.Fprintf(a.out, "codex: off (removed gateway-owned overlay %s)\n", a.overlayPath)
			return 0
		}
		fmt.Fprintf(a.out, "warning: %s changed since 'on' and no longer points at the gateway; leaving it alone and clearing the backup\n", a.overlayPath)
		a.noteNotRestored(bak)
		if err := os.Remove(a.backupPath); err != nil {
			fmt.Fprintf(a.out, "codex off: remove backup: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.out, "codex: off (noop; %s is not gateway-managed)\n", a.overlayPath)
		return 0
	}

	// Clean restore: hash matches the file exactly as written at on.
	if !bak.Existed {
		if err := os.Remove(a.overlayPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(a.out, "codex off: remove %s: %v\n", a.overlayPath, err)
			return 1
		}
		fmt.Fprintf(a.out, "codex: off (%s did not exist before 'on'; removed)\n", a.overlayPath)
	} else {
		if err := writeCodexOverlay(a.overlayPath, bak.Content); err != nil {
			fmt.Fprintf(a.out, "codex off: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.out, "codex: off (restored %s from backup)\n", a.overlayPath)
	}
	if err := os.Remove(a.backupPath); err != nil {
		fmt.Fprintf(a.out, "codex off: remove backup: %v\n", err)
		return 1
	}
	return 0
}

// offStripOnly is the conservative path: delete the overlay only when
// its current content is gateway-managed; never touch anything else,
// never create a missing file.
func (a *codexAdapter) offStripOnly(raw []byte, existed bool) int {
	if !existed {
		fmt.Fprintf(a.out, "codex: off (no overlay at %s; nothing to do)\n", a.overlayPath)
		return 0
	}
	if a.overlayManaged(raw) {
		if err := os.Remove(a.overlayPath); err != nil {
			fmt.Fprintf(a.out, "codex off: remove %s: %v\n", a.overlayPath, err)
			return 1
		}
		fmt.Fprintf(a.out, "codex: off (removed gateway-owned overlay %s)\n", a.overlayPath)
		return 0
	}
	fmt.Fprintf(a.out, "codex: off (noop; %s is not gateway-managed)\n", a.overlayPath)
	return 0
}

// noteNotRestored prints the pre-'on' overlay content on the paths
// that consume the backup without restoring it, so the user's
// original file is never silently lost (the whole-file analog of the
// claude adapter's not-restored note, which names the single value).
func (a *codexAdapter) noteNotRestored(bak *codexBackup) {
	if bak == nil || !bak.Existed {
		return
	}
	fmt.Fprintf(a.out, "note: your pre-'on' %s was NOT restored on this path; its content was:\n%s\n",
		a.overlayPath, strings.TrimRight(string(bak.Content), "\n"))
}

// --- route -------------------------------------------------------------------

const codexRouteUsage = "usage: baseten-switch codex route <baseten-slug>  (example: baseten-switch codex route zai-org/GLM-5.2)"

// route changes only the Codex client's gateway default_model. The managed
// overlay remains byte-identical and continues to emit the compatibility
// sentinel, so an active Codex session picks up the new target after the
// router hot-reloads without a profile rewrite or Codex restart.
func (a *codexAdapter) route(args []string, stdout io.Writer) int {
	opts, positional, err := parseMutationOptions(args)
	if err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation:  "set_codex_route",
			Client:     a.clientName,
			ConfigPath: a.configPath,
		}, "usage", fmt.Sprintf("%s: %v", codexRouteUsage, err), false, 2)
	}
	if len(positional) != 1 {
		return failMutation(opts, stdout, mutationResult{
			Operation:  "set_codex_route",
			Client:     a.clientName,
			ConfigPath: a.configPath,
		}, "usage", codexRouteUsage, false, 2)
	}
	target := positional[0]
	if !strings.Contains(target, "/") ||
		strings.TrimSpace(target) != target ||
		strings.ContainsAny(target, " \t\r\n\x00") {
		return failMutation(opts, stdout, mutationResult{
			OperationID:     opts.OperationID,
			Operation:       "set_codex_route",
			RequestedTarget: target,
			Client:          a.clientName,
			Key:             "default_model",
			ConfigPath:      a.configPath,
		}, "invalid_route_target",
			fmt.Sprintf("codex route: %q is not a raw Baseten slug containing \"/\"\n%s", target, codexRouteUsage),
			false, 2)
	}
	lock, err := acquireConfigMutationLock(a.configPath)
	if err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation:  "set_codex_route",
			Client:     a.clientName,
			ConfigPath: a.configPath,
		}, "mutation_locked", err.Error(), true, 1)
	}
	defer lock.close()
	if err := recoverInterruptedExactConfigCommit(a.configPath); err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation:  "set_codex_route",
			Client:     a.clientName,
			ConfigPath: a.configPath,
		}, "commit_recovery_failed", err.Error(), true, 1)
	}
	prior, mode, err := readExactConfig(a.configPath)
	if err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation:  "set_codex_route",
			Client:     a.clientName,
			ConfigPath: a.configPath,
		}, "config_read_failed", err.Error(), true, 1)
	}
	if _, err := config.Load(a.configPath); err != nil {
		return failMutation(opts, stdout, mutationResult{
			Operation:  "set_codex_route",
			Client:     a.clientName,
			ConfigPath: a.configPath,
		}, "config_load_failed", err.Error(), false, 1)
	}
	out := stdout
	if !opts.JSON {
		out = a.out
	}
	return runJournaledMutationLocked(
		a.configPath,
		prior,
		mode,
		opts,
		out,
		journaledMutationSpec{
			Operation:       "set_codex_route",
			RequestedTarget: target,
			Client:          a.clientName,
			Key:             "default_model",
			Apply: func(path string) error {
				return config.SetClientScalars(
					path,
					a.clientName,
					map[string]string{"default_model": target},
				)
			},
			HumanSuccess: fmt.Sprintf(
				"codex route: %s -> %s (in %s; managed profile unchanged)",
				a.modelSlug,
				target,
				a.configPath,
			),
		},
	)
}

// --- status ------------------------------------------------------------------

func (a *codexAdapter) status(stdout io.Writer) int {
	raw, existed, err := readCodexOverlay(a.overlayPath)
	if err != nil {
		fmt.Fprintf(a.out, "codex status: %v\n", err)
		return 1
	}
	sh := parseCodexOverlay(raw)
	ours := existed && codexOursShaped(sh)
	managed := ours && a.isGatewayURL(sh.baseURL)

	overlayLabel := "absent (default state; 'baseten-switch codex on' creates it)"
	switch {
	case managed:
		overlayLabel = "managed (baseten-switch overlay)"
	case ours:
		overlayLabel = fmt.Sprintf("baseten-switch-shaped but stale (base_url %s is not a live gateway port)", sh.baseURL)
	case existed:
		overlayLabel = "present, not baseten-switch-managed"
	}
	clientLabel := "enabled"
	if !a.clientEnabled {
		clientLabel = "parked (enabled: false)"
	}
	backupLabel := "none"
	if bak, bakErr := loadCodexBackup(a.backupPath); bakErr != nil {
		backupLabel = fmt.Sprintf("unreadable (%v)", bakErr)
	} else if bak != nil {
		backupLabel = "present (" + a.backupPath + ")"
		if a.poisonedBackup(bak) {
			backupLabel += " POISONED: discarded on off"
		}
	}
	state := "off (no managed overlay)"
	if managed {
		state = "on (managed overlay installed; opt in with 'codex --profile baseten')"
	}
	fmt.Fprintf(stdout, "codex: %s\n", state)
	fmt.Fprintf(stdout, "  overlay:             %s (%s)\n", a.overlayPath, overlayLabel)
	fmt.Fprintf(stdout, "  gateway client:      %s (%s)\n", a.clientName, clientLabel)
	fmt.Fprintf(stdout, "  door port:           %s (base_url %s)\n", a.desiredPort, a.desiredURL())
	modelLabel := a.modelSlug + " (gateway default route target; overlay uses " + gateway.CodexCompatibilityModel + ")"
	if a.slugErr != nil {
		modelLabel = fmt.Sprintf("unresolved: %v ('codex on' needs it; off does not)", a.slugErr)
	}
	fmt.Fprintf(stdout, "  model:               %s\n", modelLabel)
	fmt.Fprintf(stdout, "  backup:              %s\n", backupLabel)
	if managed {
		return 0
	}
	return statusExitOff
}
