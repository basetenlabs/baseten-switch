#!/usr/bin/env bash
# check.sh: the local build + test + smoke gate for baseten-switch.
#
# Run this after every change set (human or agent) and before calling any
# work done. `go test ./...` alone is not sufficient. The smoke stage exercises
# the real credential contract and boots throwaway gateway and door instances
# on scratch ports. It never touches a running gateway or door.
#
# Usage: scripts/check.sh [--offline]
#   --offline  skip steps that need the network or the real credential
#              store (whoami); build, vet, tests, and scratch boots still run.
#
# Scratch ports default to: 28081, 28082, 28083, 28182, 28183, 28786,
# 28787. Override individual ports with the BASETEN_SWITCH_CHECK_*_PORT variables
# when another worktree is using a default.

set -euo pipefail

OFFLINE=0
[[ "${1:-}" == "--offline" ]] && OFFLINE=1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
GW_DIR="$REPO_DIR/gateway"
GO="${GO:-go}"

ADMIN_PORT="${BASETEN_SWITCH_CHECK_ADMIN_PORT:-28787}"
CLIENT_PORT="${BASETEN_SWITCH_CHECK_CLIENT_PORT:-28081}"
DOOR_PORT="${BASETEN_SWITCH_CHECK_DOOR_PORT:-28082}"
DEAD_ROUTER_PORT="${BASETEN_SWITCH_CHECK_DEAD_ROUTER_PORT:-28182}"
# up/down/status round trip scratch ports:
UPDOWN_ADMIN_PORT="${BASETEN_SWITCH_CHECK_UPDOWN_ADMIN_PORT:-28786}"
UPDOWN_CLIENT_PORT="${BASETEN_SWITCH_CHECK_UPDOWN_CLIENT_PORT:-28183}"
UPDOWN_DOOR_PORT="${BASETEN_SWITCH_CHECK_UPDOWN_DOOR_PORT:-28083}"

