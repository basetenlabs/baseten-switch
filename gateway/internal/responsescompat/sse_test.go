package responsescompat

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSSEGuardCorrectStreamIsByteIdenticalAtEverySplit(t *testing.T) {
	arguments := `{"city":"París"}`
	stream := append(sseEvent("response.function_call_arguments.delta", map[string]any{
		"type":    "response.function_call_arguments.delta",
		"item_id": "call-1",
		"delta":   arguments,
	}), sseEvent("response.function_call_arguments.done", map[string]any{
		"type":      "response.function_call_arguments.done",
		"item_id":   "call-1",
		"arguments": arguments,
	})...)
	stream = append(stream, sseEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done",
		"item": map[string]any{
			"type":      "function_call",
			"id":        "call-1",
			"arguments": arguments,
		},
	})...)
	stream = append(stream, sseEvent("response.completed", map[string]any{
		"type":     "response.completed",
		"response": map[string]any{"id": "resp-1"},
	})...)

	for split := 0; split <= len(stream); split++ {
		guard := NewSSEGuard(StreamLimits{})
		var got []byte
		out, first := guard.Push(stream[:split])
		got = append(got, out...)
		out, second := guard.Push(stream[split:])
		got = append(got, out...)
		out, flushed := guard.Flush()
		got = append(got, out...)
		if !bytes.Equal(got, stream) {
			t.Fatalf("split %d changed correct stream\n got: %q\nwant: %q", split, got, stream)
		}
		total := StreamResult{}
		total.add(first)
		total.add(second)
		total.add(flushed)
		if total != (StreamResult{}) {
			t.Fatalf("split %d result = %#v", split, total)
		}
	}
}

func TestSSEGuardRepairsTruncatedDoneEvents(t *testing.T) {
	arguments := `{"city":"Paris"}`
	input := append(sseEvent("response.function_call_arguments.delta", map[string]any{
		"type":    "response.function_call_arguments.delta",
		"item_id": "call-1",
		"delta":   arguments,
	}), sseEvent("response.function_call_arguments.done", map[string]any{
		"type":      "response.function_call_arguments.done",
		"item_id":   "call-1",
		"arguments": "{",
	})...)
	input = append(input, sseEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done",
		"item": map[string]any{
			"type":      "function_call",
			"id":        "call-1",
			"arguments": "{",
		},
	})...)
	input = append(input, sseEvent("response.completed", map[string]any{
		"type": "response.completed", "response": map[string]any{"id": "resp-1"},
	})...)

	guard := NewSSEGuard(StreamLimits{})
	got, result := guard.Push(input)
	if result.RepairedEvents != 2 || result.ValidationErrors != 0 || !result.Changed {
		t.Fatalf("result = %#v", result)
	}
	assertDoneArguments(t, got, arguments, 2)
}

func TestSSEGuardHoldsPrematureDoneForLateDelta(t *testing.T) {
	earlyDelta := sseEvent("response.function_call_arguments.delta", map[string]any{
		"type":    "response.function_call_arguments.delta",
		"item_id": "call-1",
		"delta":   "",
	})
	functionDone := sseEvent("response.function_call_arguments.done", map[string]any{
		"type":      "response.function_call_arguments.done",
		"item_id":   "call-1",
		"arguments": "",
	})
	outputDone := sseEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done",
		"item": map[string]any{
			"type":      "function_call",
			"id":        "call-1",
			"arguments": "",
		},
	})
	arguments := `{"city": "Paris"}`
	lateDelta := sseEvent("response.function_call_arguments.delta", map[string]any{
		"type":    "response.function_call_arguments.delta",
		"item_id": "call-1",
		"delta":   arguments,
	})
	completed := sseEvent("response.completed", map[string]any{
		"type":     "response.completed",
		"response": map[string]any{"id": "resp-1"},
	})
	guard := NewSSEGuard(StreamLimits{})
	var got []byte
	var result StreamResult
	for i, event := range [][]byte{earlyDelta, functionDone, outputDone, lateDelta, completed} {
		out, step := guard.Push(event)
		got = append(got, out...)
		result.add(step)
		if i >= 1 && i <= 3 && bytes.Contains(got, []byte("function_call_arguments.done")) {
			t.Fatalf("premature done escaped before the late delta at step %d: %q", i, got)
		}
	}
	if result.RepairedEvents != 2 || result.ValidationErrors != 0 || !result.Changed {
		t.Fatalf("result = %#v", result)
	}
	assertDoneArguments(t, got, arguments, 2)
	lateAt := bytes.Index(got, lateDelta)
	functionAt := bytes.Index(got, []byte("event: response.function_call_arguments.done"))
	outputAt := bytes.Index(got, []byte("event: response.output_item.done"))
	completedAt := bytes.Index(got, []byte("event: response.completed"))
	if !(lateAt >= 0 && lateAt < functionAt && functionAt < outputAt && outputAt < completedAt) {
		t.Fatalf("repaired ordering invalid: late=%d function=%d output=%d completed=%d\n%s",
			lateAt, functionAt, outputAt, completedAt, got)
	}
}

