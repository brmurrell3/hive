#!/bin/bash
# Build a minimal Alpine Linux rootfs for Firecracker VMs.
# Usage: ./build-rootfs.sh <output_image> <size> <sidecar_binary>
set -euo pipefail

OUTPUT="${1:-rootfs.ext4}"
SIZE="${2:-512M}"
SIDECAR="${3:-hive-sidecar}"

WORKDIR=$(mktemp -d)
MOUNTDIR="$WORKDIR/mount"

cleanup() {
    if mountpoint -q "$MOUNTDIR" 2>/dev/null; then
        sudo umount "$MOUNTDIR"
    fi
    rm -rf "$WORKDIR"
}
trap cleanup EXIT

echo "==> Creating ext4 image ($SIZE)..."
truncate -s "$SIZE" "$WORKDIR/rootfs.ext4"
mkfs.ext4 -q "$WORKDIR/rootfs.ext4"

echo "==> Mounting..."
mkdir -p "$MOUNTDIR"
sudo mount -o loop "$WORKDIR/rootfs.ext4" "$MOUNTDIR"

echo "==> Installing Alpine base..."
if command -v docker &>/dev/null; then
    # Use Docker to extract Alpine rootfs
    CONTAINER_ID=$(docker create alpine:3.19 /bin/true)
    docker export "$CONTAINER_ID" | sudo tar -xf - -C "$MOUNTDIR"
    docker rm "$CONTAINER_ID" >/dev/null
else
    echo "Error: Docker is required to build the rootfs"
    exit 1
fi

echo "==> Installing sidecar binary..."
if [ -f "$SIDECAR" ]; then
    sudo cp "$SIDECAR" "$MOUNTDIR/usr/local/bin/hive-sidecar"
    sudo chmod 755 "$MOUNTDIR/usr/local/bin/hive-sidecar"
fi

echo "==> Installing overlay files..."
if [ -d "overlay" ]; then
    sudo cp -r overlay/* "$MOUNTDIR/" 2>/dev/null || true
fi

echo "==> Installing init script..."
sudo tee "$MOUNTDIR/init" > /dev/null << 'INITEOF'
#!/bin/sh
# Hive VM init script
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev

# Set hostname
hostname hive-vm

# Start sidecar as main process
exec /usr/local/bin/hive-sidecar
INITEOF
sudo chmod 755 "$MOUNTDIR/init"

echo "==> Creating workspace mount point..."
sudo mkdir -p "$MOUNTDIR/workspace"

echo "==> Unmounting..."
sudo umount "$MOUNTDIR"

echo "==> Moving image..."
mv "$WORKDIR/rootfs.ext4" "$OUTPUT"

echo "==> Done: $OUTPUT"
