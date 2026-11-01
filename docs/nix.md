# Nix and NixOS

The flake is an alternative to the containerised operator, not a replacement
for Renovate's runner image. The native service still launches
`renovate/renovate` children through the Docker-compatible Podman API.

## Package

```sh
nix build .#renovate-operator
nix run .#renovate-operator
```

The flake supports `x86_64-linux` and `aarch64-linux` and provides:

- `packages.<system>.renovate-operator` (also the default package)
- `nixosModules.renovate-operator` (also the default NixOS module)

`vendorHash` is deliberately pinned. When Go dependencies change, build the
package and replace the reported hash in `nix/package.nix`; releases do not
otherwise need to modify Nix files. Consumers pin an immutable source revision
in `flake.lock`, so a Cocogitto version tag automatically identifies the source
version used by a Nix deployment.

## NixOS service

Import the module from this flake and keep credentials outside the Nix store:

```nix
{
  inputs.renovate-operator.url =
    "git+https://git.h.oluflorenzen.de/finkregh/renovate-docker-operator.git";

  outputs = { nixpkgs, renovate-operator, ... }: {
    nixosConfigurations.host = nixpkgs.lib.nixosSystem {
      modules = [
        renovate-operator.nixosModules.default
        ({ ... }: {
          services.renovate-operator = {
            enable = true;
            environment = {
              ROP_PLATFORM_ENDPOINT = "https://forge.example/api/v1";
              RENOVATE_PLATFORM = "forgejo";
              ROP_IMAGE = "renovate/renovate:latest";
            };
            environmentFile = "/run/secrets/renovate-operator.env";
            supplementaryGroups = [ "podman" ];
          };
        })
      ];
    };
  };
}
```

The referenced `environmentFile` must contain at least:

```ini
RENOVATE_TOKEN=replace-with-a-secret
```

The default `dockerHost` is Podman's rootful API socket:
`unix:///run/podman/podman.sock`. Ensure that socket is enabled and that the
service user can access it (usually the `podman` supplementary group). For a
rootless Podman socket, set `dockerHost` to that user's socket and configure
`user`, `group`, and `supplementaryGroups` to match its ownership.

The module creates a dedicated system user and stores SQLite state in
`/var/lib/renovate-operator` by default. Its service working directory is the
package's immutable static-asset directory, while mutable data and secrets stay
outside the Nix store.
