#!/usr/bin/env bash
#
# install.sh — Install the Hive control plane and agent binaries on a Linux host.
#
# Supported platforms:
#   - Ubuntu 22.04, Ubuntu 24.04, Debian 12
#   - Architectures: amd64 (x86_64), arm64 (aarch64)
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/brmurrell3/hive/main/scripts/install.sh | sudo bash
#   # or:
#   sudo ./scripts/install.sh [OPTIONS]
#
# Options:
#   --version VERSION   Install a specific release version (e.g., v1.0.0).
#                        Default: latest release.
#   --dry-run           Show what would be done without making changes.
#   --yes               Skip all confirmation prompts (non-interactive mode).
#   --skip-images       Skip downloading kernel and rootfs images.
#   --help              Show this help message.
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

GITHUB_REPO="brmurrell3/hive"
GITHUB_API="https://api.github.com"
GITHUB_RELEASES="https://github.com/${GITHUB_REPO}/releases"

INSTALL_DIR="/usr/local/bin"
HIVE_DATA_DIR="/var/lib/hive"
HIVE_LOG_DIR="/var/log/hive"
HIVE_ROOTFS_DIR="${HIVE_DATA_DIR}/rootfs"
HIVE_STATE_DIR="${HIVE_DATA_DIR}/state"

HIVE_USER="hive"
HIVE_GROUP="hive"

SYSTEMD_UNIT_DIR="/etc/systemd/system"
SYSTEMD_UNIT_FILE="hived.service"

BINARIES="hived hivectl hive-agent hive-sidecar"

FIRECRACKER_KERNEL_URL_AMD64="https://github.com/firecracker-microvm/firecracker/releases/download/v1.6.0/vmlinux-5.10-x86_64.bin"
FIRECRACKER_KERNEL_URL_ARM64="https://github.com/firecracker-microvm/firecracker/releases/download/v1.6.0/vmlinux-5.10-aarch64.bin"

# Minimum supported versions for dependency checks.
MIN_NFTABLES_VERSION="1.0"

# ---------------------------------------------------------------------------
# Global state
# ---------------------------------------------------------------------------

VERSION=""
DRY_RUN=false
YES=false
SKIP_IMAGES=false
DETECTED_OS=""
DETECTED_ARCH=""
DOWNLOAD_URL_BASE=""
TMPDIR_INSTALL=""

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log_info() {
    printf "\033[0;34m[INFO]\033[0m  %s\n" "$1"
}

log_ok() {
    printf "\033[0;32m[OK]\033[0m    %s\n" "$1"
}

log_warn() {
    printf "\033[0;33m[WARN]\033[0m  %s\n" "$1"
}

log_error() {
    printf "\033[0;31m[ERROR]\033[0m %s\n" "$1" >&2
}

log_step() {
    printf "\n\033[1;36m==> %s\033[0m\n" "$1"
}

die() {
    log_error "$1"
    exit 1
}

confirm() {
    if [ "${YES}" = true ]; then
        return 0
    fi
    local prompt="$1"
    printf "%s [y/N] " "${prompt}"
    read -r answer
    case "${answer}" in
        [yY]|[yY][eE][sS]) return 0 ;;
        *) return 1 ;;
    esac
}

cleanup() {
    if [ -n "${TMPDIR_INSTALL:-}" ] && [ -d "${TMPDIR_INSTALL}" ]; then
        rm -rf "${TMPDIR_INSTALL}"
    fi
}
trap cleanup EXIT INT TERM

usage() {
    cat <<'USAGE'
Usage: install.sh [OPTIONS]

Install the Hive control plane and agent binaries.

Options:
  --version VERSION   Install a specific release version (e.g., v1.0.0).
                       Default: latest release.
  --dry-run           Show what would be done without making changes.
  --yes               Skip all confirmation prompts (non-interactive mode).
  --skip-images       Skip downloading kernel and rootfs images.
  --help              Show this help message.

Examples:
  # Install the latest release interactively:
  sudo ./scripts/install.sh

  # Install a specific version non-interactively:
  sudo ./scripts/install.sh --version v1.0.0 --yes

  # Dry-run to preview changes:
  sudo ./scripts/install.sh --dry-run
USAGE
    exit 0
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --version)
                if [ -z "${2:-}" ]; then
                    die "--version requires a value (e.g., --version v1.0.0)."
                fi
                VERSION="$2"
                shift 2
                ;;
            --dry-run)
                DRY_RUN=true
                shift
                ;;
            --yes|-y)
                YES=true
                shift
                ;;
            --skip-images)
                SKIP_IMAGES=true
                shift
                ;;
            --help|-h)
                usage
                ;;
            *)
                die "Unknown option: $1. Use --help for usage."
                ;;
        esac
    done
}

