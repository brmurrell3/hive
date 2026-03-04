.PHONY: build build-linux-amd64 build-linux-arm64 build-all test test-unit test-integration test-vm test-e2e lint-full fuzz audit rootfs clean demo

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS = -X main.version=$(VERSION)

# Preflight check for Go toolchain.
GO := $(shell command -v go 2>/dev/null)
ifndef GO
$(error "Go is not installed. Download it from https://go.dev/dl/")
endif

# Native platform build
build:
	@mkdir -p bin
	@go build -ldflags "$(LDFLAGS)" -o bin/hived ./cmd/hived
	@go build -ldflags "$(LDFLAGS)" -o bin/hivectl ./cmd/hivectl
	@go build -ldflags "$(LDFLAGS)" -o bin/hive-agent ./cmd/hive-agent

# Cross-compile for linux/amd64 (Firecracker VMs, x86 servers)
build-linux-amd64:
	@mkdir -p bin/linux-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/linux-amd64/hived ./cmd/hived
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/linux-amd64/hivectl ./cmd/hivectl
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/linux-amd64/hive-sidecar ./cmd/hive-sidecar
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/linux-amd64/hive-agent ./cmd/hive-agent

# Cross-compile for linux/arm64 (Raspberry Pi)
build-linux-arm64:
	@mkdir -p bin/linux-arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/linux-arm64/hive-agent ./cmd/hive-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/linux-arm64/hivectl ./cmd/hivectl

# Build all targets
build-all: build build-linux-amd64 build-linux-arm64

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

rootfs:
	$(MAKE) -C rootfs rootfs

# One-command demo: build, scaffold, start agents, trigger pipeline, show report.
demo: build
	@./scripts/demo.sh

clean:
	rm -rf bin .demo
	rm -f hived hivectl hive-agent
	$(MAKE) -C rootfs rootfs-clean
	go clean ./...
