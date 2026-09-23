package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
	"github.com/basetenlabs/baseten-switch/gateway/internal/proxy"
)

func TestSavedAPIKeyAuthenticatesRequestsWithoutCredentialFallback(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var inferenceHits, nativeHits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Api-Key synthetic-saved-key" || r.Header.Get("X-Api-Key") != "" {
					t.Errorf("%s did not use only the saved credential", r.URL.Path)
				}
				if r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/responses" {
					inferenceHits.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status != http.StatusOK {
					_, _ = io.WriteString(w, `{"error":"authorization rejected"}`)
					return
				}
				switch r.URL.Path {
				case "/v1/messages":
					_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"example/model","usage":{"input_tokens":1,"output_tokens":1}}`)
				case "/v1/responses":
					_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
				case "/v1/models":
					_, _ = io.WriteString(w, `{"data":[]}`)
				case "/v1/model_apis":
					_, _ = io.WriteString(w, `{"items":[],"pagination":{"has_more":false,"cursor":null}}`)
				default:
					t.Errorf("unexpected upstream request: %s", r.URL.Path)
				}
			}))
			defer upstream.Close()
			native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				nativeHits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer native.Close()
			cfg := testConfig(t, upstream.URL, native.URL)
			cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
			cfg.OAuthHost = upstream.URL
			cfg.OAuthProfile = "p"
			writeAPIKeyProfile(t, "synthetic-cli-key")
			if err := auth.SaveSavedAPIKey(cfg.ConfigPath, "synthetic-saved-key"); err != nil {
				t.Fatal(err)
			}
			claude := resolvedAnthropicBaseten(t)
			claude.FallbackRoute = "anthropic"
			codex := resolvedOpenAIBaseten(t, "codex", "baseten")
			g, admin, _ := newGateway(t, cfg, claude, codex)
			defer admin.Close()
			stop := start(t, g)
			defer stop()
			for _, request := range []struct{ method, client, path, body string }{
				{http.MethodPost, claude.Name, "/v1/messages", `{"model":"alias","messages":[]}`},
				{http.MethodPost, codex.Name, "/v1/responses", `{"model":"alias","input":"ping"}`},
				{http.MethodGet, codex.Name, "/v1/models", ""},
			} {
				req, _ := http.NewRequest(request.method, clientURL(g, request.client, request.path), strings.NewReader(request.body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer synthetic-harness-key")
				req.Header.Set("X-Api-Key", "synthetic-harness-key")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != status {
					t.Fatalf("%s status = %d, want %d", request.path, resp.StatusCode, status)
				}
			}
			resp, err := http.Get(adminURL(g, "/v1/admin/model-catalog"))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var catalog modelCatalogResponse
			if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
				t.Fatal(err)
			}
			wantState := map[int]string{http.StatusOK: "ready", http.StatusUnauthorized: "signed_out", http.StatusForbidden: "error"}[status]
			if catalog.State != wantState || (status == http.StatusUnauthorized && catalog.SignedOutReason != modelCatalogSignedOutReasonRejected) {
				t.Fatalf("catalog outcome = %+v", catalog)
			}
			if inferenceHits.Load() != 2 || nativeHits.Load() != 0 {
				t.Fatalf("inference requests=%d native requests=%d", inferenceHits.Load(), nativeHits.Load())
			}
		})
	}
}

func TestSavedAPIKeyPrecedenceAndTransitions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	cfg := testConfig(t, server.URL, server.URL)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
	cfg.OAuthProfile = "p"
	writeOAuthProfileExpiry(t, server.URL, "original-oauth", time.Now().Add(time.Hour))
	g, admin, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer admin.Close()
	defer g.shutdownAllClients()
	assertKey := func(want string) {
		t.Helper()
		selected, ok := g.basetenAuth()
		if !ok || selected.mode != proxy.UpstreamModeAPIKey || selected.apiKey != want || selected.source != basetenAuthSavedAPIKey {
			t.Fatalf("unexpected selection mode=%v source=%s available=%t", selected.mode, selected.source, ok)
		}
		if g.authTick != nil || g.oauthClient != nil {
			t.Fatal("saved key retained OAuth refresh path")
		}
		if _, authorization, ok := g.catalogRequestClient(); !ok || authorization != "Api-Key "+want {
			t.Fatal("pricing catalog did not use saved key")
		}
		if selected, ok := g.basetenProfileAuth(); !ok || selected.apiKey != want {
			t.Fatal("account catalog did not use saved key")
		}
	}
	oldGen := g.authGen
	g.authLastErr = "old OAuth failure"
	g.emailCached = "old@example.invalid"
	if err := auth.SaveSavedAPIKey(cfg.ConfigPath, "saved-one"); err != nil {
		t.Fatal(err)
	}
	g.authTickOnce()
	assertKey("saved-one")
	g.noteAuthRefresh(oldGen, "old", os.ErrPermission)
	if g.authHealth().Health != "ok" || g.authLastErr != "" || g.emailCached != "" {
		t.Fatal("saved key inherited OAuth health or identity")
	}
	writeAPIKeyProfile(t, "cli-new")
	g.authTickOnce()
	assertKey("saved-one")
	if err := auth.SaveSavedAPIKey(cfg.ConfigPath, "saved-two"); err != nil {
		t.Fatal(err)
	}
	g.authTickOnce()
	assertKey("saved-two")
	if err := auth.RemoveSavedAPIKey(cfg.ConfigPath); err != nil {
		t.Fatal(err)
	}
	g.authTickOnce()
	selected, ok := g.basetenAuth()
	if !ok || selected.apiKey != "cli-new" || selected.source != basetenAuthProfileAPIKey {
		t.Fatal("removal did not restore current CLI credential")
	}
}

func TestSavedAPIKeyMalformedStoreBlocksOtherCredentials(t *testing.T) {
	cfg := testConfig(t, "http://example.invalid", "http://example.invalid")
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
	cfg.OAuthProfile = "p"
	writeAPIKeyProfile(t, "cli-key")
	if err := os.WriteFile(cfg.ConfigPath+".api-key", []byte("bad key"), 0600); err != nil {
		t.Fatal(err)
	}
	g, admin, _ := newGateway(t, cfg, resolvedAnthropicBaseten(t))
	defer admin.Close()
	defer g.shutdownAllClients()
	if _, ok := g.basetenAuth(); ok {
		t.Fatal("unreadable saved key fell through to another credential")
	}
	if signedIn, _, fallback := g.authState(); signedIn || fallback || g.authHealth().Health != "error" {
		t.Fatal("saved-store failure not reflected in status")
	}
	if err := os.WriteFile(os.Getenv("BASETEN_SWITCH_AUTH_FILE"), []byte("invalid-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveSavedAPIKey(cfg.ConfigPath, "repaired-key"); err != nil {
		t.Fatal(err)
	}
	g.authTickOnce()
	if selected, ok := g.basetenAuth(); !ok || selected.apiKey != "repaired-key" {
		t.Fatal("saved key repair depended on malformed CLI store")
	}
	if err := auth.RemoveSavedAPIKey(cfg.ConfigPath); err != nil {
		t.Fatal(err)
	}
	g.authTickOnce()
	if selected, _ := g.basetenAuth(); selected.source == basetenAuthSavedAPIKey {
		t.Fatal("removed key survived malformed CLI store")
	}
}

func TestUnreadableSavedAPIKeyPreservesConfiguredNativeFallback(t *testing.T) {
	for _, fallbackEnabled := range []bool{false, true} {
		name := "without native fallback"
		if fallbackEnabled {
			name = "with native fallback"
		}
		t.Run(name, func(t *testing.T) {
			var basetenHits, nativeHits atomic.Int32
			baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				basetenHits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer baseten.Close()
			const body = `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"ping"}]}`
			native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nativeHits.Add(1)
				if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer synthetic-native-token" || r.Header.Get("X-Api-Key") != "synthetic-native-key" {
					t.Error("native fallback did not preserve the harness credentials and endpoint")
				}
				gotBody, _ := io.ReadAll(r.Body)
				if string(gotBody) != body {
					t.Error("native fallback did not preserve the ingress request")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_native","type":"message","role":"assistant","content":[],"model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer native.Close()
			cfg := testConfig(t, baseten.URL, native.URL)
			cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
			cfg.OAuthProfile = "p"
			cfg.BasetenKey = "synthetic-env-key"
			cfg.APIKeyFallback = true
			writeAPIKeyProfile(t, "synthetic-cli-key")
			if err := os.WriteFile(cfg.ConfigPath+".api-key", []byte("invalid key"), 0600); err != nil {
				t.Fatal(err)
			}
			rc := resolvedAnthropicBaseten(t)
			if fallbackEnabled {
				rc.FallbackRoute = "anthropic"
			}
			g, admin, _ := newGateway(t, cfg, rc)
			defer admin.Close()
			stop := start(t, g)
			defer stop()
			req, _ := http.NewRequest(http.MethodPost, clientURL(g, rc.Name, "/v1/messages"), strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer synthetic-native-token")
			req.Header.Set("X-Api-Key", "synthetic-native-key")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			if basetenHits.Load() != 0 || g.authSource() != "saved_api_key" || g.authHealth().Health != "error" {
				t.Fatal("unreadable saved key did not block CLI and environment credentials")
			}
			if fallbackEnabled {
				if response.StatusCode != http.StatusOK || nativeHits.Load() != 1 || response.Header.Get(authUnavailableFallbackHeader) != fallbackTriggerAuthUnavailable {
					t.Fatalf("native fallback status=%d hits=%d marker=%q", response.StatusCode, nativeHits.Load(), response.Header.Get(authUnavailableFallbackHeader))
				}
			} else if response.StatusCode != http.StatusServiceUnavailable || nativeHits.Load() != 0 {
				t.Fatalf("unconfigured native fallback status=%d hits=%d", response.StatusCode, nativeHits.Load())
			}
		})
	}
}
