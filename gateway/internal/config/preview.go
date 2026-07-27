package config

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// PreviewPolicy is the complete isolation contract for a Preview config.
// Callers still enforce filesystem containment and ownership around the file;
// this type validates the parsed configuration values that can affect runtime
// paths, credentials, and listeners.
type PreviewPolicy struct {
	Root       string
	RouterAddr string
	DoorAddr   string
}

// BuildPreviewConfig parses a Stable config without expanding environment
// placeholders, creates a deep copy, and rewrites every isolation-sensitive
// field. Using the parsed representation means valid YAML alternatives such as
// quoted keys and inline mappings cannot bypass the transformation.
func BuildPreviewConfig(source *File, policy PreviewPolicy) (*File, error) {
	if source == nil {
		return nil, fmt.Errorf("preview config: nil source")
	}
	raw, err := yaml.Marshal(source)
	if err != nil {
		return nil, fmt.Errorf("preview config: clone source: %w", err)
	}
	var out File
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("preview config: clone source: %w", err)
	}

	out.Global.TelemetryDir = filepath.Join(filepath.Clean(policy.Root), "telemetry")
	if out.Global.Auth == nil {
		out.Global.Auth = map[string]string{}
	}
	for provider := range out.Global.Auth {
		switch strings.ToLower(strings.TrimSpace(provider)) {
		case "baseten":
			out.Global.Auth[provider] = "${BASETEN_API_KEY}"
		case "anthropic":
			out.Global.Auth[provider] = "${ANTHROPIC_API_KEY}"
		case "openai":
			out.Global.Auth[provider] = "${OPENAI_API_KEY}"
		default:
			out.Global.Auth[provider] = ""
		}
	}

	for i := range out.Clients {
		client := &out.Clients[i]
		client.BindAddr = policy.RouterAddr
		if client.AuthToken == nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(client.ProtocolShape)) {
		case "openai":
			client.AuthToken.Value = "${CODEX_AUTH_TOKEN}"
		case "", "anthropic":
			client.AuthToken.Value = "${ANTHROPIC_AUTH_TOKEN}"
		default:
			client.AuthToken.Value = ""
		}
	}
	if out.Door == nil {
		out.Door = &Door{}
	}
	out.Door.Ports = []DoorPort{{
		BindAddr:   policy.DoorAddr,
		RouterAddr: policy.RouterAddr,
	}}

	if err := ValidatePreviewConfig(&out, policy); err != nil {
		return nil, err
	}
	return &out, nil
}

// ValidatePreviewConfig checks the parsed, effective configuration contract.
// It intentionally rejects literal credentials. Preview credentials belong in
// the private Preview env/auth files, not in a Stable-derived config snapshot.
func ValidatePreviewConfig(file *File, policy PreviewPolicy) error {
	if file == nil {
		return fmt.Errorf("preview config: nil config")
	}
	root, err := filepath.Abs(policy.Root)
	if err != nil || !filepath.IsAbs(policy.Root) {
		return fmt.Errorf("preview config: root must be absolute")
	}
	if err := validatePreviewAddress("router", policy.RouterAddr); err != nil {
		return err
	}
	if err := validatePreviewAddress("door", policy.DoorAddr); err != nil {
		return err
	}
	if policy.RouterAddr == policy.DoorAddr {
		return fmt.Errorf("preview config: router and door addresses must differ")
	}

	wantTelemetry := filepath.Join(filepath.Clean(root), "telemetry")
	if filepath.Clean(file.Global.TelemetryDir) != wantTelemetry {
		return fmt.Errorf(
			"preview config: telemetry_dir %q must be %q",
			file.Global.TelemetryDir, wantTelemetry)
	}
	for provider, value := range file.Global.Auth {
		want := ""
		switch strings.ToLower(strings.TrimSpace(provider)) {
		case "baseten":
			want = "${BASETEN_API_KEY}"
		case "anthropic":
			want = "${ANTHROPIC_API_KEY}"
		case "openai":
			want = "${OPENAI_API_KEY}"
		}
		if value != "" && value != want {
			return fmt.Errorf(
				"preview config: global auth %q must be empty or use the Preview placeholder",
				provider)
		}
	}
	if len(file.Clients) == 0 {
		return fmt.Errorf("preview config: at least one client is required")
	}
	for _, client := range file.Clients {
		if client.BindAddr != policy.RouterAddr {
			return fmt.Errorf(
				"preview config: client %q bind_addr %q must be %q",
				client.Name, client.BindAddr, policy.RouterAddr)
		}
		if client.AuthToken == nil || client.AuthToken.Value == "" {
			continue
		}
		want := ""
		switch strings.ToLower(strings.TrimSpace(client.ProtocolShape)) {
		case "openai":
			want = "${CODEX_AUTH_TOKEN}"
		case "", "anthropic":
			want = "${ANTHROPIC_AUTH_TOKEN}"
		}
		if client.AuthToken.Value != want {
			return fmt.Errorf(
				"preview config: client %q auth token must be empty or use the Preview placeholder",
				client.Name)
		}
	}
	if file.Door == nil || len(file.Door.Ports) != 1 {
		return fmt.Errorf("preview config: exactly one door port is required")
	}
	port := file.Door.Ports[0]
	if port.BindAddr != policy.DoorAddr || port.RouterAddr != policy.RouterAddr {
		return fmt.Errorf(
			"preview config: door mapping must be %s -> %s",
			policy.DoorAddr, policy.RouterAddr)
	}
	if err := ValidateRoutingPolicy(file); err != nil {
		return fmt.Errorf("preview config: %w", err)
	}
	return nil
}

func validatePreviewAddress(component, address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("preview config: invalid %s address %q: %w", component, address, err)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("preview config: %s address must bind 127.0.0.1", component)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("preview config: invalid %s port %q", component, portText)
	}
	return nil
}
