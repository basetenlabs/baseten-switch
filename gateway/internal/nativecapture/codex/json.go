package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// decodeStrict rejects duplicate object fields and trailing JSON. A duplicate
// field would make a content match depend on parser-specific behavior, which
// is not high-confidence correlation.
func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readJSONValue(decoder)
	if err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, destination)
}

func readJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		result := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("JSON object key is not a string")
			}
			if _, duplicate := result[key]; duplicate {
				return nil, fmt.Errorf("duplicate JSON field %q", key)
			}
			value, err := readJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, errors.New("unterminated JSON object")
		}
		return result, nil
	case '[':
		var result []any
		for decoder.More() {
			value, err := readJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, errors.New("unterminated JSON array")
		}
		return result, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func marshalAllowed(source map[string]any, keys ...string) (json.RawMessage, error) {
	result := make(map[string]any)
	for _, key := range keys {
		value, exists := source[key]
		if !exists || value == nil {
			continue
		}
		if sanitized, keep := sanitizeValue(key, value); keep {
			result[key] = sanitized
		}
	}
	if len(result) == 0 {
		return nil, nil
	}
	return json.Marshal(result)
}

func sanitizeAndMarshal(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := decodeStrict(raw, &value); err != nil {
		return nil, err
	}
	sanitized, keep := sanitizeValue("", value)
	if !keep {
		return nil, nil
	}
	return json.Marshal(sanitized)
}

func sanitizeValue(field string, value any) (any, bool) {
	lower := strings.ToLower(field)
	if lower != "" && unsafeStructuralField(lower) {
		return nil, false
	}
	switch typed := value.(type) {
	case map[string]any:
		if kind, _ := typed["type"].(string); kind == "encrypted_content" {
			return nil, false
		}
		result := make(map[string]any)
		for key, child := range typed {
			if sanitized, keep := sanitizeValue(key, child); keep {
				result[key] = sanitized
			}
		}
		return result, true
	case []any:
		result := make([]any, 0, len(typed))
		for _, child := range typed {
			if sanitized, keep := sanitizeValue("", child); keep {
				result = append(result, sanitized)
			}
		}
		return result, true
	default:
		return typed, true
	}
}

func unsafeStructuralField(field string) bool {
	if strings.Contains(field, "encrypted") || field == "raw_content" || field == "raw_reasoning" {
		return true
	}
	if field == "id" || strings.HasSuffix(field, "_id") || strings.HasSuffix(field, "_ids") {
		return true
	}
	switch field {
	case "cwd", "path", "paths", "saved_path", "workspace_roots", "git", "repository", "repo", "metadata", "environment", "base_instructions", "dynamic_tools", "local_images", "local_audio", "text_elements":
		return true
	default:
		return false
	}
}

func stringField(source map[string]any, key string) string {
	value, _ := source[key].(string)
	return value
}

func numberField(source map[string]any, key string) int64 {
	switch value := source[key].(type) {
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	case float64:
		return int64(value)
	case int64:
		return value
	default:
		return 0
	}
}

func eventTime(source map[string]any, key string, fallback time.Time) time.Time {
	seconds := numberField(source, key)
	if seconds > 0 {
		return time.Unix(seconds, 0).UTC()
	}
	return fallback.UTC()
}
