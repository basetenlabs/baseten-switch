package usage

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type Usage struct {
	InputTokens                             int64 `json:"input_tokens"`
	OutputTokens                            int64 `json:"output_tokens"`
	CacheCreationInputTokens                int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens                    int64 `json:"cache_read_input_tokens"`
	CacheCreationFiveMinuteInputTokens      int64 `json:"-"`
	CacheCreationOneHourInputTokens         int64 `json:"-"`
	CacheCreationFiveMinuteTokensObserved   bool  `json:"-"`
	CacheCreationOneHourTokensObserved      bool  `json:"-"`
	CacheCreationTokenBreakdownComplete     bool  `json:"-"`
	CacheCreationTokenBreakdownInconsistent bool  `json:"-"`
}

type SSEMetadata struct {
	Usage        Usage
	Saw          bool
	Complete     bool
	ToolCalls    int
	StopReason   string
	Model        string
	Speed        string
	SpeedPresent bool
}

var usageKeys = []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens"}

// usageAliases normalizes OpenAI chat-completions usage into the canonical
// Anthropic-shaped fields used by the gateway. OpenAI Responses already uses
// input_tokens/output_tokens, while chat completions reports
// prompt_tokens/completion_tokens.
var usageAliases = map[string]string{
	"input_tokens":  "prompt_tokens",
	"output_tokens": "completion_tokens",
}

func ParseSSEUsage(buf []byte) Usage {
	return ParseSSEUsageWithSaw(buf).Usage
}

func ParseSSEUsageWithSaw(buf []byte) SSEMetadata {
	usage := map[string]int64{}
	saw := false
	complete := false
	toolCalls := 0
	responseToolCallIDs := map[string]struct{}{}
	stopReason := ""
	model := ""
	speed := ""
	speedPresent := false
	var cacheCreation cacheCreationTokenBreakdown
	for _, raw := range bytes.Split(buf, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			complete = true
			continue
		}
		var evt map[string]json.RawMessage
		if err := json.Unmarshal(payload, &evt); err != nil {
			continue
		}
		if reportedModel := modelFromObject(evt); reportedModel != "" {
			model = reportedModel
		}
		if responseRaw, ok := evt["response"]; ok {
			if uraw := nestedRawField(responseRaw, "usage"); len(uraw) > 0 {
				usage = mergeUsage(usage, uraw, true, &saw)
				if finalSpeed, present := speedFromUsage(uraw); present {
					speed = finalSpeed
					speedPresent = true
				}
			}
			if reason := responsesStopReason(responseRaw); reason != "" {
				stopReason = reason
			}
		}
		if msg, ok := evt["message"]; ok {
			var m map[string]json.RawMessage
			if json.Unmarshal(msg, &m) == nil {
				if uraw, ok := m["usage"]; ok {
					usage = mergeUsage(usage, uraw, false, &saw)
					if speed == "" && !speedPresent {
						speed, speedPresent = speedFromUsage(uraw)
					}
					cacheCreation.merge(uraw)
				}
			}
		}
		if uraw, ok := evt["usage"]; ok {
			usage = mergeUsage(usage, uraw, true, &saw)
			if finalSpeed, present := speedFromUsage(uraw); present {
				speed = finalSpeed
				speedPresent = true
			}
			cacheCreation.merge(uraw)
		}
		if traw, ok := evt["type"]; ok {
			var t string
			if json.Unmarshal(traw, &t) == nil {
				if t == "message_stop" ||
					t == "response.completed" ||
					t == "response.failed" ||
					t == "response.incomplete" {
					complete = true
				}
				if t == "response.output_item.done" {
					if itemRaw, ok := evt["item"]; ok {
						toolCalls += addResponsesFunctionCalls(
							itemRaw,
							responseToolCallIDs,
						)
					}
				}
				// Some compatible endpoints omit output_item.done but retain
				// the complete output snapshot on the terminal response.
				// IDs deduplicate the normal case where both are present.
				if t == "response.completed" ||
					t == "response.failed" ||
					t == "response.incomplete" {
					if responseRaw, ok := evt["response"]; ok {
						toolCalls += addResponsesOutputFunctionCalls(
							responseRaw,
							responseToolCallIDs,
						)
					}
				}
				if t == "content_block_start" {
					if cbRaw, ok := evt["content_block"]; ok {
						var cb struct {
							Type string `json:"type"`
						}
						if json.Unmarshal(cbRaw, &cb) == nil && cb.Type == "tool_use" {
							toolCalls++
						}
					}
				}
				if t == "message_delta" {
					if dRaw, ok := evt["delta"]; ok {
						var d struct {
							StopReason string `json:"stop_reason"`
						}
						if json.Unmarshal(dRaw, &d) == nil && d.StopReason != "" {
							stopReason = d.StopReason
						}
					}
				}
			}
		}
	}
	cacheCreationFiveMinute, cacheCreationOneHour := cacheCreation.resolved(usage)
	return SSEMetadata{
		Usage: Usage{
			InputTokens:                             usage["input_tokens"],
			OutputTokens:                            usage["output_tokens"],
			CacheCreationInputTokens:                usage["cache_creation_input_tokens"],
			CacheReadInputTokens:                    usage["cache_read_input_tokens"],
			CacheCreationFiveMinuteInputTokens:      cacheCreationFiveMinute,
			CacheCreationOneHourInputTokens:         cacheCreationOneHour,
			CacheCreationFiveMinuteTokensObserved:   cacheCreation.fiveMinuteObserved,
			CacheCreationOneHourTokensObserved:      cacheCreation.oneHourObserved,
			CacheCreationTokenBreakdownComplete:     cacheCreation.complete(usage),
			CacheCreationTokenBreakdownInconsistent: cacheCreation.inconsistent(usage),
		},
		Saw:          saw,
		Complete:     complete,
		ToolCalls:    toolCalls,
		StopReason:   stopReason,
		Model:        model,
		Speed:        speed,
		SpeedPresent: speedPresent,
	}
}

