package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
)

const (
	adminMutationHeader      = "X-Baseten-Switch-Admin"
	adminMutationHeaderValue = "1"
)

func (g *Gateway) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		g.reject(w, 405, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, g.authStatusResponse())
}

func (g *Gateway) handleAuthReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		g.reject(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// This non-simple request header forces browser callers through a CORS
	// preflight, which the loopback admin server does not approve. Check it
	// before body or credential-store work.
	if r.Header.Get(adminMutationHeader) != adminMutationHeaderValue {
		g.reject(w, http.StatusForbidden, "admin mutation header required")
		return
	}
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil {
			g.reject(w, http.StatusBadRequest, "could not read request body")
			return
		}
	}
	if len(body) != 0 {
		g.reject(w, http.StatusBadRequest, "request body must be empty")
		return
	}

	// Serialize with config reloads so refreshAuth cannot read an old runtime
	// profile and publish it after a concurrent SIGHUP has installed a new one.
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()
	g.refreshAuthLocked()
	// refreshAuthLocked preserves the existing nonblocking catalog-refresh
	// kick. The reload receipt does not wait for that work and performs no
	// synchronous upstream identity lookup.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, g.localAuthStatusResponse())
}

// authStatusResponse preserves the existing read-only status projection. Its
// local subset is shared with the explicit reload endpoint so that mutation
// can respond without an upstream user lookup.
func (g *Gateway) authStatusResponse() map[string]any {
	response := g.localAuthStatusResponse()
	email, expiresAt := g.authEmailAndExpiry()
	ah := g.authHealth()
	response["last_refresh_error"] = ah.LastError
	response["last_refresh_error_at"] = rfc3339OrEmpty(ah.LastErrorAt)
	response["last_refresh_ok_at"] = rfc3339OrEmpty(ah.LastOKAt)
	response["email"] = email
	response["expires_at"] = expiresAt
	return response
}

// localAuthStatusResponse reads only in-memory state and configuration. It
// performs no synchronous upstream request and includes no credential values,
// fingerprints, refresh errors, or remote response bodies.
func (g *Gateway) localAuthStatusResponse() map[string]any {
	signedIn, authType, fallbackInUse := g.authState()
	ah := g.authHealth()
	cfg := g.runtimeConfig()
	return map[string]any{
		"signed_in":        signedIn,
		"auth_type":        authType,
		"health":           ah.Health,
		"profile":          cfg.OAuthProfile,
		"fallback_enabled": cfg.APIKeyFallback,
		"fallback_in_use":  fallbackInUse,
	}
}

func (g *Gateway) authEmailAndExpiry() (string, string) {
	cfg := g.runtimeConfig()
	expiresAt := ""
	if tok, _, _ := auth.Load(cfg.OAuthProfile); tok != nil {
		if exp, ok := jwtExpiry(tok.AccessToken); ok {
			expiresAt = time.Unix(exp, 0).UTC().Format(time.RFC3339)
		}
	}
	g.authMu.Lock()
	client := g.oauthClient
	g.authMu.Unlock()
	if client == nil {
		return "", expiresAt
	}
	g.emailMu.Lock()
	cached := g.emailCached
	fetchedAt := g.emailFetchedAt
	g.emailMu.Unlock()
	if cached != "" && time.Since(fetchedAt) < 60*time.Second {
		return cached, expiresAt
	}
	email := ""
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.OAuthHost+"/v1/users/me", nil)
	if err == nil {
		if resp, err := client.Do(req); err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var wa struct {
					Email string `json:"email"`
				}
				_ = json.Unmarshal(body, &wa)
				email = wa.Email
			}
		}
	}
	g.emailMu.Lock()
	g.emailCached = email
	g.emailFetchedAt = time.Now()
	g.emailMu.Unlock()
	return email, expiresAt
}

func jwtExpiry(accessToken string) (int64, bool) {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return 0, false
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return 0, false
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(raw, &claims); err != nil {
		return 0, false
	}
	v, ok := claims["exp"]
	if !ok {
		return 0, false
	}
	var exp int64
	if err := json.Unmarshal(v, &exp); err != nil {
		return 0, false
	}
	return exp, true
}