# ---------------------------------------------------------------------------
# Pre-flight: OS & architecture detection
# ---------------------------------------------------------------------------

detect_os() {
    if [ ! -f /etc/os-release ]; then
        die "Cannot detect OS: /etc/os-release not found. Only Ubuntu 22.04/24.04 and Debian 12 are supported."
    fi

    # shellcheck source=/dev/null
    . /etc/os-release

    local os_id="${ID:-unknown}"
    local os_version="${VERSION_ID:-unknown}"

    case "${os_id}" in
        ubuntu)
            case "${os_version}" in
                22.04|24.04)
                    DETECTED_OS="ubuntu-${os_version}"
                    ;;
                *)
                    log_warn "Ubuntu ${os_version} is not officially tested. Supported: 22.04, 24.04."
                    DETECTED_OS="ubuntu-${os_version}"
                    ;;
            esac
            ;;
        debian)
            case "${os_version}" in
                12)
                    DETECTED_OS="debian-${os_version}"
                    ;;
                *)
                    log_warn "Debian ${os_version} is not officially tested. Supported: 12."
                    DETECTED_OS="debian-${os_version}"
                    ;;
            esac
            ;;
        *)
            die "Unsupported OS: ${os_id}. Only Ubuntu (22.04/24.04) and Debian (12) are supported."
            ;;
    esac

    log_ok "Detected OS: ${DETECTED_OS} (${PRETTY_NAME:-${os_id}})"
}

detect_arch() {
    local machine
    machine="$(uname -m)"

    case "${machine}" in
        x86_64|amd64)
            DETECTED_ARCH="amd64"
            ;;
        aarch64|arm64)
            DETECTED_ARCH="arm64"
            ;;
        *)
            die "Unsupported architecture: ${machine}. Only amd64 and arm64 are supported."
            ;;
    esac

    log_ok "Detected architecture: ${DETECTED_ARCH}"
}

# ---------------------------------------------------------------------------
# Pre-flight: dependency and capability checks
# ---------------------------------------------------------------------------

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "This script must be run as root (use sudo)."
    fi
}

check_command() {
    local cmd="$1"
    local desc="$2"
    local required="${3:-true}"

    if command -v "${cmd}" >/dev/null 2>&1; then
        log_ok "${desc}: $(command -v "${cmd}")"
        return 0
    else
        if [ "${required}" = "true" ]; then
            log_error "${desc}: '${cmd}' not found. Please install it first."
            return 1
        else
            log_warn "${desc}: '${cmd}' not found (optional)."
            return 1
        fi
    fi
}

preflight_checks() {
    log_step "Running pre-flight checks"

    local failures=0

    # Required tools
    check_command curl "HTTP client (curl)" true || failures=$((failures + 1))
    check_command systemctl "systemd (systemctl)" true || failures=$((failures + 1))
    check_command tar "Archive tool (tar)" true || failures=$((failures + 1))

    # KVM access
    if [ -e /dev/kvm ]; then
        if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
            log_ok "KVM access: /dev/kvm is accessible."
        else
            log_warn "KVM access: /dev/kvm exists but is not readable/writable by root."
            log_warn "  Firecracker VMs may not start. Check permissions or load the kvm module."
        fi
    else
        log_warn "KVM access: /dev/kvm not found."
        log_warn "  Firecracker VMs require KVM. Ensure the kvm kernel module is loaded:"
        log_warn "    sudo modprobe kvm"
        log_warn "    sudo modprobe kvm_intel  # or kvm_amd"
    fi

    # nftables
    if check_command nft "Firewall (nft/nftables)" false; then
        local nft_version
        nft_version="$(nft --version 2>/dev/null | head -1 || echo "unknown")"
        log_info "nft version: ${nft_version}"
    else
        log_warn "nftables (nft) is required for VM network isolation."
        log_warn "  Install with: sudo apt-get install nftables"
    fi

    # Docker (optional, for rootfs builds)
    if check_command docker "Docker (for rootfs builds)" false; then
        if docker info >/dev/null 2>&1; then
            log_ok "Docker daemon is running."
        else
            log_warn "Docker is installed but the daemon is not running or not accessible."
        fi
    else
        log_info "Docker is optional. It is needed only if you want to build rootfs images locally."
    fi

    if [ "${failures}" -gt 0 ]; then
        die "Pre-flight checks failed. Please resolve the ${failures} error(s) above."
    fi

    log_ok "All required pre-flight checks passed."
}

