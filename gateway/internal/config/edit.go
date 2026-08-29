// edit.go implements comment-preserving targeted edits to gateway.yaml.
//
// Marshaling the parsed File struct (config.Save) strips every comment;
// even re-encoding a yaml.Node document tree drops blank lines and
// normalizes aligned values, so neither can round-trip the file. The
// targeted editors below locate scalar or map-entry tokens with yaml.Node,
// then splice new values into the original bytes. Every byte outside an
// edited token survives verbatim.
//
// config.Save remains for callers that legitimately regenerate the whole
// file.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// scalarValueRe is the set of values SetClientScalars will write. It
// admits "/" so raw Baseten slugs (e.g. "zai-org/GLM-5.2") are valid
// plain YAML scalars.
var scalarValueRe = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// mapKeyRe is the closed set of family keys SetClientMapEntries can write.
// Exact model IDs are intentionally unsupported.
var mapKeyRe = regexp.MustCompile(`^(fable|opus|sonnet|haiku)$`)

// yamlEdit is one pending single-line edit: either replace oldLen
// bytes at (line, col) with text, insert text as a new line after
// line, or delete the line at line. Lines and columns are 1-based,
// matching yaml.Node.
type yamlEdit struct {
	line       int
	col        int
	oldLen     int
	insert     bool
	removeLine bool
	text       string
}

// SetGlobalRoutingEnabled flips the required global routing gate while
// preserving every unrelated byte of gateway.yaml.
func SetGlobalRoutingEnabled(path string, enabled bool) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	var parsed File
	if err := UnmarshalStrict(b, &parsed); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if parsed.Global.RoutingEnabled != nil && *parsed.Global.RoutingEnabled == enabled {
		return nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return fmt.Errorf("%s: not a single-document YAML file", path)
	}
	_, global := mappingEntry(doc.Content[0], "global")
	if global == nil || global.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: no global mapping", path)
	}
	_, gate := mappingEntry(global, "routing_enabled")
	if gate == nil {
		return fmt.Errorf("%s: no global.routing_enabled scalar", path)
	}
	lines := bytes.SplitAfter(b, []byte("\n"))
	n, err := scalarTokenLength(lines, gate)
	if err != nil {
		return fmt.Errorf("%s: global.routing_enabled: %w", path, err)
	}
	want := "false"
	if enabled {
		want = "true"
	}
	out, err := applyEdits(b, []yamlEdit{{
		line: gate.Line, col: gate.Column, oldLen: n, text: want,
	}})
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var check File
	if err := UnmarshalStrict(out, &check); err != nil {
		return fmt.Errorf("%s: edited config does not parse (edit aborted): %w", path, err)
	}
	if check.Global.RoutingEnabled == nil || *check.Global.RoutingEnabled != enabled {
		return fmt.Errorf("%s: edited config did not set global.routing_enabled to %t (edit aborted)", path, enabled)
	}
	return writeFileAtomic(path, out, st.Mode().Perm())
}

// SetClientModelReasoningPolicy sets one client-scoped provider/model policy
// while preserving every byte outside that client's model_options subtree.
func SetClientModelReasoningPolicy(
	path string,
	clientName string,
	provider string,
	modelID string,
	policy ReasoningPolicy,
) error {
	if strings.TrimSpace(clientName) == "" {
		return fmt.Errorf("reasoning client name cannot be empty")
	}
	if err := validateReasoningEditTarget(provider, modelID); err != nil {
		return err
	}
	if policy.Effort != "" && !scalarValueRe.MatchString(policy.Effort) {
		return fmt.Errorf(
			"invalid reasoning effort %q for %s/%s",
			policy.Effort,
			provider,
			modelID,
		)
	}
	if err := validateModelOptions(
		ModelOptions{
			provider: {modelID: ModelOption{Reasoning: &policy}},
		},
		fmt.Sprintf("clients[%q].model_options", clientName),
	); err != nil {
		return err
	}
	return editClientModelReasoningPolicy(
		path,
		clientName,
		provider,
		modelID,
		&policy,
	)
}

// RemoveClientModelReasoningPolicy removes one client-scoped override. Empty
// provider and model_options mappings are pruned.
func RemoveClientModelReasoningPolicy(
	path string,
	clientName string,
	provider string,
	modelID string,
) error {
	if strings.TrimSpace(clientName) == "" {
		return fmt.Errorf("reasoning client name cannot be empty")
	}
	if err := validateReasoningEditTarget(provider, modelID); err != nil {
		return err
	}
	return editClientModelReasoningPolicy(
		path,
		clientName,
		provider,
		modelID,
		nil,
	)
}

func validateReasoningEditTarget(provider, modelID string) error {
	if provider != "baseten" {
		return fmt.Errorf("unsupported reasoning provider %q (allowed: baseten)", provider)
	}
	if strings.TrimSpace(modelID) == "" {
		return fmt.Errorf("reasoning model id cannot be empty")
	}
	if strings.ContainsAny(modelID, "\r\n\x00") {
		return fmt.Errorf("reasoning model id %q contains an unsupported character", modelID)
	}
	return nil
}

