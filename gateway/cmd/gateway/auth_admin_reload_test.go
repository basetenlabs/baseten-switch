package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

func newAuthReloadTestGateway(t *testing.T, cfg Config) *Gateway {
	t.Helper()
	g := &Gateway{
		cfg:     cfg,
		client:  &http.Client{Transport: defaultTransport()},
		pricing: pricing.New(),
	}
	// Match a running gateway that initialized before the CLI credential
	// appeared.
	g.refreshAuth()
	mux := http.NewServeMux()
	g.registerAdmin(mux)
	g.adminServer = &http.Server{Handler: mux}
	return g
}

func authReloadRequest(t *testing.T, g *Gateway, method string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/admin/auth/reload", body)
	req.Header.Set(adminMutationHeader, adminMutationHeaderValue)
	recorder := httptest.NewRecorder()
	g.adminServer.Handler.ServeHTTP(recorder, req)
	return recorder
}

func authReloadRequestWithHeader(t *testing.T, g *Gateway, header string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/reload", nil)
	if header != "" {
		req.Header.Set(adminMutationHeader, header)
	}
	recorder := httptest.NewRecorder()
	g.adminServer.Handler.ServeHTTP(recorder, req)
	return recorder
}

func writeOAuthProfileWithAccessToken(t *testing.T, remoteURL, accessToken, refreshToken string) {
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
        "access_token": %q,
        "refresh_token": %q,
        "expiry": %q
      }
    }
  }
}`, remoteURL, accessToken, refreshToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeAuthAdminResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, recorder.Body.String())
	}
	return response
}

func assertResponseOmits(t *testing.T, recorder *httptest.ResponseRecorder, values ...string) {
	t.Helper()
	body := recorder.Body.String()
	for _, value := range values {
		if value != "" && strings.Contains(body, value) {
			t.Fatalf("response contains protected value %q: %s", value, body)
		}
	}
}

func TestAdminAuthReloadLoadsOAuthFirstLogin(t *testing.T) {
	const refreshToken = "synthetic-refresh-token-must-not-leak"
	const accessToken = "expired-at"
	var whoamiRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		whoamiRequests++
		if r.URL.Path != "/v1/users/me" {
			t.Errorf("path = %q, want /v1/users/me", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+accessToken {
			t.Errorf("Authorization = %q, want OAuth access token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"email":"person@example.invalid"}`)
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.OAuthProfile = "p"
	cfg.OAuthHost = upstream.URL
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	g := newAuthReloadTestGateway(t, cfg)

	if signedIn, _, _ := g.authState(); signedIn {
		t.Fatal("gateway started signed in before the CLI credential appeared")
	}
	writeOAuthProfileExpiry(t, upstream.URL, refreshToken, time.Now().Add(time.Hour))

	recorder := authReloadRequest(t, g, http.MethodPost, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	response := decodeAuthAdminResponse(t, recorder)
	if response["signed_in"] != true || response["auth_type"] != "oauth" || response["health"] != "ok" {
		t.Fatalf("auth reload response = %+v", response)
	}
	if response["profile"] != "p" || response["fallback_in_use"] != false {
		t.Fatalf("auth reload projection = %+v", response)
	}
	if whoamiRequests != 0 {
		t.Fatalf("reload made %d upstream requests, want 0", whoamiRequests)
	}
	assertResponseOmits(t, recorder, refreshToken, accessToken, "Authorization", "Bearer ")

	statusReq := httptest.NewRequest(http.MethodGet, "/v1/admin/auth/status", nil)
	statusRecorder := httptest.NewRecorder()
	g.adminServer.Handler.ServeHTTP(statusRecorder, statusReq)
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("auth status = %d, want 200", statusRecorder.Code)
	}
	if got := statusRecorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("auth status Cache-Control = %q, want no-store", got)
	}
	statusResponse := decodeAuthAdminResponse(t, statusRecorder)
	for key, want := range response {
		if got := statusResponse[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("status[%q] = %v, want reload value %v", key, got, want)
		}
	}
	if whoamiRequests != 1 {
		t.Fatalf("status whoami requests = %d, want 1", whoamiRequests)
	}

	const upstreamBodySecret = "synthetic-upstream-body-secret-must-not-leak"
	g.authMu.Lock()
	g.authLastErr = `oauth2: cannot fetch token: 400 Bad Request\nResponse: {"error":"invalid_grant","refresh_token":"` + upstreamBodySecret + `"}`
	g.authLastErrAt = time.Now()
	g.authMu.Unlock()
	sanitized := authReloadRequest(t, g, http.MethodPost, nil)
	if sanitized.Code != http.StatusOK {
		t.Fatalf("reload with prior refresh failure status = %d, want 200", sanitized.Code)
	}
	if _, present := decodeAuthAdminResponse(t, sanitized)["last_refresh_error"]; present {
		t.Fatal("local reload receipt includes last_refresh_error")
	}
	assertResponseOmits(t, sanitized, refreshToken, accessToken, upstreamBodySecret, "Response:")
}

