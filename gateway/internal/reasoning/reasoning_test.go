package reasoning

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func glmInput() Input {
	return Input{
		Provider: "baseten", CanonicalModelID: "zai-org/GLM-5.2",
		WireShape: WireAnthropicMessages,
		Capability: Capability{
			Known: true, Supported: true, Toggle: true,
		},
	}
}

func glmFastEffortInput() Input {
	return Input{
		Provider:         "baseten",
		CanonicalModelID: "zai-org/GLM-5.2-Fast",
		WireShape:        WireAnthropicMessages,
		Capability: Capability{
			Known: true, Supported: true,
			Efforts: []string{"none", "high", "max"},
		},
	}
}

func TestResolveCompatibilityAndConfiguredPolicies(t *testing.T) {
	t.Run("toggle models default off independent of identity", func(t *testing.T) {
		for _, model := range []string{
			"zai-org/GLM-5.2",
			"moonshotai/Kimi-K2.7-Code",
			"nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B",
			"future-provider/Future-Toggle-Model",
		} {
			in := glmInput()
			in.CanonicalModelID = model
			got, err := Resolve(in)
			if err != nil {
				t.Fatalf("%s resolve: %v", model, err)
			}
			if got.Mode != ModeOff ||
				got.Source != SourceCompatibilityDefault {
				t.Fatalf(
					"%s decision = %+v, want compatibility Off",
					model,
					got,
				)
			}
		}
	})

	t.Run("models without validated Off default passthrough", func(t *testing.T) {
		for _, capability := range []Capability{
			{
				Known: true, Supported: true,
				Efforts: []string{"low", "high"},
			},
			{Known: true, Supported: true},
			{Known: true},
			{},
		} {
			in := glmInput()
			in.Capability = capability
			got, err := Resolve(in)
			if err != nil {
				t.Fatal(err)
			}
			if got.Mode != ModePassthrough ||
				got.Source != SourceInternalPassthrough {
				t.Fatalf("decision = %+v, want passthrough", got)
			}
		}
	})

	t.Run("reviewed exact compatibility defaults off", func(t *testing.T) {
		got, err := Resolve(glmFastEffortInput())
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != ModeOff || got.Source != SourceCompatibilityDefault {
			t.Fatalf("decision = %+v, want compatibility Off", got)
		}
	})

	t.Run("reviewed default compatibility is exact and validated", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func(*Input)
		}{
			{name: "neighboring model", mutate: func(in *Input) { in.CanonicalModelID += "-Preview" }},
			{name: "other provider", mutate: func(in *Input) { in.Provider = "example" }},
			{name: "other wire", mutate: func(in *Input) { in.WireShape = WireOpenAIResponses }},
			{name: "unknown metadata", mutate: func(in *Input) { in.Capability = Capability{} }},
			{name: "unsupported metadata", mutate: func(in *Input) { in.Capability.Supported = false }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				in := glmFastEffortInput()
				tc.mutate(&in)
				got, err := Resolve(in)
				if err != nil {
					t.Fatal(err)
				}
				if got.Mode != ModePassthrough || got.Source != SourceInternalPassthrough {
					t.Fatalf("decision = %+v, want passthrough", got)
				}
			})
		}
	})

	t.Run("configured On requires a reviewed binary control", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			input     Input
			wantError bool
		}{
			{name: "catalog toggle", input: glmInput()},
			{name: "reviewed exact model", input: glmFastEffortInput()},
			{
				name: "neighboring model",
				input: func() Input {
					in := glmFastEffortInput()
					in.CanonicalModelID = "zai-org/GLM-5.2-Fast-Preview"
					return in
				}(),
				wantError: true,
			},
			{
				name: "other adapter",
				input: func() Input {
					in := glmFastEffortInput()
					in.WireShape = WireOpenAIResponses
					return in
				}(),
				wantError: true,
			},
			{
				name: "unknown metadata",
				input: func() Input {
					in := glmFastEffortInput()
					in.Capability = Capability{}
					return in
				}(),
				wantError: true,
			},
			{
				name: "unsupported metadata",
				input: func() Input {
					in := glmFastEffortInput()
					in.Capability.Supported = false
					return in
				}(),
				wantError: true,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				in := tc.input
				in.Stored = StoredPolicy{Present: true, Mode: ModeOn}
				got, err := Resolve(in)
				if tc.wantError {
					if !IsPolicyError(err) || !AllowsFallback(err) {
						t.Fatalf("error = %v, want fallback-eligible policy error", err)
					}
					return
				}
				if err != nil || got.Mode != ModeOn || got.Source != SourceConfigured {
					t.Fatalf("decision = %+v error = %v", got, err)
				}
			})
		}
	})

	t.Run("reviewed explicit compatibility does not broaden follow harness", func(t *testing.T) {
		in := glmFastEffortInput()
		in.Stored = StoredPolicy{Present: true, Mode: ModeFollowHarness}
		if _, err := Resolve(in); !IsPolicyError(err) {
			t.Fatalf("error = %v, want policy error", err)
		}
	})

	t.Run("reviewed exact model accepts explicit Off", func(t *testing.T) {
		in := glmFastEffortInput()
		in.Stored = StoredPolicy{Present: true, Mode: ModeOff}
		got, err := Resolve(in)
		if err != nil || got.Mode != ModeOff || got.Source != SourceConfigured {
			t.Fatalf("decision = %+v error = %v", got, err)
		}
	})

	t.Run("follow harness preserves Messages semantics", func(t *testing.T) {
		in := glmInput()
		in.Stored = StoredPolicy{Present: true, Mode: ModeFollowHarness}
		got, err := Resolve(in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != ModeFollowHarness || got.Source != SourceConfigured {
			t.Fatalf("decision = %+v, want configured follow_harness", got)
		}
	})

	t.Run("follow harness validates exact capability", func(t *testing.T) {
		cases := []struct {
			name       string
			capability Capability
			requested  RequestedReasoning
			wantError  bool
		}{
			{
				name:       "unknown",
				capability: Capability{},
				wantError:  true,
			},
			{
				name: "unsupported",
				capability: Capability{
					Known: true,
				},
				wantError: true,
			},
			{
				name: "no verified control",
				capability: Capability{
					Known: true, Supported: true,
				},
				wantError: true,
			},
			{
				name: "effort only",
				capability: Capability{
					Known: true, Supported: true,
					Efforts: []string{"low", "high"},
				},
				wantError: true,
			},
			{
				name: "toggle preserves disabled",
				capability: Capability{
					Known: true, Supported: true, Toggle: true,
				},
				requested: RequestedReasoning{
					Present: true, Disabled: true, Recognized: true,
				},
			},
			{
				name: "budget preserves numeric budget",
				capability: Capability{
					Known: true, Supported: true, BudgetTokens: true,
				},
				requested: RequestedReasoning{
					Present: true, Recognized: true,
					BudgetTokens: int64Pointer(32000),
				},
			},
			{
				name: "Messages toggle preserves enabled budget object",
				capability: Capability{
					Known: true, Supported: true, Toggle: true,
				},
				requested: RequestedReasoning{
					Present: true, Recognized: true,
					BudgetTokens: int64Pointer(32000),
				},
			},
			{
				name: "malformed present control",
				capability: Capability{
					Known: true, Supported: true, Toggle: true,
				},
				requested: RequestedReasoning{Present: true},
				wantError: true,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				in := glmInput()
				in.Stored = StoredPolicy{
					Present: true, Mode: ModeFollowHarness,
				}
				in.Capability = tc.capability
				in.Requested = tc.requested
				_, err := Resolve(in)
				if tc.wantError != IsPolicyError(err) {
					t.Fatalf(
						"error = %v, want policy error %t",
						err,
						tc.wantError,
					)
				}
				if tc.name == "malformed present control" &&
					AllowsFallback(err) {
					t.Fatalf(
						"error = %v, malformed harness control must not allow fallback",
						err,
					)
				}
			})
		}
	})

	t.Run("fixed is unsupported", func(t *testing.T) {
		in := glmInput()
		in.Stored = StoredPolicy{
			Present: true, Mode: ModeFixed, Effort: "high",
		}
		_, err := Resolve(in)
		var policyErr *PolicyError
		if !errors.As(err, &policyErr) {
			t.Fatalf("error = %v, want PolicyError", err)
		}
	})

	t.Run("translated Chat is unsupported", func(t *testing.T) {
		in := glmInput()
		in.WireShape = WireTranslatedChat
		in.Stored = StoredPolicy{Present: true, Mode: ModeOff}
		_, err := Resolve(in)
		if !IsPolicyError(err) {
			t.Fatalf("error = %v, want reasoning policy error", err)
		}
	})

	t.Run("configured Off requires adapter toggle capability", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			capability Capability
			wantError  bool
		}{
			{
				name: "toggle",
				capability: Capability{
					Known: true, Supported: true, Toggle: true,
				},
			},
			{
				name: "effort only",
				capability: Capability{
					Known: true, Supported: true,
					Efforts: []string{"low", "high"},
				},
				wantError: true,
			},
			{
				name: "no controls",
				capability: Capability{
					Known: true, Supported: true,
				},
				wantError: true,
			},
			{
				name:       "unknown",
				capability: Capability{},
				wantError:  true,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				in := glmInput()
				in.Capability = tc.capability
				in.Stored = StoredPolicy{Present: true, Mode: ModeOff}
				got, err := Resolve(in)
				if tc.wantError {
					if !IsPolicyError(err) || !AllowsFallback(err) {
						t.Fatalf(
							"error = %v, want fallback-eligible policy error",
							err,
						)
					}
					return
				}
				if err != nil || got.Mode != ModeOff ||
					got.Source != SourceConfigured {
					t.Fatalf("decision = %+v error = %v", got, err)
				}
			})
		}
	})
}