# ---------------------------------------------------------------------------
# Version resolution
# ---------------------------------------------------------------------------

resolve_version() {
    if [ -n "${VERSION}" ]; then
        # Ensure version has a 'v' prefix for consistency.
        case "${VERSION}" in
            v*) ;; # already has prefix
            *)  VERSION="v${VERSION}" ;;
        esac
        log_info "Using specified version: ${VERSION}"
        return
    fi

    log_info "Resolving latest release version..."

    local api_url="${GITHUB_API}/repos/${GITHUB_REPO}/releases/latest"
    local response
    if ! response="$(curl -fsSL --retry 3 --retry-delay 2 "${api_url}" 2>/dev/null)"; then
        die "Failed to fetch latest release from ${api_url}. Check your network connection."
    fi

    # Parse tag_name from JSON without requiring jq.
    VERSION="$(printf '%s' "${response}" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"

    if [ -z "${VERSION}" ]; then
        die "Could not determine latest release version. Use --version to specify one manually."
    fi

    log_ok "Latest release version: ${VERSION}"
}

# ---------------------------------------------------------------------------
# Download helpers
# ---------------------------------------------------------------------------

download_file() {
    local url="$1"
    local dest="$2"
    local desc="${3:-file}"

    log_info "Downloading ${desc}..."
    log_info "  URL:  ${url}"
    log_info "  Dest: ${dest}"

    if [ "${DRY_RUN}" = true ]; then
        log_info "[DRY-RUN] Would download ${url} to ${dest}"
        return 0
    fi

    if ! curl -fSL --retry 3 --retry-delay 2 --progress-bar -o "${dest}" "${url}"; then
        die "Failed to download ${desc} from ${url}."
    fi

    log_ok "Downloaded ${desc}."
}

# ---------------------------------------------------------------------------
# Installation steps
# ---------------------------------------------------------------------------

create_user_and_group() {
    log_step "Creating system user and group"

    if [ "${DRY_RUN}" = true ]; then
        log_info "[DRY-RUN] Would create system group '${HIVE_GROUP}'."
        log_info "[DRY-RUN] Would create system user '${HIVE_USER}'."
        return
    fi

    if getent group "${HIVE_GROUP}" >/dev/null 2>&1; then
        log_ok "Group '${HIVE_GROUP}' already exists."
    else
        groupadd --system "${HIVE_GROUP}"
        log_ok "Created system group '${HIVE_GROUP}'."
    fi

    if id "${HIVE_USER}" >/dev/null 2>&1; then
        log_ok "User '${HIVE_USER}' already exists."
    else
        useradd --system \
            --gid "${HIVE_GROUP}" \
            --home-dir "${HIVE_DATA_DIR}" \
            --no-create-home \
            --shell /usr/sbin/nologin \
            "${HIVE_USER}"
        log_ok "Created system user '${HIVE_USER}'."
    fi

    # Add to kvm group if available (for Firecracker).
    if [ -e /dev/kvm ] && getent group kvm >/dev/null 2>&1; then
        usermod -aG kvm "${HIVE_USER}" 2>/dev/null || true
        log_ok "Added '${HIVE_USER}' to 'kvm' group."
    fi
}

create_directories() {
    log_step "Creating directory structure"

    local dirs=(
        "${HIVE_DATA_DIR}"
        "${HIVE_ROOTFS_DIR}"
        "${HIVE_STATE_DIR}"
        "${HIVE_LOG_DIR}"
    )

    for dir in "${dirs[@]}"; do
        if [ "${DRY_RUN}" = true ]; then
            log_info "[DRY-RUN] Would create directory: ${dir}"
        else
            mkdir -p "${dir}"
            chown "${HIVE_USER}:${HIVE_GROUP}" "${dir}"
            chmod 750 "${dir}"
            log_ok "Directory ready: ${dir}"
        fi
    done
}

