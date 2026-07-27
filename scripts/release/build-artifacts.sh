#!/usr/bin/env bash
# Build the immutable public macOS release assets.
#
# Outputs:
#   dist/baseten-switch_<version>_darwin_universal.zip
#   dist/baseten-switch_<version>.cdx.json
#   dist/checksums.txt
#   dist/notarization-<version>-{payload,artifact}.json
#
# The outer ZIP contains:
#   bin/baseten-switch
#   Baseten Switch.app.zip
#   install.sh
#   LICENSE
#   THIRD_PARTY_NOTICES.md
#   README.md
#
# Release builds fail closed. They require a Developer ID Application
# identity, an expected Team ID, a notarytool keychain profile, numeric
# plist versions, and an executable CycloneDX SBOM generator hook.
# This script never uploads or replaces published assets.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

log()  { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: scripts/release/build-artifacts.sh [--dry-run]

Required for a release build:
  BASETEN_SWITCH_RELEASE_TAG       Signed tag, for example v0.2.0
  BASETEN_SWITCH_BUILD_NUMBER      Period-separated integers, for example 42
  BASETEN_SWITCH_SIGNING_IDENTITY  Developer ID Application identity
  BASETEN_SWITCH_TEAM_ID           Apple Developer Team ID
  BASETEN_SWITCH_NOTARY_PROFILE    notarytool keychain profile
  BASETEN_SWITCH_SBOM_GENERATOR    Executable CycloneDX generator hook

SBOM hook contract:
  "$BASETEN_SWITCH_SBOM_GENERATOR" <final-outer-zip> <output-cdx-json>

The hook must write a nonempty CycloneDX JSON file to the second path.
The script writes checksums.txt for the final ZIP and SBOM after both
notarization submissions succeed.
EOF
}

dry_run=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)
      dry_run=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

release_tag="${BASETEN_SWITCH_RELEASE_TAG:-}"
if [[ "$dry_run" == 1 && -z "$release_tag" ]]; then
  release_tag="v0.0.0"
