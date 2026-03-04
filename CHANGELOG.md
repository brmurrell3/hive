# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

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
