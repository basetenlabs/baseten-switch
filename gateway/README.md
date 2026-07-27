# Baseten Switch gateway

This Go module builds the `baseten-switch` CLI, local router, admin API, and
front door. See the repository [README.md](../README.md) for installation and
[TESTING.md](../TESTING.md) for validation.

## Build

From the repository root:

```sh
scripts/build.sh
```

Or build the CLI directly:

```sh
cd gateway
go build -o bin/baseten-switch ./cmd/baseten-switch
```

## Start a development instance

```sh
bin/baseten-switch config init
bin/baseten-switch up
bin/baseten-switch status
```

Use the adapters instead of editing harness configuration by hand:

```sh
bin/baseten-switch claude on
bin/baseten-switch codex on
```

The generated configuration lives at
`~/.config/baseten-switch/gateway.yaml`. Runtime state, logs, and telemetry
use the same configuration directory.

## Main commands

```text
up, down, restart, status
on, off
config init, config reset
claude on|off|status|subagents|route|reasoning
codex on|off|status|route|reasoning
whoami, auth login, doctor, spend
gateway start|stop|restart|status
door
```

Run `baseten-switch <command> --help` for the current flags and environment
overrides.

## Module layout

```text
cmd/baseten-switch/   CLI and lifecycle commands
cmd/gateway/          router, admin API, and request handling
internal/auth/        Baseten CLI credential reader and OAuth transport
internal/config/      configuration parsing and editing
internal/door/        front-door configuration
internal/proxy/       upstream request and response relay
internal/telemetry/   segmented request telemetry
internal/translate/   protocol translation
```

## Development checks

From the repository root, run the full gate:

```sh
scripts/check.sh
```

For a quick package-only pass:

```sh
cd gateway
go test ./...
go vet ./...
```

The full gate remains required because it also builds the Swift app and runs
isolated lifecycle and credential-store smoke tests.
