# Hive Architecture

## Concepts

### Agent

The fundamental unit of work in Hive. An agent is a program that exposes capabilities (functions) and communicates with other agents over NATS.

- **Identity:** ID (format: `[a-z0-9][a-z0-9-]{0,62}`), labels, optional team membership
- **Transport:** NATS message bus
- **Execution:** backed by a Firecracker VM (Tier 1) or OS process (Tier 1 native / Tier 2)
- **Manifest:** declares identity, team, capabilities, resources, and runtime configuration

### Node

A physical or virtual machine in the cluster. Self-registered, classified by tier:

**Tier 1 -- Full Compute**
- Requirements: Linux, KVM, 4 GB+ RAM, x86_64 or arm64
- Capabilities: Firecracker microVMs, multi-agent hosting, VM isolation
- Agent execution: each agent in a dedicated Firecracker VM with isolated kernel, memory ceiling, and network namespace
- The control plane runs on Tier 1 nodes

**Tier 2 -- Single-Agent Linux**
- Requirements: Linux + systemd, 512 MB+ RAM, any Go-supported architecture
- Capabilities: one native agent as a process, direct hardware access
- Agent execution: `hive-agent` binary runs agent runtime as a child process with sidecar logic in-process
- Examples: Raspberry Pi, BeagleBone, Jetson Nano

**Tier classification at join time:**
```
if kvm_available AND memory >= 4 GB: Tier 1
else: Tier 2
```

Native agents can also run on Tier 1 nodes as processes managed by hived (not in VMs), for agents requiring direct host hardware access.

### Team

A named group of agents with optional shared resources and a lead agent.

- NATS namespace: `hive.team.<team_id>.*`
- Optional: shared volumes (for VM agents), lead agent designation
- Any agent from any tier can join a team
- Lead agent must be a team member

### Capability

A declared function that an agent can perform. Defined in the agent manifest with typed inputs and outputs. Used for:
- **Discovery:** other agents enumerate available capabilities
- **Tool generation:** LLM-backed agents auto-generate tool definitions
- **Routing:** the capability router dispatches invocations via NATS

### Control Plane

Runs on Tier 1 nodes. Single binary: `hived`.

Responsibilities:
- Cluster state management (SQLite)
- Agent lifecycle (create, start, stop, destroy)
- Bin-packing scheduler for VM placement
- Reconciliation loop (5-second interval, or on filesystem change)
- Capability registry and routing
- Health monitoring with auto-restart
- RBAC enforcement

---

## Component Map

| Package | Purpose |
|---------|---------|
| `internal/config` | YAML parsing + validation for cluster, agent, and team manifests |
| `internal/nats` | Embedded NATS server with TLS, JetStream, hardened cipher suites |
| `internal/state` | SQLite persistence: agents, nodes, tokens, capabilities (schema versioned) |
| `internal/vm` | Firecracker VM lifecycle management |
| `internal/sidecar` | Agent runtime HTTP API, NATS heartbeats, control message handler |
| `internal/capability` | Capability registry, NATS routing, cross-team support, dedup, timeouts |
| `internal/health` | Heartbeat monitoring and auto-restart with exponential backoff |
| `internal/reconciler` | Desired-state reconciliation (manifests vs actual state) |
| `internal/scheduler` | Bin-packing scheduler with team co-location scoring |
| `internal/watcher` | fsnotify on cluster root with 500 ms debounce |
| `internal/auth` | RBAC: admin, operator, viewer roles |
| `internal/token` | Join token generation and validation (SHA-256 hashed) |
| `internal/node` | Node registry: join, heartbeat, offline detection |
| `internal/cluster` | Multi-node clustering with TLS and reconnect jitter |
| `internal/federation` | Cluster federation with peer allowlists |
| `internal/director` | Director agent with org-wide management tools |
| `internal/dashboard` | REST + WebSocket API with per-user RBAC |
| `internal/metrics` | Prometheus /metrics with bounded cardinality |
| `internal/logs` | Log aggregation via NATS with SQLite storage |
| `internal/production` | Graceful shutdown, crash recovery, rate limiting, resource monitoring |
| `internal/plugin` | Plugin system with lifecycle management |
| `internal/types` | Shared types: Envelope, CorrelationID, subject validation |
| `internal/mqtt` | MQTT-NATS bridge for IoT integration |
| `internal/firmware` | Tier 3 firmware tracking, OTA updates, Ed25519 signing |

