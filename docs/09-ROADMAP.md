# 09 - Implementation Roadmap

This document is the authoritative build sequence for the Hive framework. It is structured as ordered milestones with explicit acceptance criteria. Each milestone must be fully validated before proceeding to the next.

Reference documents: `00-VISION-AND-PHASES.md` through `08-DEPLOYMENT.md` contain full specifications. This roadmap overrides phasing described in `00-VISION-AND-PHASES.md` where they conflict — this document reflects a leaner MVP scope with deferred complexity reintroduced in later milestones.

---

## Deferred Complexity Register

The following items are intentionally excluded from early milestones to reduce MVP scope. Each is tagged with the milestone where it gets introduced. Do NOT implement these before their designated milestone.

| Item | Deferred Until | Rationale |
|---|---|---|
| Sidecar library/goroutine mode (dual-mode abstraction) | M6 | Phase 1 is VM-only; standalone binary is sufficient |
| Reconciliation polling loop (5s timer) | M8 | fsnotify + explicit CLI command is sufficient for single-node |
| Hot reload of agent manifests, skills, AGENTS.md | M5 | MEMORY.md hot-reload only; everything else requires `hivectl agents restart` |
| Scheduler (bin-packing, placement, resource constraints) | M8 | Single node has nothing to schedule; simple memory/CPU check is sufficient |
| Capability registry (`.state/cluster/capabilities.json`) | M6 | NATS subject namespace IS the registry for single-team |
| Join token system (`hivectl tokens *`) | M6 | Single node has nothing to join; filesystem access is auth |
| Structured `.state/` directory hierarchy | M8 | Single `state.json` file owned by hived for single-node |
| NixOS rootfs image build | M5 | Use minimal Alpine rootfs with baked-in sidecar for faster iteration |
| MQTT bridge | M6 | NATS-only in single-node phases |
| Firmware SDKs (C, MicroPython) | M7 | Tier 3 support deferred |
| Cross-team capability routing | M8 | Single team in early milestones |
| Director agent | M9 | Multi-team orchestration deferred |
| Web dashboard | M10 | CLI-only until core is stable |
| Multi-user RBAC | M9 | Single operator model until clustering |
| OTA firmware updates | M7 | No Tier 3 devices in early milestones |
| Prometheus metrics export | M10 | Basic log streaming is sufficient early |

---

## Milestone 1: Project Skeleton and NATS Transport

**Goal:** Establish the Go project structure, embed NATS server, and validate basic pub/sub messaging.

### Deliverables

1. Go module initialized (`go.mod`) with project structure:
   ```
   cmd/hived/main.go
   cmd/hivectl/main.go
   internal/nats/server.go
   internal/config/loader.go
   internal/types/
   ```
2. `hived` binary starts and embeds a NATS server (using `github.com/nats-io/nats-server/v2/server` as a Go library, NOT as a subprocess).
3. `hived` reads a `cluster.yaml` from a configurable cluster root path and parses it into Go structs.
4. NATS listens on the port specified in `cluster.yaml` with JetStream enabled.
5. Basic structured logging (JSON format) to stdout.

### Acceptance Criteria

- [ ] `go build ./cmd/hived` and `go build ./cmd/hivectl` both succeed with zero errors.
- [ ] Running `hived --cluster-root /path/to/test-cluster` starts the process, logs indicate NATS is listening on the configured port.
- [ ] A standalone NATS client can connect to the embedded server and publish/subscribe on arbitrary subjects.
- [ ] `cluster.yaml` parsing validates required fields and returns structured errors for malformed input.
- [ ] Unit tests exist for config parsing covering valid input, missing required fields, and invalid values.
- [ ] JetStream is enabled and a test can create a stream and publish/consume a message.

### Explicit Exclusions

- No agent management, no VM management, no CLI commands beyond `hived` startup.
- No filesystem watching.
- No state file.

---

## Milestone 2: Agent Manifest Parsing and Validation

**Goal:** Parse agent manifest YAML files, validate them against the schema, and build an in-memory desired state.

### Deliverables

1. Agent manifest parser that reads `agents/AGENT_ID/manifest.yaml` files from the cluster root.
2. Team manifest parser that reads `teams/TEAM_ID.yaml` files.
3. Validation logic:
   - Required fields present (`metadata.id`, `spec.runtime.type`, `spec.runtime.model`).
   - Agent IDs match regex `[a-z0-9][a-z0-9-]{0,62}` (no leading dash).
   - Resource values parse correctly (memory strings like `"512Mi"`, vcpu integers).
   - Team lead references a valid agent ID.
   - Capability names are unique within an agent.
   - No duplicate agent IDs across the cluster root.
   - Team lead must reference an agent whose `metadata.team` matches the team ID.
   - Agent volumes must reference team `shared_volumes` by name.
4. `hivectl validate` command that loads the cluster root and reports all validation errors.
5. `hivectl init PATH` command that scaffolds a cluster root directory with a template `cluster.yaml`, example agent directory, and example team file.

### Acceptance Criteria

- [ ] `hivectl validate --cluster-root /path` exits 0 for valid configs, exits 1 with human-readable error list for invalid configs.
- [ ] `hivectl init /tmp/test-cluster` creates the directory structure with valid template files that pass `hivectl validate`.
- [ ] Unit tests cover: valid manifests, missing required fields, invalid agent ID format (including leading dash rejection), duplicate agent IDs, invalid resource formats, team lead with mismatched team field, agent volume referencing nonexistent shared_volume, duplicate capability names.
- [ ] Parsing produces a `DesiredState` struct containing all agents, teams, and cluster config accessible in-memory.

### Explicit Exclusions

- No reconciliation. Parsing and validation only.
- No runtime state, no state file.

---

## Milestone 3: Firecracker VM Lifecycle

**Goal:** Start, stop, and destroy Firecracker micro-VMs programmatically from hived.

### Deliverables

1. VM manager component in hived that can:
   - Create a Firecracker VM with specified memory and vcpu count.
   - Attach a rootfs image (Alpine Linux minimal, pre-built or downloaded).
   - Configure a virtio-vsock connection for host-guest communication.
   - Start the VM and confirm it reaches a running state.
   - Stop the VM gracefully (SIGTERM to Firecracker process, then SIGKILL after timeout).
   - Destroy the VM and clean up socket files, log files, and rootfs copies.
