package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestHTTPClientWithNotifyDetailedPreservesStoreErrors(t *testing.T) {
	path := setAuthFile(t, t.TempDir())
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := HTTPClientWithNotifyDetailed(
		context.Background(),
		"p",
		"https://api.baseten.co",
		func(string, error) {},
	)
	if !errors.Is(err, ErrStoreMalformed) {
		t.Fatalf("HTTPClientWithNotifyDetailed error = %v, want ErrStoreMalformed", err)
	}

	_, _, _, err = HTTPClientWithNotify(
		context.Background(),
		"p",
		"https://api.baseten.co",
		func(string, error) {},
	)
	if !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("legacy HTTPClientWithNotify error = %v, want ErrNotSignedIn", err)
	}
}

// refreshRecorder is an httptest server that counts POSTs to the token
// endpoint and returns a fresh access token.
type refreshRecorder struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newRefreshRecorder(t *testing.T) *refreshRecorder {
	t.Helper()
	rr := &refreshRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, r *http.Request) {
		rr.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new-at","refresh_token":"rt-2","token_type":"Bearer","expires_in":3600}`)
	})
	rr.srv = httptest.NewServer(mux)
	t.Cleanup(rr.srv.Close)
	return rr
}

func writeCredentialFile(t *testing.T, remoteURL string) {
	t.Helper()
	dir := t.TempDir()
	path := setAuthFile(t, dir)
	writeAuthFile(t, path, credentialStoreFile{
		Version: 1,
		Current: "p",
		Profiles: map[string]credentialProfile{
			"p": {
				RemoteURL: remoteURL,
				AuthType:  "oauth",
				InsecureOAuthCredential: &storedOAuthCredential{
					AccessToken:  "expired-at",
					RefreshToken: "rt-1",
					Expiry:       time.Now().Add(-time.Hour),
				},
			},
		},
	})
}

// TestHTTPClientRefreshHostSelection verifies token refresh prefers the
// profile's stored remote_url over the caller-provided host (which itself
// is BASETEN_REMOTE_URL, then the default), and falls back to the caller
// host when the profile has no remote_url.
func TestHTTPClientRefreshHostSelection(t *testing.T) {
	cases := []struct {
		name           string
		remoteInFile   bool // profile carries remote_url pointing at profileSrv
		wantProfileHit bool
	}{
		{name: "profile remote_url wins", remoteInFile: true, wantProfileHit: true},
		{name: "no remote_url falls back to caller host", remoteInFile: false, wantProfileHit: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profileSrv := newRefreshRecorder(t)
			callerSrv := newRefreshRecorder(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"auth": r.Header.Get("Authorization")})
			}))
			t.Cleanup(upstream.Close)

			remote := ""
			if tc.remoteInFile {
				remote = profileSrv.srv.URL
			}
			writeCredentialFile(t, remote)

			client, err := HTTPClient(context.Background(), "p", callerSrv.srv.URL)
			if err != nil {
				t.Fatalf("HTTPClient: %v", err)
			}
			resp, err := client.Get(upstream.URL)
			if err != nil {
				t.Fatalf("request via oauth client: %v", err)
			}
			var body struct {
				Auth string `json:"auth"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			resp.Body.Close()
			if body.Auth != "Bearer new-at" {
				t.Fatalf("upstream saw auth %q, want refreshed Bearer new-at", body.Auth)
			}

			profileHits := profileSrv.hits.Load()
			callerHits := callerSrv.hits.Load()
			if tc.wantProfileHit {
				if profileHits != 1 || callerHits != 0 {
					t.Fatalf("refresh hits: profile=%d caller=%d, want 1/0", profileHits, callerHits)
				}
			} else {
				if profileHits != 0 || callerHits != 1 {
					t.Fatalf("refresh hits: profile=%d caller=%d, want 0/1", profileHits, callerHits)
				}
			}
		})
	}
}

func TestAPIHostFromRemote(t *testing.T) {
	cases := []struct {
		remote string
		want   string
	}{
		{"https://app.baseten.co", "https://api.baseten.co"},
		// Custom app hosts, API hosts, and test servers pass through.
		{"https://app.custom.example.com", "https://app.custom.example.com"},
		{"https://api.baseten.co", "https://api.baseten.co"},
		{"http://127.0.0.1:5555", "http://127.0.0.1:5555"},
		{"not a url", "not a url"},
	}
	for _, tc := range cases {
		if got := apiHostFromRemote(tc.remote); got != tc.want {
			t.Errorf("apiHostFromRemote(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

// TestHTTPClientForceRefresh verifies the force-refresh variant performs a
// token refresh round trip even when the cached access token is still
// valid, while the plain variant leaves a valid token alone.
func TestHTTPClientForceRefresh(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "plain keeps valid token"
		if force {
			name = "force refreshes valid token"
		}
		t.Run(name, func(t *testing.T) {
			tokenSrv := newRefreshRecorder(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"auth": r.Header.Get("Authorization")})
			}))
			t.Cleanup(upstream.Close)

			dir := t.TempDir()
			path := setAuthFile(t, dir)
			writeAuthFile(t, path, credentialStoreFile{
				Version: 1,
				Current: "p",
				Profiles: map[string]credentialProfile{
					"p": {
						RemoteURL: tokenSrv.srv.URL,
						AuthType:  "oauth",
						InsecureOAuthCredential: &storedOAuthCredential{
							AccessToken:  "valid-at",
							RefreshToken: "rt-1",
							Expiry:       time.Now().Add(time.Hour),
						},
					},
				},
			})

			build := HTTPClient
			if force {
				build = HTTPClientForceRefresh
			}
			client, err := build(context.Background(), "p", "http://127.0.0.1:1")
			if err != nil {
				t.Fatalf("build client: %v", err)
			}
			resp, err := client.Get(upstream.URL)
			if err != nil {
				t.Fatalf("request via oauth client: %v", err)
			}
			var body struct {
				Auth string `json:"auth"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			resp.Body.Close()

			wantAuth, wantHits := "Bearer valid-at", int64(0)
			if force {
				wantAuth, wantHits = "Bearer new-at", int64(1)
			}
			if body.Auth != wantAuth {
				t.Fatalf("upstream saw auth %q, want %q", body.Auth, wantAuth)
			}
			if hits := tokenSrv.hits.Load(); hits != wantHits {
				t.Fatalf("token endpoint hits = %d, want %d", hits, wantHits)
			}
		})
	}
}

