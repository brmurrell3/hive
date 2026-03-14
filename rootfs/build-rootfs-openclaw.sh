#!/bin/bash
# Build an OpenClaw rootfs image with Node.js 20.
# Extends the base rootfs with Node.js for OpenClaw agent runtime.
#
# This script does NOT require root or loop-mount support.  It uses
# `mkfs.ext4 -d <dir>` (e2fsprogs >= 1.43) to create the ext4 image directly
# from a staging directory populated by Docker + local files.
#
# Usage: ./build-rootfs-openclaw.sh <output_image> <size> <sidecar_binary>
#
# Requirements:
#   - Docker       (to extract the Alpine base filesystem)
#   - mkfs.ext4    with -d flag support (e2fsprogs >= 1.43)
#                  On macOS: brew install e2fsprogs
#                  then add /opt/homebrew/opt/e2fsprogs/sbin to PATH
#
# The resulting ext4 image is a bootable Firecracker rootfs containing
# Node.js 20 LTS for running OpenClaw agents.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

OUTPUT="${1:-rootfs-openclaw.ext4}"
SIZE="${2:-1G}"
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
    [ -n "${CONTAINER_ID:-}" ] && docker rm "$CONTAINER_ID" >/dev/null 2>&1 || true
    rm -rf "$WORKDIR"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# 1. Extract Alpine base filesystem with Node.js 20 via Docker
# ---------------------------------------------------------------------------
echo "==> Extracting Alpine base filesystem with Node.js 20..."
if ! command -v docker &>/dev/null; then
    echo "Error: Docker is required to build the rootfs"
    exit 1
fi

mkdir -p "$STAGEDIR"

