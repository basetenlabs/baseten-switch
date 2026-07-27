package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setAuthFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "auth.json")
	t.Setenv("BASETEN_SWITCH_AUTH_FILE", path)
	t.Setenv("BASETEN_SWITCH_AUTH_NO_KEYRING", "1")
	return path
}

func writeAuthFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sampleStoredOAuthCredential() *storedOAuthCredential {
	return &storedOAuthCredential{
		AccessToken:  "at-xyz",
		RefreshToken: "rt-abc",
		Expiry:       time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second),
	}
}

// TestLoadAuthFileRoundtrip writes the current auth.json contract with an
// OAuth credential, loads the profile, and verifies the StoredToken fields
// round-trip losslessly. With BASETEN_SWITCH_AUTH_NO_KEYRING=1 Save is a no-op,
// so we also verify it does not error and that a re-Load still reads the file.
func TestLoadAuthFileRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := setAuthFile(t, dir)
	cred := sampleStoredOAuthCredential()
	writeAuthFile(t, path, credentialStoreFile{
		Version: 1,
		Current: "default",
		Profiles: map[string]credentialProfile{
			"default": {
				RemoteURL:               "https://api.baseten.co",
				AuthType:                "oauth",
				InsecureOAuthCredential: cred,
			},
		},
	})

	tok, loc, err := Load("default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tok == nil {
		t.Fatal("Load returned nil token")
	}
	if tok.AccessToken != cred.AccessToken {
		t.Fatalf("access token = %q, want %q", tok.AccessToken, cred.AccessToken)
	}
	if tok.RemoteURL != "https://api.baseten.co" {
		t.Fatalf("remote url = %q, want profile remote_url", tok.RemoteURL)
	}
	if tok.RefreshToken != cred.RefreshToken {
		t.Fatalf("refresh token = %q, want %q", tok.RefreshToken, cred.RefreshToken)
	}
	if !tok.Expiry.Equal(cred.Expiry) {
		t.Fatalf("expiry = %v, want %v", tok.Expiry, cred.Expiry)
	}
	if loc == nil {
		t.Fatal("Load returned nil locator")
	}
	if loc.Service != keyringService {
		t.Fatalf("locator.Service = %q, want %q", loc.Service, keyringService)
	}
	if loc.Account != "default" {
		t.Fatalf("locator.Account = %q, want default", loc.Account)
	}
	if loc.Source != "auth-file" {
		t.Fatalf("locator.Source = %q, want auth-file", loc.Source)
	}

	// Save is keyring-only; with keyring disabled it is a no-op success.
	if err := Save(loc, tok); err != nil {
		t.Fatalf("Save (no-op): %v", err)
	}
	// Re-Load should still read the unchanged file.
	tok2, _, err := Load("default")
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	if tok2.AccessToken != cred.AccessToken {
		t.Fatalf("re-Load access token = %q, want %q", tok2.AccessToken, cred.AccessToken)
	}
}

func TestLoadRejectsRetiredCredentialShape(t *testing.T) {
	path := setAuthFile(t, t.TempDir())
	writeAuthFile(t, path, map[string]any{
		"version": 1,
		"hosts": map[string]any{
			"https://api.baseten.co": map[string]any{
				"active_user": "user@example.com",
				"users": map[string]any{
					"user@example.com": map[string]any{
						"auth_type": "oauth",
						"oauth_credential": map[string]any{
							"access_token":  "retired-at",
							"refresh_token": "retired-rt",
						},
					},
				},
			},
		},
	})
	tok, loc, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tok != nil || loc != nil {
		t.Fatalf("retired credential shape loaded token=%v locator=%+v", tok != nil, loc)
	}
}

// TestLoadMissingReturnsNil verifies that a missing profile yields
// (nil, nil, nil) with no error.
func TestLoadMissingReturnsNil(t *testing.T) {
	dir := t.TempDir()
	setAuthFile(t, dir)
	tok, loc, err := Load("never-saved")
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if tok != nil {
		t.Fatalf("Load missing returned token %+v, want nil", tok)
	}
	if loc != nil {
		t.Fatalf("Load missing returned locator %+v, want nil", loc)
	}
}

// TestLoadEmptyProfileResolvesCurrent verifies that Load("") falls back to the
// auth.json `current` field when no profile name is supplied, mirroring how the
// gateway now invokes Load when BASETEN_SWITCH_OAUTH_PROFILE is unset.
func TestLoadEmptyProfileResolvesCurrent(t *testing.T) {
	dir := t.TempDir()
	path := setAuthFile(t, dir)
	cred := sampleStoredOAuthCredential()
	profName := "user@example.com"
	writeAuthFile(t, path, credentialStoreFile{
		Version: 1,
		Current: profName,
		Profiles: map[string]credentialProfile{
			profName: {
				RemoteURL:               "https://api.baseten.co",
				AuthType:                "oauth",
				InsecureOAuthCredential: cred,
			},
		},
	})

	tok, loc, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if tok == nil {
		t.Fatal("Load(\"\") returned nil token; expected resolution via `current`")
	}
	if tok.AccessToken != cred.AccessToken {
		t.Fatalf("access token = %q, want %q", tok.AccessToken, cred.AccessToken)
	}
	if loc == nil || loc.Account != profName {
		t.Fatalf("locator account = %q, want %q", locOrZero(loc), profName)
	}
	if loc == nil || loc.Source != "auth-file" {
		t.Fatalf("locator source = %v, want auth-file", loc)
	}
}

func locOrZero(loc *SaveLocator) string {
	if loc == nil {
		return "<nil>"
	}
	return loc.Account
}

