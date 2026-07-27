package gateway

// Background auth-tick tests: the periodic credential tick keeps health
// truthful on an IDLE gateway (no harness traffic) and self-heals after a
// re-login without SIGHUP (the credential-refresh contract, "Gateway
// self-heal"). Intervals are shrunk via the package vars; the fake token
// endpoint and temp store come from auth_health_test.go.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"

	"golang.org/x/oauth2"
	"time"
)

// shrinkAuthTick shrinks the tick cadences for one test. Must run before
// start(t, g): the tick goroutine reads the vars when arming its timer.
func shrinkAuthTick(t *testing.T, normal, dead time.Duration) {
	t.Helper()
	oldNormal, oldDead := authTickInterval, authTickDeadInterval
	authTickInterval, authTickDeadInterval = normal, dead
	t.Cleanup(func() { authTickInterval, authTickDeadInterval = oldNormal, oldDead })
}

// waitAuthHealth polls /v1/admin/status (which performs no token traffic)
// until the auth block reports the wanted health.
func waitAuthHealth(t *testing.T, g *Gateway, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if auth := adminAuthBlock(t, g); auth["health"] == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("auth health did not reach %q within 5s (last: %v)",
		want, adminAuthBlock(t, g)["health"])
}

// TestAuthTickFlagsDeadCredentialWhileIdle verifies the tick alone drives
// health to refresh_failed: no client request is ever made, the expired
// cached access token turns the tick's token acquisition into a real
// refresh, and the invalid_grant outcome lands in noteAuthRefresh. Once
// dead, further ticks must not retry the refresh (store re-reads only), so
// token-endpoint traffic stops growing.
func TestAuthTickFlagsDeadCredentialWhileIdle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream reached on an idle gateway")
	}))
	defer upstream.Close()

	tokenSrv := newDeadTokenServer(t)
	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	cfg.OAuthProfile = "p"
	writeOAuthProfile(t, tokenSrv.srv.URL, "rt-dead")
	shrinkAuthTick(t, 20*time.Millisecond, 20*time.Millisecond)

	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	waitAuthHealth(t, g, "refresh_failed")
	auth := adminAuthBlock(t, g)
	if auth["signed_in"] != true {
		t.Fatalf("signed_in = %v, want true (store presence)", auth["signed_in"])
	}
	if lastErr, _ := auth["last_refresh_error"].(string); !strings.Contains(lastErr, "invalid_grant") {
		t.Fatalf("last_refresh_error = %q, want invalid_grant", lastErr)
	}
	// One failing refresh attempt reaches the endpoint once or twice
	// (x/oauth2 auth-style autodetect retries a rejected request with the
	// other client-auth style), but never more: dead-cadence ticks only
	// read the store. Give the loop many dead periods and require zero
	// growth.
	hitsAtDead := tokenSrv.hits.Load()
	if hitsAtDead < 1 || hitsAtDead > 2 {
		t.Fatalf("token endpoint hits at dead = %d, want 1 or 2 (single refresh attempt)", hitsAtDead)
	}
	time.Sleep(200 * time.Millisecond)
	if hits := tokenSrv.hits.Load(); hits != hitsAtDead {
		t.Fatalf("token endpoint hits grew %d -> %d after dead; dead ticks must not replay the refresh", hitsAtDead, hits)
	}
}

// TestAuthTickSelfHealsAfterRelogin verifies recovery with no SIGHUP and no
// admin call: the tick marks the credential dead, the user re-logs-in
// (simulated by writing a new refresh token to the store and letting the
// token endpoint issue again), and within a few dead-cadence ticks the
// gateway rebuilds its oauth client from the store, health returns to ok,
// and a real request round trip succeeds with the refreshed bearer.
func TestAuthTickSelfHealsAfterRelogin(t *testing.T) {
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
	shrinkAuthTick(t, 20*time.Millisecond, 20*time.Millisecond)

	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	waitAuthHealth(t, g, "refresh_failed")

	// Re-login: the token endpoint issues again and the store now holds a
	// DIFFERENT refresh token. Only the tick's store watch can notice; the
	// test never calls refreshAuth or sends SIGHUP.
	tokenSrv.dead.Store(false)
	writeOAuthProfile(t, tokenSrv.srv.URL, "rt-new-login")

	waitAuthHealth(t, g, "ok")

	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(respBody), "PONG") {
		t.Fatalf("request after self-heal: %d %s", resp.StatusCode, respBody)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", upstreamHits.Load())
	}
}

