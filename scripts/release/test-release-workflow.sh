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
    [[ "$action" =~ ^actions/[a-z0-9-]+@[0-9a-f]{40}$ ]] \
        || fail "action is not allowlisted and pinned to a full commit SHA: $action"
done < <(
    sed -nE 's/^[[:space:]]+uses:[[:space:]]+([^[:space:]#]+).*/\1/p' "$WORKFLOW"
)

grep -Fq "git verify-tag \"\$RELEASE_TAG\"" "$WORKFLOW" \
    || fail "workflow does not verify the selected tag signature"
for verification_value in \
    'RELEASE_TAG_SIGNING_PUBLIC_KEY: ${{ vars.RELEASE_TAG_SIGNING_PUBLIC_KEY }}' \
    'RELEASE_TAG_SIGNING_PRINCIPAL: ${{ vars.RELEASE_TAG_SIGNING_PRINCIPAL }}' \
    'RELEASE_TAG_SIGNING_FINGERPRINT: ${{ vars.RELEASE_TAG_SIGNING_FINGERPRINT }}'; do
    grep -Fq "$verification_value" "$WORKFLOW" \
        || fail "workflow is missing SSH tag verification value: $verification_value"
done
grep -Fq 'ssh-keygen -lf "$signing_key" -E sha256' "$WORKFLOW" \
    || fail "workflow does not verify the approved SSH signing key fingerprint"
grep -Fq 'git config gpg.format ssh' "$WORKFLOW" \
    || fail "workflow does not configure Git to verify SSH signatures"
grep -Fq 'git config gpg.ssh.allowedSignersFile "$allowed_signers"' "$WORKFLOW" \
    || fail "workflow does not constrain SSH signatures to the approved principal"
grep -Fq 'git describe --tags --exact-match HEAD' "$WORKFLOW" \
    || fail "workflow does not require an exact tag checkout"
grep -Fq 'scripts/release/build-artifacts.sh' "$WORKFLOW" \
    || fail "workflow does not invoke the strict artifact builder"
grep -Fq 'BASETEN_SWITCH_RELEASE_SIGNING_MODE: adhoc' "$WORKFLOW" \
    || fail "workflow does not explicitly select ad-hoc beta signing"
grep -Fq 'scripts/release/render-formula.sh' "$WORKFLOW" \
    || fail "workflow does not invoke the canonical formula renderer"
grep -Fq 'scripts/security/run-gitleaks.sh dist' "$WORKFLOW" \
    || fail "workflow does not scan the release archive for secrets"
grep -Fq "subject-path: \${{ steps.formula.outputs.artifact }}" "$WORKFLOW" \
    || fail "workflow does not attest the final ZIP"
grep -Fq "gh release create \"\$RELEASE_TAG\"" "$WORKFLOW" \
    || fail "workflow does not create a release from the selected tag"
grep -Eq '^[[:space:]]+--prerelease[[:space:]]+\\$' "$WORKFLOW" \
    || fail "release creation is not marked as a prerelease"
grep -Fq 'release already exists for %s; refusing to replace or add assets' "$WORKFLOW" \
    || fail "workflow does not refuse existing releases"
grep -Fq 'repos/basetenlabs/baseten-switch/immutable-releases' "$WORKFLOW" \
    || fail "release publication does not require the repository immutable-release setting"
grep -Fq -- "--jq '.enabled == true'" "$WORKFLOW" \
    || fail "release publication does not fail closed on the immutable-release setting"
grep -Fq '"dist/baseten-switch_${version}_darwin_universal.zip"' "$WORKFLOW" \
    || fail "release does not upload the universal ZIP"
grep -Fq '"dist/checksums.txt"' "$WORKFLOW" \
    || fail "release does not upload checksums"
grep -Fqx '    needs: prepare' "$WORKFLOW" \
    || fail "Homebrew publication is not isolated behind the immutable release job"
for homebrew_permission in 'actions: read' 'attestations: read' 'contents: read'; do
    grep -Fqx "      $homebrew_permission" "$WORKFLOW" \
        || fail "Homebrew job is missing permission: $homebrew_permission"
done
grep -Fq 'actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c' "$WORKFLOW" \
    || fail "Homebrew publication does not download the exact formula handoff"
grep -Fq 'actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1' "$WORKFLOW" \
    || fail "Homebrew publication does not use the pinned GitHub App token action"