// SetClientModelPicker replaces or removes one client's ordered model_picker
// subtree while preserving every byte outside that subtree. Existing row
// nodes are reused by alias so row-attached comments move with reordered rows.
// A nil picker removes the subtree. The complete edited file is strictly
// parsed and routing-validated before the original is atomically replaced.
func SetClientModelPicker(path, clientName string, picker *ModelPicker) error {
	if strings.TrimSpace(clientName) == "" {
		return fmt.Errorf("model picker client name cannot be empty")
	}
	if picker != nil {
		for i, model := range picker.Models {
			if !scalarValueRe.MatchString(model.Alias) {
				return fmt.Errorf(
					"invalid model picker alias %q at index %d on client %q",
					model.Alias,
					i,
					clientName,
				)
			}
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	var parsed File
	if err := UnmarshalStrict(b, &parsed); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if err := ValidateRoutingPolicy(&parsed); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	client, ok := configClientByName(&parsed, clientName)
	if !ok {
		return fmt.Errorf("%s: no client named %q", path, clientName)
	}
	currentPicker := cloneModelPicker(client.ModelPicker)
	client.ModelPicker = cloneModelPicker(picker)
	if err := ValidateRoutingPolicy(&parsed); err != nil {
		return fmt.Errorf("client %q model_picker: %w", clientName, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	item, nameKey, nameVal, err := clientMappingNode(&doc, clientName)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if item.Style == yaml.FlowStyle {
		return fmt.Errorf(
			"%s: client %q uses a flow-style mapping; edit model_picker by hand",
			path,
			clientName,
		)
	}
	pickerKey, pickerValue := mappingEntry(item, "model_picker")
	if pickerValue == nil && picker == nil {
		return nil
	}
	if pickerValue != nil {
		if pickerValue.Kind != yaml.MappingNode || pickerValue.Tag == "!!null" {
			return fmt.Errorf(
				"%s: client %q model_picker is not a block mapping",
				path,
				clientName,
			)
		}
		if pickerValue.Style == yaml.FlowStyle {
			return fmt.Errorf(
				"%s: client %q model_picker is flow-style; edit it by hand",
				path,
				clientName,
			)
		}
	}

	var replacement []byte
	if picker != nil {
		replacement, err = renderClientModelPicker(pickerKey, pickerValue, picker)
		if err != nil {
			return fmt.Errorf("%s: client %q model_picker: %w", path, clientName, err)
		}
		if modelPickersEqual(currentPicker, picker) {
			return nil
		}
	}
	lines := bytes.SplitAfter(b, []byte("\n"))
	var out []byte
	if pickerValue == nil {
		anchorKey, anchorLine := mapBlockAnchor(item, nameKey, nameVal)
		indent := anchorKey.Column - 1
		replacement = indentYAMLBlock(replacement, indent, lineEndingFor(lines[anchorLine-1]))
		out, err = replaceYAMLLines(lines, anchorLine+1, anchorLine, replacement)
	} else {
		endLine := yamlNodeLastLine(pickerValue)
		if endLine < pickerKey.Line {
			endLine = pickerKey.Line
		}
		if picker != nil {
			replacement = indentYAMLBlock(
				replacement,
				pickerKey.Column-1,
				lineEndingFor(lines[pickerKey.Line-1]),
			)
		}
		out, err = replaceYAMLLines(lines, pickerKey.Line, endLine, replacement)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	var check File
	if err := UnmarshalStrict(out, &check); err != nil {
		return fmt.Errorf(
			"%s: edited config does not parse (edit aborted): %w",
			path,
			err,
		)
	}
	if err := ValidateRoutingPolicy(&check); err != nil {
		return fmt.Errorf(
			"%s: edited config is invalid (edit aborted): %w",
			path,
			err,
		)
	}
	checkClient, ok := configClientByName(&check, clientName)
	if !ok || !modelPickersEqual(checkClient.ModelPicker, picker) {
		return fmt.Errorf(
			"%s: edited client %q model_picker does not match requested state (edit aborted)",
			path,
			clientName,
		)
	}
	if bytes.Equal(out, b) {
		return nil
	}
	return writeFileAtomic(path, out, st.Mode().Perm())
}

func cloneModelPicker(picker *ModelPicker) *ModelPicker {
	if picker == nil {
		return nil
	}
	clone := &ModelPicker{Enabled: picker.Enabled}
	clone.Models = append([]ModelPickerModel(nil), picker.Models...)
	return clone
}

func modelPickersEqual(a, b *ModelPicker) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Enabled != b.Enabled || len(a.Models) != len(b.Models) {
		return false
	}
	for i := range a.Models {
		if a.Models[i] != b.Models[i] {
			return false
		}
	}
	return true
}

func clientMappingNode(doc *yaml.Node, clientName string) (*yaml.Node, *yaml.Node, *yaml.Node, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, nil, nil, fmt.Errorf("not a single-document YAML file")
	}
	_, clients := mappingEntry(doc.Content[0], "clients")
	if clients == nil || clients.Kind != yaml.SequenceNode {
		return nil, nil, nil, fmt.Errorf("no clients sequence")
	}
	for _, item := range clients.Content {
		nameKey, nameVal := mappingEntry(item, "name")
		if nameVal != nil && nameVal.Kind == yaml.ScalarNode && nameVal.Value == clientName {
			return item, nameKey, nameVal, nil
		}
	}
	return nil, nil, nil, fmt.Errorf("no client named %q", clientName)
}

func renderClientModelPicker(
	pickerKey *yaml.Node,
	pickerValue *yaml.Node,
	picker *ModelPicker,
) ([]byte, error) {
	var key, value *yaml.Node
	if pickerValue == nil {
		key = yamlScalar("model_picker", "!!str")
		value = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	} else {
		key = cloneYAMLNode(pickerKey)
		// A comment immediately before model_picker belongs to the surrounding
		// client block and lies outside the replaced line range.
		key.HeadComment = ""
		value = cloneYAMLNode(pickerValue)
	}

	enabledKey, enabledValue := mappingEntry(value, "enabled")
	if enabledValue == nil {
		enabledKey = yamlScalar("enabled", "!!str")
		enabledValue = yamlScalar(strconv.FormatBool(picker.Enabled), "!!bool")
	} else {
		enabledValue.Kind = yaml.ScalarNode
		enabledValue.Tag = "!!bool"
		enabledValue.Style = 0
		enabledValue.Value = strconv.FormatBool(picker.Enabled)
	}

	modelsKey, modelsValue := mappingEntry(value, "models")
	if modelsValue == nil {
		modelsKey = yamlScalar("models", "!!str")
		modelsValue = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	} else if modelsValue.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("models is not a sequence")
	} else if modelsValue.Style == yaml.FlowStyle && len(modelsValue.Content) > 0 {
		return nil, fmt.Errorf("models is a non-empty flow-style sequence")
	}

	existing := make(map[string]*yaml.Node, len(modelsValue.Content))
	for i, row := range modelsValue.Content {
		if row.Kind != yaml.MappingNode || row.Style == yaml.FlowStyle {
			return nil, fmt.Errorf("models[%d] is not a block mapping", i)
		}
		_, aliasValue := mappingEntry(row, "alias")
		if aliasValue == nil || aliasValue.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("models[%d].alias is not a scalar", i)
		}
		if _, duplicate := existing[aliasValue.Value]; duplicate {
			return nil, fmt.Errorf("alias %q is duplicated", aliasValue.Value)
		}
		existing[aliasValue.Value] = row
	}

	modelsValue.Content = nil
	for _, model := range picker.Models {
		row := cloneYAMLNode(existing[model.Alias])
		if row == nil {
			row = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		setYAMLMappingString(row, "alias", model.Alias, false)
		modelsValue.Content = append(modelsValue.Content, row)
	}
	if len(modelsValue.Content) == 0 {
		modelsValue.Style = yaml.FlowStyle
	} else {
		modelsValue.Style = 0
	}

	// The model_picker schema is intentionally narrow. Rebuild the pair order
	// deterministically while retaining comments carried by existing nodes.
	value.Content = []*yaml.Node{enabledKey, enabledValue, modelsKey, modelsValue}
	root := &yaml.Node{
		Kind: yaml.DocumentNode,
		Content: []*yaml.Node{{
			Kind:    yaml.MappingNode,
			Tag:     "!!map",
			Content: []*yaml.Node{key, value},
		}},
	}
	var rendered bytes.Buffer
	enc := yaml.NewEncoder(&rendered)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	return rendered.Bytes(), nil
}

func yamlScalar(value, tag string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	clone := *node
	clone.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		clone.Content[i] = cloneYAMLNode(child)
	}
	return &clone
}

func setYAMLMappingString(mapping *yaml.Node, key, value string, omitEmpty bool) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		if omitEmpty && value == "" {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
		node := mapping.Content[i+1]
		node.Kind = yaml.ScalarNode
		node.Tag = "!!str"
		node.Style = 0
		node.Value = value
		return
	}
	if omitEmpty && value == "" {
		return
	}
	mapping.Content = append(
		mapping.Content,
		yamlScalar(key, "!!str"),
		yamlScalar(value, "!!str"),
	)
}

func yamlNodeLastLine(node *yaml.Node) int {
	if node == nil {
		return 0
	}
	last := node.Line
	for _, child := range node.Content {
		if childLast := yamlNodeLastLine(child); childLast > last {
			last = childLast
		}
	}
	return last
}

