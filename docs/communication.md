[← Back to Documentation](README.md)

# Hive Communication Specification

## TRANSPORT LAYERS

| Tier | Agent Type | Connection | Details |
|------|-----------|-----------|---------|
| 1 | VM | virtio-vsock | Sidecar inside VM → host NATS |
| 1 | Native | localhost | Sidecar → NATS via IPC |
| 2 | hive-agent | TCP/TLS | Control plane NATS |
| 3 | Firmware | MQTT | NATS built-in MQTT listener (port 1883) |

---

## NATS SUBJECT HIERARCHY

> **Namespace convention:** All NATS subjects use the `hive.` prefix as a namespace to avoid collisions with other NATS applications sharing the same server.

### Health & Metrics
```
hive.health.{AGENT_ID}
hive.metrics.{AGENT_ID}
```

### Join Protocol
```
hive.join.request
hive.join.status.{NODE_NAME}
```

### Control
```
hive.control.{AGENT_ID}
```

### Capabilities
```
hive.capabilities.registry                                    [announce on register/deregister]
hive.capabilities.{AGENT_ID}.{CAPABILITY}.request
hive.capabilities.{AGENT_ID}.{CAPABILITY}.response
```

### Agent Messaging
```
hive.agent.{AGENT_ID}.inbox
```

### Team Subjects
```
hive.team.{TEAM_ID}.broadcast
hive.team.{TEAM_ID}.management                                [lead control channel]
hive.team.{TEAM_ID}.tasks.{TASK_ID}
hive.team.{TEAM_ID}.results.{AGENT_ID}
```

### Organization Subjects (Phase 3)
```
hive.org.broadcast                                            [all leads + director]
hive.org.leads.{TEAM_ID}                                      [external → team lead]
hive.org.cross.{SOURCE_TEAM}.{TARGET_TEAM}
hive.org.capabilities.{AGENT_ID}.{CAPABILITY}.request
hive.org.capabilities.{AGENT_ID}.{CAPABILITY}.response
```

---

## MQTT TOPIC MAPPING (Tier 3)

NATS MQTT listener: port 1883

| MQTT Topic | NATS Subject |
|-----------|-------------|
| `hive/agent/{AGENT_ID}/inbox` | `hive.agent.{AGENT_ID}.inbox` |
| `hive/team/{TEAM_ID}/broadcast` | `hive.team.{TEAM_ID}.broadcast` |
| `hive/health/{AGENT_ID}` | `hive.health.{AGENT_ID}` |
| `hive/capabilities/{AGENT_ID}/{CAPABILITY}/request` | `hive.capabilities.{AGENT_ID}.{CAPABILITY}.request` |
| `hive/capabilities/{AGENT_ID}/{CAPABILITY}/response` | `hive.capabilities.{AGENT_ID}.{CAPABILITY}.response` |
| `hive/join/request` | `hive.join.request` |
| `hive/join/status/{AGENT_ID}` | `hive.join.status.{AGENT_ID}` |

**QoS Default:** 1 (at-least-once)

**Binary Format:** MQTT bridge on control plane translates MessagePack ↔ JSON. Device declares format preference in join request. Default: JSON.

---

## MESSAGE ENVELOPE

All messages use unified JSON structure:

```json
{
  "id": "string, UUID v4, REQUIRED",
  "from": "string, sender agent ID, REQUIRED",
  "to": "string, target agent/team/broadcast, REQUIRED",
  "type": "string, REQUIRED",
  "timestamp": "string, RFC3339, REQUIRED",
  "payload": "object, REQUIRED",
  "reply_to": "string, OPTIONAL",
  "correlation_id": "string, OPTIONAL"
}
```

**type enum:** `task | result | broadcast | status | health | control | capability-request | capability-response | error`

---

## MESSAGE TYPE PAYLOADS

### capability-request
```yaml
capability: string
inputs: object                    [matches capability input schema]
timeout: duration string          [default "30s"]
```

### capability-response
```yaml
capability: string
status: enum(success|error|timeout)
outputs: object                   [when status=success, matches schema]
error: object                     [when status=error, see ERROR HANDLING]
duration_ms: int
```

### task
```yaml
task_id: string
description: string
context: object
priority: enum(low|normal|high|critical)    [default "normal"]
```

### result
```yaml
task_id: string
status: enum(completed|failed|partial)
output: any
error: object                     [when status=failed]
duration_ms: int
```