type cacheCreationTokenBreakdown struct {
	fiveMinute         int64
	oneHour            int64
	fiveMinuteObserved bool
	oneHourObserved    bool
	invalid            bool
}

func (b *cacheCreationTokenBreakdown) merge(raw []byte) {
	var usage map[string]json.RawMessage
	if json.Unmarshal(raw, &usage) != nil {
		return
	}
	var cacheCreation map[string]json.RawMessage
	if json.Unmarshal(usage["cache_creation"], &cacheCreation) != nil {
		return
	}
	if rawTokens, present := cacheCreation["ephemeral_5m_input_tokens"]; present {
		value, err := parseNumber(rawTokens)
		if err != nil || value < 0 {
			b.invalid = true
		} else {
			b.fiveMinute = value
			b.fiveMinuteObserved = true
		}
	}
	if rawTokens, present := cacheCreation["ephemeral_1h_input_tokens"]; present {
		value, err := parseNumber(rawTokens)
		if err != nil || value < 0 {
			b.invalid = true
		} else {
			b.oneHour = value
			b.oneHourObserved = true
		}
	}
}

func (b cacheCreationTokenBreakdown) complete(usage map[string]int64) bool {
	if b.invalid {
		return false
	}
	total, totalObserved := usage["cache_creation_input_tokens"]
	if !totalObserved || total < 0 {
		return false
	}
	if b.fiveMinuteObserved && b.oneHourObserved {
		return b.fiveMinute+b.oneHour == total
	}
	if b.fiveMinuteObserved {
		return b.fiveMinute <= total
	}
	if b.oneHourObserved {
		return b.oneHour <= total
	}
	return false
}

func (b cacheCreationTokenBreakdown) inconsistent(usage map[string]int64) bool {
	if b.invalid {
		return true
	}
	total, totalObserved := usage["cache_creation_input_tokens"]
	if !totalObserved {
		return b.fiveMinuteObserved || b.oneHourObserved
	}
	if total < 0 {
		return true
	}
	switch {
	case b.fiveMinuteObserved && b.oneHourObserved:
		return b.fiveMinute+b.oneHour != total
	case b.fiveMinuteObserved:
		return b.fiveMinute > total
	case b.oneHourObserved:
		return b.oneHour > total
	default:
		return false
	}
}

func (b cacheCreationTokenBreakdown) resolved(
	usage map[string]int64,
) (fiveMinute, oneHour int64) {
	total := usage["cache_creation_input_tokens"]
	switch {
	case b.fiveMinuteObserved && b.oneHourObserved:
		return b.fiveMinute, b.oneHour
	case b.fiveMinuteObserved:
		return b.fiveMinute, total - b.fiveMinute
	case b.oneHourObserved:
		return total - b.oneHour, b.oneHour
	default:
		return 0, 0
	}
}

// speedFromUsage distinguishes a missing speed field from a present but
// unsupported value. This lets the final SSE usage object authoritatively
// clear an earlier value without exposing an unsupported value downstream.
func speedFromUsage(raw []byte) (string, bool) {
	var usage map[string]json.RawMessage
	if json.Unmarshal(raw, &usage) != nil {
		return "", false
	}
	rawSpeed, present := usage["speed"]
	if !present {
		return "", false
	}
	speed := stringField(rawSpeed)
	if speed != "standard" && speed != "fast" {
		return "", true
	}
	return speed, true
}