func lineEndingFor(line []byte) []byte {
	if bytes.HasSuffix(line, []byte("\r\n")) {
		return []byte("\r\n")
	}
	return []byte("\n")
}

func indentYAMLBlock(block []byte, spaces int, lineEnding []byte) []byte {
	if len(block) == 0 {
		return nil
	}
	block = bytes.ReplaceAll(block, []byte("\r\n"), []byte("\n"))
	block = bytes.TrimSuffix(block, []byte("\n"))
	indent := bytes.Repeat([]byte(" "), spaces)
	rows := bytes.Split(block, []byte("\n"))
	var out bytes.Buffer
	for _, row := range rows {
		out.Write(indent)
		out.Write(row)
		out.Write(lineEnding)
	}
	return out.Bytes()
}

// replaceYAMLLines replaces the inclusive 1-based range start..end. When end
// is start-1 the replacement is inserted immediately before start.
func replaceYAMLLines(lines [][]byte, start, end int, replacement []byte) ([]byte, error) {
	if start < 1 || start > len(lines)+1 || end < start-1 || end > len(lines) {
		return nil, fmt.Errorf("line range %d..%d is out of bounds", start, end)
	}
	var out bytes.Buffer
	for _, line := range lines[:start-1] {
		out.Write(line)
	}
	out.Write(replacement)
	for _, line := range lines[end:] {
		out.Write(line)
	}
	return out.Bytes(), nil
}

func editClientModelReasoningPolicy(
	path string,
	clientName string,
	provider string,
	modelID string,
	policy *ReasoningPolicy,
) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	var parsed File
	if err := UnmarshalStrict(b, &parsed); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if err := ValidateRoutingPolicy(&parsed); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	client, ok := configClientByName(&parsed, clientName)
	if !ok {
		return fmt.Errorf("%s: no client named %q", path, clientName)
	}
	if current, exists := modelReasoningPolicy(
		client.ModelOptions,
		provider,
		modelID,
	); policy == nil {
		if !exists {
			return nil
		}
	} else if exists && current == *policy {
		return nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	edits, err := clientModelReasoningPolicyEdits(
		&doc,
		bytes.SplitAfter(b, []byte("\n")),
		clientName,
		provider,
		modelID,
		policy,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	out, err := applyEdits(b, edits)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var check File
	if err := UnmarshalStrict(out, &check); err != nil {
		return fmt.Errorf(
			"%s: edited config does not parse (edit aborted): %w",
			path,
			err,
		)
	}
	if err := ValidateRoutingPolicy(&check); err != nil {
		return fmt.Errorf(
			"%s: edited config is invalid (edit aborted): %w",
			path,
			err,
		)
	}
	checkClient, ok := configClientByName(&check, clientName)
	if !ok {
		return fmt.Errorf(
			"%s: edited config has no client named %q (edit aborted)",
			path,
			clientName,
		)
	}
	got, exists := modelReasoningPolicy(
		checkClient.ModelOptions,
		provider,
		modelID,
	)
	if policy == nil {
		if exists {
			return fmt.Errorf(
				"%s: edited client %q still has reasoning policy for %s/%s (edit aborted)",
				path,
				clientName,
				provider,
				modelID,
			)
		}
	} else if !exists || got != *policy {
		return fmt.Errorf(
			"%s: edited client %q reasoning policy for %s/%s is %#v, want %#v (edit aborted)",
			path,
			clientName,
			provider,
			modelID,
			got,
			*policy,
		)
	}
	return writeFileAtomic(path, out, st.Mode().Perm())
}

func configClientByName(file *File, name string) (*Client, bool) {
	for i := range file.Clients {
		if file.Clients[i].Name == name {
			return &file.Clients[i], true
		}
	}
	return nil, false
}

func modelReasoningPolicy(options ModelOptions, provider, modelID string) (ReasoningPolicy, bool) {
	models, ok := options[provider]
	if !ok {
		return ReasoningPolicy{}, false
	}
	option, ok := models[modelID]
	if !ok || option.Reasoning == nil {
		return ReasoningPolicy{}, false
	}
	return *option.Reasoning, true
}

func clientModelReasoningPolicyEdits(
	doc *yaml.Node,
	lines [][]byte,
	clientName string,
	provider string,
	modelID string,
	policy *ReasoningPolicy,
) ([]yamlEdit, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("not a single-document YAML file")
	}
	_, clients := mappingEntry(doc.Content[0], "clients")
	if clients == nil || clients.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("no clients sequence")
	}
	var client *yaml.Node
	for _, candidate := range clients.Content {
		_, name := mappingEntry(candidate, "name")
		if name != nil &&
			name.Kind == yaml.ScalarNode &&
			name.Value == clientName {
			client = candidate
			break
		}
	}
	if client == nil {
		return nil, fmt.Errorf("no client named %q", clientName)
	}
	return modelReasoningPolicyEditsInScope(
		client,
		fmt.Sprintf("client %q", clientName),
		lines,
		provider,
		modelID,
		policy,
	)
}

