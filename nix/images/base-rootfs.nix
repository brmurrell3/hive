# Base rootfs image profile — minimal Alpine with hive-sidecar.
# This is the default image used when no image is specified in the agent manifest.
{ pkgs ? import <nixpkgs> {} }:

pkgs.vmTools.runInLinuxVM (pkgs.runCommand "hive-base-rootfs" {
  nativeBuildInputs = with pkgs; [ e2fsprogs util-linux ];
} ''
  # Create a minimal ext4 rootfs.
  truncate -s 512M $out/rootfs.ext4
  mkfs.ext4 -L hive-rootfs $out/rootfs.ext4

  mkdir -p /mnt
  mount $out/rootfs.ext4 /mnt

  # Install minimal base packages.
  mkdir -p /mnt/{bin,sbin,usr/bin,usr/sbin,etc,tmp,var,home,proc,sys,dev,run}
  mkdir -p /mnt/usr/lib

  # Copy busybox for basic shell utilities.
  cp ${pkgs.busybox}/bin/busybox /mnt/bin/busybox
  for cmd in sh ls cat cp mv rm mkdir chmod chown echo grep sed awk; do
    ln -s /bin/busybox /mnt/bin/$cmd
  done

  # Basic /etc files.
  echo "root:x:0:0:root:/root:/bin/sh" > /mnt/etc/passwd
  echo "root:x:0:" > /mnt/etc/group
  echo "hive-agent" > /mnt/etc/hostname

  umount /mnt
'')
