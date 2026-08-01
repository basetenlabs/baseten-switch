package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

const mutationJournalVersion = 1

const (
	mutationSurfaceSwitch = "switch"
	mutationSurfaceClaude = "claude"
	mutationSurfaceCodex  = "codex"
)

var (
	mutationLockTimeout = 2 * time.Second
	mutationHTTPTimeout = 500 * time.Millisecond

	// Test seams for deterministic no-clobber commit interleavings. Production
	// code leaves the hook nil and uses os.Link. The hard-link install is
	// exclusive: it can never replace a path claimed by an external editor.
	exactConfigCommitHook func(stage, configPath, transactionDir string) error
	exactConfigCommitLink = os.Link
)

var errExactConfigCommitInterrupted = errors.New("exact config commit interrupted")

const (
	exactCommitStateVersion   = 1
	exactCommitStagePrepared  = "prepared"
	exactCommitStageDisplaced = "displaced"
	exactCommitStageVerified  = "verified"
	exactCommitStageInstalled = "installed"
)

type exactConfigCommitState struct {
	Version      int    `json:"version"`
	ConfigBase   string `json:"config_base"`
	ExpectedHash string `json:"expected_hash"`
	DesiredHash  string `json:"desired_hash"`
	Mode         uint32 `json:"mode"`
	Stage        string `json:"stage"`
}

type exactConfigCommitIndeterminateError struct {
	err error
}

func (e *exactConfigCommitIndeterminateError) Error() string {
	return e.err.Error()
}

func (e *exactConfigCommitIndeterminateError) Unwrap() error {
	return e.err
}

type mutationOptions struct {
	JSON           bool
	OperationID    string
	IfActiveToken  string
	IfConfigHash   string
	hasOperationID bool
	hasActiveToken bool
	hasConfigHash  bool
}

type mutationError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type mutationResult struct {
	OK                        bool           `json:"ok"`
	OperationID               string         `json:"operation_id"`
	Operation                 string         `json:"operation"`
	Requested                 bool           `json:"requested"`
	RequestedTarget           string         `json:"requested_target,omitempty"`
	Client                    string         `json:"client,omitempty"`
	Key                       string         `json:"key,omitempty"`
	ConfigPath                string         `json:"config_path"`
	PreviousActiveToken       string         `json:"previous_active_token"`
	PreviousDesiredConfigHash string         `json:"previous_desired_config_hash"`
	DesiredConfigHash         string         `json:"desired_config_hash"`
	ActiveToken               string         `json:"active_token"`
	ActiveConfigHash          string         `json:"active_config_hash"`
	Applied                   bool           `json:"applied"`
	ReconciliationRequired    bool           `json:"reconciliation_required,omitempty"`
	BlockingOperationID       string         `json:"blocking_operation_id,omitempty"`
	Outcome                   string         `json:"outcome,omitempty"`
	RequestFingerprint        string         `json:"request_fingerprint,omitempty"`
	IdentityStrength          string         `json:"identity_strength,omitempty"`
	CleanupPending            bool           `json:"cleanup_pending,omitempty"`
	Error                     *mutationError `json:"error"`
}

type routingAdminStatus struct {
	ConfigPath           string   `json:"config_path"`
	RouterPID            int      `json:"router_pid"`
	RouterBootID         string   `json:"router_boot_id"`
	ActiveGeneration     uint64   `json:"active_generation"`
	ActiveConfigHash     string   `json:"active_config_hash"`
	DesiredConfigHash    string   `json:"desired_config_hash"`
	GlobalRoutingEnabled bool     `json:"global_routing_enabled"`
	Capabilities         []string `json:"capabilities"`
}

func (s routingAdminStatus) activeToken() string {
	if s.RouterBootID == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", s.RouterBootID, s.ActiveGeneration)
}