---

## Binaries

**hived** -- Control plane daemon
- Runs on Tier 1 nodes
- Usage: `hived --cluster-root PATH [--log-level debug|info|warn|error]`
- Embeds NATS server, SQLite state, reconciler, scheduler, health monitor

**hivectl** -- Management CLI
- Runs anywhere with network access
- Usage: `hivectl [--cluster-root PATH | --control-plane ADDRESS] COMMAND`
- Commands: init, validate, dev, trigger, agents, tokens, nodes, users, capabilities, status

**hive-agent** -- Tier 2 agent host
- Single static Go binary for edge devices
- Usage: `hive-agent join --token TOKEN --control-plane HOST:PORT --agent-id ID`

**hive-sidecar** -- VM sidecar binary
- Runs inside Firecracker VMs
- Provides agent runtime, HTTP API (:9100), NATS connectivity via vsock

---

## Firecracker VM Lifecycle

When an agent with `tier: vm` is started, hived provisions a Firecracker microVM through the following stages:

### 1. Resource allocation

The scheduler selects a Tier 1 node based on available memory, CPU, and team co-location. Resources (memory, vCPUs, disk) are reserved atomically from the node's pool.

### 2. VM creation

hived prepares the VM:
- Copies the base rootfs image to `.state/agents/<agent-id>/rootfs.ext4`
- Creates an agent drive image (`.state/agents/<agent-id>/agent-drive.ext4`) containing:
  - `sidecar.conf` with agent configuration (agent ID, team, NATS auth token, capabilities)
  - Agent files (manifest, entrypoint, workspace files)
- Allocates a unique CID (Context ID) for vsock communication
- Spawns the Firecracker process with the API socket at `.state/agents/<agent-id>/firecracker.sock`
- Configures the VM via the Firecracker API: kernel, rootfs drive, agent drive, memory, vCPUs, vsock device

### 3. Network setup

For each VM:
- A TAP device is created for the VM's network interface
- nftables rules are applied based on the agent's `network.egress` setting:
  - `none`: all outbound traffic blocked
  - `restricted`: only traffic to domains/IPs in `egress_allowlist` permitted
  - `full`: all outbound traffic allowed
- The vsock forwarder starts on the host side, binding a Unix domain socket that bridges the VM's vsock CID to the host NATS server

### 4. Boot sequence

Inside the VM:
1. Firecracker boots the Linux kernel with init parameters
2. The init script mounts proc, sys, devtmpfs
3. The agent drive is mounted at `/workspace`
4. The sidecar binary starts, reads `sidecar.conf`
5. The sidecar connects to the host NATS server via vsock
6. The sidecar begins publishing heartbeats on `hive.health.<agent-id>`
7. The sidecar registers capabilities on `hive.capabilities.registry`
8. The sidecar starts the agent runtime (OpenClaw, custom script, etc.)

### 5. Steady state

Once running:
- Heartbeats are published every `health.interval` (default 30 seconds)
- The health monitor on hived tracks heartbeat arrivals
- Capabilities are invocable via NATS request-reply
- Manifest changes trigger hot-reload (MEMORY.md, skills) or cold-reload (restart)

### 6. Shutdown and cleanup

On stop or destroy:
- hived sends a shutdown command via `hive.control.<agent-id>`
- The sidecar performs graceful shutdown of the agent runtime
- The Firecracker process is terminated
- nftables rules for this VM are removed
- The vsock forwarder is stopped
- CID is reclaimed for reuse
- On destroy: rootfs copy, agent drive, and socket files are deleted

---

## Network Topology

### Tier 1 VM agent communication

