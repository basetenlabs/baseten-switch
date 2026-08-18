package config

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetTraceCapture replaces only global.trace_capture while preserving every
// unrelated byte in gateway.yaml. Comments inside the replaced subtree are not
// retained because the complete high-sensitivity policy is one atomic edit.
func SetTraceCapture(path string, policy TraceCapture) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular non-symlink file", path)
	}
	var parsed File
	if err := UnmarshalStrict(data, &parsed); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	parsed.Global.TraceCapture = &policy
	if err := ValidateRoutingPolicy(&parsed); err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return fmt.Errorf("%s: not a single-document YAML file", path)
	}
	_, global := mappingEntry(doc.Content[0], "global")
	if global == nil || global.Kind != yaml.MappingNode || global.Style == yaml.FlowStyle {
		return fmt.Errorf("%s: global must be a block mapping", path)
	}

	block, err := renderTraceCaptureBlock(policy)
	if err != nil {
		return err
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	key, value := mappingEntry(global, "trace_capture")
	var output []byte
	if key != nil {
		start := key.Line
		end := yamlSubtreeEndLine(value)
		if start < 1 || end < start || end > len(lines) {
			return fmt.Errorf("%s: trace_capture source range is invalid", path)
		}
		replacement := withDetectedLineEnding(block, lines[start-1])
		outLines := make([][]byte, 0, len(lines)-(end-start)+len(bytes.SplitAfter(replacement, []byte("\n"))))
		outLines = append(outLines, lines[:start-1]...)
		outLines = append(outLines, bytes.SplitAfter(replacement, []byte("\n"))...)
		outLines = append(outLines, lines[end:]...)
		output = bytes.Join(outLines, nil)
	} else {
		anchor := global.Line
		for i := 1; i < len(global.Content); i += 2 {
			if line := yamlSubtreeEndLine(global.Content[i]); line > anchor {
				anchor = line
			}
		}
		if anchor < 1 || anchor > len(lines) {
			return fmt.Errorf("%s: global source range is invalid", path)
		}
		endingBlock := withDetectedLineEnding(block, lines[anchor-1])
		if !bytes.HasSuffix(lines[anchor-1], []byte("\n")) {
			lines[anchor-1] = append(lines[anchor-1], '\n')
		}
		insert := bytes.SplitAfter(endingBlock, []byte("\n"))
		outLines := make([][]byte, 0, len(lines)+len(insert))
		outLines = append(outLines, lines[:anchor]...)
		outLines = append(outLines, insert...)
		outLines = append(outLines, lines[anchor:]...)
		output = bytes.Join(outLines, nil)
	}

	var check File
	if err := UnmarshalStrict(output, &check); err != nil {
		return fmt.Errorf("%s: edited config does not parse (edit aborted): %w", path, err)
	}
	if err := ValidateRoutingPolicy(&check); err != nil {
		return fmt.Errorf("%s: edited config is invalid (edit aborted): %w", path, err)
	}
	if check.Global.TraceCapture == nil || !sameTraceCapture(*check.Global.TraceCapture, policy) {
		return fmt.Errorf("%s: edited trace capture policy does not match request", path)
	}
	return writeFileAtomic(path, output, info.Mode().Perm())
}

func renderTraceCaptureBlock(policy TraceCapture) ([]byte, error) {
	var out strings.Builder
	out.WriteString("  trace_capture:\n")
	out.WriteString("    enabled: ")
	out.WriteString(strconv.FormatBool(policy.Enabled))
	out.WriteByte('\n')
	if len(policy.Clients) == 0 {
		out.WriteString("    clients: []\n")
	} else {
		out.WriteString("    clients:\n")
		for _, client := range policy.Clients {
			if strings.ContainsAny(client, "\r\n\x00") {
				return nil, fmt.Errorf("trace capture client name contains an unsupported character")
			}
			out.WriteString("      - ")
			out.WriteString(strconv.Quote(client))
			out.WriteByte('\n')
		}
	}
	retention := policy.RetentionDays
	if retention == 0 {
		retention = DefaultTraceRetentionDays
	}
	out.WriteString("    retention_days: ")
	out.WriteString(strconv.Itoa(retention))
	out.WriteByte('\n')
	return []byte(out.String()), nil
}

func yamlSubtreeEndLine(node *yaml.Node) int {
	if node == nil {
		return 0
	}
	end := node.Line
	for _, child := range node.Content {
		if line := yamlSubtreeEndLine(child); line > end {
			end = line
		}
	}
	return end
}

func withDetectedLineEnding(block, anchor []byte) []byte {
	if !bytes.HasSuffix(anchor, []byte("\r\n")) {
		return block
	}
	return bytes.ReplaceAll(block, []byte("\n"), []byte("\r\n"))
}

func sameTraceCapture(left, right TraceCapture) bool {
	if left.Enabled != right.Enabled {
		return false
	}
	leftRetention := left.RetentionDays
	if leftRetention == 0 {
		leftRetention = DefaultTraceRetentionDays
	}
	rightRetention := right.RetentionDays
	if rightRetention == 0 {
		rightRetention = DefaultTraceRetentionDays
	}
	if leftRetention != rightRetention || len(left.Clients) != len(right.Clients) {
		return false
	}
	for i := range left.Clients {
		if left.Clients[i] != right.Clients[i] {
			return false
		}
	}
	return true
}
