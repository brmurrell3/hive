.PHONY: build test test-unit test-integration test-vm rootfs clean

build:
	go build ./cmd/hived
	go build ./cmd/hivectl

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
	rm -f hived hivectl
	$(MAKE) -C rootfs rootfs-clean
	go clean ./...