// TestAuthTickKeepsValidTokenUntouched verifies the tick never forces
// rotation: with a VALID cached access token, many tick periods pass with
// zero token-endpoint traffic and health stays ok.
func TestAuthTickKeepsValidTokenUntouched(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	tokenSrv := newDeadTokenServer(t)
	tokenSrv.dead.Store(false)
	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	cfg.OAuthProfile = "p"
	writeOAuthProfileExpiry(t, tokenSrv.srv.URL, "rt-live", time.Now().Add(time.Hour))
	shrinkAuthTick(t, 20*time.Millisecond, 20*time.Millisecond)

	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// Give the loop many periods; "nothing happens" has no event to wait
	// on, so a bounded elapsed window is the assertion surface.
	time.Sleep(400 * time.Millisecond)
	if hits := tokenSrv.hits.Load(); hits != 0 {
		t.Fatalf("token endpoint hits = %d, want 0 (tick must not force-refresh a valid token)", hits)
	}
	if auth := adminAuthBlock(t, g); auth["health"] != "ok" {
		t.Fatalf("health = %v, want ok", auth["health"])
	}
}

// TestAuthTickStopsOnShutdown verifies shutdown terminates the tick
// goroutine even mid-wait on the full-length production interval: cancel
// must interrupt the timer wait, not race it.
func TestAuthTickStopsOnShutdown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, upstream.URL)
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)

	stop()
	select {
	case <-g.authTickDone:
	case <-time.After(2 * time.Second):
		t.Fatal("auth tick goroutine did not exit on shutdown")
	}
}

// TestNoteAuthRefreshLineageGuards pins the two attributions that keep
// the dead state truthful across client swaps and rotation:
//   - generation: a refresh in flight across a refreshAuth swap completes
//     against the OLD client; its outcome (failure OR success) must not
//     touch the new lineage's state. Without the guard, a stale
//     invalid_grant landing just after 'baseten-switch auth login' marks the
//     NEW fingerprint dead and permanently disarms the store-watch
//     self-heal (store fp == deadFP).
//   - fingerprint: a successful refresh advances authCredFP to the token
//     the store now holds (rotation), so a later death is recorded
//     against the token that actually died, not the boot-time one.
func TestNoteAuthRefreshLineageGuards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	tokenSrv := newDeadTokenServer(t)
	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	cfg.OAuthProfile = "p"
	writeOAuthProfileExpiry(t, tokenSrv.srv.URL, "rt-live", time.Now().Add(time.Hour))

	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()

	g.authMu.Lock()
	gen, curFP := g.authGen, g.authCredFP
	g.authMu.Unlock()
	if curFP != auth.CredFingerprint("rt-live") {
		t.Fatalf("authCredFP = %q, want fingerprint of the stored token", curFP)
	}
	deadErr := fmt.Errorf("refresh: %w", &oauth2.RetrieveError{ErrorCode: "invalid_grant"})

	// Stale-generation failure: dropped entirely, not even last_error.
	g.noteAuthRefresh(gen-1, curFP, deadErr)
	if h := g.authHealth(); h.Health != "ok" || h.LastError != "" {
		t.Fatalf("stale-generation failure changed health to %q (err %q), want untouched ok", h.Health, h.LastError)
	}

	// Current-generation failure marks dead against the failing token.
	g.noteAuthRefresh(gen, curFP, deadErr)
	if h := g.authHealth(); h.Health != "refresh_failed" {
		t.Fatalf("health = %q after current-generation failure, want refresh_failed", h.Health)
	}

	// Stale-generation success must not heal the new lineage.
	g.noteAuthRefresh(gen-1, auth.CredFingerprint("rt-old-lineage"), nil)
	if h := g.authHealth(); h.Health != "refresh_failed" {
		t.Fatalf("stale-generation success healed health to %q, want refresh_failed", h.Health)
	}

	// Current-generation success heals and advances the lineage
	// fingerprint to the rotated token.
	rotFP := auth.CredFingerprint("rt-rotated")
	g.noteAuthRefresh(gen, rotFP, nil)
	if h := g.authHealth(); h.Health != "ok" {
		t.Fatalf("health = %q after current-generation success, want ok", h.Health)
	}
	g.authMu.Lock()
	credFP := g.authCredFP
	g.authMu.Unlock()
	if credFP != rotFP {
		t.Fatalf("authCredFP = %q after rotation, want the rotated token's fingerprint", credFP)
	}

	// A death after rotation records the rotated fingerprint.
	g.noteAuthRefresh(gen, rotFP, deadErr)
	g.authMu.Lock()
	deadFP := g.authDeadFP
	g.authMu.Unlock()
	if deadFP != rotFP {
		t.Fatalf("authDeadFP = %q, want the rotated token's fingerprint", deadFP)
	}
}

