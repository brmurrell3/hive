# Custom kernel configuration for Hive Firecracker VMs.
#
# This overrides the default NixOS kernel to produce a minimal vmlinux
# suitable for Firecracker's direct-boot mechanism. Unnecessary subsystems
# (wireless, sound, USB, Bluetooth, GPU) are disabled to reduce kernel
# size and boot time.
#
# Usage:
#   nix build .#kernel
#
# The resulting vmlinux can be found at:
#   ./result/vmlinux
{ pkgs, lib, ... }:

let
  inherit (lib.kernel) yes no module freeform;
in
pkgs.linuxPackages_latest.kernel.override {
  structuredExtraConfig = {

    # ---- Virtio (required by Firecracker) ----
    VIRTIO        = yes;
    VIRTIO_PCI    = yes;
    VIRTIO_MMIO   = yes;
    VIRTIO_BLK    = yes;
    VIRTIO_NET    = yes;
    VIRTIO_CONSOLE = yes;

    # ---- vsock (host-guest communication for NATS bridge) ----
    VSOCKETS                    = yes;
    VIRTIO_VSOCKETS             = yes;
    VIRTIO_VSOCKETS_COMMON      = yes;
    VHOST_VSOCK                 = yes;

    # ---- Serial console ----
    SERIAL_8250         = yes;
    SERIAL_8250_CONSOLE = yes;

    # ---- Filesystem ----
    EXT4_FS = yes;

    # ---- Networking (minimal) ----
    INET   = yes;
    IPV6   = yes;
    NETDEVICES = yes;

    # ---- Disable unnecessary subsystems to shrink the kernel ----
    WIRELESS          = no;
    WLAN              = no;
    SOUND             = no;
    SND               = no;
    USB_SUPPORT       = no;
    BLUETOOTH         = no;
    DRM               = no;
    FB                = no;
    INPUT_TOUCHSCREEN = no;
    INPUT_TABLET      = no;
    INPUT_JOYSTICK    = no;
    NFS_FS            = no;
    CIFS              = no;
    INFINIBAND        = no;
    PCCARD            = no;
    ATA               = no;
    SCSI              = no;
    FIREWIRE          = no;
    MEDIA_SUPPORT     = no;
    STAGING           = no;
  };
}
