#!/usr/bin/env bash
# Opt-in live contract for a Baseten CLI API-key profile.
#
# This script creates all credentials and runtime state under one temporary
# directory. It never starts, stops, or reconfigures an installed Switch.

set -euo pipefail

# Never allow inherited shell tracing to expose the test key.
set +x

if [[ "${1:-}" == "--dry-run" ]]; then
    printf '%s\n' \
        "api-key profile live contract: dry run" \
        "would create an isolated Baseten CLI profile and Switch runtime" \
        "would test /v1/models and one minimal inference through a scratch door" \
        "would scan scratch output for credential disclosure before cleanup"
    exit 0
fi
if [[ "$#" -ne 0 ]]; then
    printf 'usage: %s [--dry-run]\n' "$0" >&2
    exit 2
fi

fail() {
    printf 'api-key profile live contract: %s\n' "$*" >&2
    exit 1
}

for command_name in baseten curl go lsof; do
    command -v "$command_name" >/dev/null 2>&1 \
        || fail "required command is unavailable: $command_name"
done

api_key="${BASETEN_SWITCH_TEST_API_KEY:-}"
unset BASETEN_SWITCH_TEST_API_KEY
[[ -n "$api_key" ]] \
    || fail "BASETEN_SWITCH_TEST_API_KEY must contain an inference-capable test key"

