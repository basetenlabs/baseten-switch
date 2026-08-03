# Changelog

All notable changes to Baseten Switch appear in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Baseten Switch remains in beta, so interfaces and behavior may change between
releases.

## [Unreleased]

### Added

- Add direct Pi provider installation, status, and removal commands that do
  not start the local gateway.
- Add release automation that opens a formula update pull request in the
  public Homebrew tap after each beta release.

### Fixed

- Update the Claude Code integration to enable deferred tool loading, omit the
  attribution block, and preserve unrelated settings.

## [0.1.0] - 2026-07-28

### Added

- Publish the first public beta of the local gateway, CLI, and macOS menu bar
  app.
- Support Homebrew installation on Apple Silicon and Intel Macs with a
  universal, ad hoc signed release artifact.
- Add managed routing for Claude Code and an opt-in profile for Codex CLI.

[Unreleased]: https://github.com/basetenlabs/baseten-switch/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/basetenlabs/baseten-switch/releases/tag/v0.1.0