2. VM state tracking: each VM has a state (PENDING, CREATING, STARTING, RUNNING, STOPPING, STOPPED, FAILED) persisted to `state.json`.
3. A minimal Alpine Linux rootfs image suitable for Firecracker (kernel + rootfs pair).
4. `hivectl agents start AGENT_ID` triggers VM creation and start for a validated agent.
5. `hivectl agents stop AGENT_ID` triggers graceful VM stop.
6. `hivectl agents destroy AGENT_ID` stops and removes the VM.
7. `hivectl agents list` shows all agents with their current VM state.
8. `hivectl agents status AGENT_ID` shows detailed state for one agent.

### Acceptance Criteria

- [ ] `hivectl agents start AGENT_ID` creates and boots a Firecracker VM. The VM reaches RUNNING state within 5 seconds.
- [ ] The VM respects the memory and vcpu limits from the agent manifest.
- [ ] `hivectl agents stop AGENT_ID` transitions the VM to STOPPED. The Firecracker process is no longer running.
- [ ] `hivectl agents destroy AGENT_ID` removes all VM artifacts (socket, log, rootfs copy). Agent state is removed from `state.json`.
- [ ] `hivectl agents list` outputs a table with columns: AGENT_ID, TEAM, STATE, UPTIME.
- [ ] `hivectl agents status AGENT_ID` outputs: agent ID, team, state, resource limits, VM PID, uptime, last state transition timestamp.
- [ ] `state.json` persists across hived restarts. On startup, hived reads `state.json` and reconciles with actual running Firecracker processes (marks VMs as FAILED if their process is gone).
- [ ] Integration test: start agent, verify VM running, stop agent, verify VM stopped, destroy agent, verify cleanup.
- [ ] Starting an agent that is already RUNNING returns an error (not a duplicate VM).

### Explicit Exclusions

- No sidecar inside the VM yet. The VM boots but does nothing useful.
- No networking between VM and host beyond virtio-vsock.
- No agent runtime (OpenClaw) inside the VM.
- No health checks.
- No automatic restart.

---

## Milestone 4: Sidecar and Agent Runtime

**Goal:** Deploy the sidecar binary into VMs, establish host-guest communication over virtio-vsock, and run the OpenClaw agent runtime inside the VM.

### Deliverables

1. Sidecar binary (`hive-sidecar`) compiled as a standalone Linux binary, baked into the rootfs image.
2. Sidecar starts as PID 1 (or init-launched) inside the Firecracker VM.
3. Sidecar connects to hived over virtio-vsock:
   - Registers with hived, reports agent ID and health status.
   - Receives configuration (agent manifest data, model provider config, environment variables).
4. Sidecar starts the agent runtime (OpenClaw process) as a child process.
5. Sidecar exposes a local HTTP API inside the VM at `localhost:9100`:
   - `GET /health` returns sidecar and agent runtime health.
   - `GET /capabilities` returns the agent's declared capabilities.
   - `POST /capabilities/{name}/invoke` invokes a capability (placeholder; real routing in M5).
6. Sidecar connects to the embedded NATS server (via virtio-vsock tunnel or host-forwarded TCP).
7. Agent manifest files (`AGENTS.md`, `SOUL.md`, `MEMORY.md`, `skills/`) are mounted into the VM filesystem at a known path.
8. Sidecar publishes heartbeat messages on `hive.health.{AGENT_ID}` at a configurable interval.
9. hived receives heartbeats and updates agent state (RUNNING confirmed, or UNHEALTHY if heartbeats stop).

### Acceptance Criteria

