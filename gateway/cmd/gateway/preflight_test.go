package gateway

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

// noAuthStore points the auth loader at an empty temp store so preflight
// sees "no OAuth credential" regardless of the developer's real keychain.
func noAuthStore(t *testing.T) {
	t.Helper()
	t.Setenv("BASETEN_SWITCH_AUTH_NO_KEYRING", "1")
	t.Setenv("BASETEN_SWITCH_AUTH_FILE", filepath.Join(t.TempDir(), "auth.json"))
}

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnresolvedPlaceholders(t *testing.T) {
	t.Setenv("TEST_PF_SET", "resolved")
	f := &config.File{
		Global: config.Global{
			Auth: map[string]string{"baseten": "${TEST_PF_SET}"},
		},
		Clients: []config.Client{
			{
				Name:      "claude-code",
				Enabled:   true,
				AuthToken: &config.AuthToken{Header: "authorization", Value: "${TEST_PF_UNSET}"},
			},
			// A disabled client binds nothing, so its references are
			// inert and must not be warned about (the template ships
			// the codex client parked with ${CODEX_AUTH_TOKEN}).
			{
				Name:      "codex",
				Enabled:   false,
				AuthToken: &config.AuthToken{Header: "authorization", Value: "${TEST_PF_PARKED_UNSET}"},
			},
		},
	}
	got := UnresolvedPlaceholders(f)
	if len(got) != 1 || got[0] != "TEST_PF_UNSET" {
		t.Fatalf("UnresolvedPlaceholders = %v, want [TEST_PF_UNSET] (parked client excluded)", got)
	}
	var buf bytes.Buffer
	warnUnresolvedPlaceholders(f, &buf)
	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("want exactly one warning line, got:\n%s", out)
	}
	if !strings.Contains(out, "${TEST_PF_UNSET}") {
		t.Fatalf("warning does not name the variable:\n%s", out)
	}
	if !strings.Contains(out, config.EnvFilePath()) {
		t.Fatalf("warning does not name the env file fix:\n%s", out)
	}
	if strings.Contains(out, "TEST_PF_SET") {
		t.Fatalf("resolved placeholder should not be warned about:\n%s", out)
	}
}

// TestApplyGlobalAuthAtStartup verifies the startup/PUT symmetry fix: a
// ${VAR} key set via global.auth in gateway.yaml lands in the process env
// and on cfg.BasetenKey without an admin PUT.
func TestApplyGlobalAuthAtStartup(t *testing.T) {
	t.Setenv("TEST_PF_BASETEN_SWITCH_KEY", "sk-boot-42")
	t.Setenv("BASETEN_API_KEY", "")
	path := writeYAML(t, "global:\n  auth:\n    baseten: ${TEST_PF_BASETEN_SWITCH_KEY}\nclients: []\n")
	cfg := Config{ConfigPath: path}
	applyGlobalAuth(&cfg)
	if got := os.Getenv("BASETEN_API_KEY"); got != "sk-boot-42" {
		t.Fatalf("BASETEN_API_KEY = %q, want sk-boot-42", got)
	}
	if cfg.BasetenKey != "sk-boot-42" {
		t.Fatalf("cfg.BasetenKey = %q, want sk-boot-42", cfg.BasetenKey)
	}
}

func TestApplyGlobalAuthUnsetPlaceholderStaysEmpty(t *testing.T) {
	t.Setenv("BASETEN_API_KEY", "")
	path := writeYAML(t, "global:\n  auth:\n    baseten: ${TEST_PF_NO_SUCH_KEY}\nclients: []\n")
	cfg := Config{ConfigPath: path}
	applyGlobalAuth(&cfg)
	if got := os.Getenv("BASETEN_API_KEY"); got != "" {
		t.Fatalf("BASETEN_API_KEY = %q, want empty (placeholder unset)", got)
	}
	if cfg.BasetenKey != "" {
		t.Fatalf("cfg.BasetenKey = %q, want empty", cfg.BasetenKey)
	}
}

func TestApplyGlobalAuthMissingFileIsNoop(t *testing.T) {
	cfg := Config{ConfigPath: filepath.Join(t.TempDir(), "no-such.yaml"), BasetenKey: "keep"}
	applyGlobalAuth(&cfg)
	if cfg.BasetenKey != "keep" {
		t.Fatalf("cfg.BasetenKey = %q, want keep", cfg.BasetenKey)
	}
}

