package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

func traceResponseIDs(traces []TraceReference) map[string][]string {
	result := make(map[string][]string, len(traces))
	for _, trace := range traces {
		seen := make(map[string]struct{})
		add := func(value string) {
			if validMessagesResponseID(value) {
				seen[value] = struct{}{}
			}
		}
		var unary struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(trace.ResponseBody, &unary) == nil {
			add(unary.ID)
		}
		scanner := bufio.NewScanner(bytes.NewReader(trace.ResponseBody))
		scanner.Buffer(make([]byte, 4<<10), 16<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var event struct {
				ID      string `json:"id"`
				Message *struct {
					ID string `json:"id"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(payload), &event) == nil {
				add(event.ID)
				if event.Message != nil {
					add(event.Message.ID)
				}
			}
		}
		for responseID := range seen {
			result[trace.EventID] = append(result[trace.EventID], responseID)
		}
	}
	return result
}

func validMessagesResponseID(value string) bool {
	if !strings.HasPrefix(value, "msg_") || len(value) > 256 {
		return false
	}
	for _, char := range value[4:] {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_') {
			return false
		}
	}
	return len(value) > 4
}
