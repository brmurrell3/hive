[← Back to Documentation](README.md)

# Hive CLI & Interaction Reference

## COMMAND STRUCTURE

All commands accept:
- `--config PATH` or `HIVE_CONFIG` env var (local control plane)
- `--control-plane ADDRESS` (remote control plane)
- Multi-user: additionally `--user USER_ID --token TOKEN` or `HIVE_USER`/`HIVE_TOKEN` env vars

**Output format**: tables default. `--output json` available. Errors → stderr. **Exit codes**: 0 (success), 1 (error), 2 (usage).

---

## HIVECTL CLUSTER COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl init PATH` | Scaffold cluster root (cluster.yaml, agents/, teams/, .state/). Idempotent. | Path to cluster root |
| `hivectl status` | Cluster overview: name, nodes by tier, agent count, team count, NATS status | Table |
| `hivectl validate` | Validate all manifests | 0 or 1, errors to stderr |

---

## HIVECTL TOKEN COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl tokens create [--ttl DURATION]` | Generate 256-bit join token. Print token to stdout. Store SHA-256 hash. | Token string |
| `hivectl tokens list` | List active tokens (creation time, last-used) | Table |
| `hivectl tokens revoke PREFIX` | Revoke token by first 8 chars | Status message |

---

## HIVECTL NODE COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl nodes list` | All nodes: ID, tier, arch, status, resources, agent count, labels | Table |
| `hivectl nodes status NODE_ID` | Detail view for single node | JSON or formatted |
| `hivectl nodes approve NODE_ID` | Approve pending node | Status |
| `hivectl nodes remove NODE_ID` | Deregister (Tier 1: drain VMs first. Tier 2: stop agent. Tier 3: deregister) | Status |
| `hivectl nodes drain NODE_ID` | Migrate VMs off node (Tier 1 only) | Progress or status |
| `hivectl nodes cordon NODE_ID` | Stop scheduling, keep existing workloads | Status |
| `hivectl nodes uncordon NODE_ID` | Resume scheduling | Status |
| `hivectl nodes label NODE_ID KEY=VALUE` | Add or update label | Status |
| `hivectl nodes unlabel NODE_ID KEY` | Remove label | Status |

---

## HIVECTL AGENT COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl agents list` | All agents: ID, team, tier, node, status, proxy URL (if conversational) | Table |
| `hivectl agents status AGENT_ID` | Detail: capabilities, health, resources, node, uptime | JSON or formatted |
| `hivectl agents logs AGENT_ID [--follow] [--since DURATION] [--tail N]` | Stream logs. --follow = live, --since = from timestamp, --tail = last N lines | Text stream |
| `hivectl agents restart AGENT_ID` | Restart (VM: recreate. Native: restart process) | Status |
| `hivectl agents stop AGENT_ID` | Stop agent | Status |
| `hivectl agents start AGENT_ID` | Start agent | Status |
| `hivectl agents destroy AGENT_ID [--purge]` | Destroy agent. --purge deletes workspace. | Status |
| `hivectl agents exec AGENT_ID -- COMMAND` | Exec inside VM (Tier 1 vm) or on host (Tier 1 native, Tier 2) | Command output |
| `hivectl agents capabilities AGENT_ID` | List declared capabilities with JSON schemas | Table or JSON |

---

## HIVECTL TEAM COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl teams list` | All teams: ID, lead, member count by tier | Table |
| `hivectl teams status TEAM_ID` | Detail: members, capabilities, bus status, message counts | JSON or formatted |
| `hivectl teams capabilities TEAM_ID` | All capabilities in team (local + cross-team allowed) | Table or JSON |

---

## HIVECTL FIRMWARE COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl firmware build AGENT_ID [--target PLATFORM]` | Compile firmware | Build log, binary path |
| `hivectl firmware flash AGENT_ID --port PORT [--baud BAUD]` | Flash via serial | Progress, status |
| `hivectl firmware update AGENT_ID --binary PATH` | OTA update via MQTT | Status |
| `hivectl firmware sign AGENT_ID --key PATH` | Sign firmware (Phase 3) | Signature, status |
| `hivectl firmware monitor AGENT_ID --port PORT` | Stream debug output from serial connection (Tier 3 only) | Text stream |

