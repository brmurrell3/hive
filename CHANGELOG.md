# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- TAP device lifecycle management for Firecracker VMs (`internal/vm/tap.go`)
- Per-agent /30 subnet allocation from 172.16.0.0/16 with IP masquerade NAT
- Startup pre-flight checks for KVM, CAP_NET_ADMIN, CAP_SYS_ADMIN, nft, vhost_vsock
- `--skip-network` flag to run without TAP/nftables (vsock-only mode)
- `--firecracker-bin` flag for custom Firecracker binary path
- `--kernel-path` flag for custom kernel location
- Kernel auto-download with SHA-256 checksum and ELF validation (`internal/vm/kernel.go`)
- `imageURL` field in cluster.yaml for air-gapped deployments (file:// and https://)
- Firecracker version check at startup with minimum version enforcement
- `GET /readyz` endpoint with NATS, state store, and reconciler health checks
- Enhanced `GET /healthz` to return version and uptime
- `hivectl doctor` command with 11 system checks and remediation hints
- `hivectl logs <agent-id>` command for real-time log streaming via NATS
- `hivectl exec <agent-id> -- <command>` for remote command execution
- Colorized CLI output using lipgloss (agent status, node status, doctor checks)
- Version embedding with commit hash and build date in all 4 binaries
- Systemd unit file (`deploy/systemd/hived.service`) with security hardening
- System setup script (`deploy/systemd/hive-setup.sh`)
- Full installation script (`scripts/install.sh`) with pre-flight checks
- Grafana dashboard JSON with 14 panels (`deploy/grafana/hive-dashboard.json`)
- Example Prometheus scrape configuration (`deploy/prometheus/prometheus.yml`)
- NOTICE file listing third-party dependency licenses
- TypeScript SDK README
- Threat model documentation (`docs/threat-model.md`)
- API reference documentation (`docs/api-reference.md`)

### Changed
- `make build-linux-arm64` now builds all 4 binaries (was missing hived, hive-sidecar)
- `make build-all` includes darwin/amd64 target for Intel Macs
- Makefile LDFLAGS includes commit hash and build date
- `hivectl version` now shows commit, build date, and Go version
- Rewrote README.md with value proposition, quickstart, and platform matrix
- Rewrote docs/getting-started.md as 4-part guide
- Rewrote docs/operations.md with prerequisites, troubleshooting, backup/recovery
- Updated docs/architecture.md with VM lifecycle and network topology
- Release workflow generates changelog and triggers rootfs builds
- CI pipeline now includes license header check, go mod tidy check, and vm build tag job
- Stale TAP devices are cleaned up during ReconcileOnStartup
- cleanupAgentNetworkPolicy now deletes TAP devices alongside nftables cleanup
- Private implementation docs moved from root to docs/internal/

### Fixed
- TAP device creation before Firecracker VM boot (was missing entirely)
- Guest-side network connectivity (host-side TAP was not configured)
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