type routingMutationJournal struct {
	Version             int       `json:"version"`
	OperationID         string    `json:"operation_id"`
	Operation           string    `json:"operation"`
	Surface             string    `json:"surface,omitempty"`
	ConfigPath          string    `json:"config_path"`
	Requested           bool      `json:"requested"`
	RequestedTarget     string    `json:"requested_target,omitempty"`
	Client              string    `json:"client,omitempty"`
	Key                 string    `json:"key,omitempty"`
	PreviousRouting     bool      `json:"previous_routing_enabled"`
	PreviousConfig      []byte    `json:"previous_config_bytes"`
	PreviousMode        uint32    `json:"previous_config_mode"`
	PreviousConfigHash  string    `json:"previous_config_hash"`
	DesiredConfigHash   string    `json:"desired_config_hash"`
	PreviousActiveToken string    `json:"previous_active_token"`
	IfConfigHash        string    `json:"if_config_hash,omitempty"`
	IfActiveToken       string    `json:"if_active_token,omitempty"`
	RequestFingerprint  string    `json:"request_fingerprint,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

type configMutationLock struct {
	file *os.File
}

func (l *configMutationLock) close() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
}

func acquireConfigMutationLock(configPath string) (*configMutationLock, error) {
	lockPath := configPath + ".mutation.lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config mutation lock %s: %w", lockPath, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure config mutation lock %s: %w", lockPath, err)
	}
	deadline := time.Now().Add(mutationLockTimeout)
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &configMutationLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock config mutation lock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("another config mutation still holds %s", lockPath)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func normalizeMutationInvocation(args []string) ([]string, error) {
	if len(args) == 0 || !isMutationOption(args[0]) {
		return args, nil
	}
	i := 0
	for i < len(args) && isMutationOption(args[i]) {
		consumed, err := mutationOptionWidth(args, i)
		if err != nil {
			return nil, err
		}
		i += consumed
	}
	if i >= len(args) {
		return nil, fmt.Errorf("mutation options require an on, off, claude, codex, or mutation command")
	}
	command := args[i]
	if command != "on" && command != "off" && command != "claude" && command != "codex" && command != "mutation" {
		return nil, fmt.Errorf("mutation options are supported only by on, off, claude, codex, and mutation reconcile")
	}
	out := []string{command}
	out = append(out, args[i+1:]...)
	out = append(out, args[:i]...)
	return out, nil
}

func isMutationOption(arg string) bool {
	switch {
	case arg == "--json":
		return true
	case arg == "--operation-id", strings.HasPrefix(arg, "--operation-id="):
		return true
	case arg == "--if-active-token", strings.HasPrefix(arg, "--if-active-token="):
		return true
	case arg == "--if-config-hash", strings.HasPrefix(arg, "--if-config-hash="):
		return true
	default:
		return false
	}
}

func mutationOptionWidth(args []string, i int) (int, error) {
	arg := args[i]
	if arg == "--json" || strings.Contains(arg, "=") {
		return 1, nil
	}
	if i+1 >= len(args) {
		return 0, fmt.Errorf("%s requires a value", arg)
	}
	return 2, nil
}

func parseMutationOptions(args []string) (mutationOptions, []string, error) {
	var opts mutationOptions
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			opts.JSON = true
		case arg == "--operation-id":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--operation-id requires a value")
			}
			i++
			opts.OperationID = args[i]
			opts.hasOperationID = true
		case strings.HasPrefix(arg, "--operation-id="):
			opts.OperationID = strings.TrimPrefix(arg, "--operation-id=")
			opts.hasOperationID = true
		case arg == "--if-active-token":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--if-active-token requires a value")
			}
			i++
			opts.IfActiveToken = args[i]
			opts.hasActiveToken = true
		case strings.HasPrefix(arg, "--if-active-token="):
			opts.IfActiveToken = strings.TrimPrefix(arg, "--if-active-token=")
			opts.hasActiveToken = true
		case arg == "--if-config-hash":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--if-config-hash requires a value")
			}
			i++
			opts.IfConfigHash = args[i]
			opts.hasConfigHash = true
		case strings.HasPrefix(arg, "--if-config-hash="):
			opts.IfConfigHash = strings.TrimPrefix(arg, "--if-config-hash=")
			opts.hasConfigHash = true
		case strings.HasPrefix(arg, "-"):
			return opts, nil, fmt.Errorf("unknown mutation option %q", arg)
		default:
			positional = append(positional, arg)
		}
	}
	if opts.hasOperationID && opts.OperationID == "" {
		return opts, nil, fmt.Errorf("--operation-id cannot be empty")
	}
	if opts.OperationID == "" {
		opts.OperationID = newOperationID()
	}
	if err := validateOperationID(opts.OperationID); err != nil {
		return opts, nil, err
	}
	if opts.hasConfigHash && opts.IfConfigHash == "" {
		return opts, nil, fmt.Errorf("--if-config-hash cannot be empty")
	}
	if opts.IfConfigHash != "" && !validConfigHash(opts.IfConfigHash) {
		return opts, nil, fmt.Errorf("--if-config-hash must be sha256 followed by 64 lowercase hexadecimal characters")
	}
	if opts.hasActiveToken && opts.IfActiveToken == "" {
		return opts, nil, fmt.Errorf("--if-active-token cannot be empty")
	}
	return opts, positional, nil
}

func validateOperationID(id string) error {
	if id == "" || len(id) > 128 {
		return fmt.Errorf("--operation-id must contain between 1 and 128 safe characters")
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("--operation-id contains unsafe character %q", r)
	}
	return nil
}

func validConfigHash(hash string) bool {
	if len(hash) != len("sha256:")+64 || !strings.HasPrefix(hash, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(hash, "sha256:"))
	return err == nil && strings.ToLower(hash) == hash
}

func newOperationID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		now := time.Now().UTC().UnixNano()
		return fmt.Sprintf("local-%d-%d", os.Getpid(), now)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func exactConfigHash(data []byte) string {
	return "sha256:" + sha256Hex(data)
}

func emitMutationResult(out io.Writer, result mutationResult) {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(result)
}

func failMutation(opts mutationOptions, out io.Writer, result mutationResult, code, message string, retryable bool, rc int) int {
	result.OK = false
	result.OperationID = opts.OperationID
	if opts.JSON || reviewedMutationFailureCode(code) {
		message = reviewedMutationMessage(code)
	}
	result.Error = &mutationError{Code: code, Message: message, Retryable: retryable}
	if opts.JSON {
		emitMutationResult(out, result)
	} else {
		fmt.Fprintln(os.Stderr, message)
	}
	return rc
}

func reviewedMutationFailureCode(code string) bool {
	switch code {
	case "usage", "client_scoped_switch_removed", "invalid_operation_id",
		"invalid_route_key", "invalid_route_target", "invalid_subagent_target":
		return false
	default:
		return true
	}
}

func readExactConfig(path string) ([]byte, os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("%s is a symlink; exact config mutations require a regular file", path)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !os.SameFile(info, openedInfo) {
		return nil, 0, fmt.Errorf("%s changed while it was being opened", path)
	}
	if openedInfo.Size() > mutationConfigReadLimit {
		return nil, 0, fmt.Errorf("config exceeds the 4 MiB mutation read limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, mutationConfigReadLimit+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > mutationConfigReadLimit {
		return nil, 0, fmt.Errorf("config exceeds the 4 MiB mutation read limit")
	}
	return data, openedInfo.Mode().Perm(), nil
}

func previewGlobalRoutingEdit(path string, prior []byte, mode os.FileMode, enabled bool) ([]byte, error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".routing-preview-*")
	if err != nil {
		return nil, err
	}
	previewPath := file.Name()
	defer os.Remove(previewPath)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Write(prior); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := config.SetGlobalRoutingEnabled(previewPath, enabled); err != nil {
		return nil, err
	}
	return os.ReadFile(previewPath)
}

func mutationJournalDir(configPath string) string {
	return configPath + ".mutation-journal"
}

func mutationJournalPath(configPath, operationID string) string {
	return filepath.Join(mutationJournalDir(configPath), operationID+".json")
}

func writeMutationJournal(journal routingMutationJournal) error {
	dir := mutationJournalDir(journal.ConfigPath)
	if err := secureDirectory(dir, true); err != nil {
		return fmt.Errorf("secure transaction journal directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("inspect transaction journal directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == "completed" && entry.IsDir() {
			continue
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			return fmt.Errorf("unfinished routing mutation %q must be reconciled before starting another", strings.TrimSuffix(entry.Name(), ".json"))
		}
	}
	path := mutationJournalPath(journal.ConfigPath, journal.OperationID)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("operation %q already has an unfinished transaction journal", journal.OperationID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect transaction journal: %w", err)
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode transaction journal: %w", err)
	}
	data = append(data, '\n')
	if len(data) > mutationJournalReadLimit {
		return fmt.Errorf("transaction journal exceeds the size limit")
	}
	if err := atomicWriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write transaction journal: %w", err)
	}
	return nil
}

func readMutationJournal(configPath, operationID string) (routingMutationJournal, error) {
	var journal routingMutationJournal
	if err := validateMutationStateDirectories(configPath, false); err != nil {
		return journal, err
	}
	path := mutationJournalPath(configPath, operationID)
	data, err := readBoundedRegularFile(path, mutationJournalReadLimit, 0o600)
	if err != nil {
		return journal, err
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		return journal, fmt.Errorf("decode transaction journal: %w", err)
	}
	if journal.Version != mutationJournalVersion ||
		journal.OperationID != operationID ||
		canonicalPath(journal.ConfigPath) != canonicalPath(configPath) {
		return journal, fmt.Errorf("transaction journal identity does not match this config and operation")
	}
	if journal.Operation == "" || !validConfigHash(journal.PreviousConfigHash) ||
		!validConfigHash(journal.DesiredConfigHash) ||
		exactConfigHash(journal.PreviousConfig) != journal.PreviousConfigHash ||
		journal.CreatedAt.IsZero() {
		return journal, fmt.Errorf("transaction journal contents are invalid")
	}
	if journal.RequestFingerprint != "" {
		if !validConfigHash(journal.RequestFingerprint) {
			return journal, fmt.Errorf("transaction journal fingerprint is invalid")
		}
		if !validMutationSurface(journal.Operation, journal.Surface) {
			return journal, fmt.Errorf("transaction journal surface is invalid")
		}
		expected, err := mutationRequestFingerprint(journal.ConfigPath, mutationOptions{
			IfConfigHash:   journal.IfConfigHash,
			IfActiveToken:  journal.IfActiveToken,
			hasConfigHash:  journal.IfConfigHash != "",
			hasActiveToken: journal.IfActiveToken != "",
		}, journaledMutationSpec{
			Operation:       journal.Operation,
			Surface:         journal.Surface,
			Requested:       journal.Requested,
			RequestedTarget: journal.RequestedTarget,
			Client:          journal.Client,
			Key:             journal.Key,
		})
		if err != nil || expected != journal.RequestFingerprint {
			return journal, fmt.Errorf("transaction journal fingerprint does not match its contents")
		}
	} else if journal.Surface != "" {
		return journal, fmt.Errorf("legacy transaction journal surface is invalid")
	}
	return journal, nil
}

func clearMutationJournal(configPath, operationID string) error {
	path := mutationJournalPath(configPath, operationID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(tmp)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	remove = false
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func canonicalPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = filepath.Clean(path)
	}
	if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
		return evaluated
	}
	// Replay identity must remain stable after the config is removed. Resolve
	// the longest existing parent (for example macOS /var -> /private/var),
	// then append the missing suffix without following nonexistent entries.
	parent := absolute
	var suffix []string
	for {
		if evaluated, err := filepath.EvalSymlinks(parent); err == nil {
			parts := append([]string{evaluated}, suffix...)
			return filepath.Join(parts...)
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		suffix = append([]string{filepath.Base(parent)}, suffix...)
		parent = next
	}
	return absolute
}

var fetchRoutingAdminStatus = func(adminAddr string) (routingAdminStatus, error) {
	var status routingAdminStatus
	client := &http.Client{Timeout: mutationHTTPTimeout}
	response, err := client.Get("http://" + adminAddr + "/v1/admin/status")
	if err != nil {
		return status, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return status, fmt.Errorf("admin status returned %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(&status); err != nil {
		return status, err
	}
	return status, nil
}

func validateMutationAdminStatus(status routingAdminStatus, configPath, configHash string) error {
	if status.RouterBootID == "" || status.ActiveGeneration == 0 ||
		status.ActiveConfigHash == "" || status.DesiredConfigHash == "" {
		return fmt.Errorf("running router does not expose the routing mutation ordering contract")
	}
	if status.RouterPID <= 1 {
		return fmt.Errorf("running router does not expose a safe router process identity")
	}
	if status.ConfigPath == "" {
		return fmt.Errorf("running router does not report its active config path")
	}
	if canonicalPath(status.ConfigPath) != canonicalPath(configPath) {
		return fmt.Errorf("running router uses config %s, not %s", status.ConfigPath, configPath)
	}
	if status.DesiredConfigHash != configHash {
		return fmt.Errorf("router desired config hash %s does not match current file %s", status.DesiredConfigHash, configHash)
	}
	if status.ActiveConfigHash != status.DesiredConfigHash {
		return fmt.Errorf("router desired config hash %s is not active (active %s)", status.DesiredConfigHash, status.ActiveConfigHash)
	}
	return nil
}

func validateManagedRouterIdentity(status routingAdminStatus, managedPID int) error {
	if managedPID <= 1 || status.RouterPID <= 1 || status.RouterPID != managedPID {
		return fmt.Errorf("managed router pid %d does not match admin router pid %d", managedPID, status.RouterPID)
	}
	return nil
}

func waitConfigHashActivation(adminAddr, configPath, desiredHash, previousToken string, requireAdvance bool, timeout time.Duration) (routingAdminStatus, bool) {
	deadline := time.Now().Add(timeout)
	var last routingAdminStatus
	for {
		status, err := fetchRoutingAdminStatus(adminAddr)
		if err == nil {
			last = status
			token := status.activeToken()
			tokenOK := token != ""
			if requireAdvance {
				tokenOK = tokenOK && token != previousToken
			}
			if tokenOK &&
				status.ConfigPath != "" &&
				canonicalPath(status.ConfigPath) == canonicalPath(configPath) &&
				status.ActiveConfigHash == desiredHash &&
				status.DesiredConfigHash == desiredHash {
				return status, true
			}
		}
		if time.Now().After(deadline) {
			return last, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// signalExpectedRouter resolves the managed pidfile immediately before
// signaling. It never signals pid 0 or 1, and it refuses a process
// replacement between preflight and mutation confirmation.
func signalExpectedRouter(expectedPID int) (int, error) {
	state, pid := classifyPidfile(gatewayPidfilePath())
	if state != pidfileAlive || pid <= 1 {
		return 0, fmt.Errorf("router is no longer running with a safe managed pid")
	}
	if expectedPID > 1 && pid != expectedPID {
		return 0, fmt.Errorf("router pid changed from %d to %d during the mutation", expectedPID, pid)
	}
	if err := signalRouter(pid); err != nil {
		return 0, err
	}
	return pid, nil
}

// signalVerifiedRouter ties the pidfile process to the admin endpoint's boot
// identity immediately before signaling. A live but stale/reused pidfile is
// never sufficient authority to send SIGHUP.
func signalVerifiedRouter(expected routingAdminStatus, adminAddr string) (int, error) {
	current, err := fetchRoutingAdminStatus(adminAddr)
	if err != nil {
		return 0, fmt.Errorf("recheck router identity: %w", err)
	}
	if current.RouterBootID == "" || current.RouterBootID != expected.RouterBootID {
		return 0, fmt.Errorf("router boot identity changed before signal")
	}
	state, managedPID := classifyPidfile(gatewayPidfilePath())
	if state != pidfileAlive {
		return 0, fmt.Errorf("router is no longer running with a managed pid")
	}
	if err := validateManagedRouterIdentity(current, managedPID); err != nil {
		return 0, err
	}
	if expected.RouterPID > 1 && current.RouterPID != expected.RouterPID {
		return 0, fmt.Errorf("router pid changed from %d to %d before signal", expected.RouterPID, current.RouterPID)
	}
	if err := signalRouter(current.RouterPID); err != nil {
		return 0, err
	}
	return current.RouterPID, nil
}

type journaledMutationSpec struct {
	Operation       string
	Surface         string
	Requested       bool
	RequestedTarget string
	Client          string
	Key             string
	Apply           func(path string) error
	HumanSuccess    string
}

func mutationResultForSpec(spec journaledMutationSpec, operationID, path, priorHash string) mutationResult {
	return mutationResult{
		OperationID:               operationID,
		Operation:                 spec.Operation,
		Requested:                 spec.Requested,
		RequestedTarget:           spec.RequestedTarget,
		Client:                    spec.Client,
		Key:                       spec.Key,
		ConfigPath:                path,
		PreviousDesiredConfigHash: priorHash,
		DesiredConfigHash:         priorHash,
	}
}

func previewExactConfigEdit(path string, prior []byte, mode os.FileMode, apply func(string) error) ([]byte, error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".mutation-preview-*")
	if err != nil {
		return nil, err
	}
	previewPath := file.Name()
	defer os.Remove(previewPath)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Write(prior); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := apply(previewPath); err != nil {
		return nil, err
	}
	return os.ReadFile(previewPath)
}

func validateConfigBytesForRouter(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".validation-*")
	if err != nil {
		return err
	}
	validationPath := file.Name()
	defer os.Remove(validationPath)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	parsed, err := config.Load(validationPath)
	if err != nil {
		return err
	}
	return gateway.ValidateConfigFile(parsed)
}

// compareAndSwapExactConfig commits replacement without ever renaming over the
// live config path. A check-then-rename is not a compare-and-swap: an external
// atomic editor can claim path after the check and have its write silently
// overwritten. This protocol instead:
//
//  1. prepares and fsyncs the desired inode in a private same-filesystem dir;
//  2. atomically displaces path into that dir and verifies its exact bytes;
//  3. hard-links the prepared inode to path, which fails rather than replacing
//     a path claimed by an external editor; and
//  4. keeps durable transaction state until the directory metadata is synced.
//
// A crash after displacement leaves enough state for
// recoverInterruptedExactConfigCommit, used by mutation reconciliation, to
// restore the displaced entry without clobbering anything
// that claimed path in the meantime.
func compareAndSwapExactConfig(path, expectedHash string, replacement []byte, mode os.FileMode) error {
	if !validConfigHash(expectedHash) {
		return fmt.Errorf("invalid expected config hash %q", expectedHash)
	}
	if err := recoverInterruptedExactConfigCommit(path); err != nil {
		return err
	}
	current, _, err := readExactConfig(path)
	if err != nil {
		return err
	}
	if currentHash := exactConfigHash(current); currentHash != expectedHash {
		return fmt.Errorf("config changed: expected %s, found %s", expectedHash, currentHash)
	}

	parent := filepath.Dir(path)
	base := filepath.Base(path)
	transactionDir, err := os.MkdirTemp(parent, "."+base+".exact-commit-")
	if err != nil {
		return fmt.Errorf("create exact-config transaction directory: %w", err)
	}
	if err := os.Chmod(transactionDir, 0o700); err != nil {
		_ = os.Remove(transactionDir)
		return fmt.Errorf("secure exact-config transaction directory: %w", err)
	}
	preparedPath := filepath.Join(transactionDir, "desired")
	displacedPath := filepath.Join(transactionDir, "prior")
	statePath := filepath.Join(transactionDir, "state.json")
	cleanupPrepared := true
	defer func() {
		if cleanupPrepared {
			_ = cleanupExactCommitDir(transactionDir, expectedHash)
		}
	}()

	if err := writeSyncedRegularFile(preparedPath, replacement, mode.Perm()); err != nil {
		return fmt.Errorf("prepare exact config replacement: %w", err)
	}
	desiredHash := exactConfigHash(replacement)
	state := exactConfigCommitState{
		Version:      exactCommitStateVersion,
		ConfigBase:   base,
		ExpectedHash: expectedHash,
		DesiredHash:  desiredHash,
		Mode:         uint32(mode.Perm()),
		Stage:        exactCommitStagePrepared,
	}
	if err := writeExactCommitState(statePath, state); err != nil {
		return err
	}
	if err := syncDirectory(transactionDir); err != nil {
		return fmt.Errorf("sync exact-config transaction directory: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync config directory before exact commit: %w", err)
	}
	if err := runExactConfigCommitHook("before_displacement", path, transactionDir); err != nil {
		return fmt.Errorf("before exact-config displacement: %w", err)
	}

	if err := os.Rename(path, displacedPath); err != nil {
		return fmt.Errorf("displace current config: %w", err)
	}
	cleanupPrepared = false
	if err := syncDirectory(parent); err != nil {
		return &exactConfigCommitIndeterminateError{
			err: fmt.Errorf("config displaced but parent directory sync failed: %w", err),
		}
	}
	if err := syncDirectory(transactionDir); err != nil {
		return &exactConfigCommitIndeterminateError{
			err: fmt.Errorf("config displaced but transaction directory sync failed: %w", err),
		}
	}
	state.Stage = exactCommitStageDisplaced
	if err := writeExactCommitState(statePath, state); err != nil {
		return &exactConfigCommitIndeterminateError{err: err}
	}
	if err := runExactConfigCommitHook("after_displacement", path, transactionDir); err != nil {
		if errors.Is(err, errExactConfigCommitInterrupted) {
			return &exactConfigCommitIndeterminateError{err: err}
		}
		if restoreErr := restoreDisplacedExactConfig(path, displacedPath); restoreErr != nil {
			return &exactConfigCommitIndeterminateError{
				err: fmt.Errorf("%v; restore displaced config: %w", err, restoreErr),
			}
		}
		_ = cleanupExactCommitDir(transactionDir, expectedHash)
		return fmt.Errorf("after exact-config displacement: %w", err)
	}

	displaced, _, displacedErr := readExactConfig(displacedPath)
	if displacedErr != nil {
		if restoreErr := restoreDisplacedExactConfig(path, displacedPath); restoreErr != nil {
			return &exactConfigCommitIndeterminateError{
				err: fmt.Errorf("inspect displaced config: %v; restore: %w", displacedErr, restoreErr),
			}
		}
		_ = cleanupExactCommitDir(transactionDir, "")
		return fmt.Errorf("inspect displaced config: %w", displacedErr)
	}
	displacedHash := exactConfigHash(displaced)
	if displacedHash != expectedHash {
		if restoreErr := restoreDisplacedExactConfig(path, displacedPath); restoreErr != nil {
			return &exactConfigCommitIndeterminateError{
				err: fmt.Errorf("config changed: expected %s, displaced %s; restore: %w", expectedHash, displacedHash, restoreErr),
			}
		}
		if err := cleanupExactCommitDir(transactionDir, displacedHash); err != nil {
			return fmt.Errorf("config changed: expected %s, found %s; cleanup: %w", expectedHash, displacedHash, err)
		}
		return fmt.Errorf("config changed: expected %s, found %s", expectedHash, displacedHash)
	}
	state.Stage = exactCommitStageVerified
	if err := writeExactCommitState(statePath, state); err != nil {
		return &exactConfigCommitIndeterminateError{err: err}
	}
	if err := runExactConfigCommitHook("before_install", path, transactionDir); err != nil {
		if errors.Is(err, errExactConfigCommitInterrupted) {
			return &exactConfigCommitIndeterminateError{err: err}
		}
		if restoreErr := restoreDisplacedExactConfig(path, displacedPath); restoreErr != nil {
			return &exactConfigCommitIndeterminateError{
				err: fmt.Errorf("%v; restore displaced config: %w", err, restoreErr),
			}
		}
		_ = cleanupExactCommitDir(transactionDir, expectedHash)
		return fmt.Errorf("before exact-config install: %w", err)
	}

	if err := exactConfigCommitLink(preparedPath, path); err != nil {
		if _, pathErr := os.Lstat(path); pathErr == nil {
			// An external editor claimed path during the displacement window.
			// Its entry wins. The displaced bytes are the expected prior
			// version and may be discarded without touching the external entry.
			if cleanupErr := cleanupExactCommitDir(transactionDir, expectedHash); cleanupErr != nil {
				return fmt.Errorf("install exact config without clobbering external path: %v; cleanup: %w", err, cleanupErr)
			}
			return fmt.Errorf("config changed while installing replacement: %w", err)
		}
		if restoreErr := restoreDisplacedExactConfig(path, displacedPath); restoreErr != nil {
			return &exactConfigCommitIndeterminateError{
				err: fmt.Errorf("install exact config: %v; restore displaced config: %w", err, restoreErr),
			}
		}
		_ = cleanupExactCommitDir(transactionDir, expectedHash)
		return fmt.Errorf("install exact config: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return &exactConfigCommitIndeterminateError{
			err: fmt.Errorf("config installed but parent directory sync failed: %w", err),
		}
	}
	state.Stage = exactCommitStageInstalled
	if err := writeExactCommitState(statePath, state); err != nil {
		return &exactConfigCommitIndeterminateError{err: err}
	}
	if err := runExactConfigCommitHook("after_install", path, transactionDir); err != nil {
		return &exactConfigCommitIndeterminateError{err: err}
	}
	installed, _, err := readExactConfig(path)
	if err != nil {
		return &exactConfigCommitIndeterminateError{
			err: fmt.Errorf("verify installed exact config: %w", err),
		}
	}
	if installedHash := exactConfigHash(installed); installedHash != desiredHash {
		// The desired inode was installed, then an external writer changed or
		// replaced path. Never overwrite that newer state.
		if cleanupErr := cleanupExactCommitDir(transactionDir, expectedHash); cleanupErr != nil {
			return fmt.Errorf("installed config changed externally to %s; cleanup: %w", installedHash, cleanupErr)
		}
		return fmt.Errorf("installed config changed externally: expected %s, found %s", desiredHash, installedHash)
	}
	if err := cleanupExactCommitDir(transactionDir, expectedHash); err != nil {
		return &exactConfigCommitIndeterminateError{
			err: fmt.Errorf("config installed but transaction cleanup failed: %w", err),
		}
	}
	if err := syncDirectory(parent); err != nil {
		return &exactConfigCommitIndeterminateError{
			err: fmt.Errorf("config installed but final directory sync failed: %w", err),
		}
	}
	return nil
}

func exactCommitDirPrefix(path string) string {
	return "." + filepath.Base(path) + ".exact-commit-"
}

func runExactConfigCommitHook(stage, path, transactionDir string) error {
	if exactConfigCommitHook == nil {
		return nil
	}
	return exactConfigCommitHook(stage, path, transactionDir)
}

func writeSyncedRegularFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(mode.Perm()); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func writeExactCommitState(path string, state exactConfigCommitState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode exact-config transaction state: %w", err)
	}
	data = append(data, '\n')
	if err := atomicWriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write exact-config transaction state: %w", err)
	}
	return nil
}

func readExactCommitState(path string) (exactConfigCommitState, error) {
	var state exactConfigCommitState
	data, _, err := readExactConfig(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode exact-config transaction state: %w", err)
	}
	if state.Version != exactCommitStateVersion ||
		!validConfigHash(state.ExpectedHash) ||
		!validConfigHash(state.DesiredHash) {
		return state, fmt.Errorf("invalid exact-config transaction state")
	}
	return state, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func restoreDisplacedExactConfig(path, displacedPath string) error {
	if err := exactConfigCommitLink(displacedPath, path); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

// cleanupExactCommitDir removes only artifacts created by this protocol.
// expectedPriorHash must match the displaced entry before it is removed; an
// empty hash preserves an unrecognized displaced entry for manual recovery.
func cleanupExactCommitDir(transactionDir, expectedPriorHash string) error {
	priorPath := filepath.Join(transactionDir, "prior")
	if _, err := os.Lstat(priorPath); err == nil {
		if expectedPriorHash == "" {
			return fmt.Errorf("displaced entry preserved at %s", priorPath)
		}
		prior, _, readErr := readExactConfig(priorPath)
		if readErr != nil {
			return fmt.Errorf("inspect displaced entry %s: %w", priorPath, readErr)
		}
		if got := exactConfigHash(prior); got != expectedPriorHash {
			return fmt.Errorf("displaced entry %s has hash %s, expected %s; preserved", priorPath, got, expectedPriorHash)
		}
		if err := os.Remove(priorPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, name := range []string{"desired", "state.json"} {
		artifact := filepath.Join(transactionDir, name)
		if info, err := os.Lstat(artifact); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("unexpected exact-config artifact %s; preserved", artifact)
			}
			if err := os.Remove(artifact); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	entries, err := os.ReadDir(transactionDir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("unexpected files in exact-config transaction directory %s; preserved", transactionDir)
	}
	if err := syncDirectory(transactionDir); err != nil {
		return err
	}
	if err := os.Remove(transactionDir); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(transactionDir))
}

// recoverInterruptedExactConfigCommit repairs the only state in which this
// protocol removes the live path: a durable transaction directory containing
// the displaced entry. It always restores with an exclusive hard link, so a
// concurrent writer that has already reclaimed path wins and is never
// overwritten.
func recoverInterruptedExactConfigCommit(path string) error {
	parent := filepath.Dir(path)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	prefix := exactCommitDirPrefix(path)
	var transactionDirs []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || !entry.IsDir() {
			continue
		}
		transactionDirs = append(transactionDirs, filepath.Join(parent, entry.Name()))
	}
	if len(transactionDirs) == 0 {
		return nil
	}
	var active []string
	for _, transactionDir := range transactionDirs {
		statePath := filepath.Join(transactionDir, "state.json")
		if _, err := os.Lstat(statePath); errors.Is(err, os.ErrNotExist) {
			if _, pathErr := os.Lstat(path); pathErr == nil {
				// State is made durable before displacement. A state-less
				// directory therefore cannot own a missing live path. Preserve
				// it rather than deleting a coincidentally named user directory.
				continue
			}
			return fmt.Errorf("config path is missing and incomplete exact-config transaction %s has no durable state", transactionDir)
		} else if err != nil {
			return err
		}
		active = append(active, transactionDir)
	}
	if len(active) == 0 {
		return nil
	}
	if len(active) != 1 {
		return fmt.Errorf("multiple interrupted exact-config transactions require manual recovery: %s", strings.Join(active, ", "))
	}
	transactionDir := active[0]
	state, err := readExactCommitState(filepath.Join(transactionDir, "state.json"))
	if err != nil {
		return err
	}
	if state.ConfigBase != filepath.Base(path) {
		return fmt.Errorf("exact-config transaction %s belongs to %q, not %q", transactionDir, state.ConfigBase, filepath.Base(path))
	}
	priorPath := filepath.Join(transactionDir, "prior")

	current, _, currentErr := readExactConfig(path)
	if currentErr == nil {
		currentHash := exactConfigHash(current)
		if currentHash == state.ExpectedHash || currentHash == state.DesiredHash {
			return cleanupExactCommitDir(transactionDir, state.ExpectedHash)
		}
		if prior, _, priorErr := readExactConfig(priorPath); priorErr == nil {
			priorHash := exactConfigHash(prior)
			if priorHash != state.ExpectedHash {
				return fmt.Errorf("external config at %s is preserved, and displaced external edit is preserved at %s", path, priorPath)
			}
			if err := cleanupExactCommitDir(transactionDir, state.ExpectedHash); err != nil {
				return err
			}
		} else if errors.Is(priorErr, os.ErrNotExist) {
			if err := cleanupExactCommitDir(transactionDir, state.ExpectedHash); err != nil {
				return err
			}
		} else {
			return priorErr
		}
		// An external editor owns path. Leave it untouched; its hash will fail
		// the caller's exact-byte precondition.
		return nil
	}
	if !errors.Is(currentErr, os.ErrNotExist) {
		return currentErr
	}
	prior, _, priorErr := readExactConfig(priorPath)
	if priorErr != nil {
		return fmt.Errorf("config path is missing and interrupted transaction has no restorable prior entry: %w", priorErr)
	}
	priorHash := exactConfigHash(prior)
	if err := restoreDisplacedExactConfig(path, priorPath); err != nil {
		if _, claimedErr := os.Lstat(path); claimedErr == nil {
			// A concurrent writer won the exclusive restore race.
			if priorHash == state.ExpectedHash {
				return cleanupExactCommitDir(transactionDir, state.ExpectedHash)
			}
			return fmt.Errorf("external config at %s and displaced external edit at %s were both preserved", path, priorPath)
		}
		return fmt.Errorf("restore interrupted exact-config transaction: %w", err)
	}
	if err := cleanupExactCommitDir(transactionDir, priorHash); err != nil {
		return err
	}
	if priorHash != state.ExpectedHash {
		return fmt.Errorf("restored external config edit with hash %s; expected prior hash was %s", priorHash, state.ExpectedHash)
	}
	return nil
}

func unfinishedMutationOperation(configPath string) (string, error) {
	entries, err := os.ReadDir(mutationJournalDir(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			return strings.TrimSuffix(entry.Name(), ".json"), nil
		}
	}
	return "", nil
}

func runJournaledMutationLocked(path string, prior []byte, mode os.FileMode, opts mutationOptions, out io.Writer, spec journaledMutationSpec) int {
	priorHash := exactConfigHash(prior)
	result := mutationResultForSpec(spec, opts.OperationID, path, priorHash)
	fingerprint, err := mutationRequestFingerprint(path, opts, spec)
	if err != nil {
		return failMutation(opts, out, result, "fingerprint_failed", "could not identify the routing mutation request", false, 1)
	}
	result.RequestFingerprint = fingerprint
	result.IdentityStrength = mutationIdentityExact
	if replay, found, rc, replayErr := replayTerminalForRequest(path, opts, spec); found {
		_ = gcMutationTerminals(path, time.Now().UTC())
		if replayErr != nil && replayErr.Error() != "operation_id_conflict" && replayErr.Error() != "operation_id_legacy" &&
			replayErr.Error() != "mutation_not_applied" && replayErr.Error() != "activation_failed_rolled_back" {
			replayErr = fmt.Errorf("terminal_conflict")
		}
		return emitTerminalReplay(opts, out, replay, rc, replayErr)
	}
	if opts.IfConfigHash != "" && opts.IfConfigHash != priorHash {
		return failMutation(opts, out, result, "stale_config_hash",
			fmt.Sprintf("config changed: expected %s, found %s", opts.IfConfigHash, priorHash), true, 1)
	}
	if activeJournal, found, err := singleActiveMutation(path); err != nil {
		return failMutation(opts, out, result, "journal_read_failed", reviewedMutationMessage("journal_invalid"), true, 1)
	} else if found {
		result.BlockingOperationID = activeJournal.OperationID
		return failMutation(opts, out, result, "unfinished_mutation",
			"another routing mutation must be reconciled before starting this operation", true, 1)
	}

	adminAddr := envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)
	var before routingAdminStatus
	routerRunning := false
	switch state, managedPID := classifyPidfile(gatewayPidfilePath()); state {
	case pidfileAlive:
		routerRunning = true
		status, err := fetchRoutingAdminStatus(adminAddr)
		if err != nil {
			return failMutation(opts, out, result, "router_unavailable",
				fmt.Sprintf("read running router status: %v", err), true, 1)
		}
		if err := validateMutationAdminStatus(status, path, priorHash); err != nil {
			return failMutation(opts, out, result, "router_state_mismatch", err.Error(), true, 1)
		}
		if err := validateManagedRouterIdentity(status, managedPID); err != nil {
			return failMutation(opts, out, result, "router_identity_mismatch", err.Error(), false, 1)
		}
		before = status
		result.PreviousActiveToken = status.activeToken()
		result.PreviousDesiredConfigHash = status.DesiredConfigHash
		result.ActiveToken = status.activeToken()
		result.ActiveConfigHash = status.ActiveConfigHash
	default:
		if opts.IfActiveToken != "" {
			return failMutation(opts, out, result, "router_unavailable",
				"cannot validate --if-active-token because the router is not running", true, 1)
		}
	}
	if opts.IfActiveToken != "" && opts.IfActiveToken != before.activeToken() {
		return failMutation(opts, out, result, "stale_active_token",
			fmt.Sprintf("router changed: expected active token %s, found %s", opts.IfActiveToken, before.activeToken()), true, 1)
	}

	desired, err := previewExactConfigEdit(path, prior, mode, spec.Apply)
	if err != nil {
		return failMutation(opts, out, result, "config_edit_failed",
			fmt.Sprintf("prepare config edit: %v", err), false, 1)
	}
	if len(desired) > mutationConfigReadLimit {
		return failMutation(opts, out, result, "config_size_limit", reviewedMutationMessage("config_size_limit"), false, 1)
	}
	desiredHash := exactConfigHash(desired)
	result.DesiredConfigHash = desiredHash
	if err := validateConfigBytesForRouter(path, desired, mode); err != nil {
		return failMutation(opts, out, result, "config_validation_failed",
			fmt.Sprintf("edited config would not load in the gateway: %v", err), false, 1)
	}
	if desiredHash == priorHash {
		result.OK = true
		result.Applied = routerRunning
		result.Outcome = mutationOutcomeUnchanged
		journal := routingMutationJournal{
			Version: mutationJournalVersion, OperationID: opts.OperationID, Operation: spec.Operation,
			Surface: spec.Surface, ConfigPath: path, Requested: spec.Requested, RequestedTarget: spec.RequestedTarget,
			Client: spec.Client, Key: spec.Key, PreviousConfigHash: priorHash,
			DesiredConfigHash: desiredHash, PreviousActiveToken: before.activeToken(),
			IfConfigHash: opts.IfConfigHash, IfActiveToken: opts.IfActiveToken,
			RequestFingerprint: fingerprint, CreatedAt: time.Now().UTC(),
		}
		published, publishErr := publishMutationTerminal(path, terminalFromJournal(journal, mutationOutcomeUnchanged, before, "", false), nil)
		if publishErr != nil {
			result.OK = false
			return failMutation(opts, out, result, "terminal_write_failed", reviewedMutationMessage("terminal_write_failed"), true, 1)
		}
		result.CleanupPending = published.CleanupPending
		if opts.JSON {
			emitMutationResult(out, result)
		} else if spec.HumanSuccess != "" {
			fmt.Fprintln(out, spec.HumanSuccess+"  (unchanged)")
		}
		return 0
	}

	journal := routingMutationJournal{
		Version:             mutationJournalVersion,
		OperationID:         opts.OperationID,
		Operation:           spec.Operation,
		Surface:             spec.Surface,
		ConfigPath:          path,
		Requested:           spec.Requested,
		RequestedTarget:     spec.RequestedTarget,
		Client:              spec.Client,
		Key:                 spec.Key,
		PreviousConfig:      prior,
		PreviousMode:        uint32(mode),
		PreviousConfigHash:  priorHash,
		DesiredConfigHash:   desiredHash,
		PreviousActiveToken: before.activeToken(),
		IfConfigHash:        opts.IfConfigHash,
		IfActiveToken:       opts.IfActiveToken,
		RequestFingerprint:  fingerprint,
		CreatedAt:           time.Now().UTC(),
	}
	if err := writeMutationJournal(journal); err != nil {
		return failMutation(opts, out, result, "journal_write_failed", err.Error(), false, 1)
	}
	if err := compareAndSwapExactConfig(path, priorHash, desired, mode); err != nil {
		var indeterminate *exactConfigCommitIndeterminateError
		if errors.As(err, &indeterminate) {
			result.ReconciliationRequired = true
			return failMutation(opts, out, result, "activation_indeterminate",
				fmt.Sprintf("exact config commit was interrupted: %v; run mutation reconcile %s", err, opts.OperationID), true, 1)
		}
		terminal := terminalFromJournal(journal, mutationOutcomeRejected, before, "stale_config_hash", true)
		published, terminalErr := publishMutationTerminal(path, terminal, &journal)
		if terminalErr != nil {
			result.ReconciliationRequired = true
			return failMutation(opts, out, result, "terminal_write_failed", reviewedMutationMessage("terminal_write_failed"), true, 1)
		}
		result.Outcome = mutationOutcomeRejected
		result.CleanupPending = published.CleanupPending
		return failMutation(opts, out, result, "stale_config_hash", err.Error(), true, 1)
	}

	if !routerRunning {
		result.OK = true
		result.Applied = false
		result.ReconciliationRequired = true
		if opts.JSON {
			emitMutationResult(out, result)
		} else {
			if spec.HumanSuccess != "" {
				fmt.Fprintln(out, spec.HumanSuccess)
			}
			fmt.Fprintf(os.Stderr, "router not running; %s updated and applies at next start; reconcile operation %s after startup\n", path, opts.OperationID)
		}
		return 0
	}

	signaledPID, signalErr := signalVerifiedRouter(before, adminAddr)
	active, activated := waitConfigHashActivation(adminAddr, path, desiredHash, before.activeToken(), true, routeApplyTimeout)
	if activated {
		result.Applied = true
		result.ActiveToken = active.activeToken()
		result.ActiveConfigHash = active.ActiveConfigHash
		published, err := publishMutationTerminal(path, terminalFromJournal(journal, mutationOutcomeApplied, active, "", false), &journal)
		if err != nil {
			result.ReconciliationRequired = true
			return failMutation(opts, out, result, "terminal_write_failed", reviewedMutationMessage("terminal_write_failed"), true, 1)
		}
		result.OK = true
		result.Outcome = mutationOutcomeApplied
		result.CleanupPending = published.CleanupPending
		if opts.JSON {
			emitMutationResult(out, result)
		} else {
			if spec.HumanSuccess != "" {
				fmt.Fprintln(out, spec.HumanSuccess)
			}
			fmt.Fprintf(os.Stderr, "router reloaded (SIGHUP pid %d); config mutation verified live\n", signaledPID)
		}
		return 0
	}
	cause := "router did not activate the intended config within the bounded confirmation window"
	if signalErr != nil {
		cause = fmt.Sprintf("router identity/signal check failed: %v", signalErr)
	}
	return rollbackJournaledMutation(path, journal, before, adminAddr, opts, out, result, cause)
}

func rollbackJournaledMutation(path string, journal routingMutationJournal, before routingAdminStatus, adminAddr string, opts mutationOptions, out io.Writer, result mutationResult, cause string) int {
	if err := compareAndSwapExactConfig(path, journal.DesiredConfigHash, journal.PreviousConfig, os.FileMode(journal.PreviousMode)); err != nil {
		result.ReconciliationRequired = true
		return failMutation(opts, out, result, "activation_indeterminate",
			fmt.Sprintf("%s; refusing to overwrite a concurrent config change while restoring: %v; run mutation reconcile %s", cause, err, journal.OperationID), true, 1)
	}
	if active, alreadyActive := waitConfigHashActivation(adminAddr, path, journal.PreviousConfigHash, "", false, 250*time.Millisecond); alreadyActive {
		result.ActiveToken = active.activeToken()
		result.ActiveConfigHash = active.ActiveConfigHash
	} else {
		current, statusErr := fetchRoutingAdminStatus(adminAddr)
		if statusErr == nil && canonicalPath(current.ConfigPath) == canonicalPath(path) {
			_, _ = signalVerifiedRouter(current, adminAddr)
		} else {
			_, _ = signalVerifiedRouter(before, adminAddr)
		}
	}
	active, ok := waitConfigHashActivation(adminAddr, path, journal.PreviousConfigHash, "", false, routeApplyTimeout)
	if !ok {
		result.ReconciliationRequired = true
		return failMutation(opts, out, result, "activation_indeterminate",
			fmt.Sprintf("%s; prior config was restored but not confirmed active; run mutation reconcile %s", cause, journal.OperationID), true, 1)
	}
	published, err := publishMutationTerminal(path,
		terminalFromJournal(journal, mutationOutcomeRolledBack, active, "activation_failed_rolled_back", false), &journal)
	if err != nil {
		result.ReconciliationRequired = true
		return failMutation(opts, out, result, "terminal_write_failed", reviewedMutationMessage("terminal_write_failed"), true, 1)
	}
	result.ActiveToken = active.activeToken()
	result.ActiveConfigHash = active.ActiveConfigHash
	result.Outcome = mutationOutcomeRolledBack
	result.CleanupPending = published.CleanupPending
	return failMutation(opts, out, result, "activation_failed_rolled_back",
		cause+"; restored and reactivated the prior exact config", false, 1)
}

func runGlobalSwitchLocked(verb, path string, file *config.File, prior []byte, mode os.FileMode, opts mutationOptions, out io.Writer) int {
	requested := verb == "on"
	if file.Global.RoutingEnabled == nil {
		return failMutation(opts, out, mutationResultForSpec(journaledMutationSpec{
			Operation: "set_global_routing", Surface: mutationSurfaceSwitch, Requested: requested,
		}, opts.OperationID, path, exactConfigHash(prior)), "invalid_routing_policy",
			"config has no explicit global.routing_enabled", false, 1)
	}
	state := "OFF"
	if requested {
		state = "ON"
	}
	return runJournaledMutationLocked(path, prior, mode, opts, out, journaledMutationSpec{
		Operation: "set_global_routing",
		Surface:   mutationSurfaceSwitch,
		Requested: requested,
		Apply: func(editPath string) error {
			return config.SetGlobalRoutingEnabled(editPath, requested)
		},
		HumanSuccess: "global routing " + state,
	})
}

func cmdMutation(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return cmdMutationStatus(args[1:])
		case "recover":
			return cmdMutationRecover(args[1:])
		}
	}
	opts, positional, err := parseMutationOptions(args)
	if err != nil {
		return failMutation(opts, os.Stdout, mutationResult{Operation: "reconcile_routing_mutation"},
			"usage", fmt.Sprintf("usage: baseten-switch mutation reconcile <operation-id> [--json]: %v", err), false, 2)
	}
	if len(positional) != 2 || positional[0] != "reconcile" {
		return failMutation(opts, os.Stdout, mutationResult{Operation: "reconcile_routing_mutation"},
			"usage", "usage: baseten-switch mutation reconcile <operation-id> [--json]", false, 2)
	}
	if opts.hasOperationID || opts.IfActiveToken != "" || opts.IfConfigHash != "" {
		return failMutation(opts, os.Stdout, mutationResult{Operation: "reconcile_routing_mutation"},
			"usage", "mutation reconcile accepts only --json; the operation ID is its positional argument", false, 2)
	}
	operationID := positional[1]
	if err := validateOperationID(operationID); err != nil {
		return failMutation(opts, os.Stdout, mutationResult{Operation: "reconcile_routing_mutation"},
			"invalid_operation_id", err.Error(), false, 2)
	}
	opts.OperationID = operationID
	path, notices := resolveConfigPath()
	if !opts.JSON {
		for _, notice := range notices {
			fmt.Fprintln(os.Stderr, notice)
		}
	}
	result := mutationResult{
		OperationID: opts.OperationID,
		Operation:   "reconcile_routing_mutation",
		ConfigPath:  path,
	}
	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		return failMutation(opts, os.Stdout, result, "mutation_locked", reviewedMutationMessage("mutation_locked"), true, 1)
	}
	defer lock.close()
	if terminal, terminalErr := readMutationTerminal(path, operationID); terminalErr == nil {
		_ = gcMutationTerminals(path, time.Now().UTC())
		result = resultFromTerminal(path, terminal, false, journaledMutationSpec{})
		result.OperationID = operationID
		cleanupPending, cleanupErr := terminalCleanupPending(path, terminal)
		if cleanupErr != nil {
			return failMutation(opts, os.Stdout, result, "terminal_conflict", reviewedMutationMessage("terminal_conflict"), false, 1)
		}
		result.CleanupPending = cleanupPending
		switch terminal.Outcome {
		case mutationOutcomeApplied, mutationOutcomeUnchanged, mutationOutcomeNotApplied, mutationOutcomeRolledBack:
			result.OK = true
			result.Error = nil
			if opts.JSON {
				emitMutationResult(os.Stdout, result)
			} else {
				fmt.Fprintf(os.Stdout, "mutation %s replayed (%s)\n", operationID, terminal.Outcome)
			}
			return 0
		default:
			code := terminal.ErrorCode
			if code == "" {
				code = "mutation_rejected"
			}
			return failMutation(opts, os.Stdout, result, code, reviewedMutationMessage(code), terminal.ErrorRetryable, 1)
		}
	} else if !errors.Is(terminalErr, os.ErrNotExist) {
		return failMutation(opts, os.Stdout, result, "terminal_conflict", reviewedMutationMessage("terminal_conflict"), false, 1)
	}
	if err := recoverInterruptedExactConfigCommit(path); err != nil {
		result.ReconciliationRequired = true
		return failMutation(opts, os.Stdout, result, "commit_recovery_failed", reviewedMutationMessage("commit_recovery_failed"), true, 1)
	}
	journal, err := readMutationJournal(path, operationID)
	if err != nil {
		code := "journal_read_failed"
		if errors.Is(err, os.ErrNotExist) {
			code = "journal_not_found"
		}
		message := reviewedMutationMessage(code)
		if code != "journal_not_found" {
			message = reviewedMutationMessage("journal_invalid")
		}
		return failMutation(opts, os.Stdout, result, code, message, false, 1)
	}
	result.Requested = journal.Requested
	result.RequestedTarget = journal.RequestedTarget
	result.Client = journal.Client
	result.Key = journal.Key
	result.Operation = journal.Operation
	result.PreviousActiveToken = journal.PreviousActiveToken
	result.PreviousDesiredConfigHash = journal.PreviousConfigHash
	result.DesiredConfigHash = journal.DesiredConfigHash
	result.RequestFingerprint = journal.RequestFingerprint
	result.IdentityStrength = mutationIdentityLegacy
	if journal.RequestFingerprint != "" {
		result.IdentityStrength = mutationIdentityExact
	}
	current, _, err := readExactConfig(path)
	if err != nil {
		return failMutation(opts, os.Stdout, result, "config_read_failed", reviewedMutationMessage("config_read_failed"), true, 1)
	}
	currentHash := exactConfigHash(current)
	if currentHash != journal.DesiredConfigHash && currentHash != journal.PreviousConfigHash {
		return failMutation(opts, os.Stdout, result, "external_config_change",
			reviewedMutationMessage("external_config_change"), false, 1)
	}
	switch state, pid := classifyPidfile(gatewayPidfilePath()); state {
	case pidfileAlive:
		adminAddr := envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)
		before, statusErr := fetchRoutingAdminStatus(adminAddr)
		if statusErr != nil {
			return failMutation(opts, os.Stdout, result, "router_unavailable", reviewedMutationMessage("router_unavailable"), true, 1)
		}
		if before.ConfigPath == "" || canonicalPath(before.ConfigPath) != canonicalPath(path) {
			return failMutation(opts, os.Stdout, result, "router_state_mismatch",
				reviewedMutationMessage("router_state_mismatch"), false, 1)
		}
		if err := validateManagedRouterIdentity(before, pid); err != nil {
			return failMutation(opts, os.Stdout, result, "router_identity_mismatch", reviewedMutationMessage("router_identity_mismatch"), false, 1)
		}
		if currentHash == journal.DesiredConfigHash {
			if active, ok := waitConfigHashActivation(adminAddr, path, journal.DesiredConfigHash, "", false, 250*time.Millisecond); ok {
				published, err := publishMutationTerminal(path, terminalFromJournal(journal, mutationOutcomeApplied, active, "", false), &journal)
				if err != nil {
					result.ReconciliationRequired = true
					return failMutation(opts, os.Stdout, result, "terminal_write_failed", reviewedMutationMessage("terminal_write_failed"), true, 1)
				}
				result.OK = true
				result.Applied = true
				result.Outcome = mutationOutcomeApplied
				result.CleanupPending = published.CleanupPending
				result.ActiveToken = active.activeToken()
				result.ActiveConfigHash = active.ActiveConfigHash
				if opts.JSON {
					emitMutationResult(os.Stdout, result)
				} else {
					fmt.Fprintf(os.Stdout, "mutation %s already active; journal cleared\n", operationID)
				}
				return 0
			}
			if _, err := signalVerifiedRouter(before, adminAddr); err == nil {
				if active, ok := waitConfigHashActivation(adminAddr, path, journal.DesiredConfigHash, journal.PreviousActiveToken, true, routeApplyTimeout); ok {
					published, err := publishMutationTerminal(path, terminalFromJournal(journal, mutationOutcomeApplied, active, "", false), &journal)
					if err != nil {
						result.ReconciliationRequired = true
						return failMutation(opts, os.Stdout, result, "terminal_write_failed", reviewedMutationMessage("terminal_write_failed"), true, 1)
					}
					result.OK = true
					result.Applied = true
					result.Outcome = mutationOutcomeApplied
					result.CleanupPending = published.CleanupPending
					result.ActiveToken = active.activeToken()
					result.ActiveConfigHash = active.ActiveConfigHash
					if opts.JSON {
						emitMutationResult(os.Stdout, result)
					} else {
						fmt.Fprintf(os.Stdout, "mutation %s activated; journal cleared\n", operationID)
					}
					return 0
				}
			}
			return rollbackJournaledMutation(path, journal, before, adminAddr, opts, os.Stdout, result,
				"reconciliation could not activate the intended config")
		}
		if _, alreadyActive := waitConfigHashActivation(adminAddr, path, journal.PreviousConfigHash, "", false, 250*time.Millisecond); !alreadyActive {
			_, _ = signalVerifiedRouter(before, adminAddr)
		}
		active, ok := waitConfigHashActivation(adminAddr, path, journal.PreviousConfigHash, "", false, routeApplyTimeout)
		if !ok {
			result.ReconciliationRequired = true
			return failMutation(opts, os.Stdout, result, "reconcile_failed",
				"prior config is present but could not be confirmed active; journal left intact", true, 1)
		}
		published, err := publishMutationTerminal(path, terminalFromJournal(journal, mutationOutcomeNotApplied, active, "", false), &journal)
		if err != nil {
			result.ReconciliationRequired = true
			return failMutation(opts, os.Stdout, result, "terminal_write_failed", reviewedMutationMessage("terminal_write_failed"), true, 1)
		}
		result.OK = true
		result.Applied = false
		result.Outcome = mutationOutcomeNotApplied
		result.CleanupPending = published.CleanupPending
		result.ActiveToken = active.activeToken()
		result.ActiveConfigHash = active.ActiveConfigHash
		if opts.JSON {
			emitMutationResult(os.Stdout, result)
		} else {
			fmt.Fprintf(os.Stdout, "mutation %s rolled back to the prior exact config; journal cleared\n", operationID)
		}
		return 0
	default:
		result.ReconciliationRequired = true
		return failMutation(opts, os.Stdout, result, "router_unavailable",
			"router is not running; journal left intact for later reconciliation", true, 1)
	}
}
