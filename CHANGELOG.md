# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- Declarative YAML manifests for agents, teams, and clusters
- Embedded NATS server with JetStream support
- Firecracker microVM lifecycle management (start, stop, restart, destroy)
- Sidecar binary for in-VM agent runtime management with HTTP API
- Capability routing over NATS with auto-generated tool definitions
- Health monitoring with configurable heartbeats and auto-restart
- MEMORY.md hot-reload via fsnotify
- `hivectl` CLI with full cluster management commands
- Join tokens for secure node registration
- Tier 2 native agent support with `hive-agent join`
- MQTT-NATS bridge for Tier 3 firmware devices
- C and MicroPython firmware SDKs
- Bin-packing scheduler with scoring
- Multi-node clustering (root/worker roles)
- Cross-team capability routing
- Director agent with organization-wide tools
- RBAC (admin/operator/viewer roles)
- OTA firmware updates
- REST + WebSocket dashboard API
- Prometheus metrics endpoint
- Log aggregation via NATS
- NixOS rootfs build (flake-based)
- Graceful shutdown and crash recovery
- Rate limiting and resource monitoring
