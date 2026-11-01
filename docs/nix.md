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

The default is a **rootful system service**. Its default `dockerHost` is Podman's
rootful API socket, `unix:///run/podman/podman.sock`. Enable that socket and
ensure the service user can access it (normally with the `podman`
supplementary group). The module creates a dedicated system user and stores
SQLite state in `/var/lib/renovate-operator` by default. Its service working
directory is the package's immutable static-asset directory, while mutable
data and secrets stay outside the Nix store.

## Rootless Podman user-manager service

To run the native operator with a rootless Podman socket, explicitly select an
existing non-root user with `rootlessUser`. This replaces the rootful system
service with a user-manager service that can only run in that user's systemd
user manager. It deliberately does not create a system user, set
`User`/`Group`, use `StateDirectory`, or depend on a system service reaching
into `/run/user`.

```nix
{
  services.renovate-operator = {
    enable = true;
    rootlessUser = "renovate";
    environment = {
      ROP_PLATFORM_ENDPOINT = "https://forge.example/api/v1";
      RENOVATE_PLATFORM = "forgejo";
      ROP_IMAGE = "renovate/renovate:latest";
    };
    environmentFile = "/home/renovate/.config/renovate-operator/secrets.env";
  };
}
```

In this mode, `dockerHost` defaults to
`unix://%t/podman/podman.sock`, where `%t` is the selected user's runtime
directory. Set `dockerHost` explicitly only when that user exposes Podman's
API socket elsewhere. The user service wants and starts after that user's
`podman.socket`; prepare it and enable lingering so its user manager and socket
are available at boot, even without an interactive login:

```sh
loginctl enable-linger renovate
sudo -u renovate XDG_RUNTIME_DIR=/run/user/$(id -u renovate) \
  systemctl --user enable --now podman.socket
```

The rootless default `dataDir` is
`%h/.local/share/renovate-operator`; the user service creates it before the
operator starts, so SQLite state is owned by the rootless user. You may set
`dataDir` to another writable location. `environmentFile` is still supported,
but it must be readable by the selected user rather than only by the system
manager. The service starts from the user's `default.target`, uses the same
immutable static-asset working directory as rootful mode, and restarts on
failure after five seconds.

Do not configure `rootlessUser = "root"`: the module rejects it. Rootless mode
therefore neither defaults to nor grants access to the rootful Podman socket.
