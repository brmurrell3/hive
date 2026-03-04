# Hive

Declarative framework for orchestrating LLM agent teams. Define agents in YAML, deploy to Firecracker VMs, route capabilities over NATS.

**Current state:** MVP complete (M1–M5). Single-node, single-team deployment with capability routing, auto-restart, and MEMORY.md hot-reload.

## Requirements

- Go 1.21+
- Linux with KVM for running VMs (macOS works for building and testing with mocks)

## Quick Start

```bash
# Build
make build

# Scaffold a cluster
./hivectl init my-cluster

# Inspect what was created
tree my-cluster/

# Validate the config
./hivectl validate --cluster-root my-cluster
```

## What's in a Cluster

```
my-cluster/
├── cluster.yaml                    # NATS config, defaults, cluster settings
├── agents/
│   └── example-agent/
│       └── manifest.yaml           # Agent definition: runtime, capabilities, resources
└── teams/
    └── default.yaml                # Team definition: lead agent, shared volumes
```

## Running hived

```bash
# Start the control plane (embeds NATS, watches for config changes)
./hived --cluster-root my-cluster
```

hived will:
- Parse and validate all manifests
- Start an embedded NATS server (default port 4222)
- Enable JetStream for message persistence
- Watch `MEMORY.md` files for hot-reload

## Managing Agents

Requires a Linux host with KVM for real VMs. Use `HIVE_TEST_FIRECRACKER=mock` for mock mode on any platform.

```bash
export HIVE_TEST_FIRECRACKER=mock

# Start an agent
./hivectl agents start example-agent --cluster-root my-cluster

# List agents
./hivectl agents list --cluster-root my-cluster

# Check status
./hivectl agents status example-agent --cluster-root my-cluster

# Restart (resets restart counter)
./hivectl agents restart example-agent --cluster-root my-cluster

# Stop
./hivectl agents stop example-agent --cluster-root my-cluster

# Destroy (removes all VM artifacts)
./hivectl agents destroy example-agent --cluster-root my-cluster
```

## Tests

```bash
# Unit tests (fast, no dependencies)
make test-unit

# Integration tests (spins up embedded NATS)
make test-integration

# Both
make test
```

144 tests across 8 packages covering: config parsing, validation, NATS pub/sub, JetStream, state persistence, VM lifecycle, sidecar HTTP API, heartbeat monitoring, capability routing, tool generation, auto-restart logic, and filesystem watching.

## Project Layout

```
cmd/hived/           Control plane daemon
cmd/hivectl/         CLI tool
internal/
  config/            YAML parsing + validation
  nats/              Embedded NATS server
  state/             state.json persistence
  vm/                Firecracker VM lifecycle (+ mock)
  sidecar/           Agent runtime, HTTP API, heartbeats
  capability/        NATS capability routing + tool generation
  health/            Heartbeat monitor + auto-restart
  watcher/           MEMORY.md hot-reload (fsnotify)
  types/             Shared types
  testutil/          Test helpers
rootfs/              Alpine rootfs build scripts
```

## Key Concepts

- **Agents** are defined in YAML manifests and run inside Firecracker VMs
- **Teams** group agents; a lead agent gets auto-generated tools to invoke team members' capabilities
- **Capabilities** are typed functions (inputs/outputs) exposed over NATS subjects
- **Sidecar** runs inside each VM, manages the agent runtime, publishes heartbeats, serves the HTTP API at `:9100`
- **Health** — hived monitors heartbeats; missed heartbeats trigger auto-restart per the agent's restart policy
- **MEMORY.md** is hot-reloaded into VMs when edited on the host (no restart needed)
