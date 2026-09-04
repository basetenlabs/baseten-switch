package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetFallbackPolicyTrigger changes one global status trigger while preserving
// every byte outside the selected scalar or newly inserted policy subtree.
func SetFallbackPolicyTrigger(path, trigger string, enabled bool) error {
	key, err := fallbackPolicyKey(trigger)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return fmt.Errorf("%s: not a single-document YAML file", path)
	}
	_, global := mappingEntry(doc.Content[0], "global")
	if global == nil || global.Kind != yaml.MappingNode || global.Style == yaml.FlowStyle {
		return fmt.Errorf("%s: global must be an editable block mapping", path)
	}
	lines := bytes.SplitAfter(b, []byte("\n"))
	want := "false"
	if enabled {
		want = "true"
	}
	_, policy := mappingEntry(global, "fallback_policy")
	var edits []yamlEdit
	switch {
	case policy == nil:
		routingKey, routing := mappingEntry(global, "routing_enabled")
		anchorLine := global.Line
		indent := strings.Repeat(" ", global.Column-1+2)
		if routing != nil {
			anchorLine = maxNodeLine(routing)
			indent = strings.Repeat(" ", routingKey.Column-1)
		}
		edits = []yamlEdit{{
			line: anchorLine, insert: true,
			text: indent + "fallback_policy:\n" + indent + "  " + key + ": " + want,
		}}
	case policy.Kind != yaml.MappingNode || policy.Style == yaml.FlowStyle:
		return fmt.Errorf("%s: global.fallback_policy must be an editable block mapping", path)
	default:
		_, value := mappingEntry(policy, key)
		if value == nil {
			anchorLine, indent := mappingAppendAnchor(policy)
			edits = []yamlEdit{{line: anchorLine, insert: true, text: indent + key + ": " + want}}
		} else {
			n, lengthErr := scalarTokenLength(lines, value)
			if lengthErr != nil {
				return fmt.Errorf("%s: global.fallback_policy.%s: %w", path, key, lengthErr)
			}
			edits = []yamlEdit{{line: value.Line, col: value.Column, oldLen: n, text: want}}
		}
	}
	out, err := applyEdits(b, edits)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var check File
	if err := UnmarshalStrict(out, &check); err != nil {
		return fmt.Errorf("%s: edited config does not parse (edit aborted): %w", path, err)
	}
	resolved := ResolveFallbackPolicy(check.Global.FallbackPolicy)
	if key == "on_baseten_429" && resolved.OnBaseten429 != enabled ||
		key == "on_baseten_5xx" && resolved.OnBaseten5xx != enabled {
		return fmt.Errorf("%s: edited fallback policy did not set %s to %t (edit aborted)", path, key, enabled)
	}
	return writeFileAtomic(path, out, info.Mode().Perm())
}

func fallbackPolicyKey(trigger string) (string, error) {
	switch trigger {
	case "429":
		return "on_baseten_429", nil
	case "5xx":
		return "on_baseten_5xx", nil
	default:
		return "", fmt.Errorf("unsupported fallback trigger %q (allowed: 429, 5xx)", trigger)
	}
}

// SetClientNativeFallbackModel updates one client's Baseten-specific native
// target through the generic comment-preserving scalar editor.
func SetClientNativeFallbackModel(path, clientName, model string) error {
	file, err := Load(path)
	if err != nil {
		return err
	}
	client, ok := configClientByName(file, clientName)
	if !ok {
		return fmt.Errorf("%s: no client named %q", path, clientName)
	}
	check := *client
	check.NativeFallbackModel = model
	if err := ValidateNativeFallbackModel(check); err != nil {
		return err
	}
	return SetClientScalars(path, clientName, map[string]string{
		"native_fallback_model": model,
	})
}
