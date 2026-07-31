# Releasing Baseten Switch

Baseten Switch releases are public beta prereleases. The release workflow
builds immutable macOS assets, creates the GitHub prerelease, and opens a pull
request that updates the public Homebrew formula. A human must review and
merge the Homebrew pull request.

## Protected release configuration

The GitHub `release` environment must require maintainer approval and provide
these variables:

- `RELEASE_TAG_SIGNING_PUBLIC_KEY`
- `RELEASE_TAG_SIGNING_PRINCIPAL`
- `RELEASE_TAG_SIGNING_FINGERPRINT`

It must also provide these GitHub App secrets:

- `APP_RELEASE_SETTINGS_READER_CLIENT_ID`
- `APP_RELEASE_SETTINGS_READER_PRIVATE_KEY`
- `APP_HOMEBREW_UPDATER_CLIENT_ID`
- `APP_HOMEBREW_UPDATER_PRIVATE_KEY`

The release-settings reader app must be scoped only to
`basetenlabs/baseten-switch`, with repository Administration permission set to
read. The workflow mints its token immediately before checking the immutable
release setting. It must not share credentials with the Homebrew updater.

The Homebrew updater app must be scoped only to
`basetenlabs/homebrew-baseten`, with permission to write repository contents
and pull requests. The workflow does not mint this token until it has cloned,
inspected, and prepared the public tap update without credentials. Do not give
either app access to other repositories or broader permissions.

Enable [immutable releases][github-immutable-releases] in the
`basetenlabs/baseten-switch` repository settings before dispatching the
workflow. This setting prevents changes to a release's tag and assets after
publication.

## Prepare the release

1. Choose an unused tag that matches `v<major>.<minor>.<patch>`. It must be
   newer than every published Baseten Switch release and the version currently
   on the public tap. Keep the `v` prefix and do not add a prerelease suffix.
   The workflow marks the GitHub release as a beta prerelease.
2. Choose a numeric macOS build number. Each component must contain digits,
   separated by periods. Increase it from every previous public build. The
   workflow rejects other formats.
3. Move the entries for this release from `Unreleased` in `CHANGELOG.md` to a
   dated version section. Add a new empty `Unreleased` section and update the
   comparison links.
4. Merge the changelog and all release content into `main`. Tag the exact
   `origin/main` commit, not a release branch.
5. From a clean checkout of that commit, run the full release gate:

   ```sh
   scripts/check.sh
   ```

   The full gate uses the maintainer's existing Baseten CLI credential store.
   Never supply credentials from an untrusted pull request.

## Sign and push the tag

Configure Git to use the approved SSH signing key. Confirm the configuration
before creating the tag:

```sh
git config --get gpg.format
git config --get user.signingkey
git fetch origin main --tags
test -z "$(git status --porcelain)"
test "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)"
```

The first command must print `ssh`. Create an annotated, SSH-signed tag whose
message is suitable for the GitHub release notes. The command opens an editor;
use the title `Baseten Switch vX.Y.Z beta`, followed by a concise summary from
`CHANGELOG.md`.

```sh
release_tag=vX.Y.Z
git tag -s "$release_tag"
git verify-tag "$release_tag"
git push origin "refs/tags/$release_tag"
```

Confirm that the pushed tag resolves to the reviewed `main` commit. Never move
or replace a pushed release tag.

## Publish the GitHub prerelease

Manually dispatch `.github/workflows/release.yml` from `main`. Supply the
signed tag, the macOS build number, and the approved repository license:

```sh
gh workflow run release.yml \
  --repo basetenlabs/baseten-switch \
  --ref main \
  -f release_tag=vX.Y.Z \
  -f build_number=N \
  -f approved_license_spdx=MIT
```

Approve the protected `release` environment when prompted. The release job
verifies that repository immutable releases are enabled, verifies the signed
tag is the current `origin/main` commit, and confirms the candidate version is
unused and newer than every published release and the current tap formula. It
then runs `scripts/check.sh --offline`, checks the release contracts, builds
and scans the universal archive, validates the generated formula, creates an
attestation, and publishes a GitHub beta prerelease. It refuses to replace an
existing release or its assets.

After the job succeeds, verify all of the following on GitHub:

- the release points to the intended signed tag and reviewed commit;
- the release is a prerelease, not a draft;
- the title and tag annotation describe the release accurately;
- the release contains only
  `baseten-switch_<version>_darwin_universal.zip` and `checksums.txt`;
- the checksum file contains the published archive's SHA-256 digest;
- the archive has a successful GitHub artifact attestation.

## Review and merge the Homebrew pull request

After the release job succeeds, the separate `homebrew` job downloads and
verifies the handoff, clones the public tap without credentials, treats the
current tap formula as inert text, and prepares the exact local commit. Only
then does it create a short-lived GitHub App token scoped to the public tap.
The credentialed step pushes the prepared branch
`baseten-switch-<release_tag>` and creates or reuses its pull request against
`main`. For example, release `v0.2.0` uses branch
`baseten-switch-v0.2.0`.

The workflow never pushes directly to the tap's `main` branch and never merges
the pull request. Its formula validation must confirm that:

