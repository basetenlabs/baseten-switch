#!/usr/bin/env bash
# Render a side-effect-free debug status item without starting the gateway or
# sending model traffic. Stop it with Ctrl-C.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
PACKAGE_DIR="$(dirname "$SCRIPT_DIR")/mac/BasetenSwitch"
STATE="${1:-activity}"
FIXTURE="native-off"
FORCE_ACTIVITY=0

case "$STATE" in
    idle) ;;
    active) FIXTURE="healthy" ;;
    degraded) FIXTURE="fallback" ;;
    activity) FORCE_ACTIVITY=1 ;;
    *)
        printf 'Usage: %s [idle|active|degraded|activity]\n' "$0" >&2
        exit 2
        ;;
esac

cd "$PACKAGE_DIR"
exec env \
    BASETEN_SWITCH_POPUP_PREVIEW="$FIXTURE" \
    BASETEN_SWITCH_POPUP_AUTO_OPEN=0 \
    BASETEN_SWITCH_MENUBAR_ACTIVITY="$FORCE_ACTIVITY" \
    swift run BasetenSwitch
