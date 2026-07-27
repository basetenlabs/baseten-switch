// Package upstreamerror classifies narrowly banked upstream error responses.
package upstreamerror

import (
	"bytes"
	"encoding/json"
	"io"
)

type EndpointKind string

const (
	EndpointMessages        EndpointKind = "messages"
	EndpointChatCompletions EndpointKind = "chat_completions"
	EndpointResponses       EndpointKind = "responses"
)

const (
	BasetenMultimodalUnsupportedClassifierID = "baseten.multimodal_input_unsupported.v1"
	BasetenMultimodalUnsupportedMessage      = "This model does not support multimodal (image/video/audio) inputs."
	MaxClassifierBodyBytes                   = 64 << 10
)

// IsBasetenMultimodalUnsupported400 reports whether an identity-encoded,
// bounded response body is the banked Baseten multimodal-input rejection.
func IsBasetenMultimodalUnsupported400(kind EndpointKind, statusCode int, body []byte) bool {
	if statusCode != 400 || len(body) == 0 || len(body) > MaxClassifierBodyBytes {
		return false
	}

	switch kind {
	case EndpointChatCompletions:
		return matchesBadRequest(body)
	case EndpointMessages:
		return matchesMessages(body)
	case EndpointResponses:
		return matchesBadRequest(body)
	default:
		return false
	}
}

func matchesMessages(body []byte) bool {
	var (
		envelopeType string
		errorType    string
		errorMessage string
		sawType      bool
		sawError     bool
	)
	ok := walkObject(body, func(key string, raw json.RawMessage) bool {
		switch key {
		case "type":
			if sawType || !decodeString(raw, &envelopeType) {
				return false
			}
			sawType = true
		case "error":
			if sawError {
				return false
			}
			sawError = true
			kind, message, valid := directMessagesError(raw)
			if !valid {
				return false
			}
			errorType = kind
			errorMessage = message
		}
		return true
	})
	return ok &&
		sawType &&
		envelopeType == "error" &&
		sawError &&
		errorType == "invalid_request_error" &&
		errorMessage == BasetenMultimodalUnsupportedMessage
}

func matchesBadRequest(body []byte) bool {
	var (
		message string
		kind    string
		code    int
		sawMsg  bool
		sawType bool
		sawCode bool
	)
	ok := walkObject(body, func(key string, raw json.RawMessage) bool {
		switch key {
		case "message":
			if sawMsg || !decodeString(raw, &message) {
				return false
			}
			sawMsg = true
		case "type":
			if sawType || !decodeString(raw, &kind) {
				return false
			}
			sawType = true
		case "code":
			if sawCode || json.Unmarshal(raw, &code) != nil {
				return false
			}
			sawCode = true
		}
		return true
	})
	return ok &&
		sawMsg &&
		message == BasetenMultimodalUnsupportedMessage &&
		sawType &&
		kind == "Bad Request" &&
		sawCode &&
		code == 400
}

func directMessagesError(body []byte) (kind, message string, valid bool) {
	var sawType, sawMessage bool
	valid = walkObject(body, func(key string, raw json.RawMessage) bool {
		switch key {
		case "type":
			if sawType || !decodeString(raw, &kind) {
				return false
			}
			sawType = true
		case "message":
			if sawMessage || !decodeString(raw, &message) {
				return false
			}
			sawMessage = true
		}
		return true
	})
	return kind, message, valid && sawType && sawMessage
}

func decodeString(raw json.RawMessage, dst *string) bool {
	return json.Unmarshal(raw, dst) == nil
}

func walkObject(body []byte, visit func(string, json.RawMessage) bool) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := keyToken.(string)
		if !ok {
			return false
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil || !visit(key, raw) {
			return false
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return false
	}
	_, err = decoder.Token()
	return err == io.EOF
}