---

## HIVECTL MESSAGE COMMANDS (DEBUG)

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl messages send --from AGENT_ID --to AGENT_ID --payload JSON` | Inject message on bus | Status |
| `hivectl messages subscribe SUBJECT [--since DURATION]` | Subscribe and print messages | Message stream |
| `hivectl capabilities invoke AGENT_ID CAPABILITY_NAME --inputs JSON` | Test capability end-to-end | Result JSON |

---

## HIVECTL USER COMMANDS (Phase 3, Multi-User)

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl users create USER_ID --role operator\|viewer --teams TEAM_IDS --agents AGENT_IDS` | Create user, output token | Token string |
| `hivectl users list` | List users with role and access scope | Table |
| `hivectl users update USER_ID --role\|--teams\|--agents` | Modify user | Status |
| `hivectl users revoke USER_ID` | Remove user | Status |
| `hivectl users rotate USER_ID` | Regenerate token | New token string |

---

## HIVECTL CONNECT COMMAND

**Usage**: `hivectl connect AGENT_ID [--web]`

Behavior varies by agent runtime and tier:

### OpenClaw Agents (Tier 1 vm, Tier 1 native, Tier 2)

**Default (no `--web`)**:
- Opens terminal-based OpenClaw CLI session
- Mechanism:
  - hivectl opens WebSocket to control plane proxy
  - Control plane proxy forwards to agent's OpenClaw Gateway (port 18789 inside VM, or host port for native)
  - hivectl renders CLI chat interface: stdin → WebSocket → Gateway → OpenClaw → response → WebSocket → stdout
- Interactive, full-capability conversation

**With `--web`**:
- Opens browser to `http://HOST:PROXY_PORT`
- Proxy port: `19000 + sequential_index` (assigned by control plane, visible in `hivectl agents list`)
- Browser loads OpenClaw WebChat UI
- Same capabilities as CLI, web-based rendering

### Custom Runtime Agents (Tier 1 vm, Tier 1 native, Tier 2)

**Default (no `--web`)**:
- Opens interactive message exchange via sidecar HTTP API
- Mechanism:
  ```
  Loop:
    1. Prompt user for input
    2. POST /messages/send {to: AGENT_ID, payload: {type: "user_message", content: INPUT}}
    3. GET /messages/receive ?timeout=30s (long poll)
    4. Display response
    5. Repeat
  ```
- UX is basic; custom agents get what they implement

**With `--web`**:
- Opens simple web UI served by control plane
- Wraps same send/receive loop in HTML form
- No advanced features unless agent implements them

### Firmware Agents (Tier 3)

**Opens capability testing interface**:
- Mechanism:
  ```
  1. hivectl lists agent capabilities
  2. Loop:
    a. Display numbered capability list
    b. User selects capability by number
    c. Prompt for inputs (JSON format, if any)
    d. POST capability invocation via NATS/MQTT
    e. Display result
    f. Repeat
  ```
- Limited to capability invocation; no general-purpose messaging
- No `--web` in Phase 1-3

---

## HUMAN INTERACTION MODEL

### Primary: Talk to Lead
- **Default mode**: Human talks to team lead agent (OpenClaw or custom conversational runtime)
- **Lead orchestrates team**: Receives directives, dispatches to members
- **Access**: `hivectl connect LEAD_AGENT_ID`
- **Best for**: High-level coordination, task delegation, team-wide decisions

### Secondary: Talk to Any Agent
- **Access**: `hivectl connect AGENT_ID` (bypasses team hierarchy)
- **Available for**: Any agent, any tier, regardless of team assignment
- **Use case**: Operator escape hatch, direct specialist access, debugging

### Tertiary: Observe (Read-Only)
- `hivectl agents logs AGENT_ID`: stream output
- `hivectl messages subscribe SUBJECT`: watch bus traffic
- `hivectl teams status TEAM_ID`: team activity overview

