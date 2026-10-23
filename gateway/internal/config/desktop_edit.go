package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

const ClaudeDesktopClientName = "claude-desktop"

// SetupClaudeDesktop creates the independent listener or enables its saved
// configuration. It never edits a different client or a client application file.
func SetupClaudeDesktop(path, bindAddr string) error {
	if err := validateDesktopBindAddress(bindAddr); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	var file File
	if err := UnmarshalStrict(raw, &file); err != nil {
		return err
	}
	if existing, ok := configClientByName(&file, ClaudeDesktopClientName); ok {
		if err := validateDesktopBindAddress(existing.BindAddr); err != nil {
			return err
		}
		if IsNativeAllowed(*existing) || existing.ProtocolShape != "anthropic" {
			return fmt.Errorf("existing Claude Desktop client must explicitly set allow_native: false and protocol_shape: anthropic")
		}
		if err := SetClientScalars(path, ClaudeDesktopClientName, map[string]string{"enabled": "true"}); err != nil {
			return err
		}
		picker := cloneModelPicker(existing.ModelPicker)
		if picker == nil {
			picker = &ModelPicker{}
		}
		picker.Enabled = true
		return SetClientModelPicker(path, ClaudeDesktopClientName, picker)
	}
	for _, client := range file.Clients {
		if normalizedLoopbackEndpoint(client.BindAddr) == normalizedLoopbackEndpoint(bindAddr) {
			return fmt.Errorf("Claude Desktop listener address is already assigned to client %q", client.Name)
		}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 {
		return fmt.Errorf("expected a single YAML document")
	}
	_, clients := mappingEntry(doc.Content[0], "clients")
	if clients == nil || clients.Kind != yaml.SequenceNode || clients.Style == yaml.FlowStyle || len(clients.Content) == 0 {
		return fmt.Errorf("Claude Desktop setup requires a nonempty block-style clients sequence")
	}
	allowNative := false
	client := Client{
		Name: ClaudeDesktopClientName, Enabled: true, BindAddr: bindAddr,
		ProtocolShape: "anthropic", AllowNative: &allowNative,
		ModelPicker: &ModelPicker{Enabled: true, Models: []ModelPickerModel{}},
	}
	block, err := yaml.Marshal([]Client{client})
	if err != nil {
		return err
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	last := yamlNodeLastLine(clients)
	block = indentYAMLBlock(block, clients.Column-1, lineEndingFor(lines[last-1]))
	if !bytes.HasSuffix(lines[last-1], []byte("\n")) {
		block = append([]byte("\n"), block...)
	}
	out, err := replaceYAMLLines(lines, last+1, last, block)
	if err != nil {
		return err
	}
	var check File
	if err := UnmarshalStrict(out, &check); err != nil {
		return err
	}
	if err := ValidateRoutingPolicy(&check); err != nil {
		return err
	}
	return writeFileAtomic(path, out, info.Mode().Perm())
}

func validateDesktopBindAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("Claude Desktop listener must use a loopback IP address")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("Claude Desktop listener has an invalid port")
	}
	return nil
}
