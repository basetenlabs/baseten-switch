package config

import (
	"strings"
	"testing"
)

func TestNativePolicyDefaultsAndStrictParsing(t *testing.T) {
	const prefix = "global:\n  routing_enabled: false\nclients:\n  - name: gateway-client\n    protocol_shape: anthropic\n"
	for _, test := range []struct {
		policy string
		allow  bool
	}{
		{"", true}, {"    allow_native: true\n", true}, {"    allow_native: false\n", false},
	} {
		var file File
		if err := UnmarshalStrict([]byte(prefix+test.policy), &file); err != nil {
			t.Fatal(err)
		}
		if got := IsNativeAllowed(file.Clients[0]); got != test.allow {
			t.Fatalf("policy %q: allowed=%t, want %t", test.policy, got, test.allow)
		}
	}
	for _, value := range []string{"null", "maybe", "[]"} {
		var file File
		if err := UnmarshalStrict([]byte(prefix+"    allow_native: "+value+"\n"), &file); err == nil {
			t.Fatalf("allow_native %q parsed successfully", value)
		}
	}
}

func TestNoNativePolicyRejectsIncompatibleConfiguration(t *testing.T) {
	noNative, routing := false, false
	base := func() *File {
		return &File{Global: Global{RoutingEnabled: &routing}, Clients: []Client{{
			Name: "gateway-client", Enabled: true, ProtocolShape: "anthropic", BindAddr: "127.0.0.1:12345",
			AllowNative: &noNative, ModelAliases: map[string]string{"claude-baseten-alpha": "example/Alpha"},
		}}}
	}
	if err := ValidateRoutingPolicy(base()); err != nil {
		t.Fatalf("empty selected list without default: %v", err)
	}
	for _, test := range []struct {
		name string
		edit func(*File)
		want string
	}{
		{"shape", func(f *File) { f.Clients[0].ProtocolShape = "openai" }, "requires protocol_shape anthropic"},
		{"fallback", func(f *File) { f.Clients[0].FallbackRoute = "anthropic" }, "forbids fallback_route"},
		{"native fallback model", func(f *File) { f.Clients[0].NativeFallbackModel = "claude-opus-5" }, "forbids fallback_route"},
		{"subagent model", func(f *File) { f.Clients[0].SubagentModel = "example/Worker" }, "forbids subagent routing"},
		{"subagent routing", func(f *File) { f.Clients[0].SubagentRouting = "off" }, "forbids subagent routing"},
		{"family pin", func(f *File) { f.Clients[0].ModelRoutes = map[string]string{"opus": "native"} }, "forbids subagent routing"},
		{"invalid upstream shape", func(f *File) { f.Clients[0].UpstreamShape = "invalid" }, "requires upstream_shape"},
		{"native alias target", func(f *File) { f.Clients[0].ModelAliases["claude-baseten-alpha"] = "claude-opus-5" }, "requires a Baseten slug"},
		{"padded alias target", func(f *File) { f.Clients[0].ModelAliases["claude-baseten-alpha"] = " example/Alpha" }, "requires a Baseten slug"},
		{"shared listener", func(f *File) {
			f.Clients = append(f.Clients, Client{Name: "other-client", Enabled: true, ProtocolShape: "openai", BindAddr: "localhost:12345", DefaultModel: "example/Other"})
		}, "requires a dedicated bind_addr"},
		{"door fallback", func(f *File) {
			f.Door = &Door{Ports: []DoorPort{{BindAddr: "127.0.0.1:12346", RouterAddr: "localhost:12345"}}}
		}, "cannot use a door port"},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := base()
			test.edit(file)
			if err := ValidateRoutingPolicy(file); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateRoutingPolicy=%v, want %q", err, test.want)
			}
		})
	}
	legacy := base()
	legacy.Clients[0].AllowNative = nil
	if err := ValidateRoutingPolicy(legacy); err == nil || !strings.Contains(err.Error(), "requires a Baseten default_model") {
		t.Fatalf("legacy default requirement changed: %v", err)
	}
}

func TestNoNativePolicyRequiresLoopbackListener(t *testing.T) {
	noNative, routing := false, true
	for _, test := range []struct {
		address string
		valid   bool
	}{
		{"127.0.0.1:45274", true},
		{"[::1]:45274", true},
		{"0.0.0.0:45274", false},
		{"[::]:45274", false},
		{"203.0.113.10:45274", false},
		{"localhost:45274", false},
		{"example.invalid:45274", false},
		{"127.0.0.1:0", false},
		{"127.0.0.1:70000", false},
	} {
		t.Run(test.address, func(t *testing.T) {
			file := &File{Global: Global{RoutingEnabled: &routing}, Clients: []Client{{
				Name: "gateway-client", Enabled: true, ProtocolShape: "anthropic",
				AllowNative: &noNative, BindAddr: test.address,
			}}}
			err := ValidateRoutingPolicy(file)
			if (err == nil) != test.valid {
				t.Fatalf("ValidateRoutingPolicy(%q) = %v, want valid %t", test.address, err, test.valid)
			}
		})
	}
}
