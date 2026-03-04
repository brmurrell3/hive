# NixOS configuration for Hive Firecracker VMs.
#
# This produces a minimal NixOS system image targeting Firecracker's
# constrained environment: virtio-mmio devices, serial console, vsock
# for host-guest communication, and a systemd service for the sidecar.
#
# Build:
#   nix build .#rootfs   -> result contains the ext4 rootfs image
#   nix build .#kernel   -> result contains vmlinux for Firecracker
{ config, pkgs, lib, modulesPath, ... }:

let
  # Custom kernel with only the subsystems Firecracker needs.
  customKernel = import ./kernel.nix { inherit pkgs lib; };
in
{
  imports = [
    "${modulesPath}/profiles/minimal.nix"
  ];

  # ---------------------------------------------------------------------------
  # Boot
  # ---------------------------------------------------------------------------
  boot = {
    # Use our custom minimal kernel (see kernel.nix).
    kernelPackages = pkgs.linuxPackagesFor customKernel;

    # Firecracker does direct kernel boot -- no bootloader needed.
    loader.grub.enable = false;

    # Kernel parameters matching what the Firecracker hypervisor passes.
    kernelParams = [
      "console=ttyS0"
      "reboot=k"
      "panic=1"
      "pci=off"
      "init=${config.system.build.toplevel}/init"
    ];

    # Keep initrd small; only the modules Firecracker actually exposes.
    initrd.availableKernelModules = [
      "virtio_mmio"
      "virtio_blk"
      "virtio_net"
    ];
    initrd.kernelModules = [];

    # Ensure the running kernel has virtio + vsock support.
    kernelModules = [
      "virtio_mmio"
      "virtio_blk"
      "virtio_net"
      "vsock"
      "vmw_vsock_virtio_transport"
    ];

    # Kernel tuning for a small Firecracker guest.
    kernel.sysctl = {
      "vm.overcommit_memory" = 1;
    };
  };

  # ---------------------------------------------------------------------------
  # Filesystem
  # ---------------------------------------------------------------------------
  fileSystems."/" = {
    device = "/dev/vda";
    fsType = "ext4";
    autoResize = true;
  };

  # No swap inside a Firecracker VM.
  swapDevices = [];

  # ---------------------------------------------------------------------------
  # Networking
  # ---------------------------------------------------------------------------
  networking = {
    hostName = "hive-vm";
    firewall.enable = false;
    useDHCP = true;
  };

  # ---------------------------------------------------------------------------
  # Hive sidecar service
  # ---------------------------------------------------------------------------
  systemd.services.hive-sidecar = {
    description = "Hive Agent Sidecar";
    after = [ "network.target" ];
    wantedBy = [ "multi-user.target" ];
    serviceConfig = {
      ExecStart = "/opt/hive/sidecar";
      Restart = "always";
      RestartSec = "5s";
      WorkingDirectory = "/opt/hive";

      # Sidecar environment — values here are defaults; the control plane
      # can override them via the agent drive or environment file.
      Environment = [
        "HIVE_SIDECAR_MODE=standalone"
        "HIVE_SIDECAR_PORT=9100"
      ];
    };
  };

  # ---------------------------------------------------------------------------
  # Directory structure expected by the sidecar
  # ---------------------------------------------------------------------------
  systemd.tmpfiles.rules = [
    "d /opt/hive        0755 root root -"
    "d /opt/hive/agent  0755 root root -"
    "d /opt/hive/tools  0755 root root -"
    "d /workspace       0755 root root -"
  ];

  # ---------------------------------------------------------------------------
  # vsock device access
  # ---------------------------------------------------------------------------
  # Ensure /dev/vsock and /dev/vhost-vsock are accessible.
  services.udev.extraRules = ''
    KERNEL=="vsock",       MODE="0666"
    KERNEL=="vhost-vsock", MODE="0660", GROUP="kvm"
  '';

  # ---------------------------------------------------------------------------
  # Serial console (debugging)
  # ---------------------------------------------------------------------------
  # Auto-login root on the serial console so operators can inspect the VM.
  services.getty.autologinUser = "root";

  # ---------------------------------------------------------------------------
  # Minimize image size
  # ---------------------------------------------------------------------------
  documentation.enable = false;
  programs.command-not-found.enable = false;
  security.polkit.enable = lib.mkDefault false;
  xdg.autostart.enable = false;
  xdg.icons.enable = false;
  xdg.mime.enable = false;
  xdg.sounds.enable = false;
  fonts.fontconfig.enable = false;

  # Disable nix-channel and flake registry fetching inside the VM.
  nix.settings.experimental-features = [ "nix-command" "flakes" ];
  nix.channel.enable = false;

  # ---------------------------------------------------------------------------
  # Minimal packages available inside the VM
  # ---------------------------------------------------------------------------
  environment.systemPackages = with pkgs; [
    bashInteractive
    coreutils
    iproute2
    cacert
    curl
    util-linux   # for lsblk, mount, etc.
    procps       # for ps, top
    strace       # for debugging agent issues
  ];

  # ---------------------------------------------------------------------------
  # Build the ext4 rootfs image
  # ---------------------------------------------------------------------------
  # This uses the NixOS make-disk-image machinery to produce a raw ext4
  # filesystem image suitable for Firecracker's block device.
  system.build.rootfsImage = import "${pkgs.path}/nixos/lib/make-ext4-fs.nix" {
    inherit pkgs lib;
    storePaths = [ config.system.build.toplevel ];
    volumeLabel = "hive-rootfs";
    # Target < 500 MB; the image will be sparse so the file on disk is smaller
    # than this until the guest fills it.
    # Note: if the NixOS closure exceeds this, increase the value.
    # populateImageCommands copies the toplevel init symlink and required dirs.
    populateImageCommands = ''
      mkdir -p ./files/opt/hive
      mkdir -p ./files/opt/hive/agent
      mkdir -p ./files/opt/hive/tools
      mkdir -p ./files/workspace
      mkdir -p ./files/etc

      # Ensure the init symlink exists at /sbin/init for systemd.
      mkdir -p ./files/sbin
      ln -sf ${config.system.build.toplevel}/init ./files/sbin/init
    '';
  };

  # ---------------------------------------------------------------------------
  # System version
  # ---------------------------------------------------------------------------
  system.stateVersion = "24.11";
}
