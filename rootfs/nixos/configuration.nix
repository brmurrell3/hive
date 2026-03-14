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
  # If the file does not exist the build aborts immediately — a silently
  # broken image without the sidecar is worse than a clear build failure.
  sidecarBin =
    if builtins.pathExists resolvedSidecarPath
    then
      pkgs.runCommand "hive-sidecar" {} ''
        install -Dm755 ${resolvedSidecarPath} $out/bin/hive-sidecar
      ''
    else
      builtins.abort ''
        hive-sidecar binary not found at '${resolvedSidecarPath}'.
        Compile it first:
          CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux-amd64/hive-sidecar ./cmd/hive-sidecar
        Or set HIVE_SIDECAR_BIN to point to an existing binary.
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
    firewall.enable = true;
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
    #
    # SEC-P3-004: The sidecar.conf file is written by the host via
    # os.WriteFile which is not atomic. A retry loop ensures the file is
    # fully written (non-empty and ends with a newline) before sourcing.
    script = ''
      AGENT_ID="unknown"
      TEAM_ID=""
      NATS_URL="nats://127.0.0.1:4222"
      NATS_TOKEN=""
      VSOCK_PORT="4222"

      # Wait for sidecar.conf to be fully written (retry up to 30 times, 1s apart).
      _conf="/agent/sidecar.conf"
      _retries=0
      while [ "$_retries" -lt 30 ]; do
        if [ -f "$_conf" ] && [ -s "$_conf" ]; then
          # Check that the file ends with a newline (complete write).
          if tail -c 1 "$_conf" | od -An -tx1 | grep -q '0a'; then
            break
          fi
        fi
        _retries=$((_retries + 1))
        echo "Waiting for $_conf to be fully written (attempt $_retries/30)..." >&2
        sleep 1
      done

      if [ -f "$_conf" ] && [ -s "$_conf" ]; then
        . "$_conf"
      else
        echo "WARNING: $_conf not found or empty after 30s, using defaults" >&2
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

      # Systemd hardening directives
      ProtectSystem = "strict";
      ProtectHome = true;
      PrivateTmp = true;
      NoNewPrivileges = true;
      ReadWritePaths = [ "/workspace" "/volumes" ];

      # Sidecar environment — values here are defaults; the control plane
      # can override them via the agent drive or environment file.
      Environment = [
        "HIVE_SIDECAR_MODE=standalone"
        "HIVE_SIDECAR_PORT=9100"
      ];
    };
  };

  # ---------------------------------------------------------------------------
  # Egress policy enforcement (iptables rules based on HIVE_EGRESS_MODE)
  # ---------------------------------------------------------------------------
  systemd.services.hive-egress-policy = {
    description = "Hive Egress Network Policy";
    after = [ "network.target" "agent.mount" ];
    wants = [ "agent.mount" ];
    wantedBy = [ "multi-user.target" ];
    before = [ "hive-sidecar.service" ];

    path = [ pkgs.iptables pkgs.iproute2 pkgs.dnsutils ];

    # SEC-P3-004: Wait for sidecar.conf to be fully written before sourcing.
    script = ''
      HIVE_EGRESS_MODE=""
      HIVE_EGRESS_ALLOWLIST=""
      HIVE_DNS_SERVER=""

      _conf="/agent/sidecar.conf"
      _retries=0
      while [ "$_retries" -lt 30 ]; do
        if [ -f "$_conf" ] && [ -s "$_conf" ]; then
          if tail -c 1 "$_conf" | od -An -tx1 | grep -q '0a'; then
            break
          fi
        fi
        _retries=$((_retries + 1))
        echo "Waiting for $_conf to be fully written (attempt $_retries/30)..." >&2
        sleep 1
      done

      if [ -f "$_conf" ] && [ -s "$_conf" ]; then
        . "$_conf"
      else
        echo "WARNING: $_conf not found or empty after 30s, using defaults" >&2
      fi

      # Configure DNS resolver
      if [ -n "''${HIVE_DNS_SERVER:-}" ]; then
          if echo "$HIVE_DNS_SERVER" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
              echo "nameserver $HIVE_DNS_SERVER" > /etc/resolv.conf
          else
              echo "ERROR: invalid DNS server IP: $HIVE_DNS_SERVER" >&2
          fi
      fi

      [ -z "''${HIVE_EGRESS_MODE:-}" ] && exit 0

      case "$HIVE_EGRESS_MODE" in
          none)
              # No network device attached, vsock only - restrict INPUT and OUTPUT
              echo "Network policy: egress=none (vsock only)"
              # Add ALLOW rules before setting policies to DROP
              iptables -A INPUT -i lo -j ACCEPT
              iptables -A INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
              iptables -A OUTPUT -o lo -j ACCEPT
              iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
              # Allow NATS communication (port 4222) on OUTPUT
              iptables -A OUTPUT -p tcp --dport 4222 -j ACCEPT
              # Now set all chain policies to DROP
              iptables -P INPUT DROP
              iptables -P FORWARD DROP
              iptables -P OUTPUT DROP
              ;;
          restricted)
              echo "Network policy: egress=restricted"
              iptables -P OUTPUT DROP
              iptables -P FORWARD DROP
              iptables -P INPUT DROP
              iptables -A INPUT -i lo -j ACCEPT
              iptables -A INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
              iptables -A OUTPUT -o lo -j ACCEPT

              # Allow DNS to gateway
              iptables -A OUTPUT -d 172.16.0.1 -p udp --dport 53 -j ACCEPT
              iptables -A OUTPUT -d 172.16.0.1 -p tcp --dport 53 -j ACCEPT

              # Allow established connections
              iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT

              # Parse and allow domains from HIVE_EGRESS_ALLOWLIST (JSON array)
              if [ -n "''${HIVE_EGRESS_ALLOWLIST:-}" ]; then
                  echo "$HIVE_EGRESS_ALLOWLIST" | tr -d '[]"' | tr ',' ' ' | tr ' ' '\n' | while read -r domain; do
                      [ -z "$domain" ] && continue
                      for ip in $(nslookup "$domain" 2>/dev/null | awk '/^Address: / { print $2 }'); do
                          if echo "$ip" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
                              iptables -A OUTPUT -d "$ip" -j ACCEPT
                          else
                              echo "WARNING: skipping invalid IP from nslookup: $ip" >&2
                          fi
                      done
                  done
              fi
              ;;
          full)
              echo "Network policy: egress=full (no restrictions)"
              ;;
      esac
    '';

    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
    };
  };

  # ---------------------------------------------------------------------------
  # Volume mount service (parses HIVE_VOLUMES from sidecar.conf)
  # ---------------------------------------------------------------------------
  systemd.services.hive-volumes = {
    description = "Hive Shared Volume Mounts";
    after = [ "agent.mount" ];
    wants = [ "agent.mount" ];
    wantedBy = [ "multi-user.target" ];
    before = [ "hive-sidecar.service" ];

    path = [ pkgs.util-linux pkgs.coreutils ];

    # SEC-P3-004: Wait for sidecar.conf to be fully written before sourcing.
    script = ''
      HIVE_VOLUMES=""

      _conf="/agent/sidecar.conf"
      _retries=0
      while [ "$_retries" -lt 30 ]; do
        if [ -f "$_conf" ] && [ -s "$_conf" ]; then
          if tail -c 1 "$_conf" | od -An -tx1 | grep -q '0a'; then
            break
          fi
        fi
        _retries=$((_retries + 1))
        echo "Waiting for $_conf to be fully written (attempt $_retries/30)..." >&2
        sleep 1
      done

      if [ -f "$_conf" ] && [ -s "$_conf" ]; then
        . "$_conf"
      else
        echo "WARNING: $_conf not found or empty after 30s, using defaults" >&2
      fi

      [ -z "''${HIVE_VOLUMES:-}" ] && exit 0

      OLD_IFS="$IFS"
      IFS='|'
      for volspec in $HIVE_VOLUMES; do
          IFS="$OLD_IFS"
          device=$(echo "$volspec" | cut -d: -f1)
          mountpoint=$(echo "$volspec" | cut -d: -f2)
          access=$(echo "$volspec" | cut -d: -f3)

          # Reject dangerous guest mount paths to prevent overwriting critical system directories
          case "$mountpoint" in
              /|/etc|/bin|/sbin|/usr|/lib|/dev|/proc|/sys|/boot|/run)
                  echo "ERROR: refusing to mount volume to dangerous path: $mountpoint" >&2
                  continue
                  ;;
              /etc/*|/bin/*|/sbin/*|/usr/*|/lib/*|/dev/*|/proc/*|/sys/*|/boot/*|/run/*)
                  echo "ERROR: refusing to mount volume under dangerous path: $mountpoint" >&2
                  continue
                  ;;
          esac

          # Validate device name matches expected virtio block device pattern
          case "$device" in
              /dev/vd[a-z]) ;;
              *)
                  echo "ERROR: invalid device name: $device (expected /dev/vd[a-z])" >&2
                  continue
                  ;;
          esac

          mkdir -p "$mountpoint"
          mount_opts="rw"
          if [ "$access" = "ro" ]; then
              mount_opts="ro"
          fi
          if ! mount -o "$mount_opts" "$device" "$mountpoint"; then
              echo "ERROR: failed to mount $device at $mountpoint" >&2
          fi
      done
      IFS="$OLD_IFS"
    '';

    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
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
    dnsutils     # for nslookup (egress allowlist resolution)
    iptables     # for egress policy enforcement
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
      # The build will abort if the sidecar binary is missing — see the
      # sidecarBin derivation in the let block above.
      install -Dm755 ${sidecarBin}/bin/hive-sidecar ./files/opt/hive/sidecar
    '';
  };

  # ---------------------------------------------------------------------------
  # System version
  # ---------------------------------------------------------------------------
  system.stateVersion = "24.11";
}
