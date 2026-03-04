#!/bin/bash
# Build a Raspberry Pi SD card image with hive-agent pre-installed.
# Supports RPi 3, 4, 5, and Zero 2W.
#
# Usage: ./build.sh [ARCH] [OUTPUT]
#   ARCH: arm64 (default) or armv7
#   OUTPUT: output image file (default: hive-rpi-ARCH.img)

set -euo pipefail

ARCH="${1:-arm64}"
OUTPUT="${2:-hive-rpi-${ARCH}.img}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${SCRIPT_DIR}/../.."

echo "=== Building hive-agent for linux/${ARCH} ==="
cd "${PROJECT_ROOT}"
GOOS=linux GOARCH="${ARCH}" CGO_ENABLED=0 go build -o "${SCRIPT_DIR}/hive-agent" ./cmd/hive-agent

echo "=== Binary built: ${SCRIPT_DIR}/hive-agent ==="
ls -la "${SCRIPT_DIR}/hive-agent"

echo ""
echo "=== RPi Image Build ==="
echo "To create a full SD card image, you need a base Raspberry Pi OS image."
echo ""
echo "Quick setup steps:"
echo "1. Download Raspberry Pi OS Lite (64-bit for arm64, 32-bit for armv7)"
echo "2. Flash to SD card using Raspberry Pi Imager"
echo "3. Copy hive-agent binary to /usr/local/bin/ on the SD card"
echo "4. Copy the systemd service file:"
echo "   cp ${SCRIPT_DIR}/hive-agent.service /etc/systemd/system/"
echo "5. Enable the service:"
echo "   systemctl enable hive-agent"
echo ""
echo "Or use the install.sh script on a running Pi:"
echo "   scp ${SCRIPT_DIR}/hive-agent pi@raspberrypi:/tmp/"
echo "   scp ${SCRIPT_DIR}/install.sh pi@raspberrypi:/tmp/"
echo "   ssh pi@raspberrypi 'sudo /tmp/install.sh'"