# Build a temporary image that includes Node.js 20 from Alpine repos.
# Alpine 3.19 ships nodejs-20 in its main repository.
CONTAINER_ID=$(docker create --platform "linux/${GOARCH:-$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')}" alpine:3.19 /bin/sh -c "
    apk add --no-cache bind-tools nodejs npm iptables iproute2
")
if ! docker start -a "$CONTAINER_ID"; then
    echo "Error: docker start failed for container $CONTAINER_ID" >&2
    exit 1
fi
docker export "$CONTAINER_ID" | tar -xf - -C "$STAGEDIR"
docker rm "$CONTAINER_ID" >/dev/null
CONTAINER_ID=""

# Verify Node.js was installed
if [ ! -f "$STAGEDIR/usr/bin/node" ]; then
    echo "Warning: Node.js binary not found in image — trying alternative install method..."
    # Fallback: use a multi-stage approach with explicit apk in chroot-like fashion
    CONTAINER_ID=$(docker create --platform "linux/${GOARCH:-$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')}" alpine:3.19 /bin/true)
    docker export "$CONTAINER_ID" | tar -xf - -C "$STAGEDIR"
    docker rm "$CONTAINER_ID" >/dev/null
    CONTAINER_ID=""

    # Install Node.js using a Docker run with bind mount
    docker run --rm --platform "linux/${GOARCH:-$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')}" \
        -v "$STAGEDIR:/rootfs" alpine:3.19 \
        /bin/sh -c "apk add --no-cache --root /rootfs --initdb bind-tools nodejs npm iptables iproute2"
fi

# Strip unnecessary files to keep image size down
echo "==> Stripping unnecessary files to reduce image size..."
rm -rf "$STAGEDIR/usr/share/man" \
       "$STAGEDIR/usr/share/doc" \
       "$STAGEDIR/usr/share/info" \
       "$STAGEDIR/var/cache/apk"/*

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
if [ -d "$SCRIPT_DIR/overlay" ]; then
    cp -r "$SCRIPT_DIR/overlay/." "$STAGEDIR/" 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# 4. Write the init script (same as base rootfs)
# ---------------------------------------------------------------------------
echo "==> Installing init script..."
cat > "$STAGEDIR/init" << 'INITEOF'
#!/bin/sh
# Hive VM init script (OpenClaw variant)
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev

# Bring up loopback interface (required for sidecar VsockProxy on 127.0.0.1)
ip link set lo up

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

# Configure DNS resolver
if [ -n "${HIVE_DNS_SERVER:-}" ]; then
    if echo "$HIVE_DNS_SERVER" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
        echo "nameserver $HIVE_DNS_SERVER" > /etc/resolv.conf
    else
        echo "ERROR: invalid DNS server IP: $HIVE_DNS_SERVER" >&2
    fi
fi

# Network policy enforcement
if [ -n "${HIVE_EGRESS_MODE:-}" ]; then
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
            # Configure network interface if present
            if ip link show eth0 >/dev/null 2>&1; then
                ip addr add 172.16.0.2/24 dev eth0
                ip link set eth0 up
                ip route add default via 172.16.0.1

                # Set up iptables for restricted egress
                iptables -P OUTPUT DROP
                iptables -P FORWARD DROP
                iptables -P INPUT DROP
                iptables -A INPUT -i lo -j ACCEPT
                iptables -A INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT

                # Allow loopback
                iptables -A OUTPUT -o lo -j ACCEPT

                # Allow DNS to gateway
                iptables -A OUTPUT -d 172.16.0.1 -p udp --dport 53 -j ACCEPT
                iptables -A OUTPUT -d 172.16.0.1 -p tcp --dport 53 -j ACCEPT

                # Allow established connections
                iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT

                # Parse and allow domains from HIVE_EGRESS_ALLOWLIST (JSON array)
                if [ -n "${HIVE_EGRESS_ALLOWLIST:-}" ]; then
                    echo "$HIVE_EGRESS_ALLOWLIST" | tr -d '[]"' | tr ',' ' ' | tr ' ' '\n' | while read -r domain; do
                        [ -z "$domain" ] && continue
                        # Resolve domain and add iptables rules for each IP
                        for ip in $(nslookup "$domain" 2>/dev/null | awk '/^Address: / { print $2 }'); do
                            if echo "$ip" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
                                iptables -A OUTPUT -d "$ip" -j ACCEPT
                            else
                                echo "WARNING: skipping invalid IP from nslookup: $ip" >&2
                            fi
                        done
                    done
                fi
            fi
            ;;
        full)
            echo "Network policy: egress=full"
            # Configure network interface if present
            if ip link show eth0 >/dev/null 2>&1; then
                ip addr add 172.16.0.2/24 dev eth0
                ip link set eth0 up
                ip route add default via 172.16.0.1
            fi
            ;;
    esac
fi

# Mount shared volumes (virtiofs-via-block-device)
# HIVE_VOLUMES is a pipe-delimited list of device:mount_path:access specs.
# Shared volume drives appear as /dev/vdc, /dev/vdd, etc.
# (after vda=rootfs and vdb=agent drive).
if [ -n "${HIVE_VOLUMES:-}" ]; then
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
fi

# Build sidecar arguments safely using positional parameters
set -- --agent-id "${AGENT_ID:-unknown}" --team-id "${TEAM_ID:-}" \
       --nats-url "${NATS_URL:-nats://127.0.0.1:4222}" \
       --workspace /agent --vsock --vsock-port "${VSOCK_PORT:-4222}"
[ -n "${NATS_TOKEN:-}" ] && set -- "$@" --nats-token "${NATS_TOKEN}"
[ -n "${RUNTIME_CMD:-}" ] && set -- "$@" --runtime-cmd "${RUNTIME_CMD}"
[ -n "${CAPABILITIES:-}" ] && set -- "$@" --capabilities "${CAPABILITIES}"
exec /usr/local/bin/hive-sidecar "$@"
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
truncate -s "$SIZE" "$WORKDIR/rootfs.ext4"
"$MKE2FS" -t ext4 -q -d "$STAGEDIR" "$WORKDIR/rootfs.ext4"

# ---------------------------------------------------------------------------
# 7. Move the finished image into place
# ---------------------------------------------------------------------------
echo "==> Moving image to $OUTPUT..."
mv "$WORKDIR/rootfs.ext4" "$OUTPUT"

echo "==> Done: $OUTPUT (OpenClaw variant with Node.js)"
