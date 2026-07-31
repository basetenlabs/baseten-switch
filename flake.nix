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
      packages = forAllSystems (pkgs: rec {
        default = baseten-switch;
        baseten-switch = mkBasetenSwitch pkgs;
      });

      # For consumers that layer packages into nixpkgs (home-manager,
      # NixOS, nix-darwin) instead of referencing the packages output:
      #   nixpkgs.overlays = [ baseten-switch.overlays.default ];
      # The host nixpkgs must carry a Go new enough for gateway/go.mod.
      overlays.default = final: _prev: {
        baseten-switch = mkBasetenSwitch final;
      };

      checks = forAllSystems (pkgs: {
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
      });

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
          # Experimental: nixpkgs Swift toolchain (5.10+) for building the
          # menubar app without Xcode. SwiftUI/Charts compilation against
          # the nixpkgs Apple SDK is unproven; scripts/build-menubar.sh
          # with the Xcode toolchain remains the supported app build.
          menubar = pkgs.mkShell {
            packages = [
              pkgs.swift
              pkgs.swiftpm
            ];
          };
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
