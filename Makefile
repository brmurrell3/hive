.PHONY: build build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-all test test-unit test-integration test-vm test-e2e lint-full fuzz audit rootfs rootfs-openclaw download-kernel clean demo help

.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

# Preflight check for Go toolchain.
GO := $(shell command -v go 2>/dev/null)
ifndef GO
$(error "Go is not installed. Download it from https://go.dev/dl/")
endif

# Architecture detection for kernel downloads.
# Maps uname -m to Firecracker release artifact suffixes.
HOST_ARCH := $(shell uname -m)
ifeq ($(HOST_ARCH),x86_64)
  FC_ARCH := x86_64
  GO_ARCH := amd64
else ifeq ($(HOST_ARCH),aarch64)
  FC_ARCH := aarch64
  GO_ARCH := arm64
else ifeq ($(HOST_ARCH),arm64)
  FC_ARCH := aarch64
  GO_ARCH := arm64
else
  FC_ARCH := $(HOST_ARCH)
  GO_ARCH := $(HOST_ARCH)
endif

# Firecracker kernel settings.
# Override KERNEL_URL to use a custom kernel source (e.g., internal mirror).
FC_VERSION   ?= v1.6.0
KERNEL_LINUX ?= 5.10
KERNEL_URL   ?= https://github.com/firecracker-microvm/firecracker/releases/download/$(FC_VERSION)/vmlinux-$(KERNEL_LINUX)-$(FC_ARCH).bin

# Native platform build
build:
	@mkdir -p bin
	@go build -ldflags "$(LDFLAGS)" -o bin/hived ./cmd/hived
	@go build -ldflags "$(LDFLAGS)" -o bin/hivectl ./cmd/hivectl
	@go build -ldflags "$(LDFLAGS)" -o bin/hive-agent ./cmd/hive-agent
	@go build -ldflags "$(LDFLAGS)" -o bin/hive-sidecar ./cmd/hive-sidecar

# Cross-compile for linux/amd64 (Firecracker VMs, x86 servers)
build-linux-amd64:
	@mkdir -p bin/linux-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/linux-amd64/hived ./cmd/hived
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/linux-amd64/hivectl ./cmd/hivectl
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/linux-amd64/hive-sidecar ./cmd/hive-sidecar
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/linux-amd64/hive-agent ./cmd/hive-agent

# Cross-compile for linux/arm64 (Raspberry Pi, ARM servers)
build-linux-arm64:
	@mkdir -p bin/linux-arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/linux-arm64/hived ./cmd/hived
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/linux-arm64/hivectl ./cmd/hivectl
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/linux-arm64/hive-agent ./cmd/hive-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/linux-arm64/hive-sidecar ./cmd/hive-sidecar

# Cross-compile for darwin/amd64 (Intel Macs)
build-darwin-amd64:
	@mkdir -p bin/darwin-amd64
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/darwin-amd64/hived ./cmd/hived
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/darwin-amd64/hivectl ./cmd/hivectl
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/darwin-amd64/hive-agent ./cmd/hive-agent
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/darwin-amd64/hive-sidecar ./cmd/hive-sidecar

# Build all targets
build-all: build build-linux-amd64 build-linux-arm64 build-darwin-amd64

test: test-unit test-integration

test-unit:
	go test -tags unit -race -count=1 ./...

test-integration:
	go test -tags integration -race -count=1 -timeout 5m ./...

test-vm:
	go test -tags vm -count=1 -timeout 10m ./...

test-e2e:
	go test -tags e2e -count=1 -timeout 2m -v ./test/e2e/

FUZZ_TIME ?= 30s

lint-full:
	golangci-lint run ./...

fuzz:
	go test -tags unit -run=^$$ -fuzz=^FuzzParseMemory$$ -fuzztime=$(FUZZ_TIME) ./internal/config/
	go test -tags unit -run=^$$ -fuzz=^FuzzParseDiskSize$$ -fuzztime=$(FUZZ_TIME) ./internal/config/
	go test -tags unit -run=^$$ -fuzz=^FuzzValidateSubjectComponent$$ -fuzztime=$(FUZZ_TIME) ./internal/types/
	go test -tags unit -run=^$$ -fuzz=^FuzzEnvelopeValidate$$ -fuzztime=$(FUZZ_TIME) ./internal/types/
	go test -tags unit -run=^$$ -fuzz=^FuzzAuthenticate$$ -fuzztime=$(FUZZ_TIME) ./internal/auth/

audit:
	./scripts/audit.sh

# Download a pre-built Firecracker-compatible vmlinux kernel for the host architecture.
# The kernel is placed at rootfs/vmlinux. Override KERNEL_URL for custom kernels or mirrors.
#
# Examples:
#   make download-kernel                     # Download default kernel for host arch
#   make download-kernel FC_VERSION=v1.7.0   # Use a different Firecracker release
#   make download-kernel KERNEL_URL=https://internal-mirror.example/vmlinux  # Air-gapped
download-kernel:
	@echo "Downloading Firecracker kernel for $(FC_ARCH) from $(KERNEL_URL)..."
	@mkdir -p rootfs
	curl -fSL --progress-bar -o rootfs/vmlinux "$(KERNEL_URL)"
	@echo "Kernel downloaded: rootfs/vmlinux ($(FC_ARCH))"
	@file rootfs/vmlinux 2>/dev/null || true

# Build rootfs image. Depends on kernel download to ensure vmlinux is available.
rootfs: download-kernel
	$(MAKE) -C rootfs rootfs

# Build OpenClaw variant rootfs image.
rootfs-openclaw: download-kernel
	$(MAKE) -C rootfs rootfs ROOTFS_VARIANT=openclaw

# One-command demo: build, scaffold, start agents, trigger pipeline, show report.
demo: build
	@./scripts/demo.sh

clean:
	rm -rf bin .demo
	rm -f hived hivectl hive-agent
	$(MAKE) -C rootfs rootfs-clean
	go clean ./...

help: ## Show this help
	@echo "Hive — Declarative orchestration for AI agent teams"
	@echo ""
	@echo "Build targets:"
	@echo "  make build              Build all binaries for the host platform"
	@echo "  make build-linux-amd64  Cross-compile for Linux x86_64"
	@echo "  make build-linux-arm64  Cross-compile for Linux ARM64"
	@echo "  make build-darwin-amd64 Cross-compile for macOS Intel"
	@echo "  make build-all          Build for all platforms"
	@echo ""
	@echo "Test targets:"
	@echo "  make test               Run unit + integration tests"
	@echo "  make test-unit          Run unit tests with race detector"
	@echo "  make test-integration   Run integration tests with race detector"
	@echo "  make test-vm            Run VM tests (requires KVM or mock)"
	@echo "  make test-e2e           Run end-to-end tests"
	@echo "  make fuzz               Run fuzz tests (FUZZ_TIME=30s)"
	@echo "  make lint-full          Run golangci-lint"
	@echo ""
	@echo "VM image targets:"
	@echo "  make download-kernel    Download Firecracker-compatible kernel"
	@echo "  make rootfs             Build base rootfs image"
	@echo "  make rootfs-openclaw    Build OpenClaw rootfs variant"
	@echo ""
	@echo "Other:"
	@echo "  make demo               Build and run the demo cluster"
	@echo "  make audit              Run security audit script"
	@echo "  make clean              Remove build artifacts"
	@echo ""
	@echo "Variables:"
	@echo "  VERSION=$(VERSION)"
	@echo "  FC_VERSION=$(FC_VERSION)"
	@echo "  HOST_ARCH=$(HOST_ARCH)"
