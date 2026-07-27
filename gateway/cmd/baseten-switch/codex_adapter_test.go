package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
)

// All tests operate exclusively on t.TempDir() paths; the real
// ~/.codex and ~/.config/baseten-switch are never read or written.

// codexTestStaleOverlay has the current managed shape but points at a dead
// port, modeling a stale current overlay.
const codexTestStaleOverlay = `model_provider = "baseten"
model = "baseten-switch-compat-v1"

[model_providers.baseten]
name = "baseten"
base_url = "http://127.0.0.1:9081/v1"
wire_api = "responses"
`

// codexTestForeignOverlay lacks the baseten provider table entirely.
const codexTestForeignOverlay = `# the user's own experiment
model_provider = "other"

[model_providers.other]
name = "other"
base_url = "https://api.example.com/v1"
wire_api = "responses"
env_key = "OTHER_KEY"
`

func testCodexAdapter(t *testing.T) (*codexAdapter, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	overlay := filepath.Join(dir, "codex-home", codexOverlayName)
	out := &bytes.Buffer{}
	return &codexAdapter{
		overlayPath:   overlay,
		backupPath:    codexBackupPath(filepath.Join(dir, "backups"), overlay),
		envFilePath:   filepath.Join(dir, "baseten-switch", "env"),
		configPath:    filepath.Join(dir, "gateway.yaml"),
		clientName:    "codex",
		clientEnabled: true,
		desiredPort:   "8081",
		modelSlug:     "zai-org/GLM-5.2",
		gatewayPorts:  map[string]bool{"8081": true, "18081": true},
		out:           out,
		in:            strings.NewReader(""), // EOF = decline any consent prompt
	}, out
}

