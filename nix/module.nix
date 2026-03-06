# NixOS module for hived — the Hive control plane.
#
# Usage in configuration.nix:
#
#   {
#     inputs.hive.url = "github:brmurrell3/hive";
#
#     outputs = { self, nixpkgs, hive, ... }: {
#       nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
#         modules = [
#           hive.nixosModules.default
#           {
#             services.hived = {
#               enable = true;
#               clusterRoot = "/home/deploy/hive-cluster";
#               openFirewall = true;
#             };
#           }
#         ];
#       };
#     };
#   }

flake:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.hived;
  defaultPackage = flake.packages.${pkgs.stdenv.hostPlatform.system}.hived;
  hivectlPackage = flake.packages.${pkgs.stdenv.hostPlatform.system}.hivectl;
in
{
  options.services.hived = {
    enable = lib.mkEnableOption "Hive control plane (hived)";

    package = lib.mkOption {
      type = lib.types.package;
      default = defaultPackage;
      description = "The hived package to use.";
    };

    clusterRoot = lib.mkOption {
      type = lib.types.str;
      description = "Path to the cluster root directory containing cluster.yaml.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "hive";
      description = "User account under which hived runs.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "hive";
      description = "Group under which hived runs.";
    };

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Whether to open the NATS port (4222/tcp) in the firewall.";
    };
  };

  config = lib.mkIf cfg.enable {
    # Create system user and group.
    users.users.${cfg.user} = lib.mkIf (cfg.user == "hive") {
      isSystemUser = true;
      group = cfg.group;
      home = "/var/lib/hive";
      createHome = true;
      description = "Hive control plane service user";
    };

    users.groups.${cfg.group} = lib.mkIf (cfg.group == "hive") { };

    # Systemd service.
    systemd.services.hived = {
      description = "Hive Control Plane";
      after = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];

      serviceConfig = {
        Type = "simple";
        User = cfg.user;
        Group = cfg.group;
        ExecStart = "${cfg.package}/bin/hived --cluster-root ${cfg.clusterRoot}";
        Restart = "on-failure";
        RestartSec = 5;
        StateDirectory = "hive";

        # Hardening.
        NoNewPrivileges = true;
        ProtectSystem = "full";
        ReadWritePaths = [ cfg.clusterRoot ];
        PrivateTmp = true;
      };
    };

    # Put hivectl on the system PATH.
    environment.systemPackages = [ hivectlPackage ];

    # Open NATS port if requested.
    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ 4222 ];
  };
}
