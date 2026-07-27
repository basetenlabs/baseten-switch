package door

import (
	"strings"
	"testing"
)

// (9) Flag parsing table tests.
func TestParsePortSpec(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    PortSpec
		wantErr string
	}{
		{
			name: "anthropic",
			in:   "8081=anthropic:127.0.0.1:18081",
			want: PortSpec{Port: 8081, Shape: ShapeAnthropic, RouterTarget: "127.0.0.1:18081"},
		},
		{
			name: "openai",
			in:   "8082=openai:127.0.0.1:18082",
			want: PortSpec{Port: 8082, Shape: ShapeOpenAI, RouterTarget: "127.0.0.1:18082"},
		},
		{
			name: "hostname target",
			in:   "8083=openai:gateway:18083",
			want: PortSpec{Port: 8083, Shape: ShapeOpenAI, RouterTarget: "gateway:18083"},
		},
		{name: "missing equals", in: "8081:anthropic:127.0.0.1:18081", wantErr: "missing '='"},
		{name: "bad listen port", in: "abc=anthropic:127.0.0.1:18081", wantErr: "bad listen port"},
		{name: "listen port zero", in: "0=anthropic:127.0.0.1:18081", wantErr: "bad listen port"},
		{name: "listen port too big", in: "70000=anthropic:127.0.0.1:18081", wantErr: "bad listen port"},
		{name: "bad shape", in: "8081=monitor:127.0.0.1:18081", wantErr: "unknown shape"},
		{name: "empty shape", in: "8081=:127.0.0.1:18081", wantErr: "unknown shape"},
		{name: "missing target", in: "8081=anthropic", wantErr: "missing router target"},
		{name: "target missing port", in: "8081=anthropic:127.0.0.1", wantErr: "malformed router target"},
		{name: "target empty host", in: "8081=anthropic::18081", wantErr: "malformed router target"},
		{name: "target bad port", in: "8081=anthropic:127.0.0.1:xyz", wantErr: "bad router port"},
		{name: "empty", in: "", wantErr: "missing '='"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePortSpec(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParsePortSpec(%q) = %+v, want error containing %q", tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParsePortSpec(%q) error = %q, want it to contain %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePortSpec(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParsePortSpec(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidatePortSpecs(t *testing.T) {
	ok := []PortSpec{
		{Port: 8081, Shape: ShapeAnthropic, RouterTarget: "127.0.0.1:18081"},
		{Port: 8082, Shape: ShapeOpenAI, RouterTarget: "127.0.0.1:18082"},
	}
	if err := ValidatePortSpecs(ok); err != nil {
		t.Fatalf("valid specs rejected: %v", err)
	}
	if err := ValidatePortSpecs(nil); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("empty specs: err = %v, want 'at least one --port'", err)
	}
	dup := append(ok, PortSpec{Port: 8081, Shape: ShapeOpenAI, RouterTarget: "127.0.0.1:18083"})
	if err := ValidatePortSpecs(dup); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate port: err = %v, want 'duplicate listen port'", err)
	}
}
