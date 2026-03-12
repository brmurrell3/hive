# NixOS module for a Hive CI self-hosted GitHub Actions runner.
#
# Import this in your NixOS configuration and set the token:
#
#   imports = [ ./nix/ci-runner.nix ];
#   services.hive-ci-runner = {
#     enable = true;
#     url = "https://github.com/brmurrell3/hive";
#     tokenFile = "/etc/github-runner-token";  # file containing the runner registration token
#   };
#
# Generate the token at:
#   https://github.com/brmurrell3/hive/settings/actions/runners/new
#
# The runner will have KVM, nftables, Firecracker, Go, and all tools needed
# to run the full test suite including real VM tests.

{ config, lib, pkgs, ... }:

let
  cfg = config.services.hive-ci-runner;
in
{
  options.services.hive-ci-runner = {
    enable = lib.mkEnableOption "Hive CI self-hosted GitHub Actions runner";

    url = lib.mkOption {
      type = lib.types.str;
      default = "https://github.com/brmurrell3/hive";
      description = "GitHub repository URL for the runner.";
    };

    tokenFile = lib.mkOption {
      type = lib.types.str;
      description = "Path to file containing the GitHub runner registration token.";
    };

    name = lib.mkOption {
      type = lib.types.str;
      default = config.networking.hostName;
      description = "Name for this runner (shown in GitHub UI).";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "github-runner";
      description = "User account for the runner.";
    };
  };

  config = lib.mkIf cfg.enable {
    # KVM access
    virtualisation.libvirtd.enable = false; # we don't need libvirtd, just /dev/kvm
    boot.kernelModules = [ "kvm-intel" "kvm-amd" ]; # load whichever matches

    # GitHub Actions runner
    services.github-runners.hive-ci = {
      enable = true;
      url = cfg.url;
      tokenFile = cfg.tokenFile;
      name = cfg.name;
      replace = true;
      extraLabels = [ "nixos" "kvm" "self-hosted" ];

      extraPackages = with pkgs; [
        # Build toolchain
        go
        gnumake
        gcc
        git
        curl
        coreutils

        # VM / Firecracker
        firecracker

        # Networking
        nftables
        iptables
        iproute2

        # Rootfs builds
        e2fsprogs
        gzip
        util-linux # losetup, etc.

        # Nix
        nix

        # Test tools
        python3
      ];

      extraEnvironment = {
        # Ensure nix commands work inside the runner
        NIX_PATH = "nixpkgs=${pkgs.path}";
      };

      serviceOverrides = {
        # Grant KVM access
        SupplementaryGroups = [ "kvm" ];
        # Allow nftables (needs CAP_NET_ADMIN)
        AmbientCapabilities = [ "CAP_NET_ADMIN" "CAP_SYS_ADMIN" ];
      };
    };

    # Ensure /dev/kvm is accessible
    users.groups.kvm = {};
    services.udev.extraRules = ''
      KERNEL=="kvm", GROUP="kvm", MODE="0660"
    '';
  };
}