func TestSSEGuardInterleavedCalls(t *testing.T) {
	var input []byte
	for _, event := range [][]byte{
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "a", "delta": `{"a":`,
		}),
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "b", "delta": `{"b":2}`,
		}),
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "a", "delta": `1}`,
		}),
		sseEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "b", "arguments": `{`,
		}),
		sseEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "a", "arguments": `{`,
		}),
		sseEvent("response.completed", map[string]any{
			"type": "response.completed", "response": map[string]any{"id": "resp-1"},
		}),
	} {
		input = append(input, event...)
	}
	guard := NewSSEGuard(StreamLimits{})
	got, result := guard.Push(input)
	if result.RepairedEvents != 2 || result.ValidationErrors != 0 {
		t.Fatalf("result = %#v", result)
	}
	if !bytes.Contains(got, []byte(`\"a\":1`)) || !bytes.Contains(got, []byte(`\"b\":2`)) {
		t.Fatalf("interleaved arguments not repaired: %s", got)
	}
}

func TestSSEGuardHeldParallelCallsPreserveGlobalTerminalOrder(t *testing.T) {
	events := [][]byte{
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "a", "delta": "",
		}),
		sseEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "a", "arguments": "",
		}),
		sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{"type": "function_call", "id": "a", "arguments": ""},
		}),
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "b", "delta": "",
		}),
		sseEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "b", "arguments": "",
		}),
		sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{"type": "function_call", "id": "b", "arguments": ""},
		}),
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "b", "delta": `{"b":2}`,
		}),
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "a", "delta": `{"a":1}`,
		}),
		sseEvent("response.completed", map[string]any{
			"type": "response.completed", "response": map[string]any{"id": "resp-1"},
		}),
	}

	guard := NewSSEGuard(StreamLimits{})
	var got []byte
	var result StreamResult
	for i, event := range events {
		out, step := guard.Push(event)
		got = append(got, out...)
		result.add(step)
		if i >= 6 && i <= 7 && len(terminalSequence(got)) != 0 {
			t.Fatalf("terminals released before the response boundary at step %d: %v", i, terminalSequence(got))
		}
	}
	wantOrder := []string{"function:a", "output:a", "function:b", "output:b"}
	if order := terminalSequence(got); !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("terminal order = %v, want %v\n%s", order, wantOrder, got)
	}
	if result.RepairedEvents != 4 || result.ValidationErrors != 0 || !result.Changed {
		t.Fatalf("result = %#v", result)
	}
}

func TestSSEGuardValidLatePrefixDoesNotFinalizeEarly(t *testing.T) {
	events := [][]byte{
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "a", "delta": "",
		}),
		sseEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "a", "arguments": "",
		}),
		sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{"type": "function_call", "id": "a", "arguments": ""},
		}),
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "a", "delta": `{"a":1}`,
		}),
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "a", "delta": " ",
		}),
		sseEvent("response.completed", map[string]any{
			"type": "response.completed", "response": map[string]any{"id": "resp-1"},
		}),
	}
	guard := NewSSEGuard(StreamLimits{})
	var got []byte
	var result StreamResult
	for i, event := range events {
		out, step := guard.Push(event)
		got = append(got, out...)
		result.add(step)
		if i >= 3 && i <= 4 && len(terminalSequence(got)) != 0 {
			t.Fatalf("valid prefix finalized held terminals at step %d: %v", i, terminalSequence(got))
		}
	}
	assertDoneArguments(t, got, `{"a":1} `, 2)
	if result.RepairedEvents != 2 || result.ValidationErrors != 0 || !result.Changed {
		t.Fatalf("result = %#v", result)
	}
}

