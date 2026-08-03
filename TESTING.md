# Testing

How Baseten Switch is tested and how to run each layer.

## Strategy

Testing runs in three layers, ordered from cheapest to most realistic:

1. **Unit and integration tests** (Go and Swift): hermetic, run on every change.
2. **The `check.sh` gate**: build, vet, full test suite, plus live smoke tests
   on scratch ports. This is the required gate for every change set.
3. **Fresh-install simulation** (`tests/fresh-install/`): a clean Debian
   container exercises the source-build lifecycle and verifies the full
   install-to-first-request path.

An additional opt-in live contract test validates API-key profile
authentication against Baseten. The normal gate skips this networked lane.

## Layer 1: unit and integration tests

### Go

```sh
cd gateway && go test ./...
```

| Package | Covers |
|---|---|
| `cmd/baseten-switch` | CLI surface, clean config reset, global mutation CAS/journal/reconciliation, Claude settings adapter, and diagnostics |
| `cmd/gateway` | Routing engine, global gate and resolver status, fallback waterfall, upstream discovery, subagent gate, TTFT timing, reload behavior, and admin security |
| `internal/*` | Analytics aggregation and indexing, config editing, OAuth and credential isolation, door circuit breaker, pidfiles, sanitization, telemetry, protocol translation, and usage accounting |

The only production package with no tests is `internal/version`.

### Swift (menubar app)

Tests under `mac/BasetenSwitch/Tests/BasetenSwitchTests/` cover
`DisplayTests`, `DoorStatusTests`, `LoginItemTests`, `PopupDisplayTests`,
`RuntimeCoordinationTests`, `StatsTests`, and `TrafficTests`. Run with:

```sh
cd mac/BasetenSwitch && swift test
```

These cover display formatting, stats parsing, login-item policy,
isolated runtime environments, runtime-trust
interlocks, binary lookup, icon sizing, window-presentation state, and
routing mutation tokens, receipts, timeouts, and reconciliation.
Traffic tests also cover analytics decoding, range requests, stale replay,
request coalescing, and navigation. Nothing drives live AppKit behavior
(status item, native menu, window controls, or route-flip shell-outs).

Release validation also covers the packaged app, accessibility, and
performance.

## Layer 2: the `check.sh` gate

```sh
scripts/check.sh            # full gate
scripts/check.sh --offline  # skips only the network smoke (step 5)
```

Required before calling any change set done, including work delegated to
agents. Steps, in order:

1. `gofmt` on changed files (ratchet: ignores pre-existing drift).
2. `go build ./...`, `go vet ./...`, `go test ./...`.
3. Build the `baseten-switch` binary.
4. Verify the scratch ports are free. Defaults are 28081-28083, 28182-28183,
   and 28786-28787; `BASETEN_SWITCH_CHECK_*_PORT` variables override them for parallel
   worktrees.
5. Smoke: `whoami --refresh` against the configured credential store, forcing
   a live `/v1/users/me` call. Skipped by `--offline` or when no local auth
   exists.
6. Smoke: throwaway gateway boot on scratch ports; asserts `/healthz` and two
   expected preflight warnings.
7. Smoke: throwaway door boot against a dead router; asserts `/doorz` reports
   tripped.
8. Smoke: `up`, `status`, idempotent `up`, `down`, `status` round trip on
   scratch ports with `BASETEN_SWITCH_LAUNCHD=off` and a fully sandboxed env. Never
   touches the live gateway.
9. Validate the API-key profile live-test script in dry-run mode. The live
   request runs only when explicitly enabled with a test key.

All smoke steps isolate themselves with the env seams listed below; the gate
never edits `~/.config/baseten-switch` or touches a running production gateway.

## Layer 3: fresh-install simulation (`tests/fresh-install/`)

A clean Debian container installs the binary, writes config and a 0600 env
file, runs `baseten-switch up`, then verifies that a real request through the
front door returns 200 from Baseten and that telemetry records the request.

