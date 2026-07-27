package door

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// PortSpec is one parsed --port flag value.
type PortSpec struct {
	Port         int
	Shape        Shape
	RouterTarget string // host:port
}

// ParsePortSpec parses a --port value of the form
// "45271=anthropic:127.0.0.1:45272" (PORT=shape:routerHost:routerPort).
func ParsePortSpec(s string) (PortSpec, error) {
	eq := strings.Index(s, "=")
	if eq < 0 {
		return PortSpec{}, fmt.Errorf("port spec %q: missing '=' (want PORT=shape:host:port)", s)
	}
	port, err := strconv.Atoi(s[:eq])
	if err != nil || port < 1 || port > 65535 {
		return PortSpec{}, fmt.Errorf("port spec %q: bad listen port %q", s, s[:eq])
	}
	rest := s[eq+1:]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return PortSpec{}, fmt.Errorf("port spec %q: missing router target (want PORT=shape:host:port)", s)
	}
	shape := Shape(rest[:colon])
	if shape != ShapeAnthropic && shape != ShapeOpenAI {
		return PortSpec{}, fmt.Errorf("port spec %q: unknown shape %q (want anthropic or openai)", s, rest[:colon])
	}
	target := rest[colon+1:]
	host, tp, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return PortSpec{}, fmt.Errorf("port spec %q: malformed router target %q (want host:port)", s, target)
	}
	if n, err := strconv.Atoi(tp); err != nil || n < 1 || n > 65535 {
		return PortSpec{}, fmt.Errorf("port spec %q: bad router port %q", s, tp)
	}
	return PortSpec{Port: port, Shape: shape, RouterTarget: target}, nil
}

// ValidatePortSpecs enforces at least one spec and no duplicate listen
// ports.
func ValidatePortSpecs(specs []PortSpec) error {
	if len(specs) == 0 {
		return fmt.Errorf("at least one --port is required")
	}
	seen := map[int]bool{}
	for _, sp := range specs {
		if seen[sp.Port] {
			return fmt.Errorf("duplicate listen port %d", sp.Port)
		}
		seen[sp.Port] = true
	}
	return nil
}
