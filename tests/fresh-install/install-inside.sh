#!/usr/bin/env bash
# install-inside.sh: runs INSIDE the fresh-install container as the
# non-root user. Places the single baseten-switch binary on PATH, generates
# gateway.yaml with
# `baseten-switch config init` (single-port door topology, route baseten),
# write the env file 0600, start the system with `baseten-switch up`
# (router + door), then verify: `baseten-switch status` exit 0, preflight
# output, a routed request through the DOOR port, and a telemetry row.
#
# Keyed mode (BASETEN_API_KEY set): the routed request must return 200 with
# real model content and log a telemetry row route=baseten status=200.
# No-key mode (FRESH_INSTALL_NO_KEY=1): the router must reject the
# keyless request with the needs-login 503. Through the door, that 503
# triggers the configured native fallback. The door's expected native 4xx
# proves the current keyless router and fallback path. The keyed lane covers
# request telemetry because credential rejection occurs before an upstream
# attempt exists to record.

set -uo pipefail

PASS=()
FAILED=()
ok()   { PASS+=("$1");   printf 'PASS: %s\n' "$1"; }
bad()  { FAILED+=("$1"); printf 'FAIL: %s\n' "$1"; }

GW_LOG="$HOME/.config/baseten-switch/logs/router.log"
DOOR_LOG="$HOME/.config/baseten-switch/logs/door.log"
TELEMETRY_DIR="$HOME/.config/baseten-switch/telemetry"
CFG="$HOME/.config/baseten-switch/gateway.yaml"
ENV_FILE="$HOME/.config/baseten-switch/env"

NO_KEY=0
if [[ "${FRESH_INSTALL_NO_KEY:-}" == "1" || -z "${BASETEN_API_KEY:-}" ]]; then
    NO_KEY=1
fi

step() { printf '\n== %s\n' "$1"; }

wait_http() { # url substring tries
    local url="$1" want="$2" tries="${3:-50}" body=""
    for _ in $(seq 1 "$tries"); do
        body="$(curl -fsS -m 2 "$url" 2>/dev/null || true)"
        if [[ -n "$body" && ( -z "$want" || "$body" == *"$want"* ) ]]; then
            echo "$body"
            return 0
        fi
        sleep 0.3
    done
    echo "$body"
    return 1
}

# ---------------------------------------------------- 1. binary on PATH
step "install the single binary onto PATH (~/.local/bin)"
mkdir -p "$HOME/.local/bin"
cp /opt/dist/baseten-switch "$HOME/.local/bin/"
chmod +x "$HOME/.local/bin/baseten-switch"
echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$HOME/.bashrc"
export PATH="$HOME/.local/bin:$PATH"
if command -v baseten-switch >/dev/null; then
    ok "baseten-switch on PATH"
else
    bad "baseten-switch on PATH"
fi

# ---------------------------------------------------- 2. gateway.yaml
step "generate gateway.yaml with 'baseten-switch config init' (single-port door topology)"
if baseten-switch config init; then
    ok "config init wrote the default config"
else
    bad "config init failed"
fi
if [[ -f "$CFG" ]]; then
    ok "gateway.yaml exists at $CFG"
else
    bad "gateway.yaml missing at $CFG after config init"
fi
if [[ "$(stat -c %a "$CFG")" == "600" ]]; then
    ok "gateway.yaml written with mode 0600"
else
    bad "gateway.yaml mode is $(stat -c %a "$CFG"), expected 600"
fi
# A second init must refuse over the existing file (no --force).
if baseten-switch config init > /tmp/init2.out 2>&1; then
    bad "second config init overwrote an existing config instead of refusing"
else
    ok "second config init refused over the existing file"
fi
if grep -q -- '--force' /tmp/init2.out; then
    ok "refusal names --force as the override"
else
    bad "refusal output missing --force hint: $(head -c 200 /tmp/init2.out)"