```sh
cd tests/fresh-install
BASETEN_API_KEY=... ./run.sh    # keyed mode: real routed request
./run.sh --no-key               # keyless: asserts 503 needs-login, then a
                                # SIGHUP route flip to a local stub
```

The API key enters only via `docker run -e`, never a image layer. Not
simulated: macOS keychain OAuth, the menubar app, Gatekeeper/notarization,
and real launchd installs.

## Opt-in API-key profile contract

This test creates a temporary Baseten CLI API-key profile, starts scratch
router and door processes, and sends `GET /v1/models` plus one minimal Chat
Completions request through the door. It verifies that admin status reports a
healthy API-key profile, the environment fallback is disabled, and telemetry
attributes inference to Baseten.

Use an API key that may list models and invoke the selected model. The key is
read only from the environment and is never printed. The model defaults to
the public example model; override it when the key has access to a different
model.

```sh
BASETEN_SWITCH_TEST_API_KEY='<test-key>' \
BASETEN_SWITCH_TEST_MODEL='zai-org/GLM-5.2' \
scripts/tests/test_api_key_profile_live.sh
```

To include the lane in the full gate:

```sh
BASETEN_SWITCH_RUN_API_KEY_PROFILE_LIVE=1 \
BASETEN_SWITCH_TEST_API_KEY='<test-key>' \
BASETEN_SWITCH_TEST_MODEL='zai-org/GLM-5.2' \
scripts/check.sh
```

The script requires `baseten`, `curl`, `go`, and `lsof`. It uses a temporary
`BASETEN_CONFIG_DIR`, insecure storage inside that temporary directory, an
empty Switch environment file, scratch ports, and scratch pidfiles. It does
not read the normal CLI profile store or operate on installed Switch
processes. On exit it stops only the process IDs it started and removes only
its validated temporary directory.

Before cleanup, the script scans its output, logs, telemetry, and scratch
state for the exact test key. It excludes the temporary CLI credential file,
which intentionally contains the key.

## Writing tests: isolation seams

Production code honors env overrides so tests and scratch instances never
touch real state. Any test that boots a component must set the relevant ones:

| Seam | Purpose |
|---|---|
| `BASETEN_SWITCH_CONFIG_PATH` | config file location |
| `BASETEN_SWITCH_ADMIN_ADDR` | admin API address |
| `BASETEN_SWITCH_CLAUDE_SETTINGS`, `BASETEN_SWITCH_BACKUP_DIR` | Claude settings.json adapter targets |
| `BASETEN_SWITCH_LAUNCHD=off` | disable launchd supervision entirely |
| `BASETEN_SWITCH_GATEWAY_PIDFILE`, `BASETEN_SWITCH_DOOR_PIDFILE` | pidfile locations |
| `BASETEN_SWITCH_GATEWAY_LOG`, `BASETEN_SWITCH_DOOR_LOG` | process log locations |
| `BASETEN_SWITCH_TELEMETRY_DIR` | segmented telemetry store location |
| `BASETEN_SWITCH_OAUTH_PROFILE` | named Baseten CLI profile; unset follows the CLI's current profile (point at a nonexistent name to run credential-less) |
| `BASETEN_CONFIG_DIR` | Baseten CLI profile-store directory; live credential tests must use a scratch directory. |
| `BASETEN_SWITCH_AUTH_NO_KEYRING=1` | prevent a scratch test from consulting the system keyring. |
| `BASETEN_SWITCH_MENUBAR_APP` | menubar bundle path override |

Hard rules for contributors and agents:

- Never point a test at the real `~/.claude/settings.json` or
  `~/.config/baseten-switch`; always go through the seams above.
- Never restart or kill a running production gateway from a test.
- Gate every change set with `scripts/check.sh`. `go test ./...` alone does
  not exercise the live refresh path.
- New scripts and flows must be executed at least once (or have a `--dry-run`
  wired into `check.sh`) before being handed to a user.
