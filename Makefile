.PHONY: build build-linux-amd64 build-linux-arm64 build-all test test-unit test-integration test-vm rootfs clean

# Native platform build
build:
	@mkdir -p bin
	go build -o bin/hived ./cmd/hived
	go build -o bin/hivectl ./cmd/hivectl
	go build -o bin/hive-agent ./cmd/hive-agent

# Cross-compile for linux/amd64 (Firecracker VMs, x86 servers)
build-linux-amd64:
	@mkdir -p bin/linux-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux-amd64/hived ./cmd/hived
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux-amd64/hivectl ./cmd/hivectl
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux-amd64/hive-sidecar ./cmd/hive-sidecar
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux-amd64/hive-agent ./cmd/hive-agent

# Cross-compile for linux/arm64 (Raspberry Pi)
build-linux-arm64:
	@mkdir -p bin/linux-arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/linux-arm64/hive-agent ./cmd/hive-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/linux-arm64/hivectl ./cmd/hivectl

# Build all targets
build-all: build build-linux-amd64 build-linux-arm64

test: test-unit test-integration

test-unit:
	go test -tags unit -race -count=1 ./...

test-integration:
	go test -tags integration -race -count=1 -timeout 5m ./...

test-vm:
	go test -tags vm -count=1 -timeout 10m ./...

rootfs:
	$(MAKE) -C rootfs rootfs

clean:
	rm -rf bin
	rm -f hived hivectl hive-agent
	$(MAKE) -C rootfs rootfs-clean
	go clean ./...