func TestSSEGuardDuplicateTerminalsFailOpen(t *testing.T) {
	arguments := `{"x":1}`
	cases := []struct {
		name   string
		events [][]byte
	}{
		{
			name: "duplicate function done",
			events: [][]byte{
				sseEvent("response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "item_id": "a", "delta": arguments,
				}),
				sseEvent("response.function_call_arguments.done", map[string]any{
					"type": "response.function_call_arguments.done", "item_id": "a", "arguments": arguments,
				}),
				sseEvent("response.function_call_arguments.done", map[string]any{
					"type": "response.function_call_arguments.done", "item_id": "a", "arguments": arguments,
				}),
			},
		},
		{
			name: "duplicate output done after completion",
			events: [][]byte{
				sseEvent("response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "item_id": "a", "delta": arguments,
				}),
				sseEvent("response.function_call_arguments.done", map[string]any{
					"type": "response.function_call_arguments.done", "item_id": "a", "arguments": arguments,
				}),
				sseEvent("response.output_item.done", map[string]any{
					"type": "response.output_item.done",
					"item": map[string]any{"type": "function_call", "id": "a", "arguments": arguments},
				}),
				sseEvent("response.output_item.done", map[string]any{
					"type": "response.output_item.done",
					"item": map[string]any{"type": "function_call", "id": "a", "arguments": arguments},
				}),
			},
		},
		{
			name: "duplicate held function done",
			events: [][]byte{
				sseEvent("response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "item_id": "a", "delta": "",
				}),
				sseEvent("response.function_call_arguments.done", map[string]any{
					"type": "response.function_call_arguments.done", "item_id": "a", "arguments": "",
				}),
				sseEvent("response.function_call_arguments.done", map[string]any{
					"type": "response.function_call_arguments.done", "item_id": "a", "arguments": "",
				}),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := bytes.Join(tc.events, nil)
			guard := NewSSEGuard(StreamLimits{})
			got, result := guard.Push(input)
			if !bytes.Equal(got, input) {
				t.Fatalf("fail-open changed bytes\n got: %q\nwant: %q", got, input)
			}
			if result.ValidationErrors == 0 || result.Changed {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestSSEGuardResponseTerminalBeforeValidDeltaFailsOpen(t *testing.T) {
	events := [][]byte{
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "a", "delta": "",
		}),
		sseEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "a", "arguments": "",
		}),
		sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{"type": "function_call", "id": "a", "arguments": ""},
		}),
		sseEvent("response.completed", map[string]any{
			"type": "response.completed", "response": map[string]any{"id": "resp-1"},
		}),
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "a", "delta": `{"a":1}`,
		}),
	}
	input := bytes.Join(events, nil)
	guard := NewSSEGuard(StreamLimits{})
	got, result := guard.Push(input)
	if !bytes.Equal(got, input) {
		t.Fatalf("terminal fail-open changed bytes\n got: %q\nwant: %q", got, input)
	}
	if result.ValidationErrors == 0 || result.RepairedEvents != 0 || result.Changed {
		t.Fatalf("result = %#v", result)
	}
	if flushed, flushResult := guard.Flush(); len(flushed) != 0 || flushResult != (StreamResult{}) {
		t.Fatalf("terminal leaked state: flushed=%q result=%#v", flushed, flushResult)
	}
}

func TestSSEGuardChangedEventPreservesNonDataFieldsAndCRLF(t *testing.T) {
	arguments := `{"x":1}`
	delta := sseEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "call-1", "delta": arguments,
	})
	done := []byte("id: 7\r\n: keep this\r\nevent: response.function_call_arguments.done\r\nretry: 100\r\ndata: {\"arguments\":\"{\",\r\ndata: \"item_id\":\"call-1\",\"type\":\"response.function_call_arguments.done\"}\r\n\r\n")
	guard := NewSSEGuard(StreamLimits{})
	input := append(delta, done...)
	input = append(input, sseEvent("response.completed", map[string]any{
		"type": "response.completed", "response": map[string]any{"id": "resp-1"},
	})...)
	got, result := guard.Push(input)
	if result.RepairedEvents != 1 || !result.Changed {
		t.Fatalf("result = %#v", result)
	}
	for _, preserved := range []string{"id: 7\r\n", ": keep this\r\n", "retry: 100\r\n"} {
		if !bytes.Contains(got, []byte(preserved)) {
			t.Fatalf("missing preserved field %q in %q", preserved, got)
		}
	}
	if bytes.Contains(got, []byte("\ndata: \"item_id\"")) {
		t.Fatalf("changed multiline payload retained invalid continuation: %q", got)
	}
	assertDoneArguments(t, got, arguments, 1)
}