# Template as an operand (not -t): the only mktemp form GNU and BSD
# parse the same way, so the gate runs on Linux hosts too.
TMPDIR_CHECK="$(mktemp -d "${TMPDIR:-/tmp}/baseten-switch-check.XXXXXX")"
PIDS=()
cleanup() {
    for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
    rm -rf "$TMPDIR_CHECK"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
forget_pid() {
    local target="$1"
    local pid
    local kept=()
    for pid in "${PIDS[@]:-}"; do
        [[ "$pid" == "$target" ]] || kept+=("$pid")
    done
    PIDS=()
    if [[ "${#kept[@]}" -gt 0 ]]; then
        PIDS=("${kept[@]}")
    fi
}

# ---------------------------------------------------------------- gofmt
# Only files changed relative to HEAD (plus untracked): the tree has
# pre-existing gofmt drift; this ratchets without blocking on history.
step "gofmt (changed files)"
cd "$REPO_DIR"
# Fail loudly when the repository root is wrong so formatting cannot become a
# silent no-op.
git rev-parse --show-toplevel >/dev/null 2>&1 || fail "not a git repository: $REPO_DIR"
CHANGED_GO="$( (git diff --name-only HEAD; git ls-files --others --exclude-standard) \
    | grep -E '^gateway/.*\.go$' | sort -u || true)"
if [[ -n "$CHANGED_GO" ]]; then
    BAD="$(echo "$CHANGED_GO" | xargs gofmt -l 2>/dev/null || true)"
    [[ -z "$BAD" ]] || fail "gofmt needed: $BAD"
    echo "ok ($(echo "$CHANGED_GO" | wc -l | tr -d ' ') files)"
else
    echo "ok (no changed .go files)"
fi

# ------------------------------------------- shipping port contract
step "shipping port contract"
"$REPO_DIR/scripts/tests/test_port_contract.sh" \
    || fail "shipping port contract"

# ----------------------------------------- internal export contract
# The exporter and its tests are intentionally excluded from the final public
# tree. Run them whenever they are present in the private preparation tree,
# while keeping this same check script usable after export.
step "public export contract"
if [[ -x "$REPO_DIR/scripts/tests/test_export_public.sh" ]]; then
    "$REPO_DIR/scripts/tests/test_export_public.sh" \
        || fail "public export contract"
else
    echo "skipped (export tooling is not part of the public tree)"
fi
if [[ -x "$REPO_DIR/scripts/tests/test_public_manifest.sh" ]]; then
    "$REPO_DIR/scripts/tests/test_public_manifest.sh" \
        || fail "public manifest surface"
fi

# ------------------------------------------------------- secret scan
# The scanner downloads a checksum-pinned release binary. Offline validation
# skips only this network-dependent step.
step "candidate source secret scan"
if [[ "$OFFLINE" == 1 ]]; then
    echo "skipped (--offline)"
elif [[ -x "$REPO_DIR/scripts/security/run-gitleaks.sh" ]]; then
    "$REPO_DIR/scripts/security/run-gitleaks.sh" "$REPO_DIR" \
        || fail "candidate source secret scan"
else
    fail "scripts/security/run-gitleaks.sh is missing or not executable"
fi

# ------------------------------------------------- build + vet + tests
step "go build / vet / test"
cd "$GW_DIR"
"$GO" build ./... || fail "go build"
"$GO" vet ./... || fail "go vet"
"$GO" test ./... > "$TMPDIR_CHECK/gotest.out" 2>&1 \
    || { tail -30 "$TMPDIR_CHECK/gotest.out"; fail "go test"; }
echo "ok ($(grep -cE '^ok' "$TMPDIR_CHECK/gotest.out") packages)"

# A green Go gate must not mask a native app compile or test failure. Keep the
# Swift build hermetic so this check neither depends on nor mutates user-level
# SwiftPM and Clang caches.
if [[ "$(uname -s)" == "Darwin" ]]; then
    step "swift build / test"
    SWIFT_HOME="$TMPDIR_CHECK/swift-home"
    SWIFT_BUILD="$TMPDIR_CHECK/swift-build"
    SWIFT_CLANG_CACHE="$TMPDIR_CHECK/clang-module-cache"
    SWIFT_MODULE_CACHE="$TMPDIR_CHECK/swift-module-cache"
    mkdir -p "$SWIFT_HOME" "$SWIFT_CLANG_CACHE" "$SWIFT_MODULE_CACHE"
    (
        cd "$REPO_DIR/mac/BasetenSwitch"
        # The repository has no package dependencies or build plugins.
        # Disabling SwiftPM's nested sandbox keeps this gate runnable inside
        # CI and developer sandboxes while the explicit cache and scratch
        # paths retain hermetic filesystem behavior.
        env HOME="$SWIFT_HOME" \
            CLANG_MODULE_CACHE_PATH="$SWIFT_CLANG_CACHE" \
            SWIFTPM_MODULECACHE_OVERRIDE="$SWIFT_MODULE_CACHE" \
            swift test --disable-sandbox --scratch-path "$SWIFT_BUILD"
    ) > "$TMPDIR_CHECK/swifttest.out" 2>&1 \
        || { tail -40 "$TMPDIR_CHECK/swifttest.out"; fail "swift test"; }
    SWIFT_TEST_COUNT="$(
        grep -E 'Executed [0-9]+ tests,' "$TMPDIR_CHECK/swifttest.out" \
            | tail -1 \
            | sed -E 's/.*Executed ([0-9]+) tests,.*/\1/'
    )"
    [[ -n "$SWIFT_TEST_COUNT" ]] \
        || { tail -40 "$TMPDIR_CHECK/swifttest.out"; fail "swift test count missing"; }
    echo "ok ($SWIFT_TEST_COUNT tests)"
else
    step "swift build / test"
    echo "skipped (requires macOS)"
fi

step "build binary"
cd "$GW_DIR"
BUILD_VERSION="$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)"
"$GO" build \
    -ldflags "-X github.com/basetenlabs/baseten-switch/gateway/internal/version.Version=${BUILD_VERSION}" \
    -o bin/baseten-switch ./cmd/baseten-switch || fail "build baseten-switch"
[[ "$(bin/baseten-switch --version)" == "baseten-switch $BUILD_VERSION" ]] \
    || fail "built binary is not version-stamped as $BUILD_VERSION"
echo "ok (bin/baseten-switch $BUILD_VERSION; the door is the 'baseten-switch door' subcommand)"

# ------------------------------------------------------ scratch ports
step "scratch ports free"
for port in $ADMIN_PORT $CLIENT_PORT $DOOR_PORT $DEAD_ROUTER_PORT \
    $UPDOWN_ADMIN_PORT $UPDOWN_CLIENT_PORT $UPDOWN_DOOR_PORT; do
    if lsof -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
        fail "port $port is in use; free it or adjust check.sh scratch ports"
    fi
