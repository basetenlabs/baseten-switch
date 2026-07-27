#!/usr/bin/env bash
# Pin the shipping Stable and Preview port contract without treating arbitrary
# test fixtures or check.sh scratch listeners as product defaults.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd -P)"

fail() {
    printf 'port contract: %s\n' "$*" >&2
    exit 1
}

require_text() {
    local file="$1"
    local text="$2"
    grep -Fq "$text" "$REPO_DIR/$file" \
        || fail "$file is missing required value: $text"
}

public_shipping_files=(
    README.md
    config/gateway.example.yaml
    config/schema.md
    gateway/README.md
    gateway/cmd/baseten-switch/claude_adapter.go
    gateway/cmd/baseten-switch/codex_adapter.go
    gateway/cmd/baseten-switch/doctor.go
    gateway/cmd/baseten-switch/main.go
    gateway/cmd/gateway/gateway.go
    gateway/internal/config/gateway.example.yaml
    gateway/internal/config/inittemplate.go
    gateway/internal/door/config.go
    gateway/internal/door/door.go
    gateway/internal/door/fromconfig.go
    gateway/internal/doorcli/doorcli.go
    mac/BasetenSwitch/Sources/BasetenSwitch/AppVariant.swift
    mac/BasetenSwitch/Sources/BasetenSwitch/BasetenSwitchState.swift
    mac/BasetenSwitch/Sources/BasetenSwitch/PopupPreviewFixtures.swift
    tests/fresh-install/install-inside.sh
    tests/fresh-install/README.md
)

shipping_files=("${public_shipping_files[@]}")

require_text gateway/cmd/gateway/gateway.go 'DefaultPort         = 45273'
require_text gateway/cmd/gateway/gateway.go 'DefaultAdminAddr    = "127.0.0.1:45273"'
require_text config/gateway.example.yaml 'bind_addr: 127.0.0.1:45271'
require_text config/gateway.example.yaml 'router_addr: 127.0.0.1:45272'
require_text mac/BasetenSwitch/Sources/BasetenSwitch/AppVariant.swift \
    'defaultPorts: [45271]'
require_text mac/BasetenSwitch/Sources/BasetenSwitch/AppVariant.swift \
    'defaultPort: 45273'
require_text mac/BasetenSwitch/Sources/BasetenSwitch/AppVariant.swift \
    '"BASETEN_SWITCH_GATEWAY_PORT": "45373"'
require_text mac/BasetenSwitch/Sources/BasetenSwitch/AppVariant.swift \
    '"BASETEN_SWITCH_DOOR_PORTS": "45371"'
require_text mac/BasetenSwitch/Sources/BasetenSwitch/BasetenSwitchState.swift \
    '$0.bindAddr != "127.0.0.1:45372"'

legacy_pattern='(^|[^0-9])(8081|8787|18081|18787|18789|18790)([^0-9]|$)'
legacy_hits="$(
    cd "$REPO_DIR"
    grep -nE "$legacy_pattern" "${shipping_files[@]}" || true
)"
[[ -z "$legacy_hits" ]] \
    || fail "legacy product defaults remain in shipping files:
$legacy_hits"

# scripts/check.sh intentionally owns a separate scratch-port namespace. Tests
# may also use arbitrary listener numbers when the number is not a default.
require_text scripts/check.sh 'ADMIN_PORT="${BASETEN_SWITCH_CHECK_ADMIN_PORT:-28787}"'
require_text scripts/check.sh 'CLIENT_PORT="${BASETEN_SWITCH_CHECK_CLIENT_PORT:-28081}"'
require_text scripts/check.sh 'DOOR_PORT="${BASETEN_SWITCH_CHECK_DOOR_PORT:-28082}"'

printf 'port contract: ok\n'
