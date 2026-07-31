# Baseten Switch

Baseten Switch is a local macOS app and gateway that routes supported AI
coding harnesses between their native providers and models served on Baseten.
One global switch controls Baseten routing, and per-client mappings select the
model that serves each request.

> **Beta:** Baseten Switch is under active development. Interfaces,
> configuration, and behavior may change between releases. The current macOS
> build is ad hoc signed and not yet notarized by Apple, so first launch may
> require approval under System Settings → Privacy & Security.

The first public release supports macOS 13 or newer on Apple Silicon and Intel.

## Quick start

```sh
brew install basetenlabs/baseten/baseten-switch
baseten-switch setup
baseten-switch up --install
baseten-switch claude on
baseten-switch doctor --probe
```

The fully qualified Homebrew command adds Baseten's public tap and installs
both Baseten Switch and its Baseten CLI dependency. It requires no GitHub
login, separate `brew tap`, second install command, or local compiler. The beta
release includes a universal macOS artifact for Apple Silicon and Intel.

`setup` verifies the Baseten CLI and its current credential, opens
`baseten auth login` when needed, and creates the initial configuration.
It never overwrites an existing configuration. `up --install` installs the
user launch agents, starts the local gateway, installs Baseten Switch.app in
`~/Applications`, and opens the app. `claude on` connects new Claude Code
sessions to the gateway. The final command checks the complete request path
with a small live request.

If macOS blocks the app's first launch, open **System Settings → Privacy &
Security**, scroll to **Security**, and click **Open Anyway**. This control
appears after a blocked launch attempt. A managed Mac may prohibit the
override.

## Use the Mac app

The menu bar app is the primary interface for daily use. It provides:

- the global Baseten routing switch;
- Claude Code and Codex model configuration;
- current health, active fallback, and authentication status;
- recent traffic, performance, and spend views;
- actions to start the system and run it at login.

After an upgrade, `baseten-switch up` adopts the new CLI and app version.
Run `baseten-switch menubar` when you only need to install, refresh, or reopen
the app from the current Homebrew package.

The app and CLI update the same configuration. You can use either interface
without maintaining separate state.

## Pi