fi
[[ "$release_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] \
  || fail "BASETEN_SWITCH_RELEASE_TAG must match v<major>.<minor>.<patch>"
marketing_version="${release_tag#v}"
build_number="${BASETEN_SWITCH_BUILD_NUMBER:-}"
artifact_name="baseten-switch_${marketing_version}_darwin_universal.zip"
sbom_name="baseten-switch_${marketing_version}.cdx.json"
dist_dir="$REPO_DIR/dist"
artifact="$dist_dir/$artifact_name"
sbom="$dist_dir/$sbom_name"
checksums="$dist_dir/checksums.txt"

if [[ "$dry_run" == 1 ]]; then
  log "public release plan (dry run, no files changed)"
  printf 'tag:             %s\n' "$release_tag"
  printf 'marketing:       %s\n' "$marketing_version"
  printf 'build number:    %s\n' "${build_number:-<required for build>}"
  printf 'artifact:         %s\n' "$artifact"
  printf 'archive entries:  bin/baseten-switch\n'
  printf '                  Baseten Switch.app.zip\n'
  printf '                  install.sh, LICENSE, THIRD_PARTY_NOTICES.md, README.md\n'
  printf 'sbom:             %s\n' "$sbom"
  printf 'checksums:        %s (final ZIP and SBOM)\n' "$checksums"
  printf 'signing:          Developer ID Application, hardened runtime, secure timestamp\n'
  printf 'notarization:     payload ZIP, stapled app, then exact final outer ZIP\n'
  printf 'publication:      separate immutable release step; this script never uploads\n'
  exit 0
fi

[[ "$build_number" =~ ^[0-9]+(\.[0-9]+)*$ ]] \
  || fail "BASETEN_SWITCH_BUILD_NUMBER must contain period-separated integers"

signing_identity="${BASETEN_SWITCH_SIGNING_IDENTITY:-}"
team_id="${BASETEN_SWITCH_TEAM_ID:-}"
notary_profile="${BASETEN_SWITCH_NOTARY_PROFILE:-}"
sbom_generator="${BASETEN_SWITCH_SBOM_GENERATOR:-}"

[[ "$signing_identity" == "Developer ID Application:"* ]] \
  || fail "BASETEN_SWITCH_SIGNING_IDENTITY must name a Developer ID Application certificate"
[[ "$team_id" =~ ^[A-Z0-9]{10}$ ]] \
  || fail "BASETEN_SWITCH_TEAM_ID must be a 10-character Apple Developer Team ID"
[[ -n "$notary_profile" ]] \
  || fail "BASETEN_SWITCH_NOTARY_PROFILE is required; create it with 'xcrun notarytool store-credentials'"
[[ -x "$sbom_generator" ]] \
  || fail "BASETEN_SWITCH_SBOM_GENERATOR must point to an executable CycloneDX generator"
[[ "$(uname -s)" == "Darwin" ]] || fail "release builds require macOS"

for command in go lipo codesign plutil ditto zip unzip shasum xcrun spctl security; do
  command -v "$command" >/dev/null 2>&1 || fail "required command not found: $command"
done
signing_identities="$(security find-identity -v -p codesigning)"
grep -Fq "$signing_identity" <<<"$signing_identities" \
  || fail "Developer ID signing identity is not available in the active keychain: $signing_identity"
xcrun notarytool history \
  --keychain-profile "$notary_profile" \
  --output-format json >/dev/null \
  || fail "notarytool credentials are unavailable or invalid for profile: $notary_profile"
for required_file in \
  "$REPO_DIR/LICENSE" \
  "$REPO_DIR/THIRD_PARTY_NOTICES.md" \
  "$REPO_DIR/README.md" \
  "$REPO_DIR/scripts/release/install.sh"; do
  [[ -f "$required_file" ]] || fail "required release file is missing: ${required_file#"$REPO_DIR/"}"
done

exact_tag="$(git -C "$REPO_DIR" describe --tags --exact-match HEAD 2>/dev/null || true)"
[[ "$exact_tag" == "$release_tag" ]] \
  || fail "HEAD must be the exact ${release_tag} tag (found '${exact_tag:-no exact tag}')"
[[ -z "$(git -C "$REPO_DIR" status --porcelain --untracked-files=no)" ]] \
  || fail "release builds require a clean tracked worktree"

for output in "$artifact" "$sbom" "$checksums"; do
  [[ ! -e "$output" ]] \
    || fail "refusing to replace existing release output: ${output#"$REPO_DIR/"}"
done

verify_developer_id() {
  local target="$1"
  local deep="${2:-0}"
  local signature_info
  local verify_args=(--verify --strict --verbose=2)
  if [[ "$deep" == 1 ]]; then
    verify_args+=(--deep)
  fi
  codesign "${verify_args[@]}" "$target" \
    || fail "code-signature verification failed: $target"
  signature_info="$(codesign --display --verbose=4 "$target" 2>&1)"
  grep -q '^Authority=Developer ID Application:' <<<"$signature_info" \
    || fail "Developer ID Application authority missing: $target"
  grep -qxF "TeamIdentifier=$team_id" <<<"$signature_info" \
    || fail "TeamIdentifier is not $team_id: $target"
}

notarize() {
  local input="$1"
  local log_path="$2"
  local submission_summary
  local submission_id
  local status
  submission_summary="$(mktemp "$stage_root/notary-submission.XXXXXX")"
  xcrun notarytool submit "$input" \
    --keychain-profile "$notary_profile" \
    --wait \
    --output-format json >"$submission_summary" \
    || fail "notarization submission failed: $input (log: $log_path)"
  status="$(plutil -extract status raw -o - "$submission_summary" 2>/dev/null || true)"
  submission_id="$(plutil -extract id raw -o - "$submission_summary" 2>/dev/null || true)"
  [[ "$status" == "Accepted" ]] \
    || fail "notarization was not accepted for $input (status: ${status:-unknown}; submission: $submission_summary)"
  [[ -n "$submission_id" ]] \
    || fail "notarytool returned no submission ID for $input"
  xcrun notarytool log "$submission_id" \
    --keychain-profile "$notary_profile" \
    "$log_path" >/dev/null \
    || fail "could not retrieve notarization log for $input"
  [[ -s "$log_path" ]] \
    || fail "notarytool produced an empty notarization log for $input"
}

stage_root="$(mktemp -d)"
stage="$stage_root/archive"
payload_stage="$stage_root/notary-payload"
trap 'rm -rf "$stage_root"' EXIT
mkdir -p "$stage/bin" "$payload_stage/bin" "$dist_dir"

log "building universal CLI (${release_tag})"
for goarch in arm64 amd64; do
  (
    cd "$REPO_DIR/gateway"
    env GOOS=darwin GOARCH="$goarch" CGO_ENABLED=0 \
      go build -trimpath \
        -ldflags "-s -w -X github.com/basetenlabs/baseten-switch/gateway/internal/version.Version=${release_tag}" \
        -o "$stage_root/baseten-switch-$goarch" ./cmd/baseten-switch
  )
done
lipo -create -output "$stage/bin/baseten-switch" \
  "$stage_root/baseten-switch-arm64" \
  "$stage_root/baseten-switch-amd64"
chmod 0755 "$stage/bin/baseten-switch"
codesign --force --options runtime --timestamp \
  --sign "$signing_identity" "$stage/bin/baseten-switch"
verify_developer_id "$stage/bin/baseten-switch"
[[ "$("$stage/bin/baseten-switch" --version)" == "baseten-switch $release_tag" ]] \
  || fail "CLI version does not match $release_tag"

log "building Developer ID menubar app"
BASETEN_SWITCH_MARKETING_VERSION="$marketing_version" \
BASETEN_SWITCH_BUILD_NUMBER="$build_number" \
BASETEN_SWITCH_SIGNING_IDENTITY="$signing_identity" \
  "$REPO_DIR/scripts/build-menubar.sh" --variant stable --release
app="$REPO_DIR/mac/BasetenSwitch/dist/Baseten Switch.app"
app_plist="$app/Contents/Info.plist"
[[ -d "$app" ]] || fail "menubar build did not produce $app"
[[ "$(plutil -extract CFBundleShortVersionString raw "$app_plist")" == "$marketing_version" ]] \
  || fail "app marketing version does not match $marketing_version"
[[ "$(plutil -extract CFBundleVersion raw "$app_plist")" == "$build_number" ]] \
  || fail "app build number does not match $build_number"
verify_developer_id "$app" 1
verify_developer_id "$app/Contents/MacOS/BasetenSwitch"

for binary in "$stage/bin/baseten-switch" "$app/Contents/MacOS/BasetenSwitch"; do
  archs="$(lipo -archs "$binary")"
  for wanted_arch in arm64 x86_64; do
    case " $archs " in
      *" $wanted_arch "*) ;;
      *) fail "$binary is missing the $wanted_arch slice (found: $archs)" ;;
    esac
  done