func TestSSEGuardMalformedAfterHeldDoneFailsOpenByteIdentically(t *testing.T) {
	input := append(sseEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "call-1", "delta": "",
	}), sseEvent("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": "call-1", "arguments": "",
	})...)
	input = append(input, []byte("event: response.function_call_arguments.delta\ndata: {bad\n\n")...)

	guard := NewSSEGuard(StreamLimits{})
	got, result := guard.Push(input)
	if !bytes.Equal(got, input) {
		t.Fatalf("fail-open changed bytes\n got: %q\nwant: %q", got, input)
	}
	if result.ValidationErrors == 0 || result.Changed {
		t.Fatalf("result = %#v", result)
	}
}

func TestSSEGuardMismatchedTerminalItemIDsFailOpenByteIdentically(t *testing.T) {
	arguments := `{"x":1}`
	input := append(sseEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "call-a", "delta": arguments,
	}), sseEvent("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": "call-a", "arguments": "{",
	})...)
	input = append(input, sseEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done",
		"item": map[string]any{
			"type": "function_call", "id": "call-b", "arguments": "{",
		},
	})...)
	input = append(input, sseEvent("response.completed", map[string]any{
		"type": "response.completed", "response": map[string]any{"id": "resp-1"},
	})...)

	guard := NewSSEGuard(StreamLimits{})
	got, result := guard.Push(input)
	if !bytes.Equal(got, input) {
		t.Fatalf("mismatched IDs changed bytes\n got: %q\nwant: %q", got, input)
	}
	if result.ValidationErrors != 1 || result.RepairedEvents != 0 || result.Changed {
		t.Fatalf("result = %#v", result)
	}
	if flushed, flushResult := guard.Flush(); len(flushed) != 0 || flushResult != (StreamResult{}) {
		t.Fatalf("mismatched IDs leaked state: flushed=%q result=%#v", flushed, flushResult)
	}
}

func TestSSEGuardLimitsAndHoldTimeoutFailOpen(t *testing.T) {
	t.Run("argument limit", func(t *testing.T) {
		input := sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "call-1", "delta": `{"long":true}`,
		})
		guard := NewSSEGuard(StreamLimits{MaxArgumentBytes: 4})
		got, result := guard.Push(input)
		if !bytes.Equal(got, input) || result.ValidationErrors == 0 || result.Changed {
			t.Fatalf("got=%q result=%#v", got, result)
		}
	})

	t.Run("active call limit", func(t *testing.T) {
		input := append(sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "call-1", "delta": `{"x":1}`,
		}), sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "call-2", "delta": `{"x":2}`,
		})...)
		guard := NewSSEGuard(StreamLimits{MaxActiveCalls: 1})
		got, result := guard.Push(input)
		if !bytes.Equal(got, input) || result.ValidationErrors != 1 || result.Changed {
			t.Fatalf("got=%q result=%#v", got, result)
		}
		if tail, tailResult := guard.Push([]byte("opaque tail")); string(tail) != "opaque tail" ||
			tailResult != (StreamResult{}) {
			t.Fatalf("bypass tail=%q result=%#v", tail, tailResult)
		}
	})

	t.Run("event limit", func(t *testing.T) {
		input := sseEvent("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "delta": "oversized",
		})
		guard := NewSSEGuard(StreamLimits{MaxEventBytes: len(input) - 1})
		got, result := guard.Push(input)
		if !bytes.Equal(got, input) || result.ValidationErrors != 1 || result.Changed {
			t.Fatalf("got=%q result=%#v", got, result)
		}
	})

	t.Run("held byte limit", func(t *testing.T) {
		delta := sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "call-1", "delta": "",
		})
		done := sseEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "call-1", "arguments": "",
		})
		input := append(append([]byte(nil), delta...), done...)
		guard := NewSSEGuard(StreamLimits{MaxHeldBytes: len(done) - 1})
		got, result := guard.Push(input)
		if !bytes.Equal(got, input) || result.ValidationErrors != 1 || result.Changed {
			t.Fatalf("got=%q result=%#v", got, result)
		}
	})

	t.Run("hold timeout", func(t *testing.T) {
		now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
		input := append(sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "call-1", "delta": "",
		}), sseEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "call-1", "arguments": "",
		})...)
		guard := NewSSEGuard(StreamLimits{
			MaxHoldDuration: time.Second,
			Now:             func() time.Time { return now },
		})
		got, first := guard.Push(input)
		if bytes.Contains(got, []byte("function_call_arguments.done")) || first.ValidationErrors != 0 {
			t.Fatalf("done was not held: got=%q result=%#v", got, first)
		}
		now = now.Add(time.Second)
		flushed, result := guard.FlushExpired()
		if !bytes.Contains(flushed, []byte("function_call_arguments.done")) ||
			result.ValidationErrors == 0 || result.Changed {
			t.Fatalf("flushed=%q result=%#v", flushed, result)
		}
	})
}

