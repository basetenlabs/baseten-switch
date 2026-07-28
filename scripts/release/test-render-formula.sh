#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RENDERER="$SCRIPT_DIR/render-formula.sh"
TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

formula="$TMP_ROOT/Formula/baseten-switch.rb"
patch="$TMP_ROOT/baseten-switch_1.2.3_homebrew.patch"
checksum="0123456789abcdef0123456789abcdef0123456789abcdef0123456789ABCDEF"
"$RENDERER" \
    --tag v1.2.3 \
    --sha256 "$checksum" \
    --approved-license-spdx MIT \
    --output "$formula" \
    --patch-output "$patch" >/dev/null

[[ -f "$formula" ]] || fail "renderer did not create the formula"
[[ -f "$patch" ]] || fail "renderer did not create the tap patch"
[[ "$(stat -f '%Lp' "$formula" 2>/dev/null || stat -c '%a' "$formula")" == 644 ]] \
    || fail "formula mode is not 0644"
ruby -c "$formula" >/dev/null || fail "formula is not valid Ruby syntax"
tap_checkout="$TMP_ROOT/tap-checkout"
mkdir -p "$tap_checkout"
git -C "$tap_checkout" init -q
git -C "$tap_checkout" apply "$patch" \
    || fail "rendered tap patch does not apply to a clean checkout"
cmp "$formula" "$tap_checkout/Formula/baseten-switch.rb" \
    || fail "tap patch does not produce the rendered formula"

grep -Fqx '# typed: strict' "$formula" \
    || fail "formula omitted the Homebrew Sorbet sigil"
grep -Fqx '# frozen_string_literal: true' "$formula" \
    || fail "formula omitted the frozen string literal directive"
grep -Fqx 'class BasetenSwitch < Formula' "$formula" \
    || fail "formula class is not BasetenSwitch"
if grep -Eq '^[[:space:]]*version[[:space:]]' "$formula"; then
    fail "formula contains a redundant explicit version stanza"
fi
if command -v brew >/dev/null 2>&1; then
    derived_version="$(
        brew ruby -e \
            'require "formulary"; path = Pathname(ARGV.fetch(0)); puts Formulary.from_contents("baseten-switch", path, path.read).version' \
            "$formula"
    )"
    [[ "$derived_version" == "1.2.3" ]] \
        || fail "Homebrew derived version '$derived_version' instead of 1.2.3"
fi
grep -Fqx \
    '    assert_match "baseten-switch v#{version}", shell_output("#{bin}/baseten-switch --version")' \
    "$formula" || fail "formula test does not use Homebrew's derived version"
grep -Fqx '  license "MIT"' "$formula" \
    || fail "formula license does not use the explicit approved SPDX input"
grep -Fqx \
    '  url "https://github.com/basetenlabs/baseten-switch/releases/download/v1.2.3/baseten-switch_1.2.3_darwin_universal.zip"' \
    "$formula" || fail "formula URL is not canonical"
grep -Fqx \
    '  sha256 "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"' \
    "$formula" || fail "formula checksum is not normalized and pinned"
grep -Fqx '  depends_on "basetenlabs/baseten/baseten"' "$formula" \
    || fail "formula does not depend on the fully qualified Baseten CLI"
grep -Fqx '  depends_on :macos' "$formula" \
    || fail "formula is not restricted to macOS"
grep -Fqx '    depends_on macos: :ventura' "$formula" \
    || fail "formula does not require Ventura"
for caveat in \
    'Baseten Switch is beta software. The bundled Mac app is ad-hoc signed' \
    'and is not notarized by Apple.' \
    'If macOS blocks the app, try to open it once, then open System Settings >' \
    'Privacy & Security and click Open Anyway. Managed Macs may prohibit this' \
    'override; contact your administrator if Open Anyway is unavailable.'; do
    grep -Fqx "      $caveat" "$formula" \
        || fail "formula omitted beta caveat: $caveat"
done
for payload in \
    'bin.install "bin/baseten-switch"' \
    'pkgshare.install "Baseten Switch.app.zip"' \
    'pkgshare.install "LICENSE", "THIRD_PARTY_NOTICES.md"'; do
    grep -Fqx "    $payload" "$formula" \
        || fail "formula omitted install contract: $payload"
done

failure_log="$TMP_ROOT/failure.log"
if "$RENDERER" \
    --tag v1.2.3 \
    --sha256 "$checksum" \
    --output "$TMP_ROOT/no-license.rb" \
    --patch-output "$TMP_ROOT/no-license.patch" >"$failure_log" 2>&1; then
    fail "renderer accepted a missing approved SPDX license"
fi
grep -Fq -- '--approved-license-spdx' "$failure_log" \
    || fail "missing-license failure was not actionable"

if "$RENDERER" \
    --tag 1.2.3 \
    --sha256 "$checksum" \
    --approved-license-spdx MIT \
    --output "$TMP_ROOT/bad-tag.rb" \
    --patch-output "$TMP_ROOT/bad-tag.patch" >"$failure_log" 2>&1; then
    fail "renderer accepted a tag without the required v prefix"
fi

if "$RENDERER" \
    --tag v1.2.3 \
    --sha256 not-a-checksum \
    --approved-license-spdx MIT \
    --output "$TMP_ROOT/bad-checksum.rb" \
    --patch-output "$TMP_ROOT/bad-checksum.patch" >"$failure_log" 2>&1; then
    fail "renderer accepted an invalid checksum"
fi

if "$RENDERER" \
    --tag v1.2.3 \
    --sha256 "$checksum" \
    --approved-license-spdx 'Apache-2.0"; system("id")' \
    --output "$TMP_ROOT/injected.rb" \
    --patch-output "$TMP_ROOT/injected.patch" >"$failure_log" 2>&1; then
    fail "renderer accepted an unsafe license value"
fi

if "$RENDERER" \
    --tag v1.2.3 \
    --sha256 "$checksum" \
    --approved-license-spdx MIT \
    --output "$formula" \
    --patch-output "$TMP_ROOT/overwrite.patch" >"$failure_log" 2>&1; then
    fail "renderer replaced an existing formula"
fi
grep -Fq 'refusing to replace existing output' "$failure_log" \
    || fail "overwrite refusal was not actionable"

printf 'PASS: Homebrew formula rendering contract\n'