### Director Entry Point (if configured)
- **Cluster.yaml spec**: `spec.director.agentId` (optional)
- **Access**: `hivectl connect DIRECTOR_AGENT_ID`
- **Hierarchy**: Director talks to leads, leads talk to teams
- **Bypass**: Human can always `hivectl connect` to any agent directly
- **Best for**: Multi-team orchestration, cross-team task management

### Multi-Session Support
- Multiple simultaneous sessions supported (multiple terminals, browser tabs)
- Each session is independent conversation with target agent
- Messages from one session do **not** appear in another
- Allows parallel interaction with multiple agents

---

## DIRECTOR AGENT TOOLS

Injected by control plane into director agent sidecar. Enables cross-team orchestration:

| Tool | Signature | Purpose |
|------|-----------|---------|
| `hive_list_teams()` | → team[] | All teams: lead ID, member count, status |
| `hive_list_all_agents()` | → agent[] | All agents across all teams |
| `hive_message_lead(team_id, message)` | → status | Send message to specific team lead |
| `hive_message_agent(agent_id, message)` | → status | Send message to any agent |
| `hive_broadcast_leads(message)` | → status | Send message to all leads simultaneously |
| `hive_broadcast_all(message)` | → status | Send message to every agent in cluster |
| `hive_invoke_capability(agent_id, capability, inputs)` | → result | Invoke ANY capability on ANY agent (bypasses crossTeamCapabilities restrictions) |
| `hive_team_status(team_id)` | → status_obj | Detailed team status, message counts, agent states |
| `hive_cluster_status()` | → status_obj | Cluster-wide overview (agents, nodes, health) |

**Note**: Director bypasses `crossTeamCapabilities` restrictions. This is explicit and by design.

---

## LEAD AGENT TOOLS

Injected by control plane into team lead sidecar. Enables team coordination:

| Tool | Signature | Purpose |
|------|-----------|---------|
| `hive_list_members()` | → agent[] | Team members: tier, status, capabilities |
| `hive_dispatch_task(agent_id, task)` | → task_id | Send task to team member |
| `hive_broadcast_team(message)` | → status | Send message to all team members |
| `hive_collect_results(task_id)` | → result | Retrieve results from task |
| `hive_check_status(agent_id)` | → agent_status | Agent health and activity |
| `hive_message_lead(team_id, message)` | → status | Message another team lead (always allowed, regardless of crossTeamCapabilities) |
| `hive_broadcast_leads(message)` | → status | Broadcast to all team leads |

---

## OBSERVABILITY

### Log Collection

**VM Agents** (Tier 1 vm):
- Agent runtime stdout/stderr → sidecar
- Written to: `/workspace/.logs/agent.log` (inside VM, accessible via virtiofs)
- Also forwarded to NATS: `hive.logs.{AGENT_ID}`
- Rotation: 10MB per file, 3 rotated files kept (configurable in `cluster.yaml spec.defaults.logging`)

**Native Agents** (Tier 1 native, Tier 2):
- Agent runtime stdout/stderr → hive-agent or hived
- Written to: `/var/lib/hive/agents/{AGENT_ID}/logs/agent.log`
- Forwarded to NATS: `hive.logs.{AGENT_ID}`
- Rotation: same as VM defaults

**Firmware Agents** (Tier 3):
- No traditional logs
- Device metrics included in health heartbeats
- Serial debug output: `hivectl firmware monitor AGENT_ID --port PORT`
- No NATS log forwarding in Phase 1-3

### Log Access

| Command | Source | Behavior |
|---------|--------|----------|
| `hivectl agents logs AGENT_ID` | NATS | Subscribe to `hive.logs.{AGENT_ID}`, live stream |
| `hivectl agents logs AGENT_ID --since 1h` | Filesystem | Read from log files via control plane API since timestamp |
| `hivectl agents logs AGENT_ID --tail 100` | Filesystem | Last N lines from log file |
| `hivectl agents logs AGENT_ID --follow` | NATS | Live stream (subscribe to subject) |

