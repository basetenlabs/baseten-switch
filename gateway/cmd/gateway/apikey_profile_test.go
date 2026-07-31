package gateway

// Tests for serving baseten-routed requests with an api_key-type baseten
// CLI profile ('baseten auth login' with an API key): the key is a
// first-class credential, used after OAuth and before the BASETEN_API_KEY
// env fallback, with no BASETEN_SWITCH_API_KEY_FALLBACK opt-in required.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// writeAPIKeyProfile writes an auth.json whose current profile is
// api_key-type with a plaintext key (keyring is disabled in testConfig).
func writeAPIKeyProfile(t *testing.T, profile, key string) {
	t.Helper()
	path := os.Getenv("BASETEN_SWITCH_AUTH_FILE")
	if path == "" {
		t.Fatal("BASETEN_SWITCH_AUTH_FILE not set (call testConfig first)")
	}
	blob := fmt.Sprintf(`{
  "version": 1,
  "current": %q,
  "profiles": {
    %q: {
      "remote_url": "https://app.baseten.co",
      "auth_type": "api_key",
      "api_key": %q
    }
  }
}`, profile, profile, key)
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
}

func anthropicOKUpstream(gotAuth *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"zai-org/GLM-5.2","usage":{"input_tokens":1,"output_tokens":1}}`))
	}
}

func postPing(t *testing.T, g *Gateway) *http.Response {
	t.Helper()
	body := []byte(`{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest("POST", clientURL(g, "claude-code", "/v1/messages"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// An api_key-type profile alone (no env fallback configured) serves
// baseten-routed requests with "Authorization: Api-Key <key>".
func TestBasetenRouteAPIKeyProfileForwardsAPIKeyHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(anthropicOKUpstream(&gotAuth))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	writeAPIKeyProfile(t, "default", "profile-key")
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp := postPing(t, g)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d want 200 (body: %s)", resp.StatusCode, b)
	}
	if gotAuth != "Api-Key profile-key" {
		t.Fatalf("upstream Authorization = %q, want Api-Key profile-key", gotAuth)
	}
}

// The profile key outranks the env fallback key.
func TestBasetenRouteAPIKeyProfileOutranksEnvFallback(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(anthropicOKUpstream(&gotAuth))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.APIKeyFallback = true
	cfg.BasetenKey = "env-fallback-key"
	writeAPIKeyProfile(t, "default", "profile-key")
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp := postPing(t, g)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d want 200", resp.StatusCode)
	}
	if gotAuth != "Api-Key profile-key" {
		t.Fatalf("upstream Authorization = %q, want Api-Key profile-key", gotAuth)
	}
}

// Admin auth status reports the api_key_profile mode truthfully: health ok
// (nothing to refresh), not signed_out, and no env-fallback claim.
func TestAdminAuthStatusReportsAPIKeyProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	writeAPIKeyProfile(t, "default", "profile-key")
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp, err := http.Get(adminURL(g, "/v1/admin/auth/status"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("bad status body %s: %v", b, err)
	}
	if st["signed_in"] != false {
		t.Fatalf("signed_in = %v, want false (api_key profile is not OAuth)", st["signed_in"])
	}
	if st["auth_mode"] != "api_key_profile" {
		t.Fatalf("auth_mode = %v, want api_key_profile", st["auth_mode"])
	}
	if st["api_key_profile"] != "default" {
		t.Fatalf("api_key_profile = %v, want default", st["api_key_profile"])
	}
	if st["health"] != "ok" {
		t.Fatalf("health = %v, want ok", st["health"])
	}
	if st["fallback_in_use"] != false {
		t.Fatalf("fallback_in_use = %v, want false (profile key is not the env fallback)", st["fallback_in_use"])
	}
}

// A key that is NOT readable (api_key-type profile with no plaintext key
// and keyring disabled) must not silently serve: with no env fallback the
// route still rejects needs-login.
func TestBasetenRouteAPIKeyProfileUnreadableKeyRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be hit")
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	writeAPIKeyProfile(t, "default", "")
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	resp := postPing(t, g)
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("got %d want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Baseten-Switch"); got != "needs-login" {
		t.Fatalf("X-Baseten-Switch = %q, want needs-login", got)
	}
}

// The signed-out background tick detects an API-key login appearing in the
// store and loads it without a SIGHUP; a logout is likewise picked up.
func TestAuthTickDetectsAPIKeyProfileLoginAndLogout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, srv.URL)
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	g, adminL, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	if _, _, key := g.basetenAuthClient(); key != "" {
		t.Fatalf("pre-login key = %q, want empty", key)
	}

	// Login lands as an api_key-type profile; the tick must load it.
	writeAPIKeyProfile(t, "default", "profile-key")
	g.authTickOnce()
	if _, _, key := g.basetenAuthClient(); key != "profile-key" {
		t.Fatalf("post-login key = %q, want profile-key", key)
	}

	// Logout empties the store; the tick must drop the key.
	if err := os.WriteFile(os.Getenv("BASETEN_SWITCH_AUTH_FILE"), []byte(`{"version":1,"profiles":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	g.authTickOnce()
	if _, _, key := g.basetenAuthClient(); key != "" {
		t.Fatalf("post-logout key = %q, want empty", key)
	}
}