```
+---------------------------+
|        Tier 1 Host        |
|                           |
|  +-------+    +--------+  |
|  | hived |----| NATS   |  |
|  +-------+    | Server |  |
|               +---+----+  |
|                   |        |
|            vsock forwarder |
|                   |        |
|  - - - - - - - - -|- - -  |
|  | Firecracker VM |       |
|  |                |       |
|  |  +---------+   |       |
|  |  | sidecar |   |       |
|  |  +----+----+   |       |
|  |       |         |       |
|  |   vsock proxy   |       |
|  |                  |       |
|  |  +---------+    |       |
|  |  |  agent  |    |       |
|  |  | runtime |    |       |
|  |  +---------+    |       |
|  +------------------+      |
|                           |
|  TAP device + nftables     |
|  (egress control)          |
+---------------------------+
```

- **vsock:** Bidirectional host-guest channel. No TCP ports exposed from the VM. The sidecar inside the VM connects to a vsock CID:PORT, which the host-side vsock forwarder bridges to the NATS server.
- **TAP + nftables:** Each VM gets a TAP network device. nftables rules control egress based on the agent manifest's `network.egress` setting.
- **NATS:** All agent-to-agent and agent-to-control-plane communication flows through the embedded NATS server. Agents never communicate directly with each other.

### Tier 2 agent communication

```
+----------------+          +------------------+
| Tier 2 Node    |   TCP    |   Tier 1 Host    |
| (e.g., RPi)    |  / TLS   |                  |
|                |--------->|   NATS Server    |
| +------------+ |          |                  |
| | hive-agent | |          |   +-------+      |
| | + sidecar  | |          |   | hived |      |
| +------------+ |          |   +-------+      |
+----------------+          +------------------+
```

Tier 2 agents connect to the control plane NATS server over TCP (with optional TLS). Authentication uses join tokens. The `hive-agent` binary includes the sidecar logic in-process.

### Multi-node clustering

```
+------------------+    NATS cluster    +------------------+
|   Node 1 (T1)    |<----------------->|   Node 2 (T1)    |
|   hived + NATS   |    port 6222      |   hived + NATS   |
|   agents A, B    |   (TLS + auth)    |   agents C, D    |
+------------------+                    +------------------+
        |                                       |
        v                                       v
+------------------+                    +------------------+
|   Node 3 (T2)    |                    |   Node 4 (T2)    |
|   hive-agent E   |                    |   hive-agent F   |
+------------------+                    +------------------+
```

Multiple Tier 1 nodes form a NATS cluster. Agents on any node can invoke capabilities on agents running on any other node. The scheduler distributes VMs across nodes using bin-packing with team co-location preferences.

---

## Capability Routing Flow

When Agent B invokes a capability on Agent A:

```
Agent B                  Sidecar B           NATS            Sidecar A           Agent A
   |                        |                  |                 |                  |
   |-- invoke("summarize")-->|                  |                 |                  |
   |                        |                  |                 |                  |
   |                        |-- NATS request -->|                 |                  |
   |                        | hive.capabilities |                 |                  |
   |                        | .agent-a.summarize|                 |                  |
   |                        | .request          |                 |                  |
   |                        |                  |-- deliver ------>|                  |
   |                        |                  |                 |                  |
   |                        |                  |                 |-- HTTP POST ----->|
   |                        |                  |                 |  /handle/summarize|
   |                        |                  |                 |                  |
   |                        |                  |                 |<-- JSON outputs --|
   |                        |                  |                 |                  |
   |                        |                  |<-- NATS reply ---|                  |
   |                        |                  |                 |                  |
   |                        |<-- response -----|                 |                  |
   |                        |                  |                 |                  |
   |<-- outputs ------------|                  |                 |                  |
```

Key details:
- **Request path:** Agent B's sidecar publishes to `hive.capabilities.<agent-a>.<capability>.request` with a `reply_to` subject
- **Response path:** Agent A's sidecar publishes the result to the `reply_to` subject
- **Timeout:** configurable per-invocation, default 30 seconds
- **Cross-team:** works identically across teams; the NATS subject includes the target agent ID
- **Deduplication:** the capability router tracks message IDs to prevent duplicate processing
- **Circuit breaker:** per-agent, per-capability; trips after 5 consecutive errors within 60 seconds

