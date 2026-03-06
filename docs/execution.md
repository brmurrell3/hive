[← Back to Documentation](README.md)

# Hive Execution Specification

Covers Tier 1 VM, Tier 1 native, Tier 2, and Tier 3 execution models.

---

## SIDECAR ARCHITECTURE

### Single Codebase, Two Compilation Targets

ONE sidecar codebase (Go package: `internal/sidecar`). Compiled in two modes:

**Mode 1: Standalone Binary (Tier 1 VM)**
- Built as: separate static binary in VM base image at `/usr/local/bin/hive-sidecar`
- Runs as: PID 1 (or init-launched) process inside Firecracker VM
- Connects to: NATS via virtio-vsock
- Starts: agent runtime (openclaw or entrypoint.sh) as child process
- HTTP API: localhost:9100 inside VM

**Mode 2: Library (Tier 2 and Tier 1 native)**
- Built as: Go package linked into hive-agent binary (Tier 2) or hived binary (Tier 1 native)
- Runs as: goroutines within host process
- Connects to: NATS via TCP (Tier 2) or localhost (Tier 1 native)
- Manages: agent runtime as child process of host binary
- HTTP API: localhost:9100 on host

**Key invariant**: Same code, two compilation targets. Identical HTTP API. Identical NATS behavior. No protocol differences.

### Sidecar Responsibilities

- NATS bridge to control plane
- HTTP API at localhost:9100 (capability routing, tool definitions, health reporting)
- Agent runtime lifecycle management
- Tool definition management and OpenClaw skill generation

---

## TIER 1 EXECUTION: FIRECRACKER VMs

Requirements: Linux with KVM, 4GB+ RAM, x86_64 or arm64. NixOS officially supported.
Isolation: VM-level. Each agent gets own kernel, memory ceiling, network namespace.
Multiplicity: Multiple vm-tier agents per Tier 1 node.

### VM States

| State | Meaning |
|-------|---------|
| PENDING | Manifest validated, awaiting node assignment |
| SCHEDULED | Assigned to node by scheduler |
| CREATING | Rootfs overlay, TAP device, Firecracker config setup |
| STARTING | Firecracker launched, waiting for sidecar health (60s timeout) |
| RUNNING | Sidecar healthy, agent runtime operational |
| STOPPING | Graceful shutdown in progress (max 30s) |
| STOPPED | Halted, resources released, workspace preserved |
| FAILED | Exceeded maxRestarts |
| DESTROYING | Cleanup (TAP, iptables, CID) |

### Provisioning

1. Generate unique CID for virtio-vsock
2. Create rootfs COW overlay on base image
3. Create or reuse workspace at `.state/agents/{AGENT_ID}/workspace/`
4. Sync agent directory files to workspace (see WORKSPACE MODEL below)
5. Create TAP device, add to bridge `hivebr0`
6. Apply iptables for egress policy
7. Write Firecracker config JSON
8. Write MMDS metadata: `agent_id`, `team_id`, `nats_token`, `nats_vsock_port`
9. Create virtiofs mounts: workspace, shared volumes

### Boot Sequence

1. Start Firecracker process
2. Kernel boots (~125ms)
3. Init starts sidecar binary
4. Sidecar reads MMDS, connects to NATS via vsock
5. Sidecar publishes health
6. Control plane transitions to RUNNING
7. Sidecar starts agent runtime (openclaw or entrypoint.sh)
8. Sidecar fetches capability tool definitions, generates OpenClaw skills

### Reload Behavior

**Hot reload (no restart)**: MD files, skills/, files/ changes. Synced via virtiofs. OpenClaw picks up on next tool call.

**Cold reload (VM destroy/recreate)**: Manifest changes to resources, network, volumes, runtime, tier. Workspace preserved through destroy/recreate.

**Sidecar reload**: Team membership or lead assignment changes. Sidecar re-fetches config via NATS.

**Capability update**: Sidecar receives updated tool defs via NATS, regenerates OpenClaw skills. No VM restart.

### Graceful Shutdown

1. Control plane publishes shutdown to `hive.control.{AGENT_ID}`
2. Sidecar SIGTERMs agent runtime, waits 15s
3. Control plane SIGTERMs Firecracker, waits 15s
4. SIGKILL if needed
5. Clean up TAP, iptables, CID
6. Workspace preserved

### Base VM Image

- Minimal NixOS rootfs ext4
- Contents: minimal userspace, Node.js 20+, sidecar binary, networking tools, CA certs, curl, git
- Read-only base with COW overlay
- One image per arch (amd64, arm64)
- Target: under 500MB, boot under 2s

### Hypervisor Interface

