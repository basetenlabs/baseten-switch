#!/usr/bin/env bash
# Render the checksum-pinned Homebrew formula for one immutable release asset.

set -euo pipefail
umask 022

fail() {
    printf 'render-formula: %s\n' "$*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Usage: scripts/release/render-formula.sh \
  --tag v<major>.<minor>.<patch> \
  --sha256 <64 lowercase or uppercase hex characters> \
  --approved-license-spdx <SPDX-ID> \
  --output <Formula/baseten-switch.rb path> \
  --patch-output <patch path>

The license is intentionally mandatory. Pass only the SPDX identifier approved
for the repository. This script does not infer or select a license.
EOF
}

tag=""
sha256=""
approved_license_spdx=""
output=""
patch_output=""

while [[ "$#" -gt 0 ]]; do
    case "$1" in
        --tag)
            [[ "$#" -ge 2 ]] || fail "--tag requires a value"
            [[ -z "$tag" ]] || fail "--tag was provided more than once"
            tag="$2"
            shift 2
            ;;
        --sha256)
            [[ "$#" -ge 2 ]] || fail "--sha256 requires a value"
            [[ -z "$sha256" ]] || fail "--sha256 was provided more than once"
            sha256="$2"
            shift 2
            ;;
        --approved-license-spdx)
            [[ "$#" -ge 2 ]] || fail "--approved-license-spdx requires a value"
            [[ -z "$approved_license_spdx" ]] \
                || fail "--approved-license-spdx was provided more than once"
            approved_license_spdx="$2"
            shift 2
            ;;
        --output)
            [[ "$#" -ge 2 ]] || fail "--output requires a value"
            [[ -z "$output" ]] || fail "--output was provided more than once"
            output="$2"
            shift 2
            ;;
        --patch-output)
            [[ "$#" -ge 2 ]] || fail "--patch-output requires a value"
            [[ -z "$patch_output" ]] || fail "--patch-output was provided more than once"
            patch_output="$2"
            shift 2
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

[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] \
    || fail "--tag must match v<major>.<minor>.<patch>"
[[ "$sha256" =~ ^[[:xdigit:]]{64}$ ]] \
    || fail "--sha256 must be exactly 64 hexadecimal characters"
[[ "$approved_license_spdx" =~ ^[A-Za-z0-9][A-Za-z0-9.+-]*$ ]] \
    || fail "--approved-license-spdx must be one explicit SPDX identifier"
[[ -n "$output" ]] || fail "--output is required"
[[ "$output" != -* ]] || fail "--output may not begin with '-'"
[[ -n "$patch_output" ]] || fail "--patch-output is required"
[[ "$patch_output" != -* ]] || fail "--patch-output may not begin with '-'"
[[ "$patch_output" != "$output" ]] || fail "--output and --patch-output must differ"
[[ ! -e "$output" ]] || fail "refusing to replace existing output: $output"
[[ ! -e "$patch_output" ]] || fail "refusing to replace existing output: $patch_output"

version="${tag#v}"
sha256="$(tr '[:upper:]' '[:lower:]' <<<"$sha256")"
artifact_name="baseten-switch_${version}_darwin_universal.zip"
artifact_url="https://github.com/basetenlabs/baseten-switch/releases/download/${tag}/${artifact_name}"
output_dir="$(dirname "$output")"
patch_output_dir="$(dirname "$patch_output")"
mkdir -p "$output_dir"
mkdir -p "$patch_output_dir"
tmp="$(mktemp "$output_dir/.baseten-switch-formula.XXXXXX")"
patch_tmp="$(mktemp "$patch_output_dir/.baseten-switch-formula-patch.XXXXXX")"
trap 'rm -f "$tmp" "$patch_tmp"' EXIT

cat >"$tmp" <<EOF
class BasetenSwitch < Formula
  desc "Local gateway routing AI coding harnesses between native providers and Baseten"
  homepage "https://github.com/basetenlabs/baseten-switch"
  url "${artifact_url}"
  version "${version}"
  sha256 "${sha256}"
  license "${approved_license_spdx}"

  depends_on "basetenlabs/baseten/baseten"
  depends_on macos: :ventura

  def install
    bin.install "bin/baseten-switch"
    pkgshare.install "Baseten Switch.app.zip"
    pkgshare.install "LICENSE", "THIRD_PARTY_NOTICES.md"
  end

  test do
    assert_match "baseten-switch v#{version}", shell_output("#{bin}/baseten-switch --version")
    assert_path_exists pkgshare/"Baseten Switch.app.zip"
    assert_path_exists pkgshare/"LICENSE"
    assert_path_exists pkgshare/"THIRD_PARTY_NOTICES.md"
  end
end
EOF

if command -v ruby >/dev/null 2>&1; then
    ruby -c "$tmp" >/dev/null \
        || fail "rendered formula failed Ruby syntax validation"
fi

formula_lines="$(wc -l <"$tmp" | tr -d '[:space:]')"
{
    printf 'diff --git a/Formula/baseten-switch.rb b/Formula/baseten-switch.rb\n'
    printf 'new file mode 100644\n'
    printf '%s\n' '--- /dev/null'
    printf '%s\n' '+++ b/Formula/baseten-switch.rb'
    printf '@@ -0,0 +1,%s @@\n' "$formula_lines"
    sed 's/^/+/' "$tmp"
} >"$patch_tmp"

chmod 0644 "$tmp"
chmod 0644 "$patch_tmp"
mv "$tmp" "$output"
mv "$patch_tmp" "$patch_output"
trap - EXIT
printf '%s\n' "$output" "$patch_output"