func TestReviewedAdapterAvailability(t *testing.T) {
	t.Run("toggle capability is model identity independent", func(t *testing.T) {
		for _, model := range []string{
			"zai-org/GLM-5.2",
			"moonshotai/Kimi-K2.7-Code",
			"nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B",
			"future-provider/Future-Toggle-Model",
		} {
			in := glmInput()
			in.CanonicalModelID = model
			got := ReviewedAdapterAvailability(in)
			if len(got.Modes) != 3 ||
				got.Modes[0] != ModeOff ||
				got.Modes[1] != ModeOn ||
				got.Modes[2] != ModeFollowHarness ||
				len(got.Efforts) != 0 {
				t.Fatalf("%s availability = %+v", model, got)
			}
		}
	})

	t.Run("reviewed exact effort model offers only explicit On and Off", func(t *testing.T) {
		got := ReviewedAdapterAvailability(glmFastEffortInput())
		want := []Mode{ModeOff, ModeOn}
		if !reflect.DeepEqual(got.Modes, want) || len(got.Efforts) != 0 {
			t.Fatalf("availability = %+v, want modes %v", got, want)
		}
	})

	t.Run("reviewed compatibility is exact provider and model scoped", func(t *testing.T) {
		for _, mutate := range []func(*Input){
			func(in *Input) { in.Provider = "example" },
			func(in *Input) { in.CanonicalModelID += "-Preview" },
		} {
			in := glmFastEffortInput()
			mutate(&in)
			got := ReviewedAdapterAvailability(in)
			if len(got.Modes) != 0 || len(got.Efforts) != 0 {
				t.Fatalf("availability = %+v for input %+v", got, in)
			}
		}
	})

	t.Run("unreviewed wire shapes advertise nothing", func(t *testing.T) {
		for _, shape := range []WireShape{
			WireOpenAIChat,
			WireOpenAIResponses,
			WireTranslatedChat,
		} {
			in := glmInput()
			in.WireShape = shape
			got := ReviewedAdapterAvailability(in)
			if len(got.Modes) != 0 || len(got.Efforts) != 0 {
				t.Fatalf("%s availability = %+v", shape, got)
			}
		}
	})

	t.Run("capabilities without adapter controls advertise nothing", func(t *testing.T) {
		for _, capability := range []Capability{
			{},
			{Known: true},
			{Known: true, Supported: true},
			{
				Known: true, Supported: true,
				Efforts: []string{"low", "high"},
			},
		} {
			in := glmInput()
			in.Capability = capability
			got := ReviewedAdapterAvailability(in)
			if len(got.Modes) != 0 || len(got.Efforts) != 0 {
				t.Fatalf("availability = %+v", got)
			}
		}
	})
}