// mergeUsage parses a usage JSON object that may contain nested objects (Anthropic
// emits `cache_creation: {ephemeral_5m_input_tokens: 0, ...}` and
// `output_tokens_details: {thinking_tokens: 0}` which break a
// `map[string]json.Number` unmarshal). We instead unmarshal into a
// `map[string]json.RawMessage` and convert each known scalar key individually.
// When overwrite is false (message_start) we use setdefault semantics to seed
// fields that have not been seen yet; when true (message_delta) we overwrite
// because message_delta carries the final authoritative usage.
func mergeUsage(usage map[string]int64, raw []byte, overwrite bool, saw *bool) map[string]int64 {
	var u map[string]json.RawMessage
	if json.Unmarshal(raw, &u) != nil {
		return usage
	}
	_, inputExistedBefore := usage["input_tokens"]
	for _, k := range usageKeys {
		v, ok := u[k]
		if !ok {
			if alias := usageAliases[k]; alias != "" {
				v, ok = u[alias]
			}
		}
		if !ok {
			continue
		}
		exists := false
		if _, has := usage[k]; has {
			exists = true
		}
		if !overwrite && exists {
			continue
		}
		if n, err := parseNumber(v); err == nil {
			usage[k] = n
			*saw = true
		}
	}
	mergeOpenAICachedInputUsage(
		usage,
		u,
		overwrite,
		inputExistedBefore,
		saw,
	)
	return usage
}

// mergeOpenAICachedInputUsage projects OpenAI's nested cached-token count into
// the gateway's provider-neutral usage shape. OpenAI includes cached tokens in
// input_tokens or prompt_tokens, while gateway cost accounting treats normal
// input and cache reads as disjoint dimensions.
func mergeOpenAICachedInputUsage(
	usage map[string]int64,
	fields map[string]json.RawMessage,
	overwrite bool,
	inputExistedBefore bool,
	saw *bool,
) {
	detailsRaw := fields["input_tokens_details"]
	if len(detailsRaw) == 0 {
		detailsRaw = fields["prompt_tokens_details"]
	}
	if len(detailsRaw) == 0 {
		return
	}
	var details map[string]json.RawMessage
	if json.Unmarshal(detailsRaw, &details) != nil {
		return
	}
	cached, err := parseNumber(details["cached_tokens"])
	if err != nil || cached < 0 {
		return
	}
	_, currentInputPresent := fields["input_tokens"]
	if !currentInputPresent {
		_, currentInputPresent = fields["prompt_tokens"]
	}
	input, inputPresent := usage["input_tokens"]
	if (!overwrite && inputExistedBefore) ||
		!currentInputPresent ||
		!inputPresent ||
		input < cached {
		return
	}
	if _, exists := usage["cache_read_input_tokens"]; overwrite || !exists {
		usage["cache_read_input_tokens"] = cached
		*saw = true
	}
	usage["input_tokens"] = input - cached
}

// parseNumber accepts int64 from a JSON number literal or a JSON string of digits,
// which is necessary because Baseten's usage sometimes wraps counts in strings.
func parseNumber(v json.RawMessage) (int64, error) {
	var n json.Number
	if err := json.Unmarshal(v, &n); err == nil {
		return n.Int64()
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		var iv int64
		if _, err := fmt.Sscanf(s, "%d", &iv); err == nil {
			return iv, nil
		}
	}
	return 0, fmt.Errorf("not a number: %s", string(v))
}

// NormalizeAnthropicUsage rewrites a single Anthropic-shape usage object so
// input_tokens EXCLUDES cached reads, per Anthropic semantics.
//
// Protocol normalization: a usage object that carries
// cache_creation_input_tokens (any value, including 0) is treated as already
// exclusive and passed through untouched. The guard matters because
// exclusive-semantics
// responses with a partial cache hit (input >= cache_read) are otherwise
// indistinguishable from inclusive ones and would be under-counted.
//
// Returns (rewritten, true) only when a usage object carries input_tokens
// and cache_read_input_tokens, carries NO cache_creation_input_tokens, and
// input_tokens >= cache_read (never synthesize a negative count). Otherwise
// it returns the input bytes and false, leaving anything it does not
// understand untouched.
func NormalizeAnthropicUsage(raw []byte) ([]byte, bool) {
	var u map[string]json.RawMessage
	if json.Unmarshal(raw, &u) != nil {
		return raw, false
	}
	if _, fixed := u["cache_creation_input_tokens"]; fixed {
		return raw, false
	}
	inRaw, ok := u["input_tokens"]
	if !ok {
		return raw, false
	}
	input, err := parseNumber(inRaw)
	if err != nil {
		return raw, false
	}
	crRaw, ok := u["cache_read_input_tokens"]
	if !ok {
		return raw, false
	}
	cacheRead, err := parseNumber(crRaw)
	if err != nil || input < cacheRead {
		return raw, false
	}
	u["input_tokens"] = json.RawMessage(strconv.FormatInt(input-cacheRead, 10))
	out, err := json.Marshal(u)
	if err != nil {
		return raw, false
	}
	return out, true
}

