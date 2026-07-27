package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	keyringService = "baseten-profile"
	authFileName   = "auth.json"
)

var (
	ErrNotSignedIn     = errors.New("baseten-switch: not signed in. Run 'baseten auth login'")
	errNoLocator       = errors.New("baseten-switch: no save locator (cannot persist credential)")
	errKeyringNotFound = errors.New("keyring: not found")

	// ErrAPIKeyProfile is the sentinel matched by errors.Is when the
	// resolved profile authenticates with an API key rather than OAuth.
	// The concrete error is *APIKeyProfileError, which carries the
	// resolved profile name and (when readable) the key itself.
	ErrAPIKeyProfile = errors.New("baseten-switch: profile uses api_key auth (not OAuth)")
)

// APIKeyProfileError reports that the resolved profile's auth_type is
// "api_key". It is a distinct signed-in state, not "not signed in":
// callers such as whoami and setup can report "signed in with API key"
// and, when Key is non-empty, use the key directly.
type APIKeyProfileError struct {
	Profile string // resolved profile name
	Key     string // the API key, when it could be read; may be empty
}

func (e *APIKeyProfileError) Error() string {
	return fmt.Sprintf("baseten-switch: profile %q uses api_key auth (not OAuth)", e.Profile)
}

// Is makes errors.Is(err, ErrAPIKeyProfile) match.
func (e *APIKeyProfileError) Is(target error) bool { return target == ErrAPIKeyProfile }

// StoredToken is the in-memory representation of an OAuth credential loaded
// from the baseten CLI's auth.json and/or system keyring. TokenType is empty
// when the stored credential does not carry it; oauth2 clients treat empty
// as Bearer. RemoteURL is the profile's stored remote_url from auth.json
// (empty for keyring-only loads); token refresh prefers it over
// BASETEN_REMOTE_URL / the default host.
type StoredToken struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
	TokenType    string
	RemoteURL    string
}

// SaveLocator identifies the keyring entry a credential was loaded from, so a
// refreshed token can be persisted back to the same place. Source is for
// diagnostics only.
type SaveLocator struct {
	Service string // keyring service, "baseten-profile"
	Account string // keyring account, the profile name
	Source  string // one of "keyring" or "auth-file"
}

// storedOAuthCredential is the JSON shape used both as the keyring value and
// as the oauth_credential field in auth.json. It mirrors the baseten CLI's
// OAuthCredential struct (MIT).
type storedOAuthCredential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// credentialStoreFile is the current baseten CLI auth.json contract.
type credentialStoreFile struct {
	Version  int                          `json:"version"`
	Current  string                       `json:"current,omitempty"`
	Profiles map[string]credentialProfile `json:"profiles"`
}

type credentialProfile struct {
	RemoteURL               string                 `json:"remote_url"`
	AuthType                string                 `json:"auth_type"`
	InsecureOAuthCredential *storedOAuthCredential `json:"oauth_credential,omitempty"`
	InsecureAPIKey          string                 `json:"api_key,omitempty"`
}

// ProfilePath returns the path to the baseten CLI's auth.json:
// $BASETEN_SWITCH_AUTH_FILE (test override, full path) > $BASETEN_CONFIG_DIR/auth.json >
// os.UserConfigDir()/baseten/auth.json.
func ProfilePath() string {
	if v := os.Getenv("BASETEN_SWITCH_AUTH_FILE"); v != "" {
		return v
	}
	dir := os.Getenv("BASETEN_CONFIG_DIR")
	if dir == "" {
		if d, err := os.UserConfigDir(); err == nil {
			dir = filepath.Join(d, "baseten")
		} else {
			dir = filepath.Join(os.TempDir(), "baseten")
		}
	}
	return filepath.Join(dir, authFileName)
}

func keyringDisabled() bool { return os.Getenv("BASETEN_SWITCH_AUTH_NO_KEYRING") != "" }

func keyringGet(service, account string) (string, error) {
	if keyringDisabled() {
		return "", errKeyringNotFound
	}
	return keyring.Get(service, account)
}

func keyringSet(service, account, value string) error {
	if keyringDisabled() {
		return nil
	}
	return keyring.Set(service, account, value)
}

func keyringDelete(service, account string) error {
	if keyringDisabled() {
		return nil
	}
	return keyring.Delete(service, account)
}

func storedCredentialToToken(s *storedOAuthCredential) *StoredToken {
	if s == nil {
		return nil
	}
	return &StoredToken{
		AccessToken:  s.AccessToken,
		RefreshToken: s.RefreshToken,
		Expiry:       s.Expiry,
	}
}

