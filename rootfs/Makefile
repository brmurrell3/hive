# Hive Rootfs Builder
# Builds a minimal Alpine Linux rootfs for Firecracker VMs with the sidecar binary baked in.
#
# Usage:
#   make rootfs             # Build the rootfs image
#   make download-kernel    # Download a pre-built Firecracker-compatible vmlinux kernel
#   make rootfs-clean       # Remove build artifacts
#
# Requirements:
#   - curl (for download-kernel)
#   - Docker (for Alpine container, used by build-rootfs.sh)
#   - Go toolchain (for sidecar cross-compilation, if pre-built binary not found)
#   - mkfs.ext4 with -d support (e2fsprogs >= 1.43, standard on Linux;
#     on macOS install via: brew install e2fsprogs and set PATH accordingly)
#
# Kernel notes:
#   Pre-built Firecracker-compatible kernels are published with each Firecracker
#   release at:
#     https://github.com/firecracker-microvm/firecracker/releases
#   The vmlinux-*.bin files are statically-linked ELF kernels ready for direct
#   boot. No initrd is needed; Firecracker boots them directly.
#
#   To use a different kernel version, override KERNEL_URL:
#     make download-kernel KERNEL_URL=https://...

ROOTFS_IMG  := rootfs.ext4
ROOTFS_SIZE := 512M
SIDECAR_BIN := hive-sidecar
PREBUILT_SIDECAR := ../bin/linux-amd64/$(SIDECAR_BIN)
ARCH ?= $(shell uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')

VMLINUX := vmlinux

# Default kernel: Firecracker v1.6.0 pre-built x86_64 kernel (Linux 5.10).
# Override with: make download-kernel KERNEL_URL=<url>
KERNEL_URL ?= https://github.com/firecracker-microvm/firecracker/releases/download/v1.6.0/vmlinux-5.10-x86_64.bin

.PHONY: rootfs rootfs-clean sidecar download-kernel

rootfs: sidecar
	@echo "Building Alpine rootfs..."
	@./build-rootfs.sh $(ROOTFS_IMG) $(ROOTFS_SIZE) $(SIDECAR_BIN)
	@echo "Rootfs built: $(ROOTFS_IMG)"

# Download a pre-built Firecracker-compatible vmlinux kernel.
#
# The resulting file is placed at rootfs/vmlinux and referenced by the VM
# manager as the kernel image path. Override KERNEL_URL to fetch a different
# version or architecture.
#
# Manual alternative (no make):
#   curl -fSL -o rootfs/vmlinux \
#     https://github.com/firecracker-microvm/firecracker/releases/download/v1.6.0/vmlinux-5.10-x86_64.bin
#
# For arm64 hosts, the equivalent URL is:
#   https://github.com/firecracker-microvm/firecracker/releases/download/v1.6.0/vmlinux-5.10-aarch64.bin
download-kernel:
	@echo "Downloading Firecracker kernel from $(KERNEL_URL)..."
	curl -fSL --progress-bar -o "$(VMLINUX)" "$(KERNEL_URL)"
	@echo "Kernel downloaded: $(VMLINUX)"
	@file "$(VMLINUX)" 2>/dev/null || true

sidecar:
	@if [ -f "$(PREBUILT_SIDECAR)" ]; then \
		echo "Using pre-built sidecar from $(PREBUILT_SIDECAR)"; \
		cp "$(PREBUILT_SIDECAR)" $(SIDECAR_BIN); \
	else \
		echo "Pre-built sidecar not found, cross-compiling for linux/$(ARCH)..."; \
		cd .. && CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o rootfs/$(SIDECAR_BIN) ./cmd/hive-sidecar; \
	fi

rootfs-clean:
	rm -f $(ROOTFS_IMG) $(SIDECAR_BIN) $(VMLINUX)
