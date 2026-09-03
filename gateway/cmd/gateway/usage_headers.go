package gateway

import (
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/internal/version"
)

const switchVersionHeader = "X-Baseten-Switch-Version"

func applyBasetenUsageHeaders(headers http.Header, route string) {
	if route != "baseten" {
		return
	}
	marker := "baseten-switch/" + version.Version
	if prior := strings.TrimSpace(headers.Get("User-Agent")); prior != "" {
		marker += " " + prior
	}
	headers.Set("User-Agent", marker)
	headers.Set(switchVersionHeader, version.Version)
}
