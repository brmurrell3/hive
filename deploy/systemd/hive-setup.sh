#!/usr/bin/env bash
#
# hive-setup.sh — Create system user, directories, and install the systemd unit
# for the Hive control plane daemon.
#
# Usage:
#   sudo ./hive-setup.sh
#
# This script is idempotent: it can be run multiple times safely.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
UNIT_FILE="hived.service"
UNIT_SRC="${SCRIPT_DIR}/${UNIT_FILE}"
UNIT_DEST="/etc/systemd/system/${UNIT_FILE}"

HIVE_USER="hive"
HIVE_GROUP="hive"
HIVE_DATA_DIR="/var/lib/hive"
HIVE_LOG_DIR="/var/log/hive"

# --- helpers ----------------------------------------------------------------

log_info() {
    printf "[INFO]  %s\n" "$1"
}

log_ok() {
    printf "[OK]    %s\n" "$1"
}

log_error() {
    printf "[ERROR] %s\n" "$1" >&2
}

die() {
    log_error "$1"
    exit 1
}

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "This script must be run as root (use sudo)."
    fi
}

# --- main -------------------------------------------------------------------

require_root

# 1. Create system group
if getent group "${HIVE_GROUP}" >/dev/null 2>&1; then
    log_ok "Group '${HIVE_GROUP}' already exists."
else
    log_info "Creating system group '${HIVE_GROUP}'..."
    groupadd --system "${HIVE_GROUP}"
    log_ok "Group '${HIVE_GROUP}' created."
fi

# 2. Create system user
if id "${HIVE_USER}" >/dev/null 2>&1; then
    log_ok "User '${HIVE_USER}' already exists."
else
    log_info "Creating system user '${HIVE_USER}'..."
    useradd --system \
        --gid "${HIVE_GROUP}" \
        --home-dir "${HIVE_DATA_DIR}" \
        --no-create-home \
        --shell /usr/sbin/nologin \
        "${HIVE_USER}"
    log_ok "User '${HIVE_USER}' created."
fi

# 3. Create data directory
log_info "Ensuring data directory ${HIVE_DATA_DIR} exists..."
mkdir -p "${HIVE_DATA_DIR}"
mkdir -p "${HIVE_DATA_DIR}/rootfs"
mkdir -p "${HIVE_DATA_DIR}/state"
chown -R "${HIVE_USER}:${HIVE_GROUP}" "${HIVE_DATA_DIR}"
chmod 750 "${HIVE_DATA_DIR}"
log_ok "Data directory ${HIVE_DATA_DIR} ready."

# 4. Create log directory
log_info "Ensuring log directory ${HIVE_LOG_DIR} exists..."
mkdir -p "${HIVE_LOG_DIR}"
chown -R "${HIVE_USER}:${HIVE_GROUP}" "${HIVE_LOG_DIR}"
chmod 750 "${HIVE_LOG_DIR}"
log_ok "Log directory ${HIVE_LOG_DIR} ready."

# 5. Install the systemd unit file
if [ ! -f "${UNIT_SRC}" ]; then
    die "Unit file not found at ${UNIT_SRC}. Run this script from the deploy/systemd/ directory."
fi

log_info "Installing systemd unit file to ${UNIT_DEST}..."
cp "${UNIT_SRC}" "${UNIT_DEST}"
chmod 644 "${UNIT_DEST}"
log_ok "Unit file installed."

# 6. Reload systemd
log_info "Reloading systemd daemon..."
systemctl daemon-reload
log_ok "systemd reloaded."

# 7. Add hive user to kvm group if /dev/kvm exists (for Firecracker)
if [ -e /dev/kvm ]; then
    if getent group kvm >/dev/null 2>&1; then
        log_info "Adding '${HIVE_USER}' to 'kvm' group for Firecracker access..."
        usermod -aG kvm "${HIVE_USER}"
        log_ok "User '${HIVE_USER}' added to 'kvm' group."
    fi
fi

echo ""
echo "Setup complete. Next steps:"
echo "  1. Place hived binary at /usr/local/bin/hived"
echo "  2. Place cluster.yaml at ${HIVE_DATA_DIR}/cluster.yaml"
echo "  3. Place kernel and rootfs images in ${HIVE_DATA_DIR}/rootfs/"
echo "  4. Enable and start the service:"
echo "       sudo systemctl enable hived"
echo "       sudo systemctl start hived"
echo "  5. Check status:"
echo "       sudo systemctl status hived"
echo "       sudo journalctl -u hived -f"