model="${BASETEN_SWITCH_TEST_MODEL:-zai-org/GLM-5.2}"
if [[ "$model" != */* || ! "$model" =~ ^[A-Za-z0-9._/-]+$ ]]; then
    fail "BASETEN_SWITCH_TEST_MODEL must be a Baseten model slug"
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_dir="$(cd "$script_dir/../.." && pwd -P)"
scratch_parent="${TMPDIR:-/tmp}"
scratch_parent="${scratch_parent%/}"
scratch_dir="$(mktemp -d "$scratch_parent/baseten-switch-api-key-live.XXXXXX")"
chmod 700 "$scratch_dir"

profile="switch-api-key-test"
baseten_config_dir="$scratch_dir/baseten"
auth_file="$baseten_config_dir/auth.json"
switch_dir="$scratch_dir/switch"
output_dir="$scratch_dir/output"
telemetry_dir="$switch_dir/telemetry"
router_log="$switch_dir/router.log"
door_log="$switch_dir/door.log"
router_pidfile="$switch_dir/router.pid"
door_pidfile="$switch_dir/door.pid"
config_path="$switch_dir/gateway.yaml"
env_file="$switch_dir/env"
binary="${BASETEN_SWITCH_TEST_BINARY:-$scratch_dir/bin/baseten-switch}"

mkdir -p "$baseten_config_dir" "$switch_dir" "$output_dir" \
    "$telemetry_dir" "$scratch_dir/home" "$scratch_dir/bin"
chmod 700 "$baseten_config_dir" "$switch_dir" "$output_dir" "$telemetry_dir"
: > "$env_file"
chmod 600 "$env_file"

pids=()
scan_for_key() {
    local found=""
    [[ -d "$scratch_dir" && -n "$api_key" ]] || return 0
    found="$({
        find "$scratch_dir" -type f \
            ! -path "$auth_file" \
            ! -path "$binary" \
            -exec grep -Fl -- "$api_key" {} + 2>/dev/null || true
    } | head -1)"
    [[ -z "$found" ]]
}

cleanup() {
    local status=$?
    local pid
    trap - EXIT INT TERM
    set +e
    for pid in "${pids[@]:-}"; do
        if [[ -n "$pid" ]]; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done
    if ! scan_for_key; then
        printf '%s\n' \
            "api-key profile live contract: test key appeared outside the temporary credential file" >&2
        status=1
    fi
    case "$scratch_dir" in
        "$scratch_parent"/baseten-switch-api-key-live.*)
            [[ -d "$scratch_dir" ]] && rm -rf -- "$scratch_dir"
            ;;
        *)
            printf '%s\n' \
                "api-key profile live contract: refused to remove an unexpected scratch path" >&2
            status=1
            ;;
    esac
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

valid_port() {
    local port="$1"
    case "$port" in
        ''|*[!0-9]*) return 1 ;;
    esac
    (( port >= 1024 && port <= 65535 ))
}

port_is_free() {
    ! lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}

choose_port() {
    local requested="$1"
    local offset="$2"
    local port
    if [[ -n "$requested" ]]; then
        valid_port "$requested" || fail "scratch port override is invalid"
        port_is_free "$requested" || fail "scratch port override is already in use"
        printf '%s\n' "$requested"
        return
    fi
    port=$((30000 + (($$ + offset) % 30000)))
    while (( port <= 65535 )); do
        if port_is_free "$port"; then
            printf '%s\n' "$port"
            return
        fi
        port=$((port + 1))
    done
    fail "could not find a free scratch port"
}

admin_port="$(choose_port "${BASETEN_SWITCH_TEST_ADMIN_PORT:-}" 101)"
router_port="$(choose_port "${BASETEN_SWITCH_TEST_ROUTER_PORT:-}" 211)"
door_port="$(choose_port "${BASETEN_SWITCH_TEST_DOOR_PORT:-}" 307)"
[[ "$admin_port" != "$router_port" && "$admin_port" != "$door_port" && \
    "$router_port" != "$door_port" ]] || fail "scratch ports must be distinct"

if [[ -z "${BASETEN_SWITCH_TEST_BINARY:-}" ]]; then
    (
        cd "$repo_dir/gateway"
        go build -o "$binary" ./cmd/baseten-switch
    ) > "$output_dir/build.out" 2>&1 \
        || fail "could not build the test binary"
fi
[[ -x "$binary" ]] || fail "test binary is not executable"

# The API key travels over stdin. The CLI stores it only in the isolated,
# insecure-storage auth.json that cleanup removes.
printf '%s\n' "$api_key" |
    env HOME="$scratch_dir/home" \
        BASETEN_CONFIG_DIR="$baseten_config_dir" \
        BASETEN_REMOTE_URL="https://api.baseten.co" \
        baseten auth login \
            --with-api-key \
            --profile "$profile" \
            --insecure-storage \
            --output none \
            > "$output_dir/login.out" 2>&1 \
    || fail "Baseten CLI API-key login failed"

[[ -f "$auth_file" ]] || fail "Baseten CLI did not create the isolated credential file"

cat > "$config_path" <<EOF
global:
  routing_enabled: true
  telemetry_dir: \${BASETEN_SWITCH_TELEMETRY_DIR}
  telemetry_enabled: true
clients:
  - name: api-key-profile-live
    enabled: true
    bind_addr: 127.0.0.1:$router_port
    protocol_shape: openai
    default_model: $model
door:
  cooldown: 2s
  probe_interval: 1s
  ports:
    - bind_addr: 127.0.0.1:$door_port
      router_addr: 127.0.0.1:$router_port
EOF
chmod 600 "$config_path"

switch_env=(
    "HOME=$scratch_dir/home"
    "BASETEN_CONFIG_DIR=$baseten_config_dir"
    "BASETEN_REMOTE_URL=https://api.baseten.co"
    "BASETEN_BASE_URL=https://inference.baseten.co"
    "BASETEN_SWITCH_AUTH_FILE="
    "BASETEN_SWITCH_AUTH_NO_KEYRING=1"
    "BASETEN_SWITCH_OAUTH_PROFILE=$profile"
    "BASETEN_SWITCH_CONFIG_PATH=$config_path"
    "BASETEN_SWITCH_ENV_FILE=$env_file"
    "BASETEN_SWITCH_ADMIN_ADDR=127.0.0.1:$admin_port"
    "BASETEN_SWITCH_GATEWAY_PIDFILE=$router_pidfile"
    "BASETEN_SWITCH_DOOR_PIDFILE=$door_pidfile"
    "BASETEN_SWITCH_GATEWAY_LOG=$router_log"
    "BASETEN_SWITCH_DOOR_LOG=$door_log"
    "BASETEN_SWITCH_TELEMETRY_DIR=$telemetry_dir"
    "BASETEN_SWITCH_API_KEY_FALLBACK=0"
    "BASETEN_SWITCH_PRIVATE_RUNTIME=0"
    "BASETEN_SWITCH_LAUNCHD=off"
    "BASETEN_SWITCH_MENUBAR=off"
    "BASETEN_API_KEY="
    "ANTHROPIC_API_KEY="
    "OPENAI_API_KEY="
)

unset BASETEN_API_KEY ANTHROPIC_API_KEY OPENAI_API_KEY

env "${switch_env[@]}" \
    "$binary" gateway start --foreground --port "$admin_port" \
    > "$router_log" 2>&1 &
router_pid=$!
pids+=("$router_pid")

wait_for_url() {
    local url="$1"
    local pid="$2"
    for _ in $(seq 1 50); do
        curl -fsS -m 1 "$url" >/dev/null 2>&1 && return 0
        kill -0 "$pid" 2>/dev/null || return 1
        sleep 0.2
    done
    return 1
}

wait_for_url "http://127.0.0.1:$admin_port/healthz" "$router_pid" \
    || fail "scratch router did not become healthy"

env "${switch_env[@]}" \
    "$binary" door --config "$config_path" \
    > "$door_log" 2>&1 &
door_pid=$!
pids+=("$door_pid")

wait_for_url "http://127.0.0.1:$door_port/doorz" "$door_pid" \
    || fail "scratch door did not become healthy"

curl -fsS -m 15 \
    "http://127.0.0.1:$admin_port/v1/admin/auth/status" \
    > "$output_dir/auth-status.json" \
    || fail "admin auth status request failed"

grep -Eq '"signed_in"[[:space:]]*:[[:space:]]*true' \
    "$output_dir/auth-status.json" \
    || fail "admin auth status did not report a selected profile"
grep -Eq '"auth_type"[[:space:]]*:[[:space:]]*"api_key"' \
    "$output_dir/auth-status.json" \
    || fail "admin auth status did not report API-key authentication"
grep -Eq '"profile"[[:space:]]*:[[:space:]]*"switch-api-key-test"' \
    "$output_dir/auth-status.json" \
    || fail "admin auth status did not report the isolated profile"
grep -Eq '"health"[[:space:]]*:[[:space:]]*"ok"' \
    "$output_dir/auth-status.json" \
    || fail "admin auth status did not report healthy authentication"
grep -Eq '"fallback_enabled"[[:space:]]*:[[:space:]]*false' \
    "$output_dir/auth-status.json" \
    || fail "environment API-key fallback was not disabled"
grep -Eq '"fallback_in_use"[[:space:]]*:[[:space:]]*false' \
    "$output_dir/auth-status.json" \
    || fail "admin auth status reported fallback use"

models_status="$(curl -sS -m 60 \
    -o "$output_dir/models.json" \
    -w '%{http_code}' \
    "http://127.0.0.1:$door_port/v1/models")" \
    || fail "model discovery request failed"
[[ "$models_status" == "200" ]] \
    || fail "model discovery did not return HTTP 200"

cat > "$output_dir/inference-request.json" <<EOF
{"model":"$model","messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":1,"stream":false}
EOF
inference_status="$(curl -sS -m 180 \
    -H 'Content-Type: application/json' \
    -o "$output_dir/inference-response.json" \
    -w '%{http_code}' \
    --data-binary "@$output_dir/inference-request.json" \
    "http://127.0.0.1:$door_port/v1/chat/completions")" \
    || fail "minimal inference request failed"
[[ "$inference_status" == "200" ]] \
    || fail "minimal inference did not return HTTP 200"

telemetry_ok=0
for _ in $(seq 1 25); do
    if grep -R -Eq '"client":"api-key-profile-live".*"effective_provider":"baseten"' \
        "$telemetry_dir" 2>/dev/null; then
        telemetry_ok=1
        break
    fi
    sleep 0.2
done
[[ "$telemetry_ok" == 1 ]] \
    || fail "telemetry did not attribute inference to Baseten"

scan_for_key \
    || fail "test key appeared outside the temporary credential file"

printf '%s\n' "api-key profile live contract: ok"