func modelReasoningPolicyEditsInScope(
	scope *yaml.Node,
	scopeLabel string,
	lines [][]byte,
	provider string,
	modelID string,
	policy *ReasoningPolicy,
) ([]yamlEdit, error) {
	if scope.Style == yaml.FlowStyle {
		return nil, fmt.Errorf(
			"cannot edit %s.model_options in a flow-style mapping",
			scopeLabel,
		)
	}
	optionsKey, options := mappingEntry(scope, "model_options")
	if options == nil {
		if policy == nil {
			return nil, nil
		}
		anchorLine, indent := mappingAppendAnchor(scope)
		text := indent + "model_options:\n" +
			indent + "  " + provider + ":\n" +
			indent + "    " + yamlQuotedKey(modelID) + ":\n" +
			indent + "      reasoning:\n" +
			indent + "        mode: " + string(policy.Mode)
		if policy.Effort != "" {
			text += "\n" + indent + "        effort: " + policy.Effort
		}
		return []yamlEdit{{line: anchorLine, insert: true, text: text}}, nil
	}
	if options.Kind == yaml.ScalarNode && options.Tag == "!!null" {
		if policy == nil {
			return nil, nil
		}
		indent := strings.Repeat(" ", optionsKey.Column-1)
		text := indent + "  " + provider + ":\n" +
			indent + "    " + yamlQuotedKey(modelID) + ":\n" +
			indent + "      reasoning:\n" +
			indent + "        mode: " + string(policy.Mode)
		if policy.Effort != "" {
			text += "\n" + indent + "        effort: " + policy.Effort
		}
		clear, err := clearInlineYAMLValueEdit(lines, options)
		if err != nil {
			return nil, err
		}
		return []yamlEdit{clear, {line: optionsKey.Line, insert: true, text: text}}, nil
	}
	if options.Kind == yaml.MappingNode && options.Style == yaml.FlowStyle && len(options.Content) == 0 {
		if policy == nil {
			return nil, nil
		}
		indent := strings.Repeat(" ", optionsKey.Column-1)
		text := indent + "  " + provider + ":\n" +
			indent + "    " + yamlQuotedKey(modelID) + ":\n" +
			indent + "      reasoning:\n" +
			indent + "        mode: " + string(policy.Mode)
		if policy.Effort != "" {
			text += "\n" + indent + "        effort: " + policy.Effort
		}
		clear, err := clearInlineYAMLValueEdit(lines, options)
		if err != nil {
			return nil, err
		}
		return []yamlEdit{clear, {line: optionsKey.Line, insert: true, text: text}}, nil
	}
	if options.Kind != yaml.MappingNode || options.Style == yaml.FlowStyle {
		return nil, fmt.Errorf(
			"%s.model_options is not an editable block mapping",
			scopeLabel,
		)
	}
	providerKey, providerNode := mappingEntry(options, provider)
	if providerNode == nil {
		if policy == nil {
			return nil, nil
		}
		anchorLine, indent := mappingAppendAnchor(options)
		text := indent + provider + ":\n" +
			indent + "  " + yamlQuotedKey(modelID) + ":\n" +
			indent + "    reasoning:\n" +
			indent + "      mode: " + string(policy.Mode)
		if policy.Effort != "" {
			text += "\n" + indent + "      effort: " + policy.Effort
		}
		return []yamlEdit{{line: anchorLine, insert: true, text: text}}, nil
	}
	if providerNode.Kind == yaml.MappingNode && providerNode.Style == yaml.FlowStyle && len(providerNode.Content) == 0 {
		if policy == nil {
			return nil, nil
		}
		indent := strings.Repeat(" ", providerKey.Column-1)
		text := indent + "  " + yamlQuotedKey(modelID) + ":\n" +
			indent + "    reasoning:\n" +
			indent + "      mode: " + string(policy.Mode)
		if policy.Effort != "" {
			text += "\n" + indent + "      effort: " + policy.Effort
		}
		clear, err := clearInlineYAMLValueEdit(lines, providerNode)
		if err != nil {
			return nil, err
		}
		return []yamlEdit{clear, {line: providerKey.Line, insert: true, text: text}}, nil
	}
	if providerNode.Kind != yaml.MappingNode || providerNode.Style == yaml.FlowStyle {
		return nil, fmt.Errorf(
			"%s.model_options.%s is not an editable block mapping",
			scopeLabel,
			provider,
		)
	}
	modelKey, modelNode := mappingEntry(providerNode, modelID)
	if modelNode == nil {
		if policy == nil {
			return nil, nil
		}
		anchorLine, indent := mappingAppendAnchor(providerNode)
		text := indent + yamlQuotedKey(modelID) + ":\n" +
			indent + "  reasoning:\n" +
			indent + "    mode: " + string(policy.Mode)
		if policy.Effort != "" {
			text += "\n" + indent + "    effort: " + policy.Effort
		}
		return []yamlEdit{{line: anchorLine, insert: true, text: text}}, nil
	}
	if policy == nil {
		first, last := modelKey.Line, maxNodeLine(modelNode)
		if len(providerNode.Content) == 2 {
			first = providerKey.Line
			if len(options.Content) == 2 {
				first = optionsKey.Line
			}
		}
		edits := make([]yamlEdit, 0, last-first+1)
		for line := first; line <= last; line++ {
			edits = append(edits, yamlEdit{line: line, removeLine: true})
		}
		return edits, nil
	}
	if modelNode.Kind == yaml.MappingNode && modelNode.Style == yaml.FlowStyle && len(modelNode.Content) == 0 {
		indent := strings.Repeat(" ", modelKey.Column-1)
		text := indent + "  reasoning:\n" +
			indent + "    mode: " + string(policy.Mode)
		if policy.Effort != "" {
			text += "\n" + indent + "    effort: " + policy.Effort
		}
		clear, err := clearInlineYAMLValueEdit(lines, modelNode)
		if err != nil {
			return nil, err
		}
		return []yamlEdit{clear, {line: modelKey.Line, insert: true, text: text}}, nil
	}
	if modelNode.Kind != yaml.MappingNode || modelNode.Style == yaml.FlowStyle {
		return nil, fmt.Errorf(
			"%s.model_options.%s.%s is not an editable block mapping",
			scopeLabel,
			provider,
			modelID,
		)
	}
	reasoningKey, reasoning := mappingEntry(modelNode, "reasoning")
	if reasoning == nil {
		anchorLine, indent := mappingAppendAnchor(modelNode)
		text := indent + "reasoning:\n" + indent + "  mode: " + string(policy.Mode)
		if policy.Effort != "" {
			text += "\n" + indent + "  effort: " + policy.Effort
		}
		return []yamlEdit{{line: anchorLine, insert: true, text: text}}, nil
	}
	if reasoning.Kind != yaml.MappingNode || reasoning.Style == yaml.FlowStyle {
		return nil, fmt.Errorf(
			"%s.model_options.%s.%s.reasoning is not an editable block mapping",
			scopeLabel,
			provider,
			modelID,
		)
	}
	_, modeNode := mappingEntry(reasoning, "mode")
	if modeNode == nil {
		return nil, fmt.Errorf(
			"%s.model_options.%s.%s.reasoning has no mode",
			scopeLabel,
			provider,
			modelID,
		)
	}
	modeLen, err := scalarTokenLength(lines, modeNode)
	if err != nil {
		return nil, err
	}
	edits := []yamlEdit{{line: modeNode.Line, col: modeNode.Column, oldLen: modeLen, text: string(policy.Mode)}}
	effortKey, effortNode := mappingEntry(reasoning, "effort")
	switch {
	case policy.Effort != "" && effortNode != nil:
		n, err := scalarTokenLength(lines, effortNode)
		if err != nil {
			return nil, err
		}
		edits = append(edits, yamlEdit{line: effortNode.Line, col: effortNode.Column, oldLen: n, text: policy.Effort})
	case policy.Effort != "":
		indent := strings.Repeat(" ", reasoningKey.Column-1+2)
		edits = append(edits, yamlEdit{line: maxNodeLine(reasoning), insert: true, text: indent + "effort: " + policy.Effort})
	case effortNode != nil:
		edits = append(edits, yamlEdit{line: effortKey.Line, removeLine: true})
	}
	return edits, nil
}

func mappingAppendAnchor(mapping *yaml.Node) (int, string) {
	if mapping.Kind != yaml.MappingNode || len(mapping.Content) == 0 {
		return mapping.Line, strings.Repeat(" ", mapping.Column-1+2)
	}
	lastKey := mapping.Content[len(mapping.Content)-2]
	lastVal := mapping.Content[len(mapping.Content)-1]
	return maxNodeLine(lastVal), strings.Repeat(" ", lastKey.Column-1)
}

func maxNodeLine(node *yaml.Node) int {
	if node == nil {
		return 0
	}
	last := node.Line
	for _, child := range node.Content {
		if childLast := maxNodeLine(child); childLast > last {
			last = childLast
		}
	}
	return last
}

func yamlQuotedKey(value string) string {
	return strconv.Quote(value)
}

