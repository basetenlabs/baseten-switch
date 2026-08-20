package gateway

import (
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func adminTracePolicy(enabled bool, clients ...string) *config.File {
	routing := false
	return &config.File{
		Global: config.Global{
			RoutingEnabled: &routing,
			TraceCapture: &config.TraceCapture{
				Enabled: enabled,
				Clients: clients,
			},
		},
		Clients: []config.Client{
			{Name: "claude-code", Enabled: true, BindAddr: "127.0.0.1:18081", ProtocolShape: "anthropic", DefaultModel: "example/model"},
			{Name: "codex", Enabled: true, BindAddr: "127.0.0.1:18082", ProtocolShape: "openai", DefaultModel: "example/model"},
		},
	}
}

func TestValidateAdminTraceCaptureMutation(t *testing.T) {
	tests := []struct {
		name     string
		active   *config.File
		proposed *config.File
		want     string
	}{
		{
			name:     "reject activation",
			active:   adminTracePolicy(false, "claude-code"),
			proposed: adminTracePolicy(true, "claude-code"),
			want:     "cannot be enabled",
		},
		{
			name:     "reject expansion",
			active:   adminTracePolicy(true, "claude-code"),
			proposed: adminTracePolicy(true, "claude-code", "codex"),
			want:     "cannot be expanded",
		},
		{
			name:     "allow narrowing",
			active:   adminTracePolicy(true, "claude-code", "codex"),
			proposed: adminTracePolicy(true, "claude-code"),
		},
		{
			name:     "allow disable",
			active:   adminTracePolicy(true, "claude-code"),
			proposed: adminTracePolicy(false, "claude-code", "codex"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAdminTraceCaptureMutation(tt.active, tt.proposed)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