fi
# Sim adjustment (not part of the user flow): drop the fallback_route
# lines so the keyless checks below stay deterministic. With a native
# fallback configured, the router replays a credential-less baseten
# request against the native provider by design (resolveAttempts in
# cmd/gateway), which would replace the needs-login 503 assertion with
# a network-dependent native auth error.
sed -i '/fallback_route:/d' "$CFG"
ok "sim adjustment: fallback_route removed for deterministic keyless assertions"

# ---------------------------------------------------- 3. env file 0600
step "write env file (mode 0600)"
umask 177
{
    if [[ "$NO_KEY" == 0 ]]; then
        printf 'BASETEN_API_KEY=%s\n' "$BASETEN_API_KEY"
    fi
    printf 'BASETEN_SWITCH_API_KEY_FALLBACK=1\n'
    printf 'ANTHROPIC_AUTH_TOKEN=baseten-switch-fresh-install-local-token\n'
} > "$ENV_FILE"
umask 022
chmod 600 "$ENV_FILE"
if [[ "$(stat -c %a "$ENV_FILE")" == "600" ]]; then
    ok "env file written with mode 0600"
else
    bad "env file mode is $(stat -c %a "$ENV_FILE"), expected 600"
fi
# Drop the key from this shell so the gateway must load it from the env
# file, exactly like a fresh login shell would.
unset BASETEN_API_KEY 2>/dev/null || true

# ---------------------------------------------------- 4. start the system
step "start the system: baseten-switch up (router + door)"
if baseten-switch up; then
    ok "baseten-switch up started router + door and reported healthy"
else
    bad "baseten-switch up failed"
    echo "--- $GW_LOG ---"; cat "$GW_LOG" 2>/dev/null
    echo "--- $DOOR_LOG ---"; cat "$DOOR_LOG" 2>/dev/null
fi

if baseten-switch status > /tmp/status.out 2>&1; then
    ok "baseten-switch status exit 0 with both components up"
else
    bad "baseten-switch status exited nonzero after up"
fi
cat /tmp/status.out
if grep -q 'Router:  up' /tmp/status.out && grep -q 'Door:    up' /tmp/status.out; then
    ok "status shows Router and Door up"
else
    bad "status output missing Router/Door up lines"
fi
if [[ -f "$HOME/.config/baseten-switch/door.pid" ]]; then
    ok "door pidfile written"
else
    bad "door pidfile missing"
fi

if wait_http "http://127.0.0.1:45272/healthz" '"client":"claude-code"' >/dev/null; then
    ok "router client listener healthy on 45272"
else
    bad "router client listener on 45272 never became healthy"
fi
if wait_http "http://127.0.0.1:45273/v1/admin/healthz" "" >/dev/null; then
    ok "admin healthz on 45273"
else
    bad "admin healthz on 45273"
fi
DOORZ="$(wait_http "http://127.0.0.1:45271/doorz" '"tripped":false')" \
    && ok "door up on 45271, not tripped" \
    || { bad "doorz on 45271 (got: ${DOORZ:0:200})"; cat "$DOOR_LOG" 2>/dev/null; }

# ---------------------------------------------------- 5. preflight output
step "verify startup preflight output in the gateway log"
if grep -Fq 'warning: gateway.yaml references ${ANTHROPIC_API_KEY}' "$GW_LOG"; then
    ok "preflight unresolved-placeholder warning present (ANTHROPIC_API_KEY, expected on a Baseten-only machine)"
else
    bad "preflight unresolved-placeholder warning missing from $GW_LOG"
fi
if grep -q '\[gateway\] auth: profile=' "$GW_LOG"; then
    ok "auth state line present: $(grep '\[gateway\] auth: profile=' "$GW_LOG" | tail -1)"
else
    bad "auth state line missing from $GW_LOG"
fi
if [[ "$NO_KEY" == 1 ]]; then
    if grep -q 'no Baseten credential found' "$GW_LOG"; then
        ok "no-credential banner present (expected in no-key mode)"
    else
        bad "no-credential banner missing in no-key mode"
    fi
fi

# Informational only: print the same aggregate health view recommended by the
# install guide.
echo "--- baseten-switch status ---"
baseten-switch status || true

