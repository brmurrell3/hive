# NixOS configuration for Hive Firecracker VMs.
#
# This produces a minimal NixOS system image targeting Firecracker's
# constrained environment: virtio-mmio devices, serial console, vsock
# for host-guest communication, and a systemd service for the sidecar.
#
# Build:
#   nix build .#rootfs   -> result contains the ext4 rootfs image
#   nix build .#kernel   -> result contains vmlinux for Firecracker
#
# Sidecar binary requirement:
#   The sidecar binary must be compiled for x86_64-linux before building:
#
#     CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
#       go build -o /tmp/hive-sidecar ./cmd/hive-sidecar
#
#   Then pass the path via the HIVE_SIDECAR environment variable or by
#   placing it at <repo-root>/bin/linux-amd64/hive-sidecar (the default
#   picked up by the sidecarBin derivation below).
#
#   Alternatively, build the full rootfs via:
#     make -C rootfs/ rootfs       # builds sidecar then creates ext4 image
#
{ config, pkgs, lib, modulesPath, ... }:

let
  # Custom kernel with only the subsystems Firecracker needs.
  customKernel = import ./kernel.nix { inherit pkgs lib; };

  # ---------------------------------------------------------------------------
  # Sidecar binary
  #
  # We import the pre-compiled static Linux binary as a Nix derivation so it
  # participates in the image closure.  The binary must exist at
  # HIVE_SIDECAR_BIN before invoking `nix build`.
  #
  # Default search path (relative to this flake's root, two levels up):
  #   ../../bin/linux-amd64/hive-sidecar
  #
  # Override at build time:
  #   nix build .#rootfs \
  #     --override-input sidecarSrc path:/path/to/hive-sidecar
  # ---------------------------------------------------------------------------
  sidecarBinPath = builtins.getEnv "HIVE_SIDECAR_BIN";

  # Resolve: env var takes priority; fall back to conventional repo path.
  resolvedSidecarPath =
    if sidecarBinPath != ""
    then sidecarBinPath
    else toString (../../bin/linux-amd64/hive-sidecar);

  # Wrap the binary in a derivation so Nix can track it as a store path.
  # If the file does not exist yet the derivation is a stub that installs a
  # placeholder script that prints a clear error — this prevents a hard build
  # failure during early development when the binary hasn't been compiled yet.
  sidecarBin =
    if builtins.pathExists resolvedSidecarPath
    then
      pkgs.runCommand "hive-sidecar" {} ''
        install -Dm755 ${resolvedSidecarPath} $out/bin/hive-sidecar
      ''
    else
      pkgs.writeShellScriptBin "hive-sidecar" ''
        echo "ERROR: hive-sidecar binary was not found at build time." >&2
        echo "       Compile it first:" >&2
        echo "         CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux-amd64/hive-sidecar ./cmd/hive-sidecar" >&2
        exit 1
      '';
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
  # Mount the agent drive (vdb) containing agent files and sidecar config
  # ---------------------------------------------------------------------------
  fileSystems."/agent" = {
    device = "/dev/vdb";
    fsType = "ext4";
    options = [ "rw" ];
    neededForBoot = false;
  };

  # ---------------------------------------------------------------------------
  # Hive sidecar service
  # ---------------------------------------------------------------------------
  systemd.services.hive-sidecar = {
    description = "Hive Agent Sidecar";
    after = [ "network.target" "agent.mount" ];
    wants = [ "agent.mount" ];
    wantedBy = [ "multi-user.target" ];

    # Read sidecar.conf from the agent drive to get per-agent configuration.
    # The conf file contains KEY=VALUE pairs (AGENT_ID, TEAM_ID, NATS_URL,
    # NATS_TOKEN, VSOCK_PORT).
    script = ''
      AGENT_ID="unknown"
      TEAM_ID=""
      NATS_URL="nats://127.0.0.1:4222"
      NATS_TOKEN=""
      VSOCK_PORT="4222"

      if [ -f /agent/sidecar.conf ]; then
        . /agent/sidecar.conf
      fi

      # Build arguments safely using positional parameters to avoid eval injection.
      set -- --agent-id "$AGENT_ID" --team-id "$TEAM_ID" \
             --nats-url "$NATS_URL" \
             --workspace /agent --vsock --vsock-port "$VSOCK_PORT"

      if [ -n "''${RUNTIME_CMD:-}" ]; then
          set -- "$@" --runtime-cmd "''${RUNTIME_CMD}"
      fi

      if [ -n "''${RUNTIME_ARGS:-}" ]; then
          set -- "$@" --runtime-args "''${RUNTIME_ARGS}"
      fi

      if [ -n "''${CAPABILITIES:-}" ]; then
          set -- "$@" --capabilities "''${CAPABILITIES}"
      fi

      if [ -n "''${NATS_TOKEN:-}" ]; then
          set -- "$@" --nats-token "''${NATS_TOKEN}"
      fi

      exec /opt/hive/sidecar "$@"
    '';

    serviceConfig = {
      Type = "simple";
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
    sidecarBin   # Hive sidecar — installed to /opt/hive/sidecar via populateImageCommands
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

      # Install the sidecar binary at /opt/hive/sidecar.
      # The systemd service (hive-sidecar.service) exec's this path directly.
      # sidecarBin is either the real compiled binary or a stub that prints an
      # error — see the sidecarBin derivation in the let block above.
      install -Dm755 ${sidecarBin}/bin/hive-sidecar ./files/opt/hive/sidecar
    '';
  };

  # ---------------------------------------------------------------------------
  # System version
  # ---------------------------------------------------------------------------
  system.stateVersion = "24.11";
}
