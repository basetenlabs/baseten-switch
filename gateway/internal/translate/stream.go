package translate

import (
	"encoding/json"
	"fmt"
	"io"
)

// StreamTranslator re-encodes an OpenAI chat.completions SSE stream as an
// Anthropic Messages SSE event stream. Feed it each upstream `data:`
// payload via HandleData, then call Finish after the upstream ends (or
// after `[DONE]`). Events are written to w as they are produced.
//
// Supported: text deltas, sequential streamed tool calls with argument
// fragments, finish_reason mapping, and the trailing usage chunk
// (requested via stream_options.include_usage) which lands in the final
// message_delta so downstream usage parsing sees real token counts.
type StreamTranslator struct {
	w        io.Writer
	started  bool
	finished bool
	msgID    string
	model    string

	openBlock int         // anthropic index of the open content block, -1 = none
	openType  string      // "text" | "tool_use"
	nextBlock int         // next anthropic block index to allocate
	textBlock int         // anthropic index of the open text block, -1 = none
	toolIdx   map[int]int // openai tool_call index -> anthropic block index

	finish string
	usage  *openAIUsage
}

func NewStreamTranslator(w io.Writer) *StreamTranslator {
	return &StreamTranslator{w: w, openBlock: -1, textBlock: -1, toolIdx: map[int]int{}}
}

type oaChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage    `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// HandleData processes one upstream SSE data payload (without the
// "data: " prefix). `[DONE]` payloads are ignored; call Finish for
// stream termination.
func (t *StreamTranslator) HandleData(data []byte) error {
	if len(data) == 0 || string(data) == "[DONE]" {
		return nil
	}
	var c oaChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil // tolerate unparseable keep-alive junk
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		return t.emitError(c.Error)
	}
	if err := t.ensureStarted(c.ID, c.Model); err != nil {
		return err
	}
	if c.Usage != nil {
		t.usage = c.Usage
	}
	for _, ch := range c.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			t.finish = *ch.FinishReason
		}
		for _, tc := range ch.Delta.ToolCalls {
			idx, known := t.toolIdx[tc.Index]
			if !known {
				if err := t.closeOpenBlock(); err != nil {
					return err
				}
				idx = t.nextBlock
				t.nextBlock++
				t.toolIdx[tc.Index] = idx
				t.openBlock = idx
				t.openType = "tool_use"
				if err := t.emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    tc.ID,
						"name":  tc.Function.Name,
						"input": map[string]any{},
					},
				}); err != nil {
					return err
				}
			}
			if tc.Function.Arguments != "" {
				if err := t.emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				}); err != nil {
					return err
				}
			}
		}
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			if t.textBlock == -1 {
				if err := t.closeOpenBlock(); err != nil {
					return err
				}
				t.textBlock = t.nextBlock
				t.nextBlock++
				t.openBlock = t.textBlock
				t.openType = "text"
				if err := t.emit("content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         t.textBlock,
					"content_block": map[string]any{"type": "text", "text": ""},
				}); err != nil {
					return err
				}
			}
			if err := t.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": t.textBlock,
				"delta": map[string]any{"type": "text_delta", "text": *ch.Delta.Content},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Finish closes any open block and emits message_delta (stop reason +
// usage) and message_stop. Safe to call once, after the upstream ends.
func (t *StreamTranslator) Finish() error {
	if t.finished {
		return nil
	}
	t.finished = true
	if err := t.ensureStarted("", ""); err != nil {
		return err
	}
	if err := t.closeOpenBlock(); err != nil {
		return err
	}
	u := map[string]any{"output_tokens": 0}
	if t.usage != nil {
		u = t.usage.anthropic()
	}
	if err := t.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": StopReason(t.finish), "stop_sequence": nil},
		"usage": u,
	}); err != nil {
		return err
	}
	return t.emit("message_stop", map[string]any{"type": "message_stop"})
}

func (t *StreamTranslator) ensureStarted(id, model string) error {
	if t.started {
		return nil
	}
	t.started = true
	t.msgID = messageID(id)
	t.model = model
	if err := t.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            t.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         t.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}
	return t.emit("ping", map[string]any{"type": "ping"})
}

func (t *StreamTranslator) closeOpenBlock() error {
	if t.openBlock == -1 {
		return nil
	}
	idx := t.openBlock
	if t.openType == "text" {
		t.textBlock = -1
	}
	t.openBlock = -1
	t.openType = ""
	return t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (t *StreamTranslator) emitError(raw json.RawMessage) error {
	var e struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Type == "" {
		e.Type = "api_error"
	}
	return t.emit("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": e.Type, "message": e.Message},
	})
}

func (t *StreamTranslator) emit(event string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(t.w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