done
echo "ok"

# ------------------------------------------------------- smoke: whoami
# Exercises the real credential store (keychain + auth.json) and a live
# /v1/users/me round trip. --refresh forces the token-refresh round trip
# deterministically. The refreshed token persists to the same store the
# normal path writes; otherwise this step is read-only.
step "smoke: whoami --refresh (real credential store)"
AUTH_JSON="$HOME/Library/Application Support/baseten/auth.json"
if [[ "$OFFLINE" == 1 ]]; then
    echo "skipped (--offline)"
elif [[ ! -f "$AUTH_JSON" ]]; then
    echo "skipped (no baseten auth.json on this machine)"
else
    ./bin/baseten-switch whoami --refresh > "$TMPDIR_CHECK/whoami.out" 2>&1 \
        || { cat "$TMPDIR_CHECK/whoami.out"; fail "whoami --refresh exited non-zero against the real credential store"; }
    grep -qE 'Email|API key' "$TMPDIR_CHECK/whoami.out" \
        || { cat "$TMPDIR_CHECK/whoami.out"; fail "whoami output missing identity"; }
    echo "ok ($(head -1 "$TMPDIR_CHECK/whoami.out"))"
fi

# --------------------------------------------- smoke: scratch gateway
# Boots a throwaway gateway on scratch ports with a config that has an
# unresolved ${VAR} and a baseten-routed client with no credential, then
# asserts healthz plus both preflight warnings. Isolated via env: its own
# config, pidfile, telemetry, admin addr, and a nonexistent OAuth profile.
step "smoke: scratch gateway boot + preflight"
cat > "$TMPDIR_CHECK/gateway.yaml" <<EOF
global:
    routing_enabled: true
    auth:
        baseten: \${BASETEN_SWITCH_CHECK_MISSING}
    telemetry_dir: $TMPDIR_CHECK/telemetry
clients:
    - name: check-client
      enabled: true
      bind_addr: 127.0.0.1:$CLIENT_PORT
      protocol_shape: openai
      default_model: zai-org/GLM-5.2
      fallback_route: openai
EOF
env BASETEN_SWITCH_CONFIG_PATH="$TMPDIR_CHECK/gateway.yaml" \
    BASETEN_SWITCH_ENV_FILE="$TMPDIR_CHECK/nonexistent.env" \
    BASETEN_SWITCH_ADMIN_ADDR="127.0.0.1:$ADMIN_PORT" \
    BASETEN_SWITCH_GATEWAY_PIDFILE="$TMPDIR_CHECK/gw.pid" \
    BASETEN_API_KEY= \
    BASETEN_SWITCH_OAUTH_PROFILE=baseten-switch-check-nonexistent \
    ./bin/baseten-switch gateway start --foreground --port "$ADMIN_PORT" \
    > "$TMPDIR_CHECK/gateway.log" 2>&1 &
GW_PID=$!
PIDS+=("$GW_PID")
HEALTH=""
for _ in $(seq 1 25); do
    HEALTH="$(curl -fsS -m 1 "http://127.0.0.1:$ADMIN_PORT/healthz" 2>/dev/null || true)"
    [[ -n "$HEALTH" ]] && break
    kill -0 "$GW_PID" 2>/dev/null || break
    sleep 0.2
done
[[ -n "$HEALTH" ]] || { cat "$TMPDIR_CHECK/gateway.log"; fail "scratch gateway never became healthy on $ADMIN_PORT"; }
grep -q 'references \${BASETEN_SWITCH_CHECK_MISSING}' "$TMPDIR_CHECK/gateway.log" \
    || { cat "$TMPDIR_CHECK/gateway.log"; fail "preflight missing unresolved-placeholder warning"; }
grep -q 'no Baseten credential found' "$TMPDIR_CHECK/gateway.log" \
    || { cat "$TMPDIR_CHECK/gateway.log"; fail "preflight missing no-credential banner"; }
kill "$GW_PID" 2>/dev/null || true
wait "$GW_PID" 2>/dev/null || true
forget_pid "$GW_PID"
echo "ok (healthz + placeholder warning + credential banner)"

