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
      packages = forAllSystems (pkgs: rec {
        default = baseten-switch;
        baseten-switch = pkgs.buildGoModule {
          pname = "baseten-switch";
          inherit version;
          # The Go module lives in gateway/, but its tests read the
          # repo-level config/gateway.example.yaml (byte-equality pin with
          # the embedded init template), so the whole repo is the source.
          src = self;
          modRoot = "gateway";
          subPackages = [ "cmd/baseten-switch" ];
          vendorHash = "sha256-wOrYrtvL+7qecoaFfH75KdxBOFeba0zG09LEIvLpO5o=";
          ldflags = [ "-X github.com/basetenlabs/baseten-switch/gateway/internal/version.Version=${version}" ];
        };
      });
    };
}
