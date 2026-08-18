package config

import (
	"strings"
	"testing"
)

func tracePolicyFile(capture *TraceCapture) *File {
	routing := false
	return &File{
		Global: Global{RoutingEnabled: &routing, TraceCapture: capture},
		Clients: []Client{{
			Name:          "claude-code",
			Enabled:       true,
			BindAddr:      "127.0.0.1:18081",
			ProtocolShape: "anthropic",
			DefaultModel:  "example/model",
		}},
		Door: &Door{Ports: []DoorPort{{
			BindAddr:   "127.0.0.1:8081",
			RouterAddr: "127.0.0.1:18081",
		}}},
	}
}

func TestResolveTraceCaptureDefaultsDisabled(t *testing.T) {
	got, err := ResolveTraceCapture(tracePolicyFile(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.RetentionDays != DefaultTraceRetentionDays || len(got.Clients) != 0 {
		t.Fatalf("ResolveTraceCapture() = %#v", got)
	}
}

func TestResolveTraceCaptureEnabled(t *testing.T) {
	got, err := ResolveTraceCapture(tracePolicyFile(&TraceCapture{
		Enabled: true,
		Clients: []string{"claude-code"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.RetentionDays != 7 || len(got.Clients) != 1 || got.Clients[0] != "claude-code" {
		t.Fatalf("ResolveTraceCapture() = %#v", got)
	}
}

func TestResolveTraceCaptureRejectsUnsafeEnablement(t *testing.T) {
	tests := []struct {
		name string
		edit func(*File)
		want string
	}{
		{
			name: "empty allowlist",
			edit: func(f *File) { f.Global.TraceCapture.Clients = nil },
			want: "at least one client",
		},
		{
			name: "unknown client",
			edit: func(f *File) { f.Global.TraceCapture.Clients = []string{"missing"} },
			want: "must name an enabled client",
		},
		{
			name: "disabled client",
			edit: func(f *File) { f.Clients[0].Enabled = false },
			want: "must name an enabled client",
		},
		{
			name: "non loopback listener",
			edit: func(f *File) { f.Clients[0].BindAddr = "0.0.0.0:18081" },
			want: "must be loopback",
		},
		{
			name: "non loopback door",
			edit: func(f *File) { f.Door.Ports[0].BindAddr = "0.0.0.0:8081" },
			want: "door bind_addr",
		},
		{
			name: "duplicate",
			edit: func(f *File) { f.Global.TraceCapture.Clients = []string{"claude-code", "claude-code"} },
			want: "duplicate",
		},
		{
			name: "retention too high",
			edit: func(f *File) { f.Global.TraceCapture.RetentionDays = MaxTraceRetentionDays + 1 },
			want: "retention_days",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tracePolicyFile(&TraceCapture{Enabled: true, Clients: []string{"claude-code"}})
			tt.edit(f)
			_, err := ResolveTraceCapture(f)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ResolveTraceCapture() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestResolveTraceCaptureAllowsDisabledPreparedPolicy(t *testing.T) {
	f := tracePolicyFile(&TraceCapture{
		Enabled:       false,
		Clients:       []string{"future-client"},
		RetentionDays: 30,
	})
	got, err := ResolveTraceCapture(f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.RetentionDays != 30 || len(got.Clients) != 1 {
		t.Fatalf("ResolveTraceCapture() = %#v", got)
	}
}

func TestTraceCaptureStrictYAMLRoundTrip(t *testing.T) {
	raw := []byte(`global:
  routing_enabled: false
  trace_capture:
    enabled: true
    clients: [claude-code]
    retention_days: 14
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    default_model: example/model
`)
	var f File
	if err := UnmarshalStrict(raw, &f); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveTraceCapture(&f)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.RetentionDays != 14 {
		t.Fatalf("ResolveTraceCapture() = %#v", got)
	}
}