### Capability discovery

When an agent registers or deregisters capabilities, it publishes to `hive.capabilities.registry`. All sidecars subscribe to this subject and update their local tool definitions. This enables:
- Auto-discovery of new team member capabilities
- LLM-backed agents to generate tool definitions automatically
- Cross-team capability routing when configured

---

## Agent Lifecycle State Machine

```
                      +----------+
                      | PENDING  |
                      +----+-----+
                           |
                    start  |
                           v
                      +----------+
                +---->| CREATING |
                |     +----+-----+
                |          |
                |   create |  (provision VM / spawn process)
                |          v
                |     +----------+
                |     | STARTING |
                |     +----+-----+
                |          |
                |    boot  |  (sidecar connects, heartbeat arrives)
                |          v
                |     +----------+
                |     | RUNNING  |----+
                |     +----+-----+    |
                |          |          |
                |    stop  |          | failure / crash
                |          v          |
                |     +----------+    |
                |     | STOPPING |    |
                |     +----+-----+    |
                |          |          |
                |          v          v
                |     +----------+  +--------+
                |     | STOPPED  |  | FAILED |
                |     +----+-----+  +----+---+
                |          |             |
                |  restart |     restart |
                +----------+-------------+
```

State transitions:
- `PENDING -> CREATING`: `hivectl agents start` or reconciler action
- `CREATING -> STARTING`: VM provisioned or process spawned
- `STARTING -> RUNNING`: first heartbeat received from sidecar
- `RUNNING -> STOPPING`: `hivectl agents stop` or graceful shutdown
- `STOPPING -> STOPPED`: agent process/VM terminated cleanly
- `RUNNING -> FAILED`: heartbeat timeout (maxFailures consecutive misses), VM crash, or process exit
- `CREATING -> FAILED`: VM provisioning error, resource allocation failure
- `STARTING -> FAILED`: boot timeout, sidecar connection failure
- `STOPPED -> CREATING`: `hivectl agents restart` or `on-failure` restart policy
- `FAILED -> CREATING`: `hivectl agents restart` or auto-restart (up to maxRestarts)

Auto-restart behavior is controlled by the `restart` configuration:
- `always`: restart on any termination
- `on-failure`: restart only on FAILED state (not clean stop)
- `never`: do not auto-restart
- Exponential backoff between restart attempts (configurable `backoff` duration)
- Maximum restart count (configurable `maxRestarts`)

---

## Execution Model

| Configuration | Execution | Isolation | Hardware Access |
|---------------|-----------|-----------|-----------------|
| Tier 1 + vm-tier agent | Firecracker microVM | Full (kernel, memory, network) | Indirect (via vsock) |
| Tier 1 + native-tier agent | Process on host (managed by hived) | Process-level | Direct |
| Tier 2 + native-tier agent | Process (managed by hive-agent) | Process-level | Direct |

Agent participation is tier-transparent: any agent connects to NATS, joins teams, and exposes capabilities using the same protocol regardless of whether it runs in a VM or as a native process.

---

## Cluster Root Directory

```
cluster.yaml                        # Cluster configuration
agents/<agent-id>/
  manifest.yaml                     # Agent declaration
  entrypoint.sh                     # Optional custom runtime entry
  AGENTS.md                         # Optional OpenClaw instructions
  MEMORY.md                         # Optional OpenClaw memory
teams/<team-id>.yaml                # Team definitions
.state/
  agents/<agent-id>/
    rootfs.ext4                     # VM rootfs copy
    agent-drive.ext4                # Agent files drive
    firecracker.sock                # Firecracker API socket
    console.log                     # Serial console output
    workspace/                      # Agent working directory
  jetstream/                        # NATS JetStream data
  nats-auth-token                   # Auto-generated NATS auth token
state.db                            # SQLite runtime state
```
