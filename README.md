# Hive

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![CI](https://img.shields.io/badge/CI-passing-brightgreen)]()

## What is Hive?

Existing LLM frameworks assume homogeneous cloud infrastructure, but real deployments mix hardware tiers: workstations, Raspberry Pis, and microcontrollers. Hive bridges that gap.

**Hive is a declarative framework for orchestrating LLM agent teams across heterogeneous hardware.** Define agents in YAML, deploy to Firecracker microVMs, Raspberry Pis, or microcontrollers, and route capabilities over NATS — all from a single control plane.

### Architecture

Hive uses a three-tier execution model:

| Tier | Hardware | Execution | Isolation | Examples |
|------|----------|-----------|-----------|----------|
| **1** | Linux + KVM, 4GB+ RAM | Firecracker microVM | Full (kernel, memory, network) | Workstations, servers, NUCs |
| **2** | Linux + systemd, 512MB+ RAM | Native process | Process-level | Raspberry Pi, Jetson Nano, old laptops |
| **3** | Network-capable MCU | Firmware (C/MicroPython SDK) | Device-level | ESP32, Pi Pico W, Arduino |

All tiers communicate over a unified NATS message bus. Tier 3 devices connect via an MQTT-NATS bridge.

## Features

- **Declarative YAML manifests** for agents, teams, and clusters
- **Three-tier execution model:** Firecracker VMs (Tier 1), native processes (Tier 2), firmware devices (Tier 3)
- **Capability routing** over NATS with auto-generated tool definitions
- **Health monitoring** with configurable heartbeats and auto-restart policies
- **Multi-node clustering** with bin-packing scheduler
- **Cross-team routing** and a director agent for organization-wide orchestration
- **RBAC** with admin, operator, and viewer roles
- **Firmware SDKs** for C and MicroPython with OTA updates
- **Dashboard API** with REST and WebSocket endpoints
- **Prometheus metrics** and log aggregation

## Requirements

- Go 1.25+
- Linux with KVM for Firecracker VMs (macOS works for building and testing with mocks)

## Quick Start

```bash
# Build
make build

# Scaffold a cluster
./bin/hivectl init my-cluster

# Validate the config
./bin/hivectl validate --cluster-root my-cluster

# Start the control plane
./bin/hived --cluster-root my-cluster
```

See the [Getting Started](docs/getting-started.md) guide for a full walkthrough.

## What's in a Cluster

```
my-cluster/
├── cluster.yaml                    # NATS config, defaults, cluster settings
├── agents/
│   └── example-agent/
│       └── manifest.yaml           # Agent: runtime, capabilities, resources
└── teams/
    └── default.yaml                # Team: lead agent, shared config
```

## Project Layout

```
cmd/
  hived/           Control plane daemon
  hivectl/         CLI tool
  hive-agent/      Tier 2 native agent binary
internal/
  config/          YAML parsing + validation
  nats/            Embedded NATS server
  state/           State persistence (SQLite)
  vm/              Firecracker VM lifecycle (+ mock)
  sidecar/         Agent runtime, HTTP API, heartbeats
  capability/      Capability routing + tool generation
  health/          Heartbeat monitor + auto-restart
  watcher/         MEMORY.md hot-reload (fsnotify)
  token/           Join token generation + validation
  node/            Node registry + discovery
  mqtt/            MQTT-NATS bridge for firmware devices
  firmware/        Device tracking + OTA updates
  scheduler/       Bin-packing scheduler
  reconciler/      Reconciliation loop
  cluster/         Multi-node clustering
  auth/            RBAC
  director/        Director agent
  metrics/         Prometheus metrics
  logs/            Log aggregation
  dashboard/       REST + WebSocket API
  types/           Shared type definitions
  testutil/        Test helpers
docs/              Documentation and specifications
rootfs/            Firecracker VM rootfs build scripts
sdk/
  firmware-c/          C SDK for microcontrollers
  firmware-micropython/ MicroPython SDK
```

## Documentation

### Guides

| Document | Description |
|----------|-------------|
| [Getting Started](docs/getting-started.md) | Scaffold, validate, start, and manage agents |
| [Operations Guide](docs/operations.md) | Full operational reference for all features |
| [Testing Guide](docs/testing.md) | Test strategy, mock mode, and real Firecracker |

### Specifications

| Document | Description |
|----------|-------------|
| [Architecture](docs/architecture.md) | Agent tiers, node types, components |
| [Schemas](docs/schemas.md) | YAML manifest specification |
| [Communication](docs/communication.md) | NATS subjects, message envelope format |
| [Control Plane](docs/control-plane.md) | hived internals, reconciler, scheduler |
| [Execution](docs/execution.md) | Sidecar, runtime lifecycle, boot sequences |
| [Firmware](docs/firmware.md) | C and MicroPython SDKs, OTA protocol |
| [CLI Reference](docs/cli-reference.md) | All hivectl commands |
| [Deployment](docs/deployment.md) | NixOS modules, node joining, images |

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
