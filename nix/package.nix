# SPDX-FileCopyrightText: © 2026 finkregh
# SPDX-License-Identifier: MIT
#
# Nix packaging for renovate-operator. This is an addition to the container
# image: it installs the operator binary while it continues to launch Renovate
# runner containers through a Docker-compatible API.
{
  lib,
  buildGoModule,
  makeWrapper,
  src,
  version ? "dev",
}:

buildGoModule {
  pname = "renovate-operator";
  inherit version src;

  vendorHash = "sha256-vpv5yBfxlnR5lRBiesqseiRggapuOppDn/sfPJs58Gk=";

  tags = [ "timetzdata" ];
  ldflags = [
    "-s"
    "-w"
    "-X"
    "main.Version=${version}"
  ];

  nativeBuildInputs = [ makeWrapper ];

  postInstall = ''
    mkdir -p "$out/share/renovate-operator"
    cp -r static "$out/share/renovate-operator/static"

    # The server intentionally resolves UI files relative to its working
    # directory. A wrapper makes that true for command-line users as well as
    # the NixOS unit's explicit WorkingDirectory.
    wrapProgram "$out/bin/renovate-operator" \
      --run 'cd "$out/share/renovate-operator"'
  '';

  meta = {
    description = "Standalone Renovate operator for Docker-compatible runtimes";
    homepage = "https://git.h.oluflorenzen.de/finkregh/renovate-docker-operator";
    license = lib.licenses.mit;
    mainProgram = "renovate-operator";
    platforms = lib.platforms.linux;
  };
}