// TestAuthRotatedDeathDoesNotFlapHealth verifies rotation-aware death
// attribution end to end: a successful refresh rotates the store (the
// fake endpoint issues rt-rotated), then the credential dies. The store
// now holds exactly the token that died, so the dead-cadence store watch
// must NOT read it as a re-login: with a stale boot-time fingerprint the
// gateway would loop dead -> spurious "re-login detected" heal -> false
// health=ok window -> dead, replaying the refresh each cycle.
func TestAuthRotatedDeathDoesNotFlapHealth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	tokenSrv := newDeadTokenServer(t)
	tokenSrv.dead.Store(false)
	// At or below oauth2's ~10s expiry delta the issued token is already
	// stale, so every tick performs a real refresh: the death is
	// discovered right after the rotation without waiting an access-token
	// lifetime.
	tokenSrv.expiresIn.Store(5)
	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	cfg.OAuthProfile = "p"
	writeOAuthProfile(t, tokenSrv.srv.URL, "rt-0")
	shrinkAuthTick(t, 20*time.Millisecond, 20*time.Millisecond)

	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	// Wait for the first successful refresh: the in-memory lineage is now
	// rt-rotated.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if at, _ := adminAuthBlock(t, g)["last_refresh_ok_at"].(string); at != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no successful refresh within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Emulate CachingTokenSource's rotation persistence: Save is a
	// keyring write and the hermetic store runs with BASETEN_SWITCH_AUTH_NO_KEYRING,
	// so mirror what a keyring-enabled Save does and put the rotated
	// token in the store before the death.
	writeOAuthProfile(t, tokenSrv.srv.URL, "rt-rotated")

	tokenSrv.dead.Store(true)
	waitAuthHealth(t, g, "refresh_failed")

	// Stability: health must stay refresh_failed (no false-ok window) and
	// token-endpoint traffic must stop (dead ticks only read the store).
	hitsAtDead := tokenSrv.hits.Load()
	sampleUntil := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(sampleUntil) {
		if h := adminAuthBlock(t, g)["health"]; h != "refresh_failed" {
			t.Fatalf("health flapped to %v after a rotated credential died", h)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if hits := tokenSrv.hits.Load(); hits != hitsAtDead {
		t.Fatalf("token endpoint hits grew %d -> %d after death; the store watch is replaying the dead refresh", hitsAtDead, hits)
	}
}

// TestAuthDeadCadenceEngagesOnRequestDiscoveredDeath verifies the timer
// re-arm on the ok->dead flip: with the tick parked on a full healthy
// interval (1h here), a REQUEST discovers the death, and the re-login
// store watch must still converge within the dead cadence (the ~30s
// promise in auth login's SIGHUP-failure fallback), not at the next
// healthy-interval fire.
func TestAuthDeadCadenceEngagesOnRequestDiscoveredDeath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	tokenSrv := newDeadTokenServer(t)
	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	cfg.OAuthProfile = "p"
	writeOAuthProfile(t, tokenSrv.srv.URL, "rt-dead")
	shrinkAuthTick(t, time.Hour, 25*time.Millisecond)

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
	if resp.StatusCode != 502 {
		t.Fatalf("dead-credential request = %d, want 502", resp.StatusCode)
	}
	waitAuthHealth(t, g, "refresh_failed")

	// Re-login. Only the dead-cadence store watch can notice within the
	// 5s budget; the healthy arm is an hour out.
	tokenSrv.dead.Store(false)
	writeOAuthProfile(t, tokenSrv.srv.URL, "rt-new-login")
	waitAuthHealth(t, g, "ok")
}

// TestAuthTickSignedOutWatchesForFirstLogin verifies a gateway started
// with NO credential self-heals when one appears: auth login's fallback
// message promises the router picks a fresh login up on its own, which
// only the signed-out store watch can deliver (there is no client and no
// tick to notice otherwise). The healthy interval is an hour, so only the
// tight signed-out cadence can pass this within the budget.
func TestAuthTickSignedOutWatchesForFirstLogin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	tokenSrv := newDeadTokenServer(t)
	tokenSrv.dead.Store(false)
	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.BasetenKey = ""
	cfg.APIKeyFallback = false
	cfg.OAuthProfile = "p"
	// Deliberately no credential written: the auth file does not exist.
	shrinkAuthTick(t, time.Hour, 25*time.Millisecond)

	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	if h := adminAuthBlock(t, g)["health"]; h != "signed_out" {
		t.Fatalf("initial health = %v, want signed_out", h)
	}

	writeOAuthProfileExpiry(t, tokenSrv.srv.URL, "rt-first-login", time.Now().Add(time.Hour))
	waitAuthHealth(t, g, "ok")
	if auth := adminAuthBlock(t, g); auth["signed_in"] != true {
		t.Fatalf("signed_in = %v after first login, want true", auth["signed_in"])
	}
}
