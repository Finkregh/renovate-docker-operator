{
  description = "Standalone Renovate operator for Docker-compatible runtimes";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      packageFor =
        pkgs:
        pkgs.callPackage ./nix/package.nix {
          src = self;
          # Release checkouts are tagged, while source checkouts receive a useful
          # revision suffix without maintaining a second version file.
          version = self.shortRev or "dev";
        };
    in
    {
      packages = forAllSystems (pkgs: rec {
        renovate-operator = packageFor pkgs;
        default = renovate-operator;
      });

      nixosModules.default =
        args:
        import ./nix/module.nix (
          args
          // {
            package = self.packages.${args.pkgs.system}.renovate-operator;
          }
        );
      nixosModules.renovate-operator = self.nixosModules.default;

      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
