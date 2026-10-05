{
  description = "Harness-neutral access to local coding-agent sessions";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      revision = self.rev or self.dirtyRev or "unknown";
      shortRevision = self.shortRev or self.dirtyShortRev or "source";
      dirty = !self ? rev && self ? dirtyRev;
      version = "0-unstable-${shortRevision}";
      packagesFor = system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        pkgs.callPackage ./nix/package.nix {
          src = self;
          inherit version revision dirty;
        };
    in
    {
      packages = forAllSystems (system:
        let sessionio = packagesFor system;
        in { default = sessionio; inherit sessionio; });

      apps = forAllSystems (system: {
        default = self.apps.${system}.sessionio;
        sessionio = {
          type = "app";
          program = "${self.packages.${system}.sessionio}/bin/sessionio";
          meta.description = "Read and inspect local coding-agent sessions";
        };
      });

      checks = forAllSystems (system: {
        package = self.packages.${system}.sessionio;
        smoke = nixpkgs.legacyPackages.${system}.callPackage ./nix/check.nix {
          package = self.packages.${system}.sessionio;
        };
      });
    };
}