- the GitHub release has the selected tag and immutable prerelease metadata;
- the release contains exactly the universal ZIP and `checksums.txt`;
- the ZIP's SHA-256 value matches the unique checksum entry, GitHub asset
  digest, and release-job handoff;
- the formula's derived version, release URL, and SHA-256 value match the
  published release;
- GitHub verifies the ZIP's artifact attestation;
- the formula passes Ruby syntax and Homebrew style checks;
- the candidate does not downgrade a newer formula on the tap's `main` branch.

A human reviewer must confirm that the formula changes no unrelated
installation or lifecycle behavior, and that the tap's required review and
repository rules pass.

Before merging, derive the previous tag from the current tap formula and
rehearse the upgrade on a disposable release test Mac. The pull request must
still be unmerged when deriving `previous_tag`:

```sh
release_tag=vX.Y.Z
brew update
brew tap basetenlabs/baseten
tap_repo="$(brew --repo basetenlabs/baseten)"
tap_formula="$tap_repo/Formula/baseten-switch.rb"
previous_tag="$(
  ruby -e '
    text = File.read(ARGV.fetch(0))
    pattern = %r{^\s*url "https://github\.com/basetenlabs/baseten-switch/releases/download/(v([0-9]+\.[0-9]+\.[0-9]+))/baseten-switch_([0-9]+\.[0-9]+\.[0-9]+)_darwin_universal\.zip"$}
    matches = text.scan(pattern)
    abort "expected one canonical Baseten Switch release URL" unless matches.length == 1
    tag, tag_version, archive_version = matches.fetch(0)
    abort "formula URL versions do not match" unless tag_version == archive_version
    puts tag
  ' "$tap_formula"
)"
brew install basetenlabs/baseten/baseten-switch
test "$(baseten-switch --version)" = "baseten-switch $previous_tag"
baseten-switch setup
baseten-switch up --install
baseten-switch claude on
baseten-switch doctor

git -C "$tap_repo" fetch origin "baseten-switch-$release_tag"
git -C "$tap_repo" switch --detach FETCH_HEAD
HOMEBREW_NO_AUTO_UPDATE=1 brew upgrade basetenlabs/baseten/baseten-switch
test "$(baseten-switch --version)" = "baseten-switch $release_tag"
baseten-switch up
baseten-switch claude on
baseten-switch doctor

git -C "$tap_repo" switch main
```

Confirm that the upgrade preserves configuration and state, moves the managed
processes and app to the new version, and applies the current Claude Code
integration settings. Restart Claude Code and verify a new session when the
release changes that integration. Merge the pull request only after the review
and upgrade rehearsal pass.

## Verify Homebrew after merge

Run the final checks on a release test Mac, not a machine that must preserve a
running gateway session:

```sh
brew update
brew fetch --force --formula basetenlabs/baseten/baseten-switch
brew reinstall basetenlabs/baseten/baseten-switch
brew test basetenlabs/baseten/baseten-switch
brew style basetenlabs/baseten/baseten-switch
brew audit --strict --online basetenlabs/baseten/baseten-switch
baseten-switch --version
```

Confirm that `baseten-switch --version` prints the new tag. Complete a fresh
setup smoke test using the public instructions in `README.md` when the release
changes installation, startup, authentication, or harness integration.

Existing users upgrade with:

```sh
brew upgrade baseten-switch
baseten-switch up
baseten-switch doctor
```

Users who already route Claude Code through Switch should also run
`baseten-switch claude on` before `doctor` to adopt the current managed
integration settings, then restart Claude Code. Other users should omit that
step. Homebrew preserves Switch configuration and state; `baseten-switch up`
moves the managed processes and app to the newly installed version.

## Recover from a failed release

Preserve published tags, releases, assets, and checksums.

- If the release job fails before publication because of a transient service
  or protected-configuration problem, correct the problem and rerun it with
  the same tag.
- `gh release create` can leave an unpublished draft if asset upload or final
  publication fails. Before retrying, list releases through the GitHub API and
  inspect the matching entry's `draft`, `published_at`, and assets. A
  maintainer may delete it only after confirming it is a never-published draft
  for this failed attempt. Never delete or mutate a published release.

  ```sh
  gh api --paginate repos/basetenlabs/baseten-switch/releases \
    --jq '.[] | {id, tag_name, draft, published_at, assets: [.assets[].name]}'

  release_tag=vX.Y.Z
  draft_id=N
  draft_url="repos/basetenlabs/baseten-switch/releases/$draft_id"
  test "$(gh api "$draft_url" --jq .tag_name)" = "$release_tag"
  test "$(gh api "$draft_url" --jq '.draft and (.published_at == null)')" = true
  gh api --method DELETE "$draft_url"
  ```

- If tagged source or release content must change, merge the correction and
  prepare a new version with a new signed tag. Never move the old tag.
- If the GitHub prerelease succeeds but the `homebrew` job fails, correct the
  tap permission or transient failure and rerun only the failed `homebrew`
  job. Its deterministic branch prevents duplicate release pull requests.
- If a published artifact or generated formula is wrong, leave the release
  intact, prepare a corrected version, and publish a new release.

Do not delete or edit a public release to reuse its version. Do not replace or
add assets after publication.

[github-immutable-releases]: https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/establish-provenance-and-integrity/prevent-release-changes
