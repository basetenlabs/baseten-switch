package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/cmd/gateway"
	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
)

var authReloadHTTPTimeout = 5 * time.Second
var reloadRouterAuth = confirmRouterAuthReload

func confirmRouterAuthReload(path string) (bool, error) {
	state, pid := classifyPidfile(gatewayPidfilePath())
	if state != pidfileAlive {
		return false, nil
	}
	addr := envDefault("BASETEN_SWITCH_ADMIN_ADDR", gateway.DefaultAdminAddr)
	host, _, err := net.SplitHostPort(addr)
	if err != nil || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		return true, fmt.Errorf("auth reload requires a loopback admin listener")
	}
	client := &http.Client{Timeout: authReloadHTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	snapshot := func() (routingAdminStatus, error) {
		var status routingAdminStatus
		resp, err := client.Get("http://" + addr + "/v1/admin/status")
		if err != nil {
			return status, fmt.Errorf("could not read router identity")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return status, fmt.Errorf("router status returned HTTP %d", resp.StatusCode)
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&status); err != nil {
			return status, fmt.Errorf("invalid router identity response")
		}
		if err := validateManagedRouterIdentity(status, pid); err != nil {
			return status, fmt.Errorf("admin listener does not match the managed router")
		}
		if status.RouterBootID == "" || status.ConfigPath == "" || canonicalPath(status.ConfigPath) != canonicalPath(path) {
			return status, fmt.Errorf("admin listener does not match the selected config or lacks runtime identity")
		}
		return status, nil
	}
	before, err := snapshot()
	if err != nil {
		return true, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/admin/auth/reload", nil)
	if err != nil {
		return true, fmt.Errorf("could not create auth reload request")
	}
	req.Header.Set("X-Baseten-Switch-Admin", "1")
	resp, err := client.Do(req)
	if err != nil {
		return true, fmt.Errorf("router auth reload was not confirmed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true, fmt.Errorf("router auth reload returned HTTP %d", resp.StatusCode)
	}
	var receipt struct {
		Source      string  `json:"source"`
		Profile     string  `json:"profile"`
		AuthType    string  `json:"auth_type"`
		Health      string  `json:"health"`
		SavedAPIKey *bool   `json:"saved_api_key"`
		SignedIn    *bool   `json:"signed_in"`
		Revision    *uint64 `json:"revision"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 65536))
	if err := dec.Decode(&receipt); err != nil {
		return true, fmt.Errorf("invalid auth reload receipt")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return true, fmt.Errorf("invalid auth reload receipt")
	}
	if receipt.Revision == nil || receipt.SavedAPIKey == nil || receipt.SignedIn == nil || receipt.Health == "" {
		return true, fmt.Errorf("router does not expose a complete auth reload receipt")
	}
	expectedType, known := map[string]string{
		"saved_api_key": "api_key", "profile_api_key": "api_key",
		"profile_oauth": "oauth", "environment_fallback": "api_key", "none": "none",
	}[receipt.Source]
	if !known || receipt.AuthType != expectedType {
		return true, fmt.Errorf("unsupported auth reload receipt")
	}
	switch receipt.Health {
	case "ok", "signed_out", "refresh_failed", "error":
	default:
		return true, fmt.Errorf("unsupported auth reload receipt")
	}
	if receipt.Source != "environment_fallback" && *receipt.SignedIn != (receipt.Source != "none") {
		return true, fmt.Errorf("inconsistent auth reload receipt")
	}
	after, err := snapshot()
	if err != nil {
		return true, err
	}
	if before.RouterBootID != after.RouterBootID {
		return true, fmt.Errorf("router restarted during auth reload")
	}
	key, keyErr := auth.LoadSavedAPIKey(path)
	if keyErr != nil {
		return true, fmt.Errorf("saved credential could not be verified after reload")
	}
	if (key != "") != *receipt.SavedAPIKey || (key != "" && (receipt.Source != "saved_api_key" || receipt.AuthType != "api_key" || !*receipt.SignedIn)) || (key == "" && receipt.Source == "saved_api_key") {
		return true, fmt.Errorf("router did not confirm the stored credential selection")
	}
	if key == "" {
		if err := auth.CheckStore(); err != nil {
			return true, fmt.Errorf("CLI credential store could not be verified after reload")
		}
		token, _, loadErr := auth.LoadDetailed(receipt.Profile)
		var profileKey *auth.APIKeyProfileError
		expectedSource := "none"
		switch {
		case token != nil && loadErr == nil:
			expectedSource = "profile_oauth"
		case errors.As(loadErr, &profileKey):
			if profileKey.Key == "" {
				return true, fmt.Errorf("selected CLI API key could not be loaded")
			}
			expectedSource = "profile_api_key"
		case loadErr != nil:
			return true, fmt.Errorf("selected CLI credential could not be verified")
		}
		if receipt.Source != expectedSource && !(expectedSource == "none" && receipt.Source == "environment_fallback") {
			return true, fmt.Errorf("router did not confirm the selected CLI credential")
		}
	}
	return true, nil
}

func cmdAuthReload(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch auth reload")
		return 2
	}
	path, _ := resolveConfigPath()
	running, err := reloadRouterAuth(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Runtime credential application unconfirmed: %v. Retry 'baseten-switch auth reload'.\n", err)
		return 1
	}
	if !running {
		fmt.Fprintln(os.Stderr, "Router not running; stored credentials apply on the next start.")
		return 1
	}
	fmt.Fprintln(os.Stderr, "Router credentials reloaded and confirmed.")
	return 0
}
