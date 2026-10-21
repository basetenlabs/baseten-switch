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
    in
    {
      packages = forAllSystems (
        pkgs:
        let
          # Keep Nix builds on the same compiler as go.mod.
          go = pkgs.go_1_26.overrideAttrs (finalAttrs: _: {
            version = "1.26.10";
            src = pkgs.fetchurl {
              url = "https://go.dev/dl/go${finalAttrs.version}.src.tar.gz";
              hash = "sha256-/WPog7ovK/ypPBZGsx6+9j4rHk0rMXyXidl2po2B828=";
            };
          });
          buildGoModule = pkgs.buildGoModule.override { inherit go; };
        in
        rec {
          default = baseten-switch;
          baseten-switch = buildGoModule {
            pname = "baseten-switch";
            inherit version;
            # The Go module lives in gateway/, but its tests read the
            # repo-level config/gateway.example.yaml (byte-equality pin with
            # the embedded init template), so the whole repo is the source.
            src = self;
            modRoot = "gateway";
            subPackages = [ "cmd/baseten-switch" ];
            vendorHash = "sha256-q/aZk0k/kRb+7mO0iD9vKHcchybS++zYix7dgiyZjKA=";
            ldflags = [ "-X github.com/basetenlabs/baseten-switch/gateway/internal/version.Version=${version}" ];
          };
        }
      );
    };
}
