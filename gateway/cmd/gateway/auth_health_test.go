package gateway

// Credential-health tests cover a stored OAuth credential whose refresh is
// rejected by the token endpoint.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// deadTokenServer is a token endpoint that can flip between rejecting the
// grant (400 invalid_grant) and issuing a fresh token. expiresIn controls
// the issued token's lifetime: a value at or below oauth2's ~10s expiry
// delta makes every subsequent tick a real refresh, which is how tests
// exercise a death AFTER a successful rotation.
type deadTokenServer struct {
	srv       *httptest.Server
	dead      atomic.Bool
	hits      atomic.Int64
	expiresIn atomic.Int64
}

func newDeadTokenServer(t *testing.T) *deadTokenServer {
	t.Helper()
	ts := &deadTokenServer{}
	ts.dead.Store(true)
	ts.expiresIn.Store(3600)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/users/auth/device/token", func(w http.ResponseWriter, r *http.Request) {
		ts.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if ts.dead.Load() {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh token expired or revoked"}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":"fresh-at","refresh_token":"rt-rotated","token_type":"Bearer","expires_in":%d}`, ts.expiresIn.Load())
	})
	ts.srv = httptest.NewServer(mux)
	t.Cleanup(ts.srv.Close)
	return ts
}

// writeOAuthProfile writes a v0.2.x auth.json at the already-set
// BASETEN_SWITCH_AUTH_FILE with an EXPIRED access token, so the first use forces a
// refresh round trip against remoteURL.
func writeOAuthProfile(t *testing.T, remoteURL, refreshToken string) {
	t.Helper()
	writeOAuthProfileExpiry(t, remoteURL, refreshToken, time.Now().Add(-time.Hour))
}

// writeOAuthProfileExpiry is writeOAuthProfile with the access-token expiry
// under test control: a future expiry yields a VALID cached token that must
// not trigger any refresh traffic.
func writeOAuthProfileExpiry(t *testing.T, remoteURL, refreshToken string, expiry time.Time) {
	t.Helper()
	path := os.Getenv("BASETEN_SWITCH_AUTH_FILE")
	if path == "" {
		t.Fatal("BASETEN_SWITCH_AUTH_FILE not set (call testConfig first)")
	}
	blob := fmt.Sprintf(`{
  "version": 1,
  "current": "p",
  "profiles": {
    "p": {
      "remote_url": %q,
      "auth_type": "oauth",
      "oauth_credential": {
        "access_token": "expired-at",
        "refresh_token": %q,
        "expiry": %q
      }
    }
  }
}`, remoteURL, refreshToken, expiry.UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
}

func adminAuthBlock(t *testing.T, g *Gateway) map[string]any {
	t.Helper()
	resp, err := http.Get(adminURL(g, "/v1/admin/status"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	auth, _ := st["auth"].(map[string]any)
	if auth == nil {
		t.Fatalf("admin status has no auth block: %v", st)
	}
	return auth
}

// TestAuthHealthRefreshFailed verifies refresh rejection end to end on a
// baseten-routed client with no fallback: the harness gets a 502
// naming invalid_grant, the upstream is never reached, and admin status
// keeps signed_in=true (store presence) while health flips to
// refresh_failed with the error recorded. It then simulates recovery by
// letting the token endpoint issue again: the next request succeeds with
// the refreshed Bearer token and health returns to ok.
func TestAuthHealthRefreshFailed(t *testing.T) {
	upstreamHits := atomic.Int64{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			return
		}
		upstreamHits.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer fresh-at" {
			t.Errorf("upstream saw auth %q, want refreshed bearer", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"model":"zai-org/GLM-5.2","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	tokenSrv := newDeadTokenServer(t)
	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	cfg.OAuthProfile = "p"
	writeOAuthProfile(t, tokenSrv.srv.URL, "rt-dead")

	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// Credential present, nothing failed yet: signed in and ok.
	auth := adminAuthBlock(t, g)
	if auth["signed_in"] != true || auth["health"] != "ok" {
		t.Fatalf("initial auth block = %v, want signed_in=true health=ok", auth)
	}

	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"ping"}]}`)
	post := func() (*http.Response, string) {
		req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	resp, respBody := post()
	if resp.StatusCode != 502 {
		t.Fatalf("dead credential: got %d (%s), want 502", resp.StatusCode, respBody)
	}
	if !strings.Contains(respBody, "invalid_grant") {
		t.Fatalf("502 body does not name invalid_grant: %s", respBody)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("upstream was reached %d times with a dead credential", upstreamHits.Load())
	}

	auth = adminAuthBlock(t, g)
	if auth["signed_in"] != true {
		t.Fatalf("signed_in flipped to %v; store presence should keep it true", auth["signed_in"])
	}
	if auth["health"] != "refresh_failed" {
		t.Fatalf("health = %v, want refresh_failed (auth block: %v)", auth["health"], auth)
	}
	lastErr, _ := auth["last_refresh_error"].(string)
	if !strings.Contains(lastErr, "invalid_grant") {
		t.Fatalf("last_refresh_error = %q, want invalid_grant", lastErr)
	}
	if at, _ := auth["last_refresh_error_at"].(string); at == "" {
		t.Fatal("last_refresh_error_at is empty")
	}

	// The dedicated auth status endpoint carries the same health fields.
	arResp, err := http.Get(adminURL(g, "/v1/admin/auth/status"))
	if err != nil {
		t.Fatal(err)
	}
	var ar map[string]any
	if err := json.NewDecoder(arResp.Body).Decode(&ar); err != nil {
		t.Fatal(err)
	}
	arResp.Body.Close()
	if ar["health"] != "refresh_failed" {
		t.Fatalf("/v1/admin/auth/status health = %v, want refresh_failed", ar["health"])
	}

	// Recovery: the token endpoint issues again (e.g. server-side
	// hiccup resolved); the very next request refreshes, reaches the
	// upstream with the fresh bearer, and health returns to ok.
	tokenSrv.dead.Store(false)
	resp, respBody = post()
	if resp.StatusCode != 200 {
		t.Fatalf("after recovery: got %d (%s), want 200", resp.StatusCode, respBody)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", upstreamHits.Load())
	}
	auth = adminAuthBlock(t, g)
	if auth["health"] != "ok" {
		t.Fatalf("health after successful refresh = %v, want ok", auth["health"])
	}
	if at, _ := auth["last_refresh_ok_at"].(string); at == "" {
		t.Fatal("last_refresh_ok_at is empty after successful refresh")
	}
}

