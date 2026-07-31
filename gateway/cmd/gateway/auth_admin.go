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

func (g *Gateway) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		g.reject(w, 405, "method not allowed")
		return
	}
	signedIn, fallbackInUse := g.authState()
	email, expiresAt := g.authEmailAndExpiry()
	ah := g.authHealth()
	cfg := g.runtimeConfig()
	mode, apiKeyProfile := g.authMode()
	writeJSON(w, 200, map[string]any{
		"signed_in":             signedIn,
		"auth_mode":             mode,
		"api_key_profile":       apiKeyProfile,
		"health":                ah.Health,
		"last_refresh_error":    ah.LastError,
		"last_refresh_error_at": rfc3339OrEmpty(ah.LastErrorAt),
		"last_refresh_ok_at":    rfc3339OrEmpty(ah.LastOKAt),
		"profile":               cfg.OAuthProfile,
		"fallback_enabled":      cfg.APIKeyFallback,
		"fallback_in_use":       fallbackInUse,
		"email":                 email,
		"expires_at":            expiresAt,
	})
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
