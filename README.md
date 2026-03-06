# Hive

Declarative framework for orchestrating LLM agent teams across heterogeneous hardware. Define agents in YAML, deploy to Firecracker microVMs, Raspberry Pis, or microcontrollers, and route capabilities over NATS.

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

- Go 1.23+
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

## Managing Agents

```bash
# Use mock mode on macOS or without KVM
export HIVE_TEST_FIRECRACKER=mock

./bin/hivectl agents start example-agent --cluster-root my-cluster
./bin/hivectl agents list --cluster-root my-cluster
./bin/hivectl agents status example-agent --cluster-root my-cluster
./bin/hivectl agents restart example-agent --cluster-root my-cluster
./bin/hivectl agents stop example-agent --cluster-root my-cluster
```

## Tests

```bash
make test-unit          # Fast, no dependencies
make test-integration   # Spins up embedded NATS
make test               # Both
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
docs/              Specification documents
rootfs/            Firecracker VM rootfs build scripts
sdk/
  firmware-c/          C SDK for microcontrollers
  firmware-micropython/ MicroPython SDK
```

## Documentation

- [Architecture](docs/01-ARCHITECTURE.md) — Agent tiers, node types, components
- [Schemas](docs/02-SCHEMAS.md) — YAML manifest specification
- [Communication](docs/03-COMMUNICATION.md) — NATS subjects, message envelope format
- [Control Plane](docs/04-CONTROL-PLANE.md) — hived internals
- [Execution](docs/05-EXECUTION.md) — Sidecar, runtime lifecycle, boot sequences
- [Firmware](docs/06-FIRMWARE.md) — C and MicroPython SDKs, OTA protocol
- [CLI Reference](docs/07-CLI-AND-INTERACTION.md) — All hivectl commands
- [Deployment](docs/08-DEPLOYMENT.md) — NixOS modules, node joining, images
- [Operations Guide](OPERATIONS.md) — Full end-to-end walkthrough
- [Testing Guide](TESTING.md) — Test strategy and setup

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
