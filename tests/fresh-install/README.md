# Fresh-install container simulation

Exercises the source-build lifecycle on a simulated brand-new Linux machine
and verifies the result. One command from the repository:

```sh
tests/fresh-install/run.sh            # keyed: real routed request
tests/fresh-install/run.sh --no-key   # plumbing-only stub checks
```

## What it simulates

- A clean Debian machine: non-root user, curl, CA certificates,
  nothing preinstalled. `run.sh` cross-compiles the single `baseten-switch`
  binary from the current tree and stages it off PATH. `install-inside.sh`
  then puts the binary in `~/.local/bin`, writes
  `gateway.yaml` with the single-port door topology (door 45271, router
  45272, route baseten), env file written 0600 with `BASETEN_API_KEY` and
  `BASETEN_SWITCH_API_KEY_FALLBACK=1`, then `baseten-switch up` to start router + door
  (the end-to-end proof of the lifecycle contract).
- Verification, all inside the container: `baseten-switch status` exit 0
  with both components up, startup preflight output in the router log
  (`~/.config/baseten-switch/logs/router.log`), a POST through the DOOR
  port to `/v1/messages` with a `claude-*` model returning 200 with
  real content, and a telemetry row with `route=baseten` and status
  200.

## What it does not simulate

- macOS keychain OAuth (`baseten auth login`); the container exercises
  the API-key fallback path only.
- The menubar app, Gatekeeper/notarization, and launchd. Validate those on
  clean macOS accounts as part of the signed release rehearsal.
- Harness binaries (Claude Code, Codex). The routed request uses the
  Claude Code protocol shape; validate real harness binaries separately.

## Key handling

The Baseten API key is read from the host environment (`BASETEN_API_KEY`),
falling back to parsing `~/.config/baseten-switch/env`.
It enters the container only as a `docker run` environment variable; it
is never printed, never written to any image layer, and the in-container
env file is created mode 0600. With no key available (or `--no-key`),
the routed-request step is replaced by deterministic plumbing checks: the
keyless `503 needs-login` rejection on the router port, followed by the door's
native fallback. The synthetic native credential must return a 4xx and the
door must stamp the response as fallback-served. Credential rejection happens
before an upstream attempt exists to record, and door-native fallbacks bypass
the router, so the keyed lane remains the telemetry proof.

## Notes

- No container ports are published to the host; the simulation cannot
  collide with the live gateway/door running on this machine.
- Containers are removed after each run (`--rm`); the image
  `baseten-switch-fresh-install` stays cached for fast reruns.
- The image platform follows the host arch (linux/arm64 on Apple
  Silicon) to avoid emulation.
