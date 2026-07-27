// Package requestcapability inspects protocol request bodies for capabilities
// that affect routing.
package requestcapability

import (
	"bytes"
	"encoding/json"
)

// Endpoint identifies the request schema to inspect.
type Endpoint string

const (
	AnthropicMessages Endpoint = "anthropic-messages"
	OpenAIChat        Endpoint = "openai-chat"
	OpenAIResponses   Endpoint = "openai-responses"
)

// Inspection is the capability result for one request body.
//
// Malformed reports invalid JSON only. A syntactically valid body with an
// unexpected shape has no detected image and remains the caller's responsibility
// to validate.
type Inspection struct {
	HasImage  bool
	Malformed bool
	Stateful  bool
}

// Inspect checks only the image-bearing locations defined by endpoint. It does
// not decode image payloads, fetch image URLs, or search arbitrary nested data.
func Inspect(endpoint Endpoint, body []byte) Inspection {
	if !json.Valid(body) {
		return Inspection{Malformed: true}
	}

	var hasImage bool
	var stateful bool
	switch endpoint {
	case AnthropicMessages:
		hasImage = anthropicMessagesHasImage(body)
	case OpenAIChat:
		hasImage = openAIChatHasImage(body)
	case OpenAIResponses:
		hasImage, stateful = inspectOpenAIResponses(body)
	}
	return Inspection{HasImage: hasImage, Stateful: stateful}
}

type jsonString string

func (value *jsonString) UnmarshalJSON(data []byte) error {
	if firstJSONByte(data) != '"' {
		return nil
	}
	var decoded string
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil
	}
	*value = jsonString(decoded)
	return nil
}

type jsonArray[T any] []T

func (items *jsonArray[T]) UnmarshalJSON(data []byte) error {
	if firstJSONByte(data) != '[' {
		return nil
	}
	type plainArray []T
	var decoded plainArray
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil
	}
	*items = jsonArray[T](decoded)
	return nil
}

type jsonObject[T any] struct {
	value T
}

func (object *jsonObject[T]) UnmarshalJSON(data []byte) error {
	if firstJSONByte(data) != '{' {
		return nil
	}
	return json.Unmarshal(data, &object.value)
}

func firstJSONByte(data []byte) byte {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}

type anthropicEnvelope struct {
	Messages jsonArray[jsonObject[anthropicMessage]] `json:"messages"`
}

type anthropicMessage struct {
	Role    jsonString                              `json:"role"`
	Content jsonArray[jsonObject[anthropicContent]] `json:"content"`
}

type anthropicContent struct {
	Type    jsonString                        `json:"type"`
	Content jsonArray[jsonObject[typeMarker]] `json:"content"`
}

type typeMarker struct {
	Type jsonString `json:"type"`
}

func anthropicMessagesHasImage(body []byte) bool {
	var envelope anthropicEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	for _, messageValue := range envelope.Messages {
		message := messageValue.value
		if string(message.Role) != "user" {
			continue
		}
		for _, contentValue := range message.Content {
			content := contentValue.value
			switch string(content.Type) {
			case "image":
				return true
			case "tool_result":
				for _, child := range content.Content {
					if string(child.value.Type) == "image" {
						return true
					}
				}
			}
		}
	}
	return false
}

type openAIChatEnvelope struct {
	Messages jsonArray[jsonObject[openAIChatMessage]] `json:"messages"`
}

type openAIChatMessage struct {
	Role    jsonString                        `json:"role"`
	Content jsonArray[jsonObject[typeMarker]] `json:"content"`
}

func openAIChatHasImage(body []byte) bool {
	var envelope openAIChatEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	for _, messageValue := range envelope.Messages {
		message := messageValue.value
		if string(message.Role) != "user" {
			continue
		}
		if arrayHasDirectType(message.Content, "image_url") {
			return true
		}
	}
	return false
}

type openAIResponsesEnvelope struct {
	Input              jsonArray[jsonObject[openAIResponsesItem]] `json:"input"`
	PreviousResponseID nonEmptyJSONString                         `json:"previous_response_id"`
	Conversation       responsesConversation                      `json:"conversation"`
}

type openAIResponsesItem struct {
	Type    jsonString                        `json:"type"`
	Role    jsonString                        `json:"role"`
	Content jsonArray[jsonObject[typeMarker]] `json:"content"`
	Output  openAIResponsesOutput             `json:"output"`
}

type openAIResponsesOutput struct {
	items  jsonArray[jsonObject[typeMarker]]
	marker jsonObject[typeMarker]
}

func (output *openAIResponsesOutput) UnmarshalJSON(data []byte) error {
	switch firstJSONByte(data) {
	case '[':
		return json.Unmarshal(data, &output.items)
	case '{':
		return json.Unmarshal(data, &output.marker)
	default:
		return nil
	}
}

func inspectOpenAIResponses(body []byte) (hasImage, stateful bool) {
	var envelope openAIResponsesEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		return false, false
	}
	stateful = bool(envelope.PreviousResponseID) || bool(envelope.Conversation)
	for _, itemValue := range envelope.Input {
		item := itemValue.value
		switch string(item.Type) {
		case "", "message":
			if !isResponsesMessageRole(string(item.Role)) {
				continue
			}
			if arrayHasDirectType(item.Content, "input_image") {
				return true, stateful
			}
		case "function_call_output", "custom_tool_call_output":
			if arrayHasDirectType(item.Output.items, "input_image") {
				return true, stateful
			}
		case "computer_call_output":
			if string(item.Output.marker.value.Type) == "computer_screenshot" {
				return true, stateful
			}
		}
	}
	return false, stateful
}

type nonEmptyJSONString bool

func (value *nonEmptyJSONString) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	*value = len(trimmed) > 2 && firstJSONByte(trimmed) == '"'
	return nil
}

type responsesConversation bool

func (value *responsesConversation) UnmarshalJSON(data []byte) error {
	*value = false
	switch firstJSONByte(data) {
	case '"':
		var id nonEmptyJSONString
		if err := id.UnmarshalJSON(data); err != nil {
			return nil
		}
		*value = responsesConversation(id)
	case '{':
		var reference struct {
			ID nonEmptyJSONString `json:"id"`
		}
		if err := json.Unmarshal(data, &reference); err != nil {
			return nil
		}
		*value = responsesConversation(reference.ID)
	}
	return nil
}

func isResponsesMessageRole(role string) bool {
	switch role {
	case "user", "assistant", "system", "developer":
		return true
	default:
		return false
	}
}

func arrayHasDirectType(items jsonArray[jsonObject[typeMarker]], want string) bool {
	for _, item := range items {
		if string(item.value.Type) == want {
			return true
		}
	}
	return false
}