func TestSSEGuardFlushClearsMissingDoneState(t *testing.T) {
	delta := sseEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "call-1", "delta": `{"x":1}`,
	})
	guard := NewSSEGuard(StreamLimits{})
	got, _ := guard.Push(delta)
	if !bytes.Equal(got, delta) {
		t.Fatalf("delta changed: %q", got)
	}
	flushed, result := guard.Flush()
	if len(flushed) != 0 || result.ValidationErrors != 1 {
		t.Fatalf("flushed=%q result=%#v", flushed, result)
	}
	if second, result := guard.Flush(); len(second) != 0 || result != (StreamResult{}) {
		t.Fatalf("state leaked: flushed=%q result=%#v", second, result)
	}
}

func TestSSEGuardFlushReleasesHeldBytesAndClearsState(t *testing.T) {
	delta := sseEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "call-1", "delta": "",
	})
	done := sseEvent("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": "call-1", "arguments": "",
	})
	guard := NewSSEGuard(StreamLimits{})
	got, result := guard.Push(append(append([]byte(nil), delta...), done...))
	if !bytes.Equal(got, delta) || result != (StreamResult{}) {
		t.Fatalf("held setup got=%q result=%#v", got, result)
	}
	flushed, flushResult := guard.Flush()
	if !bytes.Equal(flushed, done) ||
		flushResult.ValidationErrors != 1 ||
		flushResult.RepairedEvents != 0 ||
		flushResult.Changed {
		t.Fatalf("flushed=%q result=%#v", flushed, flushResult)
	}

	complete := sseEvent("response.completed", map[string]any{
		"type": "response.completed", "response": map[string]any{"id": "resp-2"},
	})
	second, secondResult := guard.Push(complete)
	if !bytes.Equal(second, complete) || secondResult != (StreamResult{}) {
		t.Fatalf("state leaked into next stream: got=%q result=%#v", second, secondResult)
	}
	if tail, tailResult := guard.Flush(); len(tail) != 0 || tailResult != (StreamResult{}) {
		t.Fatalf("second flush leaked state: flushed=%q result=%#v", tail, tailResult)
	}
}

func sseEvent(eventType string, data map[string]any) []byte {
	payload, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return []byte("event: " + eventType + "\ndata: " + string(payload) + "\n\n")
}

func assertDoneArguments(t *testing.T, stream []byte, want string, wantCount int) {
	t.Helper()
	count := 0
	remaining := stream
	for len(remaining) > 0 {
		end := completeEventEnd(remaining, false)
		if end == 0 {
			break
		}
		event, ok := parseSSEEvent(remaining[:end])
		remaining = remaining[end:]
		if !ok {
			continue
		}
		switch event.eventType {
		case "response.function_call_arguments.done":
			if event.data["arguments"] != want {
				t.Fatalf("function done arguments = %#v, want %q", event.data["arguments"], want)
			}
			count++
		case "response.output_item.done":
			item, _ := event.data["item"].(map[string]any)
			if item != nil && item["type"] == "function_call" {
				if item["arguments"] != want {
					t.Fatalf("output done arguments = %#v, want %q", item["arguments"], want)
				}
				count++
			}
		}
	}
	if count != wantCount {
		t.Fatalf("checked %d done events, want %d\n%s", count, wantCount, stream)
	}
}

func terminalSequence(stream []byte) []string {
	var sequence []string
	for _, block := range strings.Split(string(stream), "\n\n") {
		dataAt := strings.Index(block, "data: ")
		if dataAt < 0 {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(block[dataAt+len("data: "):]), &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.function_call_arguments.done":
			if id, ok := event["item_id"].(string); ok {
				sequence = append(sequence, "function:"+id)
			}
		case "response.output_item.done":
			if item, ok := event["item"].(map[string]any); ok {
				if id, ok := item["id"].(string); ok {
					sequence = append(sequence, "output:"+id)
				}
			}
		}
	}
	return sequence
}
