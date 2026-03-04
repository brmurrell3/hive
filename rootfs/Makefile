# Hive Rootfs Builder
# Builds a minimal Alpine Linux rootfs for Firecracker VMs with the sidecar binary baked in.
#
# Usage:
#   make rootfs           # Build the rootfs image
#   make rootfs-clean     # Remove build artifacts
#
# Requirements:
#   - Docker (for Alpine container)
#   - Go toolchain (for sidecar cross-compilation)

ROOTFS_IMG := rootfs.ext4
ROOTFS_SIZE := 512M
SIDECAR_BIN := hive-sidecar
ARCH ?= $(shell uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')

.PHONY: rootfs rootfs-clean sidecar

rootfs: sidecar
	@echo "Building Alpine rootfs..."
	@./build-rootfs.sh $(ROOTFS_IMG) $(ROOTFS_SIZE) $(SIDECAR_BIN)
	@echo "Rootfs built: $(ROOTFS_IMG)"

sidecar:
	@echo "Cross-compiling sidecar for linux/$(ARCH)..."
	cd .. && CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o rootfs/$(SIDECAR_BIN) ./cmd/hive-sidecar 2>/dev/null || \
		echo "Note: hive-sidecar cmd not yet implemented, skipping binary build"

rootfs-clean:
	rm -f $(ROOTFS_IMG) $(SIDECAR_BIN)
