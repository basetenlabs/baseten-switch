package gateway

import (
	"net/http"
	"testing"

	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

func TestApplyBasetenUsageHeaders(t *testing.T) {
	headers := http.Header{
		"User-Agent":               {"claude-code/2.1"},
		"X-Baseten-Switch-Version": {"forged"},
	}

	applyBasetenUsageHeaders(headers, "baseten")

	if got, want := headers.Get("User-Agent"), "baseten-switch/"+version.Version+" claude-code/2.1"; got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
	if got := headers.Get(switchVersionHeader); got != version.Version {
		t.Fatalf("%s = %q, want %q", switchVersionHeader, got, version.Version)
	}
}

func TestApplyBasetenUsageHeadersWithoutInboundUserAgent(t *testing.T) {
	headers := http.Header{}

	applyBasetenUsageHeaders(headers, "baseten")

	if got, want := headers.Get("User-Agent"), "baseten-switch/"+version.Version; got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
}

func TestApplyBasetenUsageHeadersLeavesNativeRoutesUnchanged(t *testing.T) {
	for _, route := range []string{"anthropic", "openai"} {
		t.Run(route, func(t *testing.T) {
			headers := http.Header{
				"User-Agent":               {"harness/1.0"},
				"X-Baseten-Switch-Version": {"inbound"},
			}

			applyBasetenUsageHeaders(headers, route)

			if got := headers.Get("User-Agent"); got != "harness/1.0" {
				t.Fatalf("User-Agent = %q, want unchanged", got)
			}
			if got := headers.Get(switchVersionHeader); got != "inbound" {
				t.Fatalf("%s = %q, want unchanged", switchVersionHeader, got)
			}
		})
	}
}
