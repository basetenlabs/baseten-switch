package door

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

func client(name, bindAddr, shape string, enabled bool) config.Client {
	return config.Client{Name: name, Enabled: enabled, BindAddr: bindAddr, ProtocolShape: shape}
}

func doorFile(d *config.Door, clients ...config.Client) *config.File {
	return &config.File{Clients: clients, Door: d}
}

func TestSpecsFromConfig(t *testing.T) {
	const (
		routerA = "127.0.0.1:18081"
		bindA   = "127.0.0.1:8081"
	)
	anthropicRule := FallbackRule{Prefix: "/v1/messages", Shape: ShapeAnthropic, Base: DefaultAnthropicBase}
	openaiRules := []FallbackRule{
		{Prefix: "/v1/chat/completions", Shape: ShapeOpenAI, Base: DefaultOpenAIBase},
		{Prefix: "/v1/responses", Shape: ShapeOpenAI, Base: DefaultOpenAIBase},
	}
	onePort := &config.Door{Ports: []config.DoorPort{{BindAddr: bindA, RouterAddr: routerA}}}

	cases := []struct {
		name     string
		file     *config.File
		opts     SpecsOptions
		want     []Config
		wantErr  string
		wantLogs []string
	}{
		{
			name: "single anthropic shape",
			file: doorFile(onePort, client("claude-code", routerA, "anthropic", true)),
			want: []Config{{
				ListenAddr: bindA, Shape: ShapeAnthropic, RouterTarget: routerA,
				FallbackBase: DefaultAnthropicBase, Fallbacks: []FallbackRule{anthropicRule},
				Cooldown: DefaultCooldown, ProbeInterval: DefaultProbeInterval,
			}},
		},
		{
			name: "empty protocol_shape defaults to anthropic",
			file: doorFile(onePort, client("claude-code", routerA, "", true)),
			want: []Config{{
				ListenAddr: bindA, Shape: ShapeAnthropic, RouterTarget: routerA,
				FallbackBase: DefaultAnthropicBase, Fallbacks: []FallbackRule{anthropicRule},
				Cooldown: DefaultCooldown, ProbeInterval: DefaultProbeInterval,
			}},
		},
		{
			name: "single openai shape",
			file: doorFile(onePort, client("codex", routerA, "openai", true)),
			want: []Config{{
				ListenAddr: bindA, Shape: ShapeOpenAI, RouterTarget: routerA,
				FallbackBase: DefaultOpenAIBase, Fallbacks: openaiRules,
				Cooldown: DefaultCooldown, ProbeInterval: DefaultProbeInterval,
			}},
		},
		{
			name: "shared addr two shapes",
			file: doorFile(onePort,
				client("claude-code", routerA, "anthropic", true),
				client("codex", routerA, "openai", true)),
			want: []Config{{
				ListenAddr: bindA, Shape: ShapeAnthropic + "+" + ShapeOpenAI, RouterTarget: routerA,
				FallbackBase: DefaultAnthropicBase,
				Fallbacks:    append([]FallbackRule{anthropicRule}, openaiRules...),
				Cooldown:     DefaultCooldown, ProbeInterval: DefaultProbeInterval,
			}},
		},
		{
			name: "base overrides",
			file: doorFile(onePort,
				client("claude-code", routerA, "anthropic", true),
				client("codex", routerA, "openai", true)),
			opts: SpecsOptions{AnthropicBase: "http://a.test", OpenAIBase: "http://o.test"},
			want: []Config{{
				ListenAddr: bindA, Shape: ShapeAnthropic + "+" + ShapeOpenAI, RouterTarget: routerA,
				FallbackBase: "http://a.test",
				Fallbacks: []FallbackRule{
					{Prefix: "/v1/messages", Shape: ShapeAnthropic, Base: "http://a.test"},
					{Prefix: "/v1/chat/completions", Shape: ShapeOpenAI, Base: "http://o.test"},
					{Prefix: "/v1/responses", Shape: ShapeOpenAI, Base: "http://o.test"},
				},
				Cooldown: DefaultCooldown, ProbeInterval: DefaultProbeInterval,
			}},
		},
		{
			name: "durations parsed",
			file: doorFile(
				&config.Door{Cooldown: "20s", ProbeInterval: "5s",
					Ports: []config.DoorPort{{BindAddr: bindA, RouterAddr: routerA}}},
				client("claude-code", routerA, "anthropic", true)),
			want: []Config{{
				ListenAddr: bindA, Shape: ShapeAnthropic, RouterTarget: routerA,
				FallbackBase: DefaultAnthropicBase, Fallbacks: []FallbackRule{anthropicRule},
				Cooldown: 20 * time.Second, ProbeInterval: 5 * time.Second,
			}},
		},
		{
			name: "bad cooldown",
			file: doorFile(
				&config.Door{Cooldown: "soon",
					Ports: []config.DoorPort{{BindAddr: bindA, RouterAddr: routerA}}},
				client("claude-code", routerA, "anthropic", true)),
			wantErr: "door.cooldown",
		},
		{
			name: "unmatched router_addr skipped with log",
			file: doorFile(
				&config.Door{Ports: []config.DoorPort{
					{BindAddr: bindA, RouterAddr: routerA},
					{BindAddr: "127.0.0.1:8082", RouterAddr: "127.0.0.1:18082"},
				}},
				client("claude-code", routerA, "anthropic", true)),
			want: []Config{{
				ListenAddr: bindA, Shape: ShapeAnthropic, RouterTarget: routerA,
				FallbackBase: DefaultAnthropicBase, Fallbacks: []FallbackRule{anthropicRule},
				Cooldown: DefaultCooldown, ProbeInterval: DefaultProbeInterval,
			}},
			wantLogs: []string{"router_addr 127.0.0.1:18082 matches no enabled client"},
		},
		{
			name:    "disabled client does not count",
			file:    doorFile(onePort, client("claude-code", routerA, "anthropic", false)),
			wantErr: "no entry matches an enabled client",
			wantLogs: []string{
				"router_addr 127.0.0.1:18081 matches no enabled client",
			},
		},
		{
			name:    "no door section",
			file:    &config.File{Clients: []config.Client{client("claude-code", routerA, "anthropic", true)}},
			wantErr: "no door.ports",
		},
		{
			name:    "empty ports list",
			file:    doorFile(&config.Door{}, client("claude-code", routerA, "anthropic", true)),
			wantErr: "no door.ports",
		},
		{
			name: "duplicate bind_addr",
			file: doorFile(
				&config.Door{Ports: []config.DoorPort{
					{BindAddr: bindA, RouterAddr: routerA},
					{BindAddr: bindA, RouterAddr: routerA},
				}},
				client("claude-code", routerA, "anthropic", true)),
			wantErr: "duplicate bind_addr",
		},
		{
			name: "malformed router_addr",
			file: doorFile(
				&config.Door{Ports: []config.DoorPort{{BindAddr: bindA, RouterAddr: "not-an-addr"}}},
				client("claude-code", routerA, "anthropic", true)),
			wantErr: "bad router_addr",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			opts := tc.opts
			opts.Logf = func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			}
			got, err := SpecsFromConfig(tc.file, opts)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("SpecsFromConfig: %v", err)
			} else {
				if len(got) != len(tc.want) {
					t.Fatalf("got %d specs, want %d: %+v", len(got), len(tc.want), got)
				}
				for i := range got {
					if !specEqual(got[i], tc.want[i]) {
						t.Errorf("spec %d = %+v, want %+v", i, got[i], tc.want[i])
					}
				}
			}
			for _, want := range tc.wantLogs {
				found := false
				for _, l := range logs {
					if strings.Contains(l, want) {
						found = true
					}
				}
				if !found {
					t.Errorf("logs %q missing %q", logs, want)
				}
			}
			if len(tc.wantLogs) == 0 && len(logs) != 0 {
				t.Errorf("unexpected logs: %q", logs)
			}
		})
	}
}

