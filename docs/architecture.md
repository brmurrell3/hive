# Hive Architecture Specification

## CONCEPTS

### Agent
- Logical unit of work or capability
- Identity: ID (format: [a-z0-9][a-z0-9-]{0,62}), labels, optional team membership
- Transport: NATS bus
- Execution: backed by VM or process
- Manifest declares: identity, team, capabilities, resources, execution config

### Node
Physical cluster device, self-registered, classified by tier:

**Tier 1 - Full Compute**
- Requirements: Linux, KVM, 4GB+ RAM, x86_64 or arm64
- Capabilities: Firecracker micro-VMs, multi-agent, VM isolation
- OS: NixOS (official), others compatible
- Examples: workstations, servers, NUCs
- Agent execution: each agent in dedicated Firecracker VM (isolated kernel, memory ceiling, network namespace)
- Control plane: runs on Tier 1 nodes

**Tier 2 - Single-Agent Linux**
- Requirements: Linux + systemd, 512MB+ RAM, any Go-supported arch
- Capabilities: one native agent as process, process isolation, direct hardware access
- OS: any Linux (NixOS images provided as convenience)
- Examples: Raspberry Pi, BeagleBone, Jetson Nano, old laptops
- Agent execution: hive-agent binary runs agent runtime as child process, sidecar logic in-process

**Classification at join:**
```
if kvm_available AND memory >= 4GB: Tier 1
else: Tier 2
```

**Native agents on Tier 1:**
Native agents CAN run on Tier 1 nodes as processes managed by hived (not in VMs). Valid for agents requiring direct host hardware access. Execution: managed by hived directly, same as native on Tier 2 but on a Tier 1 host.

### Team
- Named group of agents
- NATS namespace: team.TEAM_ID.*
- Optional: shared resources (volumes for VM agents), lead agent
- Any agent from any tier can join
- Lead agent must be team member

### Capabilities
- Declared functions an agent performs
- Declared in agent manifest
- Purposes: discovery (agent capability enumeration), tool generation (LLM auto-tooling)
- Constraint: unique within agent

### Communication Bus (NATS)
- Universal transport
- Patterns: direct messaging, team broadcast, request-reply, persistent (JetStream)
- Subject hierarchy: agent ID and team ID organized

### Control Plane
- Runs on Tier 1 node(s)
- Binary: hived (single Go executable)
- Responsibilities: cluster state, agent lifecycle, scheduling, team management, node discovery, capability routing
- Loop: reconciliation (desired state from manifests vs actual state from agents), runs every 5s or on filesystem change
- Idempotent: all actions safe to replay

### Cluster Root
Directory structure:
```
cluster.yaml                          # cluster config
agents/AGENT_ID/
  manifest.yaml                       # agent declaration
  (runtime files)
teams/TEAM_ID.yaml                   # team definitions
.state/
  nodes/NODE_ID.json                 # node records (control-plane managed)
  agents/AGENT_ID/
    vm.json                          # VM state
    workspace/                       # agent working directory
  cluster/
    desired.json                     # validated desired state
    actual.json                      # actual state
    tokens.json                      # hashed join tokens
    capabilities.json                # capability registry cache
    allocations.json                 # resource allocations
```

Notes:
- Node records NOT in cluster root; nodes self-register
- Control plane stores node records in .state/nodes/

### Users (Optional)
- Defined in cluster.yaml: spec.users
- Enables multi-user access control
- Roles: operator (full assigned access), viewer (read-only)
- Primary operator (filesystem access to cluster root): full authority
- Auth: tokens, SHA-256 hashed in config

## COMPONENT MAP

Go packages in hived:

| Package | Purpose |
|---------|---------|
| internal/config | cluster.yaml and agent/team manifest parsing and validation |
| internal/nats | embedded NATS server wrapper with TLS, JetStream, and hardened settings |
| internal/state | SQLite persistence: agents, nodes, tokens, capabilities (schema versioned) |
| internal/vm | Firecracker VM lifecycle: PENDING → SCHEDULED → CREATING → STARTING → RUNNING → STOPPING → STOPPED (or FAILED → DESTROYING) |
| internal/sidecar | agent runtime HTTP API, NATS heartbeats, control message handler |
| internal/capability | capability registry, tool generation, capability routing, panic recovery, timeout, dedup |
| internal/health | heartbeat monitoring and auto-restart with exponential backoff |
| internal/reconciler | compare desired vs actual, generate actions, idempotent, runs every 5s or on change |
| internal/scheduler | VM assignment to Tier 1 nodes (filter: tier/arch/resources/labels, score: availability + team co-location) |
| internal/watcher | fsnotify on cluster root, debounce 500ms, emits DesiredStateChange |
| internal/auth | RBAC: token validation, user authorization (admin/operator/viewer) |
| internal/token | join token generation and validation (SHA-256 hashed, MaxUses support) |
| internal/node | node registry: join, heartbeat, offline detection, rate limiting |
| internal/cluster | multi-node clustering with TLS, auth, and reconnect jitter |
| internal/secrets | secret management for agents and cluster |
| internal/events | internal event bus for component coordination |
| internal/metrics | Prometheus /metrics with bounded cardinality and stale series cleanup |
| internal/logs | log aggregation via NATS (SQLite, sanitized levels, bounded followers) |
| internal/dashboard | REST and WebSocket API with per-user RBAC |
| internal/production | graceful shutdown, crash recovery, rate limiting, resource monitoring |
| internal/types | shared types: Envelope, CorrelationID, subject/capability validation |
| internal/backend/firecracker | Firecracker backend implementation |
| internal/backend/process | process backend implementation for native agents |

## BINARIES

**hived**: control plane daemon
- Runs on: Tier 1 nodes only
- Usage: `hived --config CLUSTER_ROOT [--log-level debug|info|warn|error] [--log-format text|json]`

**hivectl**: management CLI
- Runs: anywhere with network access
- Usage: `hivectl [--config PATH | --control-plane ADDRESS] COMMAND`

**hive-agent**: Tier 2 agent host
- Type: single static Go binary
- Usage: `hive-agent --config /etc/hive/config.yaml`

**hive-sidecar**: standalone sidecar binary for VMs
- Type: single static Go binary
- Usage: runs inside Firecracker VMs to provide agent runtime and NATS connectivity

## EXECUTION MODEL SUMMARY

| Configuration | Execution | Isolation | Hardware Access |
|---------------|-----------|-----------|-----------------|
| Tier 1 + vm-tier agent | Firecracker micro-VM | full (kernel, memory, network) | indirect |
| Tier 1 + native-tier agent | process on host (managed by hived) | process-level | direct |
| Tier 2 + native-tier agent | process (managed by hive-agent) | process-level | direct |
| Tier 1 (no agents) | — | — | compute capacity only |

**Agent participation:**
- Any tier agent: connects to NATS, joins teams, exposes capabilities
- Tier-transparent capability invocation: LLM queries distributed via same mechanism regardless of agent tier
