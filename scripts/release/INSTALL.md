# Install Baseten Switch on macOS

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
`baseten-switch_<version>.cdx.json` and `checksums.txt`. Verify both SHA-256
entries before extracting the ZIP.

The release ZIP contains the universal CLI, a nested notarized
`Baseten Switch.app.zip`, and `install.sh`. Run:

```sh
unzip baseten-switch_<version>_darwin_universal.zip \
  -d baseten-switch_<version>
cd baseten-switch_<version>
./install.sh
```

The installer validates Developer ID signatures, the app's stapled
notarization ticket, Gatekeeper acceptance, versions, and architectures.
It never clears macOS quarantine metadata.

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
