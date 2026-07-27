#!/usr/bin/env bash
# run.sh: fresh-install container simulation for baseten-switch.
#
# Cross-compiles the single linux binary from the current tree, builds
# a minimal Debian image simulating a brand-new machine (non-root user,
# curl, ca certificates, nothing else), then runs install-inside.sh in
# the container. install-inside.sh exercises the source-build lifecycle
# (baseten-switch up starts router + door) and verifies a routed
# request plus a telemetry row. It is the end-to-end proof of the
# the lifecycle contract orchestrator on a clean system.
#
# Usage: tests/fresh-install/run.sh [--no-key]
#
# Key sourcing (never printed): BASETEN_API_KEY from the host environment,
# else parsed from ~/.config/baseten-switch/env. With no
# key (or --no-key) the routed-request step is replaced by a stub check
# that still proves the door -> router -> telemetry plumbing.
#
# The container publishes NO ports to the host; every check runs inside
# it. The live gateway/door on this host are never touched.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GW_DIR="$(cd "$HERE/../../gateway" && pwd)"
IMAGE=baseten-switch-fresh-install
CONTAINER=baseten-switch-fresh-install-run

MODE=keyed
[[ "${1:-}" == "--no-key" ]] && MODE=no-key

step() { printf '\n== %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || fail "docker not found on PATH"
command -v go >/dev/null 2>&1 || fail "go toolchain not found on PATH"

# ------------------------------------------------------------ arch
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
    arm64|aarch64) GOARCH=arm64 ;;
    x86_64|amd64)  GOARCH=amd64 ;;
    *) fail "unsupported host arch: $HOST_ARCH" ;;
esac
PLATFORM="linux/$GOARCH"

# ------------------------------------------------------------ key
# Resolve the Baseten API key without ever echoing it.
BASETEN_SWITCH_KEY="${BASETEN_API_KEY:-}"
if [[ "$MODE" == keyed && -z "$BASETEN_SWITCH_KEY" ]]; then
    ENV_FILE="$HOME/.config/baseten-switch/env"
    if [[ -r "$ENV_FILE" ]]; then
        BASETEN_SWITCH_KEY="$(sed -n 's/^BASETEN_API_KEY=//p' "$ENV_FILE" | head -1 | sed -e "s/^[\"']//" -e "s/[\"']\$//")"
    fi
fi
if [[ "$MODE" == keyed && -z "$BASETEN_SWITCH_KEY" ]]; then
    echo "NOTICE: no Baseten API key in the host env or ~/.config/baseten-switch/env."
    echo "NOTICE: falling back to --no-key mode (routed-request step will be skipped)."
    MODE=no-key
fi

# ------------------------------------------------------------ build
step "cross-compile linux/$GOARCH binary from the current tree"
mkdir -p "$HERE/build"
(cd "$GW_DIR" && \
    CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath \
        -o "$HERE/build/baseten-switch" ./cmd/baseten-switch)
echo "ok (build/baseten-switch; single binary, the door is 'baseten-switch door')"

step "build image $IMAGE ($PLATFORM)"
docker build --platform "$PLATFORM" -t "$IMAGE" "$HERE"

# ------------------------------------------------------------ run
step "run fresh-install simulation in container (mode: $MODE)"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true

RUN_ARGS=(--rm --name "$CONTAINER" --platform "$PLATFORM")
if [[ "$MODE" == keyed ]]; then
    # -e NAME (no value) copies from this process's environment, so the
    # key never appears in the docker argv or in any image layer.
    export BASETEN_API_KEY="$BASETEN_SWITCH_KEY"
    RUN_ARGS+=(-e BASETEN_API_KEY)
else
    RUN_ARGS+=(-e FRESH_INSTALL_NO_KEY=1)
fi

rc=0
docker run "${RUN_ARGS[@]}" "$IMAGE" || rc=$?

# Containers are removed by --rm; the image stays cached for reruns.
if [[ $rc -ne 0 ]]; then
    fail "in-container install verification failed (exit $rc)"
fi
printf '\nFRESH INSTALL SIMULATION PASSED (mode: %s)\n' "$MODE"