// NormalizeAnthropicBody applies NormalizeAnthropicUsage to the usage object(s)
// carried by one Anthropic-shape JSON object: the top-level `usage` (a
// non-streaming /v1/messages body or an SSE message_delta event) and a nested
// `message.usage` (an SSE message_start event, which Baseten emits without
// cache fields so this is a no-op there unless one appears). Untouched keys
// keep their original bytes; only rewritten usage objects change. Returns
// (rewritten, true) when any usage object was normalized, else (body, false).
func NormalizeAnthropicBody(body []byte) ([]byte, bool) {
	var evt map[string]json.RawMessage
	if json.Unmarshal(body, &evt) != nil {
		return body, false
	}
	changed := false
	if uraw, ok := evt["usage"]; ok {
		if nu, ok := NormalizeAnthropicUsage(uraw); ok {
			evt["usage"] = nu
			changed = true
		}
	}
	if mraw, ok := evt["message"]; ok {
		var m map[string]json.RawMessage
		if json.Unmarshal(mraw, &m) == nil {
			if uraw, ok := m["usage"]; ok {
				if nu, ok := NormalizeAnthropicUsage(uraw); ok {
					m["usage"] = nu
					if mb, err := json.Marshal(m); err == nil {
						evt["message"] = mb
						changed = true
					}
				}
			}
		}
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(evt)
	if err != nil {
		return body, false
	}
	return out, true
}

func ParseUsage(jsonBody []byte) Usage {
	return ParseUsageWithSaw(jsonBody).Usage
}

type Metadata struct {
	Usage        Usage
	Saw          bool
	Model        string
	ToolCalls    int
	StopReason   string
	Speed        string
	SpeedPresent bool
}

// ParseUsageWithSaw distinguishes an observed provider usage object whose
// reported values are zero from a response that carried no usable usage.
// A non-streaming usage object is final by definition.
func ParseUsageWithSaw(jsonBody []byte) Metadata {
	var parsed map[string]json.RawMessage
	u := Usage{}
	if err := json.Unmarshal(jsonBody, &parsed); err != nil {
		return Metadata{Usage: u}
	}
	model := modelFromObject(parsed)
	toolCalls := addResponsesOutputFunctionCalls(
		json.RawMessage(jsonBody),
		map[string]struct{}{},
	)
	stopReason := responsesStopReason(json.RawMessage(jsonBody))
	usageRaw := parsed["usage"]
	if len(usageRaw) == 0 {
		return Metadata{
			Usage:      u,
			Model:      model,
			ToolCalls:  toolCalls,
			StopReason: stopReason,
		}
	}
	saw := false
	merged := mergeUsage(map[string]int64{}, usageRaw, true, &saw)
	if merged == nil {
		return Metadata{Usage: u, Model: model}
	}
	speed, speedPresent := speedFromUsage(usageRaw)
	var cacheCreation cacheCreationTokenBreakdown
	cacheCreation.merge(usageRaw)
	cacheCreationFiveMinute, cacheCreationOneHour := cacheCreation.resolved(merged)
	return Metadata{
		Usage: Usage{
			InputTokens:                             merged["input_tokens"],
			OutputTokens:                            merged["output_tokens"],
			CacheCreationInputTokens:                merged["cache_creation_input_tokens"],
			CacheReadInputTokens:                    merged["cache_read_input_tokens"],
			CacheCreationFiveMinuteInputTokens:      cacheCreationFiveMinute,
			CacheCreationOneHourInputTokens:         cacheCreationOneHour,
			CacheCreationFiveMinuteTokensObserved:   cacheCreation.fiveMinuteObserved,
			CacheCreationOneHourTokensObserved:      cacheCreation.oneHourObserved,
			CacheCreationTokenBreakdownComplete:     cacheCreation.complete(merged),
			CacheCreationTokenBreakdownInconsistent: cacheCreation.inconsistent(merged),
		},
		Saw:          saw,
		Model:        model,
		ToolCalls:    toolCalls,
		StopReason:   stopReason,
		Speed:        speed,
		SpeedPresent: speedPresent,
	}
}

func nestedRawField(raw json.RawMessage, key string) json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object[key]
}

