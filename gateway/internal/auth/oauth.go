package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	OAuthClientID   = "baseten-cli"
	defaultHost     = "https://api.baseten.co"
	deviceAuthPath  = "/v1/users/auth/device/authorize"
	tokenPath       = "/v1/users/auth/device/token"
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	errAuthPending  = "authorization_pending"
	errSlowDown     = "slow_down"
	errExpiredToken = "expired_token"
	errAccessDenied = "access_denied"
)

func DefaultHost() string {
	if v := os.Getenv("BASETEN_REMOTE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultHost
}

func OAuthConfig(host string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: OAuthClientID,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: host + deviceAuthPath,
			TokenURL:      host + tokenPath,
		},
	}
}

type DeviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

type deviceError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func RequestDeviceCode(ctx context.Context, host string) (*DeviceAuth, error) {
	form := url.Values{"client_id": {OAuthClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+deviceAuthPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var de deviceError
		_ = json.Unmarshal(body, &de)
		if de.Error != "" {
			return nil, fmt.Errorf("device authorize: %s: %s", de.Error, de.ErrorDescription)
		}
		return nil, fmt.Errorf("device authorize: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var da DeviceAuth
	if err := json.Unmarshal(body, &da); err != nil {
		return nil, fmt.Errorf("device authorize: parse: %w", err)
	}
	if da.Interval <= 0 {
		da.Interval = 5
	}
	return &da, nil
}

func PollDeviceToken(ctx context.Context, host string, da *DeviceAuth) (*oauth2.Token, error) {
	deadline := time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)
	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		tok, slow, err := pollOnce(ctx, host, da.DeviceCode)
		switch {
		case err != nil:
			return nil, err
		case tok != nil:
			return tok, nil
		case slow:
			interval *= 2
		}
		if time.Now().Add(interval).After(deadline) {
			return nil, fmt.Errorf("device token: %s", errExpiredToken)
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func pollOnce(ctx context.Context, host, deviceCode string) (*oauth2.Token, bool, error) {
	form := url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {deviceCode},
		"client_id":   {OAuthClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+tokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var de deviceError
	_ = json.Unmarshal(body, &de)
	if de.Error != "" {
		switch de.Error {
		case errAuthPending:
			return nil, false, nil
		case errSlowDown:
			return nil, true, nil
		case errExpiredToken, errAccessDenied:
			return nil, false, fmt.Errorf("device token: %s: %s", de.Error, de.ErrorDescription)
		default:
			return nil, false, fmt.Errorf("device token: %s: %s", de.Error, de.ErrorDescription)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("device token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tok oauth2.Token
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, false, fmt.Errorf("device token: parse: %w", err)
	}
	if !tok.Valid() {
		return nil, false, fmt.Errorf("device token: invalid token returned")
	}
	return &tok, false, nil
}