func TestBasetenRoutedClients(t *testing.T) {
	resolved := []resolvedClientConfig{
		{Name: "claude-code", Route: "baseten"},
		{Name: "opencode", Route: "openai", FallbackRoute: "baseten"},
		{Name: "codex", Route: "anthropic"},
		{Name: "mon", Route: "monitor"},
	}
	got := basetenRoutedClients(resolved)
	want := []string{"claude-code (route: baseten)", "opencode (fallback_route: baseten)"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestRunPreflightBanner(t *testing.T) {
	oauthAuthJSON := `{"version":1,"current":"me","profiles":{"me":{"remote_url":"https://api.baseten.co","auth_type":"oauth","oauth_credential":{"access_token":"at","refresh_token":"rt"}}}}`
	apiKeyAuthJSON := `{"version":1,"current":"svc","profiles":{"svc":{"remote_url":"https://api.baseten.co","auth_type":"api_key","api_key":"sk-1"}}}`

	cases := []struct {
		name       string
		resolved   []resolvedClientConfig
		basetenKey string
		authJSON   string // "" = empty store
		wantBanner bool
	}{
		{
			name:       "baseten route without creds warns",
			resolved:   []resolvedClientConfig{{Name: "claude-code", Route: "baseten"}},
			wantBanner: true,
		},
		{
			name:       "baseten fallback route without creds warns",
			resolved:   []resolvedClientConfig{{Name: "opencode", Route: "openai", FallbackRoute: "baseten"}},
			wantBanner: true,
		},
		{
			name:       "passthrough routes never warn",
			resolved:   []resolvedClientConfig{{Name: "claude-code", Route: "anthropic"}, {Name: "codex", Route: "openai"}},
			wantBanner: false,
		},
		{
			name:       "api key suppresses banner",
			resolved:   []resolvedClientConfig{{Name: "claude-code", Route: "baseten"}},
			basetenKey: "sk-x",
			wantBanner: false,
		},
		{
			name:       "oauth credential suppresses banner",
			resolved:   []resolvedClientConfig{{Name: "claude-code", Route: "baseten"}},
			authJSON:   oauthAuthJSON,
			wantBanner: false,
		},
		{
			name:       "api_key-type CLI profile suppresses banner",
			resolved:   []resolvedClientConfig{{Name: "claude-code", Route: "baseten"}},
			authJSON:   apiKeyAuthJSON,
			wantBanner: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noAuthStore(t)
			if tc.authJSON != "" {
				path := filepath.Join(t.TempDir(), "auth.json")
				if err := os.WriteFile(path, []byte(tc.authJSON), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("BASETEN_SWITCH_AUTH_FILE", path)
			}
			cfg := Config{
				ConfigPath: filepath.Join(t.TempDir(), "no-such.yaml"),
				BasetenKey: tc.basetenKey,
			}
			var buf bytes.Buffer
			runPreflight(&cfg, tc.resolved, &buf)
			out := buf.String()
			gotBanner := strings.Contains(out, "WARNING: no Baseten credential")
			if gotBanner != tc.wantBanner {
				t.Fatalf("banner = %t, want %t; output:\n%s", gotBanner, tc.wantBanner, out)
			}
			if tc.wantBanner {
				if !strings.Contains(out, tc.resolved[0].Name) {
					t.Fatalf("banner does not name client %q:\n%s", tc.resolved[0].Name, out)
				}
				if !strings.Contains(out, "baseten auth login") {
					t.Fatalf("banner does not name the fix:\n%s", out)
				}
			}
		})
	}
}

// TestRunPreflightWarnsPlaceholdersFromFile exercises the full startup
// path: a real gateway.yaml with an unresolved ${VAR} produces a warning
// line, and preflight stays warn-only (no panic, no error).
func TestRunPreflightWarnsPlaceholdersFromFile(t *testing.T) {
	noAuthStore(t)
	path := writeYAML(t, `global:
  routing_enabled: false
  auth:
    baseten: ${TEST_PF_MISSING_KEY}
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:0
    protocol_shape: anthropic
    default_model: zai-org/GLM-5.2
`)
	cfg := Config{ConfigPath: path}
	var buf bytes.Buffer
	runPreflight(&cfg, []resolvedClientConfig{{Name: "claude-code", Route: "anthropic"}}, &buf)
	out := buf.String()
	if !strings.Contains(out, "${TEST_PF_MISSING_KEY}") {
		t.Fatalf("expected placeholder warning, got:\n%s", out)
	}
	if strings.Contains(out, "WARNING: no Baseten credential") {
		t.Fatalf("passthrough-only client should not trigger the credential banner:\n%s", out)
	}
}
