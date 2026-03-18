# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.9.0] - 2026-03-18

### Added
- TAP device lifecycle management for Firecracker VMs
- Per-agent /30 subnet allocation from 172.16.0.0/16 with IP masquerade NAT
- Startup pre-flight checks for KVM, CAP_NET_ADMIN, CAP_SYS_ADMIN, nft, vhost_vsock
- `--skip-network` flag to run without TAP/nftables (vsock-only mode)
- `--firecracker-bin` flag for custom Firecracker binary path
- `--kernel-path` flag for custom kernel location
- Kernel auto-download with SHA-256 checksum and ELF validation
- `imageURL` field in cluster.yaml for air-gapped deployments (file:// and https://)
- Firecracker version check at startup with minimum version enforcement
- `GET /readyz` endpoint with NATS, state store, and reconciler health checks
- Enhanced `GET /healthz` to return version and uptime
- `hivectl doctor` command with 11 system checks and remediation hints
- `hivectl logs <agent-id>` command for real-time log streaming via NATS
- `hivectl exec <agent-id> -- <command>` for remote command execution
- Colorized CLI output using lipgloss
- Version embedding with commit hash and build date in all binaries
- Systemd unit file with security hardening
- Full installation script with pre-flight checks
- Grafana dashboard with 14 panels
- Prometheus scrape configuration
- Go, Python, and TypeScript SDKs for building agents
- Threat model documentation
- API reference documentation

### Changed
- `make build-linux-arm64` now builds all 4 binaries
- `make build-all` includes darwin/amd64 target for Intel Macs
- Makefile LDFLAGS includes commit hash and build date
- `hivectl version` now shows commit, build date, and Go version
- Release workflow generates changelog and triggers rootfs builds
- CI pipeline includes license header check, go mod tidy check, and vm build tag job
- Stale TAP devices are cleaned up during ReconcileOnStartup

### Fixed
- TAP device creation before Firecracker VM boot
- Guest-side network connectivity
- TAP device leak on VM destroy and crash recovery paths

## [0.1.0-alpha] - 2026-03-09

Initial public alpha release.

### Added
- Declarative YAML manifests for agents, teams, and clusters
- Embedded NATS server with JetStream support
- Firecracker microVM lifecycle management (start, stop, restart, destroy)
- Sidecar binary for in-VM agent runtime management with HTTP API
- Capability routing over NATS with request/reply invocation
- Health monitoring with configurable heartbeats and auto-restart
- MEMORY.md hot-reload via fsnotify
- `hivectl` CLI with full cluster management commands
- Join tokens for secure node registration (SHA-256 hashed)
- Tier 2 native agent support with `hive-agent join`
- Bin-packing scheduler with scoring and team colocation
- Multi-node clustering (root/worker roles)
- RBAC (admin/operator/viewer roles)
- REST + WebSocket dashboard API
- Interactive TUI dashboard (`hivectl dashboard`)
- Prometheus metrics endpoint with bounded cardinality
- Log aggregation via NATS with SQLite persistence
- NixOS rootfs build (flake-based)
- Graceful shutdown and crash recovery
- Rate limiting and resource monitoring
- Fuzz tests for config parsing, NATS subject validation, and auth
- Automated security scanning (gosec, govulncheck) in CI