func TestEffectivePolicyPreservesUnavailableConfiguredValue(t *testing.T) {
	in := glmInput()
	in.Capability = Capability{}
	in.Stored = StoredPolicy{
		Present: true,
		Mode:    ModeFixed,
		Effort:  "removed",
	}
	got := EffectivePolicy(in)
	if got.Mode != ModeFixed ||
		got.Effort != "removed" ||
		got.Source != SourceConfigured {
		t.Fatalf("effective policy = %+v", got)
	}
}

func TestInspectAndApplyAnthropicMessages(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-4-8",
		"system":[{"type":"text","text":"keep system"}],
		"thinking":{"type":"enabled","budget_tokens":32000},
		"messages":[
			{"role":"assistant","content":[{"type":"thinking","thinking":"history"}]},
			{"role":"user","content":"hello"}
		],
		"metadata":{"user_id":"user-1"}
	}`)
	requested := InspectAnthropicMessages(body)
	if !requested.Present || requested.Disabled ||
		!requested.Recognized ||
		requested.BudgetTokens == nil || *requested.BudgetTokens != 32000 {
		t.Fatalf("requested = %+v", requested)
	}
	malformed := InspectAnthropicMessages(
		[]byte(`{"thinking":{"type":"future_mode"}}`),
	)
	if !malformed.Present || malformed.Recognized {
		t.Fatalf("malformed request = %+v, want present but unrecognized", malformed)
	}

	followed, err := ApplyAnthropicMessages(body, Decision{Mode: ModeFollowHarness})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(followed, body) {
		t.Fatal("follow_harness changed request bytes")
	}

	transformed, err := ApplyAnthropicMessages(
		body,
		Decision{Mode: ModeOff},
	)
	if err != nil {
		t.Fatal(err)
	}
	var before map[string]any
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal(transformed, &after); err != nil {
		t.Fatal(err)
	}
	thinking, ok := after["thinking"].(map[string]any)
	if !ok || len(thinking) != 1 || thinking["type"] != "disabled" {
		t.Fatalf("transformed thinking = %#v, want exactly type=disabled", after["thinking"])
	}
	delete(before, "thinking")
	delete(after, "thinking")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf(
			"Off changed non-thinking request content\n got: %#v\nwant: %#v",
			after,
			before,
		)
	}
}

func TestApplyAnthropicMessagesForcedOnAndOff(t *testing.T) {
	const largeInteger = "9007199254740993123456789"
	for _, mode := range []Mode{ModeOn, ModeOff} {
		for _, tc := range []struct {
			name     string
			thinking string
		}{
			{name: "absent"},
			{name: "disabled", thinking: `,"thinking":{"type":"disabled","display":"ignored"}`},
			{name: "adaptive", thinking: `,"thinking":{"type":"adaptive","display":"omitted"}`},
			{name: "enabled budget", thinking: `,"thinking":{"type":"enabled","budget_tokens":2047}`},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				body := []byte(`{"model":"example/model","system":"keep","max_tokens":4096,"large_integer":` +
					largeInteger +
					`,"output_config":{"effort":"medium"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"prior","signature":"sig_test"},{"type":"text","text":"ready"}]},{"role":"user","content":"hello"}]` +
					tc.thinking + `}`)
				var before map[string]json.RawMessage
				if err := json.Unmarshal(body, &before); err != nil {
					t.Fatal(err)
				}
				got, err := ApplyAnthropicMessages(body, Decision{Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				var envelope map[string]json.RawMessage
				if err := json.Unmarshal(got, &envelope); err != nil {
					t.Fatal(err)
				}
				wantType := "enabled"
				if mode == ModeOff {
					wantType = "disabled"
				}
				if string(envelope["thinking"]) != `{"type":"`+wantType+`"}` {
					t.Fatalf("thinking = %s", envelope["thinking"])
				}
				delete(before, "thinking")
				delete(envelope, "thinking")
				if !equalCompactRawMessages(before, envelope) {
					t.Fatalf("non-thinking fields changed\n got: %s\nwant: %s", got, body)
				}
			})
		}
	}
}

func equalCompactRawMessages(
	left map[string]json.RawMessage,
	right map[string]json.RawMessage,
) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		rightValue, ok := right[key]
		if !ok {
			return false
		}
		var leftCompact bytes.Buffer
		var rightCompact bytes.Buffer
		if json.Compact(&leftCompact, leftValue) != nil ||
			json.Compact(&rightCompact, rightValue) != nil ||
			!bytes.Equal(leftCompact.Bytes(), rightCompact.Bytes()) {
			return false
		}
	}
	return true
}

func TestClaudeAdaptiveThinkingInspectionAndFollowHarnessNormalization(
	t *testing.T,
) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"thinking":{"type":"adaptive","display":"omitted"},
		"output_config":{"effort":"high"},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	requested := InspectAnthropicMessages(body)
	if !requested.Present ||
		!requested.Recognized ||
		requested.Disabled ||
		requested.BudgetTokens != nil {
		t.Fatalf("adaptive requested reasoning = %+v", requested)
	}
	budgetOnly := glmInput()
	budgetOnly.Capability = Capability{
		Known: true, Supported: true, BudgetTokens: true,
	}
	budgetOnly.Stored = StoredPolicy{
		Present: true, Mode: ModeFollowHarness,
	}
	budgetOnly.Requested = requested
	if _, err := Resolve(budgetOnly); !IsPolicyError(err) {
		t.Fatalf(
			"adaptive request on budget-only target error = %v, want policy error",
			err,
		)
	}

	transformed, err := ApplyAnthropicMessages(
		body,
		Decision{Mode: ModeFollowHarness},
	)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(transformed, &envelope); err != nil {
		t.Fatal(err)
	}
	if string(envelope["thinking"]) != `{"type":"enabled"}` {
		t.Fatalf(
			"adaptive follow_harness thinking = %s, want exactly enabled",
			envelope["thinking"],
		)
	}
	if !bytes.Contains(
		envelope["output_config"],
		[]byte(`"effort":"high"`),
	) {
		t.Fatalf(
			"adaptive follow_harness output_config = %s, want preserved effort",
			envelope["output_config"],
		)
	}
}

func TestGLMFollowHarnessPreservesEnabledBudgetBytes(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-4-8",
		"thinking":{"type":"enabled","budget_tokens":32000},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	in := glmInput()
	in.Stored = StoredPolicy{Present: true, Mode: ModeFollowHarness}
	in.Requested = InspectAnthropicMessages(body)
	decision, err := Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	transformed, err := ApplyAnthropicMessages(body, decision)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transformed, body) {
		t.Fatalf(
			"GLM follow_harness changed enabled+budget bytes:\n got %s\nwant %s",
			transformed,
			body,
		)
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}
