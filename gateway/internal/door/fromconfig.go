package door

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

// SpecsOptions adjusts SpecsFromConfig. The zero value uses the
// hardcoded native bases and stderr logging.
type SpecsOptions struct {
	AnthropicBase string // fallback base for anthropic-shape clients
	OpenAIBase    string // fallback base for openai-shape clients
	Logf          func(format string, args ...any)
}

// SpecsFromConfig derives one door Config per door.ports entry in f.
// A port's shape set and fallback rules come
// from the enabled clients whose bind_addr equals the entry's
// router_addr; an empty protocol_shape means anthropic, mirroring the
// gateway. An entry matching no enabled client is skipped with a log
// line: the door must not front a port nothing serves. Errors when the
// config has no door.ports or when every entry was skipped.
func SpecsFromConfig(f *config.File, opts SpecsOptions) ([]Config, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}
	aBase := opts.AnthropicBase
	if aBase == "" {
		aBase = DefaultFallbackBase(ShapeAnthropic)
	}
	oBase := opts.OpenAIBase
	if oBase == "" {
		oBase = DefaultFallbackBase(ShapeOpenAI)
	}
	if f == nil || f.Door == nil || len(f.Door.Ports) == 0 {
		return nil, fmt.Errorf("config has no door.ports")
	}
	cooldown, err := durationOr(f.Door.Cooldown, DefaultCooldown)
	if err != nil {
		return nil, fmt.Errorf("door.cooldown: %v", err)
	}
	probe, err := durationOr(f.Door.ProbeInterval, DefaultProbeInterval)
	if err != nil {
		return nil, fmt.Errorf("door.probe_interval: %v", err)
	}

	specs := make([]Config, 0, len(f.Door.Ports))
	seen := map[string]bool{}
	for _, p := range f.Door.Ports {
		if _, _, err := net.SplitHostPort(p.BindAddr); err != nil {
			return nil, fmt.Errorf("door.ports: bad bind_addr %q: %v", p.BindAddr, err)
		}
		if seen[p.BindAddr] {
			return nil, fmt.Errorf("door.ports: duplicate bind_addr %q", p.BindAddr)
		}
		if _, _, err := net.SplitHostPort(p.RouterAddr); err != nil {
			return nil, fmt.Errorf("door.ports: bad router_addr %q: %v", p.RouterAddr, err)
		}
		var hasA, hasO bool
		for _, c := range f.Clients {
			if !c.Enabled || c.BindAddr != p.RouterAddr {
				continue
			}
			switch Shape(c.ProtocolShape) {
			case ShapeAnthropic, "":
				hasA = true
			case ShapeOpenAI:
				hasO = true
			default:
				logf("[baseten-switch door] config: client %s: unknown protocol_shape %q; ignoring", c.Name, c.ProtocolShape)
			}
		}
		if !hasA && !hasO {
			logf("[baseten-switch door] config: door port %s: router_addr %s matches no enabled client; skipping", p.BindAddr, p.RouterAddr)
			continue
		}
		seen[p.BindAddr] = true
		var rules []FallbackRule
		if hasA {
			rules = append(rules, FallbackRule{Prefix: "/v1/messages", Shape: ShapeAnthropic, Base: aBase})
		}
		if hasO {
			rules = append(rules,
				FallbackRule{Prefix: "/v1/chat/completions", Shape: ShapeOpenAI, Base: oBase},
				FallbackRule{Prefix: "/v1/responses", Shape: ShapeOpenAI, Base: oBase})
		}
		fb := oBase
		if hasA {
			fb = aBase
		}
		specs = append(specs, Config{
			ListenAddr:    p.BindAddr,
			Shape:         shapeLabel(rules),
			RouterTarget:  p.RouterAddr,
			FallbackBase:  fb,
			Fallbacks:     rules,
			Cooldown:      cooldown,
			ProbeInterval: probe,
		})
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("door.ports: no entry matches an enabled client")
	}
	return specs, nil
}

func durationOr(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %q", s)
	}
	return d, nil
}

// DiffSpecs partitions next against current by listen address for the
// SIGHUP reload: a port present in both but with a different spec is
// returned as removed+added so the caller rebinds it with the new spec.
func DiffSpecs(current, next []Config) (added, removed, unchanged []Config) {
	cur := map[string]Config{}
	for _, c := range current {
		cur[c.ListenAddr] = c
	}
	nxt := map[string]bool{}
	for _, n := range next {
		nxt[n.ListenAddr] = true
		c, ok := cur[n.ListenAddr]
		switch {
		case !ok:
			added = append(added, n)
		case specEqual(c, n):
			unchanged = append(unchanged, n)
		default:
			removed = append(removed, c)
			added = append(added, n)
		}
	}
	for _, c := range current {
		if !nxt[c.ListenAddr] {
			removed = append(removed, c)
		}
	}
	return added, removed, unchanged
}

// specEqual ignores Logf, which is not part of a port's identity.
func specEqual(a, b Config) bool {
	if a.ListenAddr != b.ListenAddr || a.Shape != b.Shape ||
		a.RouterTarget != b.RouterTarget || a.FallbackBase != b.FallbackBase ||
		a.Cooldown != b.Cooldown || a.ProbeInterval != b.ProbeInterval ||
		a.MaxReplay != b.MaxReplay || len(a.Fallbacks) != len(b.Fallbacks) {
		return false
	}
	for i := range a.Fallbacks {
		if a.Fallbacks[i] != b.Fallbacks[i] {
			return false
		}
	}
	return true
}
