#!/usr/bin/env bash
# Build the immutable public macOS release assets.
#
# Outputs:
#   dist/baseten-switch_<version>_darwin_universal.zip
#   dist/checksums.txt
#
# The outer ZIP contains:
#   bin/baseten-switch
#   Baseten Switch.app.zip
#   install.sh
#   LICENSE
#   THIRD_PARTY_NOTICES.md
#   README.md
#
# Beta release builds fail closed. They require an explicit ad-hoc signing
# mode and numeric plist versions. The output is ad-hoc signed, not
# notarized, and is intended for the public beta. This script never uploads
# or replaces published assets.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

log()  { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: scripts/release/build-artifacts.sh [--dry-run]

Required for a release build:
  BASETEN_SWITCH_RELEASE_TAG          Exact tag, for example v0.2.0
  BASETEN_SWITCH_BUILD_NUMBER         Period-separated integers, for example 42
  BASETEN_SWITCH_RELEASE_SIGNING_MODE Must be "adhoc"

The script writes checksums.txt for the final ZIP. Beta artifacts are
ad-hoc signed and are not Apple-notarized.
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
dist_dir="$REPO_DIR/dist"
artifact="$dist_dir/$artifact_name"
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
  printf 'checksums:        %s (final ZIP)\n' "$checksums"
  printf 'signing:          explicit ad-hoc beta signatures\n'
  printf 'notarization:     none (public beta)\n'
  printf 'publication:      separate immutable release step; this script never uploads\n'
  exit 0
fi

[[ "$build_number" =~ ^[0-9]+(\.[0-9]+)*$ ]] \
  || fail "BASETEN_SWITCH_BUILD_NUMBER must contain period-separated integers"

release_signing_mode="${BASETEN_SWITCH_RELEASE_SIGNING_MODE:-}"
[[ "$release_signing_mode" == "adhoc" ]] \
  || fail "BASETEN_SWITCH_RELEASE_SIGNING_MODE must be 'adhoc' for beta releases"
[[ "$(uname -s)" == "Darwin" ]] || fail "release builds require macOS"

for command in go lipo codesign plutil ditto zip unzip shasum; do
  command -v "$command" >/dev/null 2>&1 || fail "required command not found: $command"
done
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

for output in "$artifact" "$checksums"; do
  [[ ! -e "$output" ]] \
    || fail "refusing to replace existing release output: ${output#"$REPO_DIR/"}"
done

verify_adhoc_signature() {
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
  grep -qxF 'Signature=adhoc' <<<"$signature_info" \
    || fail "required ad-hoc beta signature is missing: $target"
}

stage_root="$(mktemp -d)"
stage="$stage_root/archive"
trap 'rm -rf "$stage_root"' EXIT
mkdir -p "$stage/bin" "$dist_dir"

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
codesign --force --sign - "$stage/bin/baseten-switch"
verify_adhoc_signature "$stage/bin/baseten-switch"
[[ "$("$stage/bin/baseten-switch" --version)" == "baseten-switch $release_tag" ]] \
  || fail "CLI version does not match $release_tag"

log "building ad-hoc signed beta menubar app"
BASETEN_SWITCH_MARKETING_VERSION="$marketing_version" \
BASETEN_SWITCH_BUILD_NUMBER="$build_number" \
BASETEN_SWITCH_RELEASE_SIGNING_MODE="$release_signing_mode" \
  "$REPO_DIR/scripts/build-menubar.sh" --variant stable --release
app="$REPO_DIR/mac/BasetenSwitch/dist/Baseten Switch.app"
app_plist="$app/Contents/Info.plist"
[[ -d "$app" ]] || fail "menubar build did not produce $app"
[[ "$(plutil -extract CFBundleShortVersionString raw "$app_plist")" == "$marketing_version" ]] \
  || fail "app marketing version does not match $marketing_version"
[[ "$(plutil -extract CFBundleVersion raw "$app_plist")" == "$build_number" ]] \
  || fail "app build number does not match $build_number"
verify_adhoc_signature "$app" 1
verify_adhoc_signature "$app/Contents/MacOS/BasetenSwitch"

for binary in "$stage/bin/baseten-switch" "$app/Contents/MacOS/BasetenSwitch"; do
  archs="$(lipo -archs "$binary")"
  for wanted_arch in arm64 x86_64; do
    case " $archs " in
      *" $wanted_arch "*) ;;
      *) fail "$binary is missing the $wanted_arch slice (found: $archs)" ;;
    esac
  done
done

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

log "writing checksums.txt"
(
  cd "$dist_dir"
  shasum -a 256 "$artifact_name" >checksums.txt
)

log "release assets complete"
printf '%s\n' "$artifact" "$checksums"
