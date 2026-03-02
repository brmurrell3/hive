# Hive rootfs image profiles.
# Build with: nix build .#images.<profile>
#
# Available profiles:
#   - base     — Minimal Alpine-based rootfs with hive-sidecar
#   - python   — Base + Python 3, pip, and common data science libraries
{
  base = import ./base-rootfs.nix;
  python-data = import ./python-data.nix;
}
