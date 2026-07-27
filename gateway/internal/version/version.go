// Package version carries the build version stamped by
// scripts/build.sh via
//
//	-ldflags "-X github.com/basetenlabs/baseten-switch/gateway/internal/version.Version=<git describe>"
//
// and stays "dev" for a plain `go build`. It is surfaced through
// `baseten-switch --version`, the admin /healthz and /v1/admin/status
// payloads, and the door's /doorz payload, so `baseten-switch status` can
// flag skew between the running processes and the binary on disk
// ("restart to adopt", the lifecycle contract launchd supervision).
package version

// Version is the build identifier of this binary.
var Version = "dev"
