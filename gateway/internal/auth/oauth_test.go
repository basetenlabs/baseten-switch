package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOAuthConfig(t *testing.T) {
	cfg := OAuthConfig(DefaultHost())
	if cfg.ClientID != OAuthClientID {
		t.Fatalf("client_id = %q, want %q", cfg.ClientID, OAuthClientID)
	}
	wantDevice := "https://api.baseten.co/v1/users/auth/device/authorize"
	wantToken := "https://api.baseten.co/v1/users/auth/device/token"
	if cfg.Endpoint.DeviceAuthURL != wantDevice {
		t.Fatalf("device auth url = %q, want %q", cfg.Endpoint.DeviceAuthURL, wantDevice)
	}
	if cfg.Endpoint.TokenURL != wantToken {
		t.Fatalf("token url = %q, want %q", cfg.Endpoint.TokenURL, wantToken)
	}
	if cfg.RedirectURL != "" {
		t.Fatalf("redirect url set: %q", cfg.RedirectURL)
	}
	if len(cfg.Scopes) != 0 {
		t.Fatalf("scopes set: %v", cfg.Scopes)
	}
}

func TestRequestDeviceCodeAndPoll(t *testing.T) {
	var pollCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/users/auth/device/authorize":
			if r.Method != http.MethodPost {
				t.Errorf("authorize method %q", r.Method)
			}
			_ = r.ParseForm()
			if got := r.PostForm.Get("client_id"); got != "baseten-cli" {
				t.Errorf("client_id form value %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"device_code":               "dev-123",
				"user_code":                 "ABC123",
				"verification_uri":          "https://example.com/device",
				"verification_uri_complete": "https://example.com/device?user_code=ABC123",
				"interval":                  1,
				"expires_in":                900,
			})
		case "/v1/users/auth/device/token":
			pollCount++
			_ = r.ParseForm()
			if got := r.PostForm.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Errorf("grant_type form value %q", got)
			}
			if got := r.PostForm.Get("device_code"); got != "dev-123" {
				t.Errorf("device_code form value %q", got)
			}
			if pollCount == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"error":"authorization_pending","error_description":"still waiting"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			exp := time.Now().Add(1 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
			_, _ = w.Write([]byte(strings.Join([]string{
				`{"access_token":"at-xyz","refresh_token":"rt-abc","token_type":"Bearer","expiry":"` + exp + `"}`,
			}, "")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	da, err := RequestDeviceCode(ctx, srv.URL)
	if err != nil {
		t.Fatalf("RequestDeviceCode: %v", err)
	}
	if da.DeviceCode != "dev-123" || da.UserCode != "ABC123" || da.Interval != 1 {
		t.Fatalf("unexpected DeviceAuth: %+v", da)
	}
	tok, err := PollDeviceToken(ctx, srv.URL, da)
	if err != nil {
		t.Fatalf("PollDeviceToken: %v", err)
	}
	if tok.AccessToken != "at-xyz" {
		t.Fatalf("access token = %q", tok.AccessToken)
	}
	if tok.RefreshToken != "rt-abc" {
		t.Fatalf("refresh token = %q", tok.RefreshToken)
	}
	if tok.TokenType != "Bearer" {
		t.Fatalf("token type = %q", tok.TokenType)
	}
	if !tok.Valid() {
		t.Fatal("returned token invalid")
	}
}

func TestPollDeviceTokenErrorsExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"expired_token","error_description":"too slow"}`))
	}))
	defer srv.Close()
	da := &DeviceAuth{DeviceCode: "x", Interval: 1, ExpiresIn: 900}
	_, err := PollDeviceToken(context.Background(), srv.URL, da)
	if err == nil || !strings.Contains(err.Error(), "expired_token") {
		t.Fatalf("expected expired_token error, got %v", err)
	}
}