for tap_token_value in \
    'client-id: ${{ secrets.APP_HOMEBREW_UPDATER_CLIENT_ID }}' \
    'private-key: ${{ secrets.APP_HOMEBREW_UPDATER_PRIVATE_KEY }}' \
    'owner: basetenlabs' \
    'repositories: homebrew-baseten' \
    'permission-contents: write' \
    'permission-pull-requests: write'; do
    grep -Fq "$tap_token_value" "$WORKFLOW" \
        || fail "Homebrew token is not scoped as required: $tap_token_value"
done
grep -Fq '"repos/basetenlabs/baseten-switch/releases/tags/$RELEASE_TAG"' "$WORKFLOW" \
    || fail "Homebrew publication does not inspect the published release"
for release_fact in \
    'release.fetch("tag_name") == tag' \
    'release.fetch("draft") == false' \
    'release.fetch("prerelease") == true' \
    'release.fetch("immutable") == true' \
    'assets.map { |asset| asset.fetch("name") }.sort == expected_names.sort' \
    'digest.match?(/\Asha256:[0-9a-f]{64}\z/)' \
    'gh release download "$RELEASE_TAG"' \
    'cmp -s "$checksums" "$published_release/checksums.txt"' \
    'published_sha256="$(shasum -a 256' \
    '"$asset_digest" == "sha256:$expected_sha256"' \
    'gh attestation verify "$published_release/$artifact_name"' \
    '--signer-workflow basetenlabs/baseten-switch/.github/workflows/release.yml'; do
    grep -Fq -- "$release_fact" "$WORKFLOW" \
        || fail "Homebrew handoff does not verify release fact: $release_fact"
done
for tap_contract in \
    'tap_branch="baseten-switch-${RELEASE_TAG}"' \
    'generated_formula="$handoff/homebrew-tap/$formula_path"' \
    'cmp -s "$generated_formula" "$formula_path"' \
    'brew style "$generated_formula"' \
    'git push origin "HEAD:refs/heads/$tap_branch"' \
    'gh pr create' \
    '--base main' \
    '--head "$tap_branch"'; do
    grep -Fq -- "$tap_contract" "$WORKFLOW" \
        || fail "Homebrew pull request contract is missing: $tap_contract"
done
grep -Fq 'Homebrew tap main already contains Baseten Switch %s' "$WORKFLOW" \
    || fail "Homebrew publication is not idempotent after a merged tap update"
grep -Fq 'existing tap branch %s contains a conflicting formula' "$WORKFLOW" \
    || fail "Homebrew publication does not reject a conflicting deterministic branch"
grep -Fq 'Reusing Homebrew tap pull request %s' "$WORKFLOW" \
    || fail "Homebrew publication does not reuse a matching pull request"
grep -Fq 'refusing tap downgrade or conflicting version' "$WORKFLOW" \
    || fail "Homebrew publication does not prevent an older release from downgrading tap main"
[[ "$(grep -Ec '^[[:space:]]+git push ' "$WORKFLOW")" == 1 ]] \
    || fail "workflow must contain exactly one deterministic-branch push"
if grep -Eq '^[[:space:]]+git push (.*--force|.*refs/heads/main|.*[[:space:]]main([[:space:]]|$))' "$WORKFLOW"; then
    fail "workflow pushes forcefully or directly to tap main"
fi

immutable_setting_line="$(grep -nF 'repos/basetenlabs/baseten-switch/immutable-releases' "$WORKFLOW" | head -1 | cut -d: -f1)"
release_create_line="$(grep -nF 'gh release create "$RELEASE_TAG"' "$WORKFLOW" | head -1 | cut -d: -f1)"
release_verify_line="$(grep -nF 'release.fetch("immutable") == true' "$WORKFLOW" | head -1 | cut -d: -f1)"
tap_token_line="$(grep -nF 'actions/create-github-app-token@' "$WORKFLOW" | head -1 | cut -d: -f1)"
[[ "$immutable_setting_line" -lt "$release_create_line" ]] \
    || fail "immutable-release setting is checked after publication"
[[ "$release_verify_line" -lt "$tap_token_line" ]] \
    || fail "tap token is minted before the immutable release is verified"

if grep -Eiq -- 'APPLE_|NOTARY|NOTARIZ|SBOM|CYCLONEDX|RELEASE_TAG_SIGNING_PUBLIC_KEY_BASE64|gpg --batch|--draft|--clobber|gh release edit|gh release upload|gh release delete|gh pr merge' "$WORKFLOW"; then
    fail "workflow contains replacement, direct-main, auto-merge, or release mutation behavior"
fi

printf 'PASS: pinned manual beta prerelease and protected tap PR workflow contract\n'
