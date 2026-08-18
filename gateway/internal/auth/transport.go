package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// refreshHTTPTimeout bounds every token-endpoint round trip. A refresh is
// a small POST (hundreds of milliseconds in practice); without a bound a
// blackholed endpoint blocks Token() indefinitely, and the gateway's
// background tick would carry that stall into Shutdown's wg.Wait. Applies
// only to the refresh transport, never to the returned client's requests
// (streams must run unbounded). Package var so tests can shrink it.
var refreshHTTPTimeout = 30 * time.Second

// CredFingerprint is a non-reversible identity for a refresh token, used
// only to detect that a credential changed (re-login, rotation). Never log
// or expose it alongside anything that could aid brute force; it is a
// truncated SHA-256, kept in memory only.
func CredFingerprint(refreshToken string) string {
	if refreshToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(refreshToken))
	return hex.EncodeToString(sum[:8])
}

// apiHostFromRemote translates a profile's stored remote_url (the app URL
// the baseten CLI records, e.g. https://app.baseten.co) into the management
// API base URL that serves the OAuth token endpoints. The public production
// app host maps to the public production API host. Any other value, including
// an API host or test server, passes through unchanged.
func apiHostFromRemote(remote string) string {
	if remote == "https://app.baseten.co" {
		return "https://api.baseten.co"
	}
	return remote
}

// CachingTokenSource wraps an oauth2.TokenSource and persists refreshed tokens
// back to the keyring entry identified by Locator. When Notify is non-nil it
// is invoked after every underlying Token() call: Notify(fp, nil) on success,
// Notify(fp, err) on failure. fp is the CredFingerprint of the refresh token
// the outcome belongs to: on failure the token whose refresh was rejected, on
// success the token now persisted (rotation moves the lineage without a
// client rebuild, and the consumer must track which credential an outcome
// describes). Because this source sits under oauth2.ReuseTokenSource, Token()
// only runs when the cached access token is expired, so Notify reports
// exactly the refresh round trips against the token endpoint (the operation
// that fails with invalid_grant when the refresh token is dead). Notify must
// not call back into this source.
type CachingTokenSource struct {
	Base    oauth2.TokenSource
	Locator *SaveLocator
	Notify  func(fp string, err error)
	Mu      sync.Mutex
	Current *StoredToken
}

func (c *CachingTokenSource) Token() (*oauth2.Token, error) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	tok, err := c.Base.Token()
	if err != nil {
		if c.Notify != nil {
			fp := ""
			if c.Current != nil {
				fp = CredFingerprint(c.Current.RefreshToken)
			}
			c.Notify(fp, err)
		}
		return nil, err
	}
	next := &StoredToken{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
		TokenType:    tok.TokenType,
	}
	if c.Notify != nil {
		c.Notify(CredFingerprint(next.RefreshToken), nil)
	}
	if c.Current == nil || next.AccessToken != c.Current.AccessToken || !next.Expiry.Equal(c.Current.Expiry) {
		c.Current = next
		_ = Save(c.Locator, next)
	}
	return tok, nil
}

// HTTPClient builds an oauth2.Client for the stored Baseten profile. Returns
// ErrNotSignedIn when no credential is found and *APIKeyProfileError when
// the profile authenticates with an API key instead of OAuth.
func HTTPClient(ctx context.Context, profile string, host string) (*http.Client, error) {
	client, _, _, err := httpClient(ctx, profile, host, false, nil)
	return client, err
}

// HTTPClientForceRefresh is HTTPClient with the cached access token
// treated as already expired, forcing a refresh round trip against the
// real token endpoint on first use. Used by health checks (whoami
// --refresh, scripts/check.sh) to exercise the refresh path
// deterministically instead of only when the cached token happens to be
// near expiry. The refreshed token is persisted as usual.
func HTTPClientForceRefresh(ctx context.Context, profile string, host string) (*http.Client, error) {
	client, _, _, err := httpClient(ctx, profile, host, true, nil)
	return client, err
}

// HTTPClientWithNotify is HTTPClient with a refresh-outcome callback:
// notify(fp, nil) after each successful token refresh, notify(fp, err)
// after each failed one (see CachingTokenSource.Notify for what fp
// identifies). The gateway uses this to keep a truthful credential-health
// state ("signed in" vs "credential present but refresh returns
// invalid_grant") without extra network traffic: the signal rides the
// refreshes that already happen. notify runs on request goroutines and
// must be fast and non-reentrant.
//
// credFP is the CredFingerprint of the stored refresh token the client was
// built from: the same store read that produced the client, so the caller
// never has to re-read the store (a login landing between two reads would
// make a separately computed fingerprint describe a credential this client
// was never built from).
//
// The returned tick performs one token acquisition through the same
// oauth2.ReuseTokenSource the client's requests use: a no-op while the
// cached access token is valid, a real refresh round trip (reported through
// notify) only when it is near expiry. A caller with no request traffic can
// tick periodically to keep the credential-health signal current without
// forcing token rotation. tick blocks on the round trip (bounded by
// refreshHTTPTimeout); never call it while holding locks that notify also
// takes.
func HTTPClientWithNotify(ctx context.Context, profile string, host string, notify func(fp string, err error)) (client *http.Client, tick func() error, credFP string, err error) {
	return httpClient(ctx, profile, host, false, notify)
}