func writeOverlayFile(t *testing.T, a *codexAdapter, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(a.overlayPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.overlayPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexOnOffRoundTripRestoresFileExactly(t *testing.T) {
	a, out := testCodexAdapter(t)
	writeOverlayFile(t, a, codexTestStaleOverlay)
	if code := a.on(); code != 0 {
		t.Fatalf("on = %d (%s)", code, out.String())
	}
	if !strings.Contains(out.String(), "baseten-switch-shaped but stale") {
		t.Errorf("expected stale-replace note, got %q", out.String())
	}
	if got := fileBytes(t, a.overlayPath); !bytes.Equal(got, a.desiredOverlay()) {
		t.Fatalf("overlay after on:\n%s", got)
	}
	if _, err := os.Stat(a.backupPath); err != nil {
		t.Fatalf("backup not written: %v", err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if got := string(fileBytes(t, a.overlayPath)); got != codexTestStaleOverlay {
		t.Errorf("off did not restore byte-exactly:\n%s", got)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("backup not deleted after off: %v", err)
	}
}

func TestCodexOnCreatesFileAndOffDeletesIt(t *testing.T) {
	a, _ := testCodexAdapter(t)
	// A user config.toml next to the overlay must never be touched.
	userConfig := filepath.Join(filepath.Dir(a.overlayPath), "config.toml")
	if err := os.MkdirAll(filepath.Dir(userConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userConfig, []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	if !bytes.Equal(fileBytes(t, a.overlayPath), a.desiredOverlay()) {
		t.Fatalf("created overlay wrong:\n%s", fileBytes(t, a.overlayPath))
	}
	if got := string(fileBytes(t, a.overlayPath)); strings.Contains(got, "env_key") ||
		strings.Contains(got, codexManagedEnvKey) {
		t.Fatalf("created overlay still requires a shell token:\n%s", got)
	}
	bak, err := loadCodexBackup(a.backupPath)
	if err != nil || bak == nil {
		t.Fatalf("backup missing: %v", err)
	}
	if bak.Existed {
		t.Errorf("backup existed=true for a created file")
	}
	if len(bak.Content) != 0 {
		t.Errorf("backup content for a created file: %q", bak.Content)
	}
	if code := a.off(); code != 0 {
		t.Fatal("off failed")
	}
	if _, err := os.Stat(a.overlayPath); !os.IsNotExist(err) {
		t.Errorf("off left behind an overlay we created")
	}
	if got := string(fileBytes(t, userConfig)); got != "model = \"gpt-5\"\n" {
		t.Errorf("user config.toml touched:\n%s", got)
	}
}

func TestCodexOffWithoutOnIsNoop(t *testing.T) {
	cases := []struct {
		name    string
		content string // "" = no file
	}{
		{"no file", ""},
		{"foreign overlay untouched", codexTestForeignOverlay},
		{"stale ours-shaped overlay untouched", codexTestStaleOverlay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, out := testCodexAdapter(t)
			if tc.content != "" {
				writeOverlayFile(t, a, tc.content)
			}
			if code := a.off(); code != 0 {
				t.Fatalf("off = %d (%s)", code, out.String())
			}
			if tc.content == "" {
				if _, err := os.Stat(a.overlayPath); !os.IsNotExist(err) {
					t.Errorf("off created an overlay file")
				}
			} else if got := string(fileBytes(t, a.overlayPath)); got != tc.content {
				t.Errorf("off modified the file:\n%s", got)
			}
			if !strings.Contains(out.String(), "off") {
				t.Errorf("expected an off report, got %q", out.String())
			}
		})
	}
}

func TestCodexDoubleOnIsSafe(t *testing.T) {
	a, _ := testCodexAdapter(t)
	writeOverlayFile(t, a, codexTestStaleOverlay)
	if code := a.on(); code != 0 {
		t.Fatal("first on failed")
	}
	bak1 := fileBytes(t, a.backupPath)
	overlay1 := fileBytes(t, a.overlayPath)
	if code := a.on(); code != 0 {
		t.Fatal("second on failed")
	}
	if !bytes.Equal(bak1, fileBytes(t, a.backupPath)) {
		t.Errorf("second on rewrote the backup; the user's original file must be snapshotted once")
	}
	if !bytes.Equal(overlay1, fileBytes(t, a.overlayPath)) {
		t.Errorf("second on rewrote an already-correct overlay")
	}
	if code := a.off(); code != 0 {
		t.Fatal("off failed")
	}
	if got := string(fileBytes(t, a.overlayPath)); got != codexTestStaleOverlay {
		t.Errorf("restore after double on:\n%s", got)
	}
}

func TestCodexDoubleOffIsSafe(t *testing.T) {
	a, _ := testCodexAdapter(t)
	writeOverlayFile(t, a, codexTestStaleOverlay)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	if code := a.off(); code != 0 {
		t.Fatal("first off failed")
	}
	after1 := fileBytes(t, a.overlayPath)
	if code := a.off(); code != 0 {
		t.Fatal("second off failed")
	}
	if !bytes.Equal(after1, fileBytes(t, a.overlayPath)) {
		t.Errorf("second off modified the file")
	}
}

// TestCodexOnStaleAfterFirstOnSaysNotBackedUp: when a stale ours-shaped
// file appears AFTER the first on (which already holds the backup), the
// second on replaces it without a new snapshot and must say so, printing
// the replaced content instead of falsely claiming it was backed up.
func TestCodexOnStaleAfterFirstOnSaysNotBackedUp(t *testing.T) {
	a, out := testCodexAdapter(t)
	if code := a.on(); code != 0 {
		t.Fatal("first on failed")
	}
	bak1 := fileBytes(t, a.backupPath)
	writeOverlayFile(t, a, codexTestStaleOverlay)
	out.Reset()
	if code := a.on(); code != 0 {
		t.Fatalf("second on = %d\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "backed it up") {
		t.Errorf("second on claimed a backup it did not take: %q", out.String())
	}
	if !strings.Contains(out.String(), "WITHOUT a new backup") ||
		!strings.Contains(out.String(), `model = "baseten-switch-compat-v1"`) {
		t.Errorf("missing not-backed-up note with the replaced content: %q", out.String())
	}
	if !bytes.Equal(bak1, fileBytes(t, a.backupPath)) {
		t.Errorf("second on rewrote the first-'on' backup")
	}
	if !bytes.Equal(fileBytes(t, a.overlayPath), a.desiredOverlay()) {
		t.Errorf("second on did not replace the stale overlay")
	}
}

func TestCodexOnRefusesUnrecognizedOverlay(t *testing.T) {
	a, out := testCodexAdapter(t)
	writeOverlayFile(t, a, codexTestForeignOverlay)
	if code := a.on(); code != 1 {
		t.Fatalf("on = %d, want 1 (refusal)\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "refusing") {
		t.Errorf("expected refusal message, got %q", out.String())
	}
	if got := string(fileBytes(t, a.overlayPath)); got != codexTestForeignOverlay {
		t.Errorf("refusal modified the file:\n%s", got)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("refusal wrote a backup")
	}
}

func TestCodexOwnershipRequiresSelectedProviderModelAndBaseURL(t *testing.T) {
	a, _ := testCodexAdapter(t)
	valid := string(a.desiredOverlay())
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{
			name: "provider not selected",
			raw:  strings.Replace(valid, `model_provider = "baseten"`, `model_provider = "other"`, 1),
		},
		{
			name: "compatibility model absent",
			raw:  strings.Replace(valid, `model = "`+gateway.CodexCompatibilityModel+`"`+"\n", "", 1),
		},
		{
			name: "noncurrent model",
			raw:  strings.Replace(valid, gateway.CodexCompatibilityModel, "zai-org/GLM-5.2", 1),
		},
		{
			name: "provider table absent",
			raw:  strings.Replace(valid, codexProviderTable, "[model_providers.other]", 1),
		},
		{
			name: "base url absent",
			raw:  strings.Replace(valid, `base_url = "http://127.0.0.1:8081/v1"`+"\n", "", 1),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if codexOursShaped(parseCodexOverlay([]byte(tc.raw))) {
				t.Fatalf("incomplete ownership markers treated as baseten-switch-managed:\n%s", tc.raw)
			}
		})
	}
	if !codexOursShaped(parseCodexOverlay([]byte(valid))) {
		t.Fatalf("desired overlay not recognized as baseten-switch-managed:\n%s", valid)
	}
}

func TestCodexOnDoesNotBackupWhenAlreadyManaged(t *testing.T) {
	// Manually gateway-managed (router port instead of the door), no
	// backup: on must not snapshot our own overlay as the user's
	// original state.
	a, _ := testCodexAdapter(t)
	managed := strings.ReplaceAll(string(a.desiredOverlay()), "127.0.0.1:8081", "127.0.0.1:18081")
	writeOverlayFile(t, a, managed)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("on snapshotted an already gateway-managed overlay")
	}
	if !bytes.Equal(fileBytes(t, a.overlayPath), a.desiredOverlay()) {
		t.Errorf("on did not update to the door port:\n%s", fileBytes(t, a.overlayPath))
	}
	// off falls back to strip-only-owned: the managed file is deleted.
	if code := a.off(); code != 0 {
		t.Fatal("off failed")
	}
	if _, err := os.Stat(a.overlayPath); !os.IsNotExist(err) {
		t.Errorf("strip-only-owned did not remove the gateway-owned overlay")
	}
}

func TestCodexPoisonedBackupDiscardedNotRestored(t *testing.T) {
	a, out := testCodexAdapter(t)
	writeOverlayFile(t, a, string(a.desiredOverlay()))
	// Hand-craft a poisoned backup: its "original" content is itself a
	// gateway-managed overlay, so restoring it would re-manage codex.
	poison := strings.ReplaceAll(string(a.desiredOverlay()), "127.0.0.1:8081", "127.0.0.1:18081")
	bak := &codexBackup{
		ConfigPath:  a.overlayPath,
		Content:     []byte(poison),
		Existed:     true,
		WrittenHash: sha256Hex(fileBytes(t, a.overlayPath)),
	}
	if err := saveCodexBackup(a.backupPath, bak); err != nil {
		t.Fatal(err)
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if !strings.Contains(out.String(), "poisoned") {
		t.Errorf("expected poisoned-backup warning, got %q", out.String())
	}
	if _, err := os.Stat(a.overlayPath); !os.IsNotExist(err) {
		t.Errorf("poisoned content restored or overlay kept")
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("poisoned backup not discarded")
	}
}

func TestCodexUserDriftStillManagedStripsAndNotes(t *testing.T) {
	a, out := testCodexAdapter(t)
	writeOverlayFile(t, a, codexTestStaleOverlay)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	// User edits OUR overlay between on and off (codex never rewrites
	// it, so any drift is user-made). Still ours + gateway port.
	edited := string(a.desiredOverlay()) + "# user note\n"
	writeOverlayFile(t, a, edited)
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if !strings.Contains(out.String(), "changed since 'on'") || !strings.Contains(out.String(), "warning:") {
		t.Errorf("expected user-drift warning, got %q", out.String())
	}
	if _, err := os.Stat(a.overlayPath); !os.IsNotExist(err) {
		t.Errorf("drifted gateway-owned overlay not removed")
	}
	// The pre-'on' file was consumed without being restored; its
	// content must be surfaced so nothing is silently lost.
	if !strings.Contains(out.String(), "NOT restored") ||
		!strings.Contains(out.String(), `model = "baseten-switch-compat-v1"`) {
		t.Errorf("missing not-restored note with the original content: %q", out.String())
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("backup not cleared after drift off")
	}
}

func TestCodexDriftRedirectedLeavesFile(t *testing.T) {
	a, out := testCodexAdapter(t)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	// Someone points the overlay elsewhere after on: not ours to touch.
	redirected := strings.ReplaceAll(string(a.desiredOverlay()), "http://127.0.0.1:8081/v1", "https://other-proxy.example.com/v1")
	writeOverlayFile(t, a, redirected)
	if code := a.off(); code != 0 {
		t.Fatalf("off = %d (%s)", code, out.String())
	}
	if !strings.Contains(out.String(), "no longer points at the gateway") {
		t.Errorf("expected redirected-drift warning, got %q", out.String())
	}
	if got := string(fileBytes(t, a.overlayPath)); got != redirected {
		t.Errorf("foreign-pointing overlay touched:\n%s", got)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Errorf("backup not cleared after redirected drift off")
	}
}

// TestCodexOverlayParseSurvivesAnnotations: legal TOML the user may
// leave on managed lines (inline comments, single-quoted strings) must
// not defeat the ownership test, or off would leave the gateway wiring
// installed while consuming the backup.
func TestCodexOverlayParseSurvivesAnnotations(t *testing.T) {
	t.Run("codexTOMLValue", func(t *testing.T) {
		cases := []struct{ in, want string }{
			{` "http://127.0.0.1:8081/v1"`, "http://127.0.0.1:8081/v1"},
			{` "http://127.0.0.1:8081/v1"  # gateway door`, "http://127.0.0.1:8081/v1"},
			{` 'http://127.0.0.1:8081/v1'`, "http://127.0.0.1:8081/v1"},
			{` 'http://127.0.0.1:8081/v1'  # single-quoted`, "http://127.0.0.1:8081/v1"},
			{` "CODEX_AUTH_TOKEN"  # stub token`, "CODEX_AUTH_TOKEN"},
			{` "keeps # inside quotes"`, "keeps # inside quotes"},
			{` bare  # comment`, "bare"},
			{` "unterminated`, "unterminated"},
			{``, ""},
		}
		for _, tc := range cases {
			if got := codexTOMLValue(tc.in); got != tc.want {
				t.Errorf("codexTOMLValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})
	t.Run("annotated overlay stays managed through off", func(t *testing.T) {
		annotations := []struct{ name, from, to string }{
			{
				"single-quoted pinned model",
				`model = "` + gateway.CodexCompatibilityModel + `"`,
				`model = '` + gateway.CodexCompatibilityModel + `'  # pinned model`,
			},
			{
				"single-quoted base_url",
				`base_url = "http://127.0.0.1:8081/v1"`,
				`base_url = 'http://127.0.0.1:8081/v1'  # gateway door`,
			},
		}
		for _, an := range annotations {
			t.Run(an.name, func(t *testing.T) {
				a, out := testCodexAdapter(t)
				writeOverlayFile(t, a, codexTestStaleOverlay)
				if code := a.on(); code != 0 {
					t.Fatal("on failed")
				}
				edited := strings.Replace(string(a.desiredOverlay()), an.from, an.to, 1)
				if edited == string(a.desiredOverlay()) {
					t.Fatalf("annotation %q did not apply; fixture drifted", an.from)
				}
				writeOverlayFile(t, a, edited)
				if code := a.off(); code != 0 {
					t.Fatalf("off = %d (%s)", code, out.String())
				}
				if strings.Contains(out.String(), "no longer points at the gateway") {
					t.Errorf("annotation misclassified the overlay as foreign: %q", out.String())
				}
				if _, err := os.Stat(a.overlayPath); !os.IsNotExist(err) {
					t.Errorf("annotated gateway-owned overlay not removed")
				}
			})
		}
	})
}

// TestCodexMissingDefaultModelBlocksOnlyOn: a config whose default model was
// removed after 'on' must not block off/status (they never need the
// slug); only on fails, naming the fix.
func TestCodexMissingDefaultModelBlocksOnlyOn(t *testing.T) {
	a, out := testCodexAdapter(t)
	writeOverlayFile(t, a, codexTestStaleOverlay)
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	a.modelSlug = ""
	a.slugErr = fmt.Errorf("no default_model configured for the codex client in gateway.yaml")
	out.Reset()
	if code := a.on(); code != 1 {
		t.Fatalf("on with missing default model = %d, want 1\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "default_model") {
		t.Errorf("on failure does not name the fix: %q", out.String())
	}
	var stdout bytes.Buffer
	if code := a.status(&stdout); code != 0 {
		t.Errorf("status with missing default model = %d, want 0 (overlay still managed)", code)
	}
	if !strings.Contains(stdout.String(), "unresolved") {
		t.Errorf("status does not report the unresolved slug: %q", stdout.String())
	}
	if code := a.off(); code != 0 {
		t.Fatalf("off with missing default model = %d, want 0 (restore must not need the slug)\n%s", code, out.String())
	}
	if got := string(fileBytes(t, a.overlayPath)); got != codexTestStaleOverlay {
		t.Errorf("off did not restore the original overlay:\n%s", got)
	}
}

func TestCodexOnRefusesNoncurrentModelOverlay(t *testing.T) {
	a, out := testCodexAdapter(t)
	unrecognized := strings.Replace(
		string(a.desiredOverlay()),
		gateway.CodexCompatibilityModel,
		a.modelSlug,
		1,
	)
	writeOverlayFile(t, a, unrecognized)
	if code := a.on(); code != 1 {
		t.Fatalf("on = %d, want refusal (%s)", code, out.String())
	}
	if !strings.Contains(out.String(), "not baseten-switch-managed") ||
		!strings.Contains(out.String(), "refusing to overwrite") {
		t.Fatalf("refusal did not classify the old shape as unrecognized: %s", out.String())
	}
	if got := string(fileBytes(t, a.overlayPath)); got != unrecognized {
		t.Fatalf("refusal changed the old overlay:\n%s", got)
	}
	if _, err := os.Stat(a.backupPath); !os.IsNotExist(err) {
		t.Fatalf("refusal created a backup: %v", err)
	}
}

func TestCodexRouteJournaledMutationLeavesOverlayByteIdentical(t *testing.T) {
	a, out := testCodexAdapter(t)
	configBody := `global:
  routing_enabled: true
clients:
  - name: codex
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: openai
    default_model: zai-org/GLM-5.2 # selected target
    fallback_route: openai
door:
  ports:
    - bind_addr: 127.0.0.1:8081
      router_addr: 127.0.0.1:18081
`
	if err := os.WriteFile(a.configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "gateway.pid"))
	if code := a.on(); code != 0 {
		t.Fatalf("on = %d (%s)", code, out.String())
	}
	overlayBefore := fileBytes(t, a.overlayPath)

	var stdout bytes.Buffer
	code := a.route([]string{
		"moonshotai/Kimi-K2.7-Code",
		"--json",
		"--operation-id", "codex-route-offline",
	}, &stdout)
	if code != 0 {
		t.Fatalf("route = %d (%s)", code, stdout.String())
	}
	result := decodeMutationResult(t, stdout.String())
	if !result.OK ||
		result.Operation != "set_codex_route" ||
		result.RequestedTarget != "moonshotai/Kimi-K2.7-Code" ||
		result.Client != "codex" ||
		result.Key != "default_model" ||
		result.Applied ||
		!result.ReconciliationRequired {
		t.Fatalf("mutation result = %+v", result)
	}
	f, err := config.Load(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Clients[0].DefaultModel; got != "moonshotai/Kimi-K2.7-Code" {
		t.Fatalf("default_model = %q", got)
	}
	if got := fileBytes(t, a.overlayPath); !bytes.Equal(got, overlayBefore) {
		t.Fatalf("route rewrote managed overlay\nbefore:\n%s\nafter:\n%s", overlayBefore, got)
	}
	journal, err := readMutationJournal(a.configPath, "codex-route-offline")
	if err != nil {
		t.Fatal(err)
	}
	if journal.Operation != "set_codex_route" ||
		journal.RequestedTarget != "moonshotai/Kimi-K2.7-Code" ||
		journal.Client != "codex" ||
		journal.Key != "default_model" {
		t.Fatalf("journal = %+v", journal)
	}
}

func TestCodexRouteRejectsNonSlugWithoutMutation(t *testing.T) {
	a, _ := testCodexAdapter(t)
	configBody := `global:
  routing_enabled: true
clients:
  - name: codex
    enabled: true
    protocol_shape: openai
    default_model: zai-org/GLM-5.2
`
	if err := os.WriteFile(a.configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, a.configPath)
	var stdout bytes.Buffer
	if code := a.route([]string{"gpt-5.6-sol", "--json"}, &stdout); code != 2 {
		t.Fatalf("route = %d, want 2 (%s)", code, stdout.String())
	}
	result := decodeMutationResult(t, stdout.String())
	if result.Error == nil || result.Error.Code != "invalid_route_target" {
		t.Fatalf("result = %+v", result)
	}
	if got := fileBytes(t, a.configPath); !bytes.Equal(got, before) {
		t.Fatal("invalid route mutated config")
	}
}

func TestCodexRouteDoesNotInspectUnrecognizedOverlay(t *testing.T) {
	a, _ := testCodexAdapter(t)
	configBody := `global:
  routing_enabled: true
clients:
  - name: codex
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: openai
    default_model: zai-org/GLM-5.2
    fallback_route: openai
door:
  ports:
    - bind_addr: 127.0.0.1:8081
      router_addr: 127.0.0.1:18081
`
	if err := os.WriteFile(a.configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(t.TempDir(), "gateway.pid"))
	unrecognized := strings.Replace(
		string(a.desiredOverlay()),
		gateway.CodexCompatibilityModel,
		a.modelSlug,
		1,
	)
	writeOverlayFile(t, a, unrecognized)
	var stdout bytes.Buffer
	if code := a.route([]string{
		"moonshotai/Kimi-K2.7-Code",
		"--json",
		"--operation-id", "route-with-unrecognized-profile",
	}, &stdout); code != 0 {
		t.Fatalf("route = %d, want success (%s)", code, stdout.String())
	}
	result := decodeMutationResult(t, stdout.String())
	if !result.OK || result.Error != nil {
		t.Fatalf("result = %+v", result)
	}
	f, err := config.Load(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Clients[0].DefaultModel; got != "moonshotai/Kimi-K2.7-Code" {
		t.Fatalf("default_model = %q", got)
	}
	if got := string(fileBytes(t, a.overlayPath)); got != unrecognized {
		t.Fatalf("route changed the unrecognized overlay:\n%s", got)
	}
}

func TestCodexGatewayAuthTokenStubPlacement(t *testing.T) {
	t.Run("created for gateway without shell guidance", func(t *testing.T) {
		a, out := testCodexAdapter(t)
		if code := a.on(); code != 0 {
			t.Fatal("on failed")
		}
		env, err := config.LoadEnvFile(a.envFilePath)
		if err != nil {
			t.Fatal(err)
		}
		if env[codexManagedEnvKey] != codexAuthTokenStub {
			t.Errorf("env file token = %q, want %q", env[codexManagedEnvKey], codexAuthTokenStub)
		}
		if strings.Contains(out.String(), "export ") {
			t.Errorf("on still asks the user to export a token: %q", out.String())
		}
		if got := string(fileBytes(t, a.overlayPath)); strings.Contains(got, "env_key") {
			t.Errorf("Codex profile still reads the gateway placeholder from the shell:\n%s", got)
		}
	})
	t.Run("idempotent across double on", func(t *testing.T) {
		a, _ := testCodexAdapter(t)
		if code := a.on(); code != 0 {
			t.Fatal("first on failed")
		}
		first := fileBytes(t, a.envFilePath)
		if code := a.on(); code != 0 {
			t.Fatal("second on failed")
		}
		if !bytes.Equal(first, fileBytes(t, a.envFilePath)) {
			t.Errorf("second on modified the env file:\n%s", fileBytes(t, a.envFilePath))
		}
	})
	t.Run("never clobbers an existing value", func(t *testing.T) {
		a, _ := testCodexAdapter(t)
		pre := "# managed by hand\nBASETEN_API_KEY=abc123\nCODEX_AUTH_TOKEN=user-chosen\n"
		if err := os.MkdirAll(filepath.Dir(a.envFilePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(a.envFilePath, []byte(pre), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := a.on(); code != 0 {
			t.Fatal("on failed")
		}
		if got := string(fileBytes(t, a.envFilePath)); got != pre {
			t.Errorf("existing env file modified:\n%s", got)
		}
	})
	t.Run("appends preserving other content", func(t *testing.T) {
		a, _ := testCodexAdapter(t)
		pre := "# comment kept\nBASETEN_API_KEY=abc123\n"
		if err := os.MkdirAll(filepath.Dir(a.envFilePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(a.envFilePath, []byte(pre), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := a.on(); code != 0 {
			t.Fatal("on failed")
		}
		got := string(fileBytes(t, a.envFilePath))
		if !strings.HasPrefix(got, pre) {
			t.Errorf("existing content not preserved byte-exactly:\n%s", got)
		}
		if !strings.Contains(got, codexManagedEnvKey+"="+codexAuthTokenStub) {
			t.Errorf("stub not appended:\n%s", got)
		}
	})
}

// codexParkedGatewayYAML is a comment-carrying config with the codex
// client parked, for the un-park consent tests.
const codexParkedGatewayYAML = `# header comment survives
global:
  routing_enabled: true
clients:
  - name: codex
    enabled: false  # parked
    bind_addr: 127.0.0.1:18081
    protocol_shape: openai
    default_model: zai-org/GLM-5.2
door:
  ports:
    - bind_addr: 127.0.0.1:8081
      router_addr: 127.0.0.1:18081
`

// writeParkedGatewayYAML writes the parked fixture at the adapter's
// configPath and points the router and door pidfiles at scratch paths
// (missing unless the test writes them), so no live process is ever
// signaled.
func writeParkedGatewayYAML(t *testing.T, a *codexAdapter) {
	t.Helper()
	if err := os.WriteFile(a.configPath, []byte(codexParkedGatewayYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_GATEWAY_PIDFILE", filepath.Join(filepath.Dir(a.configPath), "gw.pid"))
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", filepath.Join(filepath.Dir(a.configPath), "door.pid"))
}

func loadCodexEnabled(t *testing.T, path string) bool {
	t.Helper()
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Clients {
		if c.Name == "codex" {
			return c.Enabled
		}
	}
	t.Fatal("no codex client in config")
	return false
}

// TestCodexOnParkedDeclinedLeavesConfig: EOF/decline on the consent
// prompt leaves the client parked, signals nothing, and on still
// succeeds (the overlay is additive). The prompt must name the known
// side effect: the door SIGHUP rebinds the shared door port, briefly
// resetting live Claude Code connections.
func TestCodexOnParkedDeclinedLeavesConfig(t *testing.T) {
	for _, resp := range []string{"", "n\n"} {
		a, out := testCodexAdapter(t)
		a.clientEnabled = false
		a.in = strings.NewReader(resp)
		writeParkedGatewayYAML(t, a)
		signals := recordSignals(t)
		if code := a.on(); code != 0 {
			t.Fatalf("on = %d (declining un-park is not a refusal)\n%s", code, out.String())
		}
		for _, want := range []string{"parked", "resetting live Claude Code connections", "port 8081", "[y/N]", "left parked"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("resp %q: output missing %q: %q", resp, want, out.String())
			}
		}
		if loadCodexEnabled(t, a.configPath) {
			t.Errorf("resp %q: declined consent but the client was enabled", resp)
		}
		if len(*signals) != 0 {
			t.Errorf("resp %q: declined consent but the router was signaled: %v", resp, *signals)
		}
	}
}

// TestCodexOnUnparksWithConsent covers the consented flip: the enabled
// scalar is rewritten comment-preservingly, the router is SIGHUPed via
// its pidfile, on polls admin status until the client reports enabled
// AND currently bound (the applied listener state, not the enabled
// field, which is re-read from the file the flip just wrote), or notes
// the timeout when the admin never confirms; then the door is SIGHUPed
// via its own pidfile so the shared port spec picks up the client.
func TestCodexOnUnparksWithConsent(t *testing.T) {
	cases := []struct {
		name        string
		adminJSON   string // "" = admin unreachable
		doorUp      bool
		timesOut    bool // poll never satisfied; shorten the timeout
		wantReport  string
		wantSignals int
	}{
		{
			name:        "verified bound, door reloaded",
			adminJSON:   `{"clients":[{"name":"codex","enabled":true,"currently_bound":true}]}`,
			doorUp:      true,
			wantReport:  "codex client verified enabled and bound",
			wantSignals: 2,
		},
		{
			// enabled=true from the file but no bound listener (a
			// failed reload keeps current listeners): the verify must
			// NOT report success.
			name:        "enabled but unbound is not verified",
			adminJSON:   `{"clients":[{"name":"codex","enabled":true,"currently_bound":false}]}`,
			timesOut:    true,
			wantReport:  "did not report a bound codex listener",
			wantSignals: 1,
		},
		{
			name:        "admin unreachable notes",
			timesOut:    true,
			wantReport:  "did not report a bound codex listener",
			wantSignals: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, out := testCodexAdapter(t)
			a.clientEnabled = false
			a.in = strings.NewReader("y\n")
			writeParkedGatewayYAML(t, a)
			if err := pidfile.WriteAt(pidfile.Path(), os.Getpid()); err != nil {
				t.Fatal(err)
			}
			if tc.doorUp {
				if err := pidfile.WriteAt(pidfile.DoorPath(), os.Getpid()); err != nil {
					t.Fatal(err)
				}
			}
			if tc.adminJSON != "" {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, tc.adminJSON)
				}))
				defer srv.Close()
				t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", strings.TrimPrefix(srv.URL, "http://"))
			} else {
				t.Setenv("BASETEN_SWITCH_ADMIN_ADDR", "127.0.0.1:1")
			}
			if tc.timesOut {
				old := routeApplyTimeout
				routeApplyTimeout = 300 * time.Millisecond
				t.Cleanup(func() { routeApplyTimeout = old })
			}
			signals := recordSignals(t)
			if code := a.on(); code != 0 {
				t.Fatalf("on = %d\n%s", code, out.String())
			}
			if !loadCodexEnabled(t, a.configPath) {
				t.Error("consented un-park did not enable the client")
			}
			raw := string(fileBytes(t, a.configPath))
			if !strings.Contains(raw, "enabled: true  # parked") || !strings.Contains(raw, "# header comment survives") {
				t.Errorf("comment-preserving flip failed:\n%s", raw)
			}
			if len(*signals) != tc.wantSignals {
				t.Errorf("signals = %v, want %d SIGHUPs (router%s)", *signals, tc.wantSignals,
					map[bool]string{true: " + door", false: " only, door down"}[tc.doorUp])
			}
			if !strings.Contains(out.String(), tc.wantReport) {
				t.Errorf("output missing %q: %q", tc.wantReport, out.String())
			}
			if tc.doorUp {
				if !strings.Contains(out.String(), "door reloaded") {
					t.Errorf("output missing the door reload report: %q", out.String())
				}
			} else if !strings.Contains(out.String(), "door not running") {
				t.Errorf("output missing the door-down notice: %q", out.String())
			}
		})
	}
}

// TestCodexOnUnparkRouterDown: consented flip with no router running
// still edits the config and says the change applies at the next
// start, with no signal sent.
func TestCodexOnUnparkRouterDown(t *testing.T) {
	a, out := testCodexAdapter(t)
	a.clientEnabled = false
	a.in = strings.NewReader("y\n")
	writeParkedGatewayYAML(t, a) // pidfile path set but never written = router down
	signals := recordSignals(t)
	if code := a.on(); code != 0 {
		t.Fatalf("on = %d\n%s", code, out.String())
	}
	if !loadCodexEnabled(t, a.configPath) {
		t.Error("consented un-park did not enable the client")
	}
	if !strings.Contains(out.String(), "applies at next start") {
		t.Errorf("output missing applies-at-next-start notice: %q", out.String())
	}
	if len(*signals) != 0 {
		t.Errorf("router down but signaled: %v", *signals)
	}
}

// TestCodexOnUnparkEditFailure: a consented flip that cannot edit the
// config (file gone) fails on with exit 1.
func TestCodexOnUnparkEditFailure(t *testing.T) {
	a, out := testCodexAdapter(t)
	a.clientEnabled = false
	a.in = strings.NewReader("y\n")
	// No gateway.yaml at configPath.
	if code := a.on(); code != 1 {
		t.Fatalf("on = %d, want 1\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "enable the codex client") {
		t.Errorf("output missing the enable failure: %q", out.String())
	}
}

// TestCodexClientYAMLMatchesTemplate pins the paste block offered by
// the missing-client error to the shipped template: every line of the
// block must appear verbatim in the template, so the two can never
// drift apart silently.
func TestCodexClientYAMLMatchesTemplate(t *testing.T) {
	for _, line := range strings.Split(codexClientYAML, "\n") {
		if !strings.Contains(string(config.InitTemplate), line+"\n") {
			t.Errorf("paste block line %q not found in config/gateway.example.yaml; keep codexClientYAML in sync with the template", line)
		}
	}
	_, _, _, err := codexDoorPort(&config.File{Clients: []config.Client{{Name: "claude-code", ProtocolShape: "anthropic"}}})
	if err == nil || !strings.Contains(err.Error(), codexClientYAML) {
		t.Errorf("missing-client error does not carry the paste block: %v", err)
	}
}

func TestCodexStatusExitCodes(t *testing.T) {
	a, _ := testCodexAdapter(t)
	var stdout bytes.Buffer
	if code := a.status(&stdout); code != statusExitOff {
		t.Errorf("status with no overlay = %d, want %d", code, statusExitOff)
	}
	for _, want := range []string{"absent", testDoorPort, "zai-org/GLM-5.2"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("status missing %q: %q", want, stdout.String())
		}
	}
	if code := a.on(); code != 0 {
		t.Fatal("on failed")
	}
	stdout.Reset()
	if code := a.status(&stdout); code != 0 {
		t.Errorf("status when on = %d, want 0", code)
	}
	for _, want := range []string{"on (managed overlay", "http://127.0.0.1:8081/v1", "backup:"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("status missing %q: %q", want, stdout.String())
		}
	}
	// Parked client and unrecognized overlay are both reported.
	a.clientEnabled = false
	writeOverlayFile(t, a, codexTestForeignOverlay)
	stdout.Reset()
	if code := a.status(&stdout); code != statusExitOff {
		t.Errorf("status with foreign overlay = %d, want %d", code, statusExitOff)
	}
	for _, want := range []string{"parked", "not baseten-switch-managed"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("status missing %q: %q", want, stdout.String())
		}
	}
}

func TestCodexDoorPortResolution(t *testing.T) {
	claudeClient := config.Client{Name: "claude-code", BindAddr: "127.0.0.1:18081", ProtocolShape: "anthropic"}
	codexClient := config.Client{Name: "codex", Enabled: true, BindAddr: "127.0.0.1:18081", ProtocolShape: "openai"}
	door := &config.Door{Ports: []config.DoorPort{{BindAddr: "127.0.0.1:8081", RouterAddr: "127.0.0.1:18081"}}}

	cases := []struct {
		name      string
		file      *config.File
		wantName  string
		wantPort  string
		wantErr   string
		wantPorts []string
	}{
		{
			name:      "door port resolved for codex on the shared listener",
			file:      &config.File{Clients: []config.Client{claudeClient, codexClient}, Door: door},
			wantName:  "codex",
			wantPort:  "8081",
			wantPorts: []string{"8081", "18081"},
		},
		{
			name:     "parked codex client still resolves",
			file:     &config.File{Clients: []config.Client{{Name: "codex", Enabled: false, BindAddr: "127.0.0.1:18081", ProtocolShape: "openai"}}, Door: door},
			wantName: "codex",
			wantPort: "8081",
		},
		{
			name:     "first openai-shape client when none named codex",
			file:     &config.File{Clients: []config.Client{claudeClient, {Name: "opencode", Enabled: true, BindAddr: "127.0.0.1:18081", ProtocolShape: "openai"}}, Door: door},
			wantName: "opencode",
			wantPort: "8081",
		},
		{
			name:    "no openai-shape client",
			file:    &config.File{Clients: []config.Client{claudeClient}, Door: door},
			wantErr: "no openai-shape client",
		},
		{
			name:    "no door section",
			file:    &config.File{Clients: []config.Client{codexClient}},
			wantErr: "no door: section",
		},
		{
			name:    "door does not route to the codex listener",
			file:    &config.File{Clients: []config.Client{{Name: "codex", Enabled: true, BindAddr: "127.0.0.1:19999", ProtocolShape: "openai"}}, Door: door},
			wantErr: "no door port routes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, port, ports, err := codexDoorPort(tc.file)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want contains %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if client.Name != tc.wantName {
				t.Errorf("client = %q, want %q", client.Name, tc.wantName)
			}
			if port != tc.wantPort {
				t.Errorf("port = %q, want %q", port, tc.wantPort)
			}
			for _, p := range tc.wantPorts {
				if !ports[p] {
					t.Errorf("gateway ports missing %s: %v", p, ports)
				}
			}
		})
	}
}

func TestCodexDefaultSlug(t *testing.T) {
	c := &config.Client{Name: "codex", DefaultModel: "zai-org/GLM-5.2"}
	if s, err := codexDefaultSlug(c); err != nil || s != "zai-org/GLM-5.2" {
		t.Errorf("client default model: %q, %v", s, err)
	}
	if _, err := codexDefaultSlug(&config.Client{Name: "codex"}); err == nil || !strings.Contains(err.Error(), "default_model") {
		t.Errorf("missing default model err = %v", err)
	}
}

// TestCmdCodexEnvPlumbing exercises the cmdCodex entry point end to
// end against temp paths via the documented env overrides.
func TestCmdCodexEnvPlumbing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.yaml")
	cfg := `global:
  routing_enabled: true
clients:
  - name: codex
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: openai
    default_model: zai-org/GLM-5.2
door:
  ports:
    - bind_addr: 127.0.0.1:8081
      router_addr: 127.0.0.1:18081
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(dir, "codex-home")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", cfgPath)
	t.Setenv("BASETEN_SWITCH_CODEX_HOME", codexHome)
	t.Setenv("BASETEN_SWITCH_BACKUP_DIR", filepath.Join(dir, "backups"))
	t.Setenv("BASETEN_SWITCH_ENV_FILE", filepath.Join(dir, "env"))

	overlay := filepath.Join(codexHome, codexOverlayName)
	if code := cmdCodex([]string{"on"}); code != 0 {
		t.Fatalf("cmdCodex on = %d", code)
	}
	raw := string(fileBytes(t, overlay))
	for _, want := range []string{`model_provider = "baseten"`, `model = "` + gateway.CodexCompatibilityModel + `"`, `base_url = "http://127.0.0.1:8081/v1"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("overlay missing %s:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "env_key") || strings.Contains(raw, codexManagedEnvKey) {
		t.Errorf("overlay unexpectedly requires a shell token:\n%s", raw)
	}
	if code := cmdCodex([]string{"status"}); code != 0 {
		t.Errorf("status while on = %d", code)
	}
	// stop alias == off
	if code := cmdCodex([]string{"stop"}); code != 0 {
		t.Fatalf("cmdCodex stop = %d", code)
	}
	if _, err := os.Stat(overlay); !os.IsNotExist(err) {
		t.Errorf("stop did not remove the created overlay")
	}
	if code := cmdCodex([]string{"status"}); code != statusExitOff {
		t.Errorf("status while off = %d, want %d", code, statusExitOff)
	}
	if code := cmdCodex([]string{"bogus"}); code != 2 {
		t.Errorf("bogus subcommand = %d, want 2", code)
	}
	if code := cmdCodex(nil); code != 2 {
		t.Errorf("no subcommand = %d, want 2", code)
	}
}

// TestCmdCodexNoDefaultModelOffStillWorks pins the constructor's lazy slug
// resolution end to end: with default_model gone from gateway.yaml, off
// and status must still run (off restores/removes the overlay), and
// only on fails naming the missing default model.
func TestCmdCodexNoDefaultModelOffStillWorks(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.yaml")
	cfg := `clients:
  - name: codex
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: openai
door:
  ports:
    - bind_addr: 127.0.0.1:8081
      router_addr: 127.0.0.1:18081
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(dir, "codex-home")
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", cfgPath)
	t.Setenv("BASETEN_SWITCH_CODEX_HOME", codexHome)
	t.Setenv("BASETEN_SWITCH_BACKUP_DIR", filepath.Join(dir, "backups"))
	t.Setenv("BASETEN_SWITCH_ENV_FILE", filepath.Join(dir, "env"))

	if code := cmdCodex([]string{"on"}); code != 1 {
		t.Errorf("on without default_model = %d, want 1", code)
	}
	if code := cmdCodex([]string{"status"}); code != statusExitOff {
		t.Errorf("status without default_model = %d, want %d", code, statusExitOff)
	}
	if code := cmdCodex([]string{"off"}); code != 0 {
		t.Errorf("off without default_model = %d, want 0 (restore must not depend on it)", code)
	}
}
