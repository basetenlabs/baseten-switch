// Package piconfig updates one provider entry in Pi's JSONC model
// configuration while preserving all unrelated bytes.
package piconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// ErrProviderCollision is returned when UpsertProvider finds an existing
// provider entry and replacement was not explicitly allowed.
var ErrProviderCollision = errors.New("provider entry already exists")

type edit struct {
	start int
	end   int
	text  []byte
}

// Provider returns the exact bytes of providerID's value from the top-level
// "providers" object. The returned slice does not alias input.
func Provider(input []byte, providerID string) (raw []byte, found bool, err error) {
	if err := validateProviderID(providerID); err != nil {
		return nil, false, err
	}
	root, err := parseJSONCDocument(input)
	if err != nil {
		return nil, false, err
	}
	if root.kind != kindObject {
		return nil, false, errors.New("Pi model configuration must be a top-level object")
	}
	providersIndex, duplicate := duplicateMember(root, "providers")
	if duplicate {
		return nil, false, errors.New(`Pi model configuration contains duplicate top-level "providers" keys`)
	}
	if providersIndex < 0 {
		return nil, false, nil
	}
	providers := root.members[providersIndex].value
	if providers.kind != kindObject {
		return nil, false, errors.New(`top-level "providers" value must be an object`)
	}
	targetIndex, duplicate := duplicateMember(providers, providerID)
	if duplicate {
		return nil, false, fmt.Errorf("Pi model configuration contains duplicate provider key %q", providerID)
	}
	if targetIndex < 0 {
		return nil, false, nil
	}
	value := providers.members[targetIndex].value
	return append([]byte(nil), input[value.start:value.end]...), true, nil
}

// UpsertProvider adds or replaces providerID under the top-level "providers"
// object. providerJSON must contain exactly one strict JSON object value.
//
// Existing providers are replaced only when allowReplace is true. The returned
// bool reports whether the output differs from input.
func UpsertProvider(input []byte, providerID string, providerJSON []byte, allowReplace bool) ([]byte, bool, error) {
	if err := validateProviderID(providerID); err != nil {
		return nil, false, err
	}
	rawProvider, err := parseProviderJSON(providerJSON)
	if err != nil {
		return nil, false, err
	}

	root, err := parseJSONCDocument(input)
	if err != nil {
		return nil, false, err
	}
	if root.kind != kindObject {
		return nil, false, errors.New("Pi model configuration must be a top-level object")
	}

	providersIndex, duplicate := duplicateMember(root, "providers")
	if duplicate {
		return nil, false, errors.New(`Pi model configuration contains duplicate top-level "providers" keys`)
	}
	if providersIndex < 0 {
		key, _ := json.Marshal("providers")
		providerKey, _ := json.Marshal(providerID)
		value := make([]byte, 0, len(providerKey)+len(rawProvider)+6)
		value = append(value, '{')
		value = append(value, providerKey...)
		value = append(value, ':', ' ')
		value = append(value, rawProvider...)
		value = append(value, '}')
		edits := objectInsertEdits(input, root, key, value)
		return applyAndValidate(input, edits)
	}

	providers := root.members[providersIndex].value
	if providers.kind != kindObject {
		return nil, false, errors.New(`top-level "providers" value must be an object`)
	}
	targetIndex, duplicate := duplicateMember(providers, providerID)
	if duplicate {
		return nil, false, fmt.Errorf("Pi model configuration contains duplicate provider key %q", providerID)
	}
	if targetIndex >= 0 {
		if !allowReplace {
			return nil, false, fmt.Errorf("%w: %q", ErrProviderCollision, providerID)
		}
		current := input[providers.members[targetIndex].value.start:providers.members[targetIndex].value.end]
		if bytes.Equal(current, rawProvider) {
			return append([]byte(nil), input...), false, nil
		}
		return applyAndValidate(input, []edit{{
			start: providers.members[targetIndex].value.start,
			end:   providers.members[targetIndex].value.end,
			text:  rawProvider,
		}})
	}

	key, _ := json.Marshal(providerID)
	edits := objectInsertEdits(input, providers, key, rawProvider)
	return applyAndValidate(input, edits)
}

// RemoveProvider removes providerID from the top-level "providers" object.
// It leaves the providers object and all unrelated bytes in place.
func RemoveProvider(input []byte, providerID string) ([]byte, bool, error) {
	if err := validateProviderID(providerID); err != nil {
		return nil, false, err
	}
	root, err := parseJSONCDocument(input)
	if err != nil {
		return nil, false, err
	}
	if root.kind != kindObject {
		return nil, false, errors.New("Pi model configuration must be a top-level object")
	}
	providersIndex, duplicate := duplicateMember(root, "providers")
	if duplicate {
		return nil, false, errors.New(`Pi model configuration contains duplicate top-level "providers" keys`)
	}
	if providersIndex < 0 {
		return append([]byte(nil), input...), false, nil
	}
	providers := root.members[providersIndex].value
	if providers.kind != kindObject {
		return nil, false, errors.New(`top-level "providers" value must be an object`)
	}
	targetIndex, duplicate := duplicateMember(providers, providerID)
	if duplicate {
		return nil, false, fmt.Errorf("Pi model configuration contains duplicate provider key %q", providerID)
	}
	if targetIndex < 0 {
		return append([]byte(nil), input...), false, nil
	}

	target := providers.members[targetIndex]
	edits := []edit{{start: target.keyStart, end: target.value.end}}
	switch {
	case target.comma >= 0:
		edits = append(edits, edit{start: target.comma, end: target.comma + 1})
	case targetIndex > 0:
		previousComma := providers.members[targetIndex-1].comma
		if previousComma < 0 {
			return nil, false, errors.New("invalid providers object separator")
		}
		edits = append(edits, edit{start: previousComma, end: previousComma + 1})
	}
	return applyAndValidate(input, edits)
}