download_binaries() {
    log_step "Downloading Hive binaries (${VERSION}, linux/${DETECTED_ARCH})"

    TMPDIR_INSTALL="$(mktemp -d)"

    DOWNLOAD_URL_BASE="${GITHUB_RELEASES}/download/${VERSION}"

    for binary in ${BINARIES}; do
        local url="${DOWNLOAD_URL_BASE}/${binary}-linux-${DETECTED_ARCH}"
        local dest="${TMPDIR_INSTALL}/${binary}"

        download_file "${url}" "${dest}" "${binary}"
    done

    # Also try downloading a checksum file if available.
    local checksum_url="${DOWNLOAD_URL_BASE}/checksums.txt"
    local checksum_file="${TMPDIR_INSTALL}/checksums.txt"
    if curl -fsSL --retry 1 -o "${checksum_file}" "${checksum_url}" 2>/dev/null; then
        log_info "Verifying checksums..."
        local verify_failures=0
        for binary in ${BINARIES}; do
            local expected
            expected="$(grep "${binary}-linux-${DETECTED_ARCH}" "${checksum_file}" 2>/dev/null | awk '{print $1}' || true)"
            if [ -n "${expected}" ] && [ "${DRY_RUN}" != true ]; then
                local actual
                actual="$(sha256sum "${TMPDIR_INSTALL}/${binary}" | awk '{print $1}')"
                if [ "${expected}" = "${actual}" ]; then
                    log_ok "Checksum verified: ${binary}"
                else
                    log_error "Checksum mismatch for ${binary}: expected ${expected}, got ${actual}"
                    verify_failures=$((verify_failures + 1))
                fi
            fi
        done
        if [ "${verify_failures}" -gt 0 ]; then
            die "Checksum verification failed for ${verify_failures} binary(ies). Aborting."
        fi
    else
        log_warn "No checksums.txt found for this release. Skipping checksum verification."
    fi
}

install_binaries() {
    log_step "Installing binaries to ${INSTALL_DIR}"

    for binary in ${BINARIES}; do
        local src="${TMPDIR_INSTALL}/${binary}"
        local dest="${INSTALL_DIR}/${binary}"

        if [ "${DRY_RUN}" = true ]; then
            log_info "[DRY-RUN] Would install ${binary} to ${dest}"
        else
            if [ -f "${src}" ]; then
                install -m 0755 "${src}" "${dest}"
                log_ok "Installed ${binary} -> ${dest}"
            else
                log_warn "Binary not found at ${src}; skipping."
            fi
        fi
    done

    # Verify installed versions.
    if [ "${DRY_RUN}" != true ]; then
        for binary in hived hivectl; do
            local installed_version
            installed_version="$("${INSTALL_DIR}/${binary}" --version 2>/dev/null || echo "unknown")"
            log_info "${binary} version: ${installed_version}"
        done
    fi
}

download_images() {
    if [ "${SKIP_IMAGES}" = true ]; then
        log_step "Skipping kernel and rootfs image download (--skip-images)"
        return
    fi

    log_step "Downloading kernel and rootfs images"

    # Select kernel URL based on architecture.
    local kernel_url
    case "${DETECTED_ARCH}" in
        amd64) kernel_url="${FIRECRACKER_KERNEL_URL_AMD64}" ;;
        arm64) kernel_url="${FIRECRACKER_KERNEL_URL_ARM64}" ;;
    esac

    local kernel_dest="${HIVE_ROOTFS_DIR}/vmlinux"
    if [ -f "${kernel_dest}" ]; then
        log_ok "Kernel image already exists at ${kernel_dest}. Skipping."
    else
        download_file "${kernel_url}" "${kernel_dest}" "Firecracker kernel (vmlinux)"
        if [ "${DRY_RUN}" != true ]; then
            chown "${HIVE_USER}:${HIVE_GROUP}" "${kernel_dest}"
            chmod 644 "${kernel_dest}"
        fi
    fi

    # Download rootfs image from the Hive release.
    local rootfs_url="${DOWNLOAD_URL_BASE}/rootfs-${DETECTED_ARCH}.ext4"
    local rootfs_dest="${HIVE_ROOTFS_DIR}/rootfs.ext4"
    if [ -f "${rootfs_dest}" ]; then
        log_ok "Rootfs image already exists at ${rootfs_dest}. Skipping."
    else
        log_info "Downloading rootfs image..."
        log_info "  URL:  ${rootfs_url}"
        log_info "  Dest: ${rootfs_dest}"
        if [ "${DRY_RUN}" = true ]; then
            log_info "[DRY-RUN] Would download ${rootfs_url} to ${rootfs_dest}"
        elif curl -fSL --retry 3 --retry-delay 2 --progress-bar -o "${rootfs_dest}" "${rootfs_url}" 2>/dev/null; then
            chown "${HIVE_USER}:${HIVE_GROUP}" "${rootfs_dest}"
            chmod 644 "${rootfs_dest}"
            log_ok "Downloaded rootfs image."
        else
            rm -f "${rootfs_dest}"
            log_warn "Pre-built rootfs image not available in release ${VERSION}."
            log_warn "You can build it locally with: cd /path/to/hive && make rootfs"
            log_warn "Then copy rootfs/rootfs.ext4 to ${rootfs_dest}"
        fi
    fi
}