func TestAdminAuthReloadLoadsAndReplacesAPIKeyProfile(t *testing.T) {
	const firstKey = "synthetic-profile-key-one-must-not-leak"
	const replacementKey = "synthetic-profile-key-two-must-not-leak"
	cfg := testConfig(t, "http://baseten.invalid", "http://anthropic.invalid")
	cfg.OAuthProfile = "p"
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	g := newAuthReloadTestGateway(t, cfg)

	writeAPIKeyProfile(t, firstKey)
	first := authReloadRequest(t, g, http.MethodPost, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first reload status = %d, want 200; body = %s", first.Code, first.Body.String())
	}
	firstResponse := decodeAuthAdminResponse(t, first)
	if firstResponse["signed_in"] != true || firstResponse["auth_type"] != "api_key" || firstResponse["health"] != "ok" {
		t.Fatalf("first reload response = %+v", firstResponse)
	}
	assertResponseOmits(t, first, firstKey, "Api-Key ")

	writeAPIKeyProfile(t, replacementKey)
	replacement := authReloadRequest(t, g, http.MethodPost, nil)
	if replacement.Code != http.StatusOK {
		t.Fatalf("replacement reload status = %d, want 200; body = %s", replacement.Code, replacement.Body.String())
	}
	selected, ok := g.basetenProfileAuth()
	if !ok || selected.source != basetenAuthProfileAPIKey || selected.apiKey != replacementKey {
		t.Fatalf("selected profile auth = source %q, key replaced %t", selected.source, selected.apiKey == replacementKey)
	}
	assertResponseOmits(t, replacement, firstKey, replacementKey, "Api-Key ")
}

func TestAdminAuthReloadRejectsMethodsAndBodiesBeforeMutation(t *testing.T) {
	const profileKey = "synthetic-rejected-request-key-must-not-leak"
	cfg := testConfig(t, "http://baseten.invalid", "http://anthropic.invalid")
	cfg.OAuthProfile = "p"
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	g := newAuthReloadTestGateway(t, cfg)
	writeAPIKeyProfile(t, profileKey)

	for name, header := range map[string]string{
		"missing": "",
		"wrong":   "0",
	} {
		t.Run("admin_header_"+name, func(t *testing.T) {
			recorder := authReloadRequestWithHeader(t, g, header)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", recorder.Code, recorder.Body.String())
			}
			assertResponseOmits(t, recorder, profileKey)
			if signedIn, _, _ := g.authState(); signedIn {
				t.Fatal("rejected admin header reloaded authentication")
			}
		})
	}

	for _, method := range []string{
		http.MethodGet,
		http.MethodHead,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions,
	} {
		t.Run(method, func(t *testing.T) {
			// Method rejection precedes the mutation-header check. In
			// particular, browser preflight receives 405 with no CORS grant.
			req := httptest.NewRequest(method, "/v1/admin/auth/reload", nil)
			recorder := httptest.NewRecorder()
			g.adminServer.Handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405; body = %s", recorder.Code, recorder.Body.String())
			}
			assertResponseOmits(t, recorder, profileKey)
			if signedIn, _, _ := g.authState(); signedIn {
				t.Fatal("rejected method reloaded authentication")
			}
		})
	}

	for name, body := range map[string]string{
		"json":       `{}`,
		"space":      " ",
		"line_break": "\n",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := authReloadRequest(t, g, http.MethodPost, strings.NewReader(body))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			assertResponseOmits(t, recorder, profileKey)
			if signedIn, _, _ := g.authState(); signedIn {
				t.Fatal("rejected body reloaded authentication")
			}
		})
	}

	valid := authReloadRequest(t, g, http.MethodPost, bytes.NewReader(nil))
	if valid.Code != http.StatusOK {
		t.Fatalf("empty-body reload status = %d, want 200; body = %s", valid.Code, valid.Body.String())
	}
	if signedIn, authType, _ := g.authState(); !signedIn || authType != "api_key" {
		t.Fatalf("valid reload = signed_in %t, auth_type %q", signedIn, authType)
	}
	assertResponseOmits(t, valid, profileKey, "Api-Key ")
}

