package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoctorProbeAcceptsTwoTokenMinimum(t *testing.T) {
	for _, tc := range []struct {
		shape string
		path  string
		model string
	}{
		{shape: "anthropic", path: "/v1/messages", model: "claude-opus-4-8"},
		{shape: "openai", path: "/v1/chat/completions", model: "gpt-4o"},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != tc.path {
					t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, tc.path)
					http.NotFound(w, r)
					return
				}
				if got := r.Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
				var request struct {
					Model     string `json:"model"`
					MaxTokens int    `json:"max_tokens"`
					Messages  []struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode probe: %v", err)
					http.Error(w, "invalid JSON", http.StatusBadRequest)
					return
				}
				if request.MaxTokens != 2 {
					t.Errorf("max_tokens = %d, want 2", request.MaxTokens)
				}
				if request.MaxTokens < 2 {
					http.Error(w, "max_tokens must be at least 2", http.StatusBadRequest)
					return
				}
				if request.Model != tc.model {
					t.Errorf("model = %q, want %q", request.Model, tc.model)
				}
				if len(request.Messages) != 1 || request.Messages[0].Role != "user" || request.Messages[0].Content != "ping" {
					t.Errorf("messages = %+v, want one user ping", request.Messages)
				}
				w.Header().Set("X-Baseten-Switch-Door", "router")
				fmt.Fprint(w, `{"model":"served-model"}`)
			}))
			defer srv.Close()

			result := doctorProbeClient(&http.Client{Timeout: time.Second}, doctorProbeTarget{
				ProtocolShape: tc.shape,
				BindAddr:      strings.TrimPrefix(srv.URL, "http://"),
			})
			if !doctorProbeOK(result) {
				t.Fatalf("probe failed against two-token minimum: %+v", result)
			}
			if result.Status != http.StatusOK || result.Model != "served-model" || result.DoorVia != "router" {
				t.Errorf("probe result = %+v, want status 200, served-model, router", result)
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("requests = %d, want 1", got)
			}
		})
	}
}