done

log "notarizing signed payload"
ditto "$stage/bin/baseten-switch" "$payload_stage/bin/baseten-switch"
ditto "$app" "$payload_stage/Baseten Switch.app"
payload_zip="$stage_root/notary-payload.zip"
(
  cd "$payload_stage"
  zip -qry "$payload_zip" .
)
payload_log="$dist_dir/notarization-${marketing_version}-payload.json"
[[ ! -e "$payload_log" ]] || fail "refusing to replace existing notarization log: $payload_log"
notarize "$payload_zip" "$payload_log"

log "stapling and validating app"
xcrun stapler staple "$app" || fail "failed to staple the menubar app"
xcrun stapler validate "$app" || fail "stapled ticket validation failed"
verify_developer_id "$app" 1
spctl --assess --type execute --verbose=4 "$app" \
  || fail "Gatekeeper rejected the menubar app"
spctl --assess --type execute --verbose=4 "$stage/bin/baseten-switch" \
  || fail "Gatekeeper rejected the CLI"

log "assembling $artifact_name"
ditto -c -k --keepParent "$app" "$stage/Baseten Switch.app.zip"
install -m 0755 "$REPO_DIR/scripts/release/install.sh" "$stage/install.sh"
install -m 0644 "$REPO_DIR/LICENSE" "$stage/LICENSE"
install -m 0644 "$REPO_DIR/THIRD_PARTY_NOTICES.md" "$stage/THIRD_PARTY_NOTICES.md"
install -m 0644 "$REPO_DIR/README.md" "$stage/README.md"
(
  cd "$stage"
  zip -qry "$artifact" .
)

archive_entries="$(unzip -Z1 "$artifact")"
for required_entry in \
  "bin/baseten-switch" \
  "Baseten Switch.app.zip" \
  "install.sh" \
  "LICENSE" \
  "THIRD_PARTY_NOTICES.md" \
  "README.md"; do
  grep -qxF "$required_entry" <<<"$archive_entries" \
    || fail "outer ZIP is missing $required_entry"
done
if grep -Eq '(^|/)docs/' <<<"$archive_entries"; then
  fail "outer ZIP must not bundle the docs directory"
fi

log "notarizing exact final outer ZIP"
artifact_log="$dist_dir/notarization-${marketing_version}-artifact.json"
[[ ! -e "$artifact_log" ]] || fail "refusing to replace existing notarization log: $artifact_log"
notarize "$artifact" "$artifact_log"

log "generating CycloneDX SBOM"
"$sbom_generator" "$artifact" "$sbom" \
  || fail "SBOM generator failed"
[[ -s "$sbom" ]] || fail "SBOM generator did not write $sbom"
grep -Eq '"bomFormat"[[:space:]]*:[[:space:]]*"CycloneDX"' "$sbom" \
  || fail "SBOM is not a CycloneDX JSON document"

log "writing checksums.txt"
(
  cd "$dist_dir"
  shasum -a 256 "$artifact_name" "$sbom_name" >checksums.txt
)

log "release assets complete"
printf '%s\n' "$artifact" "$sbom" "$checksums" "$payload_log" "$artifact_log"