# ------------------------------------------------- smoke: scratch door
# Boots a throwaway door (the `baseten-switch door` subcommand) in flags mode
# pointing at a dead router port and asserts /doorz answers. No traffic
# is sent through it. BASETEN_SWITCH_DOOR_PIDFILE is scratched so the real
# ~/.config/baseten-switch/door.pid is never touched.
step "smoke: scratch door boot + doorz"
env BASETEN_SWITCH_DOOR_PIDFILE="$TMPDIR_CHECK/door.pid" \
    ./bin/baseten-switch door --port "$DOOR_PORT=anthropic:127.0.0.1:$DEAD_ROUTER_PORT" --probe-interval 1s \
    > "$TMPDIR_CHECK/door.log" 2>&1 &
DOOR_PID=$!
PIDS+=("$DOOR_PID")
DOORZ=""
for _ in $(seq 1 25); do
    DOORZ="$(curl -fsS -m 1 "http://127.0.0.1:$DOOR_PORT/doorz" 2>/dev/null || true)"
    if echo "$DOORZ" | grep -qE \
        '"tripped"[[:space:]]*:[[:space:]]*true'; then
        break
    fi
    kill -0 "$DOOR_PID" 2>/dev/null || break
    sleep 0.2
done
[[ -n "$DOORZ" ]] || { cat "$TMPDIR_CHECK/door.log"; fail "scratch door never answered /doorz on $DOOR_PORT"; }
echo "$DOORZ" | grep -qE '"tripped"[[:space:]]*:[[:space:]]*true' \
    || { echo "$DOORZ"; fail "/doorz did not report the dead router as tripped"; }
kill "$DOOR_PID" 2>/dev/null || true
wait "$DOOR_PID" 2>/dev/null || true
forget_pid "$DOOR_PID"
echo "ok"

# --------------------------------- smoke: up/down/status round trip
# Exercises the the lifecycle contract orchestrator on scratch ports with
# a scratch config: up starts router + door, status exits 0 and shows
# both up, down stops both, status exits nonzero after. Fully isolated
# via env (config, pidfiles, logs, admin addr); the live
# gateway/door on this machine are never touched.
step "smoke: up/down/status round trip (scratch ports)"
UP_DIR="$TMPDIR_CHECK/updown"
UPDOWN_MANAGED_PIDS=()
mkdir -p "$UP_DIR"
cat > "$UP_DIR/gateway.yaml" <<EOF
global:
    routing_enabled: false
    telemetry_dir: $UP_DIR/telemetry
clients:
    - name: updown-client
      enabled: true
      bind_addr: 127.0.0.1:$UPDOWN_CLIENT_PORT
      protocol_shape: anthropic
      default_model: zai-org/GLM-5.2
door:
    cooldown: 5s
    probe_interval: 1s
    ports:
        - bind_addr: 127.0.0.1:$UPDOWN_DOOR_PORT
          router_addr: 127.0.0.1:$UPDOWN_CLIENT_PORT
EOF

updown() {
    # BASETEN_SWITCH_LAUNCHD=off: the gate must never run launchctl operations
    # against the user's launchd session (supervision detection in
    # up/down/status is covered by unit tests with a fake runner).
    # BASETEN_SWITCH_MENUBAR=off: likewise, the gate must never quit or relaunch
    # the user's real menubar app (the up/down menubar step is covered
    # by unit tests over the runCmd seam).
    env BASETEN_SWITCH_LAUNCHD=off \
        BASETEN_SWITCH_MENUBAR=off \
        BASETEN_SWITCH_CONFIG_PATH="$UP_DIR/gateway.yaml" \
        BASETEN_SWITCH_ENV_FILE="$UP_DIR/nonexistent.env" \
        BASETEN_SWITCH_ADMIN_ADDR="127.0.0.1:$UPDOWN_ADMIN_PORT" \
        BASETEN_SWITCH_GATEWAY_PIDFILE="$UP_DIR/gw.pid" \
        BASETEN_SWITCH_DOOR_PIDFILE="$UP_DIR/door.pid" \
        BASETEN_SWITCH_GATEWAY_LOG="$UP_DIR/router.log" \
        BASETEN_SWITCH_DOOR_LOG="$UP_DIR/door.log" \
        BASETEN_SWITCH_TELEMETRY_DIR="$UP_DIR/telemetry" \
        BASETEN_SWITCH_OAUTH_PROFILE=baseten-switch-check-nonexistent \
        BASETEN_API_KEY= \
        ./bin/baseten-switch "$@"
}