func TestDiffSpecs(t *testing.T) {
	a := Config{ListenAddr: "127.0.0.1:8081", Shape: ShapeAnthropic, RouterTarget: "127.0.0.1:18081",
		FallbackBase: DefaultAnthropicBase, Cooldown: DefaultCooldown, ProbeInterval: DefaultProbeInterval}
	b := Config{ListenAddr: "127.0.0.1:8082", Shape: ShapeOpenAI, RouterTarget: "127.0.0.1:18082",
		FallbackBase: DefaultOpenAIBase, Cooldown: DefaultCooldown, ProbeInterval: DefaultProbeInterval}
	bChanged := b
	bChanged.Cooldown = 42 * time.Second
	c := Config{ListenAddr: "127.0.0.1:8083", Shape: ShapeOpenAI, RouterTarget: "127.0.0.1:18083",
		FallbackBase: DefaultOpenAIBase, Cooldown: DefaultCooldown, ProbeInterval: DefaultProbeInterval}
	aRules := a
	aRules.Fallbacks = []FallbackRule{{Prefix: "/v1/messages", Shape: ShapeAnthropic, Base: DefaultAnthropicBase}}

	addrs := func(specs []Config) string {
		out := make([]string, 0, len(specs))
		for _, s := range specs {
			out = append(out, s.ListenAddr)
		}
		return strings.Join(out, ",")
	}
	cases := []struct {
		name                          string
		current, next                 []Config
		wantAdd, wantRemove, wantKeep string
	}{
		{name: "identical", current: []Config{a, b}, next: []Config{a, b},
			wantKeep: "127.0.0.1:8081,127.0.0.1:8082"},
		{name: "add and remove", current: []Config{a, b}, next: []Config{b, c},
			wantAdd: "127.0.0.1:8083", wantRemove: "127.0.0.1:8081", wantKeep: "127.0.0.1:8082"},
		{name: "changed spec rebinds", current: []Config{a, b}, next: []Config{a, bChanged},
			wantAdd: "127.0.0.1:8082", wantRemove: "127.0.0.1:8082", wantKeep: "127.0.0.1:8081"},
		{name: "fallback rules change rebinds", current: []Config{a}, next: []Config{aRules},
			wantAdd: "127.0.0.1:8081", wantRemove: "127.0.0.1:8081"},
		{name: "from empty", current: nil, next: []Config{a},
			wantAdd: "127.0.0.1:8081"},
		{name: "to empty", current: []Config{a}, next: nil,
			wantRemove: "127.0.0.1:8081"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			added, removed, unchanged := DiffSpecs(tc.current, tc.next)
			if got := addrs(added); got != tc.wantAdd {
				t.Errorf("added = %q, want %q", got, tc.wantAdd)
			}
			if got := addrs(removed); got != tc.wantRemove {
				t.Errorf("removed = %q, want %q", got, tc.wantRemove)
			}
			if got := addrs(unchanged); got != tc.wantKeep {
				t.Errorf("unchanged = %q, want %q", got, tc.wantKeep)
			}
		})
	}
}