func clearInlineYAMLValueEdit(lines [][]byte, node *yaml.Node) (yamlEdit, error) {
	if node.Line < 1 || node.Line > len(lines) || node.Column < 1 {
		return yamlEdit{}, fmt.Errorf("inline YAML value position %d:%d is invalid", node.Line, node.Column)
	}
	rest := lines[node.Line-1][node.Column-1:]
	var oldLen int
	switch {
	case node.Kind == yaml.ScalarNode && node.Tag == "!!null":
		n, err := scalarTokenLength(lines, node)
		if err != nil {
			return yamlEdit{}, err
		}
		oldLen = n
	case node.Kind == yaml.MappingNode && node.Style == yaml.FlowStyle && len(node.Content) == 0:
		if len(rest) < 2 || string(rest[:2]) != "{}" {
			return yamlEdit{}, fmt.Errorf("empty flow mapping token not found at %d:%d", node.Line, node.Column)
		}
		oldLen = 2
	default:
		return yamlEdit{}, fmt.Errorf("value at %d:%d is not an empty inline mapping or null", node.Line, node.Column)
	}
	start := node.Column - 1
	line := lines[node.Line-1]
	if oldLen == 0 && start < len(line) && line[start] == '#' {
		return yamlEdit{line: node.Line, col: node.Column}, nil
	}
	for start > 0 && (line[start-1] == ' ' || line[start-1] == '\t') {
		start--
	}
	return yamlEdit{
		line: node.Line,
		col:  start + 1, oldLen: oldLen + (node.Column - 1 - start),
	}, nil
}