Install the direct Baseten provider for
[Pi](https://github.com/badlogic/pi-mono) without starting Baseten Switch:

```sh
brew install basetenlabs/baseten/baseten-switch
export BASETEN_API_KEY="<your-api-key>"
baseten-switch pi install
pi --provider baseten --model <model-slug>
```

`pi install` reads the visible model catalog through the Baseten CLI and adds
only the direct `baseten` provider to Pi. It joins exact model IDs with the
public [models.dev](https://models.dev/) catalog for reasoning and text/image
input capabilities. Models without an exact capability match use conservative
defaults and are reported during installation. The API key remains in the
environment; Pi's configuration contains only a `$BASETEN_API_KEY` reference.
The command does not create gateway configuration, install launch agents,
start the router or front door, or change another harness profile.

The Homebrew formula contains the optional Mac app archive, but this flow does
not install the app in `~/Applications` or open it. Manage the direct provider
without running local services:

```sh
baseten-switch pi status
baseten-switch pi uninstall
```

## Claude Code

`baseten-switch claude on` saves the previous Claude Code environment values,
points new sessions at Baseten Switch, enables deferred tool loading, and
omits Claude Code's attribution block to improve gateway prompt-cache hit
rates. Restart Claude Code after enabling or disabling the integration.

Useful controls:

```sh
baseten-switch on
baseten-switch off
baseten-switch claude status
baseten-switch claude route
baseten-switch claude route sonnet zai-org/GLM-5.2
baseten-switch claude route sonnet native
baseten-switch claude subagents zai-org/GLM-5.2
```

`on` and `off` change one global routing switch. Saved model mappings remain
editable while routing is off. A Claude family can map to `native`, a
configured alias, or a Baseten model slug. Run
`baseten-switch claude route <family> default` to remove a family override.

To restore the Claude Code setting that existed before setup:

```sh
baseten-switch claude off
```

## Codex CLI

Codex support is opt-in. Install
[Codex CLI](https://github.com/openai/codex), start Baseten Switch,
and create its managed profile:

```sh
baseten-switch codex on
baseten-switch codex route zai-org/GLM-5.2
baseten-switch codex status
codex --profile baseten
```

The first `codex on` may request permission to enable the parked Codex
listener. Baseten Switch writes `~/.codex/baseten.config.toml`; it does not
modify `~/.codex/config.toml`. Start Codex without `--profile baseten` to use
native OpenAI routing.

Remove the managed profile and restore any file it replaced:

```sh
baseten-switch codex off
```

Do not override the managed profile's compatibility model with `-m`. Select
the upstream Baseten model with `baseten-switch codex route` instead.

## Status and troubleshooting

```sh
baseten-switch status
baseten-switch status --verbose
baseten-switch doctor
baseten-switch doctor --probe
```

`status` summarizes the router, front door, Mac app, authentication, and
client routing state. `doctor` inspects the same path without changing it and
prints the first failure with a concrete fix. `doctor --fix` can apply
supported repairs after confirmation.

Baseten Switch stores configuration, local state, logs, and telemetry under
`~/.config/baseten-switch/`. The primary logs are:

```text
~/.config/baseten-switch/logs/router.log
~/.config/baseten-switch/logs/door.log
```

Run `baseten-switch auth login` if the Baseten credential expires. The command
delegates authentication to the Baseten CLI, reloads the gateway, and prints
the current identity.

## Upgrade

```sh
brew upgrade baseten-switch
baseten-switch up
baseten-switch doctor
```

`up` leaves healthy current components alone and moves stale components to the
new binary and app. Homebrew remains the canonical public install and upgrade
channel.

## Uninstall

Inspect the exact removal first:

```sh
baseten-switch uninstall --dry-run
```

Then remove managed harness settings, processes, launch agents, runtime
residue, and the Mac app:

```sh
baseten-switch uninstall
brew uninstall baseten-switch
```

The default uninstall retains configuration, telemetry, logs, and backups.
To remove those files as well, use this instead of the standard uninstall:

```sh
baseten-switch uninstall --purge --yes
brew uninstall baseten-switch
```

Uninstall never removes Baseten CLI credentials or keychain entries. The Mac
app unregisters its own Start at Login item as part of bundle removal. When
the installed app predates that helper or the unregister fails, the command
leaves the bundle in place and prints the manual action required in macOS
System Settings.

## Privacy and trust

Baseten Switch binds its services to the local loopback interface. It receives
harness requests and credentials because it is in the selected request path.
Request content leaves the machine only for the upstream chosen by the active
routing policy. Baseten credentials go only to Baseten, and native credentials
go only to their matching native provider.

Local telemetry contains request metadata, not prompts, responses,
credentials, headers, or request bodies. Disable future records by setting
`telemetry_enabled: false` in `gateway.yaml`, then reload the configuration.
Delete existing records from
`~/.config/baseten-switch/telemetry/`.

Public model-catalog refreshes from models.dev send no credential or
user-derived data. The unauthenticated administration API binds to loopback
and must never be exposed on a network interface.

## Configuration

The generated configuration is
`~/.config/baseten-switch/gateway.yaml`. Prefer the Mac app or typed CLI
commands over direct edits; routing changes hot-reload without a restart.

See [config/schema.md](config/schema.md) for every field and
[config/gateway.example.yaml](config/gateway.example.yaml) for the generated
shape. Store API-key overrides in
`~/.config/baseten-switch/env`, which must use mode `0600`, rather than in
`gateway.yaml`.

## Build and test

The Go module in `gateway/` builds the CLI, router, administration API, and
front door. The Swift package in `mac/BasetenSwitch/` builds the native menu
bar app without external Swift packages.

```sh
scripts/build.sh
scripts/check.sh
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution and dependency rules.
See [TESTING.md](TESTING.md) for test layers and isolation requirements.

## Nix source build

The Nix flake is an alternate source build for Linux and Apple Silicon macOS.
It does not include the signed Mac app or install the Baseten CLI dependency:

```sh
nix profile install github:basetenlabs/baseten-switch#baseten-switch
```

Upgrade and restart the locally running components:

```sh
nix profile upgrade --refresh baseten-switch
baseten-switch up
```

Homebrew is the supported path for the complete macOS product.

## License

Baseten Switch is available under the [MIT License](LICENSE).
