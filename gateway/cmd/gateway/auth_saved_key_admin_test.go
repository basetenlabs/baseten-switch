package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
)

func savedKeyRequest(g *Gateway, method, body, header string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/v1/admin/auth/api-key", strings.NewReader(body))
	r.Header.Set(adminMutationHeader, header)
	w := httptest.NewRecorder()
	g.adminServer.Handler.ServeHTTP(w, r)
	return w
}

func TestSavedKeyAdminReconcilesAuthAfterStorageFailure(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
			cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
			cfg.OAuthProfile = "p"
			writeAPIKeyProfile(t, "synthetic-cli-key")
			if err := auth.SaveSavedAPIKey(cfg.ConfigPath, "synthetic-old-key"); err != nil {
				t.Fatal(err)
			}
			g := newAuthReloadTestGateway(t, cfg)
			if err := os.Remove(cfg.ConfigPath + ".api-key"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(cfg.ConfigPath+".api-key", 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.ConfigPath+".api-key", "obstruction"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			body := ""
			if method == http.MethodPut {
				body = `{"api_key":"synthetic-new-key"}`
			}
			w := savedKeyRequest(g, method, body, "1")
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want storage failure", w.Code)
			}
			if _, ok := g.basetenAuth(); ok {
				t.Fatal("failed mutation retained stale credentials or selected CLI auth")
			}
			assertResponseOmits(t, w, "synthetic-old-key", "synthetic-new-key", "synthetic-cli-key")
		})
	}
}

func TestSavedKeyAdminLifecycle(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
	cfg.OAuthProfile = "p"
	writeAPIKeyProfile(t, "synthetic-cli-key")
	g := newAuthReloadTestGateway(t, cfg)
	for _, key := range []string{"synthetic-saved-one", "synthetic-saved-two"} {
		w := savedKeyRequest(g, http.MethodPut, `{"api_key":"`+key+`"}`, "1")
		if w.Code != http.StatusOK {
			t.Fatalf("save status = %d", w.Code)
		}
		status := decodeAuthAdminResponse(t, w)
		if status["source"] != "saved_api_key" || status["saved_api_key"] != true || status["profile"] != "" {
			t.Fatalf("unexpected saved status: %v", status)
		}
		assertResponseOmits(t, w, key, "synthetic-cli-key")
		selected, ok := g.basetenAuth()
		if !ok || selected.apiKey != key {
			t.Fatal("gateway did not use saved key")
		}
		if stored, err := auth.LoadSavedAPIKey(cfg.ConfigPath); err != nil || stored != key {
			t.Fatal("saved key did not persist")
		}
		if email, expiry := g.authEmailAndExpiry(); email != "" || expiry != "" {
			t.Fatal("saved key inherited CLI identity")
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("credential response is cacheable")
		}
	}
	w := savedKeyRequest(g, http.MethodDelete, "", "1")
	if w.Code != http.StatusOK {
		t.Fatalf("remove status = %d", w.Code)
	}
	selected, ok := g.basetenAuth()
	if !ok || selected.apiKey != "synthetic-cli-key" {
		t.Fatal("removal did not restore CLI auth")
	}
	if key, err := auth.LoadSavedAPIKey(cfg.ConfigPath); err != nil || key != "" {
		t.Fatal("saved key remains")
	}
}

func TestSavedKeyAdminRejectsInvalidMutations(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
	if err := auth.SaveSavedAPIKey(cfg.ConfigPath, "synthetic-existing-key"); err != nil {
		t.Fatal(err)
	}
	g := newAuthReloadTestGateway(t, cfg)
	for _, tc := range []struct {
		name, method, body, header string
		code                       int
	}{
		{"no header", "PUT", `{"api_key":"synthetic-new-key"}`, "", 403},
		{"delete no header", "DELETE", "", "", 403},
		{"read", "GET", "", "1", 405},
		{"empty", "PUT", `{"api_key":" "}`, "1", 400},
		{"newline", "PUT", `{"api_key":"synthetic\nkey"}`, "1", 400},
		{"unknown field", "PUT", `{"api_key":"synthetic-new-key","other":true}`, "1", 400},
		{"trailing body", "PUT", `{"api_key":"synthetic-new-key"}{}`, "1", 400},
		{"oversized", "PUT", strings.Repeat("x", 9000), "1", 400},
		{"delete body", "DELETE", "x", "1", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := savedKeyRequest(g, tc.method, tc.body, tc.header)
			if w.Code != tc.code {
				t.Fatalf("status = %d, want %d", w.Code, tc.code)
			}
			assertResponseOmits(t, w, "synthetic-existing-key", "synthetic-new-key")
			key, err := auth.LoadSavedAPIKey(cfg.ConfigPath)
			if err != nil || key != "synthetic-existing-key" {
				t.Fatal("rejected request changed saved key")
			}
		})
	}
}
