#!/bin/bash
# Build a minimal Alpine Linux rootfs for Firecracker VMs.
#
# This script does NOT require root or loop-mount support.  It uses
# `mkfs.ext4 -d <dir>` (e2fsprogs >= 1.43) to create the ext4 image directly
# from a staging directory populated by Docker + local files.
#
# Usage: ./build-rootfs.sh <output_image> <size> <sidecar_binary>
#
# Requirements:
#   - Docker       (to extract the Alpine base filesystem)
#   - mkfs.ext4    with -d flag support (e2fsprogs >= 1.43)
#                  On macOS: brew install e2fsprogs
#                  then add /opt/homebrew/opt/e2fsprogs/sbin to PATH
#
# The resulting ext4 image is a bootable Firecracker rootfs.  Pair it with a
# vmlinux kernel (see `make download-kernel`) to launch a VM.
set -euo pipefail

OUTPUT="${1:-rootfs.ext4}"
SIZE="${2:-512M}"
SIDECAR="${3:-hive-sidecar}"

# ---------------------------------------------------------------------------
# Locate mkfs.ext4 — on macOS brew installs it under a versioned prefix
# ---------------------------------------------------------------------------
MKE2FS=""
for candidate in \
    mkfs.ext4 \
    /opt/homebrew/opt/e2fsprogs/sbin/mkfs.ext4 \
    /usr/local/opt/e2fsprogs/sbin/mkfs.ext4; do
    if command -v "$candidate" &>/dev/null; then
        MKE2FS="$candidate"
        break
    fi
done

if [ -z "$MKE2FS" ]; then
    echo "Error: mkfs.ext4 not found."
    echo "  On Linux: install e2fsprogs (>= 1.43)"
    echo "  On macOS: brew install e2fsprogs"
    exit 1
fi

# Verify that -d flag is supported (e2fsprogs >= 1.43)
if ! "$MKE2FS" --help 2>&1 | grep -q -- '-d'; then
    echo "Error: mkfs.ext4 at '$MKE2FS' does not support the -d flag."
    echo "  Upgrade e2fsprogs to >= 1.43."
    exit 1
fi

WORKDIR=$(mktemp -d)
STAGEDIR="$WORKDIR/rootfs"

cleanup() {
    rm -rf "$WORKDIR"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# 1. Extract Alpine base filesystem via Docker (no root needed)
# ---------------------------------------------------------------------------
echo "==> Extracting Alpine base filesystem..."
if ! command -v docker &>/dev/null; then
    echo "Error: Docker is required to build the rootfs"
    exit 1
fi

mkdir -p "$STAGEDIR"
CONTAINER_ID=$(docker create alpine:3.19 /bin/true)
docker export "$CONTAINER_ID" | tar -xf - -C "$STAGEDIR"
docker rm "$CONTAINER_ID" >/dev/null

# ---------------------------------------------------------------------------
# 2. Install the sidecar binary
# ---------------------------------------------------------------------------
echo "==> Installing sidecar binary..."
if [ -f "$SIDECAR" ]; then
    install -m 755 "$SIDECAR" "$STAGEDIR/usr/local/bin/hive-sidecar"
else
    echo "Warning: sidecar binary '$SIDECAR' not found — image will lack the sidecar"
fi

# ---------------------------------------------------------------------------
# 3. Apply overlay files (optional)
# ---------------------------------------------------------------------------
echo "==> Applying overlay files..."
if [ -d "overlay" ]; then
    cp -r overlay/. "$STAGEDIR/" 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# 4. Write the init script
# ---------------------------------------------------------------------------
echo "==> Installing init script..."
cat > "$STAGEDIR/init" << 'INITEOF'
#!/bin/sh
# Hive VM init script
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev

# Set hostname
hostname hive-vm

# Mount agent drive (vdb) which contains AGENTS.md, SOUL.md, sidecar.conf, etc.
mkdir -p /agent
if [ -b /dev/vdb ]; then
    mount -t ext4 /dev/vdb /agent
else
    echo "Warning: /dev/vdb not present — agent drive not mounted"
fi

# Provide writable scratch space for agent processes
mount -t tmpfs tmpfs /tmp
mount -t tmpfs tmpfs /var
mkdir -p /var/log /var/tmp

# Source agent config (AGENT_ID, TEAM_ID, NATS_URL, NATS_TOKEN)
if [ -f /agent/sidecar.conf ]; then
    . /agent/sidecar.conf
fi

# Build sidecar arguments
SIDECAR_ARGS="--agent-id ${AGENT_ID:-unknown} --team-id ${TEAM_ID:-} --nats-url ${NATS_URL:-nats://127.0.0.1:4222} --nats-token ${NATS_TOKEN} --workspace /agent --vsock --vsock-port ${VSOCK_PORT:-4222}"

# Pass runtime command if set (starts the agent workload process)
if [ -n "${RUNTIME_CMD:-}" ]; then
    SIDECAR_ARGS="${SIDECAR_ARGS} --runtime-cmd ${RUNTIME_CMD}"
fi

# Pass runtime arguments if set
if [ -n "${RUNTIME_ARGS:-}" ]; then
    SIDECAR_ARGS="${SIDECAR_ARGS} --runtime-args ${RUNTIME_ARGS}"
fi

# Pass capabilities JSON if set (registers with capability router)
if [ -n "${CAPABILITIES:-}" ]; then
    SIDECAR_ARGS="${SIDECAR_ARGS} --capabilities '${CAPABILITIES}'"
fi

# Start sidecar with per-agent config from the agent drive
exec /usr/local/bin/hive-sidecar ${SIDECAR_ARGS}
INITEOF
chmod 755 "$STAGEDIR/init"

# ---------------------------------------------------------------------------
# 5. Ensure the agent mount point exists
# ---------------------------------------------------------------------------
mkdir -p "$STAGEDIR/agent"

# ---------------------------------------------------------------------------
# 6. Create the ext4 image using mkfs.ext4 -d (no loop mount, no root)
# ---------------------------------------------------------------------------
echo "==> Creating ext4 image ($SIZE) from staging directory..."
# truncate creates the raw file at the target size; mkfs.ext4 -d then formats
# it and populates it from the staging directory in one pass.
truncate -s "$SIZE" "$WORKDIR/rootfs.ext4"
"$MKE2FS" -t ext4 -q -d "$STAGEDIR" "$WORKDIR/rootfs.ext4"

# ---------------------------------------------------------------------------
# 7. Move the finished image into place
# ---------------------------------------------------------------------------
echo "==> Moving image to $OUTPUT..."
mv "$WORKDIR/rootfs.ext4" "$OUTPUT"

echo "==> Done: $OUTPUT"
