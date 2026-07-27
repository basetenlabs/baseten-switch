// Package responsescompat contains isolated compatibility safeguards for
// OpenAI Responses-shaped requests and streams.
package responsescompat

import "time"

const (
	RuleTextFormatDefault            = "responses.text_format_default"
	RuleAdditionalToolsInput         = "responses.additional_tools_input"
	RuleReasoningEffort              = "responses.reasoning_effort"
	RuleFunctionArgumentsConsistency = "responses.function_arguments_consistency"
	RuleToolTypeDenylist             = "responses.tool_type_denylist"
)

type Mode string

const (
	ModeOn  Mode = "on"
	ModeOff Mode = "off"
)

const (
	DefaultMaxArgumentBytes = 1 << 20
	DefaultMaxActiveCalls   = 32
	DefaultMaxEventBytes    = 2 << 20
	DefaultMaxHeldBytes     = 2 << 20
	DefaultMaxHoldDuration  = 30 * time.Second
)