# ---------------------------------------------------- 6. routed request
REQ_BODY='{"model":"claude-opus-4-8","max_tokens":128,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}'

post_messages() { # port outfile hdrfile
    curl -sS -m 120 -o "$2" -D "$3" -w '%{http_code}' \
        -X POST "http://127.0.0.1:$1/v1/messages" \
        -H 'content-type: application/json' \
        -H 'anthropic-version: 2023-06-01' \
        -H 'Authorization: Bearer baseten-switch-fresh-install-local-token' \
        -d "$REQ_BODY" 2>/dev/null
}

if [[ "$NO_KEY" == 0 ]]; then
    step "routed request through the DOOR port (45271) to Baseten"
    CODE="$(post_messages 45271 /tmp/resp.json /tmp/resp.hdr)"
    if [[ "$CODE" == "200" ]]; then
        TEXT="$(sed -n 's/.*"text":"\([^"]*\)".*/\1/p' /tmp/resp.json | head -1)"
        if [[ -n "$TEXT" ]]; then
            ok "routed request returned 200 with content: ${TEXT:0:120}"
        else
            bad "routed request returned 200 but no text content: $(head -c 300 /tmp/resp.json)"
        fi
    else
        bad "routed request returned HTTP $CODE: $(head -c 300 /tmp/resp.json)"
    fi

    step "telemetry row (route=baseten, status 200)"
    ROW="$(grep -h '"configured_route":"baseten"' "$TELEMETRY_DIR"/requests-*.jsonl 2>/dev/null | tail -1)"
    if [[ "$ROW" == *'"client":"claude-code"'* && "$ROW" == *'"status":200'* ]]; then
        ok "telemetry row: ${ROW:0:260}"
    else
        bad "expected telemetry row with client=claude-code route=baseten status=200; got: ${ROW:0:260}"
    fi
else
    step "NO-KEY MODE: routed-request step skipped (no Baseten API key provided)"
    echo "NOTICE: running the plumbing stub checks instead."

    # 6a. Against the ROUTER port the gateway must fail fast with the
    # needs-login 503. Through the DOOR the same 503 trips the failover
    # and the request is replayed against the native provider (that is
    # the door working as designed, see internal/door), so the
    # deterministic keyless assertion targets the router directly.
    CODE="$(post_messages 45272 /tmp/resp.json /tmp/resp.hdr)"
    if [[ "$CODE" == "503" ]] && grep -qi '^x-baseten-switch: needs-login' /tmp/resp.hdr; then
        ok "keyless request rejected with 503 needs-login on the router"
    else
        bad "expected 503 with X-Baseten-Switch: needs-login from the router; got HTTP $CODE ($(head -c 200 /tmp/resp.json))"
    fi

    # Through the door, the 503 becomes a native-provider failover response.
    # The token is intentionally synthetic, so the native provider must reject
    # it with a 4xx while the door stamps the response as fallback-served.
    DCODE="$(post_messages 45271 /tmp/door.json /tmp/door.hdr)"
    if [[ "$DCODE" =~ ^4[0-9][0-9]$ ]] &&
        grep -qi '^x-baseten-switch-door: fallback' /tmp/door.hdr; then
        ok "door replayed the keyless request to native fallback (HTTP $DCODE)"
    else
        bad "expected native fallback 4xx with X-Baseten-Switch-Door: fallback; got HTTP $DCODE ($(head -c 200 /tmp/door.json))"
    fi
fi

# ---------------------------------------------------- summary
printf '\n========== FRESH INSTALL SUMMARY ==========\n'
printf 'pass: %d\n' "${#PASS[@]}"
printf 'fail: %d\n' "${#FAILED[@]}"
if [[ "${#FAILED[@]}" -gt 0 ]]; then
    printf 'failed checks:\n'
    for f in "${FAILED[@]}"; do printf '  - %s\n' "$f"; done
    printf 'RESULT: FAIL\n'
    exit 1
fi
printf 'RESULT: PASS\n'
exit 0
