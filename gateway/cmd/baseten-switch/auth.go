package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
)

type whoamiResponse struct {
	Email         string `json:"email"`
	WorkspaceName string `json:"workspace_name"`
}

func cmdWhoami(args []string) int {
	// Empty profile falls through to auth.json's current-profile pointer
	// inside auth.Load, matching the gateway (BASETEN_SWITCH_OAUTH_PROFILE defaults
	// to empty) and the baseten CLI's email-derived profile names.
	profile := ""
	host := auth.DefaultHost()
	forceRefresh := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--refresh":
			// Force a token refresh round trip even when the cached
			// access token is still valid, so health checks exercise
			// the real token endpoint deterministically.
			forceRefresh = true
		case args[i] == "--profile":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--profile requires a value")
				return 2
			}
			profile = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--profile="):
			profile = strings.TrimPrefix(args[i], "--profile=")
		case args[i] == "--host":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--host requires a value")
				return 2
			}
			host = strings.TrimRight(args[i+1], "/")
			i++
		case strings.HasPrefix(args[i], "--host="):
			host = strings.TrimRight(strings.TrimPrefix(args[i], "--host="), "/")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	buildClient := auth.HTTPClient
	if forceRefresh {
		buildClient = auth.HTTPClientForceRefresh
	}
	client, err := buildClient(ctx, profile, host)
	if err != nil {
		var ak *auth.APIKeyProfileError
		if errors.As(err, &ak) {
			if ak.Key == "" {
				fmt.Fprintf(os.Stderr, "profile %q uses API key auth, but the key could not be read (check the keyring or auth.json)\n", ak.Profile)
				return 1
			}
			fmt.Printf("Signed in with API key (profile %q)\n", ak.Profile)
			fmt.Println("Identity lookup requires OAuth; run 'baseten auth login' to switch, or inspect routing with 'baseten-switch status' and 'baseten-switch doctor --probe'.")
			return 0
		}
		if errors.Is(err, auth.ErrNotSignedIn) {
			fmt.Fprintln(os.Stderr, "not signed in (run 'baseten auth login')")
			return 3
		}
		fmt.Fprintf(os.Stderr, "whoami: %v\n", err)
		return 1
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, host+"/v1/users/me", nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "whoami: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "whoami: HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}
	var wa whoamiResponse
	if err := json.Unmarshal(body, &wa); err != nil {
		fmt.Fprintf(os.Stderr, "whoami: parse: %v\n", err)
		return 1
	}
	tok, _, _ := auth.Load(profile)
	expires := "(unknown)"
	if tok != nil {
		if exp, ok := jwtExpiry(tok.AccessToken); ok {
			expires = time.Unix(exp, 0).UTC().Format(time.RFC3339)
		}
	}
	profileLabel := profile
	if profileLabel == "" {
		profileLabel = "(current)"
	}
	fmt.Printf("Email:      %s\n", wa.Email)
	fmt.Printf("Workspace:  %s\n", wa.WorkspaceName)
	fmt.Printf("Expires at: %s\n", expires)
	fmt.Printf("Profile:    %s\n", profileLabel)
	return 0
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