VM manager talks to Firecracker via HTTP API over Unix socket (not CLI). Boundary for future alternative hypervisor support.

---

## TIER 1 NATIVE EXECUTION

When `spec.tier=native` and agent placed on Tier 1 node.

- Managed by: `hived` directly (not `hive-agent`)
- Sidecar: library mode, runs as goroutines in `hived`
- Agent runtime: child process of `hived`
- Hardware access: direct host access (GPIO, USB, GPU, etc.)
- Isolation: process-level only (systemd user, no VM)
- Multiplicity: multiple native agents can run on a Tier 1 node
- Use case: agents needing direct hardware access on powerful machine (GPU without passthrough overhead, direct USB device access)

States: PENDING, DEPLOYING, RUNNING, STOPPED, FAILED (same as Tier 2)

---

## TIER 2 EXECUTION: NATIVE SINGLE-AGENT LINUX

Requirements: Linux (any distro with systemd), 512MB+ RAM, any Go arch.
Isolation: process-level. Dedicated user account. No kernel isolation.
Multiplicity: one agent per Tier 2 node.
Key differentiator: direct hardware access (GPIO, I2C, SPI, USB, camera, serial).

### hive-agent Binary

- Single statically-linked Go binary
- No runtime deps beyond Linux + network
- Cross-compiled for: amd64, arm64, armv7, armv6
- Combines: sidecar logic (library mode) + lightweight agent lifecycle manager
- NOT `hived`; `hive-agent` is thin agent host

### Agent Deployment

1. Control plane sends agent config to node via NATS: manifest + all files
2. `hive-agent` writes files to `/var/lib/hive/agents/{AGENT_ID}/workspace/`
3. `hive-agent` starts runtime as child process:
   - `openclaw`: runs openclaw command with workspace path
   - `custom`: runs entrypoint.sh from workspace
4. `hive-agent` runs sidecar logic in-process (HTTP API at localhost:9100, NATS bridge)
5. Reports health to control plane

### Agent States

| State | Meaning |
|-------|---------|
| PENDING | Assigned to node, config not yet received |
| DEPLOYING | Config received, files being written |
| RUNNING | Runtime alive, health checks passing |
| STOPPED | Runtime stopped gracefully |
| FAILED | Runtime crashed, maxRestarts exceeded |

### Hardware Access

Agent runtime runs natively on host. Can access:
- GPIO: `/sys/class/gpio` or libgpiod
- I2C: `/dev/i2c-N`
- SPI: `/dev/spidev`
- USB: standard device files
- Camera: v4l2 or libcamera
- Serial: `/dev/ttyUSB*` or `/dev/ttyAMA*`

Exposed to other agents via capabilities. Manifest declares hardware. Capability definitions describe interface. Control plane generates tools for remote invocation.

### Hot Reload

- **File changes**: control plane sends updated files via NATS. `hive-agent` writes to workspace.
- **OpenClaw**: picks up on next session.
- **Custom**: SIGHUP sent to process (convention: SIGHUP = reload config).
- **Manifest resource/runtime changes**: stop and restart agent process.

### Resource Constraints

- `hive-agent` overhead: ~20-30MB RAM
- Available for agent: total RAM - OS overhead (~200-300MB) - `hive-agent` overhead
- Scheduler checks agent manifest `spec.resources.memory` against available memory before deployment

### Isolation

- Process-level only. Agent runs under dedicated user account.
- No memory ceiling enforcement (scheduling checks only).
- Misbehaving agent can affect host. Accepted tradeoff for Tier 2.
- Future: systemd MemoryMax/CPUQuota for soft isolation (not Phase 1).

---

## TIER 3 EXECUTION: MICROCONTROLLER FIRMWARE

See [firmware.md](firmware.md) for SDK, build toolchain, and OTA details.

Requirements: network (WiFi/Ethernet/BLE gateway), programmable, flash storage
Isolation: none (bare metal)
Multiplicity: one agent per device

### Modes

- **Tool mode (default)**: responds to capability invocations only. No initiative. Reacts, does not act.
- **Peer mode**: receives tasks via team broadcast, can initiate messages, runs autonomous logic loop.
- Mode set in agent manifest `spec.mode` field.

### Connection and Registration

1. Firmware boots, connects WiFi
2. Connects MQTT at control plane port 1883
3. Authenticates: `username=agent_id`, `password=join_token`
4. Publishes join request: `agent_id`, `token`, `arch`, `capabilities`, `mode`, `firmware_version`
5. Subscribes to `hive/join/status/{AGENT_ID}`
6. On approval: subscribes to capability request topics and team topics
7. Begins heartbeats to `hive/health/{AGENT_ID}`