### health
```yaml
healthy: bool
uptime_seconds: int
tier: string                      [vm|native|firmware]
cpu_percent: int                  [VM only]
memory_used_bytes: int            [VM only]
battery_percent: int              [Firmware only]
rssi: int                         [Firmware only]
free_heap_bytes: int              [Firmware only]
```

### broadcast
```yaml
content: any
```

### status
```yaml
state: string
details: object
```

---

## CAPABILITY INVOCATION PROTOCOL

### Within-Team Flow

**Request:**
1. Agent B (LLM) invokes auto-generated tool (name = capability name)
2. Sidecar publishes to `hive.capabilities.{AGENT_A}.{CAPABILITY}.request`
3. Message includes `reply_to` subject and correlation_id

**Response:**
4. Agent A receives request
5. Agent A executes capability
6. Agent A publishes to `reply_to` subject with matching `correlation_id`
7. Sidecar on B returns result to LLM as tool output

**Modes:**
- **Sync:** default 30s timeout, configurable per-invocation
- **Async:** capability.async=true returns task_id immediately, result via `hive.team.{TEAM_ID}.results.{AGENT_ID}`

### Cross-Team Flow (Phase 3)

Same as within-team, differences:
- **Subject:** `hive.org.capabilities.{AGENT_ID}.{CAPABILITY}.request/response`
- **Tool Name:** `{CAPABILITY_NAME}-{TEAM_ID}` (namespaced)
- **Exposure:** only if team manifest includes capability in `crossTeamCapabilities`

### Capability Discovery

- On agent registration: control plane publishes to `hive.capabilities.registry`
- All sidecars subscribe, update local tool definitions
- New team member capabilities auto-discovered by existing members

### Tool Generation

**OpenClaw agents:**
- Each remote capability → skill at `/workspace/skills/hive-remote-{AGENT_ID}/SKILL.md`

**Custom agents:**
- Capabilities via sidecar: `GET /capabilities`, `POST /capabilities/{NAME}/invoke`

**Firmware agents:**
- Capabilities as firmware SDK functions, invoked via MQTT request/response

### Tool Naming & Collision Resolution

| Scenario | Tool Name | Notes |
|----------|-----------|-------|
| Within-team local | `{CAPABILITY}` | No prefix |
| Within-team remote | `{CAPABILITY}` | No prefix, collision resolved by agent providing first |
| Cross-team | `{CAPABILITY}-{TEAM_ID}` | Always suffixed |
| Local + cross-team same name | Local wins, cross-team suffixed | `{CAPABILITY}` for local, `{CAPABILITY}-{TEAM_ID}` for cross-team |
| Two teams same capability | `{CAPABILITY}-team-a`, `{CAPABILITY}-team-b` | Both suffixed |
| Director agent | All available, cross-team suffixed | `{CAPABILITY}` for local, `{CAPABILITY}-{TEAM_ID}` for cross-team |

---

## ERROR HANDLING

### Error Object Schema

```json
{
  "code": "string, REQUIRED",
  "message": "string, REQUIRED",
  "retryable": "bool, REQUIRED",
  "details": "object, OPTIONAL"
}
```

### ERROR_CODES

| Code | Description | Retryable |
|------|-------------|-----------|
| `AGENT_OFFLINE` | Target agent not reachable | true |
| `CAPABILITY_NOT_FOUND` | Agent lacks requested capability | false |
| `CAPABILITY_TIMEOUT` | Execution exceeded timeout | true |
| `CAPABILITY_FAILED` | Execution error in handler | depends on error |
| `CAPABILITY_BUSY` | Agent busy (Tier 3 single-threaded) | true |
| `INVALID_INPUTS` | Inputs don't match schema | false |
| `UNAUTHORIZED` | Caller not authorized | false |
| `PAYLOAD_TOO_LARGE` | Payload exceeds max size | false |
| `INTERNAL_ERROR` | Unexpected error | false |

### Retry Behavior

Sidecar implements automatic retry:

```yaml
max_retries: 2                    [total 3 attempts]
backoff:
  type: exponential
  base: 1s
  multiplier: 2x
  max: 10s
retry_condition: error.retryable == true
on_exhaustion: return error to caller
```

### Circuit Breaker (Phase 2+)

Per-agent, per-capability:

