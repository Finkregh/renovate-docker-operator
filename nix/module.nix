# SPDX-FileCopyrightText: © 2026 finkregh
# SPDX-License-Identifier: MIT
#
# NixOS module for the native operator. Renovate itself still runs in child
# containers, so a Docker-compatible Podman socket must be reachable.
{
  config,
  lib,
  pkgs,
  package ? null,
  ...
}:

let
  cfg = config.services.renovate-operator;
  inherit (lib)
    mkEnableOption
    mkIf
    mkOption
    types
    ;
  renderedEnvironment = lib.mapAttrsToList (name: value: "${name}=${value}") cfg.environment;
  isRootless = cfg.rootlessUser != null;
in
{
  options.services.renovate-operator = {
    enable = mkEnableOption "the native Renovate operator";

    package = mkOption {
      type = types.package;
      default =
        if package != null then
          package
        else
          throw "Set services.renovate-operator.package when using this module outside its flake";
      description = "The renovate-operator package to run.";
    };

    environment = mkOption {
      type = types.attrsOf types.str;
      default = { };
      description = "Non-secret environment variables for the operator.";
    };

    environmentFile = mkOption {
      type = types.nullOr types.path;
      default = null;
      example = "/run/secrets/renovate-operator.env";
      description = "Environment file holding secrets such as RENOVATE_TOKEN.";
    };

    dataDir = mkOption {
      type = types.str;
      default =
        if cfg.rootlessUser == null then
          "/var/lib/renovate-operator"
        else
          "%h/.local/share/renovate-operator";
      description = "Directory used for SQLite state. The rootless user-manager default is relative to the selected user's home directory.";
    };

    rootlessUser = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "renovate";
      description = ''
        Existing non-root user whose systemd user manager runs the operator.
        Setting this option enables rootless user-manager mode instead of the
        default rootful system service.
      '';
    };

    user = mkOption {
      type = types.str;
      default = "renovate-operator";
      description = "System user running the operator.";
    };

    group = mkOption {
      type = types.str;
      default = "renovate-operator";
      description = "Primary group running the operator.";
    };

    supplementaryGroups = mkOption {
      type = types.listOf types.str;
      default = [ ];
      example = [ "podman" ];
      description = "Groups permitted to access the configured Docker-compatible socket.";
    };

    dockerHost = mkOption {
      type = types.str;
      default =
        if cfg.rootlessUser == null then
          "unix:///run/podman/podman.sock"
        else
          "unix://%t/podman/podman.sock";
      description = ''
        DOCKER_HOST endpoint for the Podman or Docker API. Rootless
        user-manager mode defaults to the selected user's runtime directory.
      '';
    };
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = !isRootless || cfg.rootlessUser != "root";
        message = "services.renovate-operator.rootlessUser must name a non-root user.";
      }
    ];

    users.users = lib.optionalAttrs (!isRootless && cfg.user == "renovate-operator") {
      renovate-operator = {
        isSystemUser = true;
        group = cfg.group;
      };
    };
    users.groups = lib.optionalAttrs (!isRootless && cfg.group == "renovate-operator") {
      renovate-operator = { };
    };

    systemd.services = lib.optionalAttrs (!isRootless) {
      renovate-operator = {
        description = "Renovate Operator";
        wantedBy = [ "multi-user.target" ];
        after = [ "network-online.target" ];
        wants = [ "network-online.target" ];
        serviceConfig = {
          ExecStart = "${cfg.package}/bin/renovate-operator";
          User = cfg.user;
          Group = cfg.group;
          SupplementaryGroups = cfg.supplementaryGroups;
          WorkingDirectory = "${cfg.package}/share/renovate-operator";
          StateDirectory = lib.removePrefix "/var/lib/" cfg.dataDir;
          Restart = "on-failure";
          RestartSec = "5s";
        }
        // lib.optionalAttrs (cfg.environmentFile != null) {
          EnvironmentFile = cfg.environmentFile;
        };
        environment = [
          "DOCKER_HOST=${cfg.dockerHost}"
          "ROP_SQLITE_PATH=${cfg.dataDir}/renovate.db"
        ]
        ++ renderedEnvironment;
      };
    };

    systemd.user.services = lib.optionalAttrs isRootless {
      renovate-operator = {
        description = "Renovate Operator (rootless)";
        wantedBy = [ "default.target" ];
        after = [ "podman.socket" ];
        wants = [ "podman.socket" ];
        unitConfig.ConditionUser = cfg.rootlessUser;
        serviceConfig = {
          ExecStartPre = [ "${pkgs.coreutils}/bin/mkdir -p ${cfg.dataDir}" ];
          ExecStart = "${cfg.package}/bin/renovate-operator";
          WorkingDirectory = "${cfg.package}/share/renovate-operator";
          Restart = "on-failure";
          RestartSec = "5s";
        }
        // lib.optionalAttrs (cfg.environmentFile != null) {
          EnvironmentFile = cfg.environmentFile;
        };
        environment = {
          DOCKER_HOST = cfg.dockerHost;
          ROP_SQLITE_PATH = "${cfg.dataDir}/renovate.db";
        }
        // cfg.environment;
      };
    };
  };
}
