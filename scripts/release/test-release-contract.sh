#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
PACKAGER="$SCRIPT_DIR/build-artifacts.sh"
CANONICAL_INSTALL="brew install basetenlabs/baseten/baseten-switch"
INSTALL_SURFACES=(
  "$REPO_DIR/README.md"
  "$REPO_DIR/scripts/release/INSTALL.md"
  "$REPO_DIR/scripts/release/install.sh"
)

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

bash -n \
  "$REPO_DIR/scripts/build-menubar.sh" \
  "$PACKAGER"
sh -n "$SCRIPT_DIR/install.sh"

for install_surface in "${INSTALL_SURFACES[@]}"; do
  grep -Fq "$CANONICAL_INSTALL" "$install_surface" \
    || fail "$install_surface omitted the canonical single-formula install"
done
if grep -F 'basetenlabs/baseten/baseten ' "${INSTALL_SURFACES[@]}"; then
  fail "a redundant two-formula install command remains"
fi

dry_output="$(
  BASETEN_SWITCH_RELEASE_TAG=v1.2.3 \
    "$PACKAGER" --dry-run
)"
grep -q 'baseten-switch_1.2.3_darwin_universal.zip' <<<"$dry_output" \
  || fail "dry run omitted the canonical artifact name"
grep -q 'Baseten Switch.app.zip' <<<"$dry_output" \
  || fail "dry run omitted the nested app ZIP"
grep -q 'checksums.txt (final ZIP and SBOM)' <<<"$dry_output" \
  || fail "dry run omitted the checksum contract"
grep -q 'this script never uploads' <<<"$dry_output" \
  || fail "dry run omitted the immutable publication boundary"

missing_credentials_log="$(mktemp)"
trap 'rm -f "$missing_credentials_log"' EXIT
if env \
  -u BASETEN_SWITCH_BUILD_NUMBER \
  -u BASETEN_SWITCH_SIGNING_IDENTITY \
  -u BASETEN_SWITCH_TEAM_ID \
  -u BASETEN_SWITCH_NOTARY_PROFILE \
  -u BASETEN_SWITCH_SBOM_GENERATOR \
  BASETEN_SWITCH_RELEASE_TAG=v1.2.3 \
  "$PACKAGER" >"$missing_credentials_log" 2>&1; then
  fail "release build accepted missing credentials"
fi
grep -q 'BASETEN_SWITCH_BUILD_NUMBER' "$missing_credentials_log" \
  || fail "missing release credentials did not produce an actionable error"

if env \
  -u BASETEN_SWITCH_MARKETING_VERSION \
  -u BASETEN_SWITCH_BUILD_NUMBER \
  -u BASETEN_SWITCH_SIGNING_IDENTITY \
  "$REPO_DIR/scripts/build-menubar.sh" --release \
  >"$missing_credentials_log" 2>&1; then
  fail "menubar release build accepted missing signing inputs"
fi
grep -q 'BASETEN_SWITCH_MARKETING_VERSION' "$missing_credentials_log" \
  || fail "menubar release build did not identify its missing version input"

if grep -En -- \
  '--clobber|--overwrite|xattr[[:space:]].*quarantine|cp[[:space:]]+docs/|tar\\.gz|SHA256SUMS' \
  "$PACKAGER" \
  "$SCRIPT_DIR/install.sh"; then
  fail "disallowed release behavior remains"
fi

printf 'PASS: public release script contract\n'