updown up > "$UP_DIR/up.out" 2>&1 \
    || { cat "$UP_DIR/up.out" "$UP_DIR/router.log" "$UP_DIR/door.log" 2>/dev/null; fail "baseten-switch up failed on scratch ports"; }
# Register the daemons for trap cleanup in case a later step fails.
for pf in "$UP_DIR/gw.pid" "$UP_DIR/door.pid"; do
    if [[ -f "$pf" ]]; then
        managed_pid="$(cat "$pf")"
        PIDS+=("$managed_pid")
        UPDOWN_MANAGED_PIDS+=("$managed_pid")
    fi
done
[[ -f "$UP_DIR/gw.pid" ]] || { cat "$UP_DIR/up.out"; fail "up left no router pidfile"; }
[[ -f "$UP_DIR/door.pid" ]] || { cat "$UP_DIR/up.out"; fail "up left no door pidfile"; }

updown status > "$UP_DIR/status-up.out" 2>&1 \
    || { cat "$UP_DIR/status-up.out"; fail "status exited nonzero while both components are up"; }
grep -q 'Router:  up' "$UP_DIR/status-up.out" \
    || { cat "$UP_DIR/status-up.out"; fail "status missing 'Router:  up'"; }
grep -q 'Door:    up' "$UP_DIR/status-up.out" \
    || { cat "$UP_DIR/status-up.out"; fail "status missing 'Door:    up'"; }

# Idempotency: a second up must leave the healthy components alone.
updown up > "$UP_DIR/up2.out" 2>&1 \
    || { cat "$UP_DIR/up2.out"; fail "second up (idempotent) failed"; }
grep -q 'already up' "$UP_DIR/up2.out" \
    || { cat "$UP_DIR/up2.out"; fail "second up did not report already-up components"; }

updown down > "$UP_DIR/down.out" 2>&1 \
    || { cat "$UP_DIR/down.out"; fail "baseten-switch down failed"; }
for managed_pid in "${UPDOWN_MANAGED_PIDS[@]:-}"; do
    forget_pid "$managed_pid"
done
[[ ! -f "$UP_DIR/gw.pid" ]] || fail "down left the router pidfile behind"
[[ ! -f "$UP_DIR/door.pid" ]] || fail "down left the door pidfile behind"

if updown status > "$UP_DIR/status-down.out" 2>&1; then
    cat "$UP_DIR/status-down.out"
    fail "status exited 0 after down"
fi
grep -q 'Router:  DOWN' "$UP_DIR/status-down.out" \
    || { cat "$UP_DIR/status-down.out"; fail "status missing 'Router:  DOWN' after down"; }
echo "ok (up -> status 0 -> idempotent up -> down -> status nonzero)"

# -------------------------------- opt-in: API-key profile live contract
# The script always runs in dry-run mode so its public entrypoint is covered.
# The networked contract requires both an explicit opt-in and a caller-supplied
# test key. It creates a separate CLI store and starts only scratch processes.
step "API-key profile live contract"
"$REPO_DIR/scripts/tests/test_api_key_profile_live.sh" --dry-run \
    > "$TMPDIR_CHECK/api-key-profile-live-dry-run.out" \
    || fail "API-key profile live contract dry run"
if [[ "$OFFLINE" == 1 ]]; then
    echo "skipped (--offline; dry run passed)"
elif [[ "${BASETEN_SWITCH_RUN_API_KEY_PROFILE_LIVE:-}" == "1" ]]; then
    [[ -n "${BASETEN_SWITCH_TEST_API_KEY:-}" ]] \
        || fail "BASETEN_SWITCH_TEST_API_KEY is required for the opted-in live contract"
    env BASETEN_SWITCH_TEST_BINARY="$GW_DIR/bin/baseten-switch" \
        "$REPO_DIR/scripts/tests/test_api_key_profile_live.sh" \
        || fail "API-key profile live contract"
else
    echo "skipped (set BASETEN_SWITCH_RUN_API_KEY_PROFILE_LIVE=1 and BASETEN_SWITCH_TEST_API_KEY to opt in; dry run passed)"
fi

printf '\nALL CHECKS PASSED\n'
