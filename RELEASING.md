# Releasing Baseten Switch

The [release workflow](.github/workflows/release.yml) publishes a beta release
and then updates `Formula/baseten-switch.rb` directly on `main` in
[`basetenlabs/homebrew-baseten`](https://github.com/basetenlabs/homebrew-baseten).
It does not require a separate tap pull request.

## Configuration

Configure these Actions variables for signed-tag verification:

- `RELEASE_TAG_SIGNING_PUBLIC_KEY`
- `RELEASE_TAG_SIGNING_PRINCIPAL`
- `RELEASE_TAG_SIGNING_FINGERPRINT`

Provide these Actions secrets to the `update-homebrew` job through repository
or organization secrets:

- `APP_HOMEBREW_UPDATER_CLIENT_ID`: the updater GitHub App's client ID.
- `APP_HOMEBREW_UPDATER_PRIVATE_KEY`: that App's private key.

The App installation must have `contents: write` access to
`basetenlabs/homebrew-baseten` and permission to update its `main` branch
directly under the tap's rules. The workflow requests a token limited to that
repository and permission. A declared App permission or a configured secret
does not establish that the installation has accepted permission changes or
can bypass a pull request rule. Confirm those settings before a release.
See GitHub's [App token action](https://github.com/actions/create-github-app-token#usage).

The `prepare` job uses the `release` environment and its configured protection
rules. The dependent updater adds no second environment approval. Source
release publication uses `GITHUB_TOKEN`; tap updates use the App token.

## Publish

1. Run `scripts/check.sh` and the [release contracts](CONTRIBUTING.md)
   against the release candidate. Review the final source and release notes.
2. Create and push an annotated, SSH-signed `v<major>.<minor>.<patch>` tag using
   the configured signing identity.
3. Run **Publish beta release** in GitHub Actions. Supply the exact tag, a
   period-separated numeric macOS build number, and the approved license SPDX
   identifier. Complete the `release` environment approval when required.
4. Verify that both `prepare` and `update-homebrew` succeed.

Pushing a tag does not start this workflow. The manually dispatched `prepare`
job verifies the tag, runs the release checks, builds and scans the universal
macOS archive, renders the formula, attests the ZIP, and publishes the GitHub
prerelease. Only then does `update-homebrew` publish the formula preserved by
that same run.

The GitHub release can be available before the tap update finishes. Publication
is complete when the updater verifies the intended formula on tap `main`.
Publishing does not upgrade installed clients or restart their gateways.
Users follow the [upgrade instructions](README.md#upgrade) when ready.

## Recover a failed tap update

If `prepare` succeeded and `update-homebrew` failed, resolve the reported App
access or tap conflict, then choose **Re-run failed jobs** on the original run:

```sh
gh run rerun RUN_ID --failed --repo basetenlabs/baseten-switch
```

This retries the updater without rebuilding or recreating the published
release. GitHub supports [rerunning failed jobs](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs#re-running-failed-jobs)
for 30 days after the original run; the formula artifact has the same retention
period. Keep release tags and assets immutable. The publication job refuses to
replace an existing release, so rerunning every job is not the recovery path.

The updater handles concurrent formula changes with the file's GitHub SHA.
It makes at most three write attempts, reading and validating the current
formula again before retrying a conflict. After any failed write, it checks
whether the intended formula was published despite the error. Other failures
stop the job.
An identical formula is a successful no-op. A newer version already in the tap,
or different formula bytes for the same version, stops the update for review.
An older run cannot overwrite a newer formula.