### Capability Handling

1. MQTT message on `hive/capabilities/{AGENT_ID}/{CAPABILITY}/request`
2. Firmware deserializes inputs (JSON or MessagePack)
3. Calls registered handler function
4. Handler reads sensor / toggles GPIO / etc.
5. Serializes outputs
6. Publishes response with `correlation_id`

### Peer Mode Additional Behavior

- Subscribes to team broadcast and agent inbox
- Can publish to team broadcast and direct messages
- Can invoke capabilities on other agents (including LLM agents)
- Runs user-defined logic loop for autonomous actions (threshold alerts, timed actions)

### Device Lifecycle

| State | Meaning |
|-------|---------|
| ONLINE | MQTT connected, heartbeats arriving, capabilities available |
| OFFLINE | Heartbeats stopped for 3x `health.interval` (default 90s). Capabilities unavailable. |

Reconnect: re-registers, transitions to ONLINE. JetStream delivers missed messages if team persistent.

---

## HEALTH CHECK SEMANTICS BY TIER

All tiers share same schema: `health.enabled`, `health.interval`, `health.timeout`, `health.maxFailures`.
Behavior on maxFailures differs by tier:

### VM Tier

- **Check**: control plane pings `hive.control.{AGENT_ID}` every interval. Sidecar responds within timeout.
- **On maxFailures consecutive failures**: restart VM per `restart.policy`
- **Restart means**: STOPPING → STARTING with same workspace
- **After maxRestarts exhausted**: FAILED state, no more restarts

### Native Tier (Tier 1 or Tier 2)

- **Check**: same ping/response mechanism
- **On maxFailures consecutive failures**: restart process per `restart.policy`
- **Restart means**: kill process, start new process with same workspace
- **After maxRestarts exhausted**: FAILED state

### Firmware Tier

- **Check**: heartbeat monitoring (device sends heartbeats, control plane checks arrival)
- **On maxFailures consecutive missed heartbeats**: mark OFFLINE
- **NO restart action**: Device self-manages. Control plane only updates state.
- **Capabilities removed**: from team tool definitions while OFFLINE
- **No maxRestarts concept**: firmware devices do not auto-restart

---

## WORKSPACE MODEL

Two locations for agent files:

| Location | Purpose |
|----------|---------|
| `agents/{AGENT_ID}/` in cluster root | User-authored, version-controlled (definition) |
| `.state/agents/{AGENT_ID}/workspace/` | Runtime state (runtime workspace) |

### Sync Rules

**First creation**: definition files copied to workspace

**Hot reload**: definition files synced to workspace, OVERWRITING workspace copies
- EXCEPTION: `MEMORY.md` uses last-modified-wins logic:
  - If workspace `MEMORY.md` mtime > cluster root `MEMORY.md` mtime: preserve workspace version
  - If cluster root `MEMORY.md` mtime > workspace `MEMORY.md` mtime: overwrite workspace with cluster root
  - If cluster root `MEMORY.md` does not exist but workspace has one: preserve workspace version

**Cold reload** (manifest change requiring restart): same sync rules as hot reload

**Destroy (no --purge)**: workspace preserved in `.state/` for potential reuse

**Destroy --purge**: workspace deleted permanently

### Rationale

`MEMORY.md` is the only file OpenClaw modifies at runtime. All other definition files (AGENTS.md, SOUL.md, skills/, files/) are user-authored and should always come from cluster root. `MEMORY.md` needs special handling because runtime state (conversation history, learned facts) is valuable and should survive reloads unless the user explicitly overwrites it by editing the cluster root copy.

---

## Summary Table: Execution Models

| Property | Tier 1 VM | Tier 1 Native | Tier 2 | Tier 3 |
|----------|-----------|---------------|--------|--------|
| Hypervisor | Firecracker | Host kernel | Host kernel | Bare metal |
| Sidecar | Standalone binary (PID 1) | Library in hived | Library in hive-agent | Firmware logic |
| Agent runtime | Child of sidecar | Child of hived | Child of hive-agent | Built-in to firmware |
| Isolation | VM (kernel, memory, network) | Process (systemd user) | Process (systemd user) | None |
| Hardware access | Passthrough (slow) | Direct | Direct | Direct |
| Multiplicity | Multiple per node | Multiple per node | One per node | One per device |
| Health check | Ping/response | Ping/response | Ping/response | Heartbeat |
| Restart on failure | VM restart | Process restart | Process restart | None (device self-manages) |
| Workspace location | `.state/agents/{ID}/workspace/` | `.state/agents/{ID}/workspace/` | `/var/lib/hive/agents/{ID}/workspace/` | Device memory |
