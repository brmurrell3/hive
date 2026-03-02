# Getting Started

Scaffold a cluster, start the control plane, and manage agents.

## Prerequisites

- Go 1.25+ installed
- Binaries built: `make build`
- On macOS or without KVM: `export HIVE_TEST_FIRECRACKER=mock`

## 1. Scaffold a Cluster

```bash
./bin/hivectl init my-cluster
```

This creates:

```
my-cluster/
├── cluster.yaml
├── agents/
│   └── example-agent/
│       └── manifest.yaml
└── teams/
    └── default.yaml
```

## 2. Validate

```bash
./bin/hivectl validate --cluster-root my-cluster
# Validation passed.
```

## 3. Start the Control Plane

```bash
./bin/hived --cluster-root my-cluster
```

hived runs in the foreground with an embedded NATS server. Leave it running and open a second terminal.

## 4. Manage Agents

On macOS or without KVM:

```bash
export HIVE_TEST_FIRECRACKER=mock
```

```bash
# Start
./bin/hivectl agents start example-agent --cluster-root my-cluster

# List
./bin/hivectl agents list --cluster-root my-cluster

# Status
./bin/hivectl agents status example-agent --cluster-root my-cluster

# Stop / Restart / Destroy
./bin/hivectl agents stop example-agent --cluster-root my-cluster
./bin/hivectl agents restart example-agent --cluster-root my-cluster
./bin/hivectl agents destroy example-agent --cluster-root my-cluster
```

## Next Steps

- **Add agents**: Create `my-cluster/agents/<id>/manifest.yaml`. See [Schemas](schemas.md).
- **Join Tier 2 devices**: Use `hive-agent join` on a Raspberry Pi. See [Operations](operations.md).
- **Set up RBAC**: Create users with roles. See [Operations](operations.md).
- **Monitor**: Enable the dashboard and metrics in `cluster.yaml`. See [Operations](operations.md).