// TestAuthHealthClearsOnRelogin verifies re-login detection: with the
// credential marked dead and the token endpoint STILL rejecting, a reload
// (SIGHUP / admin config PUT path -> refreshAuth) clears the dead state
// only when the stored refresh token changed. Same-token reloads keep
// reporting refresh_failed; a new refresh token (fresh 'baseten auth
// login') resets health without waiting for the next successful refresh.
func TestAuthHealthClearsOnRelogin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	tokenSrv := newDeadTokenServer(t)
	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	cfg.OAuthProfile = "p"
	writeOAuthProfile(t, tokenSrv.srv.URL, "rt-dead")

	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if auth := adminAuthBlock(t, g); auth["health"] != "refresh_failed" {
		t.Fatalf("health = %v, want refresh_failed before re-login", auth["health"])
	}

	// Reload with the SAME stored refresh token: still dead.
	g.refreshAuth()
	if auth := adminAuthBlock(t, g); auth["health"] != "refresh_failed" {
		t.Fatalf("health = %v after same-token reload, want refresh_failed", auth["health"])
	}

	// Re-login writes a different refresh token; reload clears the dead
	// state even though no refresh has succeeded yet.
	writeOAuthProfile(t, tokenSrv.srv.URL, "rt-new-login")
	g.refreshAuth()
	if auth := adminAuthBlock(t, g); auth["health"] != "ok" {
		t.Fatalf("health = %v after re-login reload, want ok", auth["health"])
	}
}

// TestAuthDeadCredentialEngagesFallbackRoute verifies the request-path
// impact when a fallback_route is configured: the dead baseten primary
// trips the 30s cooldown and the request is served by the fallback route,
// so the harness sees the fallback provider's answer, not an error.
func TestAuthDeadCredentialEngagesFallbackRoute(t *testing.T) {
	fallbackHits := atomic.Int64{}
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			return
		}
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"PONG"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer openai.Close()
	basetenUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("baseten upstream reached despite dead credential")
	}))
	defer basetenUpstream.Close()

	tokenSrv := newDeadTokenServer(t)
	cfg := testConfig(t, basetenUpstream.URL, basetenUpstream.URL)
	cfg.OpenAIURL = openai.URL
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	cfg.OAuthProfile = "p"
	writeOAuthProfile(t, tokenSrv.srv.URL, "rt-dead")

	rc := resolvedOpenAIBaseten(t, "codex", "baseten")
	rc.DefaultModel = "moonshotai/Kimi-K2.7-Code"
	rc.FallbackRoute = "openai"
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "codex", "/v1/chat/completions"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer harness-native-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(respBody), "PONG") {
		t.Fatalf("fallback did not serve: %d %s", resp.StatusCode, respBody)
	}
	if fallbackHits.Load() != 1 {
		t.Fatalf("fallback hits = %d, want 1", fallbackHits.Load())
	}
	if !g.fallbackActive("codex") {
		t.Fatal("30s fallback cooldown did not trip")
	}
	if auth := adminAuthBlock(t, g); auth["health"] != "refresh_failed" {
		t.Fatalf("health = %v, want refresh_failed", auth["health"])
	}
}