- [ ] After `hivectl agents start AGENT_ID`, the sidecar process is running inside the VM (visible via virtio-vsock health check from host).
- [ ] hived receives heartbeat messages on NATS subject `hive.health.{AGENT_ID}` within 10 seconds of VM start.
- [ ] The OpenClaw agent runtime process is running inside the VM as a child of the sidecar (verified via process list or sidecar health endpoint).
- [ ] `curl localhost:9100/health` from inside the VM returns JSON with sidecar status and agent runtime status.
- [ ] `curl localhost:9100/capabilities` returns the capabilities declared in the agent's manifest.
- [ ] Agent files (AGENTS.md, SOUL.md, MEMORY.md, skills/) are accessible at the expected path inside the VM.
- [ ] If the agent runtime process crashes, the sidecar detects it and reports UNHEALTHY via heartbeat (but does NOT restart it yet — that's M5).
- [ ] hived marks an agent as UNHEALTHY if no heartbeat is received for 3x the heartbeat interval.
- [ ] Integration test: start agent, confirm heartbeat received, confirm sidecar health endpoint responds, confirm agent files are mounted.

### Explicit Exclusions

- No capability invocation routing (sidecar accepts the request but does not route to other agents).
- No auto-restart of crashed agents.
- No MEMORY.md hot-reload.
- No tool auto-generation.

---

## Milestone 5: Capability Routing, Tool Generation, and Health Management

**Goal:** Agents can invoke each other's capabilities over NATS. Lead agents get auto-generated tool definitions. Failed agents auto-restart. MEMORY.md hot-reloads.

### Deliverables

1. **Capability routing via NATS subjects:**
   - Each agent's sidecar subscribes to `hive.capabilities.{AGENT_ID}.{CAPABILITY}.request`.
   - When a request arrives, the sidecar calls the agent runtime to execute the capability and publishes the result to the reply subject.
   - Invoking agent's sidecar publishes to the target subject and waits for the response (with configurable timeout).
2. **Tool auto-generation:**
   - For each agent in the team, the sidecar of the lead agent generates tool definitions (compatible with OpenClaw tool format) from the capability schemas in the team members' manifests.
   - Tools are written to the lead agent's runtime filesystem so the LLM can discover and invoke them.
   - When the lead agent calls a generated tool, the sidecar translates it to a NATS capability request.
3. **Health checks and auto-restart:**
   - hived monitors heartbeats per agent.
   - If an agent exceeds `maxFailures` consecutive missed heartbeats, hived triggers a restart according to the agent's `restart.policy` (on-failure, always, never).
   - Restart count tracked in `state.json`. Backoff delay between restarts (default 10s, from `restart.backoff` in manifest). If `maxRestarts` exceeded, agent enters FAILED state and stays down.
   - `hivectl agents restart AGENT_ID` manually restarts an agent (resets restart counter).
4. **MEMORY.md hot-reload:**
   - hived watches `agents/AGENT_ID/MEMORY.md` for changes via fsnotify.
   - On change, hived pushes the updated content to the sidecar over NATS or virtio-vsock.
   - Sidecar writes the updated file into the VM filesystem. Agent runtime picks it up without restart.
5. **Rootfs image improvement:**
   - Alpine-based rootfs with sidecar binary, agent runtime dependencies, and a startup script.
   - Image build scripted (shell script or Makefile, NOT Nix yet).
6. **Team broadcast:**
   - Lead agent can publish to `team.{TEAM_ID}.broadcast` and all team members receive.

### Acceptance Criteria

- [ ] Agent A (lead) can invoke a capability on Agent B (tool) via the auto-generated tool. The request routes through NATS and the response returns to Agent A within 2 seconds.
- [ ] Lead agent's runtime filesystem contains auto-generated tool files for each team member's capabilities. Tool definitions include name, description, input schema, and output schema matching the manifest.
- [ ] If Agent B's runtime process crashes:
  - hived detects missing heartbeats within `3 * healthInterval`.
  - hived restarts the agent (VM stop + start) if `restart.policy` is `on-failure` or `always`.
  - Restart count increments in `state.json`.
  - If `maxRestarts` exceeded, agent enters FAILED and is not restarted again.
- [ ] `hivectl agents restart AGENT_ID` restarts the agent and resets the restart counter.
- [ ] Editing `agents/AGENT_ID/MEMORY.md` on the host filesystem results in the updated content appearing inside the VM within 5 seconds, without VM restart.
- [ ] Team broadcast: lead publishes a message on `team.{TEAM_ID}.broadcast`, all team member sidecars receive it.
- [ ] Integration test: deploy a team with 1 lead + 2 tool agents. Lead invokes capabilities on both tools. Kill one tool agent's runtime. Verify auto-restart. Verify capability invocation works after restart.
- [ ] Rootfs image builds via a single `make rootfs` or equivalent command.

### Explicit Exclusions

- No hot-reload of manifests, AGENTS.md, SOUL.md, or skills (requires `hivectl agents restart`).
- No cross-team capability routing.
- No capability registry file (NATS subjects are the registry).
- No NixOS image build.

---

## Milestone 6: Tier 2 Native Agents and Cross-Tier Communication

**Goal:** Support native (non-VM) agents on Tier 2 devices (RPi, Jetson, etc.). Introduce join tokens, the capability registry, and the sidecar library mode.

### Deliverables

1. **`hive-agent` binary:** Standalone Go binary for Tier 2 devices. Embeds the sidecar as a library (goroutines, not a separate process). Manages a single agent runtime as a child process.
2. **Sidecar library mode:** Refactor sidecar internals so the core logic (NATS bridge, capability routing, health reporting, HTTP API) can run either as a standalone binary (VM mode) or as goroutines within a host process (library mode). Both modes use the same internal interfaces.
3. **Join token system:**
   - `hivectl tokens create [--ttl DURATION]` generates a join token (random, stored as SHA-256 hash in `state.json`).
   - `hivectl tokens list` shows active tokens (prefix only, not full token).
   - `hivectl tokens revoke PREFIX` revokes a token.
   - Tier 2 device runs `hive-agent join --token TOKEN --control-plane HOST:PORT`.
   - hived validates the token hash, registers the node, and assigns a node ID.
4. **Node registration and hardware inventory:**
   - `hive-agent` collects hardware info on join (arch, memory, CPU count, KVM availability, GPU presence).
   - hived stores node inventory in `state.json` under a `nodes` key.
   - Auto-classification: KVM + >=4GB RAM = Tier 1, else Linux = Tier 2.
   - `hivectl nodes list` shows all registered nodes with tier, arch, memory, and status.
   - `hivectl nodes status NODE_ID` shows detailed hardware info.
5. **Capability registry:**
   - hived maintains an in-memory capability registry (backed by `state.json`).
   - When an agent starts, its capabilities are registered.
   - When an agent stops, its capabilities are deregistered.
   - Tool auto-generation now consults the registry rather than relying solely on NATS subject conventions.
   - This enables cross-tier capability invocation: a VM agent can invoke a capability on a Tier 2 native agent transparently.
6. **MQTT bridge (embedded in hived):**
   - hived starts an MQTT listener on port 1883.
   - Translates MQTT topics to NATS subjects and vice versa for Tier 3 device connectivity (Tier 3 implementation is M7, but the bridge is ready).

### Acceptance Criteria

- [ ] `hive-agent` binary compiles for linux/amd64 and linux/arm64.
- [ ] `hive-agent join --token TOKEN --control-plane HOST:PORT` successfully registers a Tier 2 node with hived. Node appears in `hivectl nodes list` with correct tier and hardware info.
- [ ] A native agent on the Tier 2 node starts its runtime, reports heartbeats, and its capabilities are invocable from a Tier 1 VM agent.
- [ ] Sidecar library mode passes the same functional tests as standalone binary mode (health endpoint, capability routing, heartbeats).
- [ ] Token lifecycle works: create, list (shows prefix), revoke (prevents future joins with that token). Expired TTL tokens are rejected.
- [ ] Invalid or revoked tokens are rejected with a clear error message on the `hive-agent` side.
- [ ] Capability registry correctly reflects running agents. Starting an agent adds its capabilities; stopping removes them.
- [ ] Cross-tier invocation: VM agent invokes capability on native agent, receives response within 2 seconds.
- [ ] MQTT listener starts on port 1883. A standalone MQTT client can connect and publish a message that arrives as a NATS message (subject mapping validated).
- [ ] Integration test: 1 Tier 1 node with hived + 1 VM agent, 1 Tier 2 node (simulated or real) with 1 native agent. VM agent invokes native agent's capability and receives the result.

### Explicit Exclusions

- No Tier 3 firmware agents (M7).
- No cross-team capabilities (M8).
- No multi-node Tier 1 clustering (M8).
- No pre-built RPi images (M7).

---

## Milestone 7: Tier 3 Firmware Agents and Device Tooling

**Goal:** Support microcontroller (ESP32, Pi Pico, etc.) firmware agents that communicate via MQTT. Provide build/flash tooling.

### Deliverables

1. **C SDK for firmware agents:**
   - Minimal C library providing: MQTT connection to hived, capability registration, message serialization (JSON), heartbeat publishing.
   - Two modes: tool mode (responds to capability invocations only) and peer mode (can initiate messages and run autonomous logic).
   - Example firmware project for ESP32 (ESP-IDF based) demonstrating a temperature sensor capability.
2. **MicroPython SDK:**
   - Python module providing the same interface as the C SDK.
   - Example firmware for Pi Pico W demonstrating a GPIO toggle capability.
3. **Firmware build and flash tooling:**
   - `hivectl firmware build AGENT_ID --target PLATFORM` compiles the firmware source in `agents/AGENT_ID/firmware/` for the specified platform.
   - `hivectl firmware flash AGENT_ID --port PORT` flashes the compiled firmware to a connected device.
   - `hivectl firmware monitor AGENT_ID --port PORT` opens a serial monitor for debugging.
   - Build toolchain dependencies documented (ESP-IDF, Pico SDK, etc.).
4. **Tier 3 device state tracking:**
   - hived tracks Tier 3 devices as ONLINE/OFFLINE based on MQTT heartbeats (via the MQTT bridge from M6).
   - No restart management (devices self-manage restarts).
   - `hivectl agents list` includes Tier 3 agents with their ONLINE/OFFLINE status.
5. **Pre-built Tier 2 images:**
   - Downloadable SD card images for Raspberry Pi 3, 4, 5, and Zero 2W with `hive-agent` pre-installed.
   - Image boots, runs `hive-agent`, and prompts for join token and control plane address.

### Acceptance Criteria

- [ ] C SDK compiles for ESP32 (ESP-IDF) and produces a flashable firmware binary.
- [ ] MicroPython SDK runs on Pi Pico W and connects to hived's MQTT listener.
- [ ] Firmware agent in tool mode: publishes heartbeat, responds to capability invocation from a Tier 1 agent. Response arrives within 500ms on local network.
- [ ] Firmware agent in peer mode: can publish messages to team broadcast subject.
- [ ] `hivectl firmware build AGENT_ID --target esp-idf` produces a binary from the agent's firmware source directory.
- [ ] `hivectl firmware flash AGENT_ID --port /dev/ttyUSB0` flashes the device. Device boots and connects to hived.
- [ ] `hivectl firmware monitor AGENT_ID --port /dev/ttyUSB0` streams serial output to the terminal.
- [ ] hived tracks Tier 3 device as ONLINE when heartbeats arrive, OFFLINE after 3x heartbeat interval with no messages.
- [ ] End-to-end test: Tier 1 lead agent invokes a sensor-read capability on a Tier 3 ESP32. Data returns through MQTT bridge → NATS → lead agent.
- [ ] RPi image: flash to SD card, boot RPi, run join command, node appears in `hivectl nodes list` as Tier 2.

### Explicit Exclusions

- No OTA firmware updates (add in M9 or later).
- No firmware signing.
- No MessagePack encoding (JSON only over MQTT).

---

## Milestone 8: Multi-Node Clustering and Structured State

**Goal:** Multiple Tier 1 nodes form a cluster. Introduce the scheduler, structured state directory, reconciliation polling, and external NATS.

### Deliverables

1. **Cluster topology:**
   - One root node (authoritative, runs hived in root mode).
   - One or more worker nodes (run hived in worker mode, connect to root).
   - Root publishes state changes via NATS. Workers subscribe and cache locally.
2. **External NATS:**
   - Transition from embedded NATS to external NATS server/cluster.
   - `cluster.yaml` gains `nats.mode: external` and `nats.urls` fields.
   - hived connects as a client rather than embedding the server.
   - NixOS module configures external NATS as a separate systemd service.
3. **Scheduler:**
   - When an agent is created, the scheduler assigns it to a Tier 1 node based on available resources (memory, vcpus).
   - Simple bin-packing strategy: assign to the node with the most available resources that still fits the request.
   - `placement.nodeId` in manifest overrides scheduler (explicit pinning).
   - `placement.nodeLabels` in manifest constrains scheduler to matching nodes.
4. **Structured state directory:**
   - Migrate from single `state.json` to `.state/` directory:
     ```
     .state/
     ├── nodes/NODE_ID.json
     ├── agents/AGENT_ID/vm.json
     └── cluster/
         ├── desired.json
         ├── actual.json
         ├── capabilities.json
         └── allocations.json
     ```
   - Root node is authoritative for `.state/`. Workers have read-only local caches.
5. **Reconciliation polling loop:**
   - hived runs a reconciliation loop every 5 seconds (in addition to fsnotify events).
   - Compares desired state (from manifests) with actual state (from `.state/`).
   - Generates idempotent actions to converge actual → desired.
   - Handles: agents added, agents removed, agents with changed config (triggers restart).
6. **Hot-reload expansion:**
   - In addition to MEMORY.md (from M5), manifest changes and skill changes now trigger reconciliation.
   - Changed manifests cause agent restart (not in-place update).
   - Added/removed agent directories cause agent creation/destruction.
7. **Node drain and cordon:**
   - `hivectl nodes drain NODE_ID` migrates all VMs off a node (stop on old, start on new via scheduler).
   - `hivectl nodes cordon NODE_ID` prevents new VMs from being scheduled on the node.

### Acceptance Criteria

- [ ] A 3-node cluster (1 root, 2 workers) forms successfully. All nodes appear in `hivectl nodes list`.
- [ ] Creating an agent on the root node results in the VM being scheduled and started on a worker node with available resources.
- [ ] External NATS: hived connects to an external NATS server. All messaging works identically to embedded mode.
- [ ] Scheduler respects `placement.nodeId` (agent runs on the specified node) and `placement.nodeLabels` (agent runs on a node matching all labels).
- [ ] If no node has sufficient resources, the agent enters PENDING state and is scheduled when resources become available.
- [ ] `.state/` directory is created and populated correctly. State survives hived restart.
- [ ] Reconciliation loop detects a manually deleted agent directory and stops the corresponding VM within 10 seconds.
- [ ] Reconciliation loop detects a new agent directory and starts the corresponding VM within 10 seconds.
- [ ] `hivectl nodes drain NODE_ID` migrates all VMs to other nodes. The drained node has zero running VMs.
- [ ] `hivectl nodes cordon NODE_ID` prevents new scheduling. Existing VMs continue running.
- [ ] Replication: state changes on root are visible on workers within 1 second.
- [ ] Integration test: 3-node cluster, create 5 agents, verify distribution across workers. Drain one worker, verify migration. Cordon the other, verify new agents go PENDING.

### Explicit Exclusions

- No cross-team capabilities (M9).
- No multi-user access control (M9).
- No director agent (M9).

---

## Milestone 9: Multi-Team Organization, Cross-Team Routing, and Access Control

**Goal:** Support multiple teams with isolated namespaces, cross-team capability exposure, director agent, and multi-user access control.

### Deliverables

1. **Multi-team support:**
   - Multiple `teams/TEAM_ID.yaml` files, each with its own lead and members.
   - Team-scoped NATS subjects: `team.{TEAM_ID}.*` provides namespace isolation.
   - Agents in one team cannot see another team's broadcast or task subjects.
2. **Cross-team capability exposure:**
   - Team manifest gains `communication.crossTeamCapabilities` field.
   - Value is a list of capability names (or `"all"`) that are exposed to other teams.
   - Exposed capabilities are routable via `org.capabilities.*` NATS subjects.
   - Tool auto-generation for leads includes cross-team tools with a team prefix in the tool name.
3. **Director agent:**
   - Optional org-level agent specified in `cluster.yaml` under `director.agentId`.
   - Has visibility into all teams' exposed capabilities.
   - Can assign tasks to team leads and receive results.
   - Runs as a standard VM agent with additional NATS subject subscriptions.
4. **Multi-user access control:**
   - mTLS between nodes (certificates generated by `hivectl`).
   - JWT tokens for user authentication to hivectl.
   - Roles: `admin` (full access), `operator` (manage agents and teams), `viewer` (read-only).
   - `hivectl users create USER_ID --role ROLE` creates a user and generates credentials.
   - `hivectl users list`, `hivectl users update`, `hivectl users revoke`.
5. **OTA firmware updates:**
   - `hivectl firmware update AGENT_ID --binary PATH` pushes firmware to a Tier 3 device over MQTT.
   - Chunk-based transfer with SHA-256 verification.
   - Device validates checksum before applying. Rollback on boot failure (device-side logic).

### Acceptance Criteria

- [ ] Two teams with separate leads and members operate independently. Team A's broadcast messages are not received by Team B's agents.
- [ ] Cross-team capability: Team A exposes `analyze_data`. Team B's lead can invoke `teamA.analyze_data` via auto-generated tool.
- [ ] Director agent can invoke exposed capabilities from any team.
- [ ] mTLS: worker nodes authenticate to root via client certificates. Connections without valid certs are rejected.
- [ ] JWT auth: `hivectl` commands require a valid token. Viewer role cannot start/stop agents. Operator role can. Admin can manage users.
- [ ] OTA update: firmware binary pushed to ESP32 over MQTT. Device applies update and comes back online with new firmware. Bad firmware triggers rollback (device boots previous version).
- [ ] Integration test: 2 teams, director agent, cross-team invocation. Director assigns task → Team A lead processes → invokes Team B capability → result returns to director.

### Explicit Exclusions

- No web dashboard (M10).
- No firmware signing with ed25519 (future enhancement).
- No historical metrics or analytics.

---

## Milestone 10: Observability, Dashboard, and Production Hardening

**Goal:** Web dashboard for cluster management, metrics export, log aggregation, and production-readiness improvements.

### Deliverables

1. **Web dashboard (React SPA):**
   - Cluster overview: nodes, teams, agents with status.
   - Team view: agent topology, message flow between agents.
   - Agent detail: status, logs, capabilities, resource usage.
   - Capability browser: all registered capabilities across the cluster, filterable by team.
   - Click-to-chat: interact with any agent via the browser (proxied through sidecar's OpenClaw gateway).
   - WebSocket API for live updates (agent state changes, heartbeats, log streaming).
2. **Log aggregation:**
   - Agent logs streamed to hived via NATS.
   - Stored locally with configurable rotation (default 30 days).
   - `hivectl agents logs AGENT_ID --follow --tail N` enhanced with time range filtering.
   - Dashboard streams logs in real-time per agent.
3. **Prometheus metrics export:**
   - hived exposes `/metrics` endpoint.
   - Metrics: agent count by state, VM resource usage, NATS message rates, capability invocation latency, heartbeat status.
4. **Message flow visualization:**
   - Dashboard renders a DAG of message flow between agents (who invoked what capability on whom).
   - Based on correlation IDs in message envelopes.
5. **NixOS rootfs image build:**
   - Replace Alpine rootfs with NixOS-based rootfs built from a flake.
   - Reproducible builds: same flake input produces identical rootfs.
   - All dependencies (sidecar, agent runtime, tools) bundled in the image.
6. **Production hardening:**
   - Graceful shutdown for hived (drain agents, close NATS connections).
   - Signal handling (SIGTERM, SIGINT).
   - Crash recovery: hived startup reconciles `state.json` with actual running processes.
   - Rate limiting on NATS subjects to prevent agent message floods.
   - Resource usage monitoring: alert when a node exceeds 80% memory or CPU.

### Acceptance Criteria

- [ ] Dashboard loads in browser, shows cluster overview with all nodes, teams, and agents.
- [ ] Dashboard updates in real-time when agent state changes (start, stop, fail) via WebSocket.
- [ ] Click-to-chat: user can send a message to an agent and receive a response in the browser.
- [ ] Log streaming: `hivectl agents logs AGENT_ID --follow` streams new log lines as they are produced. Dashboard shows the same stream.
- [ ] Prometheus metrics: `curl http://hived-host:PORT/metrics` returns valid Prometheus format. Key metrics (agent_count, message_rate, invocation_latency) are present.
- [ ] Message flow DAG renders correctly for a team with 3+ agents and multiple capability invocations. Renders in under 1 second for 100 messages.
- [ ] NixOS rootfs: `nix build .#rootfs` produces a rootfs image. VM boots with this image and sidecar starts.
- [ ] Graceful shutdown: `SIGTERM` to hived causes all agents to stop cleanly before hived exits.
- [ ] Crash recovery: kill hived with SIGKILL, restart it, verify it detects running VMs and resumes tracking them.
- [ ] Rate limiting: an agent publishing 10,000 messages/second is throttled. Other agents remain responsive.

### Explicit Exclusions

- No Grafana integration (use Prometheus endpoint with external Grafana if desired).
- No multi-region clustering.
- No agent marketplace or sharing mechanism.

---

## Implementation Sequence Summary

```
M1  Project Skeleton + NATS Transport
 │
M2  Manifest Parsing + Validation
 │
M3  Firecracker VM Lifecycle
 │
M4  Sidecar + Agent Runtime
 │
M5  Capability Routing + Tool Gen + Health + MEMORY.md Hot-Reload
 │
M6  Tier 2 Native Agents + Join Tokens + Capability Registry + MQTT Bridge
 │
M7  Tier 3 Firmware Agents + Build/Flash Tooling + Pre-built RPi Images
 │
M8  Multi-Node Clustering + Scheduler + Structured State + Reconciliation Loop
 │
M9  Multi-Team + Cross-Team Routing + Director + RBAC + OTA Updates
 │
M10 Dashboard + Metrics + Logs + NixOS Rootfs + Production Hardening
```

Milestones are strictly sequential. Each milestone's acceptance criteria must fully pass before the next milestone begins. No milestone may introduce items from a later milestone unless explicitly listed in its deliverables.

The MVP delivery point is **after Milestone 5**. At that point the system supports: declarative YAML-defined agents, Firecracker VM isolation, auto-generated tools, capability routing within a team, health management with auto-restart, MEMORY.md hot-reload, and full CLI management. This is sufficient for a single-node, single-team deployment that demonstrates the core value proposition.

---

## Testing Strategy

Every milestone must pass its acceptance criteria before proceeding. This section defines the testing infrastructure, test categories, and specific validation approach per milestone. Tests are cumulative: all prior milestone tests must continue to pass (regression).

### Test Infrastructure

1. **Go test framework:** All unit and integration tests use `go test` with the standard `testing` package. Table-driven tests preferred.
2. **Test helpers package:** `internal/testutil/` provides:
   - Temporary cluster root scaffolding (creates valid directory structures with configurable manifests).
   - Embedded NATS test server (starts/stops per test, random port allocation to avoid conflicts).
   - Firecracker test harness (mock or real, based on environment variable `HIVE_TEST_FIRECRACKER=real|mock`).
   - NATS message assertion helpers (subscribe, wait for message with timeout, validate envelope schema).
3. **Test tagging:** Tests are tagged by category so they can be run selectively:
   - `//go:build unit` — no external dependencies, fast, runs everywhere.
   - `//go:build integration` — requires NATS, may require filesystem access.
   - `//go:build vm` — requires Firecracker and KVM. Skipped in CI unless KVM-enabled runner.
   - `//go:build firmware` — requires connected hardware or hardware emulator.
   - `//go:build e2e` — full system tests, may take minutes.
4. **CI pipeline stages:**
   - Stage 1: `go vet ./...` + `go build ./...` (compilation and static analysis).
   - Stage 2: `go test -tags unit ./...` (unit tests, no external deps).
   - Stage 3: `go test -tags integration ./...` (integration tests, NATS embedded).
   - Stage 4: `go test -tags vm ./...` (VM tests, requires KVM runner).
   - Stage 5: `go test -tags e2e ./...` (end-to-end, requires full environment).
5. **Test fixtures directory:** `testdata/` at project root contains:
   - Valid and invalid cluster root examples for each milestone.
   - Sample agent manifests covering edge cases.
   - Pre-built minimal rootfs for VM tests (or script to generate one).

### Test Categories

**Unit tests** validate isolated functions and structs: config parsing, validation rules, state transitions, message serialization, subject pattern generation. No I/O, no network, no processes. Mock all external interfaces.

**Integration tests** validate component interactions within a single process: embedded NATS pub/sub, config watcher triggering reconciler, state persistence to `state.json` and reload on startup. Real NATS (embedded), real filesystem (temp dirs), no Firecracker.

**VM tests** validate Firecracker lifecycle and host-guest communication: VM creation/start/stop/destroy, virtio-vsock connectivity, sidecar health endpoint from host, rootfs mounting. Requires KVM-enabled host.

**End-to-end tests** validate full user-facing workflows: `hivectl init` through agent deployment, capability invocation across agents, health failure and auto-restart, multi-node cluster formation. Uses real binaries, real VMs, real NATS.

**Regression tests** are all tests from prior milestones. The full test suite runs on every milestone completion. A regression failure blocks the current milestone from being marked complete.

### Per-Milestone Test Specifications

#### M1: Project Skeleton and NATS Transport

Unit tests:
- `cluster.yaml` parsing: valid input produces correct struct fields. Missing `metadata.name` returns error. Invalid `nats.port` (negative, >65535, non-integer) returns error. Missing `nats` section uses defaults.
- JetStream config parsing: `enabled: true` vs `enabled: false` vs omitted (default true).

Integration tests:
- Embedded NATS starts on configured port. A `nats.Conn` connects successfully. Publish on subject X, subscriber on subject X receives the message with matching payload.
- JetStream: create stream, publish message, consumer reads message back.
- Port conflict: if configured port is in use, hived exits with a clear error (not a panic).

Validation approach: run `go test -tags unit,integration ./...` and confirm all pass. Manually verify `hived` starts and a NATS client (e.g., `nats pub/sub` CLI) can interact.

#### M2: Agent Manifest Parsing and Validation

Unit tests (table-driven, one test function per validation rule):
- Valid manifest: parses without error, all fields populated correctly.
- Missing `metadata.id`: returns error mentioning the field name.
- Agent ID `"my-agent"`: passes. Agent ID `"-leading-dash"`: fails. Agent ID with uppercase: fails. Agent ID 64 chars: fails. Empty string: fails.
- Duplicate agent IDs across two manifests: error identifies both paths.
- `spec.resources.memory: "512Mi"`: parses to 536870912 bytes. `"invalid"`: error.
- `spec.resources.vcpus: 0`: error. `spec.resources.vcpus: -1`: error. `spec.resources.vcpus: 2`: passes.
- Team lead references agent ID not present in any manifest: error.
- Team lead references agent whose `metadata.team` does not match the team ID: error.
- Agent volume references `shared_volumes` name not defined in team: error.
- Duplicate capability names within one agent: error.
- Capability with missing `name` or `description`: error.
- `hivectl init` output passes `hivectl validate`.

Integration tests:
- `hivectl validate` on a valid test cluster root exits 0 with no output on stderr.
- `hivectl validate` on a cluster root with 3 distinct errors exits 1 and stderr contains all 3 error messages.
- `hivectl init /tmp/test-XXX` creates expected directory structure. Verify files exist with correct template content.

Validation approach: run `go test -tags unit,integration ./...`. Confirm >90% branch coverage on validation logic via `go test -cover`.

#### M3: Firecracker VM Lifecycle

Unit tests:
- VM state machine transitions: PENDING→CREATING is valid, PENDING→RUNNING is invalid, STOPPED→CREATING is valid (restart), FAILED→CREATING is valid (manual restart). All invalid transitions return errors.
- `state.json` serialization/deserialization: write state, read back, fields match.
- Resource check: agent requests 1GB memory on a node with 512MB free returns error.

Integration tests:
- `state.json` persistence: write state, create new state manager from same file, verify state is identical.
- State recovery: write state with agent in RUNNING, simulate process not found for that VM PID, state manager marks agent FAILED on load.

VM tests:
- Create VM with 512Mi memory, 2 vcpus. Verify Firecracker process is running. Verify memory cgroup limit matches. Stop VM. Verify process is gone. Destroy VM. Verify socket and rootfs copy are deleted.
- Start same agent twice: second call returns error, original VM unaffected.
- Stop a STOPPED agent: returns error (or no-op with warning), no crash.
- Destroy a RUNNING agent: stops then destroys in one operation.
- VM boot time: measure time from create call to RUNNING state. Assert < 5 seconds.
- Kill Firecracker process externally (SIGKILL). On next state check, hived detects the dead process and transitions to FAILED.

End-to-end tests:
- `hivectl agents start test-agent` → `hivectl agents list` shows RUNNING → `hivectl agents status test-agent` shows PID, uptime, resources → `hivectl agents stop test-agent` → list shows STOPPED → `hivectl agents destroy test-agent` → list shows no agent.

Validation approach: VM tests require KVM. CI runs unit + integration on every commit. VM tests run on KVM-enabled runner or manually before milestone sign-off.

#### M4: Sidecar and Agent Runtime

Unit tests:
- Heartbeat message construction: correct NATS subject, correct JSON schema, correct agent ID and timestamp.
- Health endpoint response: mock sidecar health state, verify JSON output matches expected schema.
- Capability list endpoint: mock manifest capabilities, verify response matches.

Integration tests:
- Sidecar connects to embedded NATS and publishes heartbeat. Test subscriber receives heartbeat on correct subject within expected interval.
- hived heartbeat monitor: receives heartbeats, agent stays RUNNING. Stop sending heartbeats, agent transitions to UNHEALTHY after `3 * interval`.

VM tests:
- Boot VM with sidecar in rootfs. From host, connect to sidecar health endpoint via virtio-vsock tunnel. Verify response indicates sidecar running and agent runtime running.
- Verify agent files (AGENTS.md, SOUL.md, MEMORY.md) exist at expected paths inside VM (check via sidecar endpoint or virtio-vsock command).
- Kill agent runtime process inside VM (via sidecar command or direct process kill). Sidecar heartbeat changes to UNHEALTHY. hived detects UNHEALTHY within `3 * interval`.

End-to-end tests:
- Deploy agent with OpenClaw runtime and model config. Verify via `hivectl agents status` that both sidecar and runtime are healthy. Verify heartbeats in NATS via `hivectl` or test subscriber.

#### M5: Capability Routing, Tool Generation, and Health Management

Unit tests:
- Tool definition generation: given a capability schema (name, description, inputs, outputs), produce correct OpenClaw tool file content. Verify tool name, parameter schema, and description match.
- Restart policy logic: `on-failure` restarts on crash, does not restart on clean stop. `always` restarts on both. `never` does not restart. `maxRestarts` exceeded transitions to FAILED.
- Backoff calculation: first restart waits `backoff` duration, subsequent restarts also wait `backoff`.
- MEMORY.md change detection: given old and new content, correctly identifies a change (not triggered by identical content).

Integration tests:
- Two NATS clients simulate agents A and B. B subscribes to its capability subject. A publishes a capability request with reply subject. B receives request, publishes response on reply. A receives response. Verify message envelope fields (id, from, to, type, correlation_id).
- Timeout: A publishes request, B does not respond. A's invocation times out after configured duration. Error message is clear.
- Health auto-restart: start mock agent publishing heartbeats. Stop heartbeats. Verify hived triggers restart action after `maxFailures * interval`. Verify restart count increments. Repeat until `maxRestarts`. Verify agent enters FAILED and no more restarts.
- MEMORY.md watch: write initial file, start watcher, modify file, verify watcher emits change event within 5 seconds.

VM tests:
- Deploy team with lead (Agent A) + tool (Agent B). Verify lead's runtime filesystem contains auto-generated tool files for Agent B's capabilities. Invoke tool from lead (via sidecar's capability invocation endpoint). Verify response matches Agent B's capability output.
- Kill Agent B's runtime. Verify hived restarts Agent B. After restart, verify capability invocation from Agent A still works.
- Edit MEMORY.md on host. Verify updated content appears inside VM within 5 seconds (read via sidecar endpoint).

End-to-end tests:
- Deploy the reference team (1 lead + 2 tool agents). Via `hivectl connect AGENT_ID`, send a prompt to the lead that requires invoking both tool agents' capabilities. Verify the lead's response incorporates data from both tools.
- Crash test: kill one tool agent's VM process. Verify auto-restart. Repeat the same prompt. Verify it succeeds after restart.

#### M6: Tier 2 Native Agents and Cross-Tier Communication

Unit tests:
- Hardware inventory collection: mock `/proc/cpuinfo`, `/proc/meminfo`, KVM device check. Verify tier classification: KVM + 4GB = Tier 1, no KVM + Linux = Tier 2.
- Token generation: output is URL-safe, stored hash matches input. Token with expired TTL is rejected.
- Capability registry CRUD: add agent capabilities, query by capability name, remove agent, verify capabilities gone.
- Sidecar library mode: same interface methods as standalone mode. Mock underlying transport, verify identical behavior.

Integration tests:
- `hive-agent join` protocol: start hived with embedded NATS. Start `hive-agent` in a separate process with valid token. Verify node appears in hived's node list with correct hardware info and tier.
- Invalid token: `hive-agent join` with wrong token. Verify rejection error message. Node does not appear in node list.
- Expired token: create token with 1-second TTL. Wait 2 seconds. Join attempt rejected.
- Capability registry: register two agents with overlapping capability names. Query returns both. Deregister one, query returns only the other.
- MQTT bridge: connect MQTT client to port 1883. Publish on MQTT topic. Verify message arrives on corresponding NATS subject.

VM + native tests:
- Start hived with one VM agent. Start `hive-agent` (native) on same machine (simulating Tier 2). Native agent registers, publishes heartbeats, capabilities are in registry. VM agent invokes native agent's capability. Response returns within 2 seconds.
- Stop native agent process. hived detects UNHEALTHY. Restart native agent. Capabilities re-register. Invocation works again.

End-to-end tests:
- Full cross-tier deployment: 1 Tier 1 VM lead, 1 Tier 2 native tool agent. Lead invokes tool agent's capability via auto-generated tool. Verify response.

#### M7: Tier 3 Firmware Agents and Device Tooling

Unit tests:
- C SDK: message serialization produces valid JSON matching envelope schema. Heartbeat publishes on correct MQTT topic. Capability response matches expected format.
- MicroPython SDK: same validation as C SDK.
- Firmware manifest parsing: `spec.tier: firmware`, `spec.firmware.platform`, `spec.firmware.board` fields parse correctly.

Integration tests:
- MQTT heartbeat: firmware simulator publishes heartbeats on MQTT. hived tracks device as ONLINE. Stop heartbeats. hived marks OFFLINE after timeout.
- Capability invocation over MQTT: publish capability request on NATS (from VM agent). MQTT bridge translates to MQTT. Firmware simulator responds on MQTT. Bridge translates back to NATS. VM agent receives response.
- `hivectl firmware build`: given a test firmware source directory with a valid ESP-IDF project, build command produces a binary. (Requires ESP-IDF toolchain installed or skipped with tag.)

Hardware tests (tagged `firmware`, run manually or on hardware-equipped CI):
- Flash firmware to real ESP32. Device connects to hived MQTT. Heartbeats arrive. Capability invocation returns real sensor data.
- Pi Pico W with MicroPython SDK: same validation as ESP32.
- RPi image: flash to SD card, boot, `hive-agent join` succeeds, agent appears in cluster.

End-to-end tests:
- Full 3-tier deployment: Tier 1 lead (VM), Tier 2 camera agent (native on RPi), Tier 3 sensor (firmware on ESP32). Lead invokes sensor read (cross-tier, NATS→MQTT). Lead invokes camera capture (cross-tier, NATS→TCP). Verify both responses arrive at lead.

#### M8: Multi-Node Clustering and Structured State

Unit tests:
- Scheduler bin-packing: given 3 nodes with known resources and 5 agents with known requirements, verify optimal assignment. Verify `placement.nodeId` override. Verify `placement.nodeLabels` constraint. Verify PENDING when no node fits.
- State directory serialization: write structured state to `.state/`, read back, verify all fields. Migration from `state.json` to `.state/` directory.
- Reconciliation diff: given desired state and actual state, verify correct action list (create, destroy, restart).

Integration tests:
- External NATS: start external NATS server. hived connects as client. All existing tests (pub/sub, JetStream, heartbeats) pass against external NATS.
- Reconciliation loop: start hived. Add agent manifest to cluster root. Verify reconciler generates "create agent" action within 10 seconds. Remove manifest. Verify "destroy agent" action within 10 seconds.
- State replication: start root hived. Start worker hived connected to root. Create agent on root. Verify worker's local state cache reflects the new agent within 1 second.

Multi-node tests (tagged `e2e`, require multiple VMs or containers):
- 3-node cluster formation: 1 root, 2 workers. All nodes visible in `hivectl nodes list` with correct roles.
- Agent scheduling: create 5 agents. Verify distribution across workers (not all on one node). Resource constraints respected.
- Node drain: `hivectl nodes drain WORKER_1`. Verify all VMs migrate to WORKER_2. WORKER_1 has zero agents.
- Node failure: kill WORKER_2 process. Agents on WORKER_2 detected as FAILED. When WORKER_2 rejoins, agents resume.
- Reconciliation under partition: disconnect WORKER_1 from NATS. Create agent on root assigned to WORKER_1. Reconnect WORKER_1. Agent eventually starts.

#### M9: Multi-Team, Cross-Team, Director, RBAC, OTA

Integration tests:
- Team isolation: two teams on same NATS. Team A publishes to `team.teamA.broadcast`. Subscriber on `team.teamB.broadcast` does not receive it.
- Cross-team capability: Team A exposes capability X. Team B queries org capability registry. X appears with team prefix. Team B invokes `teamA.X`. Response arrives.
- Unexposed capability: Team A has capability Y not in `crossTeamCapabilities`. Team B invocation of `teamA.Y` returns permission error.
- mTLS: worker connects without valid cert. Connection rejected. Worker connects with valid cert. Connection accepted.
- JWT auth: issue token with `viewer` role. Attempt `hivectl agents stop`. Rejected. Issue `operator` token. Same command succeeds.
- OTA update: MQTT firmware simulator advertises current version. `hivectl firmware update` pushes new binary. Simulator receives all chunks, reassembles, verifies SHA-256.

End-to-end tests:
- Director workflow: 2 teams + director. Director sends task to Team A lead. Team A processes, invokes Team B cross-team capability. Result flows back to director. Full message chain validated via correlation IDs.
- RBAC end-to-end: create 3 users (admin, operator, viewer). Each attempts the same set of operations. Verify correct allow/deny for each role.

#### M10: Dashboard, Metrics, Logs, Production Hardening

Integration tests:
- WebSocket API: connect WebSocket client. Start agent. Verify state change event received on WebSocket.
- Prometheus endpoint: `GET /metrics` returns valid Prometheus text format. Contains expected metric names. Metric values update after actions (start agent → agent count increases).
- Log streaming: agent produces log lines. NATS subscriber receives them. `hivectl agents logs --follow` outputs them.
- Graceful shutdown: send SIGTERM to hived. Verify all agents stop before hived exits. Verify NATS connections close cleanly.
- Crash recovery: write state with 3 RUNNING agents. Kill hived (SIGKILL). Start hived. Verify it discovers the running VMs and resumes tracking.
- Rate limiting: single agent publishes 10,000 msg/sec on NATS. Verify throttling engages. Verify other agents' message latency stays below 100ms.

End-to-end tests:
- Dashboard smoke test: start cluster with 2 teams, 5 agents. Open dashboard in headless browser (Playwright or similar). Verify cluster overview shows correct node/team/agent counts. Click into team view. Verify agent topology renders. Click agent. Verify logs stream.
- NixOS rootfs: build rootfs from flake. Boot VM with it. Sidecar starts. Agent runtime starts. Capability invocation works.

### Regression Policy

After each milestone, the complete test suite for all completed milestones runs as a single `go test` invocation. Any failure in a prior milestone's tests blocks sign-off on the current milestone. Test output is logged and the specific failing test is identified before any investigation of current milestone work.

Test execution command per CI stage:
```
# All unit tests (every commit)
go test -tags unit -race -count=1 ./...

# Integration tests (every commit)
go test -tags integration -race -count=1 -timeout 5m ./...

# VM tests (milestone sign-off or KVM runner)
go test -tags vm -count=1 -timeout 10m ./...

# End-to-end tests (milestone sign-off)
go test -tags e2e -count=1 -timeout 30m ./...

# Full regression (milestone sign-off only)
go test -tags unit,integration,vm,e2e -race -count=1 -timeout 45m ./...
```

The `-race` flag is included on unit and integration tests to catch data races early. The `-count=1` flag disables test caching to ensure fresh runs.