// responsesStopReason extracts the most specific terminal reason available
// from an OpenAI Responses object. Completed responses have no finish_reason,
// so their status remains the useful provider-authored terminal value.
func responsesStopReason(raw json.RawMessage) string {
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil {
		return ""
	}
	if detailsRaw := response["incomplete_details"]; len(detailsRaw) > 0 {
		var details map[string]json.RawMessage
		if json.Unmarshal(detailsRaw, &details) == nil {
			if reason := stringField(details["reason"]); reason != "" {
				return reason
			}
		}
	}
	if errorRaw := response["error"]; len(errorRaw) > 0 {
		var responseError map[string]json.RawMessage
		if json.Unmarshal(errorRaw, &responseError) == nil {
			if code := stringField(responseError["code"]); code != "" {
				return code
			}
			if errorType := stringField(responseError["type"]); errorType != "" {
				return errorType
			}
		}
	}
	return stringField(response["status"])
}

func addResponsesOutputFunctionCalls(
	responseRaw json.RawMessage,
	seen map[string]struct{},
) int {
	outputRaw := nestedRawField(responseRaw, "output")
	var output []json.RawMessage
	if json.Unmarshal(outputRaw, &output) != nil {
		return 0
	}
	added := 0
	for _, itemRaw := range output {
		added += addResponsesFunctionCalls(itemRaw, seen)
	}
	return added
}

func addResponsesFunctionCalls(
	itemRaw json.RawMessage,
	seen map[string]struct{},
) int {
	var item map[string]json.RawMessage
	if json.Unmarshal(itemRaw, &item) != nil ||
		stringField(item["type"]) != "function_call" {
		return 0
	}
	key := stringField(item["id"])
	if key == "" {
		key = stringField(item["call_id"])
	}
	if key == "" {
		// A malformed item without either stable identifier is still a call,
		// but cannot safely deduplicate against a later response snapshot.
		return 1
	}
	if _, ok := seen[key]; ok {
		return 0
	}
	seen[key] = struct{}{}
	return 1
}

func modelFromObject(object map[string]json.RawMessage) string {
	if model := stringField(object["model"]); model != "" {
		return model
	}
	for _, key := range []string{"message", "response"} {
		var nested map[string]json.RawMessage
		if json.Unmarshal(object[key], &nested) == nil {
			if model := stringField(nested["model"]); model != "" {
				return model
			}
		}
	}
	return ""
}

func stringField(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func SynthCountTokens(body []byte) int {
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return 0
	}
	total := 0
	addStr := func(s string) {
		total += len(s)
	}
	addBlocks := func(c interface{}) {
		switch v := c.(type) {
		case string:
			addStr(v)
		case []interface{}:
			for _, b := range v {
				if m, ok := b.(map[string]interface{}); ok {
					if s, ok := m["text"].(string); ok {
						addStr(s)
					}
					if s, ok := m["content"].(string); ok {
						addStr(s)
					}
					if s, ok := m["input"].(string); ok {
						addStr(s)
					}
				}
			}
		}
	}
	if messages, ok := data["messages"].([]interface{}); ok {
		for _, mm := range messages {
			if m, ok := mm.(map[string]interface{}); ok {
				addBlocks(m["content"])
			}
		}
	}
	switch sv := data["system"].(type) {
	case string:
		addStr(sv)
	case []interface{}:
		for _, s := range sv {
			if m, ok := s.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					addStr(t)
				}
			}
		}
	}
	if tools, ok := data["tools"].([]interface{}); ok {
		for _, tt := range tools {
			if b, err := json.Marshal(tt); err == nil {
				total += len(b)
			}
		}
	}
	if total/4 < 1 {
		return 1
	}
	return total / 4
}

func MaybeDecompress(buf []byte, contentEncoding string) []byte {
	ce := strings.ToLower(contentEncoding)
	if strings.Contains(ce, "gzip") {
		if zr, err := gzip.NewReader(bytes.NewReader(buf)); err == nil {
			if out, err := io.ReadAll(zr); err == nil {
				_ = zr.Close()
				return out
			}
		}
		return buf
	}
	if strings.Contains(ce, "deflate") {
		if zr, err := zlib.NewReader(bytes.NewReader(buf)); err == nil {
			if out, err := io.ReadAll(zr); err == nil {
				_ = zr.Close()
				return out
			}
		}
		if zr := flate.NewReader(bytes.NewReader(buf)); zr != nil {
			if out, err := io.ReadAll(zr); err == nil {
				_ = zr.Close()
				return out
			}
		}
		return buf
	}
	return buf
}
