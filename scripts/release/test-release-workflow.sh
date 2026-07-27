#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORKFLOW="$REPO_DIR/.github/workflows/release.yml"

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

ruby -e 'require "yaml"; YAML.parse_file(ARGV.fetch(0))' "$WORKFLOW" \
    || fail "release workflow is not valid YAML"

grep -Fqx '  workflow_dispatch:' "$WORKFLOW" \
    || fail "release workflow is not manually dispatched"
if grep -Eq '^  (push|pull_request|release|schedule):' "$WORKFLOW"; then
    fail "release workflow has an automatic trigger"
fi
grep -Fqx 'permissions: read-all' "$WORKFLOW" \
    || fail "workflow does not default to read-only permissions"
grep -Fqx '    environment: release' "$WORKFLOW" \
    || fail "release job does not require the protected release environment"
for permission in 'contents: write' 'id-token: write' 'attestations: write'; do
    grep -Fqx "      $permission" "$WORKFLOW" \
        || fail "release job is missing permission: $permission"
done

while IFS= read -r action; do
    [[ "$action" =~ ^(actions/[a-z0-9-]+|anchore/scan-action)@[0-9a-f]{40}$ ]] \
        || fail "action is not allowlisted and pinned to a full commit SHA: $action"
done < <(
    sed -nE 's/^[[:space:]]+uses:[[:space:]]+([^[:space:]#]+).*/\1/p' "$WORKFLOW"
)

grep -Fq "git verify-tag \"\$RELEASE_TAG\"" "$WORKFLOW" \
    || fail "workflow does not verify the selected tag signature"
grep -Fq 'git describe --tags --exact-match HEAD' "$WORKFLOW" \
    || fail "workflow does not require an exact tag checkout"
grep -Fq 'scripts/release/build-artifacts.sh' "$WORKFLOW" \
    || fail "workflow does not invoke the strict artifact builder"
grep -Fq 'scripts/release/render-formula.sh' "$WORKFLOW" \
    || fail "workflow does not invoke the canonical formula renderer"
grep -Fq 'scripts/security/run-gitleaks.sh dist' "$WORKFLOW" \
    || fail "workflow does not scan the release archive for secrets"
grep -Fq 'anchore/scan-action@e1165082ffb1fe366ebaf02d8526e7c4989ea9d2' "$WORKFLOW" \
    || fail "workflow does not use the approved pinned artifact vulnerability scanner"
grep -Fq -- "--patch-output \"\$patch\"" "$WORKFLOW" \
    || fail "workflow does not render the public-tap patch artifact"
grep -Fq "subject-path: \${{ steps.formula.outputs.artifact }}" "$WORKFLOW" \
    || fail "workflow does not attest the final ZIP"
grep -Fq "gh release create \"\$RELEASE_TAG\"" "$WORKFLOW" \
    || fail "workflow does not create a release from the selected tag"
grep -Eq '^[[:space:]]+--draft[[:space:]]+\\$' "$WORKFLOW" \
    || fail "release creation is not draft-only"
grep -Fq 'release already exists for %s; refusing to replace or add assets' "$WORKFLOW" \
    || fail "workflow does not refuse existing releases"

if grep -Eq -- '--clobber|gh release edit|gh release upload|gh release delete|git push|brew tap' "$WORKFLOW"; then
    fail "workflow contains replacement, publication, tap mutation, or release mutation behavior"
fi

printf 'PASS: pinned manual release workflow contract\n'