install_systemd_unit() {
    log_step "Installing systemd unit file"

    local unit_dest="${SYSTEMD_UNIT_DIR}/${SYSTEMD_UNIT_FILE}"

    if [ "${DRY_RUN}" = true ]; then
        log_info "[DRY-RUN] Would install systemd unit to ${unit_dest}"
        log_info "[DRY-RUN] Would run systemctl daemon-reload"
        return
    fi

    # Generate the unit file inline so the install script is self-contained.
    cat > "${unit_dest}" <<'UNIT'
[Unit]
Description=Hive Control Plane Daemon
After=network-online.target
Wants=network-online.target
Documentation=https://github.com/brmurrell3/hive

[Service]
Type=notify
User=hive
Group=hive
ExecStart=/usr/local/bin/hived --cluster-root /var/lib/hive --log-level info
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
AmbientCapabilities=CAP_NET_ADMIN CAP_SYS_ADMIN
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/hive /var/log/hive
ProtectHome=true
PrivateTmp=true
Environment=HIVE_CLUSTER_ROOT=/var/lib/hive

[Install]
WantedBy=multi-user.target
UNIT

    chmod 644 "${unit_dest}"
    log_ok "Unit file installed to ${unit_dest}."

    systemctl daemon-reload
    log_ok "systemd daemon reloaded."
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

print_summary() {
    echo ""
    echo "============================================================"
    if [ "${DRY_RUN}" = true ]; then
        echo "  Hive Installation - DRY RUN COMPLETE"
    else
        echo "  Hive Installation Complete"
    fi
    echo "============================================================"
    echo ""
    echo "  Version:      ${VERSION}"
    echo "  OS:           ${DETECTED_OS}"
    echo "  Architecture: ${DETECTED_ARCH}"
    echo "  Binaries:     ${INSTALL_DIR}/{hived,hivectl,hive-agent,hive-sidecar}"
    echo "  Data dir:     ${HIVE_DATA_DIR}"
    echo "  Log dir:      ${HIVE_LOG_DIR}"
    echo "  Rootfs dir:   ${HIVE_ROOTFS_DIR}"
    echo "  Unit file:    ${SYSTEMD_UNIT_DIR}/${SYSTEMD_UNIT_FILE}"
    echo ""

    if [ "${DRY_RUN}" = true ]; then
        echo "  No changes were made. Remove --dry-run to install."
    else
        echo "  Next steps:"
        echo "    1. Create your cluster configuration:"
        echo "         sudo -u hive vi ${HIVE_DATA_DIR}/cluster.yaml"
        echo ""
        echo "    2. Enable and start the service:"
        echo "         sudo systemctl enable hived"
        echo "         sudo systemctl start hived"
        echo ""
        echo "    3. Check service status:"
        echo "         sudo systemctl status hived"
        echo "         sudo journalctl -u hived -f"
        echo ""
        echo "    4. Use hivectl to interact with the cluster:"
        echo "         hivectl agents"
        echo "         hivectl nodes"
    fi
    echo ""
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
    echo ""
    echo "============================================================"
    echo "  Hive Installer"
    echo "  https://github.com/${GITHUB_REPO}"
    echo "============================================================"
    echo ""

    parse_args "$@"

    if [ "${DRY_RUN}" = true ]; then
        log_info "Running in DRY-RUN mode. No changes will be made."
        echo ""
    fi

    require_root

    # Phase 1: Detection
    log_step "Detecting system"
    detect_os
    detect_arch

    # Phase 2: Pre-flight checks
    preflight_checks

    # Phase 3: Resolve version
    log_step "Resolving version"
    resolve_version

    # Show plan and confirm
    echo ""
    log_info "Installation plan:"
    log_info "  - Version:      ${VERSION}"
    log_info "  - OS:           ${DETECTED_OS}"
    log_info "  - Architecture: ${DETECTED_ARCH}"
    log_info "  - Binaries:     hived, hivectl, hive-agent, hive-sidecar"
    log_info "  - Install to:   ${INSTALL_DIR}"
    log_info "  - Data dir:     ${HIVE_DATA_DIR}"
    echo ""

    if [ "${DRY_RUN}" != true ]; then
        if ! confirm "Proceed with installation?"; then
            log_info "Installation cancelled."
            exit 0
        fi
    fi

    # Phase 4: Create user and directories
    create_user_and_group
    create_directories

    # Phase 5: Download and install binaries
    download_binaries
    install_binaries

    # Phase 6: Download kernel and rootfs images
    download_images

    # Phase 7: Install systemd unit
    install_systemd_unit

    # Phase 8: Summary
    print_summary
}

main "$@"
