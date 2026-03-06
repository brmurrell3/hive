# 04-CONTROL-PLANE.md

Consolidated control plane spec for Hive. Format optimized for Claude Code. Terse, structured, deterministic. Minimized prose.

## OVERVIEW

**hived**: single Go binary on Tier 1 (control plane node). Reconciliation loop: desired state (manifests on disk) vs actual state (running agents, registered nodes). Converges to desired.

---

## NATS MANAGEMENT (resolves review #6)

**Phase 1 (single-node)**: NATS embedded in hived
- hived starts NATS server in-process via nats-server Go library
- No separate systemd service
- Simplifies single-node deployment
- NixOS module: NO nats.service created

**Phase 3 (multi-node)**: NATS external
- Each Tier 1 node runs nats-server as separate systemd service
- hived connects to NATS as client
- Independent clustering support
- NixOS module: creates nats.service

**Detection** (in hived):
- Read cluster.yaml
- If `spec.nats.clusterPeers` empty or absent → embedded mode
- If `spec.nats.clusterPeers` has entries → external mode

**NixOS config**:
```
services.hive.nats.mode = enum(embedded|external), default: embedded
  embedded → hived manages NATS lifecycle
  external → NixOS creates nats.service, hived connects as client
```

---

## COMPONENTS

### Watcher
**Package**: `internal/watcher`
**Function**: Monitor cluster root for manifest changes (fsnotify-based)

**Behavior**:
- Debounce: 500ms
- Full scan on startup
- On change: parse, validate, emit DesiredStateChange to reconciler
- Validation failure: log error, retain previous valid state
- Watches: `cluster.yaml`, `agents/**/manifest.yaml`, `teams/*.yaml`

### Reconciler
**Package**: `internal/reconciler`
**Function**: Compare desired vs actual state, generate actions
**Loop**: Every 5s OR immediately on DesiredStateChange
**Critical**: MUST be idempotent

**Action set**:
```
CreateVM(agent_id, spec)
DestroyVM(agent_id)
StartNativeAgent(agent_id, node_id)
StopNativeAgent(agent_id)
RegisterFirmwareAgent(agent_id)
DeregisterFirmwareAgent(agent_id)
CreateTeamNamespace(team_id)
DestroyTeamNamespace(team_id)
UpdateCapabilityRouting(agent_id)
InjectLeadTools(agent_id)
RemoveLeadTools(agent_id)
InjectDirectorTools(agent_id)
RemoveDirectorTools(agent_id)
ScheduleAgent(agent_id, node_id)
```

### Node Registry
**Package**: `internal/noderegistry`
**Function**: Inventory of all nodes across all tiers

**Node record**:
```
{
  nodeId: string              (auto-generated at join)
  tier: int                   (1, 2, 3)
  arch: string                (amd64, arm64, armv7, armv6, rp2040, esp32, ...)
  resources: {
    memory: bytes             (Tier 1/2 only)
    vcpus: int                (Tier 1/2 only)
    disk: bytes               (Tier 1/2 only)
  }
  hardware: {
    peripherals: [...]        (from inventory scan)
  }
  status: enum                (online|offline|pending|draining|cordoned)
  lastHeartbeat: timestamp
  agents: [agent_id, ...]     (running on this node)
  labels: map[string]string   (auto-generated + user-provided)
}
```

**Storage**: `.state/nodes/{NODE_ID}.json`

**Heartbeat timeout**:
- Tier 1/2: offline after 3 missed heartbeats @ 10s interval = 30s
- Tier 3: offline after 3 missed heartbeats @ 30s interval = 90s (sleep cycles)

### VM Manager
**Package**: `internal/vm`
**Function**: Firecracker micro-VM lifecycle for vm-tier agents

**VM states**:
```
PENDING → SCHEDULED → CREATING → STARTING → RUNNING → STOPPING → STOPPED → DESTROYING
Also: FAILED (from RUNNING after maxRestarts exceeded)
```

**Provisioning steps**:
1. Generate unique CID for virtio-vsock
2. Create rootfs copy-on-write overlay on base image
3. Create/reuse workspace at `.state/agents/{AGENT_ID}/workspace/`
4. Sync agent directory files to workspace (MEMORY.md exception per 02-SCHEMAS)
5. Create TAP device, add to bridge `hivebr0`
6. Apply iptables for egress policy
7. Write Firecracker config JSON
8. Write MMDS metadata:
   - agent_id, team_id, nats_token, nats_vsock_port
9. Create virtiofs mounts: workspace, shared volumes

**Boot sequence**:
1. Start Firecracker process
2. Kernel boots (~125ms)
3. Init starts sidecar
4. Sidecar reads MMDS, connects NATS via vsock
5. Sidecar publishes health
6. Control plane transitions to RUNNING
7. Sidecar starts agent runtime (openclaw or entrypoint.sh)
8. Sidecar fetches capability tool definitions from control plane

**Hypervisor interface**: HTTP API over Unix socket (not Firecracker CLI). Boundary for future alternatives (Cloud Hypervisor, QEMU).

**Reload modes**:
- **Hot** (no restart): MD files, `skills/`, `files/` synced via virtiofs
- **Cold** (VM destroy/recreate): manifest resource/network/volume/runtime/tier changes; workspace preserved
- **Sidecar**: team membership or lead assignment changes
- **Capability update**: sidecar receives updated tool defs via NATS, regenerates OpenClaw skills; no VM restart

### Native Agent Manager
**Package**: `internal/native`
**Function**: Native-tier agents on Tier 1 and Tier 2

**Tier 1 (managed by hived directly)**:
- hived starts agent runtime as child process
- Sidecar runs in-process within hived OR as co-process (implementation choice)
- Agent has direct host hardware access