// SetClientScalars rewrites scalar key/value pairs on the named client
// in the gateway.yaml at path, preserving every other byte of the file.
// It is the comment-preserving sibling of SetClientMapEntries for arbitrary
// client-block scalars (default_model, subagent_model, subagent_routing,
// enabled, ...).
// The enabled key is bool-valued: only the literal tokens "true" and
// "false" are accepted, and the post-edit verify reads the parsed bool
// back through the same path as the string fields. For each
// key -> value pair, the named client gets its existing scalar replaced
// in place (quoting style and inline comments preserved), or a new
// "key: value" line inserted when the key is absent. Insert anchor
// priority: after the LAST line of the model_aliases block when that key
// exists on the client, else after protocol_shape, else after name; the
// anchor key's column sets the indentation. Keys are processed in
// sorted order for deterministic output. The file is rewritten
// atomically with its original permission bits. A missing client,
// malformed YAML, a value that fails scalarValueRe, or a file whose
// edited form does not verify back to the requested values is an error
// and leaves the file untouched.
func SetClientScalars(path string, clientName string, set map[string]string) error {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := set[k]
		if !scalarValueRe.MatchString(v) {
			return fmt.Errorf("invalid value %q for key %q on client %q", v, k, clientName)
		}
		// enabled parses into a bool field; any other token would fail
		// the post-edit verify parse, so refuse it up front with a
		// clearer error.
		if k == "enabled" && v != "true" && v != "false" {
			return fmt.Errorf("invalid value %q for key %q on client %q (must be true or false)", v, k, clientName)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	edits, err := clientScalarEdits(&doc, bytes.SplitAfter(b, []byte("\n")), clientName, set, keys)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	out, err := applyEdits(b, edits)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// Verify the edit before writing: the edited bytes must parse and
	// carry exactly the requested values, so a splice bug can never
	// corrupt the live config.
	var check File
	if err := UnmarshalStrict(out, &check); err != nil {
		return fmt.Errorf("%s: edited config does not parse (edit aborted): %w", path, err)
	}
	found := false
	for i := range check.Clients {
		c := &check.Clients[i]
		if c.Name != clientName {
			continue
		}
		found = true
		if err := scalarVerify(c, set); err != nil {
			return fmt.Errorf("%s: %w (edit aborted)", path, err)
		}
	}
	if !found {
		return fmt.Errorf("%s: no client named %q (edit aborted)", path, clientName)
	}
	return writeFileAtomic(path, out, st.Mode().Perm())
}

// SetClientMapEntries rewrites the entries of a single map-valued key
// (mapKey, e.g. "model_routes") on the named client in the gateway.yaml
// at path, preserving every other byte of the file. It is the
// comment-preserving sibling of SetClientScalars for client-block maps.
// For each entry key -> value pair, the named client gets its existing
// entry value replaced in place (quoting style and inline comments
// preserved) or a new "key: value" line inserted at the end of the
// block. The block is created when absent, anchored after the
// model_aliases block, else after subagent_routing, else after
// subagent_model, else after protocol_shape, else after name. New
// entries are inserted in sorted-key deterministic order. A bare
// "mapKey:" line with a null value is treated as an empty mapping: the
// entries are inserted as its first children. The file is rewritten
// atomically with its original permission bits. A missing client,
// malformed YAML, a key or value that fails its regex, a flow-style
// mapping (whether the client block or the map value itself), or a
// file whose edited form does not verify back to the requested entries
// is an error and leaves the file untouched.
func SetClientMapEntries(path, clientName, mapKey string, entries map[string]string) error {
	if len(entries) == 0 {
		return nil
	}
	for k, v := range entries {
		if !mapKeyRe.MatchString(k) {
			return fmt.Errorf("invalid map key %q for map %q on client %q", k, mapKey, clientName)
		}
		if !scalarValueRe.MatchString(v) {
			return fmt.Errorf("invalid value %q for key %q in map %q on client %q", v, k, mapKey, clientName)
		}
	}
	return setClientMapEntries(path, clientName, mapKey, entries)
}

// SetClientModelAliases adds or replaces alias-to-slug entries in one
// client's model_aliases mapping. Existing entries not named in entries are
// preserved. Alias keys and slugs must be safe plain YAML scalars; the edit is
// comment-preserving, post-validated, and atomically installed.
func SetClientModelAliases(path, clientName string, entries map[string]string) error {
	if len(entries) == 0 {
		return nil
	}
	for alias, slug := range entries {
		if !scalarValueRe.MatchString(alias) {
			return fmt.Errorf("invalid model alias %q on client %q", alias, clientName)
		}
		if !scalarValueRe.MatchString(slug) || !strings.Contains(slug, "/") {
			return fmt.Errorf("invalid Baseten slug %q for alias %q on client %q", slug, alias, clientName)
		}
	}
	return setClientMapEntries(path, clientName, "model_aliases", entries)
}

func setClientMapEntries(path, clientName, mapKey string, entries map[string]string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	edits, err := clientMapEdits(&doc, bytes.SplitAfter(b, []byte("\n")), clientName, mapKey, entries, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	out, err := applyEdits(b, edits)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// Verify the edit before writing: the edited bytes must parse and
	// carry exactly the requested entries, so a splice bug can never
	// corrupt the live config.
	var check File
	if err := UnmarshalStrict(out, &check); err != nil {
		return fmt.Errorf("%s: edited config does not parse (edit aborted): %w", path, err)
	}
	found := false
	for i := range check.Clients {
		c := &check.Clients[i]
		if c.Name != clientName {
			continue
		}
		found = true
		if err := mapEntriesVerify(c, mapKey, entries, nil); err != nil {
			return fmt.Errorf("%s: %w (edit aborted)", path, err)
		}
	}
	if !found {
		return fmt.Errorf("%s: no client named %q (edit aborted)", path, clientName)
	}
	return writeFileAtomic(path, out, st.Mode().Perm())
}

// RemoveClientMapEntries deletes the named entry keys from a single
// map-valued key (mapKey, e.g. "model_routes") on the named client in
// the gateway.yaml at path, preserving every other byte of the file.
// Removing a key that is absent is a no-op, not an error, as is
// removing from a bare "mapKey:" line with a null value. When the
// block would become empty, the mapKey line itself is removed too (an
// empty "mapKey:" line is invalid YAML for a non-null mapping and
// would fail load), along with any comment-only lines that were inside
// the block and the contiguous run of comment-only lines directly
// above the mapKey line. The file is rewritten atomically with its
// original permission bits. A missing client, malformed YAML, or a
// flow-style mapping (whether the client block or the map value
// itself) is an error and leaves the file untouched.
func RemoveClientMapEntries(path, clientName, mapKey string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	// Dedup so the verify step and the empty-block check are exact.
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	edits, err := clientMapEdits(&doc, bytes.SplitAfter(b, []byte("\n")), clientName, mapKey, nil, want)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	out, err := applyEdits(b, edits)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// Verify the edit before writing: the edited bytes must parse and
	// none of the requested keys may remain.
	var check File
	if err := UnmarshalStrict(out, &check); err != nil {
		return fmt.Errorf("%s: edited config does not parse (edit aborted): %w", path, err)
	}
	found := false
	for i := range check.Clients {
		c := &check.Clients[i]
		if c.Name != clientName {
			continue
		}
		found = true
		if err := mapEntriesVerify(c, mapKey, nil, want); err != nil {
			return fmt.Errorf("%s: %w (edit aborted)", path, err)
		}
	}
	if !found {
		return fmt.Errorf("%s: no client named %q (edit aborted)", path, clientName)
	}
	return writeFileAtomic(path, out, st.Mode().Perm())
}

// mapEntriesVerify confirms the parsed client's map field carries
// exactly the requested set state: every set entry key -> value is
// present with that value, and every remove key is absent. Only the
// map fields the editors are built for are checked; an unknown mapKey
// is a caller bug and fails loudly.
func mapEntriesVerify(c *Client, mapKey string, set map[string]string, remove map[string]bool) error {
	m := mapFieldValue(c, mapKey)
	for k, want := range set {
		got, ok := m[k]
		if !ok {
			return fmt.Errorf("edited config has no entry %q in map %q on client %q", k, mapKey, c.Name)
		}
		if got != want {
			return fmt.Errorf("edited config has %q for entry %q in map %q on client %q, want %q", got, k, mapKey, c.Name, want)
		}
	}
	for k := range remove {
		if _, ok := m[k]; ok {
			return fmt.Errorf("edited config still has entry %q in map %q on client %q", k, mapKey, c.Name)
		}
	}
	return nil
}

// mapFieldValue maps a mapKey to the parsed Client field it targets.
// Adding a new map field to the editors requires a case here so the
// post-edit verify can read it back.
func mapFieldValue(c *Client, key string) map[string]string {
	switch key {
	case "model_aliases":
		return c.ModelAliases
	case "model_routes":
		return c.ModelRoutes
	default:
		return nil
	}
}

// scalarVerify confirms the parsed client carries every requested
// key/value via the struct's yaml-tagged fields. Only the fields
// SetClientScalars is built for are checked; unknown keys are a caller
// bug and fail loudly.
func scalarVerify(c *Client, set map[string]string) error {
	for k, want := range set {
		got := scalarFieldValue(c, k)
		if got == "" {
			return fmt.Errorf("edited config has no value for key %q on client %q", k, c.Name)
		}
		if got != want {
			return fmt.Errorf("edited config has %q for key %q on client %q, want %q", got, k, c.Name, want)
		}
	}
	return nil
}

// scalarFieldValue maps a set key to the parsed Client field it targets.
// Adding a new scalar field to SetClientScalars requires a case here so
// the post-edit verify can read it back.
func scalarFieldValue(c *Client, key string) string {
	switch key {
	case "default_model":
		return c.DefaultModel
	case "subagent_model":
		return c.SubagentModel
	case "subagent_routing":
		return c.SubagentRouting
	case "enabled":
		// Bool field rendered back to the written token; never "", so
		// the missing-value check in scalarVerify cannot false-fire.
		return strconv.FormatBool(c.Enabled)
	default:
		return ""
	}
}

// clientScalarEdits walks the parsed document for one named client and
// produces one edit per requested key: a token replacement when the
// key exists, a line insertion (after the model_aliases block,
// protocol_shape, or name) when it does not. keys is the sorted
// processing order.
func clientScalarEdits(doc *yaml.Node, lines [][]byte, clientName string, set map[string]string, keys []string) ([]yamlEdit, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("not a single-document YAML file")
	}
	_, clients := mappingEntry(doc.Content[0], "clients")
	if clients == nil || clients.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("no clients sequence")
	}
	var item *yaml.Node
	var nameKey, nameVal *yaml.Node
	for _, it := range clients.Content {
		nk, nv := mappingEntry(it, "name")
		if nv == nil || nv.Kind != yaml.ScalarNode || nv.Value != clientName {
			continue
		}
		item = it
		nameKey, nameVal = nk, nv
		break
	}
	if item == nil {
		return nil, fmt.Errorf("no client named %q", clientName)
	}
	var edits []yamlEdit
	var inserts []yamlEdit
	for _, k := range keys {
		v := set[k]
		if _, val := mappingEntry(item, k); val != nil {
			n, err := scalarTokenLength(lines, val)
			if err != nil {
				return nil, fmt.Errorf("client %q key %q: %w", clientName, k, err)
			}
			edits = append(edits, yamlEdit{line: val.Line, col: val.Column, oldLen: n, text: v})
			continue
		}
		// Key absent: insert a new line inside the block mapping.
		// Anchor priority: after the LAST line of the model_aliases
		// block when that key exists on the client, else after
		// protocol_shape, else after name.
		if item.Style == yaml.FlowStyle {
			return nil, fmt.Errorf("client %q: cannot insert key %q into a flow-style mapping; add it by editing the file", clientName, k)
		}
		anchorKey, anchorVal := modelAliasesLastLine(item)
		if anchorKey == nil {
			anchorKey, anchorVal = mappingEntry(item, "protocol_shape")
			if anchorVal == nil {
				anchorKey, anchorVal = nameKey, nameVal
			}
		}
		line := anchorKey.Line
		if anchorVal.Line > line {
			line = anchorVal.Line
		}
		indent := bytes.Repeat([]byte(" "), anchorKey.Column-1)
		inserts = append(inserts, yamlEdit{line: line, insert: true, text: string(indent) + k + ": " + v})
	}
	// applyEdits applies inserts bottom-up (highest line first); for
	// inserts sharing one anchor line, that reverses slice order. Append
	// inserts in reverse sorted-key order so the file ends up in sorted
	// key order after the bottom-up pass.
	for i := len(inserts) - 1; i >= 0; i-- {
		edits = append(edits, inserts[i])
	}
	return edits, nil
}