### Log Storage

- **Phase 1-3**: Per-agent in workspace (VM) or host filesystem (native). No centralized storage.
- **Phase 4**: Optional centralized log aggregation via NATS JetStream. `hive.logs.*` subjects persisted and searchable via dashboard.

### Metrics

**Health Heartbeats** contain:
- Uptime
- Tier-specific:
  - VM (Tier 1 vm, Tier 2): CPU, memory
  - Firmware (Tier 3): Battery, RSSI, heap
  - Native (Tier 1 native, Tier 2): Process memory, CPU

**Access**:
- Point-in-time: `hivectl agents status AGENT_ID`
- Streaming: `hive.metrics.{AGENT_ID}` NATS subject
- Phase 4: Dashboard visualizes metrics over time

### Cluster-Level Observability

| Source | Interval | Data |
|--------|----------|------|
| `hivectl status` | On-demand | Node counts, agent counts, team counts, NATS status |
| `hivectl teams status TEAM_ID` | On-demand | Message counts, agent states, capability availability |
| `hive.metrics.cluster` | 30s | Total agents, nodes, capability count, message rate |

---

## WEB DASHBOARD (Phase 4)

REST/WebSocket API exposed by hived. Does **not** replace CLI in Phases 1-3; supplements it.

### Views

| View | Purpose |
|------|---------|
| Cluster Overview | Nodes by tier, agents by team, health status (red/yellow/green) |
| Team View | Team members, capabilities, message flow visualization |
| Agent Detail | Health, resources, logs, conversation history |
| Click-to-Chat | WebSocket session to agent Gateway (works with OpenClaw agents) |
| Capability Browser | Browse and invoke capabilities end-to-end |
| Org Chart | Visual hierarchy: director → leads → agents (if director configured) |

### Architecture

- Dashboard connects to control plane via REST/WebSocket API
- All data already flows through hived and NATS
- Dashboard is **read/interact layer only**, not new data source
- Backed by existing observability (logs, metrics, NATS subjects)

### Phase 1-3 vs Phase 4

| Phase | Primary Interface | Secondary |
|-------|-------------------|-----------|
| 1-3 | CLI (hivectl) | Logs via NATS, manual inspection |
| 4 | CLI (hivectl) | Web dashboard, historical metrics |

---

## EXIT CODES

| Code | Meaning |
|------|---------|
| 0 | Command succeeded |
| 1 | Runtime error (invalid cluster, agent not found, execution failure, etc.) |
| 2 | Usage error (missing required arg, invalid flag, etc.) |

---

## ENV VARS (Summary)

| Var | Purpose | Example |
|-----|---------|---------|
| `HIVE_CONFIG` | Path to local cluster root (default: ./cluster.yaml) | `/etc/hive/cluster.yaml` |
| `HIVE_CONTROL_PLANE` | Remote control plane address | `hive.example.com:9090` |
| `HIVE_USER` | User ID (Phase 3 multi-user) | `alice` |
| `HIVE_TOKEN` | Auth token (Phase 3 multi-user) | `hv_xxxxxxxxxxxxxxxx` |

---

## ERROR HANDLING

- **Invalid config**: Error to stderr, exit 1. Message: "config validation failed: ..."
- **Agent not found**: Error to stderr, exit 1. Message: "agent {ID} not found"
- **Network failure**: Error to stderr, exit 1. Message: "connection to control plane failed: ..."
- **Permission denied** (Phase 3): Error to stderr, exit 1. Message: "user {UID} lacks access to {resource}"
- **Usage error**: Error to stderr, exit 2. Message: "usage: hivectl {command} ..."

---

## DETERMINISM & TESTING

- Logs and metrics are append-only
- Commands are idempotent where stated (e.g., `hivectl init`, `hivectl tokens create`)
- No hidden side effects (all state changes logged to `hive.log.{AGENT_ID}` or `hive.metrics.cluster`)
- Timestamps in UTC, ISO 8601 format
- Table output is deterministic (sorted by primary key)
- JSON output includes version key for schema versioning

