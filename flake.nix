{
  description = "Baseten Switch: local gateway routing AI coding harnesses between native providers and Baseten";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      # No x86_64-darwin: nixpkgs 26.11 dropped it.
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      # scripts/build.sh stamps `git describe --tags --always --dirty` so
      # `baseten-switch status` can flag binary/process skew. Pure flake
      # evaluation can't run git, so stamp the commit hash instead; an
      # unpinned/dirty tree falls back to "dev" like a plain `go build`.
      version = self.shortRev or self.dirtyShortRev or "dev";
      vendorHash = "sha256-wOrYrtvL+7qecoaFfH75KdxBOFeba0zG09LEIvLpO5o=";
      ldflags = [
        "-X github.com/basetenlabs/baseten-switch/gateway/internal/version.Version=${version}"
      ];
      # The SwiftUI menubar app, built with the nixpkgs Swift toolchain —
      # no Xcode. Single-arch with a dev-build identity (version 0.0.0,
      # ad hoc executable signature from the stdenv fixup, no sealed
      # bundle resources); universal signed release artifacts remain the
      # job of scripts/build-menubar.sh with Xcode.
      menubarInfoPlist =
        pkgs:
        pkgs.writeText "Info.plist" ''
          <?xml version="1.0" encoding="UTF-8"?>
          <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
          <plist version="1.0">
          <dict>
          	<key>CFBundleIdentifier</key>
          	<string>co.baseten.switch</string>
          	<key>CFBundleName</key>
          	<string>Baseten Switch</string>
          	<key>CFBundleDisplayName</key>
          	<string>Baseten Switch</string>
          	<key>CFBundleExecutable</key>
          	<string>BasetenSwitch</string>
          	<key>BasetenSwitchBuildChannel</key>
          	<string>stable</string>
          	<key>CFBundlePackageType</key>
          	<string>APPL</string>
          	<key>CFBundleInfoDictionaryVersion</key>
          	<string>6.0</string>
          	<key>CFBundleShortVersionString</key>
          	<string>0.0.0</string>
          	<key>CFBundleVersion</key>
          	<string>0</string>
          	<key>LSMinimumSystemVersion</key>
          	<string>13.0</string>
          	<key>CFBundleIconFile</key>
          	<string>AppIcon</string>
          	<key>LSUIElement</key>
          	<true/>
          	<key>NSHighResolutionCapable</key>
          	<true/>
          	<!-- The Reauthenticate button sends an Apple event to Terminal via
          	     osascript; without this key macOS 13+ denies Automation consent
          	     with no prompt (errAEEventNotPermitted, -1743) in the packaged
          	     bundle, while a bare dev binary inherits the invoking terminal's
          	     consent and masks the failure. -->
          	<key>NSAppleEventsUsageDescription</key>
          	<string>Baseten Switch opens Terminal to run 'baseten auth login' when the shared Baseten CLI credential needs reauthentication.</string>
          </dict>
          </plist>
        '';
      mkMenubar =
        pkgs:
        pkgs.stdenv.mkDerivation {
          pname = "baseten-switch-menubar";
          inherit version;
          src = self;
          sourceRoot = "source/mac/BasetenSwitch";
          nativeBuildInputs = [
            pkgs.swift
            pkgs.swiftpm
            pkgs.rcodesign
          ];
          # swiftpm's setup hook supplies the build phase (swift-build -c
          # release) and swiftpmBinPath; assembly mirrors
          # scripts/build-menubar.sh minus lipo. rcodesign stands in for
          # `codesign --force --sign -` (Apple codesign doesn't exist in
          # the nix sandbox) and must run after fixup: fixup's strip would
          # invalidate the seal it creates.
          installPhase = ''
            runHook preInstall
            app="$out/Applications/Baseten Switch.app"
            mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
            install -m 0755 "$(swiftpmBinPath)/BasetenSwitch" "$app/Contents/MacOS/BasetenSwitch"
            cp Assets/AppIcon.icns "$app/Contents/Resources/AppIcon.icns"
            cp Assets/baseten-logo-white.svg "$app/Contents/Resources/baseten-logo-white.svg"
            cp Assets/openai-blossom.svg "$app/Contents/Resources/openai-blossom.svg"
            cp ${menubarInfoPlist pkgs} "$app/Contents/Info.plist"
            runHook postInstall
          '';
          postFixup = ''
            rcodesign sign "$out/Applications/Baseten Switch.app"
          '';
          meta = {
            description = "Baseten Switch menubar app (nix dev build; release ships via scripts/build-menubar.sh)";
            homepage = "https://github.com/basetenlabs/baseten-switch";
            license = nixpkgs.lib.licenses.mit;
            platforms = [ "aarch64-darwin" ];
          };
        };
      # The darwin default install: gateway CLI plus the menubar app in
      # one output, so `home.packages`/`nix profile` consumers get both.
      # home-manager surfaces the Applications/ entry in
      # "~/Applications/Home Manager Apps"; the CLI-only install remains
      # available as packages.baseten-switch.
      mkDefaultDarwin =
        pkgs:
        pkgs.symlinkJoin {
          # Named like the CLI package so the `nix profile upgrade
          # baseten-switch` invocation in the README matches either
          # install.
          name = "baseten-switch-${version}";
          paths = [
            (mkBasetenSwitch pkgs)
            (mkMenubar pkgs)
          ];
          meta = {
            description = "Baseten Switch gateway and menubar app";
            homepage = "https://github.com/basetenlabs/baseten-switch";
            license = nixpkgs.lib.licenses.mit;
            # `nix run` still resolves to the gateway CLI.
            mainProgram = "baseten-switch";
            platforms = [ "aarch64-darwin" ];
          };
        };
      mkBasetenSwitch =
        pkgs:
        pkgs.buildGoModule {
          pname = "baseten-switch";
          inherit version vendorHash ldflags;
          # The Go module lives in gateway/, but its tests read the
          # repo-level config/gateway.example.yaml (byte-equality pin with
          # the embedded init template), so the whole repo is the source.
          src = self;
          modRoot = "gateway";
          subPackages = [ "cmd/baseten-switch" ];
          meta = {
            description = "Local gateway routing AI coding harnesses between native providers and Baseten";
            homepage = "https://github.com/basetenlabs/baseten-switch";
            license = nixpkgs.lib.licenses.mit;
            # `nix run` and lib.getExe resolve the binary through this.
            mainProgram = "baseten-switch";
            platforms = systems;
          };
        };
    in
    {
      packages = forAllSystems (
        pkgs:
        rec {
          default = if pkgs.stdenv.isDarwin then mkDefaultDarwin pkgs else baseten-switch;
          baseten-switch = mkBasetenSwitch pkgs;
        }
        // nixpkgs.lib.optionalAttrs pkgs.stdenv.isDarwin {
          menubar = mkMenubar pkgs;
        }
      );

      # For consumers that layer packages into nixpkgs (home-manager,
      # NixOS, nix-darwin) instead of referencing the packages output:
      #   nixpkgs.overlays = [ baseten-switch.overlays.default ];
      # The host nixpkgs must carry a Go new enough for gateway/go.mod.
      overlays.default = final: _prev: {
        baseten-switch = mkBasetenSwitch final;
        # darwin-only (lazy, so harmless elsewhere): the menubar app for
        # consumers that want it as a separate store path.
        baseten-switch-menubar = mkMenubar final;
      };

      checks = forAllSystems (
        pkgs:
        {
          # The shipped package; its check phase runs the cmd/baseten-switch
          # tests because subPackages scopes both build and test.
          package = mkBasetenSwitch pkgs;
          # The full gateway test suite: with no subPackages set,
          # buildGoModule builds and tests every package in the module,
          # sandboxed (no HOME, no network beyond loopback) — the same
          # honesty gate CI relies on, extended to the whole tree.
          gateway-tests = pkgs.buildGoModule {
            pname = "baseten-switch-gateway-tests";
            inherit version vendorHash ldflags;
            src = self;
            modRoot = "gateway";
          };
        }
        // nixpkgs.lib.optionalAttrs pkgs.stdenv.isDarwin {
          # Compile gate only: XCTest runs stay on the Xcode toolchain
          # (scripts/check.sh) — nixpkgs swiftpm has test running patched
          # out on darwin.
          menubar = mkMenubar pkgs;
        }
      );

      devShells = forAllSystems (
        pkgs:
        {
          # Gateway development: the Go toolchain for gateway/go.mod.
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
            ];
          };
        }
        // nixpkgs.lib.optionalAttrs pkgs.stdenv.isDarwin {
          # nixpkgs Swift toolchain (5.10) for building the menubar app
          # without Xcode: `swift build` works (SwiftUI/Charts compile
          # against the nixpkgs Apple SDK), and swift-corelibs-xctest lets
          # `swift build --build-tests` type-check the test target. Running
          # tests does not work here — nixpkgs swiftpm patches out darwin
          # test running, and its swift-driver mangles SwiftPM's
          # `-rpath @loader_path/...` into a response-file read at the test
          # bundle link. `swift test` stays on the Xcode toolchain via
          # scripts/check.sh; universal release bundles stay on
          # scripts/build-menubar.sh.
          menubar = pkgs.mkShell {
            packages = [
              pkgs.swift
              pkgs.swiftpm
              pkgs.swiftPackages.XCTest
            ];
          };
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
