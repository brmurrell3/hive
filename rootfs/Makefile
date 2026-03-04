# Hive Rootfs Builder
# Builds a minimal Alpine Linux rootfs for Firecracker VMs with the sidecar binary baked in.
#
# Usage:
#   make rootfs           # Build the rootfs image
#   make rootfs-clean     # Remove build artifacts
#
# Requirements:
#   - Docker (for Alpine container)
#   - Go toolchain (for sidecar cross-compilation, if pre-built binary not found)

ROOTFS_IMG := rootfs.ext4
ROOTFS_SIZE := 512M
SIDECAR_BIN := hive-sidecar
PREBUILT_SIDECAR := ../bin/linux-amd64/$(SIDECAR_BIN)
ARCH ?= $(shell uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')

.PHONY: rootfs rootfs-clean sidecar

rootfs: sidecar
	@echo "Building Alpine rootfs..."
	@./build-rootfs.sh $(ROOTFS_IMG) $(ROOTFS_SIZE) $(SIDECAR_BIN)
	@echo "Rootfs built: $(ROOTFS_IMG)"

sidecar:
	@if [ -f "$(PREBUILT_SIDECAR)" ]; then \
		echo "Using pre-built sidecar from $(PREBUILT_SIDECAR)"; \
		cp "$(PREBUILT_SIDECAR)" $(SIDECAR_BIN); \
	else \
		echo "Pre-built sidecar not found, cross-compiling for linux/$(ARCH)..."; \
		cd .. && CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o rootfs/$(SIDECAR_BIN) ./cmd/hive-sidecar; \
	fi

rootfs-clean:
	rm -f $(ROOTFS_IMG) $(SIDECAR_BIN)