// clientMapEdits walks the parsed document for one named client and
// produces edits for a single map-valued key (mapKey): value
// replacements for existing entries, line insertions for missing set
// entries, and line removals for remove entries. When the block is
// absent and there are set entries, it is created (anchored after
// model_aliases, subagent_routing, subagent_model, protocol_shape, or
// name). When removals would empty the block, the mapKey line itself
// is removed too, along with orphaned comment-only lines inside the
// block and directly above the mapKey line. set is nil for a
// pure-remove call; remove is nil for a pure-set call.
func clientMapEdits(doc *yaml.Node, lines [][]byte, clientName, mapKey string, set map[string]string, remove map[string]bool) ([]yamlEdit, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("not a single-document YAML file")
	}
	_, clients := mappingEntry(doc.Content[0], "clients")
	if clients == nil || clients.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("no clients sequence")
	}
	var item *yaml.Node
	var nameKey, nameVal *yaml.Node
	for _, it := range clients.Content {
		nk, nv := mappingEntry(it, "name")
		if nv == nil || nv.Kind != yaml.ScalarNode || nv.Value != clientName {
			continue
		}
		item = it
		nameKey, nameVal = nk, nv
		break
	}
	if item == nil {
		return nil, fmt.Errorf("no client named %q", clientName)
	}
	if item.Style == yaml.FlowStyle {
		return nil, fmt.Errorf("client %q: cannot edit map %q in a flow-style mapping; edit it by hand", clientName, mapKey)
	}
	blockKey, blockVal := mappingEntry(item, mapKey)
	var edits []yamlEdit
	var inserts []yamlEdit
	var removes []yamlEdit

	// A bare "mapKey:" line with no entries (e.g. the user uncommented
	// only the key from the template) parses as a null scalar, not a
	// mapping. Treat it as an empty mapping: pure removals are a no-op,
	// sets insert the sorted entries as new child lines right after the
	// key line.
	nullBlock := blockVal != nil && blockVal.Kind == yaml.ScalarNode && blockVal.Tag == "!!null"

	if nullBlock {
		if len(set) == 0 {
			return nil, nil
		}
		childIndent := string(bytes.Repeat([]byte(" "), blockKey.Column-1+2))
		var keys []string
		for k := range set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// Appended sorted; the reversal below flips the slice so the
		// bottom-up same-line insert pass lands them in sorted order.
		for _, k := range keys {
			inserts = append(inserts, yamlEdit{line: blockKey.Line, insert: true, text: childIndent + k + ": " + set[k]})
		}
	} else if blockVal != nil {
		if blockVal.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("client %q map %q is not a mapping", clientName, mapKey)
		}
		if blockVal.Style == yaml.FlowStyle {
			// A flow-style map value ({opus: native, ...}) shares one
			// physical line with every entry and the key; the line-based
			// splices below would clobber unrelated entries. Refuse before
			// planning any edit, for both set and remove.
			return nil, fmt.Errorf("client %q: cannot edit flow-style map %q; edit it by hand", clientName, mapKey)
		}
		// Replace existing set entries in place.
		for k, v := range set {
			if _, val := mappingEntry(blockVal, k); val != nil {
				n, err := scalarTokenLength(lines, val)
				if err != nil {
					return nil, fmt.Errorf("client %q map %q entry %q: %w", clientName, mapKey, k, err)
				}
				edits = append(edits, yamlEdit{line: val.Line, col: val.Column, oldLen: n, text: v})
			}
		}
		// Collect missing set entries for sorted insertion at block end.
		var missing []string
		for k := range set {
			if _, val := mappingEntry(blockVal, k); val == nil {
				missing = append(missing, k)
			}
		}
		sort.Strings(missing)
		// Child indent follows existing block children; fall back to
		// blockKey column + one indent step (2 spaces) when the block is
		// somehow empty (only possible for a flow-ish edge the parser still
		// accepted as a mapping).
		childIndent := childIndentFor(blockKey, blockVal)
		// Insert line: after the LAST line of the block.
		lastLine := blockKey.Line
		for i := 0; i+1 < len(blockVal.Content); i += 2 {
			if blockVal.Content[i+1].Line > lastLine {
				lastLine = blockVal.Content[i+1].Line
			}
		}
		for _, k := range missing {
			inserts = append(inserts, yamlEdit{line: lastLine, insert: true, text: childIndent + k + ": " + set[k]})
		}
		// Removals: delete the entry line for each remove key present.
		// Track how many entries remain after removal to decide whether
		// to drop the mapKey line too.
		remaining := len(blockVal.Content) / 2
		for k := range remove {
			ek, ev := mappingEntry(blockVal, k)
			if ev == nil {
				continue // absent: no-op
			}
			removes = append(removes, yamlEdit{line: ek.Line, removeLine: true})
			remaining--
		}
		if remaining <= 0 {
			// Block becomes empty: drop the mapKey line too. An empty
			// "mapKey:" line is invalid YAML for a non-null mapping and
			// would fail load, so the whole line goes.
			removes = append(removes, yamlEdit{line: blockKey.Line, removeLine: true})
			// Sweep orphaned comments so no stranded over-indented
			// comment lines remain: comment-only lines that were inside
			// the block (between the mapKey line and the last entry) and
			// the contiguous run of comment-only lines directly above the
			// mapKey line. Conservative scope: only comment-only lines,
			// only in those two positions.
			lastEntry := blockKey.Line
			for i := 0; i+1 < len(blockVal.Content); i += 2 {
				if blockVal.Content[i].Line > lastEntry {
					lastEntry = blockVal.Content[i].Line
				}
				if blockVal.Content[i+1].Line > lastEntry {
					lastEntry = blockVal.Content[i+1].Line
				}
			}
			for ln := blockKey.Line + 1; ln <= lastEntry; ln++ {
				if isCommentOnlyLine(lines, ln) {
					removes = append(removes, yamlEdit{line: ln, removeLine: true})
				}
			}
			for ln := blockKey.Line - 1; ln >= 1 && isCommentOnlyLine(lines, ln); ln-- {
				removes = append(removes, yamlEdit{line: ln, removeLine: true})
			}
		}
	} else {
		// Block absent. Removals of absent keys are no-ops; only set
		// entries create the block.
		if len(set) == 0 {
			return nil, nil
		}
		anchorKey, lastLine := mapBlockAnchor(item, nameKey, nameVal)
		line := lastLine
		blockIndent := string(bytes.Repeat([]byte(" "), anchorKey.Column-1))
		childIndent := string(bytes.Repeat([]byte(" "), anchorKey.Column-1+2))
		// Build the inserts so the final file order is the mapKey line
		// followed by its children in sorted order. applyEdits applies
		// same-line inserts in slice order, and each lands right after
		// the anchor, so the slice order is the REVERSE of the desired
		// file order: children reverse-sorted, then the mapKey line last.
		var keys []string
		for k := range set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i := len(keys) - 1; i >= 0; i-- {
			k := keys[i]
			inserts = append(inserts, yamlEdit{line: line, insert: true, text: childIndent + k + ": " + set[k]})
		}
		inserts = append(inserts, yamlEdit{line: line, insert: true, text: blockIndent + mapKey + ":"})
	}

	// applyEdits applies inserts bottom-up (highest line first); inserts
	// sharing one anchor line come out in reverse slice order. For the
	// insert-at-end and null-block cases, reverse the sorted slice so
	// they land sorted. The block-creation case is already ordered above.
	if blockVal != nil && len(inserts) > 0 {
		for i, j := 0, len(inserts)-1; i < j; i, j = i+1, j-1 {
			inserts[i], inserts[j] = inserts[j], inserts[i]
		}
	}
	edits = append(edits, inserts...)
	edits = append(edits, removes...)
	return edits, nil
}