func TestAdminAuthReloadSerializesWithConfigReload(t *testing.T) {
	const profileKey = "synthetic-serialized-profile-key"
	cfg := testConfig(t, "http://baseten.invalid", "http://anthropic.invalid")
	cfg.OAuthProfile = "profile-before-config-reload"
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	g := newAuthReloadTestGateway(t, cfg)
	writeAPIKeyProfile(t, profileKey)

	g.reloadMu.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- authReloadRequest(t, g, http.MethodPost, nil)
	}()
	select {
	case <-done:
		g.reloadMu.Unlock()
		t.Fatal("auth reload completed while config reload lock was held")
	case <-time.After(25 * time.Millisecond):
	}
	next := g.runtimeConfig()
	next.OAuthProfile = "p"
	g.setRuntimeConfig(next)
	g.reloadMu.Unlock()

	select {
	case recorder := <-done:
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auth reload did not complete after config reload lock was released")
	}
	selected, ok := g.basetenProfileAuth()
	if !ok || selected.source != basetenAuthProfileAPIKey || selected.apiKey != profileKey {
		t.Fatalf("auth reload published stale profile: source %q, selected current key %t", selected.source, selected.apiKey == profileKey)
	}
}

func TestAdminAuthReloadClearsCachedIdentityOnCredentialReplacement(t *testing.T) {
	const (
		accessA  = "synthetic-access-a"
		refreshA = "synthetic-refresh-a"
		accessB  = "synthetic-access-b"
		refreshB = "synthetic-refresh-b"
	)
	var whoamiRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		whoamiRequests++
		email := ""
		switch r.Header.Get("Authorization") {
		case "Bearer " + accessA:
			email = "account-a@example.invalid"
		case "Bearer " + accessB:
			email = "account-b@example.invalid"
		default:
			t.Errorf("unexpected Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"email":%q}`, email)
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.OAuthProfile = "p"
	cfg.OAuthHost = upstream.URL
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	g := newAuthReloadTestGateway(t, cfg)
	writeOAuthProfileWithAccessToken(t, upstream.URL, accessA, refreshA)
	if recorder := authReloadRequest(t, g, http.MethodPost, nil); recorder.Code != http.StatusOK {
		t.Fatalf("first reload status = %d", recorder.Code)
	}

	status := httptest.NewRecorder()
	g.adminServer.Handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/v1/admin/auth/status", nil))
	if got := decodeAuthAdminResponse(t, status)["email"]; got != "account-a@example.invalid" {
		t.Fatalf("first email = %q", got)
	}
	if whoamiRequests != 1 {
		t.Fatalf("first whoami requests = %d, want 1", whoamiRequests)
	}

	writeOAuthProfileWithAccessToken(t, upstream.URL, accessB, refreshB)
	if recorder := authReloadRequest(t, g, http.MethodPost, nil); recorder.Code != http.StatusOK {
		t.Fatalf("replacement reload status = %d", recorder.Code)
	}
	if whoamiRequests != 1 {
		t.Fatalf("replacement reload made an upstream request; hits = %d", whoamiRequests)
	}

	status = httptest.NewRecorder()
	g.adminServer.Handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/v1/admin/auth/status", nil))
	if got := decodeAuthAdminResponse(t, status)["email"]; got != "account-b@example.invalid" {
		t.Fatalf("replacement email = %q, want account B", got)
	}
	if whoamiRequests != 2 {
		t.Fatalf("replacement whoami requests = %d, want 2", whoamiRequests)
	}
}

func TestAdminAuthReloadClearsTransientHealthAcrossLogoutAndLogin(t *testing.T) {
	cfg := testConfig(t, "http://baseten.invalid", "http://anthropic.invalid")
	cfg.OAuthProfile = "p"
	cfg.APIKeyFallback = false
	cfg.BasetenKey = ""
	g := newAuthReloadTestGateway(t, cfg)
	writeOAuthProfileWithAccessToken(t, "http://baseten.invalid", "synthetic-access-a", "synthetic-refresh-a")
	if recorder := authReloadRequest(t, g, http.MethodPost, nil); recorder.Code != http.StatusOK {
		t.Fatalf("first reload status = %d", recorder.Code)
	}
	g.authMu.Lock()
	g.authLastErr = "synthetic transient network failure"
	g.authLastErrAt = time.Now()
	g.authMu.Unlock()

	if err := os.Remove(os.Getenv("BASETEN_SWITCH_AUTH_FILE")); err != nil {
		t.Fatal(err)
	}
	logout := authReloadRequest(t, g, http.MethodPost, nil)
	logoutResponse := decodeAuthAdminResponse(t, logout)
	if logoutResponse["signed_in"] != false || logoutResponse["health"] != "signed_out" {
		t.Fatalf("logout reload response = %+v", logoutResponse)
	}

	writeOAuthProfileWithAccessToken(t, "http://baseten.invalid", "synthetic-access-b", "synthetic-refresh-b")
	login := authReloadRequest(t, g, http.MethodPost, nil)
	loginResponse := decodeAuthAdminResponse(t, login)
	if loginResponse["signed_in"] != true || loginResponse["health"] != "ok" {
		t.Fatalf("replacement login response = %+v", loginResponse)
	}
	if health := g.authHealth(); health.LastError != "" || !health.LastErrorAt.IsZero() {
		t.Fatalf("replacement login retained transient health: %+v", health)
	}
}