// Load resolves an OAuth credential for the given profile name from the
// current baseten CLI auth.json and keyring contract.
// Returns (nil, nil, nil) when nothing is found. When the resolved profile's
// auth_type is "api_key" it returns (nil, nil, *APIKeyProfileError) so
// callers can distinguish "signed in with API key" from "not signed in".
func Load(profile string) (*StoredToken, *SaveLocator, error) {
	path := ProfilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		// File unreadable or missing: an explicitly named profile can still
		// resolve directly from the current keyring service.
		if tok, loc := loadKeyringCredential(profile); tok != nil {
			return tok, loc, nil
		}
		return nil, nil, nil
	}
	if tok, loc, err, ok := loadCredentialProfile(data, profile); ok {
		return tok, loc, err
	}
	return nil, nil, nil
}

// loadKeyringCredential is used when auth.json is unreadable: look up the
// current keyring entry directly by profile name.
func loadKeyringCredential(profile string) (*StoredToken, *SaveLocator) {
	if profile == "" {
		return nil, nil
	}
	secret, err := keyringGet(keyringService, profile)
	if err != nil {
		return nil, nil
	}
	var s storedOAuthCredential
	if json.Unmarshal([]byte(secret), &s) != nil || s.AccessToken == "" {
		return nil, nil
	}
	return storedCredentialToToken(&s), &SaveLocator{
		Service: keyringService,
		Account: profile,
		Source:  "keyring",
	}
}

// loadCredentialProfile resolves a credential from the current auth.json
// shape. The final bool reports whether the file resolved to a profile, with
// either a token or a typed error.
func loadCredentialProfile(data []byte, profile string) (*StoredToken, *SaveLocator, error, bool) {
	var af credentialStoreFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, nil, nil, false
	}
	if af.Profiles == nil {
		return nil, nil, nil, false
	}
	name := profile
	if name == "" {
		name = af.Current
	}
	if name == "" {
		return nil, nil, nil, false
	}
	p, ok := af.Profiles[name]
	if !ok {
		return nil, nil, nil, false
	}
	// api_key-type profile: a distinct signed-in state, not an OAuth
	// credential. Surface the key when it is readable (plaintext
	// auth.json first, then the keyring entry).
	if p.AuthType == "api_key" {
		key := p.InsecureAPIKey
		if key == "" {
			if v, err := keyringGet(keyringService, name); err == nil {
				key = extractAPIKey(v)
			}
		}
		return nil, nil, &APIKeyProfileError{Profile: name, Key: key}, true
	}
	remote := strings.TrimRight(p.RemoteURL, "/")
	// Keyring first.
	if v, err := keyringGet(keyringService, name); err == nil {
		var s storedOAuthCredential
		if json.Unmarshal([]byte(v), &s) == nil && s.AccessToken != "" {
			tok := storedCredentialToToken(&s)
			tok.RemoteURL = remote
			return tok, &SaveLocator{
				Service: keyringService,
				Account: name,
				Source:  "keyring",
			}, nil, true
		}
	}
	// Plaintext fallback in auth.json.
	if p.InsecureOAuthCredential != nil && p.InsecureOAuthCredential.AccessToken != "" {
		tok := storedCredentialToToken(p.InsecureOAuthCredential)
		tok.RemoteURL = remote
		return tok, &SaveLocator{
			Service: keyringService,
			Account: name,
			Source:  "auth-file",
		}, nil, true
	}
	return nil, nil, nil, false
}

// extractAPIKey interprets a keyring value stored for an api_key-type
// profile: either the raw key string or a small JSON wrapper
// {"api_key": "..."}. OAuth-credential JSON (or any other JSON without an
// api_key field) yields "".
func extractAPIKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "{") {
		var w struct {
			APIKey string `json:"api_key"`
		}
		if json.Unmarshal([]byte(v), &w) == nil {
			return w.APIKey
		}
		return ""
	}
	return v
}

// Save writes a refreshed token back to the keyring entry identified by loc.
// It never writes to auth.json. Returns errNoLocator when loc is nil.
func Save(loc *SaveLocator, tok *StoredToken) error {
	if loc == nil {
		return errNoLocator
	}
	if tok == nil {
		return errors.New("baseten-switch: cannot save nil token")
	}
	s := storedOAuthCredential{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
	}
	enc, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return keyringSet(loc.Service, loc.Account, string(enc))
}

// Delete performs a best-effort keyring.Delete at the locator. No-op when loc
// is nil and never touches auth.json.
func Delete(loc *SaveLocator) error {
	if loc == nil {
		return nil
	}
	_ = keyringDelete(loc.Service, loc.Account)
	return nil
}