```yaml
states:
  closed: requests flow normal
  open: requests immediately fail (AGENT_OFFLINE)
  half-open: one request allowed to test recovery

trip_condition: 5 consecutive errors within 60s
open_duration: 30s before transition to half-open
recovery:
  success: transition to closed, re-add to tool definitions
  failure: return to open state
```

---

## SIDECAR HTTP API

Runs at `localhost:9100` (VM Tier 1) or host (Tier 2 via hive-agent)

### Messages

```
POST /messages/send
  Request: {to, payload}
  Response: {message_id}

GET /messages/receive
  Query: ?timeout=30s
  Response: {messages: [{from, payload, timestamp, message_id}]}

POST /messages/reply
  Request: {message_id, payload}
  Response: {ok}
```

### Status & Discovery

```
GET /status
  Response: {agent_id, team_id, tier, uptime, connected}

GET /capabilities
  Response: [capability definitions]
```

### Capability Invocation

```
POST /capabilities/{NAME}/invoke
  Request: {inputs}
  Response: {outputs} or {task_id} for async

GET /blobs/{AGENT_ID}/{UUID}
  Response: raw binary data [large payload retrieval]
```

### Team Operations (Lead Agents Only)

```
GET /team/members
  Response: [{id, tier, status, capabilities}]

POST /team/broadcast
  Request: {payload}
  Response: {message_id}
```

---

## BINARY ENCODING

### Large Payloads

Payloads > 256KB use binary blob reference:

```json
{
  "blob_ref": "{AGENT_ID}/{UUID}",
  "size_bytes": 4194304,
  "encoding": "gzip|raw"
}
```

**Retrieval:** `GET /blobs/{AGENT_ID}/{UUID}`

**Encoding:** gzip compression recommended. Firmware devices may set raw.

**Lifetime:** Blobs retained for 24h after last access. TTL reset on access.

### MessagePack Format

Tier 3 firmware devices (MQTT):

```
[id, from, to, type, timestamp, payload, reply_to, correlation_id]
```

- Compact binary encoding
- Optional (device declares in join request)
- Default: JSON
- Bridge translates between MQTT MessagePack ↔ NATS JSON

---

## PERSISTENCE MODEL

### Team-Level Persistence

**Enabled by:** team manifest `spec.communication.persistent=true`

**Backend:** NATS JetStream

**Subjects Persisted:**
```
hive.team.{TEAM_ID}.broadcast
hive.team.{TEAM_ID}.tasks.*
hive.team.{TEAM_ID}.results.*
```

**Retention:**
```yaml
historyDepth: 100               [default, per subject]
auto_discard_after_delivery: true
```

**Offline Behavior:** Agents receive missed messages on reconnect (up to historyDepth)

### Org-Level Persistence (Phase 3)

| Subject | Persistent | Retention | Notes |
|---------|-----------|-----------|-------|
| `hive.org.broadcast` | default true (if any team persistent) | max team historyDepth | All leads + director |
| `hive.org.leads.*` | true | max team historyDepth | Persistent across reconnects |
| `hive.org.capabilities.*` | false | ephemeral | Request-reply, no replay needed |
| `hive.org.cross.*` | true | 100 messages | Cross-team coordination |

### Non-Persistent Subjects

```
hive.health.*                   [ephemeral]
hive.metrics.*                  [ephemeral]
hive.control.*                  [ephemeral]
hive.join.*                     [ephemeral]
hive.agent.*.inbox              [NOT persistent by default, future per-agent config]
hive.capabilities.registry      [ephemeral]
```

---

## SECURITY

### Authentication

| Tier | Mechanism | Details |
|------|-----------|---------|
| 1 VM | vsock | No TCP exposure, per-agent NATS auth token |
| 1 Native | IPC | Per-agent NATS auth token |
| 2 | TLS + Token | Control plane TLS, per-agent auth token |
| 3 | MQTT + Username/Password | Join token derived, TLS optional (MCU constraints) |

### Authorization

- NATS ACLs restrict agents to allowed subjects
- Multi-user mode: user-specific NATS credentials, restricted to authorized team/agent subjects
- Cross-team capabilities: caller team must be in target team's `crossTeamCapabilities.allowedCallers`

### Data Protection

- TLS in transit (Tier 2+)
- vsock/IPC for Tier 1 (no network exposure)
- Binary blob references: UUID-based, no PII in filenames
- Message signing: OPTIONAL Phase 2 enhancement


