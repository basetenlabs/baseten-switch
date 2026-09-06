#!/usr/bin/env bash
# Workflow expressions and shell variables below are matched as literal source.
# shellcheck disable=SC2016
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
grep -Fq -- "--patch-output \"\$patch\"" "$WORKFLOW" \
    || fail "workflow does not render the public-tap patch artifact"
grep -Fq "subject-path: \${{ steps.formula.outputs.artifact }}" "$WORKFLOW" \
    || fail "workflow does not attest the final ZIP"
grep -Fq "gh release create \"\$RELEASE_TAG\"" "$WORKFLOW" \
    || fail "workflow does not create a release from the selected tag"
grep -Eq '^[[:space:]]+--prerelease[[:space:]]+\\$' "$WORKFLOW" \
    || fail "release creation is not marked as a prerelease"
grep -Fq 'release already exists for %s; refusing to replace or add assets' "$WORKFLOW" \
    || fail "workflow does not refuse existing releases"
grep -Fq '"dist/baseten-switch_${version}_darwin_universal.zip"' "$WORKFLOW" \
    || fail "release does not upload the universal ZIP"
grep -Fq '"dist/checksums.txt"' "$WORKFLOW" \
    || fail "release does not upload checksums"

if grep -Eiq -- 'APPLE_|NOTARY|NOTARIZ|SBOM|CYCLONEDX|RELEASE_TAG_SIGNING_PUBLIC_KEY_BASE64|gpg --batch|--draft|--clobber|gh release edit|gh release upload|gh release delete|git push|[[:space:]]brew[[:space:]]+tap([[:space:]]|$)|gh pr ' "$WORKFLOW"; then
    fail "workflow contains unsupported signing, asset replacement, or unscoped publication behavior"
fi

ruby - "$WORKFLOW" "$REPO_DIR/.github/workflows/ci.yml" <<'RUBY'
require "yaml"

def check(condition, message)
  abort "FAIL: #{message}" unless condition
end

workflow = YAML.load_file(ARGV.fetch(0))
ci = YAML.load_file(ARGV.fetch(1))
jobs = workflow.fetch("jobs")
prepare = jobs.fetch("prepare")
publisher = jobs.fetch("update-homebrew")
check(Array(publisher["needs"]) == ["prepare"] && !publisher.key?("if"),
      "Homebrew publication must require successful release preparation")
check(publisher.fetch("permissions") == {"contents" => "read", "actions" => "read"},
      "Homebrew job must keep its workflow token read-only")
steps = publisher.fetch("steps")
checkout = steps.find { |step| step.fetch("uses", "").start_with?("actions/checkout@") }
check(checkout && checkout.fetch("with")["ref"] == '${{ needs.prepare.outputs.release_commit }}' &&
      checkout.fetch("with")["persist-credentials"] == false,
      "Homebrew source must be the verified release commit without persisted credentials")
check(prepare.fetch("outputs")["release_commit"] == '${{ steps.release-tag.outputs.commit }}',
      "release source output is not bound to tag verification")
verification = prepare.fetch("steps").find { |step| step["id"] == "release-tag" }
check(verification && verification.fetch("run").include?('git verify-tag "$RELEASE_TAG"') &&
      verification.fetch("run").include?('git rev-parse "HEAD^{commit}"'),
      "published source commit must come from signed tag verification")

download = steps.find { |step| step.fetch("uses", "").start_with?("actions/download-artifact@") }
upload = prepare.fetch("steps").find { |step| step.fetch("uses", "").start_with?("actions/upload-artifact@") }
check(download && upload && download.fetch("with")["name"] == upload.fetch("with")["name"] &&
      download.fetch("with").keys.sort == ["name", "path"],
      "Homebrew formula must come from the matching artifact in this release run")
check(download.fetch("with")["name"] == 'baseten-switch-${{ inputs.release_tag }}-homebrew',
      "Homebrew artifact must match the selected release")
token_steps = steps.select { |step| step.fetch("uses", "").start_with?("actions/create-github-app-token@") }
check(token_steps.length == 1, "Homebrew publication requires one scoped app token")
token = token_steps.first
token_inputs = token.fetch("with")
check(token_inputs == {
  "client-id" => '${{ secrets.APP_HOMEBREW_UPDATER_CLIENT_ID }}',
  "private-key" => '${{ secrets.APP_HOMEBREW_UPDATER_PRIVATE_KEY }}',
  "owner" => "basetenlabs", "repositories" => "homebrew-baseten", "permission-contents" => "write",
}, "Homebrew app token must be limited to writing contents in the tap")
publish_steps = steps.select { |step| step.fetch("run", "").include?("scripts/release/publish-homebrew.sh") }
check(publish_steps.length == 1, "Homebrew job must invoke the canonical publisher once")
publish = publish_steps.first
check(steps.index(download) < steps.index(token) && steps.index(token) < steps.index(publish),
      "Homebrew app token must be created after the formula download and before publication")
check(publish.fetch("env")["GH_TOKEN"] == "${{ steps.#{token.fetch('id')}.outputs.token }}" &&
      !publisher.fetch("env", {}).key?("GH_TOKEN"),
      "tap token must be scoped to the publication step")
check(publish.fetch("env")["RELEASE_TAG"] == '${{ inputs.release_tag }}' &&
      publish.fetch("env")["FORMULA_PATH"] == "#{download.fetch('with').fetch('path')}/homebrew-tap/Formula/baseten-switch.rb",
      "publisher must consume the formula downloaded for the selected release")

%w[test-release-contract.sh test-release-workflow.sh test-render-formula.sh test-publish-homebrew.sh].each do |script|
  command = "scripts/release/#{script}"
  check(prepare.fetch("steps").any? { |step| step.fetch("run", "").lines.any? { |line| line.strip == command } },
        "release gate omitted #{script}")
  check(ci.fetch("jobs").values.any? { |job| job.fetch("steps").any? { |step| step.fetch("run", "").lines.any? { |line| line.strip == command } } },
        "normal CI omitted #{script}")
end
RUBY

printf 'PASS: manual beta release and scoped Homebrew publication workflow contract\n'