func validateProviderID(providerID string) error {
	if providerID == "" {
		return errors.New("provider ID must not be empty")
	}
	if !utf8.ValidString(providerID) {
		return errors.New("provider ID must be valid UTF-8")
	}
	return nil
}

func parseProviderJSON(input []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(input)
	if !json.Valid(trimmed) {
		return nil, errors.New("provider JSON must be valid strict JSON")
	}
	value, err := parseJSONCDocument(trimmed)
	if err != nil {
		return nil, fmt.Errorf("provider JSON: %w", err)
	}
	if value.kind != kindObject {
		return nil, errors.New("provider JSON must be an object")
	}
	if err := validateUniqueObjectKeys(value); err != nil {
		return nil, err
	}
	return append([]byte(nil), trimmed...), nil
}

func objectInsertEdits(src []byte, object jsonValue, keyJSON, value []byte) []edit {
	member := make([]byte, 0, len(keyJSON)+len(value)+2)
	member = append(member, keyJSON...)
	member = append(member, ':', ' ')
	member = append(member, value...)

	edits := make([]edit, 0, 2)
	if len(object.members) > 0 {
		last := object.members[len(object.members)-1]
		if last.comma < 0 {
			edits = append(edits, edit{
				start: last.value.end,
				end:   last.value.end,
				text:  []byte{','},
			})
		}
	}

	if bytes.IndexByte(src[object.open+1:object.close], '\n') < 0 {
		prefix := []byte(nil)
		if object.close > object.open+1 &&
			(len(object.members) > 0 || !isJSONCWhitespace(src[object.close-1])) {
			prefix = []byte{' '}
		}
		edits = append(edits, edit{
			start: object.close,
			end:   object.close,
			text:  append(prefix, member...),
		})
		return edits
	}

	insertAt, closeIndent, closeOnOwnLine := lineIndentBefore(src, object.close)
	memberIndent := objectMemberIndent(src, object, closeIndent)
	newline := objectNewline(src[object.open+1 : object.close])
	text := make([]byte, 0, len(memberIndent)+len(member)+len(closeIndent)+2*len(newline))
	if !closeOnOwnLine {
		text = append(text, newline...)
	}
	text = append(text, memberIndent...)
	text = append(text, member...)
	text = append(text, newline...)
	if !closeOnOwnLine {
		text = append(text, closeIndent...)
	}
	edits = append(edits, edit{start: insertAt, end: insertAt, text: text})
	return edits
}

func objectNewline(body []byte) []byte {
	index := bytes.IndexByte(body, '\n')
	if index > 0 && body[index-1] == '\r' {
		return []byte{'\r', '\n'}
	}
	return []byte{'\n'}
}

func isJSONCWhitespace(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}

func lineIndentBefore(src []byte, pos int) (int, []byte, bool) {
	lineStart := bytes.LastIndexByte(src[:pos], '\n') + 1
	indent := src[lineStart:pos]
	for _, b := range indent {
		if b != ' ' && b != '\t' {
			return pos, objectLineIndent(src, pos), false
		}
	}
	return lineStart, append([]byte(nil), indent...), true
}

func objectLineIndent(src []byte, pos int) []byte {
	lineStart := bytes.LastIndexByte(src[:pos], '\n') + 1
	end := lineStart
	for end < pos && (src[end] == ' ' || src[end] == '\t') {
		end++
	}
	return append([]byte(nil), src[lineStart:end]...)
}

func objectMemberIndent(src []byte, object jsonValue, closeIndent []byte) []byte {
	if len(object.members) > 0 {
		first := object.members[0]
		lineStart := bytes.LastIndexByte(src[:first.keyStart], '\n') + 1
		indent := src[lineStart:first.keyStart]
		valid := true
		for _, b := range indent {
			if b != ' ' && b != '\t' {
				valid = false
				break
			}
		}
		if valid {
			return append([]byte(nil), indent...)
		}
	}
	indent := append([]byte(nil), closeIndent...)
	return append(indent, ' ', ' ')
}

func applyAndValidate(input []byte, edits []edit) ([]byte, bool, error) {
	if len(edits) == 0 {
		return append([]byte(nil), input...), false, nil
	}
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start < edits[j].start
		}
		return edits[i].end < edits[j].end
	})
	var output bytes.Buffer
	cursor := 0
	for _, change := range edits {
		if change.start < cursor || change.start < 0 || change.end < change.start || change.end > len(input) {
			return nil, false, errors.New("overlapping or invalid JSONC edit")
		}
		output.Write(input[cursor:change.start])
		output.Write(change.text)
		cursor = change.end
	}
	output.Write(input[cursor:])
	result := output.Bytes()
	if _, err := parseJSONCDocument(result); err != nil {
		return nil, false, fmt.Errorf("edited Pi model configuration is invalid: %w", err)
	}
	return result, !bytes.Equal(input, result), nil
}