// TestLoadAuthFileMode0600 verifies a current auth.json written with mode 0600
// is readable by Load.
func TestLoadAuthFileMode0600(t *testing.T) {
	dir := t.TempDir()
	path := setAuthFile(t, dir)
	cred := sampleStoredOAuthCredential()
	writeAuthFile(t, path, credentialStoreFile{
		Version: 1,
		Current: "default",
		Profiles: map[string]credentialProfile{
			"default": {
				AuthType:                "oauth",
				InsecureOAuthCredential: cred,
			},
		},
	})
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("file mode = %o, want 0600", mode)
	}
	tok, _, err := Load("default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tok == nil || tok.AccessToken != cred.AccessToken {
		t.Fatalf("Load did not read 0600 file: %+v", tok)
	}
}

// TestLoadAPIKeyProfile verifies that api_key-type profiles surface a typed
// *APIKeyProfileError (matched by errors.Is(_, ErrAPIKeyProfile)) instead of
// reading as "not signed in", with the key available when readable.
func TestLoadAPIKeyProfile(t *testing.T) {
	cases := []struct {
		name        string
		profileArg  string // argument to Load
		current     string
		profName    string
		insecureKey string
		wantKey     string
	}{
		{
			name:        "explicit profile with plaintext key",
			profileArg:  "svc",
			profName:    "svc",
			insecureKey: "sk-live-123",
			wantKey:     "sk-live-123",
		},
		{
			name:        "empty profile resolves current pointer",
			profileArg:  "",
			current:     "user@example.com",
			profName:    "user@example.com",
			insecureKey: "sk-live-456",
			wantKey:     "sk-live-456",
		},
		{
			name:       "key unreadable (no plaintext, keyring disabled)",
			profileArg: "svc",
			profName:   "svc",
			wantKey:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := setAuthFile(t, dir)
			writeAuthFile(t, path, credentialStoreFile{
				Version: 1,
				Current: tc.current,
				Profiles: map[string]credentialProfile{
					tc.profName: {
						RemoteURL:      "https://api.baseten.co",
						AuthType:       "api_key",
						InsecureAPIKey: tc.insecureKey,
					},
				},
			})
			tok, loc, err := Load(tc.profileArg)
			if tok != nil || loc != nil {
				t.Fatalf("api_key profile should not yield a token/locator, got %+v / %+v", tok, loc)
			}
			if err == nil {
				t.Fatal("Load on api_key profile returned nil error; want ErrAPIKeyProfile")
			}
			if !errors.Is(err, ErrAPIKeyProfile) {
				t.Fatalf("errors.Is(err, ErrAPIKeyProfile) = false for %v", err)
			}
			var ak *APIKeyProfileError
			if !errors.As(err, &ak) {
				t.Fatalf("error %v is not *APIKeyProfileError", err)
			}
			if ak.Profile != tc.profName {
				t.Fatalf("profile = %q, want %q", ak.Profile, tc.profName)
			}
			if ak.Key != tc.wantKey {
				t.Fatalf("key = %q, want %q", ak.Key, tc.wantKey)
			}
		})
	}
}

// TestLoadOAuthProfileNextToAPIKey verifies an OAuth profile in the same file
// loads normally when another profile uses API-key authentication.
func TestLoadOAuthProfileNextToAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := setAuthFile(t, dir)
	cred := sampleStoredOAuthCredential()
	writeAuthFile(t, path, credentialStoreFile{
		Version: 1,
		Current: "svc",
		Profiles: map[string]credentialProfile{
			"svc": {AuthType: "api_key", InsecureAPIKey: "sk-1"},
			"me": {
				RemoteURL:               "https://api.baseten.co/",
				AuthType:                "oauth",
				InsecureOAuthCredential: cred,
			},
		},
	})
	tok, loc, err := Load("me")
	if err != nil {
		t.Fatalf("Load(me): %v", err)
	}
	if tok == nil || tok.AccessToken != cred.AccessToken {
		t.Fatalf("oauth profile did not load: %+v", tok)
	}
	if tok.RemoteURL != "https://api.baseten.co" {
		t.Fatalf("remote url = %q, want trailing slash trimmed", tok.RemoteURL)
	}
	if loc == nil || loc.Account != "me" {
		t.Fatalf("locator = %+v, want account me", loc)
	}
}

func TestExtractAPIKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"sk-raw-key", "sk-raw-key"},
		{"  sk-raw-key\n", "sk-raw-key"},
		{`{"api_key":"sk-wrapped"}`, "sk-wrapped"},
		{`{"access_token":"at","refresh_token":"rt"}`, ""}, // oauth JSON is not a key
		{"", ""},
	}
	for _, tc := range cases {
		if got := extractAPIKey(tc.in); got != tc.want {
			t.Errorf("extractAPIKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSaveNilLocatorReturnsSentinel verifies Save with a nil locator surfaces
// the errNoLocator sentinel rather than mutating the keyring or file.
func TestSaveNilLocatorReturnsSentinel(t *testing.T) {
	dir := t.TempDir()
	path := setAuthFile(t, dir)
	err := Save(nil, &StoredToken{AccessToken: "x"})
	if err == nil {
		t.Fatal("Save(nil, ...) returned nil error, want errNoLocator")
	}
	if err != errNoLocator {
		t.Fatalf("Save(nil, ...) error = %v, want %v", err, errNoLocator)
	}
	// The auth.json file should not have been created.
	if _, err := os.Stat(path); err == nil {
		t.Fatal("Save created auth.json; it must never write to disk")
	}
}
