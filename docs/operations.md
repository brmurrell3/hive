# Hive Operations Guide

Complete operational reference for deploying, configuring, and maintaining Hive clusters. For a quick tutorial, see [Getting Started](getting-started.md).

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Installation](#installation)
3. [Configuration Reference](#configuration-reference)
4. [Agent Lifecycle](#agent-lifecycle)
5. [Join Tokens](#join-tokens)
6. [Node Management](#node-management)
7. [Tier 2 Native Agents](#tier-2-native-agents)
8. [RBAC and User Management](#rbac-and-user-management)
9. [Dashboard and Web UI](#dashboard-and-web-ui)
10. [Prometheus Metrics](#prometheus-metrics)
11. [Log Aggregation](#log-aggregation)
12. [Network Policy](#network-policy)
13. [Backup and Recovery](#backup-and-recovery)
14. [Production Hardening](#production-hardening)
15. [Troubleshooting](#troubleshooting)

---

## Prerequisites

### Hardware requirements

| Deployment | CPU | RAM | Disk | KVM |
|-----------|-----|-----|------|-----|
| Local development (process backend) | Any | 1 GB+ | 1 GB | Not required |
| Production (Firecracker VMs) | x86_64 or arm64 | 4 GB+ | 10 GB+ | Required |
| Tier 2 node (Raspberry Pi, etc.) | Any Go-supported arch | 512 MB+ | 1 GB | Not required |

### Software requirements

| Component | Version | Notes |
|-----------|---------|-------|
| Go | 1.25+ | Required for building from source |
| Linux kernel | 4.14+ | For Firecracker (KVM support) |
| Firecracker | v1.6.0+ | For VM isolation (Linux only) |
| Docker | Any recent | For building Alpine rootfs (optional) |
| Nix | With flakes | For building NixOS rootfs (optional) |

### Kernel modules (Linux, VM mode only)

```bash
# Verify KVM is available
ls -la /dev/kvm

# Load kernel modules if needed
sudo modprobe kvm
sudo modprobe kvm_intel    # Intel CPUs
sudo modprobe kvm_amd      # AMD CPUs

# Persistent: add to /etc/modules-load.d/kvm.conf
echo -e "kvm\nkvm_intel" | sudo tee /etc/modules-load.d/kvm.conf

# Grant access to your user
sudo usermod -aG kvm $USER
```

### Required Linux capabilities (Firecracker VM mode)

Running Hive with Firecracker VMs requires the following system capabilities and resources:

| Requirement | Purpose |
|-------------|---------|
| `CAP_NET_ADMIN` | Creating TAP devices and managing nftables rules for VM networking |
| `CAP_SYS_ADMIN` | Accessing `/dev/kvm` for hardware virtualization |
| `/dev/kvm` | KVM device node must be readable/writable by the hived process |
| `vhost_vsock` kernel module | Required for virtio-vsock host-guest communication |
| `nft` binary in PATH | nftables CLI for applying egress firewall rules |
| `ip` binary in PATH | iproute2 CLI for TAP device creation and network configuration |

```bash
# Load the vhost_vsock module
sudo modprobe vhost_vsock

# Persistent: add to /etc/modules-load.d/vhost_vsock.conf
echo "vhost_vsock" | sudo tee /etc/modules-load.d/vhost_vsock.conf

# Verify required binaries are available
which nft ip

# If running hived without root, grant capabilities to the binary:
sudo setcap 'cap_net_admin,cap_sys_admin+ep' ./bin/hived
```

### Installing Firecracker (Linux only)

Firecracker is a lightweight VMM (virtual machine monitor) developed by AWS. It is only supported on Linux with KVM.

**Download from GitHub releases (Ubuntu/Debian):**

```bash
# Check the latest release at https://github.com/firecracker-microvm/firecracker/releases
ARCH=$(uname -m)  # x86_64 or aarch64
FC_VERSION="1.6.0"

# Download and extract
curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/v${FC_VERSION}/firecracker-v${FC_VERSION}-${ARCH}.tgz" \
  -o firecracker.tgz
tar xzf firecracker.tgz
sudo mv release-v${FC_VERSION}-${ARCH}/firecracker-v${FC_VERSION}-${ARCH} /usr/local/bin/firecracker
rm -rf release-v${FC_VERSION}-${ARCH} firecracker.tgz

# Verify installation
firecracker --version
```

**Required kernel modules:**

```bash
# KVM (hardware virtualization)
sudo modprobe kvm
sudo modprobe kvm_intel    # Intel CPUs
sudo modprobe kvm_amd      # AMD CPUs

# vhost_vsock (host-guest communication)
sudo modprobe vhost_vsock

# Verify
ls -la /dev/kvm
ls -la /dev/vhost-vsock
```

---

## Installation

### From source

```bash
git clone https://github.com/brmurrell3/hive && cd hive
make build
```

Produces binaries in `./bin/`:
- `hived` -- control plane daemon
- `hivectl` -- management CLI
- `hive-agent` -- Tier 2 agent host binary

### Cross-compilation

```bash
# Linux x86_64 (servers, VMs)
make build-linux-amd64

# Linux arm64 (Raspberry Pi)
make build-linux-arm64

# macOS Intel
make build-darwin-amd64

# All targets
make build-all
```

### VM images (Linux only)

```bash
# Download Firecracker-compatible kernel
make download-kernel

# Build Alpine rootfs (requires Docker)
make rootfs

# Build NixOS rootfs (requires Nix with flakes)
cd rootfs/nixos && nix build .#rootfs && nix build .#kernel
```

---

## Configuration Reference

### cluster.yaml

The cluster configuration file lives at the root of the cluster directory. Full schema: [Schemas](schemas.md).

```yaml
apiVersion: hive/v1
kind: Cluster
metadata:
  name: my-cluster
spec:
  nats:
    port: 4222                    # NATS client port
    clusterPort: 6222             # NATS cluster peering (multi-node)
    jetstream:
      enabled: true
      maxMemory: "1GB"
      maxStorage: "10GB"
  vm:
    kernelPath: /path/to/vmlinux       # Firecracker kernel
    rootfsPath: /path/to/rootfs.ext4   # Firecracker rootfs
  defaults:
    resources:
      memory: "512Mi"
      vcpus: 1
      disk: "5GB"
    network:
      egress: restricted                # none, restricted, full
    health:
      enabled: true
      interval: "30s"
      timeout: "5s"
      maxFailures: 3
    restart:
      policy: on-failure               # always, on-failure, never
      maxRestarts: 5
      backoff: "10s"
  nodes:
    autoApprove: true                  # Auto-approve Tier 2 join requests
  secrets:
    ANTHROPIC_KEY: ""                  # Empty = read from HIVE_SECRET_ANTHROPIC_KEY env
```

### Agent manifest

Each agent is defined in `agents/<agent-id>/manifest.yaml`:

```yaml
apiVersion: hive/v1
kind: Agent
metadata:
  id: my-agent                   # Unique, [a-z0-9][a-z0-9-]{0,62}
  team: default                  # References a team ID
  labels:
    role: worker
spec:
  tier: vm                       # vm or native (auto-inferred if omitted)
  runtime:
    type: openclaw               # openclaw, custom, or process
    model:
      provider: anthropic
      name: claude-sonnet-4-5
  capabilities:
    - name: summarize
      description: Summarize text input
      inputs:
        - name: text
          type: string
          description: Text to summarize
      outputs:
        - name: summary
          type: string
          description: The summary
  resources:
    memory: "512Mi"
    vcpus: 2
    disk: "5GB"
  network:
    egress: restricted
    egress_allowlist:
      - "api.anthropic.com"
  health:
    interval: "10s"
    timeout: "3s"
    maxFailures: 5
  restart:
    policy: on-failure
    maxRestarts: 3
    backoff: "5s"
```

### Team manifest

Teams are defined in `teams/<team-id>.yaml`:

```yaml
apiVersion: hive/v1
kind: Team
metadata:
  id: ci-pipeline
spec:
  lead: code-reviewer            # Agent ID of team lead
  resources:
    maxMemory: "4Gi"
    maxAgents: 10
  communication:
    persistent: true             # Enable JetStream for offline message delivery
    historyDepth: 100
  shared_volumes:
    - name: shared-data
      hostPath: /data/shared
      access: read-write
```

---

## Agent Lifecycle

### State machine

```
PENDING --> CREATING --> STARTING --> RUNNING --> STOPPING --> STOPPED
                 \                      \          \
                  <-------- FAILED <-----<----------<
STOPPED --> CREATING  (restart)
FAILED  --> CREATING  (restart)
```

### Commands

```bash
# Start (PENDING -> CREATING -> STARTING -> RUNNING)
./bin/hivectl agents start my-agent --cluster-root my-cluster

# Stop (RUNNING -> STOPPING -> STOPPED)
./bin/hivectl agents stop my-agent --cluster-root my-cluster

# Restart (stop + start, resets restart counter)
./bin/hivectl agents restart my-agent --cluster-root my-cluster

# Destroy (force kill, delete rootfs copy, remove from state)
./bin/hivectl agents destroy my-agent --cluster-root my-cluster

# List all agents
./bin/hivectl agents list --cluster-root my-cluster

# Detailed status (JSON)
./bin/hivectl agents status my-agent --cluster-root my-cluster
```

### State persistence

Runtime state is stored in `<cluster-root>/state.db` (SQLite). This includes agent states, node registry, tokens, and capability registrations. Do not edit this file while hived is running.

---

## Join Tokens

Tokens authenticate Tier 2 nodes joining the cluster. The raw token is shown once at creation time; only a SHA-256 hash is stored.

```bash
# Create a token (no expiry)
./bin/hivectl tokens create --cluster-root my-cluster

# Create a token with TTL
./bin/hivectl tokens create --ttl 24h --cluster-root my-cluster

# List tokens (shows prefix, creation time, status)
./bin/hivectl tokens list --cluster-root my-cluster

# Revoke by prefix
./bin/hivectl tokens revoke a1b2c3d4 --cluster-root my-cluster
```

---

## Node Management

Nodes self-register when agents join via `hive-agent join`. Tier classification is automatic:

- **Tier 1:** KVM available AND >= 4 GB RAM (can run Firecracker VMs)
- **Tier 2:** Everything else (native agents only)

```bash
# List all nodes
./bin/hivectl nodes list --cluster-root my-cluster

# Detailed status
./bin/hivectl nodes status pi4-aarch64 --cluster-root my-cluster

# Cordon (prevent new scheduling, existing agents stay)
./bin/hivectl nodes cordon pi4-aarch64 --cluster-root my-cluster

# Drain (prevent scheduling, signal migration)
./bin/hivectl nodes drain pi4-aarch64 --cluster-root my-cluster

# Uncordon (return to online)
./bin/hivectl nodes uncordon pi4-aarch64 --cluster-root my-cluster

# Add labels
./bin/hivectl nodes label pi4-aarch64 env=prod gpu=none --cluster-root my-cluster

# Remove labels
./bin/hivectl nodes unlabel pi4-aarch64 gpu --cluster-root my-cluster
```

---

## Tier 2 Native Agents

Tier 2 agents run natively on hardware (no VM). They use `hive-agent join` to connect to the control plane.

**On the control plane host:**

```bash
./bin/hivectl tokens create --cluster-root my-cluster
# Save the token output
```

**On the Tier 2 node (e.g., Raspberry Pi):**

```bash
# Cross-compile for the target
GOOS=linux GOARCH=arm64 go build -o hive-agent ./cmd/hive-agent

# Copy to target and run
./hive-agent join \
    --token <join-token> \
    --control-plane 192.168.1.10:4222 \
    --agent-id my-pi-agent \
    --http-addr :9100 \
    --work-dir /var/lib/hive/workspace
```

The agent will connect to NATS, send a join request with hardware inventory, start the sidecar, begin heartbeats, and listen for tasks.

**Verify:**

```bash
./bin/hivectl nodes list --cluster-root my-cluster
./bin/hivectl agents list --cluster-root my-cluster
```

---

## RBAC and User Management

Three roles:

| Role | Permissions | Scope |
|------|-------------|-------|
| `admin` | All operations | All resources |
| `operator` | start, stop, restart, destroy, list, status, logs | Assigned teams/agents |
| `viewer` | list, status, logs | Assigned teams/agents |

```bash
# Create an admin
./bin/hivectl users create alice --role admin --cluster-root my-cluster

# Create an operator scoped to a team
./bin/hivectl users create bob --role operator --teams default --cluster-root my-cluster

# Create a viewer scoped to specific agents
./bin/hivectl users create carol --role viewer --agents my-agent,other-agent --cluster-root my-cluster

# List users
./bin/hivectl users list --cluster-root my-cluster

# Update role or scope
./bin/hivectl users update bob --role admin --cluster-root my-cluster

# Revoke
./bin/hivectl users revoke carol --cluster-root my-cluster
```

---

## Dashboard and Web UI

hived serves a REST API and WebSocket endpoint for real-time monitoring.

### REST API

```bash
# Cluster overview
curl http://localhost:8080/api/cluster

# List agents
curl http://localhost:8080/api/agents

# Agent detail
curl http://localhost:8080/api/agents/my-agent

# List nodes
curl http://localhost:8080/api/nodes

# Registered capabilities
curl http://localhost:8080/api/capabilities

# Chat with an agent
curl -X POST http://localhost:8080/api/agents/my-agent/chat \
  -H 'Content-Type: application/json' \
  -d '{"message":"Hello"}'

# Agent logs
curl http://localhost:8080/api/logs/my-agent?limit=50

# Health check
curl http://localhost:8080/healthz
```

### WebSocket

Connect to `ws://localhost:8080/ws` for live events:

```bash
websocat ws://localhost:8080/ws
```

Event types:
- `agent_state_change` -- state transitions
- `heartbeat` -- agent health updates
- `log_entry` -- log messages

---

## Prometheus Metrics

Exposed at `/metrics` on the dashboard port (default `:8080`).

```bash
curl http://localhost:8080/metrics
```

Add to `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'hive'
    static_configs:
      - targets: ['localhost:8080']
    scrape_interval: 15s
```

Key metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `hive_agents_total{status}` | gauge | Agent count by state |
| `hive_heartbeat_healthy{agent_id}` | gauge | 1=healthy, 0=unhealthy |
| `hive_capability_invocation_duration_ms` | summary | Capability latency |
| `hive_nats_messages_total{subject}` | counter | NATS message throughput |
| `hive_node_memory_usage_percent{node_id}` | gauge | Node memory usage |
| `hive_node_cpu_usage_percent{node_id}` | gauge | Node CPU usage |

---

## Log Aggregation

Agent logs are streamed via NATS and persisted to JSONL files.

### Storage layout

```
<cluster-root>/logs/
+-- <agent-id>/
    +-- 2026-03-14.jsonl        # One file per day
    +-- 2026-03-14.1.jsonl      # Rotated when >100MB
```

### Configuration

- Retention: 30 days (older files deleted on startup)
- Rotation: files > 100 MB rotated to `{date}.1.jsonl`, `.2.jsonl`, etc.
- Max entry size: 64 KB
- Max concurrent followers: 1000

### CLI access

```bash
./bin/hivectl agents logs my-agent --cluster-root my-cluster
./bin/hivectl agents logs my-agent --follow --cluster-root my-cluster
./bin/hivectl agents logs my-agent --tail 100 --cluster-root my-cluster
./bin/hivectl agents logs my-agent --since 1h --cluster-root my-cluster
```

---

## Network Policy

### Network topology

Hive uses three communication layers depending on the agent tier:

**Tier 1 VM agents:** Each Firecracker VM gets a TAP device and a private network namespace. The VM communicates with the host via virtio-vsock (no TCP exposure). The vsock forwarder on the host side bridges the VM's NATS traffic to the embedded NATS server. Network egress from VMs is controlled by nftables rules based on the `network.egress` setting in the agent manifest.

**Tier 1 native agents:** Agents running as processes on a Tier 1 host connect to NATS via localhost IPC. No network namespace isolation.

**Tier 2 agents:** Remote agents connect to the control plane NATS server over TCP. TLS is supported and recommended for production. The `hive-agent join` command establishes the connection using a join token for authentication.

### Egress control (VM agents only)

```yaml
# In agent manifest
spec:
  network:
    egress: restricted          # none, restricted, or full
    egress_allowlist:           # Only when egress=restricted
      - "api.anthropic.com"
      - "api.openai.com"
```

- `none` -- no outbound network access
- `restricted` -- only domains/IPs in `egress_allowlist` allowed (via nftables)
- `full` -- unrestricted outbound access

### NATS subjects

All agent communication flows through the embedded NATS server. Subject hierarchy:

| Subject | Direction | Description |
|---------|-----------|-------------|
| `hive.health.<agent_id>` | Agent -> hived | Heartbeats |
| `hive.control.<agent_id>` | hived -> Agent | Control commands |
| `hive.agent.<agent_id>.inbox` | Any -> Agent | Message inbox |
| `hive.capabilities.<agent>.<cap>.request` | Any -> Agent | Capability invocation |
| `hive.capabilities.<agent>.<cap>.response` | Agent -> Requester | Capability response |
| `hive.team.<team_id>.broadcast` | Lead -> Team | Team broadcast |
| `hive.logs.<agent_id>` | Agent -> hived | Log entries |
| `hive.join.request` | Agent -> hived | Tier 2 join |

See [Communication](communication.md) for the full subject hierarchy and envelope format.

---

## Backup and Recovery

### What to back up

| Path | Contents | Priority |
|------|----------|----------|
| `cluster.yaml` | Cluster configuration | Critical |
| `agents/*/manifest.yaml` | Agent definitions | Critical |
| `teams/*.yaml` | Team definitions | Critical |
| `state.db` | Runtime state (agent states, nodes, tokens) | High |
| `.state/agents/*/workspace/` | Agent workspace data (MEMORY.md, runtime files) | Medium |
| `.state/jetstream/` | JetStream persistent messages | Low |

### Backup procedure

```bash
CLUSTER_ROOT=/var/lib/hive/cluster
BACKUP_DIR=/backups/hive/$(date +%Y%m%d-%H%M%S)

mkdir -p "$BACKUP_DIR"

# Back up configuration (always safe to copy)
cp "$CLUSTER_ROOT/cluster.yaml" "$BACKUP_DIR/"
cp -r "$CLUSTER_ROOT/agents" "$BACKUP_DIR/"
cp -r "$CLUSTER_ROOT/teams" "$BACKUP_DIR/"

# Back up state (stop hived first for consistency, or accept point-in-time snapshot)
cp "$CLUSTER_ROOT/state.db" "$BACKUP_DIR/"

# Back up workspaces (optional, can be large)
cp -r "$CLUSTER_ROOT/.state/agents" "$BACKUP_DIR/agent-state/"
```

### Recovery procedure

```bash
# 1. Stop hived
sudo systemctl stop hived

# 2. Restore configuration
cp "$BACKUP_DIR/cluster.yaml" "$CLUSTER_ROOT/"
cp -r "$BACKUP_DIR/agents" "$CLUSTER_ROOT/"
cp -r "$BACKUP_DIR/teams" "$CLUSTER_ROOT/"

# 3. Restore state
cp "$BACKUP_DIR/state.db" "$CLUSTER_ROOT/"

# 4. Restart hived (crash recovery runs automatically)
sudo systemctl start hived
```

### State file corruption

If `state.db` becomes corrupted, delete it and restart. hived will rebuild state from manifests. Runtime state (which agents were running, restart counts) will be lost, but configuration is preserved.

```bash
rm "$CLUSTER_ROOT/state.db"
sudo systemctl restart hived
```

---

## Production Hardening

### Graceful shutdown

`SIGTERM` or `SIGINT` triggers graceful shutdown:

1. Stop accepting new connections
2. Stop all running agents in parallel
3. Wait up to 30 seconds for clean shutdown
4. Close all NATS connections
5. Exit

### Crash recovery

If hived is killed with `SIGKILL`, crash recovery runs on next startup:

1. Reads `state.db` for agents marked as RUNNING/STARTING
2. Checks if the VM/process PID is still alive
3. Marks dead agents as FAILED
4. Agents can then be restarted normally

### Rate limiting

Per-subject token bucket: 100 messages/second, burst of 100. Exceeded messages are dropped with a warning logged.

### Resource monitoring

Checks node resource usage every 30 seconds. Logs warnings when memory or CPU usage exceeds 80%.

---

## Troubleshooting

### Common errors and solutions

**"address already in use" on NATS startup**

```bash
# Change the port in cluster.yaml
spec:
  nats:
    port: 4223

# Or find and kill the existing process
lsof -i :4222
```

**"firecracker: command not found"**

```bash
# Option 1: Use process backend (any OS)
export HIVE_TEST_FIRECRACKER=mock

# Option 2: Install Firecracker (Linux only)
# See https://github.com/firecracker-microvm/firecracker/releases
```

**"/dev/kvm not available"**

```bash
# On macOS: use process backend
export HIVE_TEST_FIRECRACKER=mock

# On Linux: load kernel modules
sudo modprobe kvm
sudo modprobe kvm_intel  # or kvm_amd
sudo chmod 666 /dev/kvm
```

**Agent stuck in CREATING or STARTING**

```bash
# Force destroy, then restart
./bin/hivectl agents destroy stuck-agent --cluster-root my-cluster
./bin/hivectl agents start stuck-agent --cluster-root my-cluster
```

**NATS connection refused**

```bash
# Verify hived is running
pgrep hived

# Check NATS is listening
lsof -i :4222

# Check hived logs (stdout by default)
```

**VM boots but sidecar does not connect**

```bash
# Check serial console output
cat my-cluster/.state/agents/my-agent/console.log

# Check vsock UDS files exist
ls my-cluster/.state/agents/my-agent/*.vsock*

# Monitor NATS for heartbeats
nats sub 'hive.health.>'
```

**Capability invoke times out**

Wait for the sidecar to finish connecting (heartbeat appears on `hive.health.>`), then retry. Default timeout is 30 seconds.

**Tests fail with timeout**

```bash
go test -tags integration -race -count=1 -v -timeout 10m ./internal/...
```

### Diagnostic commands

```bash
# Validate all manifests
./bin/hivectl validate --cluster-root my-cluster

# Check cluster status
./bin/hivectl status --cluster-root my-cluster

# List everything
./bin/hivectl agents list --cluster-root my-cluster
./bin/hivectl nodes list --cluster-root my-cluster
./bin/hivectl tokens list --cluster-root my-cluster
./bin/hivectl capabilities list --cluster-root my-cluster

# Verbose hived logs
./bin/hived --cluster-root my-cluster --log-level debug

# NATS diagnostics (requires nats CLI)
nats server info
nats sub 'hive.>'
nats stream list
```

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `HIVE_TEST_FIRECRACKER` | _(unset)_ | Set to `mock` to use process backend |
| `HIVE_CONFIG` | _(unset)_ | Path to cluster root |
| `HIVE_CONTROL_PLANE` | _(unset)_ | Remote control plane address |
| `HIVE_USER` | _(unset)_ | User ID for RBAC |
| `HIVE_TOKEN` | _(unset)_ | Auth token for RBAC |

## File Reference

| File | Owned By | Purpose |
|------|----------|---------|
| `cluster.yaml` | Operator | Cluster configuration |
| `agents/*/manifest.yaml` | Operator | Agent definitions |
| `teams/*.yaml` | Operator | Team definitions |
| `state.db` | hived | Runtime state (do not edit while hived runs) |
| `.state/jetstream/` | NATS | JetStream persistence |
| `.state/agents/*/` | hived | Per-agent VM artifacts (sockets, rootfs copies) |
| `.state/nats-auth-token` | hived | Auto-generated NATS auth token |