// TestHTTPClientWithNotifyReportsRefreshOutcome verifies the Notify seam
// the gateway's credential-health state is built on: a token-endpoint
// rejection (invalid_grant) notifies with an error RefreshErrorCode
// classifies as a grant failure and the fingerprint of the refresh token
// whose refresh was rejected, and a subsequent successful refresh
// notifies nil with the fingerprint of the token now persisted (the
// rotated one). The returned credFP must fingerprint the stored token the
// client was built from.
func TestHTTPClientWithNotifyReportsRefreshOutcome(t *testing.T) {
	var dead atomic.Bool
	dead.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if dead.Load() {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"new-at","refresh_token":"rt-2","token_type":"Bearer","expires_in":3600}`)
	})
	tokenSrv := httptest.NewServer(mux)
	t.Cleanup(tokenSrv.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(upstream.Close)

	writeCredentialFile(t, tokenSrv.URL) // expired access token: first use must refresh

	var mu sync.Mutex
	var outcomes []string
	notify := func(fp string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			outcomes = append(outcomes, "ok:"+fp)
			return
		}
		outcomes = append(outcomes, "err:"+RefreshErrorCode(err)+":"+fp)
	}
	client, _, credFP, err := HTTPClientWithNotify(context.Background(), "p", tokenSrv.URL, notify)
	if err != nil {
		t.Fatalf("HTTPClientWithNotify: %v", err)
	}
	if credFP != CredFingerprint("rt-1") {
		t.Fatalf("credFP = %q, want fingerprint of the stored token rt-1", credFP)
	}

	if _, err := client.Get(upstream.URL); err == nil {
		t.Fatal("request with dead refresh token unexpectedly succeeded")
	}
	dead.Store(false)
	if _, err := client.Get(upstream.URL); err != nil {
		t.Fatalf("request after token endpoint recovery: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// The failure carries the fingerprint of the token that was rejected
	// (rt-1); the success carries the token now persisted (rt-2), so the
	// consumer can track rotation without re-reading the store.
	want := []string{
		"err:invalid_grant:" + CredFingerprint("rt-1"),
		"ok:" + CredFingerprint("rt-2"),
	}
	if len(outcomes) != len(want) || outcomes[0] != want[0] || outcomes[1] != want[1] {
		t.Fatalf("notify outcomes = %v, want %v", outcomes, want)
	}
}

// TestHTTPClientWithNotifyTick pins the tick contract the gateway's
// background health tick depends on: tick is a no-op while the cached
// access token is valid (zero token-endpoint traffic), performs exactly one
// refresh once the token is expired (reported through notify), and after a
// successful refresh the next tick rides the fresh cached token again.
func TestHTTPClientWithNotifyTick(t *testing.T) {
	var outcomes []string
	notify := func(fp string, err error) {
		if err == nil {
			outcomes = append(outcomes, "ok")
			return
		}
		outcomes = append(outcomes, "err:"+RefreshErrorCode(err))
	}

	t.Run("valid token: tick performs no refresh", func(t *testing.T) {
		tokenSrv := newRefreshRecorder(t)
		dir := t.TempDir()
		path := setAuthFile(t, dir)
		writeAuthFile(t, path, credentialStoreFile{
			Version: 1,
			Current: "p",
			Profiles: map[string]credentialProfile{
				"p": {
					RemoteURL: tokenSrv.srv.URL,
					AuthType:  "oauth",
					InsecureOAuthCredential: &storedOAuthCredential{
						AccessToken:  "valid-at",
						RefreshToken: "rt-1",
						Expiry:       time.Now().Add(time.Hour),
					},
				},
			},
		})
		outcomes = nil
		_, tick, _, err := HTTPClientWithNotify(context.Background(), "p", tokenSrv.srv.URL, notify)
		if err != nil {
			t.Fatalf("HTTPClientWithNotify: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := tick(); err != nil {
				t.Fatalf("tick %d with valid token: %v", i, err)
			}
		}
		if hits := tokenSrv.hits.Load(); hits != 0 {
			t.Fatalf("token endpoint hits = %d, want 0 (valid token must not be force-refreshed)", hits)
		}
		if len(outcomes) != 0 {
			t.Fatalf("notify outcomes = %v, want none", outcomes)
		}
	})

	t.Run("expired token: tick refreshes once", func(t *testing.T) {
		tokenSrv := newRefreshRecorder(t)
		writeCredentialFile(t, tokenSrv.srv.URL) // expired access token
		outcomes = nil
		_, tick, _, err := HTTPClientWithNotify(context.Background(), "p", tokenSrv.srv.URL, notify)
		if err != nil {
			t.Fatalf("HTTPClientWithNotify: %v", err)
		}
		if err := tick(); err != nil {
			t.Fatalf("tick with expired token: %v", err)
		}
		if err := tick(); err != nil {
			t.Fatalf("tick after refresh: %v", err)
		}
		if hits := tokenSrv.hits.Load(); hits != 1 {
			t.Fatalf("token endpoint hits = %d, want exactly 1", hits)
		}
		if len(outcomes) != 1 || outcomes[0] != "ok" {
			t.Fatalf("notify outcomes = %v, want [ok]", outcomes)
		}
	})
}

// TestRefreshErrorCodeClassification pins the transient-vs-dead split:
// only token-endpoint rejections classify as a code; network-level
// failures return "" so health does not flip to refresh_failed on a
// flaky connection.
func TestRefreshErrorCodeClassification(t *testing.T) {
	if got := RefreshErrorCode(fmt.Errorf("dial tcp: connection refused")); got != "" {
		t.Fatalf("network error classified as %q, want empty", got)
	}
	if got := RefreshErrorCode(nil); got != "" {
		t.Fatalf("nil classified as %q, want empty", got)
	}
	re := &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	if got := RefreshErrorCode(fmt.Errorf("wrapped: %w", re)); got != "invalid_grant" {
		t.Fatalf("RetrieveError classified as %q, want invalid_grant", got)
	}
	reNoCode := &oauth2.RetrieveError{Response: &http.Response{StatusCode: 401}}
	if got := RefreshErrorCode(reNoCode); got != "http_401" {
		t.Fatalf("codeless RetrieveError classified as %q, want http_401", got)
	}
}

// TestRefreshRoundTripBounded verifies token-refresh round trips carry a
// deadline: a token endpoint that accepts the connection and never
// answers must fail the tick within refreshHTTPTimeout instead of
// blocking forever (the gateway's background tick is counted in its
// shutdown WaitGroup, so an unbounded refresh would stall SIGTERM on an
// idle gateway indefinitely).
func TestRefreshRoundTripBounded(t *testing.T) {
	stall := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, r *http.Request) {
		<-stall // blackhole: headers accepted, no response ever
	})
	tokenSrv := httptest.NewServer(mux)
	t.Cleanup(func() {
		close(stall)
		tokenSrv.Close()
	})

	oldTimeout := refreshHTTPTimeout
	refreshHTTPTimeout = 150 * time.Millisecond
	t.Cleanup(func() { refreshHTTPTimeout = oldTimeout })

	writeCredentialFile(t, tokenSrv.URL) // expired access token: tick must refresh
	_, tick, _, err := HTTPClientWithNotify(context.Background(), "p", tokenSrv.URL, func(string, error) {})
	if err != nil {
		t.Fatalf("HTTPClientWithNotify: %v", err)
	}
	start := time.Now()
	if err := tick(); err == nil {
		t.Fatal("tick against a stalled token endpoint unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("tick took %v against a stalled endpoint; refresh round trips must be bounded", elapsed)
	}
}

// TestHTTPClientUnboundedTimeout pins the fix for the 30s whole-exchange
// timeout that x/oauth2 NewClient copies from the injected context client
// onto the client it returns. That client relays every upstream request and
// stream, so a non-zero Timeout would cap long non-streaming turns (compaction
// needs 60-90s) at refreshHTTPTimeout and truncate long streams. The bound
// must live only on token-endpoint round trips (covered by
// TestRefreshRoundTripBounded), never on the returned client.
func TestHTTPClientUnboundedTimeout(t *testing.T) {
	tokenSrv := newRefreshRecorder(t)
	writeCredentialFile(t, tokenSrv.srv.URL)

	client, err := HTTPClient(context.Background(), "p", tokenSrv.srv.URL)
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	if client.Timeout != 0 {
		t.Fatalf("returned client Timeout = %v, want 0 (unbounded); refreshHTTPTimeout must bound only token round trips", client.Timeout)
	}

	nClient, _, _, err := HTTPClientWithNotify(context.Background(), "p", tokenSrv.srv.URL, func(string, error) {})
	if err != nil {
		t.Fatalf("HTTPClientWithNotify: %v", err)
	}
	if nClient.Timeout != 0 {
		t.Fatalf("WithNotify client Timeout = %v, want 0 (unbounded)", nClient.Timeout)
	}
}
