#!/usr/bin/env bash
# Run a checksum-pinned Gitleaks binary without adding a repository dependency.

set -euo pipefail

fail() {
    printf 'run-gitleaks: %s\n' "$*" >&2
    exit 1
}

target="${1:-.}"
[[ -e "$target" ]] || fail "scan target does not exist: $target"

version="8.30.1"
os="$(uname -s)"
arch="$(uname -m)"
case "$os:$arch" in
    Darwin:arm64)
        platform="darwin_arm64"
        expected_sha256="b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5"
        ;;
    Darwin:x86_64)
        platform="darwin_x64"
        expected_sha256="dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709"
        ;;
    Linux:x86_64)
        platform="linux_x64"
        expected_sha256="551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb"
        ;;
    *)
        fail "unsupported platform: $os $arch"
        ;;
esac
[[ "$expected_sha256" =~ ^[0-9a-f]{64}$ ]] \
    || fail "invalid pinned SHA-256 for $platform"

tool_root="$(mktemp -d "${TMPDIR:-/tmp}/baseten-switch-gitleaks.XXXXXX")"
cleanup() {
    if [[ -n "${tool_root:-}" &&
          -d "$tool_root" &&
          "$(basename "$tool_root")" == baseten-switch-gitleaks.* ]]; then
        rm -rf -- "$tool_root"
    fi
}
trap cleanup EXIT

asset="gitleaks_${version}_${platform}.tar.gz"
archive="$tool_root/$asset"
url="https://github.com/gitleaks/gitleaks/releases/download/v${version}/${asset}"
curl --fail --silent --show-error --location \
    --proto '=https' --tlsv1.2 \
    --output "$archive" \
    "$url"

actual_sha256="$(shasum -a 256 "$archive" | awk '{print $1}')"
[[ "$actual_sha256" == "$expected_sha256" ]] \
    || fail "checksum mismatch for $asset"

tar -xzf "$archive" -C "$tool_root" gitleaks
[[ -x "$tool_root/gitleaks" ]] || fail "archive did not contain gitleaks"

repo_root="$(git rev-parse --show-toplevel)"
scan_target="$target"
if [[ "$target" == "." || "$target" == "$repo_root" ]]; then
    # Scan exactly the tracked and non-ignored candidate source files. Local
    # ignored credentials such as .env are not part of a checkout or export.
    # If one is ever tracked, git ls-files includes it and Gitleaks rejects it.
    scan_target="$tool_root/source"
    mkdir -p "$scan_target"
    while IFS= read -r -d '' path; do
        # A tracked path deleted in the candidate working tree is intentionally
        # absent from the source snapshot.
        if [[ ! -e "$repo_root/$path" && ! -L "$repo_root/$path" ]]; then
            continue
        fi
        mkdir -p "$scan_target/$(dirname "$path")"
        cp -p -- "$repo_root/$path" "$scan_target/$path"
    done < <(
        git -C "$repo_root" ls-files \
            --cached \
            --others \
            --exclude-standard \
            -z
    )
fi

"$tool_root/gitleaks" dir \
    --config "$repo_root/.gitleaks.toml" \
    --redact=100 \
    --no-banner \
    --no-color \
    --exit-code=1 \
    --max-archive-depth="${GITLEAKS_MAX_ARCHIVE_DEPTH:-0}" \
    "$scan_target"