// mapBlockAnchor returns the key node to anchor a new map block after
// (its column sets the block indentation) and the greatest line number
// spanned by the anchor's value, in priority order: model_aliases,
// subagent_routing, subagent_model, protocol_shape, name. For a
// mapping-valued anchor (model_aliases) the last line is the last
// child's line, not the mapping start line, so the new block lands
// AFTER the whole anchor block rather than splitting it.
func mapBlockAnchor(item, nameKey, nameVal *yaml.Node) (*yaml.Node, int) {
	for _, k := range []string{"model_aliases", "subagent_routing", "subagent_model", "protocol_shape"} {
		ak, av := mappingEntry(item, k)
		if av == nil {
			continue
		}
		last := av.Line
		if av.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(av.Content); i += 2 {
				if av.Content[i+1].Line > last {
					last = av.Content[i+1].Line
				}
			}
		}
		return ak, last
	}
	return nameKey, nameVal.Line
}

// childIndentFor returns the indentation string for new children of the
// block, derived from an existing child when present, else blockKey
// column + one 2-space indent step.
func childIndentFor(blockKey, blockVal *yaml.Node) string {
	if blockVal.Kind == yaml.MappingNode && len(blockVal.Content) > 0 {
		firstChild := blockVal.Content[0] // first key node
		if firstChild.Line > 0 && firstChild.Column > 0 {
			return string(bytes.Repeat([]byte(" "), firstChild.Column-1))
		}
	}
	return string(bytes.Repeat([]byte(" "), blockKey.Column-1+2))
}

// modelAliasesLastLine returns the model_aliases key node (for its
// column, so inserted siblings sit at the client-block level, not the
// model_aliases entry level) paired with the value node whose line is
// the greatest among the block's children, so a new scalar can be
// inserted after the whole model_aliases block. Returns nil, nil when
// model_aliases is absent or not a mapping.
func modelAliasesLastLine(item *yaml.Node) (*yaml.Node, *yaml.Node) {
	ak, av := mappingEntry(item, "model_aliases")
	if ak == nil || av == nil || av.Kind != yaml.MappingNode || len(av.Content) == 0 {
		return nil, nil
	}
	// The last child pair's value node has the highest line. Walk all
	// child nodes (key, value, key, value, ...) and track the max line
	// among value nodes; pair the model_aliases key (for its column)
	// with that value node (for its line).
	var bestVal *yaml.Node
	maxLine := 0
	for i := 0; i+1 < len(av.Content); i += 2 {
		vn := av.Content[i+1]
		if vn.Line > maxLine {
			maxLine = vn.Line
			bestVal = vn
		}
	}
	if bestVal == nil {
		return nil, nil
	}
	return ak, bestVal
}

// isCommentOnlyLine reports whether 1-based line n of lines holds only
// a comment (optionally indented). Blank lines are not comment-only, so
// a contiguous-run scan stops at them.
func isCommentOnlyLine(lines [][]byte, n int) bool {
	if n < 1 || n > len(lines) {
		return false
	}
	t := bytes.TrimSpace(lines[n-1])
	return len(t) > 0 && t[0] == '#'
}

// mappingEntry returns the key and value nodes for key in mapping m,
// or nil, nil when absent (or m is not a mapping).
func mappingEntry(m *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// scalarTokenLength returns the byte length of the raw source token
// for single-line scalar n, so a replacement can splice exactly over
// it (leaving any inline comment after it intact). Plain scalars must
// match their Value verbatim at the recorded position; quoted scalars
// are scanned to their closing quote.
func scalarTokenLength(lines [][]byte, n *yaml.Node) (int, error) {
	if n.Kind != yaml.ScalarNode {
		return 0, fmt.Errorf("scalar value is not a scalar")
	}
	if n.Line < 1 || n.Line > len(lines) || n.Column < 1 || n.Column-1 > len(lines[n.Line-1]) {
		return 0, fmt.Errorf("scalar value position %d:%d out of range", n.Line, n.Column)
	}
	rest := lines[n.Line-1][n.Column-1:]
	switch n.Style {
	case 0:
		if len(rest) >= len(n.Value) && string(rest[:len(n.Value)]) == n.Value {
			return len(n.Value), nil
		}
		return 0, fmt.Errorf("scalar value %q not found at %d:%d", n.Value, n.Line, n.Column)
	case yaml.DoubleQuotedStyle:
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\\' {
				i++
				continue
			}
			if rest[i] == '"' {
				return i + 1, nil
			}
		}
		return 0, fmt.Errorf("unterminated double-quoted scalar value at %d:%d", n.Line, n.Column)
	case yaml.SingleQuotedStyle:
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\'' {
				if i+1 < len(rest) && rest[i+1] == '\'' {
					i++
					continue
				}
				return i + 1, nil
			}
		}
		return 0, fmt.Errorf("unterminated single-quoted scalar value at %d:%d", n.Line, n.Column)
	default:
		return 0, fmt.Errorf("scalar value at %d:%d has an unsupported scalar style", n.Line, n.Column)
	}
}

// applyEdits splices the edits into b. Replacements never renumber
// lines, so they apply first; removals and insertions apply bottom-up
// (highest line first) so neither renumbers the anchor of the other.
func applyEdits(b []byte, edits []yamlEdit) ([]byte, error) {
	lines := bytes.SplitAfter(b, []byte("\n"))
	var pending []yamlEdit
	for _, e := range edits {
		if e.insert || e.removeLine {
			pending = append(pending, e)
			continue
		}
		if e.line < 1 || e.line > len(lines) {
			return nil, fmt.Errorf("edit line %d out of range", e.line)
		}
		line := lines[e.line-1]
		end := e.col - 1 + e.oldLen
		if end > len(line) {
			return nil, fmt.Errorf("edit at %d:%d overruns the line", e.line, e.col)
		}
		nl := make([]byte, 0, len(line)-e.oldLen+len(e.text))
		nl = append(nl, line[:e.col-1]...)
		nl = append(nl, e.text...)
		nl = append(nl, line[end:]...)
		lines[e.line-1] = nl
	}
	// Bottom-up so a removal or insertion at a lower line never shifts
	// the anchor index of one at a higher line. Removals and inserts at
	// the same line would be ambiguous, but the editors never emit both
	// for one line: removals target existing entry/mapKey lines, inserts
	// anchor after a block's last line or create a new block.
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].line > pending[j].line })
	for _, e := range pending {
		if e.line < 1 || e.line > len(lines) {
			return nil, fmt.Errorf("edit line %d out of range", e.line)
		}
		if e.removeLine {
			lines = append(lines[:e.line-1], lines[e.line:]...)
			continue
		}
		anchor := lines[e.line-1]
		ending := []byte("\n")
		if bytes.HasSuffix(anchor, []byte("\r\n")) {
			ending = []byte("\r\n")
		} else if !bytes.HasSuffix(anchor, []byte("\n")) {
			// Anchor is the last line and lacks a newline: terminate it
			// so the inserted line starts fresh.
			lines[e.line-1] = append(append([]byte{}, anchor...), '\n')
		}
		newLine := append([]byte(e.text), ending...)
		lines = append(lines[:e.line:e.line], append([][]byte{newLine}, lines[e.line:]...)...)
	}
	return bytes.Join(lines, nil), nil
}

// writeFileAtomic writes data to path via a temp file in the same
// directory, chmod to mode, then rename, so readers never observe a
// partial file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gateway.yaml.*")
	if err != nil {
		return fmt.Errorf("createtmp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
