# Contributing to Baseten Switch

Baseten Switch runs in the credential and prompt path of supported coding
harnesses. Changes must preserve that trust boundary and keep the runtime
small and auditable.

## Development requirements

- macOS with the Xcode command line tools
- Go as declared by `gateway/go.mod`
- Swift as provided by Xcode

## Dependency policy

Baseten Switch runs in the credential and prompt path, so dependency changes
receive security review.

- Use the Go standard library and Apple system frameworks first.
- Add no runtime dependency without a concrete need and written justification
  in the pull request. Include why existing code cannot meet the need, the
  direct and transitive packages added, the licenses, and the security review.
- Keep the Mac app self-contained. Do not load scripts, fonts, images, or other
  runtime assets from a CDN or third-party host.
- Vendor only a small, reviewable component when that creates less risk than a
  package dependency. Preserve its provenance and license, document how it is
  validated and refreshed, and include the required notice.
- Prefer a vetted dependency when implementing security-sensitive platform
  behavior by hand would create greater risk.

The current direct Go dependencies provide OS keychain access, OAuth support,
and YAML parsing. The Swift app has no external packages. Changes to Go
modules, Nix inputs, GitHub Actions, vendored material, or remote network
destinations must describe their effect on the dependency and trust boundary.

Build the CLI from the repository root:

```sh
scripts/build.sh gateway
```

Run the complete hermetic gate:

```sh
scripts/check.sh --offline
```

Maintainers run `scripts/check.sh` before accepting a release candidate. The
full gate includes a live identity refresh against the maintainer's existing
Baseten CLI credential store. It must not be run with credentials supplied by
an untrusted pull request.

For release workflow or packaging changes, also run the contracts required by
the macOS CI job:

```sh
scripts/release/test-release-contract.sh
scripts/release/test-release-workflow.sh
scripts/release/test-render-formula.sh
scripts/release/test-publish-homebrew.sh
```

See [RELEASING.md](RELEASING.md) for release configuration, publication, and
Homebrew update recovery.

## Pull requests

Keep changes focused and include tests for user-visible behavior. Describe any
effect on:

- credential loading or storage;
- prompt or response handling;
- provider selection and fallback;
- local telemetry;
- localhost APIs;
- network destinations;
- build, signing, or release artifacts.

Never commit credentials, request bodies, customer data, internal URLs, or
captured production logs. Use synthetic fixtures.

Use the private reporting process in `SECURITY.md` for vulnerabilities.