**Tier 2 (managed remotely via NATS)**:
- Control plane sends agent config to hive-agent via NATS: manifest + all files
- hive-agent writes to `/var/lib/hive/agents/{AGENT_ID}/workspace/`
- hive-agent starts agent runtime as child process
- hive-agent runs sidecar logic in-process
- hive-agent reports health to control plane

**States**:
```
PENDING → DEPLOYING → RUNNING → STOPPED
Also: FAILED (from RUNNING after maxRestarts)
```

**Hot reload**: Control plane sends updated files via NATS. hive-agent writes to workspace.
- OpenClaw: picked up next session
- Custom: SIGHUP sent to process

**Cold reload**: runtime/resource changes; stop and restart agent process.

### Firmware Agent Tracker
**Package**: `internal/firmware`
**Function**: Track Tier 3 firmware agents. Does NOT manage deployment (SDK toolchain out-of-band).

**Flow**:
1. Firmware boots, connects WiFi, connects MQTT
2. Publishes join with: agent_id, token, arch, capabilities, mode, firmware_version
3. Control plane registers, records capabilities
4. Device sends heartbeats via MQTT
5. Control plane routes capability invocations to device
6. Heartbeat timeout → OFFLINE, capabilities unavailable

**States**: `ONLINE`, `OFFLINE`
**Restart**: None; devices self-manage lifecycle

### Capability Router
**Package**: `internal/capabilities`
**Function**: Registry of all agent capabilities, generate tools

**On capability change** (agent added/removed/updated):
1. Rebuild capability index: `map[capability_name] → {agent_id, schema, team_id}`
2. For each team: generate tool defs from member capabilities
3. For cross-team exposed capabilities: generate namespaced tools (leads, director)
4. Push tool defs to team member sidecars
5. For OpenClaw agents: generate SKILL.md files, sync to workspace
6. Publish updated registry to `hive.capabilities.registry`

### Scheduler
**Package**: `internal/scheduler`
**Function**: Assign agents to nodes

**Tier 3**: No scheduling; agent IS the device; always pinned.
**Tier 2**: Usually pinned via `placement.nodeId`. If not pinned: any Tier 2 node with sufficient resources, matching arch/labels. One agent per node.
**Tier 1 VM**: Schedulable if no `placement.nodeId`. Multiple per node.
**Tier 1 native**: Placed on control plane node or specified Tier 1 node.

**VM scheduling algorithm**:
1. Filter: Tier 1 nodes only
2. Filter: KVM available
3. Filter: sufficient resources (memory, vcpus, disk after existing allocations)
4. Filter: arch compatibility
5. Filter: label selectors from `placement.nodeLabels`
6. Filter: not cordoned or draining
7. Score:
   - `available_memory_after / total_memory` (weight: 1.0)
   - `available_vcpus_after / total_vcpus` (weight: 1.0)
   - team_colocation_bonus: +0.5 if node has another agent from same team
   - spread_penalty: -0.3 if node is most loaded
8. Highest score wins; tie: alphabetical node ID

**Pending agents**: Retried every reconciliation loop (5s) and on node state changes.

**Resource tracking**: In-memory map `node_id → allocated resources`. Persisted to `.state/cluster/allocations.json`. Rebuilt from running VMs on startup.

**GPU scheduling**: Filter for nodes with matching GPU label. One GPU per VM via passthrough.

---

## STATE STORAGE

```
.state/nodes/{NODE_ID}.json
.state/agents/{AGENT_ID}/vm.json
.state/agents/{AGENT_ID}/workspace/
.state/cluster/desired.json
.state/cluster/actual.json
.state/cluster/tokens.json
.state/cluster/capabilities.json
.state/cluster/allocations.json
```

---

## CLUSTER ROOT SYNC (resolves review #5)

**Phase 1 (single-node)**: No sync needed. One filesystem, one watcher.

**Phase 3 (multi-node)**:
- Control plane node: AUTHORITATIVE for cluster root
- Worker Tier 1 nodes: do NOT have cluster root copy
- Workers receive agent config via NATS from control plane (same as Tier 2 mechanism)
- Workers run VMs; do not parse manifests

**User workflow**:
- Edit cluster root ONLY on control plane node
- Recommended: cluster root in git repo; user pushes, control plane pulls; one writer

**Future (HA control plane, beyond Phase 3)**:
- Single authoritative NATS JetStream KV store for desired state
- Control plane leader election
- Out of scope for Phase 3

---

## STARTUP SEQUENCE

1. Parse `cluster.yaml`, validate
2. Determine NATS mode (embedded if no clusterPeers, external otherwise)
3. If embedded: start NATS server in-process (with MQTT listener if enabled)
   If external: connect to NATS as client, verify connectivity
4. Load node registry from `.state/nodes/`
5. Full scan of `agents/` and `teams/`, validate all manifests
6. Build desired state
7. Query actual state from registered nodes
8. Run reconciliation
9. Start watcher
10. Enter steady-state loop

---

## SHUTDOWN SEQUENCE

1. Stop watcher
2. Stop reconciler
3. Graceful shutdown all VM agents: SIGTERM, 30s timeout, SIGKILL
4. Notify Tier 2 nodes to stop their agents via NATS
5. Persist final state
6. If embedded: stop NATS server
   If external: disconnect from NATS
7. Exit

**Tier 3 devices**: Unaffected by control plane shutdown. Continue running firmware. Capabilities uninvocable until control plane returns.

---

## NODE DRAIN

**Command**: `hivectl nodes drain NODE_ID`

**Sequence**:
1. Mark node as draining (no new scheduling)
2. For each VM on node:
   - Find alternative node
   - Stop VM
   - rsync workspace to target
   - Start VM on target
3. All VMs migrated → node marked drained

**Blocking**: If no capacity for a VM elsewhere, drain blocks and reports which VMs stuck.