// HTTPClientWithNotifyDetailed is the gateway reload variant of
// HTTPClientWithNotify. It preserves StoreLoadError so the gateway can keep a
// previously loaded credential through a transient auth.json read or parse
// failure. The legacy client builders continue to preserve their historical
// signed-out behavior for the same failures.
func HTTPClientWithNotifyDetailed(ctx context.Context, profile string, host string, notify func(fp string, err error)) (client *http.Client, tick func() error, credFP string, err error) {
	return httpClientWithLoader(ctx, profile, host, false, notify, LoadDetailed)
}

// RefreshErrorCode classifies a token-refresh error: it returns RFC 6749's
// error code (e.g. "invalid_grant") when err came from the token endpoint
// rejecting the grant, "http_<status>" for a token-endpoint response with
// no parseable code, and "" for everything else (network errors, timeouts:
// transient conditions that do not indicate a dead credential).
func RefreshErrorCode(err error) string {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return ""
	}
	if re.ErrorCode != "" {
		return re.ErrorCode
	}
	if re.Response != nil {
		return fmt.Sprintf("http_%d", re.Response.StatusCode)
	}
	return "token_endpoint_error"
}

func httpClient(ctx context.Context, profile string, host string, forceRefresh bool, notify func(fp string, err error)) (*http.Client, func() error, string, error) {
	return httpClientWithLoader(ctx, profile, host, forceRefresh, notify, Load)
}

func httpClientWithLoader(
	ctx context.Context,
	profile string,
	host string,
	forceRefresh bool,
	notify func(fp string, err error),
	load func(string) (*StoredToken, *SaveLocator, error),
) (*http.Client, func() error, string, error) {
	stored, loc, err := load(profile)
	if err != nil {
		return nil, nil, "", err
	}
	if stored == nil {
		return nil, nil, "", ErrNotSignedIn
	}
	initial := &oauth2.Token{
		AccessToken:  stored.AccessToken,
		RefreshToken: stored.RefreshToken,
		Expiry:       stored.Expiry,
		TokenType:    stored.TokenType,
	}
	if forceRefresh {
		initial.Expiry = time.Now().Add(-time.Minute)
	}
	// Token refresh targets the profile's stored remote_url when present;
	// the caller-provided host (BASETEN_REMOTE_URL, then the default) is
	// only the fallback. This keeps refresh pointed at the host that
	// minted the credential rather than whatever the process env says.
	// The stored value is the app URL (https://app.baseten.co), which the
	// token endpoints do not live on; translate it to the API host.
	refreshHost := host
	if stored.RemoteURL != "" {
		refreshHost = apiHostFromRemote(stored.RemoteURL)
	}
	cfg := OAuthConfig(refreshHost)
	// Bound the refresh round trips: oauth2 performs them with the
	// context's HTTP client, which defaults to http.DefaultClient (no
	// timeout). Injecting a client carrying only a Timeout bounds every
	// token-endpoint round trip (the TokenSource below reads this same
	// context client). It does NOT bound the client returned by NewClient
	// on its own: see the client.Timeout = 0 reset below.
	if ctx.Value(oauth2.HTTPClient) == nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: refreshHTTPTimeout})
	}
	base := cfg.TokenSource(ctx, initial)
	caching := &CachingTokenSource{Base: base, Locator: loc, Notify: notify, Current: stored}
	reuse := oauth2.ReuseTokenSource(initial, caching)
	tick := func() error {
		_, err := reuse.Token()
		return err
	}
	client := oauth2.NewClient(ctx, reuse)
	// x/oauth2 NewClient copies the context client's Timeout onto the
	// client it returns (oauth2.go NewClient: Timeout: cc.Timeout). Left
	// as-is that would impose refreshHTTPTimeout as a hard whole-exchange
	// deadline (http.Client.Timeout covers headers AND body) on every
	// upstream request and stream, killing long non-streaming turns
	// (compaction needs 60-90s) at ~30s and truncating long streams. The
	// bound must apply only to token-endpoint round trips, which stay
	// bounded because the TokenSource uses the injected context client
	// above. Zero it here so relay/stream traffic runs unbounded.
	client.Timeout = 0
	return client, tick, CredFingerprint(stored.RefreshToken), nil
}
