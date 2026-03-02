# Python data science rootfs image profile.
# Includes Python 3, pip, and common libraries for data analysis.
{ pkgs ? import <nixpkgs> {} }:

let
  pythonEnv = pkgs.python312.withPackages (ps: with ps; [
    # Core data science.
    numpy
    pandas
    scipy
    matplotlib
    seaborn

    # Finance.
    yfinance
    requests

    # Utilities.
    beautifulsoup4
    jupyter
    ipython
  ]);
in
pkgs.vmTools.runInLinuxVM (pkgs.runCommand "hive-python-data-rootfs" {
  nativeBuildInputs = with pkgs; [ e2fsprogs util-linux ];
} ''
  # Create a larger rootfs for Python.
  truncate -s 2G $out/rootfs.ext4
  mkfs.ext4 -L hive-rootfs $out/rootfs.ext4

  mkdir -p /mnt
  mount $out/rootfs.ext4 /mnt

  # Install base + Python.
  mkdir -p /mnt/{bin,sbin,usr/bin,usr/sbin,etc,tmp,var,home,proc,sys,dev,run}

  cp ${pkgs.busybox}/bin/busybox /mnt/bin/busybox
  for cmd in sh ls cat cp mv rm mkdir chmod chown echo grep sed awk; do
    ln -s /bin/busybox /mnt/bin/$cmd
  done

  # Copy Python environment.
  cp -r ${pythonEnv}/* /mnt/usr/

  # Basic /etc files.
  echo "root:x:0:0:root:/root:/bin/sh" > /mnt/etc/passwd
  echo "root:x:0:" > /mnt/etc/group
  echo "hive-agent" > /mnt/etc/hostname

  umount /mnt
'')
