[← Back to Documentation](README.md)

# Getting Started with Hive

This guide walks you through scaffolding a cluster, validating the configuration, starting the control plane, and managing agents. For the full operational reference, see the [Operations Guide](operations.md).

## Prerequisites

- Go 1.25+ installed
- Binaries built: `make build`
- On macOS or without KVM: `export HIVE_TEST_FIRECRACKER=mock`

## 1. Scaffold a Cluster

Create a fresh cluster directory with template files:

```bash
./bin/hivectl init my-cluster
```

This creates:

```
my-cluster/
├── cluster.yaml                    # Cluster configuration
├── agents/
│   └── example-agent/
│       └── manifest.yaml           # Example agent manifest
└── teams/
    └── default.yaml                # Default team
```

Inspect the generated files to understand the structure:

```bash
cat my-cluster/cluster.yaml
cat my-cluster/agents/example-agent/manifest.yaml
cat my-cluster/teams/default.yaml
```

## 2. Validate the Configuration

Check all YAML manifests for errors without starting anything:

```bash
./bin/hivectl validate --cluster-root my-cluster
# Validation passed.
```

Validation catches missing required fields, invalid agent IDs, duplicate IDs, and schema errors.

## 3. Start the Control Plane

```bash
./bin/hived --cluster-root my-cluster
```

hived runs in the foreground, embedding a NATS server. You'll see structured JSON logs:

```json
{"level":"INFO","msg":"starting hived","cluster_root":"/path/to/my-cluster"}
{"level":"INFO","msg":"hived is ready","nats_url":"nats://127.0.0.1:4222"}
```

Leave this running and open a second terminal for the next steps.

## 4. Manage Agents

On macOS or without KVM, enable mock mode first:

```bash
export HIVE_TEST_FIRECRACKER=mock
```

### Start an agent

```bash
./bin/hivectl agents start example-agent --cluster-root my-cluster
# Agent example-agent started
```

### List agents

```bash
./bin/hivectl agents list --cluster-root my-cluster
# AGENT_ID        TEAM     STATE    UPTIME
# example-agent   default  RUNNING  5s
```

### Check agent status

```bash
./bin/hivectl agents status example-agent --cluster-root my-cluster
```

### Stop and restart

```bash
./bin/hivectl agents stop example-agent --cluster-root my-cluster
./bin/hivectl agents restart example-agent --cluster-root my-cluster
```

### Destroy

```bash
./bin/hivectl agents destroy example-agent --cluster-root my-cluster
```

## Next Steps

- **Add more agents**: Create a new directory under `my-cluster/agents/` with a `manifest.yaml`. See [Schemas](schemas.md) for the full manifest specification.
- **Join Tier 2 devices**: Use `hive-agent join` to connect Raspberry Pis and other Linux devices. See [Operations Guide](operations.md#7-join-a-tier-2-native-agent).
- **Connect firmware devices**: Build and flash Tier 3 agents with the C or MicroPython SDK. See [Firmware](firmware.md).
- **Set up RBAC**: Create users with admin, operator, or viewer roles. See [Operations Guide](operations.md#8-rbac-and-user-management).
