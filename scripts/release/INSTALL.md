# Install Baseten Switch on macOS

> **Beta:** Baseten Switch is currently a public beta. The CLI and app are
> ad-hoc signed but are not Apple-notarized. macOS may require explicit
> approval before the app opens for the first time.

Homebrew is the canonical public install and upgrade channel. This one
command installs the Baseten CLI and Baseten Switch from Baseten's public tap:

```sh
brew install basetenlabs/baseten/baseten-switch
```

Then authenticate and start the gateway:

```sh
baseten-switch setup
baseten-switch up --install
baseten-switch claude on
baseten-switch doctor --probe
```

## Direct release asset

Approved direct installs use
`baseten-switch_<version>_darwin_universal.zip`. The release also publishes
`checksums.txt`. Verify the ZIP's SHA-256 entry before extracting it.

The release ZIP contains the universal, ad-hoc signed CLI, a nested ad-hoc signed
`Baseten Switch.app.zip`, and `install.sh`. Run:

```sh
unzip baseten-switch_<version>_darwin_universal.zip \
  -d baseten-switch_<version>
cd baseten-switch_<version>
./install.sh
```

The installer validates the ad-hoc signatures, matching versions, and universal
architectures. An ad-hoc signature detects changes after packaging, but it does
not establish an Apple-verified developer identity or notarization.

## First launch approval

If macOS blocks the beta app on first launch:

1. In Finder, open `~/Applications`, double-click **Baseten Switch**, then
   dismiss the warning.
2. Choose **Apple menu > System Settings > Privacy & Security**.
3. Scroll to the **Security** section and click **Open Anyway** for Baseten
   Switch.
4. Confirm **Open** and authenticate if macOS asks.

The **Open Anyway** button is available for about one hour after the blocked
launch attempt. A managed Mac may prevent this override. Baseten Switch does
not ask users to clear quarantine metadata or disable Gatekeeper.

## Uninstall

```sh
baseten-switch uninstall --dry-run
baseten-switch uninstall
```

Use `baseten-switch uninstall --purge --yes` to remove retained Baseten Switch
config, telemetry, logs, and backups. Baseten CLI credentials and keychain
entries are never removed. The command prints manual instructions when the
macOS app's Start at Login item prevents safe automated bundle removal.

The repository README contains the supported install, operation,
troubleshooting, and uninstall instructions.
