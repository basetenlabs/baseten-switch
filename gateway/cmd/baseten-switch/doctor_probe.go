package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type doctorProbeTarget struct {
	ProtocolShape string
	BindAddr      string
}

type doctorProbeResult struct {
	Status          int
	LatencyMs       int64
	Model           string
	Error           string
	FallbackTrigger string

	// DoorVia is the X-Baseten-Switch-Door response header: "router" when the
	// router answered and "fallback" when the door's native failover
	// served the request.
	DoorVia string
}

// doctorProbeRequestModel is the model ID requested by the live doctor
// probe. The served-route check matches telemetry against this same value
// so concurrent harness requests cannot be mistaken for the probe.
func doctorProbeRequestModel(shape string) string {
	if shape == "openai" {
		return "gpt-4o"
	}
	return "claude-opus-4-8"
}

func doctorProbeClient(c *http.Client, target doctorProbeTarget) *doctorProbeResult {
	shape := target.ProtocolShape
	if shape == "" {
		shape = "anthropic"
	}
	url := "http://" + target.BindAddr
	var body []byte
	switch shape {
	case "anthropic":
		url += "/v1/messages"
		body = []byte(fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`, doctorProbeRequestModel(shape)))
	case "openai":
		url += "/v1/chat/completions"
		body = []byte(fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`, doctorProbeRequestModel(shape)))
	case "monitor":
		return &doctorProbeResult{Error: "monitor shape has no probe"}
	default:
		return &doctorProbeResult{Error: "unknown shape: " + shape}
	}

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &doctorProbeResult{Error: err.Error()}
	}
	req.Header.Set("content-type", "application/json")
	resp, err := c.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return &doctorProbeResult{Error: err.Error(), LatencyMs: latency}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	doorVia := resp.Header.Get("X-Baseten-Switch-Door")
	fallbackTrigger := ""
	if doorVia == "router" {
		fallbackTrigger = resp.Header.Get("X-Baseten-Switch-Fallback")
	}
	result := &doctorProbeResult{
		Status:          resp.StatusCode,
		LatencyMs:       latency,
		DoorVia:         doorVia,
		FallbackTrigger: fallbackTrigger,
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var payload struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(respBody, &payload) == nil && payload.Model != "" {
			result.Model = payload.Model
		}
	} else {
		result.Error = oneLine(string(respBody))
	}
	return result
}

func doctorProbeOK(result *doctorProbeResult) bool {
	return result != nil && result.Status >= 200 && result.Status < 300
}

func confirmDoctorProbes(targets []doctorProbeTarget) (bool, error) {
	count := 0
	for _, target := range targets {
		shape := target.ProtocolShape
		if shape == "" {
			shape = "anthropic"
		}
		if shape != "monitor" {
			count++
		}
	}
	if count == 0 {
		return true, nil
	}
	fmt.Fprintf(os.Stderr, "About to fire %d real 1-token inference request(s) through the gateway.\n", count)
	fmt.Fprintln(os.Stderr, "Each request may cost a fraction of a cent on the upstream provider (baseten/anthropic/openai).")
	fmt.Fprint(os.Stderr, "Proceed? [y/N] ")
	var response string
	fmt.Fscanln(os.Stdin, &response)
	return strings.EqualFold(strings.TrimSpace(response), "y"), nil
}
